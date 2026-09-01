package copytrade

import (
	"strings"
	"time"

	"nofx/logger"
	"nofx/market"
	"nofx/store"
)

// ============================================================================
// AI 收盘失效条件求值（观察态）
//
// AI 在批准重入时会写下「某周期收盘于某位置即论点失效」。此前这条结论只被解析、
// 存储和展示，没有任何代码核对它——AI 输出里最具执行价值的一项一直空转。
//
// 为什么需要一对结构化字段：close_invalidation 是自由文本（线上样本包含
// 「连续两根」「不适用」等复合与否定表述），任何正则解析都会在关键时刻给出错误
// 结论。因此模型被要求同时给出 close_invalidation_timeframe +
// close_invalidation_level 作为同一条件的机器可核对形式；缺失或不自洽时该条件
// 退回纯观察，不做任何推断。
//
// 为什么只记事件不离场：这条链路会改变实际交易行为，而线上尚无一条 v6+ 的
// ENTER 裁决填写过该字段，没有可回放的真实输入。先让事件积累一段真实样本，再
// 决定是否接入既有的 UnprotectableDisposition 处置链离场。
// ============================================================================

// closeInvalidationEventType 命中事件类型。命中不改变仓位，只留痕。
const closeInvalidationEventType = "AI_CLOSE_INVALIDATION_HIT"

// closeInvalidationMinInterval 是取数节流的下限。求值挂在领航员同步轮询上（秒级），
// 而 K 线接口不带缓存：不节流就会对每个在场仓位每轮发一次请求，白白占用限频额度
// 且拖慢热路径。真实节流间隔取该周期时长——新的已收盘 K 线一个周期才可能出现
// 一根，比周期更密的取数不可能得到新结论。
const closeInvalidationMinInterval = time.Minute

// closeInvalidationInterval 周期时长，无法解析时退回节流下限。
func closeInvalidationInterval(timeframe string) time.Duration {
	interval, err := market.TFDuration(timeframe)
	if err != nil || interval < closeInvalidationMinInterval {
		return closeInvalidationMinInterval
	}
	return interval
}

// nextCloseInvalidationCheck 判定本轮是否需要为该候选取数。
func (e *Engine) nextCloseInvalidationCheck(candidateID int64, interval time.Duration) bool {
	e.closeInvalidationMu.Lock()
	defer e.closeInvalidationMu.Unlock()
	if last, seen := e.closeInvalidationChecked[candidateID]; seen && time.Since(last) < interval {
		return false
	}
	if e.closeInvalidationChecked == nil {
		e.closeInvalidationChecked = map[int64]time.Time{}
	}
	e.closeInvalidationChecked[candidateID] = time.Now()
	return true
}

// evaluateCloseInvalidations 用已收盘 K 线核对在场 AI 重入仓位的收盘失效条件。
//
// 只读已完成 K 线：未收盘的那根随时会翻回去，用它判定等于把「收盘失效」降级成
// 「触碰失效」，与 AI 的原意不符。
func (e *Engine) evaluateCloseInvalidations() {
	if e.store == nil || e.config == nil {
		return
	}
	candidates, err := e.store.ReentryAI().ListCloseInvalidationWatch(e.traderID, 50)
	if err != nil {
		logger.Warnf("⚠️ [%s] 读取收盘失效待核对候选失败: %v", e.traderID, err)
		return
	}
	watched := make(map[int64]struct{}, len(candidates))
	for _, c := range candidates {
		if c == nil {
			continue
		}
		watched[c.ID] = struct{}{}
		interval := closeInvalidationInterval(c.CloseInvalidationTimeframe)
		if !e.nextCloseInvalidationCheck(c.ID, interval) {
			continue
		}
		candles, candleErr := market.GetOKXCompletedMarkCandles(c.Symbol, c.CloseInvalidationTimeframe, 2)
		if candleErr != nil || len(candles) == 0 {
			// 取数失败不推断结论：宁可这一轮不判，也不能用缺数据当作未失效或已失效。
			logger.Debugf("🔍 [%s] %s %s 收盘失效条件暂无可用 K 线: %v",
				e.traderID, c.Symbol, c.CloseInvalidationTimeframe, candleErr)
			continue
		}
		// 已按开盘时间升序，末位是最近一根已收盘 K 线。
		latest := candles[len(candles)-1]
		if !closeInvalidationBreached(c.Side, latest.Close, c.CloseInvalidationLevel) {
			continue
		}
		// 收盘时刻由开盘时间推导：该接口只填 OpenTime，直接读 CloseTime 会恒为
		// 零值，使这道闸门形同不存在。
		candleClose := time.UnixMilli(latest.OpenTime).Add(interval)
		// 成交后才开始核对：AI 写下条件的时刻可能落在这根 K 线之前，用更早的收盘
		// 判定会把入场前就已存在的形态算成入场后的失效。
		if c.ClosedAt != nil && candleClose.Before(*c.ClosedAt) {
			continue
		}
		marked, markErr := e.store.ReentryAI().MarkCloseInvalidationHit(c.ID)
		if markErr != nil {
			logger.Warnf("⚠️ [%s] 记录收盘失效命中失败: %v", e.traderID, markErr)
			continue
		}
		if !marked {
			continue
		}
		logger.Infof("📉 [%s] %s AI 收盘失效条件命中 | %s 收盘 %.6f 越过 %.6f（仅记录，不自动离场）",
			e.traderID, c.Symbol, c.CloseInvalidationTimeframe, latest.Close, c.CloseInvalidationLevel)
		_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
			CycleID: c.CycleID, TraderID: e.traderID, Type: closeInvalidationEventType,
			Price: latest.Close,
			Metadata: map[string]interface{}{
				"candidate_id":        c.ID,
				"timeframe":           c.CloseInvalidationTimeframe,
				"level":               c.CloseInvalidationLevel,
				"close_price":         latest.Close,
				"candle_close_time":   candleClose.UTC().Format(time.RFC3339),
				"condition_text":      c.CloseInvalidation,
				"ai_stop_price":       c.AIStopPrice,
				"action":              "OBSERVE_ONLY",
				"last_analysis_id":    c.LastAnalysisID,
				"decision_generation": c.DecisionGeneration,
			},
		})
	}
	e.forgetCloseInvalidationChecks(watched)
}

// forgetCloseInvalidationChecks 丢弃已离开观察集合的节流记录，避免长时间运行的
// 交易员在该 map 上单调累积。
func (e *Engine) forgetCloseInvalidationChecks(watched map[int64]struct{}) {
	e.closeInvalidationMu.Lock()
	defer e.closeInvalidationMu.Unlock()
	for id := range e.closeInvalidationChecked {
		if _, still := watched[id]; !still {
			delete(e.closeInvalidationChecked, id)
		}
	}
}

// closeInvalidationBreached 判定收盘价是否越过失效位。多单收在位下方失效，空单
// 收在位上方失效——与 AI 止损同向，但按收盘而非触价判定。
func closeInvalidationBreached(side string, closePrice, level float64) bool {
	if closePrice <= 0 || level <= 0 {
		return false
	}
	if strings.EqualFold(side, string(SideShort)) {
		return closePrice > level
	}
	return closePrice < level
}
