package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	SourceHealthHealthy    = "HEALTHY"
	SourceHealthPrivate    = "PRIVATE"
	SourceHealthDisabled   = "DISABLED"
	SourceHealthAuthFailed = "AUTH_FAILED"
	SourceHealthDegraded   = "DEGRADED"
	SourceHealthStale      = "STALE"
)

type CopyTradeSourceHealth struct {
	TraderID               string                           `json:"trader_id"`
	LeaderID               string                           `json:"leader_id"`
	SourceMode             string                           `json:"source_mode"`
	SourceGeneration       int                              `json:"source_generation"`
	Status                 string                           `json:"status"`
	PreviousStatus         string                           `json:"previous_status,omitempty"`
	TraderName             string                           `json:"trader_name,omitempty"`
	LastCheckedAt          *time.Time                       `json:"last_checked_at,omitempty"`
	LastCompleteSnapshotAt *time.Time                       `json:"last_complete_snapshot_at,omitempty"`
	LastTransitionAt       *time.Time                       `json:"last_transition_at,omitempty"`
	LastNotifiedAt         *time.Time                       `json:"last_notified_at,omitempty"`
	ConsecutiveFailures    int                              `json:"consecutive_failures"`
	LastError              string                           `json:"last_error,omitempty"`
	UnsupportedContracts   []UnsupportedExecutionInstrument `json:"unsupported_contracts,omitempty"`
}

type SourceHealthObservation struct {
	Status           string
	TraderName       string
	Error            string
	CompleteSnapshot bool
	CheckedAt        time.Time
}

func (s *CopyTradeStore) initSourceHealthTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_source_health (
			trader_id TEXT PRIMARY KEY,
			leader_id TEXT NOT NULL,
			source_mode TEXT NOT NULL,
			source_generation INTEGER NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'DEGRADED',
			previous_status TEXT DEFAULT '',
			trader_name TEXT DEFAULT '',
			last_checked_at DATETIME,
			last_complete_snapshot_at DATETIME,
			last_transition_at DATETIME,
			last_notified_at DATETIME,
			consecutive_failures INTEGER NOT NULL DEFAULT 0,
			last_error TEXT DEFAULT ''
		)
	`)
	return err
}

func sourceHealthFrozen(status string) bool {
	switch status {
	case SourceHealthPrivate, SourceHealthDisabled, SourceHealthAuthFailed, SourceHealthStale:
		return true
	default:
		return false
	}
}

func (h *CopyTradeSourceHealth) Frozen() bool {
	return h != nil && sourceHealthFrozen(h.Status)
}

func scanSourceHealth(scanner interface{ Scan(...interface{}) error }) (*CopyTradeSourceHealth, error) {
	var h CopyTradeSourceHealth
	var checked, complete, transitioned, notified sql.NullString
	err := scanner.Scan(
		&h.TraderID, &h.LeaderID, &h.SourceMode, &h.SourceGeneration,
		&h.Status, &h.PreviousStatus, &h.TraderName,
		&checked, &complete, &transitioned, &notified,
		&h.ConsecutiveFailures, &h.LastError,
	)
	if err != nil {
		return nil, err
	}
	h.LastCheckedAt = parseOptionalSQLiteTime(checked)
	h.LastCompleteSnapshotAt = parseOptionalSQLiteTime(complete)
	h.LastTransitionAt = parseOptionalSQLiteTime(transitioned)
	h.LastNotifiedAt = parseOptionalSQLiteTime(notified)
	return &h, nil
}

func parseOptionalSQLiteTime(v sql.NullString) *time.Time {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return nil
	}
	// go-sqlite3 renders DATETIME values both with and without fractional
	// seconds. Notification timestamps are commonly second-aligned, while
	// polling timestamps usually carry nanoseconds, so both variants must be
	// accepted or persisted health silently loses its last-known times.
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
	} {
		if parsed, err := time.Parse(layout, v.String); err == nil {
			return &parsed
		}
	}
	return nil
}

const sourceHealthColumns = `trader_id, leader_id, source_mode, source_generation,
	status, COALESCE(previous_status,''), COALESCE(trader_name,''),
	CAST(last_checked_at AS TEXT), CAST(last_complete_snapshot_at AS TEXT),
	CAST(last_transition_at AS TEXT), CAST(last_notified_at AS TEXT),
	consecutive_failures, COALESCE(last_error,'')`

func (s *CopyTradeStore) GetSourceHealth(traderID string) (*CopyTradeSourceHealth, error) {
	h, err := scanSourceHealth(s.db.QueryRow(`SELECT `+sourceHealthColumns+` FROM copy_trade_source_health WHERE trader_id=?`, traderID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return h, err
}

// RecordSourceHealthObservation applies the persisted health state machine.
// Transient failures never become PRIVATE and a healthy recovery requires a
// complete, all-pages position snapshot.
func (s *CopyTradeStore) RecordSourceHealthObservation(traderID, leaderID, sourceMode string, generation int, obs SourceHealthObservation) (*CopyTradeSourceHealth, bool, error) {
	if obs.CheckedAt.IsZero() {
		obs.CheckedAt = time.Now()
	}
	if generation <= 0 {
		generation = 1
	}
	current, err := s.GetSourceHealth(traderID)
	if err != nil {
		return nil, false, err
	}
	isNew := current == nil || current.SourceGeneration != generation || current.LeaderID != leaderID || current.SourceMode != sourceMode
	if isNew {
		current = &CopyTradeSourceHealth{TraderID: traderID, LeaderID: leaderID, SourceMode: sourceMode, SourceGeneration: generation, Status: SourceHealthDegraded}
	}

	previous := current.Status
	// A process can be offline long enough for its last healthy snapshot to
	// become stale without recording an intermediate poll failure. Treat the
	// first complete post-gap observation as STALE -> HEALTHY so callers apply
	// recovery/no-chase semantics and emit one recovery notification.
	if !isNew && previous == SourceHealthHealthy && current.LastCompleteSnapshotAt != nil &&
		obs.CompleteSnapshot && strings.EqualFold(obs.Status, SourceHealthHealthy) &&
		obs.CheckedAt.Sub(*current.LastCompleteSnapshotAt) > time.Minute {
		previous = SourceHealthStale
	}
	status := strings.ToUpper(strings.TrimSpace(obs.Status))
	if status == "" {
		status = "ERROR"
	}
	if obs.CompleteSnapshot && status == SourceHealthHealthy {
		current.ConsecutiveFailures = 0
		current.LastCompleteSnapshotAt = &obs.CheckedAt
		current.LastError = ""
		current.Status = SourceHealthHealthy
	} else if status == SourceHealthPrivate || status == SourceHealthDisabled || status == SourceHealthAuthFailed {
		current.ConsecutiveFailures++
		current.Status = status
		current.LastError = obs.Error
	} else {
		current.ConsecutiveFailures++
		current.LastError = obs.Error
		// Explicit frozen states are sticky. A subsequent timeout/JSON failure
		// carries less information than the last confirmed PRIVATE/DISABLED/
		// AUTH_FAILED/STALE observation and must never thaw the source into the
		// non-frozen DEGRADED state. Only a complete HEALTHY snapshot recovers it.
		if sourceHealthFrozen(current.Status) {
			// keep the confirmed frozen status
		} else if current.LastCompleteSnapshotAt != nil && obs.CheckedAt.Sub(*current.LastCompleteSnapshotAt) > time.Minute {
			current.Status = SourceHealthStale
		} else if current.ConsecutiveFailures >= 3 {
			current.Status = SourceHealthDegraded
		}
	}
	current.LastCheckedAt = &obs.CheckedAt
	if obs.TraderName != "" {
		current.TraderName = obs.TraderName
	}
	transitioned := !isNew && previous != current.Status
	if transitioned {
		current.PreviousStatus = previous
		current.LastTransitionAt = &obs.CheckedAt
	} else if isNew {
		current.PreviousStatus = ""
		current.LastTransitionAt = &obs.CheckedAt
	}

	_, err = s.db.Exec(`
		INSERT INTO copy_trade_source_health
			(trader_id, leader_id, source_mode, source_generation, status, previous_status,
			 trader_name, last_checked_at, last_complete_snapshot_at, last_transition_at,
			 last_notified_at, consecutive_failures, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trader_id) DO UPDATE SET
			leader_id=excluded.leader_id, source_mode=excluded.source_mode,
			source_generation=excluded.source_generation, status=excluded.status,
			previous_status=excluded.previous_status, trader_name=excluded.trader_name,
			last_checked_at=excluded.last_checked_at,
			last_complete_snapshot_at=excluded.last_complete_snapshot_at,
			last_transition_at=excluded.last_transition_at,
			last_notified_at=excluded.last_notified_at,
			consecutive_failures=excluded.consecutive_failures, last_error=excluded.last_error
	`, current.TraderID, current.LeaderID, current.SourceMode, current.SourceGeneration,
		current.Status, current.PreviousStatus, current.TraderName, current.LastCheckedAt,
		current.LastCompleteSnapshotAt, current.LastTransitionAt, current.LastNotifiedAt,
		current.ConsecutiveFailures, current.LastError)
	if err != nil {
		return nil, false, fmt.Errorf("save source health: %w", err)
	}
	return current, transitioned, nil
}

func (s *CopyTradeStore) MarkSourceHealthNotified(traderID string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE copy_trade_source_health SET last_notified_at=? WHERE trader_id=?`, at, traderID)
	return err
}
