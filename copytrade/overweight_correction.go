package copytrade

import (
	"fmt"
	"time"

	"nofx/logger"
	"nofx/notifier"
	"nofx/store"
	"nofx/trader"
)

// Automatic correction of an overweight follower position (on by default,
// see CopyConfig.DisableAutoCorrect). A position more than
// overweightCorrectionMultiple times its proportional target on two checks at
// least overweightCheckInterval apart is reduced to the target at market.
// Copy Guard sources (OKX/Binance) are excluded: their protective stops own
// the follower quantity.
const (
	overweightCorrectionMultiple = 1.5
	overweightCheckInterval      = 30 * time.Second
	overweightConfirmations      = 2
)

// PositionReducer reduces a follower position by quantity at market.
type PositionReducer interface {
	ReducePositionMarket(symbol, side string, quantity float64) (string, error)
}

func (ti *TraderIntegration) correctProportionalOverweight() {
	if ti.engine == nil || ti.engine.config == nil || ti.store == nil {
		return
	}
	e, cfg := ti.engine, ti.engine.config
	if cfg.DisableAutoCorrect || SupportsCopyGuard(cfg.ProviderType) || !e.proportionalGuardsApply() {
		return
	}
	if time.Since(ti.lastOverweightCheck) < overweightCheckInterval {
		return
	}
	ti.lastOverweightCheck = time.Now()
	reducer, ok := ti.executor.(PositionReducer)
	if !ok {
		return
	}

	e.leaderStateMu.RLock()
	leaderEquity := 0.0
	if e.leaderState != nil {
		leaderEquity = e.leaderState.TotalEquity
	}
	e.leaderStateMu.RUnlock()
	if leaderEquity <= 0 || e.leaderAccountLeverage(leaderEquity) > maxPlausibleLeaderAccountLeverage {
		return // never correct against an untrustworthy denominator
	}
	followerEquity := ti.getEquityFunc()()
	if followerEquity <= 0 {
		return
	}
	mappings, err := ti.store.CopyTrade().ListActiveMappings(ti.traderID)
	if err != nil {
		return
	}
	if ti.overweightStrikes == nil {
		ti.overweightStrikes = make(map[string]int)
	}
	checked := map[string]bool{}
	for _, m := range mappings {
		key := m.Symbol + "|" + m.Side + "|" + m.MarginMode
		if checked[key] {
			continue
		}
		checked[key] = true
		groupNotional, known := e.followedLeaderGroupNotional(m, m.LeaderPosID)
		if !known || groupNotional <= 0 {
			delete(ti.overweightStrikes, key)
			continue
		}
		follower, err := e.readFollowerGroupPosition(m.Symbol, m.Side, m.MarginMode)
		if err != nil {
			return // follower state unknown: try again on the next check
		}
		// One live follower position per group: an ambiguous venue position
		// is never reduced automatically.
		if follower.Positions != 1 || follower.Quantity <= 0 || follower.Notional <= 0 {
			delete(ti.overweightStrikes, key)
			continue
		}
		target := proportionalCopyNotional(groupNotional, leaderEquity, followerEquity, cfg.CopyRatio)
		if follower.Notional <= target*overweightCorrectionMultiple {
			delete(ti.overweightStrikes, key)
			continue
		}
		ti.overweightStrikes[key]++
		if ti.overweightStrikes[key] < overweightConfirmations {
			logger.Warnf("⚠️ [%s] 跟随仓位超出比例目标，待下次复核 | %s %s | 目标=%.2f 实际=%.2f", ti.traderID, m.Symbol, m.Side, target, follower.Notional)
			continue
		}
		delete(ti.overweightStrikes, key)
		ti.reduceOverweightGroup(reducer, m, follower, target)
	}
}

func (ti *TraderIntegration) reduceOverweightGroup(reducer PositionReducer, m *store.CopyTradePositionMapping, follower followerGroupPosition, target float64) {
	price := follower.Notional / follower.Quantity
	quantity := follower.Quantity * (follower.Notional - target) / follower.Notional
	if resolver, ok := ti.executor.(trader.ExecutionInstrumentResolver); ok {
		inst, err := resolver.ResolveExecutionInstrument(m.Symbol)
		if err != nil || inst == nil {
			return
		}
		// A target below the venue minimum is a position opened at the
		// exchange minimum on purpose; it is left to the leader's own exits.
		if minimum, minErr := trader.MinimumExecutableQuantity(inst, price); minErr != nil || target/price < minimum {
			logger.Infof("📊 [%s] 超额仓位目标低于交易所最小量，保留最小仓位 | %s %s | 目标=%.2f 实际=%.2f", ti.traderID, m.Symbol, m.Side, target, follower.Notional)
			return
		}
		q, err := trader.QuantizeOrderIntent(inst, quantity, trader.QuantityPartialReduce)
		if err != nil {
			return
		}
		quantity = q.Quantized
	}
	if quantity <= 0 || quantity >= follower.Quantity {
		return
	}
	ti.executionMu.Lock()
	orderID, err := reducer.ReducePositionMarket(m.Symbol, m.Side, quantity)
	ti.executionMu.Unlock()

	summary := fmt.Sprintf("跟随仓位超出比例目标 %.1f 倍，自动减仓 %.8f 至目标（目标=%.2f 实际=%.2f）", follower.Notional/target, quantity, target, follower.Notional)
	severity, status := store.CopyEventSeverityWarn, "executed"
	if err != nil {
		severity, status = store.CopyEventSeverityError, "failed"
		summary += "，下单失败：" + err.Error()
		logger.Errorf("❌ [%s] 超额自动减仓失败 | %s %s: %v", ti.traderID, m.Symbol, m.Side, err)
	} else {
		logger.Warnf("🔻 [%s] 超额自动减仓 | %s %s | 数量=%.8f 订单=%s | 目标=%.2f 实际=%.2f", ti.traderID, m.Symbol, m.Side, quantity, orderID, target, follower.Notional)
	}
	_ = ti.store.CopyTrade().LogCopyEvent(&store.CopyTradeEvent{
		TraderID: ti.traderID, LeaderID: ti.engine.config.LeaderID, ProviderType: string(ti.engine.config.ProviderType),
		Category: store.CopyEventCategoryAction, EventType: "PROPORTIONAL_OVERWEIGHT_CORRECTED", Severity: severity,
		Symbol: m.Symbol, Side: m.Side, LeaderPosID: m.LeaderPosID, Status: status, Summary: summary,
		Detail: map[string]interface{}{"target_notional": target, "follower_notional": follower.Notional, "reduce_quantity": quantity, "order_id": orderID},
	})
	traderName := ti.traderDisplayName()
	notifier.Notify(notifier.Alert{
		Time: time.Now(), Category: "copy_trade", TraderID: ti.traderID, TraderName: traderName,
		Title: fmt.Sprintf("%s | %s 超额自动减仓", traderName, m.Symbol), Body: summary,
		RateKey: "overweight|" + ti.traderID + "|" + m.Symbol, DedupKey: fmt.Sprintf("overweight|%s|%s|%d", ti.traderID, m.Symbol, time.Now().Unix()/600),
	})
}
