package store

import (
	"fmt"
	"strings"
	"time"
)

// PositionSettlementDiagnostic is accounting health, never an execution gate.
// Its identity is the original venue lifecycle, not a corrected fill time.
type PositionSettlementDiagnostic struct {
	ExchangeID      string `json:"exchange_id"`
	Identity        string `json:"identity"`
	PositionID      string `json:"position_id"`
	Symbol          string `json:"symbol"`
	Side            string `json:"side"`
	MarginMode      string `json:"margin_mode"`
	OpenedMS        int64  `json:"opened_ms"`
	Stage           string `json:"stage"`
	ReasonCode      string `json:"reason_code"`
	Detail          string `json:"detail"`
	Status          string `json:"status"`
	Attempts        int    `json:"attempts"`
	FirstObservedAt string `json:"first_observed_at"`
	LastObservedAt  string `json:"last_observed_at"`
	ResolvedAt      string `json:"resolved_at,omitempty"`
}

func settlementLifecycleIdentity(positionID, symbol, side, mode string, opened time.Time) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", positionID, symbol, side, mode, opened.UnixMilli())
}

func (s *PositionStore) initSettlementDiagnosticTable() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS position_settlement_diagnostics (
 exchange_id TEXT NOT NULL,identity TEXT NOT NULL,position_id TEXT NOT NULL DEFAULT '',
 symbol TEXT NOT NULL DEFAULT '',side TEXT NOT NULL DEFAULT '',margin_mode TEXT NOT NULL DEFAULT '',
 opened_ms INTEGER NOT NULL DEFAULT 0,stage TEXT NOT NULL,reason_code TEXT NOT NULL,detail TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'PENDING',attempts INTEGER NOT NULL DEFAULT 1,
 first_observed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,last_observed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 resolved_at DATETIME,PRIMARY KEY(exchange_id,identity));`)
	return err
}

func (d PositionSettlementDiagnostic) identity() string {
	if d.PositionID == "" {
		return "account_history"
	}
	return settlementLifecycleIdentity(d.PositionID, d.Symbol, d.Side, d.MarginMode, time.UnixMilli(d.OpenedMS))
}

// RecordSettlementDiagnostic persists the latest failed stage per lifecycle;
// repeated polls update one row rather than emitting unbounded event rows.
func (s *PositionStore) RecordSettlementDiagnostic(d PositionSettlementDiagnostic) error {
	if d.ExchangeID == "" || d.Stage == "" || d.ReasonCode == "" {
		return fmt.Errorf("settlement diagnostic identity/stage missing")
	}
	d.Detail = strings.TrimSpace(d.Detail)
	if len(d.Detail) > 1024 {
		d.Detail = d.Detail[:1024]
	}
	_, err := s.db.Exec(`INSERT INTO position_settlement_diagnostics
 (exchange_id,identity,position_id,symbol,side,margin_mode,opened_ms,stage,reason_code,detail)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(exchange_id,identity) DO UPDATE SET
 stage=excluded.stage,reason_code=excluded.reason_code,detail=excluded.detail,status='PENDING',
 attempts=position_settlement_diagnostics.attempts+1,last_observed_at=CURRENT_TIMESTAMP,resolved_at=NULL`,
		d.ExchangeID, d.identity(), d.PositionID, d.Symbol, d.Side, d.MarginMode, d.OpenedMS, d.Stage, d.ReasonCode, d.Detail)
	return err
}

func (s *PositionStore) ResolveSettlementDiagnostic(d PositionSettlementDiagnostic) error {
	_, err := s.db.Exec(`UPDATE position_settlement_diagnostics SET status='RESOLVED',resolved_at=COALESCE(resolved_at,CURRENT_TIMESTAMP),last_observed_at=CURRENT_TIMESTAMP WHERE exchange_id=? AND identity=? AND status='PENDING'`, d.ExchangeID, d.identity())
	return err
}

// ListSettlementDiagnostics returns evidence gaps separately from source and
// order health. The caller must authorize the execution account before use.
func (s *PositionStore) ListSettlementDiagnostics(exchangeID string, includeResolved bool, limit int) ([]PositionSettlementDiagnostic, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT exchange_id,identity,position_id,symbol,side,margin_mode,opened_ms,stage,reason_code,detail,status,attempts,first_observed_at,last_observed_at,COALESCE(resolved_at,'') FROM position_settlement_diagnostics WHERE exchange_id=? AND (? OR status='PENDING') ORDER BY last_observed_at DESC,identity LIMIT ?`, exchangeID, includeResolved, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PositionSettlementDiagnostic, 0)
	for rows.Next() {
		var d PositionSettlementDiagnostic
		if err = rows.Scan(&d.ExchangeID, &d.Identity, &d.PositionID, &d.Symbol, &d.Side, &d.MarginMode, &d.OpenedMS, &d.Stage, &d.ReasonCode, &d.Detail, &d.Status, &d.Attempts, &d.FirstObservedAt, &d.LastObservedAt, &d.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
