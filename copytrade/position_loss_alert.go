package copytrade

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"nofx/logger"
	"nofx/notifier"
	"nofx/trader"
)

// 跟单仓位亏损告警（仅通知，不做任何交易动作）。
//
// 设计要点：
//   - 阈值全局生效（env NOTIFY_POSITION_LOSS_PCT），对所有跟单交易员账户上的
//     全部仓位适用，无需逐个交易员配置。
//   - 亏损比例直接复用 AutoTrader.GetPositions 输出的 unrealized_pnl_pct，
//     与页面持仓列表展示的数字完全一致。
//   - 每个仓位跌破阈值只发一封；亏损回升到"阈值 - 回升间隔"以上或仓位消失后
//     才重新监测，避免在阈值附近来回波动时反复发信。
//   - 告警状态按"交易所账户 + 仓位标识"在进程内共享：多个交易员共用同一账户时
//     同一仓位只发一封。
//   - 邮件发送失败/被丢弃时，延迟后重试，不会因一次 SMTP 故障永久漏发。
const (
	positionLossAlertInterval = 30 * time.Second
	// positionLossAlertRearmGapPct 回升多少个百分点后重新监测（阈值很小时取阈值一半）。
	positionLossAlertRearmGapPct = 10.0
	// positionLossAlertRetryDelay 邮件发送失败后的重试间隔。
	positionLossAlertRetryDelay = 5 * time.Minute
	// positionLossAlertStaleAfter 状态超过该时长未被任何监测轮次观察到即视为过期：
	// 例如交易员停止期间仓位已平、之后同标识的新仓位不应继承旧的"已告警"状态。
	positionLossAlertStaleAfter = 10 * time.Minute
)

type positionLossAlertEntry struct {
	alerted  bool
	token    uint64
	retryAt  time.Time // 非零表示上次投递失败，到期后在仍满足条件时重发
	lastSeen time.Time
}

// positionLossAlerts: account -> position_key -> entry
var positionLossAlerts = struct {
	sync.Mutex
	seq     uint64
	entries map[string]map[string]*positionLossAlertEntry
}{entries: make(map[string]map[string]*positionLossAlertEntry)}

type positionLossAlertCandidate struct {
	positionKey string
	token       uint64
	pnlPct      float64
	position    map[string]interface{}
}

func (ti *TraderIntegration) monitorPositionLossAlerts() {
	ticker := time.NewTicker(positionLossAlertInterval)
	defer ticker.Stop()
	account := ""
	for {
		select {
		case <-ti.ctx.Done():
			return
		case <-ticker.C:
		}
		threshold := notifier.PositionLossAlertPct()
		if threshold <= 0 || !ti.lifecycleRunning() {
			continue
		}
		if account == "" {
			resolved, ok := ti.positionLossAlertAccount()
			if !ok {
				// 账户暂不可解析（存储瞬时错误）时跳过本轮，避免换用其它去重键
				// 导致账户解析成功后同一仓位重复发信。
				continue
			}
			account = resolved
		}
		ti.checkPositionLossAlerts(account, threshold)
	}
}

// positionLossAlertAccount 返回执行账户维度的去重标识。
func (ti *TraderIntegration) positionLossAlertAccount() (string, bool) {
	if ti.store == nil {
		return "", false
	}
	owner, err := ti.store.Trader().GetByID(ti.traderID)
	if err != nil || owner == nil || strings.TrimSpace(owner.ExchangeID) == "" {
		return "", false
	}
	return "exchange:" + owner.ExchangeID, true
}

func positionLossAlertRearmPct(thresholdPct float64) float64 {
	gap := math.Min(positionLossAlertRearmGapPct, thresholdPct/2)
	return -(thresholdPct - gap)
}

func positionLossAlertPositionKey(pos map[string]interface{}) string {
	if key := getStringField(pos, "position_key"); key != "" {
		return key
	}
	symbol := getStringField(pos, "symbol")
	side := getStringField(pos, "side")
	if symbol == "" || side == "" {
		return ""
	}
	return trader.CopyPositionKey(symbol, side, getStringField(pos, "mgnMode", "marginMode"), getStringField(pos, "posId", "positionId"))
}

// checkPositionLossAlerts 执行一轮检查。thresholdPct 为正数（50 表示 -50%）。
func (ti *TraderIntegration) checkPositionLossAlerts(account string, thresholdPct float64) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warnf("⚠️ [%s] 仓位亏损告警检查异常已忽略: %v", ti.traderID, r)
		}
	}()
	if thresholdPct <= 0 || ti.executor == nil {
		return
	}
	positions, err := ti.executor.GetPositions()
	if err != nil {
		// 读取失败不改变任何告警状态，下一轮再试。
		logger.Debugf("📭 [%s] 仓位亏损告警：读取持仓失败: %v", ti.traderID, err)
		return
	}

	now := time.Now()
	rearmPct := positionLossAlertRearmPct(thresholdPct)
	var due []positionLossAlertCandidate

	positionLossAlerts.Lock()
	accountEntries := positionLossAlerts.entries[account]
	if accountEntries == nil {
		accountEntries = make(map[string]*positionLossAlertEntry)
		positionLossAlerts.entries[account] = accountEntries
	}
	seen := make(map[string]bool, len(positions))
	for _, pos := range positions {
		if pos == nil {
			continue
		}
		key := positionLossAlertPositionKey(pos)
		pnlPct, ok := pos["unrealized_pnl_pct"].(float64)
		if key == "" || !ok || math.IsNaN(pnlPct) || math.IsInf(pnlPct, 0) {
			continue
		}
		seen[key] = true
		entry := accountEntries[key]
		if entry == nil || now.Sub(entry.lastSeen) > positionLossAlertStaleAfter {
			entry = &positionLossAlertEntry{}
			accountEntries[key] = entry
		}
		entry.lastSeen = now

		if entry.alerted {
			if pnlPct > rearmPct {
				// 亏损已明显回升：重新进入监测状态。
				entry.alerted = false
				entry.retryAt = time.Time{}
				continue
			}
			if entry.retryAt.IsZero() || now.Before(entry.retryAt) || pnlPct > -thresholdPct {
				continue
			}
		} else if pnlPct > -thresholdPct {
			continue
		}
		positionLossAlerts.seq++
		entry.alerted = true
		entry.token = positionLossAlerts.seq
		entry.retryAt = time.Time{}
		due = append(due, positionLossAlertCandidate{positionKey: key, token: entry.token, pnlPct: pnlPct, position: pos})
	}
	// 本账户已不存在的仓位（已平仓）清除状态；新仓位重新监测。
	for key := range accountEntries {
		if !seen[key] {
			delete(accountEntries, key)
		}
	}
	if len(accountEntries) == 0 {
		delete(positionLossAlerts.entries, account)
	}
	positionLossAlerts.Unlock()

	for _, candidate := range due {
		ti.sendPositionLossAlert(account, candidate, thresholdPct)
	}
}

// releasePositionLossAlert 投递失败时安排重试；只作用于同一次告警（token 一致）。
func releasePositionLossAlert(account, positionKey string, token uint64) {
	positionLossAlerts.Lock()
	defer positionLossAlerts.Unlock()
	entry := positionLossAlerts.entries[account][positionKey]
	if entry == nil || entry.token != token || !entry.alerted {
		return
	}
	entry.retryAt = time.Now().Add(positionLossAlertRetryDelay)
}

func (ti *TraderIntegration) sendPositionLossAlert(account string, c positionLossAlertCandidate, thresholdPct float64) {
	pos := c.position
	symbol := getStringField(pos, "symbol")
	side := getStringField(pos, "side")
	marginMode := getStringField(pos, "mgnMode", "marginMode")
	leverage := getIntOrFloatField(pos, "leverage")
	entry := getFloatField(pos, "entry_price", "entryPrice")
	mark := getFloatField(pos, "mark_price", "markPrice")
	liq := getFloatField(pos, "liquidation_price", "liquidationPrice")
	qty := math.Abs(getFloatField(pos, "quantity", "positionAmt"))
	upl := getFloatField(pos, "unrealized_pnl", "unRealizedProfit")
	margin := getFloatField(pos, "margin_used")

	liqDistance := "未知"
	if liq > 0 && mark > 0 {
		distance := (mark - liq) / mark * 100
		if strings.EqualFold(side, "short") {
			distance = (liq - mark) / mark * 100
		}
		liqDistance = fmt.Sprintf("%.2f%%", distance)
	}

	traderName := ti.traderDisplayName()
	title := fmt.Sprintf("%s | %s %s 仓位亏损 %.2f%%（阈值 -%.2f%%）", traderName, symbol, side, c.pnlPct, thresholdPct)
	body := fmt.Sprintf(
		"跟单仓位亏损已达到告警阈值，请检查仓位并根据情况手动调整止损。\n\n"+
			"交易员: %s (%s)\n"+
			"品种 / 方向: %s / %s\n"+
			"保证金模式: %s\n"+
			"杠杆: %dx\n"+
			"数量: %.8g\n"+
			"开仓均价: %.8g\n"+
			"标记价格: %.8g\n"+
			"强平价格: %.8g\n"+
			"距强平: %s\n"+
			"未实现盈亏: %.4f USDT\n"+
			"占用保证金: %.4f USDT\n"+
			"仓位亏损比例: %.2f%%\n"+
			"告警阈值: -%.2f%%\n\n"+
			"亏损比例口径 = 未实现盈亏 / (数量 × 标记价 / 杠杆)，与页面持仓列表一致。\n"+
			"本仓位在亏损回升到 %.2f%% 以上或平仓之前不会再次提醒。",
		traderName, ti.traderID, symbol, side, marginMode, leverage, qty, entry, mark, liq, liqDistance,
		upl, margin, c.pnlPct, thresholdPct, positionLossAlertRearmPct(thresholdPct))

	logger.Warnf("🚨 [%s] 仓位亏损达到告警阈值 | %s %s pnl=%.2f%% threshold=-%.2f%%",
		ti.traderID, symbol, side, c.pnlPct, thresholdPct)

	notifier.Notify(notifier.Alert{
		Time:       time.Now(),
		Category:   "copy_trade",
		TraderID:   ti.traderID,
		TraderName: traderName,
		Title:      title,
		Body:       body,
		RateKey:    fmt.Sprintf("position_loss|%s|%s|%d", account, c.positionKey, c.token),
		Fields: map[string]string{
			"TraderName":  traderName,
			"PositionKey": c.positionKey,
			"PnLPct":      fmt.Sprintf("%.2f", c.pnlPct),
			"Threshold":   fmt.Sprintf("-%.2f", thresholdPct),
		},
		StatusHook: func(status notifier.DeliveryStatus, err error) {
			switch status {
			case notifier.DeliveryFailed, notifier.DeliveryDropped, notifier.DeliveryDisabled:
				releasePositionLossAlert(account, c.positionKey, c.token)
			}
		},
	})
}
