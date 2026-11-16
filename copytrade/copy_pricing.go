package copytrade

import (
	"fmt"
	"math"
	"strings"
	"time"

	"nofx/decision"
	"nofx/logger"
)

// Price-aware execution of risk increases. An open or add executes at market
// when the follower's price is no worse than the leader's own fill price by
// more than the symbol tolerance; otherwise the source revision is deferred
// and re-checked every poll (a "soft limit" at the leader's price, with no
// resting order). An initial open still follows after initialOpenMaxWait.
const (
	majorCopyPriceTolerancePct = 0.2 // BTC / ETH
	altCopyPriceTolerancePct   = 0.5
	initialOpenMaxWait         = 2 * time.Minute
	costCreditMaxMultiple      = 3.0
	priceWaitLogInterval       = time.Minute
)

// isMajorCopySymbol reports BTC and ETH perpetuals (any stable quote).
func isMajorCopySymbol(symbol string) bool {
	base := strings.ToUpper(strings.TrimSpace(symbol))
	for _, quote := range []string{"USDT", "USDC", "USD"} {
		if strings.HasSuffix(base, quote) {
			base = strings.TrimSuffix(base, quote)
			break
		}
	}
	return base == "BTC" || base == "ETH"
}

// copyPriceTolerancePct is the default adverse deviation from the leader's
// price that may still be taken at market.
func copyPriceTolerancePct(symbol string) float64 {
	if isMajorCopySymbol(symbol) {
		return majorCopyPriceTolerancePct
	}
	return altCopyPriceTolerancePct
}

// priceTolerancePct applies the trader's configured tolerance, if any.
func (e *Engine) priceTolerancePct(symbol string) float64 {
	tol := copyPriceTolerancePct(symbol)
	if e.config == nil {
		return tol
	}
	configured := e.config.PriceToleranceAltPct
	if isMajorCopySymbol(symbol) {
		configured = e.config.PriceToleranceMajorPct
	}
	if configured != nil && *configured >= 0 {
		return *configured
	}
	return tol
}

// copyPriceWithinTolerance reports whether executing at price is at most
// tolPct worse than ref: a buy wants a lower price, a sell a higher one.
func copyPriceWithinTolerance(buying bool, price, ref, tolPct float64) bool {
	if buying {
		return price <= ref*(1+tolPct/100)
	}
	return price >= ref*(1-tolPct/100)
}

// leaderEntryObservation is the leader size and average entry last seen for a
// position, used to derive the price of the next add from the entry move.
type leaderEntryObservation struct {
	size, entry float64
}

// leaderAddAccumulator holds the leader adds the follower has not bought yet
// (e.g. below the venue minimum or waiting for price) and their VWAP.
type leaderAddAccumulator struct {
	leaderQty, leaderNotional float64
	leaderSize                float64 // leader size already counted
	followerQty               float64 // follower size when accumulation began
}

func WithFollowerMarketPrice(getPrice func(symbol string) (float64, error)) EngineOption {
	return func(e *Engine) { e.getFollowerMarketPrice = getPrice }
}

// observeLeaderPosition records the latest size/entry of a leader position.
func (e *Engine) observeLeaderPosition(match *SignalMatchResult) {
	if match == nil || match.PosID == "" {
		return
	}
	e.pricingMu.Lock()
	defer e.pricingMu.Unlock()
	if e.leaderEntries == nil {
		e.leaderEntries = make(map[string]leaderEntryObservation)
	}
	if match.Action == ActionClose || match.LeaderPosition == nil || match.LeaderPosition.Size <= 0 {
		delete(e.leaderEntries, match.PosID)
		delete(e.leaderAdds, match.PosID)
		delete(e.reduceWaits, match.PosID)
		return
	}
	if match.Action == ActionReduce {
		// A reduction resets the unbought adds; the deficit itself is
		// recomputed from the new target on the next add.
		delete(e.leaderAdds, match.PosID)
	}
	e.leaderEntries[match.PosID] = leaderEntryObservation{size: match.LeaderPosition.Size, entry: match.LeaderPosition.EntryPrice}
}

// leaderAddReferencePrice accumulates this leader add and returns the VWAP of
// every add the follower has not bought yet. lastKnownSize is the leader size
// acknowledged before this change; followerQty the follower's live size.
func (e *Engine) leaderAddReferencePrice(match *SignalMatchResult, fill *Fill, lastKnownSize, followerQty float64) float64 {
	cur := match.LeaderPosition
	e.pricingMu.Lock()
	defer e.pricingMu.Unlock()
	if e.leaderAdds == nil {
		e.leaderAdds = make(map[string]*leaderAddAccumulator)
	}
	acc := e.leaderAdds[match.PosID]
	// A grown follower bought the earlier deficit; a smaller leader undid it.
	if acc == nil || followerQty > acc.followerQty+math.Max(1e-12, acc.followerQty*1e-9) || cur.Size < acc.leaderSize {
		acc = &leaderAddAccumulator{leaderSize: lastKnownSize, followerQty: followerQty}
		e.leaderAdds[match.PosID] = acc
	}
	if delta := cur.Size - acc.leaderSize; delta > 0 {
		price := fill.Price
		if prev, ok := e.leaderEntries[match.PosID]; ok && prev.size > 0 && prev.entry > 0 && cur.EntryPrice > 0 &&
			math.Abs(prev.size-acc.leaderSize) <= math.Max(1e-12, prev.size*1e-9) {
			// Average-cost accounting: the add price follows from the entry move.
			derived := (cur.EntryPrice*cur.Size - prev.entry*prev.size) / delta
			if derived > 0 && (fill.Price <= 0 || math.Abs(derived/fill.Price-1) <= 0.2) {
				price = derived
			}
		}
		if price > 0 {
			acc.leaderQty += delta
			acc.leaderNotional += delta * price
		}
		acc.leaderSize = cur.Size
	}
	if acc.leaderQty <= 0 {
		return fill.Price
	}
	return acc.leaderNotional / acc.leaderQty
}

// riskIncreasePriceGate decides whether an open/add executes now. It returns
// wait=true with a reason when the price is too far from the leader's; the
// caller defers the source revision for a re-check on the next poll.
func (e *Engine) riskIncreasePriceGate(fill *Fill, match *SignalMatchResult, dec *decision.Decision, copySize float64) (wait bool, note string) {
	if !e.proportionalGuardsApply() || e.getFollowerMarketPrice == nil || fill == nil || match == nil ||
		(match.Action != ActionOpen && match.Action != ActionAdd) || fill.CurrentPositionTaskID > 0 ||
		match.LeaderPosition == nil || copySize <= 0 || match.LeaderReversed {
		// A reversal is a risk decision like a stop: the new side follows at once.
		return false, ""
	}
	price, err := e.getFollowerMarketPrice(dec.Symbol)
	if err != nil || price <= 0 {
		return false, "" // the execution preflight re-reads the venue price
	}
	buying := strings.HasSuffix(dec.Action, "_long")
	tol := e.priceTolerancePct(fill.Symbol)
	leader := match.LeaderPosition

	if match.Action == ActionOpen {
		ref := leader.EntryPrice
		if ref <= 0 {
			ref = fill.Price
		}
		if ref <= 0 || copyPriceWithinTolerance(buying, price, ref, tol) {
			return false, fmt.Sprintf("首仓 | 参照价=%.6g 现价=%.6g", ref, price)
		}
		if e.store != nil && dec.ExecutionIntentID > 0 {
			if intent, getErr := e.store.CopyTrade().GetExecutionIntentByID(dec.ExecutionIntentID); getErr == nil &&
				!intent.CreatedAt.IsZero() && time.Since(intent.CreatedAt) >= initialOpenMaxWait {
				return false, fmt.Sprintf("首仓等待 %.0f 秒后按市价跟上 | 参照价=%.6g 现价=%.6g", initialOpenMaxWait.Seconds(), ref, price)
			}
		}
		return true, fmt.Sprintf("首仓价格偏离领航员开仓价超过 %.1f%% | 参照价=%.6g 现价=%.6g，最多等待 %.0f 秒", tol, ref, price, initialOpenMaxWait.Seconds())
	}

	if e.store == nil {
		return false, ""
	}
	mapping, err := e.store.CopyTrade().GetActiveMapping(e.traderID, match.PosID)
	if err != nil || mapping == nil {
		return false, ""
	}
	follower, err := e.readFollowerGroupPosition(mapping.Symbol, mapping.Side, mapping.MarginMode)
	if err != nil {
		return false, ""
	}
	ref := e.leaderAddReferencePrice(match, fill, mapping.LastKnownSize, follower.Quantity)
	scenario := "逆势补仓"
	if (buying && ref > leader.EntryPrice) || (!buying && ref < leader.EntryPrice) {
		scenario = "顺势加仓"
	}
	if ref <= 0 || copyPriceWithinTolerance(buying, price, ref, tol) {
		return false, fmt.Sprintf("%s | 参照价=%.6g 现价=%.6g", scenario, ref, price)
	}
	// Cost credit: when our average entry is better than the leader's, that
	// advantage may absorb a worse add price, up to costCreditMaxMultiple
	// times the tolerance. Without an advantage the plain tolerance applies.
	if follower.Quantity > 0 && follower.AvgEntry > 0 && leader.EntryPrice > 0 {
		advantagePct := (leader.EntryPrice - follower.AvgEntry) / leader.EntryPrice * 100
		if !buying {
			advantagePct = -advantagePct
		}
		if advantagePct > 0 {
			allowed := math.Min(tol+advantagePct, tol*costCreditMaxMultiple)
			if copyPriceWithinTolerance(buying, price, ref, allowed) {
				return false, fmt.Sprintf("%s（成本优势抵扣 %.2f%%）| 参照价=%.6g 现价=%.6g 我方均价=%.6g 领航员均价=%.6g", scenario, advantagePct, ref, price, follower.AvgEntry, leader.EntryPrice)
			}
		}
	}
	return true, fmt.Sprintf("%s价格偏离领航员加仓均价超过 %.1f%% | 参照价=%.6g 现价=%.6g，等待回到阈值内", scenario, tol, ref, price)
}

// logPriceWait throttles the per-poll wait log to once a minute per intent.
func (e *Engine) logPriceWait(dec *decision.Decision, note string) {
	e.pricingMu.Lock()
	if e.priceWaitLoggedAt == nil {
		e.priceWaitLoggedAt = make(map[int64]time.Time)
	}
	last := e.priceWaitLoggedAt[dec.ExecutionIntentID]
	due := time.Since(last) >= priceWaitLogInterval
	if due {
		for id, at := range e.priceWaitLoggedAt {
			if time.Since(at) > time.Hour {
				delete(e.priceWaitLoggedAt, id)
			}
		}
		e.priceWaitLoggedAt[dec.ExecutionIntentID] = time.Now()
	}
	e.pricingMu.Unlock()
	if due {
		logger.Infof("🎯 [%s] ⏳ 价格等待 | %s %s | %s", e.traderID, dec.Action, dec.Symbol, note)
	}
}
