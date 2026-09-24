package copytrade

import (
	"errors"
	"fmt"
	"math"
	"nofx/store"
	"nofx/trader"
	"strings"
)

const copyGuardRiskExitSource = "COPY_GUARD_RISK_EXIT"

// Called under protectionMu. Unlike ordinary leader intents, these intents
// cannot advance mappings or re-enter a stopped lifecycle during recovery.
func (ti *TraderIntegration) closeCopyGuardPosition(cycle *store.CopyGuardCycle) (string, error) {
	resolve := func() (float64, bool) {
		if ti.copyGuardOwnsPosition(cycle) {
			if err := ti.verifyCopyGuardContinuity(cycle); err != nil && !errors.Is(err, errCopyPositionEnded) {
				return 0, false
			}
		}
		return ti.copyGuardFollowerQuantity(cycle, true)
	}
	return ti.closeCopyGuardPositionWithQuantity(cycle, resolve, ti.copyGuardOwnsPosition(cycle))
}

// The acknowledgement, quantity budget and order identities are shared with
// exceptional AI entries whose lifecycle transaction has not committed yet.
func (ti *TraderIntegration) closeCopyGuardPositionWithQuantity(cycle *store.CopyGuardCycle, resolve func() (float64, bool), allowLegacy bool) (string, error) {
	ti.exitExecutionMu.Lock()
	defer ti.exitExecutionMu.Unlock()
	// Reconcile an earlier exit even when immutable fills already proved flat.
	// Custody release prevents another close; it does not erase its accounting.
	intents, err := ti.store.CopyTrade().ListExecutionIntentsByCycle(cycle.ID)
	if err != nil {
		return "", err
	}
	for _, intent := range intents {
		if intent.SourceKind == copyGuardRiskExitSource && intent.AttemptNo == cycle.ReentryCount {
			if e := ti.reconcileRiskExitIntent(intent); e != nil {
				return "", e
			}
		}
	}
	quantity, known := resolve()
	if !known {
		return "", fmt.Errorf("risk exit position scope is unknown")
	}
	if quantity <= 0 {
		return "", nil
	}
	closer, ok := ti.executor.(trader.CopyGuardScopedCloser)
	if !ok {
		// Compatibility for legacy in-process executors. AutoTrader always uses
		// the scoped interface and never falls back to a venue's generic close.
		if legacy, yes := ti.executor.(EmergencyPositionCloser); yes && allowLegacy && ti.copyGuardOwnsPosition(cycle) {
			return legacy.ClosePositionMarket(cycle.Symbol, cycle.Side)
		}
		return "", fmt.Errorf("scoped risk exit is unsupported")
	}
	cs := ti.store.CopyTrade()
	remaining, err := cs.RemainingCopyGuardExitQuantity(cycle.ID, cycle.ReentryCount, quantity)
	if err != nil {
		return "", err
	}
	quantity = math.Min(quantity, remaining)
	if quantity <= 1e-12 {
		return "", nil
	}
	intent, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: ti.traderID, LeaderPosID: cycle.LeaderPosID, SourceRevision: cycle.ID,
		SourceKind: copyGuardRiskExitSource, CanonicalKey: fmt.Sprintf("guard-exit|%d|%d", cycle.ID, cycle.ReentryCount),
		CycleID: cycle.ID, AttemptNo: cycle.ReentryCount, Action: fmt.Sprintf("risk_exit_%s_%d", cycle.Side, cycle.ReentryCount),
		Symbol: cycle.Symbol, Side: cycle.Side, MarginMode: cycle.MarginMode, TargetQuantity: quantity,
	})
	if err != nil {
		return "", err
	}
	attempts, err := cs.ListExecutionOrderAttempts(intent.ID)
	if err != nil {
		return "", err
	}
	sequence := 1
	clientID := ""
	previouslySubmitted := false
	if len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		sequence = last.AttemptNo
		clientID = last.ClientOrderID
		previouslySubmitted = last.SubmittedAt != nil
		if store.ExecutionOrderAttemptNeedsReconciliation(last) {
			return "", fmt.Errorf("risk exit acknowledgement is unresolved")
		}
		if last.Status == store.ExecutionOrderAttemptFilled || last.Status == store.ExecutionOrderAttemptTerminalNoFill {
			// Re-read AFTER terminal acknowledgement; the earlier snapshot may
			// predate that fill and must not cause another full-size close.
			quantity, known = resolve()
			if !known {
				return "", fmt.Errorf("risk exit residual is unknown")
			}
			if quantity <= 0 {
				return last.ExchangeOrderID, nil
			}
			remaining, budgetErr := cs.RemainingCopyGuardExitQuantity(cycle.ID, cycle.ReentryCount, quantity)
			if budgetErr != nil {
				return "", budgetErr
			}
			quantity = math.Min(quantity, remaining)
			if quantity <= 1e-12 {
				return last.ExchangeOrderID, nil
			}
			if last.SubmittedAt != nil {
				sequence++
				clientID = ""
				previouslySubmitted = false
			}
		}
	}
	if clientID == "" {
		clientID = fmt.Sprintf("cgx%xA%xN%x", cycle.ID, cycle.ReentryCount, sequence)
	}
	if _, err = cs.PrepareExecutionOrderAttemptRecordWithKind(intent.ID, clientID, "RISK_EXIT", quantity, quantity); err != nil {
		return "", err
	}
	submitted := false
	order, submitErr := closer.CloseCopyGuardPosition(trader.CopyGuardExitRequest{
		CycleID: cycle.ID, AttemptNo: cycle.ReentryCount, Symbol: cycle.Symbol, Side: cycle.Side,
		MarginMode: cycle.MarginMode, PositionID: cycle.FollowerPosID, Quantity: quantity, ClientOrderID: clientID,
		BeforeSubmit: func() error {
			residual, proven := resolve()
			if !proven || residual+1e-10 < quantity {
				return fmt.Errorf("risk exit residual authority unavailable")
			}
			_, boundaryErr := cs.MarkExecutionOrderAttemptSubmitted(intent.ID, clientID)
			submitted = boundaryErr == nil
			return boundaryErr
		},
	})
	orderID := getStringField(order, "orderId", "ordId")
	state, filled := exitReceipt(order)
	message := ""
	if submitErr != nil {
		message = submitErr.Error()
	}
	// An acknowledgement is not proof of fill. Always reconcile before using a
	// new identity, including transport errors and venue-specific payloads.
	completion := store.ExecutionOrderAttemptSubmitted
	if !submitted && !previouslySubmitted && submitErr != nil {
		completion = store.ExecutionOrderAttemptTerminalNoFill
	}
	if isTerminalExchangeOrderState(strings.ToUpper(state)) {
		if filled > 0 {
			completion = store.ExecutionOrderAttemptFilled
		} else if strings.EqualFold(state, "FILLED") {
			// Some market-close ACKs say FILLED before supplying executions.
			// Do not let that label mark a zero-quantity attempt terminal.
			state = ""
			message = "awaiting confirmed exit fill quantity"
		} else {
			completion = store.ExecutionOrderAttemptTerminalNoFill
		}
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, clientID, completion, orderID, state, message, filled); err != nil {
		return orderID, err
	}
	if err = cs.ReconcileCopyGuardExitIntent(intent.ID); err != nil {
		return orderID, err
	}
	return orderID, submitErr
}

func (ti *TraderIntegration) reconcileRiskExitIntent(intent *store.CopyTradeExecutionIntent) error {
	attempts, err := ti.store.CopyTrade().ListExecutionOrderAttempts(intent.ID)
	if err != nil {
		return err
	}
	for _, a := range attempts {
		if err = ti.reconcileExitOrderEvidence(intent.ID, a, intent.Symbol); err != nil {
			return err
		}
	}
	return ti.store.CopyTrade().ReconcileCopyGuardExitIntent(intent.ID)
}
