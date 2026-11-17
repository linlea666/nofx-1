package copytrade

import (
	"fmt"
	"math"
	"strings"
	"time"

	"nofx/logger"
	"nofx/notifier"
	"nofx/store"
)

// Following a leader's exit is the highest-priority copy action. The normal
// close path verifies custody, retires old catch-ups and keeps bookkeeping
// exact; any of those can stall. This watchdog is the final guarantee: once
// the installed (healthy) leader snapshot has shown no position on a followed
// symbol+side for leaderExitEnforceAfter, the follower position still held for
// that active mapping is closed at market. Bookkeeping then settles through
// the regular close signal, which finds the follower position gone.
const (
	leaderExitWatchInterval  = 15 * time.Second
	leaderExitEnforceAfter   = 60 * time.Second
	leaderExitSnapshotMaxAge = 30 * time.Second
)

func (ti *TraderIntegration) enforceLeaderExits() {
	if ti.engine == nil || ti.engine.config == nil || ti.store == nil || !ti.lifecycleRunning() {
		return
	}
	if time.Since(ti.lastLeaderExitWatch) < leaderExitWatchInterval {
		return
	}
	ti.lastLeaderExitWatch = time.Now()
	reducer, ok := ti.executor.(PositionReducer)
	if !ok {
		return
	}

	e := ti.engine
	e.leaderStateMu.RLock()
	state, synced := e.leaderState, e.lastStateSync
	held := map[string]bool{}
	if state != nil {
		for _, p := range state.Positions {
			if p != nil && math.Abs(p.Size) > 0 {
				held["pos|"+p.PosID] = true
				held[p.Symbol+"|"+string(p.Side)] = true
			}
		}
	}
	e.leaderStateMu.RUnlock()
	if state == nil || time.Since(synced) > leaderExitSnapshotMaxAge {
		return // leader exit cannot be confirmed without a fresh healthy snapshot
	}

	mappings, err := ti.store.CopyTrade().ListActiveMappings(ti.traderID)
	if err != nil {
		return
	}
	if ti.leaderExitAbsentSince == nil {
		ti.leaderExitAbsentSince = make(map[string]time.Time)
	}
	active := map[string]bool{}
	enforced := map[string]bool{}
	now := time.Now()
	for _, m := range mappings {
		active[m.LeaderPosID] = true
		if held["pos|"+m.LeaderPosID] || held[m.Symbol+"|"+m.Side] {
			delete(ti.leaderExitAbsentSince, m.LeaderPosID)
			continue
		}
		first, seen := ti.leaderExitAbsentSince[m.LeaderPosID]
		if !seen {
			ti.leaderExitAbsentSince[m.LeaderPosID] = now
			continue
		}
		if now.Sub(first) < leaderExitEnforceAfter || enforced[m.Symbol+"|"+m.Side] {
			continue
		}
		// A position the user explicitly took over is no longer ours to close.
		if custody, custodyErr := ti.store.CopyTrade().GetPositionCustody(ti.traderID, m.LeaderPosID); custodyErr == nil && custody.State == "RELEASED" {
			continue
		}
		enforced[m.Symbol+"|"+m.Side] = true
		ti.enforceLeaderExit(reducer, m, now.Sub(first))
	}
	for posID := range ti.leaderExitAbsentSince {
		if !active[posID] {
			delete(ti.leaderExitAbsentSince, posID)
		}
	}
}

// leaderExitFollowerQuantity is the follower's held quantity in the
// mapping's margin scope (adoptVenueMarginMode books the venue's actual mode).
func (ti *TraderIntegration) leaderExitFollowerQuantity(m *store.CopyTradePositionMapping) (float64, error) {
	positions, err := ti.getFreshPositions()
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, p := range positions {
		if getStringField(p, "symbol") != m.Symbol || !strings.EqualFold(getStringField(p, "side"), m.Side) {
			continue
		}
		if m.MarginMode != "" && !strings.EqualFold(getStringField(p, "mgnMode", "marginMode"), m.MarginMode) {
			continue
		}
		amount, known := knownQuantityField(p, "positionAmt", "quantity")
		if !known || math.IsNaN(amount) || math.IsInf(amount, 0) {
			return 0, fmt.Errorf("current position quantity is unavailable")
		}
		total += math.Abs(amount)
	}
	return total, nil
}

func (ti *TraderIntegration) enforceLeaderExit(reducer PositionReducer, m *store.CopyTradePositionMapping, absentFor time.Duration) {
	// Read and close under the execution lock so a regular close finishing
	// at the same moment is observed instead of raced.
	ti.executionMu.Lock()
	quantity, err := ti.leaderExitFollowerQuantity(m)
	if err != nil || quantity <= 0 {
		ti.executionMu.Unlock()
		if err != nil {
			logger.Warnf("⚠️ [%s] 领航员已平仓兜底：读取跟随持仓失败，下轮重试 | %s %s: %v", ti.traderID, m.Symbol, m.Side, err)
		}
		return // already flat: the regular close signal settles the mapping
	}
	orderID, err := reducer.ReducePositionMarket(m.Symbol, m.Side, quantity)
	ti.executionMu.Unlock()

	summary := fmt.Sprintf("领航员已平仓 %.0f 秒，跟随仓位仍在，平仓优先：市价全平 %.8f", absentFor.Seconds(), quantity)
	severity, status := store.CopyEventSeverityWarn, "executed"
	if err != nil {
		severity, status = store.CopyEventSeverityError, "failed"
		summary += "，下单失败：" + err.Error()
		logger.Errorf("❌ [%s] 领航员已平仓兜底平仓失败 | %s %s posId=%s: %v", ti.traderID, m.Symbol, m.Side, m.LeaderPosID, err)
	} else {
		logger.Warnf("🚨 [%s] 领航员已平仓兜底平仓 | %s %s posId=%s 数量=%.8f 订单=%s 领航员已平 %.0fs", ti.traderID, m.Symbol, m.Side, m.LeaderPosID, quantity, orderID, absentFor.Seconds())
		delete(ti.leaderExitAbsentSince, m.LeaderPosID)
	}
	_ = ti.store.CopyTrade().LogCopyEvent(&store.CopyTradeEvent{
		TraderID: ti.traderID, LeaderID: ti.engine.config.LeaderID, ProviderType: string(ti.engine.config.ProviderType),
		Category: store.CopyEventCategoryAction, EventType: "LEADER_EXIT_ENFORCED", Severity: severity,
		Symbol: m.Symbol, Side: m.Side, LeaderPosID: m.LeaderPosID, Status: status, Summary: summary,
		Detail: map[string]interface{}{"quantity": quantity, "order_id": orderID, "leader_absent_seconds": absentFor.Seconds()},
	})
	traderName := ti.traderDisplayName()
	notifier.Notify(notifier.Alert{
		Time: time.Now(), Category: "copy_trade", TraderID: ti.traderID, TraderName: traderName,
		Title: fmt.Sprintf("%s | %s 领航员已平仓，兜底平仓", traderName, m.Symbol), Body: summary,
		RateKey: "leader_exit|" + ti.traderID + "|" + m.Symbol, DedupKey: fmt.Sprintf("leader_exit|%s|%s|%d", ti.traderID, m.Symbol, time.Now().Unix()/600),
	})
}
