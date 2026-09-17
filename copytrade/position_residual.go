package copytrade

import (
	"fmt"
	"math"
	"strings"

	"nofx/store"
	"nofx/trader"
)

// Release ends authority over the venue's position. An order that crossed our
// submission boundary before the stop can nevertheless fill after that flat.
// Only its proven remaining quantity may be exited; manual entries add no budget.
func (ti *TraderIntegration) releasedCopyGuardResidual(c *store.CopyGuardCycle, fresh bool) (float64, bool) {
	fills, entryID, err := ti.loadCopyGuardContinuity(c)
	if err != nil {
		return 0, false
	}
	intents, err := ti.store.CopyTrade().ListExecutionIntentsByCycle(c.ID)
	if err != nil {
		return 0, false
	}
	ownedOpens := map[string]bool{}
	receipts := map[string]float64{}
	entryTime := c.OpenedAt
	for _, f := range orderedContinuityFills(fills) {
		if f.OrderID == entryID {
			entryTime = f.Time
			break
		}
	}
	for _, i := range intents {
		if i.SourceKind != "LEADER_TRANSITION" && i.AttemptNo != c.ReentryCount {
			continue
		}
		// Earlier attempts of an ATR lifecycle are outside this entry's proof.
		if c.ReentryCount > 0 && i.SourceKind == "LEADER_TRANSITION" && i.TerminalAt != nil && i.TerminalAt.Before(entryTime) {
			continue
		}
		attempts, e := ti.store.CopyTrade().ListExecutionOrderAttempts(i.ID)
		if e != nil {
			return 0, false
		}
		isOpen := i.SourceKind == "LEADER_TRANSITION" && (i.Action == "open_long" || i.Action == "open_short")
		if len(attempts) == 0 && i.FilledQuantity > 0 {
			if i.ExchangeOrderID == "" {
				return 0, false
			}
			receipts[i.ExchangeOrderID] = i.FilledQuantity
			ownedOpens[i.ExchangeOrderID] = isOpen
		}
		for _, a := range attempts {
			if a.ExchangeOrderID == "" {
				if a.FilledQuantity > 0 {
					return 0, false
				}
				continue
			}
			receipts[a.ExchangeOrderID] = a.FilledQuantity
			ownedOpens[a.ExchangeOrderID] = isOpen
		}
	}
	quantity, err := ownedQuantityAfterFlat(fills, entryID, c.Symbol, c.Side, ownedOpens, receipts)
	if err != nil {
		return 0, false
	}
	if quantity == 0 {
		return 0, true
	}
	actual, known := ti.followerPositionQuantity(c.Symbol, c.Side, c.MarginMode, c.FollowerPosID, fresh)
	if !known {
		return 0, false
	}
	return math.Min(quantity, actual), true
}

func ownedQuantityAfterFlat(fills []trader.TradeRecord, entryID, symbol, side string, ownedOpens map[string]bool, receipts map[string]float64) (float64, error) {
	return ownedFillQuantity(fills, entryID, symbol, side, ownedOpens, receipts, true)
}

func ownedFillQuantity(fills []trader.TradeRecord, entryID, symbol, side string, ownedOpens map[string]bool, receipts map[string]float64, afterFlat bool) (float64, error) {
	if afterFlat {
		ended, err := positionEndedInFills(fills, entryID, symbol, side)
		if err != nil || !ended {
			return 0, fmt.Errorf("released position requires a proven original flat: %v", err)
		}
	}
	seen := map[string]bool{}
	filledByOrder := map[string]float64{}
	started, flat := false, !afterFlat
	net, owned := 0.0, 0.0
	for _, f := range orderedContinuityFills(fills) {
		if !strings.EqualFold(f.Symbol, symbol) || !strings.EqualFold(f.PositionSide, side) || seen[f.TradeID] {
			continue
		}
		if f.TradeID == "" || f.OrderID == "" || f.Quantity <= 0 || math.IsNaN(f.Quantity) || math.IsInf(f.Quantity, 0) || (!strings.EqualFold(f.Side, "BUY") && !strings.EqualFold(f.Side, "SELL")) {
			return 0, fmt.Errorf("invalid immutable residual fill")
		}
		seen[f.TradeID] = true
		filledByOrder[f.OrderID] += f.Quantity
		if !started {
			if f.OrderID != entryID {
				continue
			}
			started = true
		}
		increase := strings.EqualFold(f.Side, "BUY") == strings.EqualFold(side, "long")
		if !flat {
			if increase {
				net += f.Quantity
			} else {
				net -= f.Quantity
			}
			flat = net <= 1e-10
			continue
		}
		if increase {
			if ownedOpens[f.OrderID] {
				owned += f.Quantity
			}
		} else {
			// Reductions are fungible at the venue. Attribute them to our slice
			// first, so uncertainty can never increase a close of manual funds.
			owned = math.Max(0, owned-f.Quantity)
		}
	}
	if !started {
		return 0, fmt.Errorf("confirmed entry absent from immutable history")
	}
	for id, quantity := range receipts {
		if filledByOrder[id]+math.Max(1e-10, quantity*1e-8) < quantity {
			return 0, fmt.Errorf("immutable history has not caught up with order %s", id)
		}
	}
	return owned, nil
}
