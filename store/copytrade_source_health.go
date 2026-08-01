package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	SourceHealthHealthy      = "HEALTHY"
	SourceHealthPrivate      = "PRIVATE"
	SourceHealthDisabled     = "DISABLED"
	SourceHealthAuthFailed   = "AUTH_FAILED"
	SourceHealthNotFollowing = "NOT_FOLLOWING_LEADER"
	SourceHealthDegraded     = "DEGRADED"
	SourceHealthStale        = "STALE"
)

const sourceHealthStaleAfter = 2 * time.Minute

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
	LastRequestStartedAt   *time.Time                       `json:"last_request_started_at,omitempty"`
	LastRequestCompletedAt *time.Time                       `json:"last_request_completed_at,omitempty"`
	LastRequestDurationMS  int64                            `json:"last_request_duration_ms"`
	NextPollAt             *time.Time                       `json:"next_poll_at,omitempty"`
	BackoffUntil           *time.Time                       `json:"backoff_until,omitempty"`
	RateLimit429Count      int                              `json:"rate_limit_429_count"`
	LastProcessingDelayMS  int64                            `json:"last_processing_delay_ms"`
	LastMailStatus         string                           `json:"last_mail_status,omitempty"`
	LastMailError          string                           `json:"last_mail_error,omitempty"`
	LastMailAt             *time.Time                       `json:"last_mail_at,omitempty"`
	ConsecutiveFailures    int                              `json:"consecutive_failures"`
	LastError              string                           `json:"last_error,omitempty"`
	UnsupportedContracts   []UnsupportedExecutionInstrument `json:"unsupported_contracts,omitempty"`
}

type SourceHealthObservation struct {
	Status             string
	TraderName         string
	Error              string
	CompleteSnapshot   bool
	CheckedAt          time.Time
	RequestStartedAt   time.Time
	RequestCompletedAt time.Time
	RequestDurationMS  int64
	NextPollAt         time.Time
	BackoffUntil       time.Time
	RateLimit429Count  int
	DirectRateLimit    bool
	ScheduledBackoff   bool
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
			last_request_started_at DATETIME,
			last_request_completed_at DATETIME,
			last_request_duration_ms INTEGER NOT NULL DEFAULT 0,
			next_poll_at DATETIME,
			backoff_until DATETIME,
			rate_limit_429_count INTEGER NOT NULL DEFAULT 0,
			last_processing_delay_ms INTEGER NOT NULL DEFAULT 0,
			last_mail_status TEXT DEFAULT '',
			last_mail_error TEXT DEFAULT '',
			last_mail_at DATETIME,
			consecutive_failures INTEGER NOT NULL DEFAULT 0,
			last_error TEXT DEFAULT ''
		)
	`)
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"last_request_started_at", "DATETIME"},
		{"last_request_completed_at", "DATETIME"},
		{"last_request_duration_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"next_poll_at", "DATETIME"},
		{"backoff_until", "DATETIME"},
		{"rate_limit_429_count", "INTEGER NOT NULL DEFAULT 0"},
		{"last_processing_delay_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"last_mail_status", "TEXT DEFAULT ''"},
		{"last_mail_error", "TEXT DEFAULT ''"},
		{"last_mail_at", "DATETIME"},
	} {
		if err = ensureSQLiteColumn(s.db, "copy_trade_source_health", column.name, column.definition); err != nil {
			return fmt.Errorf("migrate copy_trade_source_health.%s: %w", column.name, err)
		}
	}
	return nil
}

func sourceHealthFrozen(status string) bool {
	switch status {
	case SourceHealthPrivate, SourceHealthDisabled, SourceHealthAuthFailed, SourceHealthNotFollowing, SourceHealthStale:
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
	var checked, complete, transitioned, notified, requestStarted, requestCompleted, nextPoll, backoff, mailAt sql.NullString
	err := scanner.Scan(
		&h.TraderID, &h.LeaderID, &h.SourceMode, &h.SourceGeneration,
		&h.Status, &h.PreviousStatus, &h.TraderName,
		&checked, &complete, &transitioned, &notified,
		&requestStarted, &requestCompleted, &h.LastRequestDurationMS,
		&nextPoll, &backoff, &h.RateLimit429Count, &h.LastProcessingDelayMS,
		&h.LastMailStatus, &h.LastMailError, &mailAt,
		&h.ConsecutiveFailures, &h.LastError,
	)
	if err != nil {
		return nil, err
	}
	h.LastCheckedAt = parseOptionalSQLiteTime(checked)
	h.LastCompleteSnapshotAt = parseOptionalSQLiteTime(complete)
	h.LastTransitionAt = parseOptionalSQLiteTime(transitioned)
	h.LastNotifiedAt = parseOptionalSQLiteTime(notified)
	h.LastRequestStartedAt = parseOptionalSQLiteTime(requestStarted)
	h.LastRequestCompletedAt = parseOptionalSQLiteTime(requestCompleted)
	h.NextPollAt = parseOptionalSQLiteTime(nextPoll)
	h.BackoffUntil = parseOptionalSQLiteTime(backoff)
	h.LastMailAt = parseOptionalSQLiteTime(mailAt)
	return &h, nil
}

func parseOptionalSQLiteTime(v sql.NullString) *time.Time {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return nil
	}
	// 历史遗留：早期版本把 time.Time 直接绑给驱动，落库为 Go String() 格式并
	// 带单调时钟后缀（"… +0800 CST m=+77990.894116101"）。该后缀让所有布局
	// 解析失败 → LastCompleteSnapshotAt 恒为 nil → Smart Money 每轮都误判
	// "断供恢复"并把新开仓吸收进 no-chase 基线（GLWUSDT 漏跟单根因）。
	// 剥离后缀让旧行无需修库即可自愈；新写入已统一为规范 UTC 格式。
	value := v.String
	if idx := strings.Index(value, " m=+"); idx > 0 {
		value = value[:idx]
	} else if idx := strings.Index(value, " m=-"); idx > 0 {
		value = value[:idx]
	}
	// DATETIME 文本可能带或不带小数秒：通知时间常为整秒，轮询时间常带纳秒，
	// 两种变体都必须接受，否则持久化健康状态会静默丢失最后已知时间。
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return &parsed
		}
	}
	return nil
}

// sourceHealthTimeLayout 是健康表时间列的规范落库格式（UTC、可含纳秒）。
// 必须显式格式化后再绑定：modernc/sqlite 对裸 time.Time 会以 Go String()
// 落库，带单调时钟后缀导致读取解析失败（见 parseOptionalSQLiteTime）。
const sourceHealthTimeLayout = "2006-01-02 15:04:05.999999999"

func formatSourceHealthTime(t *time.Time) interface{} {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(sourceHealthTimeLayout)
}

const sourceHealthColumns = `trader_id, leader_id, source_mode, source_generation,
	status, COALESCE(previous_status,''), COALESCE(trader_name,''),
	CAST(last_checked_at AS TEXT), CAST(last_complete_snapshot_at AS TEXT),
	CAST(last_transition_at AS TEXT), CAST(last_notified_at AS TEXT),
	CAST(last_request_started_at AS TEXT), CAST(last_request_completed_at AS TEXT),
	last_request_duration_ms, CAST(next_poll_at AS TEXT), CAST(backoff_until AS TEXT),
	rate_limit_429_count, last_processing_delay_ms,
	COALESCE(last_mail_status,''), COALESCE(last_mail_error,''), CAST(last_mail_at AS TEXT),
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
	status := strings.ToUpper(strings.TrimSpace(obs.Status))
	if status == "" {
		status = "ERROR"
	}
	if obs.ScheduledBackoff {
		// The credential-scoped scheduler already recorded the one upstream 429.
		// Other traders merely observing that shared backoff must not each add a
		// failure or manufacture independent DEGRADED -> HEALTHY transitions.
		if obs.Error != "" {
			current.LastError = obs.Error
		}
	} else if obs.CompleteSnapshot && status == SourceHealthHealthy {
		current.ConsecutiveFailures = 0
		current.LastCompleteSnapshotAt = &obs.CheckedAt
		current.LastError = ""
		current.Status = SourceHealthHealthy
	} else if status == SourceHealthPrivate || status == SourceHealthDisabled || status == SourceHealthAuthFailed || status == SourceHealthNotFollowing {
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
		} else if current.LastCompleteSnapshotAt != nil && obs.CheckedAt.Sub(*current.LastCompleteSnapshotAt) > sourceHealthStaleAfter {
			current.Status = SourceHealthStale
		} else if current.ConsecutiveFailures >= 3 {
			current.Status = SourceHealthDegraded
		}
	}
	current.LastCheckedAt = &obs.CheckedAt
	current.LastRequestStartedAt = optionalSourceHealthTime(obs.RequestStartedAt)
	current.LastRequestCompletedAt = optionalSourceHealthTime(obs.RequestCompletedAt)
	current.LastRequestDurationMS = obs.RequestDurationMS
	current.NextPollAt = optionalSourceHealthTime(obs.NextPollAt)
	current.BackoffUntil = optionalSourceHealthTime(obs.BackoffUntil)
	current.RateLimit429Count = obs.RateLimit429Count
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
			 last_notified_at, last_request_started_at, last_request_completed_at,
			 last_request_duration_ms, next_poll_at, backoff_until, rate_limit_429_count,
			 last_processing_delay_ms, last_mail_status, last_mail_error, last_mail_at,
			 consecutive_failures, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trader_id) DO UPDATE SET
			leader_id=excluded.leader_id, source_mode=excluded.source_mode,
			source_generation=excluded.source_generation, status=excluded.status,
			previous_status=excluded.previous_status, trader_name=excluded.trader_name,
			last_checked_at=excluded.last_checked_at,
			last_complete_snapshot_at=excluded.last_complete_snapshot_at,
			last_transition_at=excluded.last_transition_at,
			last_notified_at=excluded.last_notified_at,
			last_request_started_at=excluded.last_request_started_at,
			last_request_completed_at=excluded.last_request_completed_at,
			last_request_duration_ms=excluded.last_request_duration_ms,
			next_poll_at=excluded.next_poll_at, backoff_until=excluded.backoff_until,
			rate_limit_429_count=excluded.rate_limit_429_count,
			last_processing_delay_ms=excluded.last_processing_delay_ms,
			last_mail_status=excluded.last_mail_status,last_mail_error=excluded.last_mail_error,
			last_mail_at=excluded.last_mail_at,
			consecutive_failures=excluded.consecutive_failures, last_error=excluded.last_error
	`, current.TraderID, current.LeaderID, current.SourceMode, current.SourceGeneration,
		current.Status, current.PreviousStatus, current.TraderName, formatSourceHealthTime(current.LastCheckedAt),
		formatSourceHealthTime(current.LastCompleteSnapshotAt), formatSourceHealthTime(current.LastTransitionAt), formatSourceHealthTime(current.LastNotifiedAt),
		formatSourceHealthTime(current.LastRequestStartedAt), formatSourceHealthTime(current.LastRequestCompletedAt), current.LastRequestDurationMS,
		formatSourceHealthTime(current.NextPollAt), formatSourceHealthTime(current.BackoffUntil), current.RateLimit429Count,
		current.LastProcessingDelayMS, current.LastMailStatus, current.LastMailError, formatSourceHealthTime(current.LastMailAt),
		current.ConsecutiveFailures, current.LastError)
	if err != nil {
		return nil, false, fmt.Errorf("save source health: %w", err)
	}
	return current, transitioned, nil
}

func optionalSourceHealthTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func (s *CopyTradeStore) MarkSourceHealthNotified(traderID string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE copy_trade_source_health SET last_notified_at=? WHERE trader_id=?`, formatSourceHealthTime(&at), traderID)
	return err
}

func (s *CopyTradeStore) MarkSourceHealthMailDelivery(traderID, status, message string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE copy_trade_source_health SET last_mail_status=?,last_mail_error=?,last_mail_at=? WHERE trader_id=?`, status, message, formatSourceHealthTime(&at), traderID)
	return err
}

func (s *CopyTradeStore) MarkSourceHealthProcessingDelay(traderID string, delay time.Duration) error {
	if delay < 0 {
		delay = 0
	}
	_, err := s.db.Exec(`UPDATE copy_trade_source_health SET last_processing_delay_ms=? WHERE trader_id=?`, delay.Milliseconds(), traderID)
	return err
}
