package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ReentryAIDiagnostic is a zero-trade connectivity/schema check. It is kept
// separate from reentry_ai_analyses so diagnostics never contaminate trading
// call budgets, candidate statistics, attribution, or cycle exports.
type ReentryAIDiagnostic struct {
	ID            int64     `json:"id"`
	UserID        string    `json:"user_id,omitempty"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	PromptVersion string    `json:"prompt_version"`
	Success       bool      `json:"success"`
	LatencyMS     int64     `json:"latency_ms"`
	RawResponse   string    `json:"raw_response"`
	ParsedJSON    string    `json:"parsed_json"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *ReentryAIStore) initReentryDiagnosticTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS reentry_ai_diagnostics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			prompt_version TEXT NOT NULL DEFAULT '',
			success BOOLEAN NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			raw_response TEXT NOT NULL DEFAULT '',
			parsed_json TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_reentry_ai_diagnostics_user_time
			ON reentry_ai_diagnostics(user_id,created_at);
	`)
	return err
}

const reentryDiagnosticColumns = `id,user_id,provider,model,prompt_version,success,latency_ms,raw_response,parsed_json,error,created_at`

func scanReentryAIDiagnostic(row rowScanner) (*ReentryAIDiagnostic, error) {
	var d ReentryAIDiagnostic
	var created string
	if err := row.Scan(&d.ID, &d.UserID, &d.Provider, &d.Model, &d.PromptVersion, &d.Success, &d.LatencyMS, &d.RawResponse, &d.ParsedJSON, &d.Error, &created); err != nil {
		return nil, err
	}
	var err error
	d.CreatedAt, err = parseDBTime(created)
	if err != nil {
		return nil, fmt.Errorf("reentry AI diagnostic %d created_at: %w", d.ID, err)
	}
	return &d, nil
}

func (s *ReentryAIStore) SaveReentryAIDiagnostic(d *ReentryAIDiagnostic) (*ReentryAIDiagnostic, error) {
	if d == nil || d.UserID == "" {
		return nil, fmt.Errorf("invalid reentry AI diagnostic")
	}
	res, err := s.db.Exec(`INSERT INTO reentry_ai_diagnostics
		(user_id,provider,model,prompt_version,success,latency_ms,raw_response,parsed_json,error)
		VALUES(?,?,?,?,?,?,?,?,?)`, d.UserID, d.Provider, d.Model, d.PromptVersion, d.Success, d.LatencyMS, d.RawResponse, d.ParsedJSON, d.Error)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return scanReentryAIDiagnostic(s.db.QueryRow(`SELECT `+reentryDiagnosticColumns+` FROM reentry_ai_diagnostics WHERE id=?`, id))
}

func (s *ReentryAIStore) ListReentryAIDiagnostics(userID string, limit int) ([]*ReentryAIDiagnostic, error) {
	if userID == "" {
		return []*ReentryAIDiagnostic{}, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.Query(`SELECT `+reentryDiagnosticColumns+` FROM reentry_ai_diagnostics WHERE user_id=? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*ReentryAIDiagnostic{}
	for rows.Next() {
		d, err := scanReentryAIDiagnostic(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s *ReentryAIStore) LatestReentryAIDiagnostic(userID string) (*ReentryAIDiagnostic, error) {
	if userID == "" {
		return nil, sql.ErrNoRows
	}
	return scanReentryAIDiagnostic(s.db.QueryRow(`SELECT `+reentryDiagnosticColumns+` FROM reentry_ai_diagnostics WHERE user_id=? ORDER BY id DESC LIMIT 1`, userID))
}
