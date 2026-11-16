package copytrade

import (
	"fmt"
	"math"
	"time"

	"nofx/logger"
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
		if pos == nil || pos.Size <= 0 || pos.Symbol != symbol || string(pos.Side) != side ||
			(marginMode != "" && pos.MarginMode != "" && pos.MarginMode != marginMode) {
			continue
		}
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

	e.leaderStateMu.RLock()
	state := e.leaderState
	groupNotional := 0.0
	known := state != nil
	if state != nil {
		for _, p := range state.Positions {
			if p == nil || p.Symbol != mapping.Symbol || string(p.Side) != mapping.Side ||
				(mapping.MarginMode != "" && p.MarginMode != "" && p.MarginMode != mapping.MarginMode) {
				continue
			}
			if p.PosID != match.PosID {
				if m, mErr := e.store.CopyTrade().GetActiveMapping(e.traderID, p.PosID); mErr != nil || m == nil {
					continue // only followed leader positions define the target
				}
			}
			if p.PositionValue <= 0 {
				known = false
				break
			}
			groupNotional += p.PositionValue
		}
	}
	e.leaderStateMu.RUnlock()
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
	if actual <= 0 {
		// A followed mapping without a live follower position is reconciled by
		// the lifecycle checks; an add never rebuilds the whole position.
		return math.Min(copySize, headroom), nil
	}
	if math.Abs(headroom-copySize) > 1e-9 {
		logger.Infof("📊 [%s] 加仓按比例目标补差 | %s | 本次变化对应=%.2f → 补差=%.2f（目标=%.2f 实际=%.2f）", e.traderID, symbol, copySize, headroom, target, actual)
	}
	return headroom, nil
}
