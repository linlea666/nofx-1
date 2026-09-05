package copytrade

import (
	"errors"
	"fmt"
	"nofx/store"
	"nofx/trader"
	"strings"
)

const copyGuardRiskExitSource = "COPY_GUARD_RISK_EXIT"

// Called under protectionMu. Unlike ordinary leader intents, these intents
// cannot advance mappings or re-enter a stopped lifecycle during recovery.
func (ti *TraderIntegration) closeCopyGuardPosition(cycle *store.CopyGuardCycle) (string, error) {
	closer, ok := ti.executor.(trader.CopyGuardScopedCloser)
	if !ok {
		// Compatibility for legacy in-process executors. AutoTrader always uses
		// the scoped interface and never falls back to a venue's generic close.
		if legacy, yes := ti.executor.(EmergencyPositionCloser); yes {
			return legacy.ClosePositionMarket(cycle.Symbol, cycle.Side)
		}
		return "", fmt.Errorf("scoped risk exit is unsupported")
	}
	quantity, known := ti.followerPositionQuantity(cycle.Symbol, cycle.Side, cycle.MarginMode, cycle.FollowerPosID, true)
	if !known {
		return "", fmt.Errorf("risk exit position scope is unknown")
	}
	if quantity <= 0 {
		return "", nil
	}
	cs := ti.store.CopyTrade()
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
		if last.SubmittedAt != nil && last.Status != store.ExecutionOrderAttemptFilled && last.Status != store.ExecutionOrderAttemptTerminalNoFill {
			lookup, yes := ti.executor.(ClientOrderStatusProvider)
			if !yes {
				return "", fmt.Errorf("risk exit acknowledgement lookup unavailable")
			}
			order, lookupErr := lookup.GetOrderStatusByClientID(cycle.Symbol, clientID)
			if lookupErr != nil && !errors.Is(lookupErr, trader.ErrExecutionOrderNotFound) {
				return "", fmt.Errorf("risk exit acknowledgement pending: %w", lookupErr)
			}
			if lookupErr == nil {
				state := strings.ToUpper(getStringField(order, "status", "state"))
				if !isTerminalExchangeOrderState(state) {
					return getStringField(order, "orderId", "ordId"), fmt.Errorf("risk exit order is still %s", state)
				}
				filled := getFloatField(order, "executedQty", "filled_quantity", "accFillSz")
				status := store.ExecutionOrderAttemptTerminalNoFill
				if filled > 0 || state == "FILLED" {
					status = store.ExecutionOrderAttemptFilled
				}
				if err = cs.CompleteExecutionOrderAttempt(intent.ID, clientID, status, getStringField(order, "orderId", "ordId"), state, "", filled); err != nil {
					return "", err
				}
				last.Status = status
			}
		}
		if last.Status == store.ExecutionOrderAttemptFilled || last.Status == store.ExecutionOrderAttemptTerminalNoFill {
			// Re-read AFTER terminal acknowledgement; the earlier snapshot may
			// predate that fill and must not cause another full-size close.
			quantity, known = ti.followerPositionQuantity(cycle.Symbol, cycle.Side, cycle.MarginMode, cycle.FollowerPosID, true)
			if !known {
				return "", fmt.Errorf("risk exit residual is unknown")
			}
			if quantity <= 0 {
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
			_, boundaryErr := cs.MarkExecutionOrderAttemptSubmitted(intent.ID, clientID)
			submitted = boundaryErr == nil
			return boundaryErr
		},
	})
	orderID := getStringField(order, "orderId", "ordId")
	state := getStringField(order, "status", "state")
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
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, clientID, completion, orderID, state, message, 0); err != nil {
		return orderID, err
	}
	return orderID, submitErr
}
