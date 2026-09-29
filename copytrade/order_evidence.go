package copytrade

import (
	"fmt"
	"math"
	"strings"

	"nofx/store"
	"nofx/trader"
)

// Missing/malformed quantities are not zero fills or flat positions. Keep the
// adapter's numeric contract, without changing legacy optional-field readers.
func knownQuantityField(data map[string]interface{}, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, exists := data[key]; exists {
			switch value.(type) {
			case float64, float32, int, int64:
				q := getFloatField(data, key)
				return q, !math.IsNaN(q) && !math.IsInf(q, 0)
			default:
				return 0, false
			}
		}
	}
	return 0, false
}

func exitReceipt(order map[string]interface{}) (string, float64) {
	state := strings.ToUpper(getStringField(order, "status", "state"))
	qty, known := knownQuantityField(order, "executedQty", "filled_quantity")
	// FILLED/0 remains explicitly unresolved in the store. Other terminal
	// labels must not imply a zero fill when the ACK omitted its quantity.
	if isTerminalExchangeOrderState(state) && state != "FILLED" && !known {
		state = ""
	}
	return state, qty
}

// Shared by leader and risk exits. Even a legacy terminal timestamp cannot
// suppress lookup when the persisted receipt lacks executed quantity.
func (ti *TraderIntegration) reconcileExitOrderEvidence(intentID int64, a *store.CopyTradeExecutionOrderAttempt, symbol string) error {
	lookup, _ := ti.executor.(ClientOrderStatusProvider)
	order, err := trader.ReconcileExitOrderEvidence(ti.store.CopyTrade(), lookup, intentID, a, symbol)
	if err == nil && order != nil {
		ti.observeExecutionFillTime(intentID, order)
	}
	return err
}

// Evidence-only repair: never executes a close, changes source progress, or
// resurrects a completed task. Active tasks resume via normal reconciliation.
func (ti *TraderIntegration) recoverExitOrderEvidence() {
	intents, err := ti.store.CopyTrade().ListExitOrderEvidenceGaps(ti.traderID, ti.lastExitEvidenceID)
	if err != nil {
		return
	}
	if len(intents) == 0 {
		ti.lastExitEvidenceID = 0
	}
	for _, intent := range intents {
		ti.lastExitEvidenceID = intent.ID
		attempts, e := ti.store.CopyTrade().ListExecutionOrderAttempts(intent.ID)
		if e == nil {
			for _, a := range attempts {
				if e = ti.reconcileExitOrderEvidence(intent.ID, a, intent.Symbol); e != nil {
					break
				}
			}
		}
		if e == nil {
			e = ti.store.CopyTrade().BookRecoveredCompletedExit(intent.ID)
		}
		key := fmt.Sprintf("exit-evidence:%d", intent.ID)
		if e != nil {
			_, _ = ti.store.CopyTrade().RecordRuntimeIssue(store.CopyRuntimeIssue{TraderID: ti.traderID, Area: "execution", ResourceID: key, LeaderPosID: intent.LeaderPosID, Symbol: intent.Symbol, Side: intent.Side, Code: "EXIT_EVIDENCE_PENDING", Detail: e.Error()})
		} else {
			_ = ti.store.CopyTrade().ResolveRuntimeIssue(ti.traderID, "execution", key)
		}
	}
}
