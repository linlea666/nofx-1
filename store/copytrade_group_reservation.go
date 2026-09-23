package store

import "fmt"

// DeferUnsubmittedSourceReservation keeps an incomplete local reservation
// recoverable without consuming its source revision or changing order identity.
// Evidence and the status update share one transaction so an exchange submit
// cannot be accidentally overwritten by this local failure path.
func (s *CopyTradeStore) DeferUnsubmittedSourceReservation(intentID int64, traderID, detail string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	facts, err := inspectLeaderTransitionTx(tx, intentID)
	if err != nil {
		return err
	}
	if facts.TraderID != traderID || facts.SourceKind != "LEADER_TRANSITION" || facts.Resolution != "" || facts.Effect != SourceEffectUnsubmitted {
		return ErrSourceTransitionUnresolved
	}
	if facts.Status != ExecutionIntentReserved {
		return fmt.Errorf("source reservation is no longer locally reserved")
	}
	result, err := tx.Exec(`UPDATE copy_trade_execution_intents SET status='RECONCILING',reason_code='SOURCE_REVALIDATION_REQUIRED',last_error=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND trader_id=? AND status='RESERVED'`, detail, intentID, traderID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSourceTransitionChanged
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='RECONCILING',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, intentID); err != nil {
		return err
	}
	return tx.Commit()
}
