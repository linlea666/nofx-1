package copytrade

import (
	"fmt"
	"math"
	"time"

	"nofx/logger"
)

// Leader reductions are read by intent: below the leader's average entry it
// is a stop (risk control) and follows at once; above it is a take-profit,
// which may wait for a price no worse than the leader's own reduce price,
// but never longer than the take-profit wait (default 5 minutes).
const defaultTakeProfitMaxWait = 5 * time.Minute

// reduceWaitState keeps the leader's reduce price from the first time the
// take-profit was seen, so a moving market does not move the reference.
type reduceWaitState struct {
	since time.Time
	ref   float64
}

func WithFollowerMinNotional(getMin func(symbol string) (float64, error)) EngineOption {
	return func(e *Engine) { e.getFollowerMinNotional = getMin }
}

func (e *Engine) takeProfitMaxWait() time.Duration {
	if e.config != nil && e.config.TakeProfitMaxWaitSeconds > 0 {
		return time.Duration(e.config.TakeProfitMaxWaitSeconds) * time.Second
	}
	return defaultTakeProfitMaxWait
}

func (e *Engine) clearReduceWait(posID string) {
	e.pricingMu.Lock()
	delete(e.reduceWaits, posID)
	e.pricingMu.Unlock()
}

// reducePriceGate decides whether a leader reduction executes now. A wait
// happens before any intent is reserved: the caller only releases the fill so
// the next poll rebuilds the signal from the latest leader position.
func (e *Engine) reducePriceGate(fill *Fill, match *SignalMatchResult) (wait bool, note string) {
	if !e.proportionalGuardsApply() || fill == nil || match == nil || match.Action != ActionReduce || match.LeaderPosition == nil {
		return false, ""
	}
	leader := match.LeaderPosition
	if fill.Price <= 0 || leader.EntryPrice <= 0 {
		return false, ""
	}
	long := leader.Side == SideLong
	if !((long && fill.Price > leader.EntryPrice) || (!long && fill.Price < leader.EntryPrice)) {
		e.clearReduceWait(match.PosID)
		return false, fmt.Sprintf("止损减仓 | 领航员减仓价=%.6g 均价=%.6g，立即执行", fill.Price, leader.EntryPrice)
	}
	if e.getFollowerMarketPrice == nil {
		return false, "止盈减仓"
	}
	symbol := fill.Symbol
	if match.ExecutionSymbol != "" {
		symbol = match.ExecutionSymbol
	}
	price, err := e.getFollowerMarketPrice(symbol)
	if err != nil || price <= 0 {
		return false, "止盈减仓"
	}

	e.pricingMu.Lock()
	if e.reduceWaits == nil {
		e.reduceWaits = make(map[string]*reduceWaitState)
	}
	state := e.reduceWaits[match.PosID]
	if state == nil {
		state = &reduceWaitState{since: time.Now(), ref: fill.Price}
		e.reduceWaits[match.PosID] = state
	}
	ref, since := state.ref, state.since
	e.pricingMu.Unlock()

	// Reducing a long sells (higher is better); reducing a short buys.
	tol := e.priceTolerancePct(fill.Symbol)
	if copyPriceWithinTolerance(!long, price, ref, tol) {
		e.clearReduceWait(match.PosID)
		return false, fmt.Sprintf("止盈减仓 | 领航员减仓价=%.6g 现价=%.6g", ref, price)
	}
	maxWait := e.takeProfitMaxWait()
	if time.Since(since) >= maxWait {
		e.clearReduceWait(match.PosID)
		return false, fmt.Sprintf("止盈减仓等待 %.0f 秒后按市价执行 | 领航员减仓价=%.6g 现价=%.6g", maxWait.Seconds(), ref, price)
	}
	return true, fmt.Sprintf("止盈减仓价格差于领航员减仓价超过 %.2f%% | 领航员减仓价=%.6g 现价=%.6g，最多等待 %.0f 秒", tol, ref, price, maxWait.Seconds())
}

// logReduceWait throttles the per-poll wait log to once a minute per position.
func (e *Engine) logReduceWait(posID, note string) {
	e.pricingMu.Lock()
	if e.reduceWaitLoggedAt == nil {
		e.reduceWaitLoggedAt = make(map[string]time.Time)
	}
	due := time.Since(e.reduceWaitLoggedAt[posID]) >= priceWaitLogInterval
	if due {
		e.reduceWaitLoggedAt[posID] = time.Now()
	}
	e.pricingMu.Unlock()
	if due {
		logger.Infof("🎯 [%s] ⏳ 减仓价格等待 | posId=%s | %s", e.traderID, posID, note)
	}
}

// planTargetReduce sizes a leader reduction against the proportional target
// instead of the leader's change ratio: the follower is reduced to its share
// of the remaining leader position. Reductions too small to trade stay in
// that excess and are executed with a later one; a remainder below the venue
// minimum is closed completely. Copy Guard sources keep their own group exit.
func (e *Engine) planTargetReduce(match *SignalMatchResult) {
	if !e.proportionalGuardsApply() || SupportsCopyGuard(e.config.ProviderType) || match == nil ||
		match.Action != ActionReduce || e.store == nil || e.getFollowerEquity == nil {
		return
	}
	mapping, err := e.store.CopyTrade().GetActiveMapping(e.traderID, match.PosID)
	if err != nil || mapping == nil {
		return
	}
	e.leaderStateMu.RLock()
	leaderEquity := 0.0
	if e.leaderState != nil {
		leaderEquity = e.leaderState.TotalEquity
	}
	e.leaderStateMu.RUnlock()
	if leaderEquity <= 0 || e.leaderAccountLeverage(leaderEquity) > maxPlausibleLeaderAccountLeverage {
		return // an untrustworthy denominator keeps the leader's own ratio
	}
	followerEquity := e.getFollowerEquity()
	if followerEquity <= 0 {
		return
	}
	groupNotional, known := e.followedLeaderGroupNotional(mapping, match.PosID)
	if !known || groupNotional <= 0 {
		// The leader still holds after a reduce; an empty aggregate is an
		// inconsistency and must never become a full close.
		return
	}
	follower, err := e.readFollowerGroupPosition(mapping.Symbol, mapping.Side, mapping.MarginMode)
	if err != nil || follower.Notional <= 0 {
		return
	}
	target := proportionalCopyNotional(groupNotional, leaderEquity, followerEquity, e.config.CopyRatio)
	excess := follower.Notional - target
	prefix := ""
	if match.ReduceNote != "" {
		prefix = match.ReduceNote + " | "
	}
	if excess <= follower.Notional*1e-6 {
		match.ReduceSkip = true
		match.ReduceNote = prefix + fmt.Sprintf("跟随仓位已不高于比例目标（目标 %.2f，实际 %.2f）", target, follower.Notional)
		return
	}
	ratio := math.Min(excess/follower.Notional, 1)
	note := fmt.Sprintf("按比例目标减仓 %.1f%%（目标=%.2f 实际=%.2f）", ratio*100, target, follower.Notional)
	if e.getFollowerMinNotional != nil {
		symbol := mapping.Symbol
		if match.ExecutionSymbol != "" {
			symbol = match.ExecutionSymbol
		}
		if minNotional, minErr := e.getFollowerMinNotional(symbol); minErr == nil && minNotional > 0 && target < minNotional {
			ratio = 1
			note = fmt.Sprintf("减后剩余 %.2f 低于交易所最小下单额 %.2f，直接全平", target, minNotional)
		}
	}
	match.ReduceRatioOverride = ratio
	match.ReduceNote = prefix + note
}
