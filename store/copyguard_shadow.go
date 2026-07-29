package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	CopyGuardShadowCurrentStop       = "CURRENT_STOP"
	CopyGuardShadowWideStopEqualRisk = "WIDE_STOP_EQUAL_RISK"
	CopyGuardShadowStagedReduction   = "STAGED_REDUCTION"
	CopyGuardShadowProbeReentry25Pct = "PROBE_REENTRY_25_PCT"
	CopyGuardShadowEvaluationVersion = 1
	CopyGuardShadowScorable          = "SCORABLE"
	CopyGuardShadowNoSignal          = "NO_SIGNAL"
	CopyGuardShadowUnscorable        = "UNSCORABLE"
	CopyGuardShadowQualityVerified   = "VERIFIED"
	CopyGuardShadowQualityEstimated  = "ESTIMATED_SHADOW"
	CopyGuardShadowQualityUnscorable = "UNSCORABLE"
)

type CopyGuardShadowEvaluation struct {
	ID                int64     `json:"id"`
	CycleID           int64     `json:"cycle_id"`
	TraderID          string    `json:"trader_id"`
	Policy            string    `json:"policy"`
	EvaluationVersion int       `json:"evaluation_version"`
	Status            string    `json:"status"`
	DataQuality       string    `json:"data_quality"`
	GrossPnL          float64   `json:"gross_pnl"`
	EstimatedCost     float64   `json:"estimated_cost"`
	NetPnL            float64   `json:"net_pnl"`
	SizeFactor        float64   `json:"size_factor"`
	EntryPrice        float64   `json:"entry_price"`
	ExitPrice         float64   `json:"exit_price"`
	Reason            string    `json:"reason"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (s *CopyTradeStore) SaveCopyGuardShadowEvaluation(e *CopyGuardShadowEvaluation) error {
	if e == nil || e.CycleID <= 0 || strings.TrimSpace(e.TraderID) == "" ||
		strings.TrimSpace(e.Policy) == "" || e.EvaluationVersion <= 0 {
		return fmt.Errorf("invalid copy guard shadow evaluation")
	}
	_, err := s.db.Exec(`INSERT INTO copy_guard_shadow_evaluations
		(cycle_id,trader_id,policy,evaluation_version,status,data_quality,gross_pnl,
		 estimated_cost,net_pnl,size_factor,entry_price,exit_price,reason)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(cycle_id,policy,evaluation_version) DO UPDATE SET
			status=excluded.status,data_quality=excluded.data_quality,
			gross_pnl=excluded.gross_pnl,estimated_cost=excluded.estimated_cost,
			net_pnl=excluded.net_pnl,size_factor=excluded.size_factor,
			entry_price=excluded.entry_price,exit_price=excluded.exit_price,
			reason=excluded.reason,updated_at=CURRENT_TIMESTAMP`,
		e.CycleID, e.TraderID, e.Policy, e.EvaluationVersion, e.Status, e.DataQuality,
		e.GrossPnL, e.EstimatedCost, e.NetPnL, e.SizeFactor, e.EntryPrice,
		e.ExitPrice, e.Reason)
	return err
}

func (s *CopyTradeStore) ListCopyGuardShadowEvaluations(cycleID int64) ([]*CopyGuardShadowEvaluation, error) {
	rows, err := s.db.Query(`SELECT id,cycle_id,trader_id,policy,evaluation_version,status,
		data_quality,gross_pnl,estimated_cost,net_pnl,size_factor,entry_price,exit_price,
		reason,created_at,updated_at
		FROM copy_guard_shadow_evaluations WHERE cycle_id=?
		ORDER BY policy,evaluation_version`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyGuardShadowEvaluation
	for rows.Next() {
		e, scanErr := scanCopyGuardShadowEvaluation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *CopyTradeStore) ListCopyGuardShadowEvaluationsForTraders(
	traderIDs []string, from, to time.Time,
) ([]*CopyGuardShadowEvaluation, error) {
	if len(traderIDs) == 0 {
		return []*CopyGuardShadowEvaluation{}, nil
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+2)
	for _, traderID := range traderIDs {
		args = append(args, traderID)
	}
	args = append(args, from.UTC().Format("2006-01-02 15:04:05"),
		to.UTC().Format("2006-01-02 15:04:05"))
	rows, err := s.db.Query(`SELECT e.id,e.cycle_id,e.trader_id,e.policy,
		e.evaluation_version,e.status,e.data_quality,e.gross_pnl,e.estimated_cost,
		e.net_pnl,e.size_factor,e.entry_price,e.exit_price,e.reason,e.created_at,e.updated_at
		FROM copy_guard_shadow_evaluations e
		JOIN copy_guard_cycles c ON c.id=e.cycle_id
		WHERE e.trader_id IN (`+marks+`) AND c.closed_at>=? AND c.closed_at<?
		ORDER BY e.policy,e.cycle_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyGuardShadowEvaluation
	for rows.Next() {
		e, scanErr := scanCopyGuardShadowEvaluation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *CopyTradeStore) CountUnprotectedFilledIntents(
	traderIDs []string, from, to time.Time,
) (int, error) {
	if len(traderIDs) == 0 {
		return 0, nil
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+2)
	for _, traderID := range traderIDs {
		args = append(args, traderID)
	}
	args = append(args, from.UTC().Format("2006-01-02 15:04:05"),
		to.UTC().Format("2006-01-02 15:04:05"))
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents i
		JOIN copy_guard_cycles c ON c.id=i.cycle_id
		WHERE i.trader_id IN (`+marks+`) AND c.closed_at>=? AND c.closed_at<?
		  AND COALESCE(i.filled_quantity,0)>0
		  AND i.action IN ('open_long','open_short')
		  AND i.protected_at IS NULL`, args...).Scan(&count)
	return count, err
}

type shadowEvaluationScanner interface {
	Scan(dest ...interface{}) error
}

func scanCopyGuardShadowEvaluation(row shadowEvaluationScanner) (*CopyGuardShadowEvaluation, error) {
	var e CopyGuardShadowEvaluation
	var createdAt, updatedAt string
	if err := row.Scan(&e.ID, &e.CycleID, &e.TraderID, &e.Policy,
		&e.EvaluationVersion, &e.Status, &e.DataQuality, &e.GrossPnL,
		&e.EstimatedCost, &e.NetPnL, &e.SizeFactor, &e.EntryPrice,
		&e.ExitPrice, &e.Reason, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	var err error
	if e.CreatedAt, err = parseDBTime(createdAt); err != nil {
		return nil, err
	}
	if e.UpdatedAt, err = parseDBTime(updatedAt); err != nil {
		return nil, err
	}
	return &e, nil
}

var _ shadowEvaluationScanner = (*sql.Row)(nil)
