package copytrade

import (
	"fmt"
	"math"

	"nofx/decision"
	"nofx/store"
)

// A failed post-fill transaction leaves custody on the previous attempt. Its
// absence must not claim that the new AI entry was exited. Prove that entry's
// own fills and reuse the normal durable, scoped exit submission machinery.
func (ti *TraderIntegration) uncommittedReentryExitScope(dec *decision.Decision, cycle *store.CopyGuardCycle) (*store.CopyGuardCycle, func() (float64, bool), error) {
	i, err := ti.store.CopyTrade().GetExecutionIntentByID(dec.ExecutionIntentID)
	if err != nil {
		return nil, nil, err
	}
	if i.SourceKind != "AI_REENTRY" || i.CycleID != cycle.ID || i.AttemptNo != cycle.ReentryCount+1 || i.TraderID != ti.traderID || dec.ExchangeOrderID == "" || dec.FilledQuantity <= 0 {
		return nil, nil, fmt.Errorf("uncommitted reentry identity is not proven")
	}
	scope := *cycle
	scope.ReentryCount = i.AttemptNo
	scope.EntryOrderID = dec.ExchangeOrderID
	scope.OpenedAt = i.CreatedAt
	resolve := func() (float64, bool) {
		fills, entryID, e := ti.loadCopyGuardContinuity(&scope)
		if e != nil {
			return 0, false
		}
		owned := map[string]bool{dec.ExchangeOrderID: true}
		receipts := map[string]float64{dec.ExchangeOrderID: dec.FilledQuantity}
		attempts, e := ti.store.CopyTrade().ListExecutionOrderAttempts(i.ID)
		if e != nil {
			return 0, false
		}
		for _, a := range attempts {
			if a.ExchangeOrderID != "" {
				owned[a.ExchangeOrderID] = true
				receipts[a.ExchangeOrderID] = math.Max(receipts[a.ExchangeOrderID], a.FilledQuantity)
			}
		}
		exits, e := ti.store.CopyTrade().ListExecutionIntentsByCycle(cycle.ID)
		if e != nil {
			return 0, false
		}
		for _, exit := range exits {
			if exit.SourceKind != copyGuardRiskExitSource || exit.AttemptNo != i.AttemptNo {
				continue
			}
			orders, e := ti.store.CopyTrade().ListExecutionOrderAttempts(exit.ID)
			if e != nil {
				return 0, false
			}
			for _, a := range orders {
				if a.FilledQuantity > 0 {
					receipts[a.ExchangeOrderID] = a.FilledQuantity
				}
			}
		}
		quantity, e := ownedFillQuantity(fills, entryID, scope.Symbol, scope.Side, owned, receipts, false)
		if e != nil {
			return 0, false
		}
		if quantity <= 0 {
			return 0, true
		}
		actual, known := ti.followerPositionQuantity(scope.Symbol, scope.Side, scope.MarginMode, scope.FollowerPosID, true)
		return math.Min(quantity, actual), known
	}
	return &scope, resolve, nil
}
