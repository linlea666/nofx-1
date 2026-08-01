package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

const StoppedTraderFlatRetirementReason = "STOPPED_TRADER_AUTHORITATIVE_FLAT"

// RetireStoppedTraderCopyGuardState closes local Copy Guard state only after
// the caller has obtained fresh, complete exchange evidence that the account
// has no positions and no regular/conditional orders. The transaction repeats
// every local no-risk precondition so a concurrent submission fails closed.
func (s *CopyTradeStore) RetireStoppedTraderCopyGuardState(traderID, evidence string) (int, error) {
	traderID = strings.TrimSpace(traderID)
	if traderID == "" {
		return 0, fmt.Errorf("invalid stopped trader retirement")
	}
	if strings.TrimSpace(evidence) == "" {
		evidence = "fresh exchange positions=0; regular/algo pending orders=0"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var openPositions int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM trader_positions WHERE trader_id=? AND status='OPEN' AND ABS(quantity)>0`, traderID).Scan(&openPositions); err != nil {
		return 0, err
	}
	if openPositions > 0 {
		return 0, fmt.Errorf("stopped trader still has %d local open positions", openPositions)
	}
	// PREPARED proves that no adapter crossed its HTTP boundary. It is safe to
	// terminalize, but preserving the row keeps the exact abandoned quantity.
	if _, err = tx.Exec(`UPDATE copy_trade_execution_order_attempts SET
		status='TERMINAL_NO_FILL',last_error=?,terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE intent_id IN (SELECT id FROM copy_trade_execution_intents WHERE trader_id=?)
		  AND status='PREPARED' AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')='' AND COALESCE(filled_quantity,0)=0`,
		StoppedTraderFlatRetirementReason, traderID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP
		WHERE intent_id IN (SELECT id FROM copy_trade_execution_intents WHERE trader_id=? AND status IN ('RESERVED','RECONCILING','FAILED')
		  AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')='' AND COALESCE(filled_quantity,0)=0)`,
		ExecutionIntentCycleTerminatedBeforeSubmit, traderID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_execution_intents SET status='FAILED',reason_code=?,last_error=?,
		terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND status IN ('RESERVED','RECONCILING','FAILED') AND submitted_at IS NULL
		  AND COALESCE(exchange_order_id,'')='' AND COALESCE(filled_quantity,0)=0
		  AND `+noUnsafeExecutionAttemptSQL,
		ExecutionIntentCycleTerminatedBeforeSubmit, evidence, traderID); err != nil {
		return 0, err
	}

	var uncertain int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents i
		WHERE i.trader_id=? AND i.terminal_at IS NULL AND (
			i.status IN ('SUBMITTED','PARTIALLY_FILLED','RECONCILING')
			OR i.submitted_at IS NOT NULL OR COALESCE(i.exchange_order_id,'')<>'' OR COALESCE(i.filled_quantity,0)>0
			OR EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id AND (
				a.submitted_at IS NOT NULL OR COALESCE(a.exchange_order_id,'')<>'' OR COALESCE(a.filled_quantity,0)>0
				OR a.status IN ('SUBMITTED','PARTIALLY_FILLED','FILLED','UNKNOWN')
			))
		)`, traderID).Scan(&uncertain); err != nil {
		return 0, err
	}
	if uncertain > 0 {
		return 0, fmt.Errorf("stopped trader still has %d uncertain exchange execution intents", uncertain)
	}

	rows, err := tx.Query(`SELECT id FROM copy_guard_cycles WHERE trader_id=? AND closed_at IS NULL ORDER BY id`, traderID)
	if err != nil {
		return 0, err
	}
	var cycleIDs []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		cycleIDs = append(cycleIDs, id)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}

	if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET status='detached',last_known_size=0,
		last_failure_reason=?,consecutive_fail_count=0,updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND status IN ('active','stopped_by_risk','detached')`,
		StoppedTraderFlatRetirementReason, traderID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_protective_orders SET status='canceled',replacement_pending=0,
		previous_algo_id='',previous_algo_client_id='',updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND (LOWER(status)='live' OR replacement_pending=1)`, traderID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_attempts SET
		status=CASE WHEN status='OPEN' THEN 'DETACHED' ELSE status END,
		closed_at=CASE WHEN status='OPEN' THEN COALESCE(closed_at,CURRENT_TIMESTAMP) ELSE closed_at END,
		protection_status='CANCELED',protection_coverage=0,protection_updated_at=CURRENT_TIMESTAMP
		WHERE cycle_id IN (SELECT id FROM copy_guard_cycles WHERE trader_id=? AND closed_at IS NULL)`, traderID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET status=?,accounting_status=?,accounting_error=?,
		protection_status='CANCELED',protection_coverage=0,closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND closed_at IS NULL`, CopyGuardDetached, CopyGuardAccountingUnscorable,
		StoppedTraderFlatRetirementReason+": "+evidence, traderID); err != nil {
		return 0, err
	}
	for _, cycleID := range cycleIDs {
		if err = terminalizeCopyGuardAuxiliaryStateTx(tx, cycleID, CopyGuardDetached); err != nil {
			return 0, err
		}
		metadata := normalizeCopyGuardEventMetadata(cycleID, traderID, StoppedTraderFlatRetirementReason, map[string]interface{}{
			"reason_code": StoppedTraderFlatRetirementReason,
			"evidence":    evidence,
		})
		raw, _ := json.Marshal(metadata)
		if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,metadata_json)
			SELECT ?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM copy_guard_events WHERE cycle_id=? AND type=?)`,
			cycleID, traderID, StoppedTraderFlatRetirementReason, string(raw), cycleID, StoppedTraderFlatRetirementReason); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return len(cycleIDs), nil
}
