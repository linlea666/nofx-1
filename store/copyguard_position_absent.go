package store

import "fmt"

const CopyGuardProtectionPositionAbsent = "POSITION_ABSENT"
const CopyGuardProtectionFlatReconciling = "FLAT_RECONCILING"

// Flatness and order settlement are independent facts. This deliberately leaves
// the source mapping, custody and accounting untouched while exits reconcile.
func (s *CopyTradeStore) MarkCopyGuardFlatReconciling(cycleID int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE copy_guard_cycles SET protection_status=?,protection_coverage=0,
 protection_error='已空仓，退出对账中；等待原订单成交证据',protection_missing_at=NULL,updated_at=CURRENT_TIMESTAMP
 WHERE id=? AND closed_at IS NULL AND status IN ('FOLLOWING','FOLLOWING_REENTRY')
 AND EXISTS(SELECT 1 FROM copy_trade_execution_intents i WHERE i.trader_id=copy_guard_cycles.trader_id
 AND (i.leader_pos_id=copy_guard_cycles.leader_pos_id OR i.cycle_id=copy_guard_cycles.id)
 AND (i.source_kind='COPY_GUARD_RISK_EXIT' OR i.action IN ('reduce_long','reduce_short','close_long','close_short'))
 AND ((i.terminal_at IS NULL AND i.status IN ('RESERVED','SUBMITTED','PARTIALLY_FILLED','RECONCILING'))
 OR EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id AND `+unsettledExecutionAttemptSQL("a")+`)))
 AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_intents i WHERE i.trader_id=copy_guard_cycles.trader_id
 AND i.symbol=copy_guard_cycles.symbol AND i.side=copy_guard_cycles.side AND i.action IN ('open_long','open_short')
 AND ((i.terminal_at IS NULL AND i.status IN ('RESERVED','SUBMITTED','PARTIALLY_FILLED','RECONCILING'))
 OR EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id AND `+unsettledExecutionAttemptSQL("a")+`)))`, CopyGuardProtectionFlatReconciling, cycleID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

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
	if _, err = tx.Exec(`UPDATE copy_trade_position_custody SET state='RELEASED',reason='FOLLOWER_POSITION_ABSENT',released_at=COALESCE(released_at,CURRENT_TIMESTAMP) WHERE trader_id=? AND leader_pos_id=? AND cycle_id=?`, traderID, leaderPosID, cycleID); err != nil {
		return err
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
	if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET status='detached',last_failure_reason='FOLLOWER_POSITION_ABSENT',updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status IN ('active','manual_stopped')`, traderID, leaderPosID); err != nil {
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
