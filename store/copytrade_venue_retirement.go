package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// StoppedTraderFlatSnapshot describes successful, uncached account-wide reads.
// It proves current risk is absent, not how a historical order was executed.
type StoppedTraderFlatSnapshot struct {
	ExchangeID       string
	TraderGeneration int64
	ObservedAt       time.Time // Before either exchange request starts.
	PositionsEmpty   bool
	OrdersEmpty      bool // Both ordinary and conditional orders were read.
}

func (s *CopyTradeStore) initVenueRetirementTable() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_venue_retirements (
 intent_id INTEGER PRIMARY KEY,trader_id TEXT NOT NULL,exchange_id TEXT NOT NULL,
 trader_generation INTEGER NOT NULL,intent_updated_at TEXT NOT NULL,
 client_order_id TEXT NOT NULL,canonical_key TEXT NOT NULL,
 observed_at DATETIME NOT NULL,retired_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 evidence TEXT NOT NULL,before_json TEXT NOT NULL
 );`)
	return err
}

// Only the historical mapping-acknowledged close format is eligible. Unknown
// submissions, real fills, modern attempts and active source mappings are not.
// All callers use alias i; this is shared by retirement and account ownership.
const legacyAcknowledgedCloseSQL = `(
 i.source_kind='LEADER_TRANSITION' AND i.action IN ('close_long','close_short')
 AND i.status='FILLED' AND i.reason_code='MAPPING_ALREADY_ACKNOWLEDGED'
 AND i.submitted_at IS NOT NULL AND i.terminal_at IS NULL
 AND COALESCE(i.exchange_state,'')='' AND COALESCE(i.exchange_order_id,'')=''
 AND COALESCE(i.filled_quantity,0)=0 AND COALESCE(i.filled_notional,0)=0
 AND COALESCE(i.leader_target_size,0)=0
 AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id)
 AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_fill_commits f WHERE f.intent_id=i.id)
 AND EXISTS(SELECT 1 FROM copy_trade_position_mappings m WHERE m.trader_id=i.trader_id
   AND m.leader_pos_id=i.leader_pos_id AND m.status='closed' AND m.source_revision>=i.source_revision)
)`

// A retirement is tied to the exact old instruction. Later order evidence or a
// changed business identity invalidates the exemption; it is not a blanket
// permission to ignore stopped/archived traders or an exchange result of FILLED.
const retiredLegacyVenueObligationSQL = `(` + legacyAcknowledgedCloseSQL + ` AND EXISTS(
 SELECT 1 FROM copy_trade_venue_retirements r WHERE r.intent_id=i.id AND r.trader_id=i.trader_id
 AND r.intent_updated_at=CAST(i.updated_at AS TEXT)
 AND r.client_order_id=COALESCE(i.client_order_id,'') AND r.canonical_key=COALESCE(i.canonical_key,'')
))`

func retireLegacyVenueObligationsTx(tx *sql.Tx, traderID, evidence string, snapshot StoppedTraderFlatSnapshot) error {
	if snapshot.ExchangeID == "" || !snapshot.PositionsEmpty || !snapshot.OrdersEmpty || snapshot.ObservedAt.IsZero() ||
		time.Since(snapshot.ObservedAt) > time.Minute || snapshot.ObservedAt.After(time.Now().Add(time.Second)) {
		return fmt.Errorf("fresh complete flat-account evidence is required for legacy retirement")
	}
	var account, status string
	var generation int64
	var running bool
	if err := tx.QueryRow(`SELECT exchange_id,lifecycle_status,lifecycle_generation,is_running FROM traders WHERE id=?`, traderID).
		Scan(&account, &status, &generation, &running); err != nil {
		return err
	}
	if account != snapshot.ExchangeID || generation != snapshot.TraderGeneration || running ||
		(status != TraderLifecycleStopped && status != TraderLifecycleStopping && status != TraderLifecycleStoppingReconcileRequired) {
		return ErrTraderLifecycleConflict
	}
	rows, err := tx.Query(`SELECT i.id,i.leader_pos_id,i.source_revision,i.action,COALESCE(i.canonical_key,''),CAST(i.updated_at AS TEXT)
 FROM copy_trade_execution_intents i WHERE i.trader_id=? AND `+legacyAcknowledgedCloseSQL+`
 AND NOT `+retiredLegacyVenueObligationSQL+` ORDER BY i.id`, traderID)
	if err != nil {
		return err
	}
	type candidate struct {
		id, revision                   int64
		position, action, key, updated string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.position, &c.revision, &c.action, &c.key, &c.updated); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(candidates) == 0 {
		return err
	}
	// A stopping runtime can still be draining requests. Historical retirement
	// waits for STOPPED, and cannot race an active user of the same account.
	if status != TraderLifecycleStopped {
		return fmt.Errorf("legacy retirement requires the trader to be fully stopped")
	}
	// Configuration can change while stopped. A previous durable claim still
	// identifies the venue account used by unsettled historical instructions.
	var claimedAccount string
	err = tx.QueryRow(`SELECT exchange_id FROM copy_trade_account_claims WHERE trader_id=?`, traderID).Scan(&claimedAccount)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if claimedAccount != "" && claimedAccount != account {
		return fmt.Errorf("legacy retirement requires evidence from the previously claimed execution account")
	}
	var active int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM traders t LEFT JOIN copy_trade_account_claims a ON a.trader_id=t.id
 WHERE t.id<>? AND (t.exchange_id=? OR a.exchange_id=?)
 AND (t.is_running=1 OR t.lifecycle_status IN ('STARTING','RUNNING','STOPPING','STOPPING_RECONCILE_REQUIRED'))`, traderID, account, account).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("execution account has another active or stopping trader; legacy retirement deferred")
	}
	for _, c := range candidates {
		intent, err := getExecutionIntentTx(tx, traderID, c.position, c.revision, c.action, c.key)
		if err != nil {
			return err
		}
		if intent.ID != c.id {
			return ErrSourceTransitionChanged
		}
		raw, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		// Do not alter the instruction, mapping, terminal time or financial
		// ledger. The dedicated audit records only absence of current risk.
		if _, err = tx.Exec(`INSERT INTO copy_trade_venue_retirements
 (intent_id,trader_id,exchange_id,trader_generation,intent_updated_at,client_order_id,canonical_key,observed_at,evidence,before_json)
 VALUES(?,?,?,?,?,?,?,?,?,?)`, c.id, traderID, account, generation, c.updated, intent.ClientOrderID, c.key,
			snapshot.ObservedAt.UTC().Format(time.RFC3339Nano), evidence, string(raw)); err != nil {
			return err
		}
	}
	return nil
}
