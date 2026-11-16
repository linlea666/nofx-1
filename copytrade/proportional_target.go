package copytrade

import (
	"fmt"
	"math"
	"time"

	"nofx/logger"
	"nofx/store"
)

// WarnProportionalTargetReached marks an add that was not copied because the
// follower already holds its proportional share of the whole leader position.
// It is a deliberate skip: the source revision is acknowledged, not replayed.
const WarnProportionalTargetReached = "proportional_target_reached"

const (
	// A leader whose positions exceed 50x its equity has an implausible
	// denominator (e.g. a unified account read through its perp sub-balance).
	maxPlausibleLeaderAccountLeverage = 50.0
	// A denominator whose larger/smaller ratio to the previous sizing exceeds
	// 1+this is confirmed on the next poll before it can size a risk increase.
	maxLeaderEquityJump = 0.5
	// An add never starts once the follower holds this multiple of its target.
	proportionalTargetHardCap = 1.2
	// The follower's account leverage may exceed the leader's by this factor,
	// with accountLeverageFloor as the minimum allowance.
	accountLeverageCapMultiple = 1.5
	accountLeverageFloor       = 3.0
)

// proportionalGuardsApply limits the guards to sources whose position values
// and equity share one scale. Binance copy portfolios size against a mirrored
// follower anchor and are excluded.
func (e *Engine) proportionalGuardsApply() bool {
	if e.config == nil {
		return false
	}
	switch e.config.ProviderType {
	case ProviderHyperliquid, ProviderOKX, ProviderFollowCoin:
		return true
	}
	return false
}

// checkLeaderEquityPlausible rejects a risk increase sized against an equity
// value that cannot be the leader's account equity. The returned warning
// defers the source revision so the next poll replays it.
func (e *Engine) checkLeaderEquityPlausible(symbol string, leaderEquity float64) *Warning {
	if !e.proportionalGuardsApply() || leaderEquity <= 0 {
		return nil
	}
	e.leaderStateMu.RLock()
	state := e.leaderState
	totalNotional := 0.0
	if state != nil {
		for _, p := range state.Positions {
			if p != nil && p.PositionValue > 0 {
				totalNotional += p.PositionValue
			}
		}
	}
	e.leaderStateMu.RUnlock()

	reject := func(reason string) *Warning {
		logger.Warnf("⚠️ [%s] 领航员权益不可信，暂停本次风险增加 | %s | %s", e.traderID, symbol, reason)
		return &Warning{Timestamp: time.Now(), Symbol: symbol, Type: WarnLeaderEquityUnavailable,
			Message: "领航员权益不可信（" + reason + "），暂停本次跟单并在下一轮确认", Executed: false}
	}
	if leverage := totalNotional / leaderEquity; leverage > maxPlausibleLeaderAccountLeverage {
		return reject(fmt.Sprintf("隐含账户杠杆 %.1fx > %.0fx，权益=%.2f 持仓名义=%.2f", leverage, maxPlausibleLeaderAccountLeverage, leaderEquity, totalNotional))
	}
	prev := e.lastSizingLeaderEquity
	e.lastSizingLeaderEquity = leaderEquity
	if prev > 0 && math.Max(prev, leaderEquity)/math.Min(prev, leaderEquity)-1 > maxLeaderEquityJump {
		return reject(fmt.Sprintf("权益从 %.2f 跳变到 %.2f", prev, leaderEquity))
	}
	return nil
}

// followerGroupPosition is the follower's live exposure in one
// symbol/side/margin-mode group.
type followerGroupPosition struct {
	Quantity, Notional, AvgEntry float64
	// AccountNotional is the follower's exposure across all positions.
	AccountNotional float64
	// Positions counts live follower positions in the group (margin modes).
	Positions int
}

func (e *Engine) readFollowerGroupPosition(symbol, side, marginMode string) (followerGroupPosition, error) {
	var positions map[string]*Position
	if e.getFollowerPositionsResult != nil {
		var err error
		if positions, err = e.getFollowerPositionsResult(); err != nil {
			return followerGroupPosition{}, err
		}
	} else if e.getFollowerPositions != nil {
		positions = e.getFollowerPositions()
	}
	var g followerGroupPosition
	entryWeight := 0.0
	for _, pos := range positions {
		if pos != nil && pos.Size > 0 {
			g.AccountNotional += pos.PositionValue
		}
		if pos == nil || pos.Size <= 0 || pos.Symbol != symbol || string(pos.Side) != side ||
			(marginMode != "" && pos.MarginMode != "" && pos.MarginMode != marginMode) {
			continue
		}
		g.Positions++
		g.Quantity += pos.Size
		g.Notional += pos.PositionValue
		if pos.EntryPrice > 0 {
			entryWeight += pos.Size
			g.AvgEntry += pos.EntryPrice * pos.Size
		}
	}
	if entryWeight > 0 {
		g.AvgEntry /= entryWeight
	}
	return g, nil
}

// sizeAddByProportionalTarget sizes an add against the whole position
// instead of the single leader change: the follower is topped up to its
// proportional share of every followed leader position in the same
// symbol/side/margin-mode group. Small leader adds that were below the venue
// minimum stay in this deficit and are bought once it is executable, while
// rounding and denominator errors can no longer compound (39x observed live).
func (e *Engine) sizeAddByProportionalTarget(match *SignalMatchResult, symbol string, leaderEquity, followerEquity, copySize float64) (float64, *Warning) {
	if !e.proportionalGuardsApply() || match == nil || match.Action != ActionAdd || e.store == nil || copySize <= 0 {
		return copySize, nil
	}
	mapping, err := e.store.CopyTrade().GetActiveMapping(e.traderID, match.PosID)
	if err != nil || mapping == nil {
		return copySize, nil
	}

	groupNotional, known := e.followedLeaderGroupNotional(mapping, match.PosID)
	if !known || groupNotional <= 0 {
		return copySize, nil
	}

	follower, err := e.readFollowerGroupPosition(mapping.Symbol, mapping.Side, mapping.MarginMode)
	if err != nil {
		return 0, &Warning{Timestamp: time.Now(), Symbol: symbol, Type: WarnFollowerEquityUnavailable,
			Message: "跟随者持仓暂不可用，无法核对比例目标，等待恢复后重放", Executed: false}
	}
	actual := follower.Notional

	target := proportionalCopyNotional(groupNotional, leaderEquity, followerEquity, e.config.CopyRatio)
	headroom := target - actual
	if actual >= target*proportionalTargetHardCap || headroom <= 0 {
		logger.Infof("📊 [%s] 比例目标已达成，跳过加仓 | %s | 目标=%.2f 实际=%.2f", e.traderID, symbol, target, actual)
		return 0, &Warning{Timestamp: time.Now(), Symbol: symbol, Type: WarnProportionalTargetReached,
			Message: fmt.Sprintf("跟随仓位已达比例目标（目标 %.2f，实际 %.2f），本次加仓不跟", target, actual), Executed: false}
	}
	size := headroom
	if actual <= 0 {
		// A followed mapping without a live follower position is reconciled by
		// the lifecycle checks; an add never rebuilds the whole position.
		size = math.Min(copySize, headroom)
	} else if math.Abs(headroom-copySize) > 1e-9 {
		logger.Infof("📊 [%s] 加仓按比例目标补差 | %s | 本次变化对应=%.2f → 补差=%.2f（目标=%.2f 实际=%.2f）", e.traderID, symbol, copySize, headroom, target, actual)
	}
	return e.capByAccountLeverage(symbol, leaderEquity, followerEquity, follower.AccountNotional, size)
}

// followedLeaderGroupNotional sums the leader positions this follower copies
// into the mapping's follower scope (symbol/side and the follower's margin
// mode). The leader's own margin mode is not compared: the follower may hold
// the copy in another mode (e.g. Binance Multi-Assets forces cross).
// known=false when a value is missing, so callers keep their old behaviour.
func (e *Engine) followedLeaderGroupNotional(mapping *store.CopyTradePositionMapping, posID string) (float64, bool) {
	e.leaderStateMu.RLock()
	defer e.leaderStateMu.RUnlock()
	state := e.leaderState
	if state == nil {
		return 0, false
	}
	total := 0.0
	for _, p := range state.Positions {
		if p == nil || p.Symbol != mapping.Symbol || string(p.Side) != mapping.Side {
			continue
		}
		if p.PosID != posID {
			m, err := e.store.CopyTrade().GetActiveMapping(e.traderID, p.PosID)
			if err != nil || m == nil {
				continue // only followed leader positions define the target
			}
			if mapping.MarginMode != "" && m.MarginMode != "" && m.MarginMode != mapping.MarginMode {
				continue // copied into another follower margin scope
			}
		}
		if p.PositionValue <= 0 {
			return 0, false
		}
		total += p.PositionValue
	}
	return total, true
}

// leaderAccountLeverage is the leader's total notional over its equity.
func (e *Engine) leaderAccountLeverage(leaderEquity float64) float64 {
	if leaderEquity <= 0 {
		return 0
	}
	e.leaderStateMu.RLock()
	defer e.leaderStateMu.RUnlock()
	total := 0.0
	if e.leaderState != nil {
		for _, p := range e.leaderState.Positions {
			if p != nil && p.PositionValue > 0 {
				total += p.PositionValue
			}
		}
	}
	return total / leaderEquity
}

// capByAccountLeverage keeps the follower's whole-account leverage within
// max(leader account leverage x 1.5, 3x) after this add.
func (e *Engine) capByAccountLeverage(symbol string, leaderEquity, followerEquity, followerNotional, size float64) (float64, *Warning) {
	if followerEquity <= 0 {
		return size, nil
	}
	limit := math.Max(e.leaderAccountLeverage(leaderEquity)*accountLeverageCapMultiple, accountLeverageFloor)
	allowed := followerEquity*limit - followerNotional
	if allowed <= 0 {
		logger.Infof("📊 [%s] 账户杠杆已达上限，跳过加仓 | %s | 上限=%.2fx 当前名义=%.2f 权益=%.2f", e.traderID, symbol, limit, followerNotional, followerEquity)
		return 0, &Warning{Timestamp: time.Now(), Symbol: symbol, Type: WarnProportionalTargetReached,
			Message: fmt.Sprintf("跟随账户杠杆已达上限 %.2fx，本次加仓不跟", limit), Executed: false}
	}
	if size > allowed {
		logger.Infof("📊 [%s] 加仓受账户杠杆上限收敛 | %s | %.2f → %.2f（上限=%.2fx）", e.traderID, symbol, size, allowed, limit)
		return allowed, nil
	}
	return size, nil
}
