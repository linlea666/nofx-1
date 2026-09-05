package store

import (
	"database/sql"
	"fmt"
)

func CopyGuardRiskExitPending(status string) bool {
	return status == CopyGuardStopPendingFlat || status == CopyGuardStopPartial || status == CopyGuardStopTriggered
}

// Flat is only final after previously submitted leader orders have settled.
// A late add fill can otherwise arrive after STOPPED and escape residual exit.
func assertCopyGuardLeaderOrdersSettledTx(tx *sql.Tx, traderID, leaderPosID string) error {
	var pending int
	err := tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents i
		WHERE i.trader_id=? AND i.leader_pos_id=? AND i.source_kind<>'COPY_GUARD_RISK_EXIT'
		AND (EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id
			AND a.submitted_at IS NOT NULL AND a.terminal_at IS NULL AND a.status IN ('SUBMITTED','PARTIALLY_FILLED','UNKNOWN'))
		OR (i.status IN ('SUBMITTED','PARTIALLY_FILLED','RECONCILING') AND i.submitted_at IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id)))`, traderID, leaderPosID).Scan(&pending)
	if err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("Copy Guard flat settlement awaits %d submitted leader orders", pending)
	}
	return nil
}
