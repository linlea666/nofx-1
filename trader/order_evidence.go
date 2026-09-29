package trader

import (
	"fmt"
	"math"
	"strings"
	"time"

	"nofx/store"
)

// OrderEvidenceLookup deliberately exposes no order submission methods.
type OrderEvidenceLookup interface {
	GetOrderStatusByClientID(string, string) (map[string]interface{}, error)
}

// ReconcileExitOrderEvidence is shared by running and stopped reconciliation.
// It only queries the original order and records evidence; it never replays it.
func ReconcileExitOrderEvidence(st *store.CopyTradeStore, lookup OrderEvidenceLookup, intentID int64, a *store.CopyTradeExecutionOrderAttempt, symbol string) (map[string]interface{}, error) {
	if !store.ExecutionOrderAttemptNeedsReconciliation(a) {
		return nil, nil
	}
	if lookup == nil {
		return nil, fmt.Errorf("exit acknowledgement lookup unavailable")
	}
	order, err := lookup.GetOrderStatusByClientID(symbol, a.ClientOrderID)
	if err != nil {
		return nil, fmt.Errorf("order %s acknowledgement pending: %w", a.ClientOrderID, err)
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := order[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	if a.ExchangeOrderID != "" && str("orderId", "ordId") != a.ExchangeOrderID {
		return nil, fmt.Errorf("order %s lookup did not confirm the original exchange order identity", a.ClientOrderID)
	}
	if id := str("clientOrderId", "clOrdId"); id != "" && id != a.ClientOrderID {
		return nil, fmt.Errorf("exit lookup client order identity changed")
	}
	state := strings.ToUpper(str("status", "state"))
	var filled float64
	known := false
	for _, k := range []string{"executedQty", "filled_quantity"} {
		if value, exists := order[k]; exists {
			switch v := value.(type) {
			case float64:
				filled, known = v, true
			case float32:
				filled, known = float64(v), true
			case int:
				filled, known = float64(v), true
			case int64:
				filled, known = float64(v), true
			}
			break
		}
	}
	terminal := state == "FILLED" || state == "CANCELED" || state == "CANCELLED" || state == "REJECTED" || state == "EXPIRED" || state == "FAILED"
	if !terminal || !known || math.IsNaN(filled) || math.IsInf(filled, 0) || filled < 0 || (state == "FILLED" && filled <= 0) {
		return nil, fmt.Errorf("order %s outcome or fill quantity is not confirmed (%s)", a.ClientOrderID, state)
	}
	status := store.ExecutionOrderAttemptTerminalNoFill
	if filled > 0 {
		status = store.ExecutionOrderAttemptFilled
	}
	var filledAt time.Time
	if filled > 0 {
		for _, k := range []string{"fillTime", "fill_time", "updateTime"} {
			if ms := int64(getFloatFromMap(order, k)); ms > 0 {
				filledAt = time.UnixMilli(ms)
				break
			}
		}
	}
	err = st.CompleteExecutionOrderAttemptWithEvidence(intentID, a.ClientOrderID, status, str("orderId", "ordId"), state, "", filled, filledAt)
	return order, err
}

// RecoverStoppedExitEvidence drains all pages of historical gaps. Errors retain
// account fences and include the exact instruction requiring attention.
func RecoverStoppedExitEvidence(st *store.CopyTradeStore, traderID string, executor interface{}) error {
	lookup, _ := executor.(OrderEvidenceLookup)
	var after int64
	for {
		intents, err := st.ListExitOrderEvidenceGaps(traderID, after)
		if err != nil {
			return err
		}
		if len(intents) == 0 {
			return nil
		}
		for _, intent := range intents {
			after = intent.ID
			attempts, err := st.ListExecutionOrderAttempts(intent.ID)
			if err != nil {
				return err
			}
			for _, a := range attempts {
				if _, err = ReconcileExitOrderEvidence(st, lookup, intent.ID, a, intent.Symbol); err != nil {
					return fmt.Errorf("intent %d: %w", intent.ID, err)
				}
			}
			if err = st.BookRecoveredCompletedExit(intent.ID); err != nil {
				return err
			}
			_ = st.ResolveRuntimeIssue(traderID, "execution", fmt.Sprintf("exit-evidence:%d", intent.ID))
		}
	}
}

// NewAccountQueryTrader does not set leverage, margin mode or position mode.
func NewAccountQueryTrader(exchange *store.Exchange, userID string) (Trader, error) {
	if exchange.UserID != userID {
		return nil, fmt.Errorf("execution account ownership mismatch")
	}
	if exchange.APIKey == "" || exchange.SecretKey == "" {
		return nil, fmt.Errorf("execution account %s credentials unavailable", exchange.ID)
	}
	switch exchange.ExchangeType {
	case "okx":
		if exchange.Passphrase == "" {
			return nil, fmt.Errorf("OKX passphrase unavailable")
		}
		return NewOKXQueryTrader(exchange.APIKey, exchange.SecretKey, exchange.Passphrase), nil
	case "binance":
		return newFuturesQueryTrader(exchange.APIKey, exchange.SecretKey, userID), nil
	default:
		return nil, fmt.Errorf("exchange %s does not support authoritative account queries", exchange.ExchangeType)
	}
}
