package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// A venue label is not fill evidence. In particular, historical OKX market
// close acknowledgements said FILLED without an executed quantity.
func executionOrderAttemptTerminal(status, state string, quantity float64) bool {
	if !finiteNonnegative(quantity) {
		return false
	}
	status, state = strings.ToUpper(strings.TrimSpace(status)), strings.ToUpper(strings.TrimSpace(state))
	if state == "FILLED" || status == ExecutionOrderAttemptFilled {
		return quantity > 0 && (state == "" || state == "FILLED" || state == "CANCELED" || state == "CANCELLED" || state == "EXPIRED" || state == "REJECTED" || state == "FAILED")
	}
	switch state {
	case "CANCELED", "CANCELLED", "REJECTED", "EXPIRED", "FAILED":
		return true
	case "":
		return status == ExecutionOrderAttemptFailed || status == ExecutionOrderAttemptTerminalNoFill
	default:
		return false
	}
}

func ExecutionOrderAttemptNeedsReconciliation(a *CopyTradeExecutionOrderAttempt) bool {
	return a != nil && (a.SubmittedAt != nil || a.ExchangeOrderID != "") &&
		(a.TerminalAt == nil || !executionOrderAttemptTerminal(a.Status, a.ExchangeState, a.FilledQuantity))
}

// SQL equivalent for historical contradictory FILLED receipts. Keep this
// shared by submission, lifecycle/account fences and late-entry checks.
// The caller supplies the attempt alias, never user input.
func unsettledExecutionAttemptSQL(alias string) string {
	state := "UPPER(TRIM(COALESCE(" + alias + ".exchange_state,'')))"
	status := "UPPER(TRIM(" + alias + ".status))"
	qty := "COALESCE(" + alias + ".filled_quantity,-1)"
	terminal := "(" + qty + ">=0 AND " + qty + "<=1.7976931348623157e308 AND " +
		"((" + state + "='FILLED' AND " + qty + ">0) OR (" + state + " IN ('CANCELED','CANCELLED','REJECTED','EXPIRED','FAILED') AND (" + status + "<>'FILLED' OR " + qty + ">0)) OR (" + state + "='' AND (" + status + " IN ('FAILED','TERMINAL_NO_FILL') OR (" + status + "='FILLED' AND " + qty + ">0)))))"
	return "((" + alias + ".submitted_at IS NOT NULL OR COALESCE(" + alias + ".exchange_order_id,'')<>'') AND (" + alias + ".terminal_at IS NULL OR NOT " + terminal + "))"
}

func (s *CopyTradeStore) initOrderEvidenceRepairTable() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_order_evidence_repairs (
 attempt_id INTEGER PRIMARY KEY,intent_id INTEGER NOT NULL,before_json TEXT NOT NULL,
 after_json TEXT NOT NULL,observed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,booked_at DATETIME);`)
	return err
}

// Confirmed lookup evidence can repair a contradictory old terminal receipt.
// No original row is deleted; the repair and its before/after audit commit
// together. filledAt is supplied only by a venue fill timestamp, never a guess.
func (s *CopyTradeStore) CompleteExecutionOrderAttemptWithEvidence(intentID int64, clientOrderID, status, exchangeOrderID, exchangeState, lastError string, filledQuantity float64, filledAt time.Time) error {
	if intentID <= 0 || strings.TrimSpace(clientOrderID) == "" || status == "" || !finiteNonnegative(filledQuantity) {
		return fmt.Errorf("invalid execution order attempt update")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := scanExecutionOrderAttempt(tx.QueryRow(`SELECT id,intent_id,attempt_no,client_order_id,COALESCE(quantity_kind,''),requested_quantity,quantized_quantity,filled_quantity,exchange_order_id,exchange_state,status,last_error,created_at,updated_at,submitted_at,filled_at,terminal_at FROM copy_trade_execution_order_attempts WHERE intent_id=? AND client_order_id=?`, intentID, clientOrderID))
	if err != nil {
		return err
	}
	if exchangeOrderID != "" && before.ExchangeOrderID != "" && exchangeOrderID != before.ExchangeOrderID {
		return fmt.Errorf("execution order identity changed")
	}
	if filledQuantity < before.FilledQuantity {
		filledQuantity = before.FilledQuantity
	}
	if exchangeState == "" {
		exchangeState = before.ExchangeState
	}
	terminal := executionOrderAttemptTerminal(status, exchangeState, filledQuantity)
	if strings.EqualFold(exchangeState, "FILLED") {
		if filledQuantity > 0 {
			status = ExecutionOrderAttemptFilled
		} else if status == ExecutionOrderAttemptFilled {
			status = ExecutionOrderAttemptUnknown
		}
	}
	// A late ACK must not erase already confirmed terminal evidence.
	if !terminal && !ExecutionOrderAttemptNeedsReconciliation(before) && before.TerminalAt != nil {
		status, exchangeState, terminal = before.Status, before.ExchangeState, true
	}
	var at interface{}
	if filledQuantity > 0 && !filledAt.IsZero() {
		at = filledAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = tx.Exec(`UPDATE copy_trade_execution_order_attempts SET status=?,
 exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,exchange_state=?,
 filled_quantity=?,last_error=?,
 submitted_at=CASE WHEN ? IN ('SUBMITTED','PARTIALLY_FILLED','FILLED') THEN COALESCE(submitted_at,CURRENT_TIMESTAMP) ELSE submitted_at END,
 filled_at=CASE WHEN ? IS NOT NULL THEN ? WHEN ?>0 THEN COALESCE(filled_at,CURRENT_TIMESTAMP) ELSE filled_at END,
 terminal_at=CASE WHEN ? THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE NULL END,updated_at=CURRENT_TIMESTAMP
 WHERE intent_id=? AND client_order_id=?`, status, exchangeOrderID, exchangeOrderID, exchangeState, filledQuantity, lastError, status, at, at, filledQuantity, terminal, intentID, clientOrderID)
	if err != nil {
		return err
	}
	if before.TerminalAt != nil && ExecutionOrderAttemptNeedsReconciliation(before) && terminal {
		raw, e := json.Marshal(before)
		if e != nil {
			return e
		}
		after, e := scanExecutionOrderAttempt(tx.QueryRow(`SELECT id,intent_id,attempt_no,client_order_id,COALESCE(quantity_kind,''),requested_quantity,quantized_quantity,filled_quantity,exchange_order_id,exchange_state,status,last_error,created_at,updated_at,submitted_at,filled_at,terminal_at FROM copy_trade_execution_order_attempts WHERE id=?`, before.ID))
		if e != nil {
			return e
		}
		updated, e := json.Marshal(after)
		if e != nil {
			return e
		}
		if _, err = tx.Exec(`INSERT INTO copy_trade_order_evidence_repairs(attempt_id,intent_id,before_json,after_json) VALUES(?,?,?,?) ON CONFLICT(attempt_id) DO NOTHING`, before.ID, intentID, string(raw), string(updated)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Include completed historical exits without replaying their business action.
func (s *CopyTradeStore) ListExitOrderEvidenceGaps(traderID string, afterID int64) ([]*CopyTradeExecutionIntent, error) {
	rows, err := s.db.Query(`SELECT DISTINCT i.id FROM copy_trade_execution_intents i JOIN copy_trade_execution_order_attempts a ON a.intent_id=i.id
 WHERE i.trader_id=? AND i.id>? AND ((UPPER(COALESCE(a.exchange_state,''))='FILLED' AND a.filled_quantity=0)
 OR EXISTS(SELECT 1 FROM copy_trade_order_evidence_repairs r WHERE r.intent_id=i.id AND r.booked_at IS NULL))
 AND (i.source_kind='COPY_GUARD_RISK_EXIT' OR EXISTS(SELECT 1 FROM copy_trade_leader_exits e WHERE e.intent_id=i.id)) ORDER BY i.id LIMIT 25`, traderID, afterID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var result []*CopyTradeExecutionIntent
	for _, id := range ids {
		i, e := s.GetExecutionIntentByID(id)
		if e != nil {
			return nil, e
		}
		result = append(result, i)
	}
	return result, nil
}

// Closed business tasks are never reopened by historical evidence recovery.
func (s *CopyTradeStore) BookRecoveredCompletedExit(intentID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done bool
	err = tx.QueryRow(`SELECT completed FROM copy_trade_leader_exits WHERE intent_id=?`, intentID).Scan(&done)
	if err == sql.ErrNoRows {
		// Historical risk exits can gain quantities without reopening their
		// completed risk lifecycle or replaying its bookkeeping side effects.
		err = tx.QueryRow(`SELECT terminal_at IS NOT NULL FROM copy_trade_execution_intents WHERE id=? AND source_kind='COPY_GUARD_RISK_EXIT'`, intentID).Scan(&done)
		if err == sql.ErrNoRows {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if !done {
		return nil
	}
	qty, err := bookTerminalLeaderExitAttemptsTx(tx, intentID)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_execution_intents SET filled_quantity=? WHERE id=? AND filled_quantity<=?`, qty, intentID, qty); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_order_evidence_repairs SET booked_at=COALESCE(booked_at,CURRENT_TIMESTAMP) WHERE intent_id=?`, intentID); err != nil {
		return err
	}
	return tx.Commit()
}
