package store

import (
	"database/sql"
	"fmt"
	"math"
)

// RemainingCopyGuardExitQuantity is bounded by the exposure present when the
// exit began plus subsequently acknowledged leader fills, minus actual exits.
// A new manual position must never increase this budget.
func (s *CopyTradeStore) RemainingCopyGuardExitQuantity(cycleID int64, attempt int, initialQuantity float64) (float64, error) {
	if invalidPositiveNumber(initialQuantity) {
		return 0, fmt.Errorf("invalid risk exit quantity")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var net float64
	if err = tx.QueryRow(`SELECT COALESCE(SUM(CASE
	 WHEN source_kind='AI_REENTRY' THEN MAX(filled_quantity,COALESCE((SELECT SUM(a.filled_quantity) FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id),0))
	 WHEN action IN ('open_long','open_short') THEN filled_quantity
	 WHEN action IN ('reduce_long','reduce_short','close_long','close_short') THEN -filled_quantity ELSE 0 END),0)
	 FROM copy_trade_execution_intents i WHERE cycle_id=? AND (source_kind='LEADER_TRANSITION' OR (source_kind='AI_REENTRY' AND attempt_no=?))`, cycleID, attempt).Scan(&net); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO copy_guard_exit_budgets(cycle_id,attempt_no,initial_quantity,leader_net_at_start) VALUES(?,?,?,?)`, cycleID, attempt, initialQuantity, net); err != nil {
		return 0, err
	}
	var initial, baseline, exited float64
	if err = tx.QueryRow(`SELECT initial_quantity,leader_net_at_start FROM copy_guard_exit_budgets WHERE cycle_id=? AND attempt_no=?`, cycleID, attempt).Scan(&initial, &baseline); err != nil {
		return 0, err
	}
	if err = tx.QueryRow(`SELECT COALESCE(SUM(a.filled_quantity),0) FROM copy_trade_execution_order_attempts a JOIN copy_trade_execution_intents i ON i.id=a.intent_id WHERE i.cycle_id=? AND i.attempt_no=? AND i.source_kind='COPY_GUARD_RISK_EXIT'`, cycleID, attempt).Scan(&exited); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return math.Max(0, initial+net-baseline-exited), nil
}

func (s *CopyTradeStore) FinalizeUncommittedReentryExit(cycleID int64, attempt int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pending int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts a JOIN copy_trade_execution_intents i ON i.id=a.intent_id WHERE i.cycle_id=? AND i.attempt_no=? AND i.source_kind='AI_REENTRY' AND a.submitted_at IS NOT NULL AND a.terminal_at IS NULL`, cycleID, attempt).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("reentry entry acknowledgements remain pending")
	}
	if err = finalizeCopyGuardExitIntentsTx(tx, cycleID, attempt, "REENTRY_COMMIT_FAILURE_EXIT_CONFIRMED"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CopyTradeStore) ReconcileCopyGuardExitIntent(intentID int64) error {
	_, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET
	 filled_quantity=(SELECT COALESCE(SUM(filled_quantity),0) FROM copy_trade_execution_order_attempts WHERE intent_id=?),
	 exchange_order_id=COALESCE((SELECT exchange_order_id FROM copy_trade_execution_order_attempts WHERE intent_id=? AND exchange_order_id<>'' ORDER BY attempt_no DESC LIMIT 1),exchange_order_id),
	 status=CASE WHEN EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts WHERE intent_id=? AND submitted_at IS NOT NULL AND terminal_at IS NULL) THEN 'RECONCILING'
	 WHEN EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts WHERE intent_id=? AND filled_quantity>0) THEN 'PARTIALLY_FILLED' ELSE 'RESERVED' END,
	 filled_at=CASE WHEN EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts WHERE intent_id=? AND filled_quantity>0) THEN COALESCE(filled_at,CURRENT_TIMESTAMP) ELSE filled_at END,
	 updated_at=CURRENT_TIMESTAMP WHERE id=? AND source_kind='COPY_GUARD_RISK_EXIT' AND terminal_at IS NULL`, intentID, intentID, intentID, intentID, intentID, intentID)
	return err
}

// A flat snapshot cannot manufacture a filled exit. Retain actual fill evidence
// and refuse settlement while any submitted exit still has an uncertain ACK.
func finalizeCopyGuardExitIntentsTx(tx interface {
	Exec(string, ...interface{}) (sql.Result, error)
	QueryRow(string, ...interface{}) *sql.Row
}, cycleID int64, attempt int, reason string) error {
	var pending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts a JOIN copy_trade_execution_intents i ON i.id=a.intent_id WHERE i.cycle_id=? AND i.attempt_no=? AND i.source_kind='COPY_GUARD_RISK_EXIT' AND a.submitted_at IS NOT NULL AND a.terminal_at IS NULL`, cycleID, attempt).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("risk exit settlement awaits %d order acknowledgements", pending)
	}
	_, err := tx.Exec(`UPDATE copy_trade_execution_intents SET
	 filled_quantity=(SELECT COALESCE(SUM(filled_quantity),0) FROM copy_trade_execution_order_attempts WHERE intent_id=copy_trade_execution_intents.id),
	 exchange_order_id=COALESCE((SELECT exchange_order_id FROM copy_trade_execution_order_attempts WHERE intent_id=copy_trade_execution_intents.id AND exchange_order_id<>'' ORDER BY attempt_no DESC LIMIT 1),exchange_order_id),
	 status=CASE WHEN EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts WHERE intent_id=copy_trade_execution_intents.id AND filled_quantity>0) THEN 'FILLED' ELSE 'SKIPPED' END,
	 reason_code=?,terminal_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND attempt_no=? AND source_kind='COPY_GUARD_RISK_EXIT'`, reason, cycleID, attempt)
	return err
}
