package copytrade

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nofx/logger"
	"nofx/notifier"
	"nofx/store"
)

// FollowCoin token 健康监视：
//   - 运行时识别到 token 失效（20001/20003）→ 凭证状态写为 expired（UI 立即
//     显示"已失效"），首次立即发邮件，仍未恢复则每 6 小时提醒一次；
//   - 之后一次成功读取 → 状态写回 valid，并发一封恢复通知；
//   - 根据 JWT exp，到期前 3 天开始预警，每天最多一封。
//
// 告警时间戳持久化在 followcoin_credentials 表，进程重启后节奏不变。
// 发信频率由本文件控制；每封邮件使用唯一 RateKey，避免通知器自身的
// 限流吞掉提醒。

const (
	followCoinFailureReminderInterval = 6 * time.Hour
	followCoinExpiryWarnWindow        = 72 * time.Hour
	followCoinExpiryWarnInterval      = 24 * time.Hour
)

var (
	followCoinCredObserveInflight atomic.Bool
	followCoinCredObserveMu       sync.Mutex
	// 未配置 token 时没有凭证行可记账，仅在进程内节流。
	followCoinUnconfiguredAlertAt time.Time
	// followCoinNotify 便于测试替换。
	followCoinNotify = notifier.Notify
)

// observeFollowCoinCredential 在每次权威快照读取后调用。只关心"token 失效"
// 与"成功"两类结果；网络错误、限频、账户停用等不改变凭证状态。
// 观测在后台执行，绝不阻塞轮询协程。
func (e *Engine) observeFollowCoinCredential(err error) {
	if e == nil || e.config == nil || e.config.ProviderType != ProviderFollowCoin || e.store == nil {
		return
	}
	failure := false
	switch {
	case err == nil:
	case errors.Is(err, ErrFollowCoinTokenInvalid):
		failure = true
		logger.Warnf("🔐 [%s] FollowCoin token 未配置或已失效，本轮不产生跟单信号 | apiId=%s err=%v",
			e.traderID, e.config.LeaderID, err)
	default:
		return
	}
	if !followCoinCredObserveInflight.CompareAndSwap(false, true) {
		return // 上一次观测尚未完成；下个轮询周期会再次观测
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	go func() {
		defer followCoinCredObserveInflight.Store(false)
		observeFollowCoinCredentialState(e.store, failure, errMsg, time.Now())
	}()
}

func observeFollowCoinCredentialState(st *store.Store, failure bool, errMsg string, now time.Time) {
	followCoinCredObserveMu.Lock()
	defer followCoinCredObserveMu.Unlock()
	cs := st.FollowCoinCreds()
	creds, err := cs.Get(store.FollowCoinCredsLabelDefault)
	if err != nil {
		logger.Warnf("⚠️ 读取 FollowCoin 凭证状态失败: %v", err)
		return
	}
	if failure {
		if creds == nil {
			if followCoinUnconfiguredAlertAt.IsZero() || now.Sub(followCoinUnconfiguredAlertAt) >= followCoinFailureReminderInterval {
				followCoinUnconfiguredAlertAt = now
				sendFollowCoinCredentialMail(st, "unconfigured", now, nil, errMsg)
			}
			return
		}
		if creds.LastStatus != store.FollowCoinCredsStatusExpired {
			if err := cs.UpdateStatus(store.FollowCoinCredsLabelDefault, store.FollowCoinCredsStatusExpired, errMsg); err != nil {
				logger.Warnf("⚠️ 写入 FollowCoin 凭证失效状态失败: %v", err)
			}
		}
		if creds.LastFailureAlertAt.IsZero() || now.Sub(creds.LastFailureAlertAt) >= followCoinFailureReminderInterval {
			kind := "expired"
			if !creds.LastFailureAlertAt.IsZero() {
				kind = "expired_reminder"
			}
			sendFollowCoinCredentialMail(st, kind, now, creds, errMsg)
			if err := cs.MarkFailureAlert(store.FollowCoinCredsLabelDefault, now); err != nil {
				logger.Warnf("⚠️ 记录 FollowCoin 失效告警时间失败: %v", err)
			}
		}
		return
	}

	followCoinUnconfiguredAlertAt = time.Time{}
	if creds == nil {
		return
	}
	if creds.LastStatus != store.FollowCoinCredsStatusValid {
		if err := cs.UpdateStatus(store.FollowCoinCredsLabelDefault, store.FollowCoinCredsStatusValid, ""); err != nil {
			logger.Warnf("⚠️ 写入 FollowCoin 凭证有效状态失败: %v", err)
		}
	}
	if !creds.LastFailureAlertAt.IsZero() {
		sendFollowCoinCredentialMail(st, "recovered", now, creds, "")
		if err := cs.MarkFailureAlert(store.FollowCoinCredsLabelDefault, time.Time{}); err != nil {
			logger.Warnf("⚠️ 清除 FollowCoin 失效告警时间失败: %v", err)
		}
	}
	if !creds.ExpiresAt.IsZero() && creds.ExpiresAt.Sub(now) <= followCoinExpiryWarnWindow &&
		(creds.LastExpiryWarnAt.IsZero() || now.Sub(creds.LastExpiryWarnAt) >= followCoinExpiryWarnInterval) {
		sendFollowCoinCredentialMail(st, "expiring", now, creds, "")
		if err := cs.MarkExpiryWarn(store.FollowCoinCredsLabelDefault, now); err != nil {
			logger.Warnf("⚠️ 记录 FollowCoin 到期预警时间失败: %v", err)
		}
	}
}

func sendFollowCoinCredentialMail(st *store.Store, kind string, now time.Time, creds *store.FollowCoinCredentials, errMsg string) {
	var title string
	var body strings.Builder
	switch kind {
	case "unconfigured":
		title = "FollowCoin 跟单 token 未配置，跟单已暂停"
		body.WriteString("FollowCoin 数据源的跟单交易员正在运行，但系统中没有可用的 FollowCoin token，跟单信号已暂停。\n")
	case "expired":
		title = "FollowCoin 跟单 token 已失效，跟单已暂停"
		body.WriteString("FollowCoin 接口拒绝了当前 token（未登录或已过期），所有 FollowCoin 跟单交易员已暂停产生新信号，避免使用过期数据下单。\n")
	case "expired_reminder":
		title = "FollowCoin 跟单 token 仍未恢复（提醒）"
		body.WriteString("FollowCoin token 依然无效，FollowCoin 跟单仍处于暂停状态。\n")
	case "recovered":
		title = "FollowCoin 跟单 token 已恢复"
		body.WriteString("FollowCoin token 已重新可用，跟单信号恢复。暂停期间领航员的仓位变化会在下一次快照对账中按当前持仓补齐。\n")
	case "expiring":
		title = "FollowCoin 跟单 token 即将到期"
		fmt.Fprintf(&body, "当前 FollowCoin token 将于 %s 到期，到期后跟单会暂停。\n", creds.ExpiresAt.Local().Format("2006-01-02 15:04"))
	default:
		return
	}
	if kind != "recovered" {
		body.WriteString("\n处理方法：在 FollowCoin App 重新登录并抓取请求头里的 token，然后打开 NOFX 交易员页面，点击「FollowCoin 凭证」粘贴保存，无需重启。\n")
	}
	if creds != nil && !creds.ExpiresAt.IsZero() && kind != "expiring" {
		fmt.Fprintf(&body, "\ntoken 到期时间：%s\n", creds.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	if strings.TrimSpace(errMsg) != "" {
		fmt.Fprintf(&body, "最近错误：%s\n", errMsg)
	}
	if ids, err := st.FollowCoinCreds().ListFollowCoinCopyTraderIDs(); err == nil && len(ids) > 0 {
		body.WriteString("\n受影响的跟单交易员：\n")
		for _, id := range ids {
			fmt.Fprintf(&body, "  - %s\n", st.Trader().ResolveDisplayName(id))
		}
	}
	followCoinNotify(notifier.Alert{
		Time:     now,
		Category: "copy_trade",
		Title:    title,
		Body:     body.String(),
		RateKey:  fmt.Sprintf("followcoin_credential|%s|%d", kind, now.Unix()),
	})
	logger.Infof("📧 FollowCoin 凭证通知已提交 | kind=%s", kind)
}
