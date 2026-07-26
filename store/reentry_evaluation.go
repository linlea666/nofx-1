package store

import (
	"database/sql"
	"fmt"
	"time"
)

const ReentryDecisionEvaluationVersion = 3

const (
	ReentryMarketReversal     = "REVERSAL_CONFIRMED"
	ReentryMarketAgainst      = "CONTINUED_AGAINST"
	ReentryMarketChop         = "CHOP_INCONCLUSIVE"
	ReentryMarketInsufficient = "INSUFFICIENT_DATA"
)

// ReentryAIDecisionEvaluation is a deterministic, post-decision measurement.
// It never calls a model and never enters the trading execution path. A
// versioned row makes later calibration comparable without silently rewriting
// the historical meaning of an outcome label.
type ReentryAIDecisionEvaluation struct {
	ID                   int64      `json:"id"`
	AnalysisID           int64      `json:"analysis_id"`
	CandidateID          int64      `json:"candidate_id"`
	TraderID             string     `json:"trader_id"`
	TraderNameSnapshot   string     `json:"trader_name_snapshot"`
	CycleID              int64      `json:"cycle_id"`
	AttemptNo            int        `json:"attempt_no"`
	DecisionGeneration   int        `json:"decision_generation"`
	Decision             string     `json:"decision"`
	Horizon              string     `json:"horizon"`
	EvaluationVersion    int        `json:"evaluation_version"`
	EvaluationStatus     string     `json:"evaluation_status"`
	DataQuality          string     `json:"data_quality"`
	ExecutionDataQuality string     `json:"execution_data_quality"`
	MarketOutcome        string     `json:"market_outcome"`
	DecisionOutcome      string     `json:"decision_outcome"`
	Actionability        string     `json:"actionability"`
	Reason               string     `json:"reason"`
	ReferencePrice       float64    `json:"reference_price"`
	ReferenceATR         float64    `json:"reference_atr"`
	MFEATR               float64    `json:"mfe_atr"`
	MAEATR               float64    `json:"mae_atr"`
	FirstReversalAt      *time.Time `json:"first_reversal_at,omitempty"`
	WindowStartAt        time.Time  `json:"window_start_at"`
	WindowEndAt          time.Time  `json:"window_end_at"`
	SampleCount          int        `json:"sample_count"`
	CoverageRatio        float64    `json:"coverage_ratio"`
	MaxGapSeconds        float64    `json:"max_gap_seconds"`
	ActualExecuted       bool       `json:"actual_executed"`
	ExecutionRequested   bool       `json:"execution_requested"`
	ExecutionSubmitted   bool       `json:"execution_submitted"`
	ExecutionFilled      bool       `json:"execution_filled"`
	ExecutionProtected   bool       `json:"execution_protected"`
	ActualPnL            *float64   `json:"actual_pnl,omitempty"`
	EvaluationLatency    float64    `json:"evaluation_latency_seconds"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

func (s *ReentryAIStore) initReentryDecisionEvaluationTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS reentry_ai_decision_evaluations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			analysis_id INTEGER NOT NULL,
			candidate_id INTEGER NOT NULL,
			trader_id TEXT NOT NULL,
			trader_name_snapshot TEXT NOT NULL DEFAULT '',
			cycle_id INTEGER NOT NULL,
			attempt_no INTEGER NOT NULL DEFAULT 0,
			decision_generation INTEGER NOT NULL DEFAULT 0,
			decision TEXT NOT NULL DEFAULT '',
			horizon TEXT NOT NULL DEFAULT '',
			evaluation_version INTEGER NOT NULL DEFAULT 1,
			evaluation_status TEXT NOT NULL DEFAULT 'FINAL',
			data_quality TEXT NOT NULL DEFAULT 'UNSCORABLE',
			execution_data_quality TEXT NOT NULL DEFAULT 'UNSCORABLE',
			market_outcome TEXT NOT NULL DEFAULT '',
			decision_outcome TEXT NOT NULL DEFAULT '',
			actionability TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			reference_price REAL NOT NULL DEFAULT 0,
			reference_atr REAL NOT NULL DEFAULT 0,
			mfe_atr REAL NOT NULL DEFAULT 0,
			mae_atr REAL NOT NULL DEFAULT 0,
			first_reversal_at DATETIME,
			window_start_at DATETIME NOT NULL,
			window_end_at DATETIME NOT NULL,
			sample_count INTEGER NOT NULL DEFAULT 0,
			coverage_ratio REAL NOT NULL DEFAULT 0,
			max_gap_seconds REAL NOT NULL DEFAULT 0,
			actual_executed BOOLEAN NOT NULL DEFAULT 0,
			execution_requested BOOLEAN NOT NULL DEFAULT 0,
			execution_submitted BOOLEAN NOT NULL DEFAULT 0,
			execution_filled BOOLEAN NOT NULL DEFAULT 0,
			execution_protected BOOLEAN NOT NULL DEFAULT 0,
			actual_pnl REAL,
			evaluation_latency_seconds REAL NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(analysis_id,horizon,evaluation_version)
		);
		CREATE INDEX IF NOT EXISTS idx_reentry_ai_eval_cycle ON reentry_ai_decision_evaluations(cycle_id,analysis_id);
		CREATE INDEX IF NOT EXISTS idx_reentry_ai_eval_trader_outcome ON reentry_ai_decision_evaluations(trader_id,decision_outcome);
	`)
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"data_quality", "TEXT NOT NULL DEFAULT 'UNSCORABLE'"},
		{"execution_data_quality", "TEXT NOT NULL DEFAULT 'UNSCORABLE'"},
		{"execution_requested", "BOOLEAN NOT NULL DEFAULT 0"},
		{"execution_submitted", "BOOLEAN NOT NULL DEFAULT 0"},
		{"execution_filled", "BOOLEAN NOT NULL DEFAULT 0"},
		{"execution_protected", "BOOLEAN NOT NULL DEFAULT 0"},
	} {
		if err = ensureSQLiteColumn(s.db, "reentry_ai_decision_evaluations", column.name, column.definition); err != nil {
			return fmt.Errorf("migrate reentry_ai_decision_evaluations.%s: %w", column.name, err)
		}
	}
	return nil
}

const reentryEvaluationColumns = `id,analysis_id,candidate_id,trader_id,trader_name_snapshot,cycle_id,attempt_no,decision_generation,decision,horizon,evaluation_version,evaluation_status,data_quality,execution_data_quality,market_outcome,decision_outcome,actionability,reason,reference_price,reference_atr,mfe_atr,mae_atr,first_reversal_at,window_start_at,window_end_at,sample_count,coverage_ratio,max_gap_seconds,actual_executed,execution_requested,execution_submitted,execution_filled,execution_protected,actual_pnl,evaluation_latency_seconds,created_at,updated_at`

func scanReentryDecisionEvaluation(row rowScanner) (*ReentryAIDecisionEvaluation, error) {
	var e ReentryAIDecisionEvaluation
	var reversal sql.NullString
	var pnl sql.NullFloat64
	var start, end, created, updated string
	if err := row.Scan(&e.ID, &e.AnalysisID, &e.CandidateID, &e.TraderID, &e.TraderNameSnapshot, &e.CycleID, &e.AttemptNo, &e.DecisionGeneration, &e.Decision, &e.Horizon, &e.EvaluationVersion, &e.EvaluationStatus, &e.DataQuality, &e.ExecutionDataQuality, &e.MarketOutcome, &e.DecisionOutcome, &e.Actionability, &e.Reason, &e.ReferencePrice, &e.ReferenceATR, &e.MFEATR, &e.MAEATR, &reversal, &start, &end, &e.SampleCount, &e.CoverageRatio, &e.MaxGapSeconds, &e.ActualExecuted, &e.ExecutionRequested, &e.ExecutionSubmitted, &e.ExecutionFilled, &e.ExecutionProtected, &pnl, &e.EvaluationLatency, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if e.FirstReversalAt, err = parseNullableDBTime(reversal); err != nil {
		return nil, err
	}
	if e.WindowStartAt, err = parseDBTime(start); err != nil {
		return nil, err
	}
	if e.WindowEndAt, err = parseDBTime(end); err != nil {
		return nil, err
	}
	if e.CreatedAt, err = parseDBTime(created); err != nil {
		return nil, err
	}
	if e.UpdatedAt, err = parseDBTime(updated); err != nil {
		return nil, err
	}
	if pnl.Valid {
		v := pnl.Float64
		e.ActualPnL = &v
	}
	return &e, nil
}

func (s *ReentryAIStore) SaveReentryDecisionEvaluation(e *ReentryAIDecisionEvaluation) (*ReentryAIDecisionEvaluation, bool, error) {
	if e == nil || e.AnalysisID <= 0 || e.CandidateID <= 0 || e.CycleID <= 0 || e.TraderID == "" || e.Horizon == "" {
		return nil, false, fmt.Errorf("invalid reentry AI decision evaluation")
	}
	if e.EvaluationVersion <= 0 {
		e.EvaluationVersion = ReentryDecisionEvaluationVersion
	}
	if e.DataQuality == "" {
		e.DataQuality = "UNSCORABLE"
	}
	if e.ExecutionDataQuality == "" {
		e.ExecutionDataQuality = "UNSCORABLE"
	}
	var reversal interface{}
	if e.FirstReversalAt != nil {
		reversal = e.FirstReversalAt.UTC()
	}
	var pnl interface{}
	if e.ActualPnL != nil {
		pnl = *e.ActualPnL
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO reentry_ai_decision_evaluations
		(analysis_id,candidate_id,trader_id,trader_name_snapshot,cycle_id,attempt_no,decision_generation,decision,horizon,evaluation_version,evaluation_status,data_quality,execution_data_quality,market_outcome,decision_outcome,actionability,reason,reference_price,reference_atr,mfe_atr,mae_atr,first_reversal_at,window_start_at,window_end_at,sample_count,coverage_ratio,max_gap_seconds,actual_executed,execution_requested,execution_submitted,execution_filled,execution_protected,actual_pnl,evaluation_latency_seconds)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.AnalysisID, e.CandidateID, e.TraderID, e.TraderNameSnapshot, e.CycleID, e.AttemptNo, e.DecisionGeneration, e.Decision, e.Horizon, e.EvaluationVersion, e.EvaluationStatus, e.DataQuality, e.ExecutionDataQuality, e.MarketOutcome, e.DecisionOutcome, e.Actionability, e.Reason, e.ReferencePrice, e.ReferenceATR, e.MFEATR, e.MAEATR, reversal, e.WindowStartAt.UTC(), e.WindowEndAt.UTC(), e.SampleCount, e.CoverageRatio, e.MaxGapSeconds, e.ActualExecuted, e.ExecutionRequested, e.ExecutionSubmitted, e.ExecutionFilled, e.ExecutionProtected, pnl, e.EvaluationLatency)
	if err != nil {
		return nil, false, err
	}
	inserted, _ := res.RowsAffected()
	saved, err := scanReentryDecisionEvaluation(s.db.QueryRow(`SELECT `+reentryEvaluationColumns+` FROM reentry_ai_decision_evaluations WHERE analysis_id=? AND horizon=? AND evaluation_version=?`, e.AnalysisID, e.Horizon, e.EvaluationVersion))
	return saved, inserted == 1, err
}

func (s *ReentryAIStore) ListReentryDecisionEvaluationsByCycle(cycleID int64) ([]*ReentryAIDecisionEvaluation, error) {
	rows, err := s.db.Query(`SELECT `+reentryEvaluationColumns+` FROM reentry_ai_decision_evaluations WHERE cycle_id=? ORDER BY analysis_id,horizon`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReentryAIDecisionEvaluation{}
	for rows.Next() {
		e, err := scanReentryDecisionEvaluation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *ReentryAIStore) ListReentryDecisionEvaluationsByAnalysis(analysisID int64) ([]*ReentryAIDecisionEvaluation, error) {
	rows, err := s.db.Query(`SELECT `+reentryEvaluationColumns+` FROM reentry_ai_decision_evaluations WHERE analysis_id=? ORDER BY id`, analysisID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReentryAIDecisionEvaluation{}
	for rows.Next() {
		e, err := scanReentryDecisionEvaluation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListClosedCyclesPendingAIEvaluation supports bounded historical backfill.
// Active cycles are evaluated on demand when their canonical final summary is
// produced; incomplete retained history is recorded as UNSCORABLE, not guessed.
func (s *ReentryAIStore) ListClosedCyclesPendingAIEvaluation(limit int) ([]int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := s.db.Query(`SELECT DISTINCT a.cycle_id FROM reentry_ai_analyses a
		JOIN copy_guard_cycles c ON c.id=a.cycle_id
		WHERE a.candidate_id>0 AND a.call_status='COMPLETED' AND a.verdict<>'' AND c.closed_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM reentry_ai_decision_evaluations e WHERE e.analysis_id=a.id AND e.evaluation_version=? AND e.horizon='LEADER_FINAL')
		ORDER BY a.cycle_id LIMIT ?`, ReentryDecisionEvaluationVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListCyclesWithMatureAIEvaluationWindows includes active cycles whose fixed
// 30m/2h window is mature as well as closed cycles awaiting LEADER_FINAL.
func (s *ReentryAIStore) ListCyclesWithMatureAIEvaluationWindows(limit int) ([]int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := s.db.Query(`SELECT DISTINCT a.cycle_id FROM reentry_ai_analyses a
		JOIN copy_guard_cycles c ON c.id=a.cycle_id
		WHERE a.candidate_id>0 AND a.call_status='COMPLETED' AND a.verdict<>''
		AND (
			(COALESCE(a.model_completed_at,a.snapshot_at)<=datetime('now','-30 minutes')
			 AND NOT EXISTS (SELECT 1 FROM reentry_ai_decision_evaluations e WHERE e.analysis_id=a.id AND e.evaluation_version=? AND e.horizon='30_MINUTES'))
			OR (COALESCE(a.model_completed_at,a.snapshot_at)<=datetime('now','-2 hours')
			 AND NOT EXISTS (SELECT 1 FROM reentry_ai_decision_evaluations e WHERE e.analysis_id=a.id AND e.evaluation_version=? AND e.horizon='2_HOURS'))
			OR (c.closed_at IS NOT NULL
			 AND NOT EXISTS (SELECT 1 FROM reentry_ai_decision_evaluations e WHERE e.analysis_id=a.id AND e.evaluation_version=? AND e.horizon='LEADER_FINAL'))
		)
		ORDER BY a.cycle_id LIMIT ?`, ReentryDecisionEvaluationVersion, ReentryDecisionEvaluationVersion, ReentryDecisionEvaluationVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
