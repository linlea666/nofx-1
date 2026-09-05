package store

import (
	"database/sql"
	"fmt"
)

// InitialFillEvidence is immutable. Current attempt position fields are not
// evidence: adds, partial fills and leverage synchronization can change them.
type InitialFillEvidence struct {
	CycleID         int64
	IntentID        int64
	EntryPrice      float64
	Quantity        float64
	Leverage        float64
	ClientOrderID   string
	ExchangeOrderID string
}

func (s *CopyTradeStore) RecordCopyGuardLeverageReceipt(intentID int64, clientID string, leverage int) error {
	if intentID <= 0 || clientID == "" || leverage <= 0 {
		return fmt.Errorf("invalid leverage receipt")
	}
	_, err := s.db.Exec(`INSERT INTO copy_guard_leverage_receipts(intent_id,client_order_id,leverage)
		VALUES(?,?,?) ON CONFLICT(intent_id,client_order_id) DO UPDATE SET leverage=excluded.leverage,observed_at=CURRENT_TIMESTAMP
		WHERE NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=excluded.intent_id AND a.client_order_id=excluded.client_order_id AND (a.submitted_at IS NOT NULL OR COALESCE(a.exchange_order_id,'')<>''))
		AND NOT EXISTS (SELECT 1 FROM copy_guard_initial_fill_evidence e WHERE e.intent_id=excluded.intent_id AND e.client_order_id=excluded.client_order_id)`, intentID, clientID, leverage)
	return err
}

func saveInitialFillEvidenceTx(tx *sql.Tx, cycleID int64, in InitialCopyGuardLifecycle) error {
	// Always freeze fill price/quantity, even when no leverage receipt exists.
	// Missing evidence is explicit and must never be replaced by a later fill.
	_, err := tx.Exec(`INSERT INTO copy_guard_initial_fill_evidence
		(cycle_id,intent_id,entry_price,quantity,leverage,client_order_id,exchange_order_id)
		VALUES(?,?,?,?,COALESCE((SELECT leverage FROM copy_guard_leverage_receipts WHERE intent_id=? AND client_order_id=?),0),?,?)
		ON CONFLICT(cycle_id) DO NOTHING`, cycleID, in.IntentID, in.FollowerEntry, in.FollowerQuantity, in.IntentID, in.EntryClientID, in.EntryClientID, in.EntryOrderID)
	return err
}

func (s *CopyTradeStore) GetCopyGuardInitialFillEvidence(cycleID int64) (*InitialFillEvidence, error) {
	var in InitialFillEvidence
	err := s.db.QueryRow(`SELECT cycle_id,intent_id,entry_price,quantity,leverage,client_order_id,exchange_order_id FROM copy_guard_initial_fill_evidence WHERE cycle_id=?`, cycleID).
		Scan(&in.CycleID, &in.IntentID, &in.EntryPrice, &in.Quantity, &in.Leverage, &in.ClientOrderID, &in.ExchangeOrderID)
	return &in, err
}

// CompleteInitialFillLeverage is only called during the original first-fill
// observation, before any further transition. Recovery may read but not guess it.
func (s *CopyTradeStore) CompleteInitialFillLeverage(cycleID, intentID int64, leverage float64) error {
	if invalidPositiveNumber(leverage) {
		return fmt.Errorf("invalid initial leverage")
	}
	_, err := s.db.Exec(`UPDATE copy_guard_initial_fill_evidence SET leverage=? WHERE cycle_id=? AND intent_id=? AND leverage=0`, leverage, cycleID, intentID)
	return err
}
