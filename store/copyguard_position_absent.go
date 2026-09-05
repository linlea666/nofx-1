package store

import "fmt"

const CopyGuardProtectionPositionAbsent = "POSITION_ABSENT"

// MarkCopyGuardFollowerAbsent is NOT a risk exit. The caller must freshly
// confirm the exact position is flat, outside any fill-settlement window.
// Keep the lifecycle open until the leader closes, but prevent a later add
// from manufacturing a new follower entry into that old lifecycle.
func (s *CopyTradeStore) MarkCopyGuardFollowerAbsent(cycleID int64, traderID, leaderPosID string) error {
	if cycleID <= 0 || traderID == "" || leaderPosID == "" {
		return fmt.Errorf("invalid absent-position scope")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pending int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents WHERE trader_id=? AND leader_pos_id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED')`, traderID, leaderPosID).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("position absence cannot be committed during execution reconciliation")
	}
	res, err := tx.Exec(`UPDATE copy_guard_cycles SET protection_status=?,protection_coverage=0,protection_error='fresh follower position absent without stop evidence',accounting_status=?,accounting_error='FOLLOWER_POSITION_ABSENT',updated_at=CURRENT_TIMESTAMP WHERE id=? AND trader_id=? AND leader_pos_id=? AND closed_at IS NULL AND status IN ('FOLLOWING','FOLLOWING_REENTRY') AND protection_status<>?`, CopyGuardProtectionPositionAbsent, CopyGuardAccountingUnscorable, cycleID, traderID, leaderPosID, CopyGuardProtectionPositionAbsent)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return tx.Commit()
	}
	if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET status='detached',last_failure_reason='FOLLOWER_POSITION_ABSENT',updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='active'`, traderID, leaderPosID); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,metadata_json) VALUES(?,?,'FOLLOWER_POSITION_ABSENT','{"stop_evidence":false,"automatic_reopen":false}')`, cycleID, traderID); err != nil {
		return err
	}
	return tx.Commit()
}

// Persist what the venue actually returned without disturbing replacement
// ownership or setting a triggered order terminal before exit settlement.
func (s *CopyTradeStore) ObserveCopyGuardProtectiveOrder(o *CopyGuardProtectiveOrder) error {
	_, err := s.db.Exec(`UPDATE copy_guard_protective_orders SET symbol=?,side=?,margin_mode=?,quantity=?,trigger_price=?,trigger_type=?,coverage_mode=?,updated_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND algo_id=?`, o.Symbol, o.Side, o.MarginMode, o.Quantity, o.TriggerPrice, o.TriggerType, o.CoverageMode, o.CycleID, o.AlgoID)
	return err
}
