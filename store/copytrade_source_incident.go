package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	SourceIncidentScopeCredential = "CREDENTIAL"
	SourceIncidentScopeLeader     = "LEADER"
	SourceIncidentOpen            = "OPEN"
	SourceIncidentRecovering      = "RECOVERING"
	SourceIncidentClosed          = "CLOSED"
	SourceIncidentMailFrozen      = "FROZEN"
	SourceIncidentMailRecovered   = "RECOVERED"

	sourceIncidentRecoveryStableAfter = 15 * time.Minute
	sourceIncidentHealthySnapshots    = 3
	sourceIncidentClaimTTL            = 5 * time.Minute
	SourceIncidentFirstReminderAfter  = time.Hour
	SourceIncidentReminderAfter       = 6 * time.Hour
)

type CopyTradeSourceIncident struct {
	ID                      int64
	ScopeKey                string
	ScopeKind               string
	Status                  string
	Cause                   string
	LastError               string
	OpenedAt                time.Time
	LastFailureAt           time.Time
	RecoveryStartedAt       *time.Time
	ClosedAt                *time.Time
	HealthySnapshots        int
	FailureCount            int
	FrozenMailStatus        string
	FrozenMailAttempts      int
	FrozenMailSentCount     int
	FrozenMailLastAttempt   *time.Time
	FrozenMailNextAttempt   *time.Time
	RecoveryMailStatus      string
	RecoveryMailAttempts    int
	RecoveryMailLastAttempt *time.Time
	RecoveryMailNextAttempt *time.Time
}

type SourceIncidentObservation struct {
	ScopeKey   string
	ScopeKind  string
	TraderID   string
	LeaderID   string
	Cause      string
	Error      string
	Failed     bool
	Healthy    bool
	ObservedAt time.Time
	// InitialNotificationDelay suppresses noisy transient failures such as a
	// single HTTP 429. It does not delay risk freezing or source recovery.
	InitialNotificationDelay time.Duration
}

type SourceIncidentMailPolicy struct {
	FirstReminderAfter time.Duration
	ReminderAfter      time.Duration
}

func normalizeSourceIncidentMailPolicy(policy SourceIncidentMailPolicy) SourceIncidentMailPolicy {
	if policy.FirstReminderAfter <= 0 {
		policy.FirstReminderAfter = SourceIncidentFirstReminderAfter
	}
	if policy.ReminderAfter <= 0 {
		policy.ReminderAfter = SourceIncidentReminderAfter
	}
	return policy
}

func (s *CopyTradeStore) initSourceIncidentTable() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_source_incidents (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scope_key TEXT NOT NULL,
		scope_kind TEXT NOT NULL,
		status TEXT NOT NULL,
		cause TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		opened_at DATETIME NOT NULL,
		last_failure_at DATETIME NOT NULL,
		recovery_started_at DATETIME,
		closed_at DATETIME,
		healthy_snapshots INTEGER NOT NULL DEFAULT 0,
		failure_count INTEGER NOT NULL DEFAULT 0,
		frozen_mail_status TEXT NOT NULL DEFAULT 'PENDING',
		frozen_mail_attempts INTEGER NOT NULL DEFAULT 0,
		frozen_mail_sent_count INTEGER NOT NULL DEFAULT 0,
		frozen_mail_last_attempt_at DATETIME,
		frozen_mail_next_attempt_at DATETIME,
		recovery_mail_status TEXT NOT NULL DEFAULT 'NONE',
		recovery_mail_attempts INTEGER NOT NULL DEFAULT 0,
		recovery_mail_last_attempt_at DATETIME,
		recovery_mail_next_attempt_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return err
	}
	if err := ensureSQLiteColumn(s.db, "copy_trade_source_incidents", "frozen_mail_sent_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Existing releases used frozen_mail_status='SENT' as the durable proof of
	// a delivered outage notification. Preserve that proof when introducing
	// reminder counts, otherwise an old open incident would never remind and a
	// later recovery would be incorrectly suppressed.
	if _, err := s.db.Exec(`UPDATE copy_trade_source_incidents
		SET frozen_mail_sent_count=1,
			frozen_mail_next_attempt_at=COALESCE(
				frozen_mail_next_attempt_at,
				datetime(substr(COALESCE(
					CAST(frozen_mail_last_attempt_at AS TEXT),CAST(updated_at AS TEXT),CURRENT_TIMESTAMP
				),1,19), '+1 hour')
			)
		WHERE frozen_mail_sent_count=0 AND UPPER(frozen_mail_status)='SENT'`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_copy_source_incident_live
		ON copy_trade_source_incidents(scope_key) WHERE status IN ('OPEN','RECOVERING')`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_source_incident_members (
		incident_id INTEGER NOT NULL,
		trader_id TEXT NOT NULL,
		leader_id TEXT NOT NULL DEFAULT '',
		first_seen_at DATETIME NOT NULL,
		last_seen_at DATETIME NOT NULL,
		PRIMARY KEY(incident_id,trader_id),
		FOREIGN KEY(incident_id) REFERENCES copy_trade_source_incidents(id) ON DELETE CASCADE
	)`); err != nil {
		return err
	}
	return nil
}

const sourceIncidentSelect = `id,scope_key,scope_kind,status,cause,last_error,
	CAST(opened_at AS TEXT),CAST(last_failure_at AS TEXT),CAST(recovery_started_at AS TEXT),CAST(closed_at AS TEXT),
	healthy_snapshots,failure_count,frozen_mail_status,frozen_mail_attempts,
	frozen_mail_sent_count,
	CAST(frozen_mail_last_attempt_at AS TEXT),CAST(frozen_mail_next_attempt_at AS TEXT),
	recovery_mail_status,recovery_mail_attempts,
	CAST(recovery_mail_last_attempt_at AS TEXT),CAST(recovery_mail_next_attempt_at AS TEXT)`

func scanSourceIncident(scanner interface{ Scan(...interface{}) error }) (*CopyTradeSourceIncident, error) {
	var out CopyTradeSourceIncident
	var opened, failed, recovering, closed, frozenLast, frozenNext, recoveryLast, recoveryNext sql.NullString
	if err := scanner.Scan(&out.ID, &out.ScopeKey, &out.ScopeKind, &out.Status, &out.Cause, &out.LastError,
		&opened, &failed, &recovering, &closed, &out.HealthySnapshots, &out.FailureCount,
		&out.FrozenMailStatus, &out.FrozenMailAttempts, &out.FrozenMailSentCount, &frozenLast, &frozenNext,
		&out.RecoveryMailStatus, &out.RecoveryMailAttempts, &recoveryLast, &recoveryNext); err != nil {
		return nil, err
	}
	if parsed := parseOptionalSQLiteTime(opened); parsed != nil {
		out.OpenedAt = *parsed
	}
	if parsed := parseOptionalSQLiteTime(failed); parsed != nil {
		out.LastFailureAt = *parsed
	}
	out.RecoveryStartedAt = parseOptionalSQLiteTime(recovering)
	out.ClosedAt = parseOptionalSQLiteTime(closed)
	out.FrozenMailLastAttempt = parseOptionalSQLiteTime(frozenLast)
	out.FrozenMailNextAttempt = parseOptionalSQLiteTime(frozenNext)
	out.RecoveryMailLastAttempt = parseOptionalSQLiteTime(recoveryLast)
	out.RecoveryMailNextAttempt = parseOptionalSQLiteTime(recoveryNext)
	return &out, nil
}

func getLatestSourceIncidentTx(ctx context.Context, tx *sql.Tx, scopeKey string) (*CopyTradeSourceIncident, error) {
	incident, err := scanSourceIncident(tx.QueryRowContext(ctx, `SELECT `+sourceIncidentSelect+` FROM copy_trade_source_incidents WHERE scope_key=? ORDER BY id DESC LIMIT 1`, scopeKey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return incident, err
}

func (s *CopyTradeStore) RecordSourceIncidentObservation(obs SourceIncidentObservation) (*CopyTradeSourceIncident, string, error) {
	return s.RecordSourceIncidentObservationContext(context.Background(), obs)
}

func (s *CopyTradeStore) RecordSourceIncidentObservationContext(ctx context.Context, obs SourceIncidentObservation) (*CopyTradeSourceIncident, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	obs.ScopeKey = strings.TrimSpace(obs.ScopeKey)
	obs.ScopeKind = strings.ToUpper(strings.TrimSpace(obs.ScopeKind))
	if obs.ScopeKey == "" || (obs.ScopeKind != SourceIncidentScopeCredential && obs.ScopeKind != SourceIncidentScopeLeader) || obs.Failed == obs.Healthy {
		return nil, "", fmt.Errorf("invalid source incident observation")
	}
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = time.Now()
	}
	obs.ObservedAt = obs.ObservedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	incident, err := getLatestSourceIncidentTx(ctx, tx, obs.ScopeKey)
	if err != nil {
		return nil, "", err
	}
	nowValue := formatSourceHealthTime(&obs.ObservedAt)
	if obs.Failed {
		if incident == nil || incident.Status == SourceIncidentClosed {
			var firstNotificationAt interface{}
			if obs.InitialNotificationDelay > 0 {
				due := obs.ObservedAt.Add(obs.InitialNotificationDelay)
				firstNotificationAt = formatSourceHealthTime(&due)
			}
			res, insertErr := tx.ExecContext(ctx, `INSERT INTO copy_trade_source_incidents
				(scope_key,scope_kind,status,cause,last_error,opened_at,last_failure_at,healthy_snapshots,failure_count,
				 frozen_mail_status,frozen_mail_next_attempt_at,recovery_mail_status)
				VALUES(?,?,'OPEN',?,?,?,?,0,1,'PENDING',?,'NONE')`, obs.ScopeKey, obs.ScopeKind, obs.Cause, obs.Error, nowValue, nowValue, firstNotificationAt)
			if insertErr != nil {
				return nil, "", insertErr
			}
			id, idErr := res.LastInsertId()
			if idErr != nil {
				return nil, "", idErr
			}
			incident, err = scanSourceIncident(tx.QueryRowContext(ctx, `SELECT `+sourceIncidentSelect+` FROM copy_trade_source_incidents WHERE id=?`, id))
			if err != nil {
				return nil, "", err
			}
		} else {
			clearDelayedFirstNotice := obs.InitialNotificationDelay <= 0 && incident.FrozenMailSentCount == 0
			if _, err = tx.ExecContext(ctx, `UPDATE copy_trade_source_incidents SET status='OPEN',cause=?,last_error=?,last_failure_at=?,
				recovery_started_at=NULL,healthy_snapshots=0,failure_count=failure_count+1,
				frozen_mail_next_attempt_at=CASE WHEN ? THEN NULL ELSE frozen_mail_next_attempt_at END,updated_at=? WHERE id=?`,
				obs.Cause, obs.Error, nowValue, clearDelayedFirstNotice, nowValue, incident.ID); err != nil {
				return nil, "", err
			}
			incident, err = scanSourceIncident(tx.QueryRowContext(ctx, `SELECT `+sourceIncidentSelect+` FROM copy_trade_source_incidents WHERE id=?`, incident.ID))
			if err != nil {
				return nil, "", err
			}
		}
	} else if incident == nil {
		if err = tx.Commit(); err != nil {
			return nil, "", err
		}
		return nil, "", nil
	} else if incident.Status != SourceIncidentClosed {
		if _, err = tx.ExecContext(ctx, `UPDATE copy_trade_source_incidents SET status='RECOVERING',
			recovery_started_at=COALESCE(recovery_started_at,?),healthy_snapshots=healthy_snapshots+1,updated_at=? WHERE id=?`,
			nowValue, nowValue, incident.ID); err != nil {
			return nil, "", err
		}
		incident, err = scanSourceIncident(tx.QueryRowContext(ctx, `SELECT `+sourceIncidentSelect+` FROM copy_trade_source_incidents WHERE id=?`, incident.ID))
		if err != nil {
			return nil, "", err
		}
		if incident.HealthySnapshots >= sourceIncidentHealthySnapshots && !incident.LastFailureAt.IsZero() &&
			obs.ObservedAt.Sub(incident.LastFailureAt) >= sourceIncidentRecoveryStableAfter {
			if _, err = tx.ExecContext(ctx, `UPDATE copy_trade_source_incidents SET status='CLOSED',closed_at=?,
				frozen_mail_status=CASE WHEN frozen_mail_sent_count>0 THEN 'SENT' ELSE 'CANCELLED' END,
				frozen_mail_next_attempt_at=NULL,
				recovery_mail_status=CASE WHEN frozen_mail_sent_count>0 THEN 'PENDING' ELSE 'CANCELLED' END,
				updated_at=? WHERE id=? AND status='RECOVERING'`, nowValue, nowValue, incident.ID); err != nil {
				return nil, "", err
			}
			incident, err = scanSourceIncident(tx.QueryRowContext(ctx, `SELECT `+sourceIncidentSelect+` FROM copy_trade_source_incidents WHERE id=?`, incident.ID))
			if err != nil {
				return nil, "", err
			}
		}
	}
	if incident != nil && strings.TrimSpace(obs.TraderID) != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO copy_trade_source_incident_members(incident_id,trader_id,leader_id,first_seen_at,last_seen_at)
			VALUES(?,?,?,?,?) ON CONFLICT(incident_id,trader_id) DO UPDATE SET leader_id=excluded.leader_id,last_seen_at=excluded.last_seen_at`,
			incident.ID, obs.TraderID, obs.LeaderID, nowValue, nowValue); err != nil {
			return nil, "", err
		}
	}
	action, err := claimSourceIncidentMailTx(ctx, tx, incident, obs.ObservedAt)
	if err != nil {
		return nil, "", err
	}
	if incident != nil {
		incident, err = scanSourceIncident(tx.QueryRowContext(ctx, `SELECT `+sourceIncidentSelect+` FROM copy_trade_source_incidents WHERE id=?`, incident.ID))
		if err != nil {
			return nil, "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}
	return incident, action, nil
}

func sourceIncidentMailDue(status string, next, last *time.Time, now time.Time, allowSentReminder bool) bool {
	switch strings.ToUpper(status) {
	case "PENDING", "FAILED":
		return next == nil || !next.After(now)
	case "CLAIMED", "QUEUED":
		return last == nil || now.Sub(*last) >= sourceIncidentClaimTTL
	case "SENT":
		return allowSentReminder && next != nil && !next.After(now)
	default:
		return false
	}
}

func claimSourceIncidentMailTx(ctx context.Context, tx *sql.Tx, incident *CopyTradeSourceIncident, now time.Time) (string, error) {
	if incident == nil {
		return "", nil
	}
	kind := ""
	if incident.Status == SourceIncidentClosed && sourceIncidentMailDue(incident.RecoveryMailStatus, incident.RecoveryMailNextAttempt, incident.RecoveryMailLastAttempt, now, false) {
		kind = SourceIncidentMailRecovered
	} else if incident.Status == SourceIncidentOpen && sourceIncidentMailDue(incident.FrozenMailStatus, incident.FrozenMailNextAttempt, incident.FrozenMailLastAttempt, now, true) {
		kind = SourceIncidentMailFrozen
	}
	if kind == "" {
		return "", nil
	}
	column := "frozen"
	if kind == SourceIncidentMailRecovered {
		column = "recovery"
	}
	allowedStatuses := "'PENDING','FAILED','CLAIMED','QUEUED'"
	if kind == SourceIncidentMailFrozen {
		allowedStatuses += ",'SENT'"
	}
	res, err := tx.ExecContext(ctx, `UPDATE copy_trade_source_incidents SET `+column+`_mail_status='CLAIMED',`+column+`_mail_last_attempt_at=?,updated_at=?
		WHERE id=? AND `+column+`_mail_status IN (`+allowedStatuses+`)`,
		formatSourceHealthTime(&now), formatSourceHealthTime(&now), incident.ID)
	if err != nil {
		return "", err
	}
	if count, _ := res.RowsAffected(); count != 1 {
		return "", nil
	}
	return kind, nil
}

func sourceIncidentRetryDelay(attempts int) time.Duration {
	delays := [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour}
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempts]
}

func (s *CopyTradeStore) MarkSourceIncidentMailDelivery(id int64, kind, delivery, message string, at time.Time) error {
	return s.MarkSourceIncidentMailDeliveryContext(context.Background(), id, kind, delivery, message, at, SourceIncidentMailPolicy{})
}

func (s *CopyTradeStore) MarkSourceIncidentMailDeliveryContext(ctx context.Context, id int64, kind, delivery, message string, at time.Time, policy SourceIncidentMailPolicy) error {
	if id <= 0 {
		return fmt.Errorf("invalid source incident mail delivery")
	}
	column := "frozen"
	if kind == SourceIncidentMailRecovered {
		column = "recovery"
	} else if kind != SourceIncidentMailFrozen {
		return fmt.Errorf("invalid source incident mail kind")
	}
	if at.IsZero() {
		at = time.Now()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	policy = normalizeSourceIncidentMailPolicy(policy)
	status := strings.ToUpper(strings.TrimSpace(delivery))
	next := interface{}(nil)
	var attempts, sentCount int
	if err := s.db.QueryRowContext(ctx, `SELECT `+column+`_mail_attempts,frozen_mail_sent_count FROM copy_trade_source_incidents WHERE id=?`, id).Scan(&attempts, &sentCount); err != nil {
		return err
	}
	switch status {
	case "SENT":
		status = "SENT"
		if kind == SourceIncidentMailFrozen {
			delay := policy.ReminderAfter
			if sentCount == 0 {
				delay = policy.FirstReminderAfter
			}
			nextAt := at.Add(delay)
			next = formatSourceHealthTime(&nextAt)
		}
	case "DEDUPED":
		// In-memory dedupe proves only that the same mail is already queued or
		// was accepted by this process; it is not proof of SMTP delivery. Keep
		// the durable claim retryable until the original sender reports SENT or
		// FAILED, so a deep queue or shutdown cannot manufacture a delivery.
		status = "QUEUED"
		nextAt := at.Add(sourceIncidentClaimTTL)
		next = formatSourceHealthTime(&nextAt)
	case "QUEUED":
		nextAt := at.Add(sourceIncidentClaimTTL)
		next = formatSourceHealthTime(&nextAt)
	case "RATE_LIMITED":
		status = "FAILED"
		nextAt := at.Add(time.Minute)
		next = formatSourceHealthTime(&nextAt)
	case "FAILED", "DROPPED", "DISABLED":
		status = "FAILED"
		nextAt := at.Add(sourceIncidentRetryDelay(attempts))
		next = formatSourceHealthTime(&nextAt)
	default:
		return nil
	}
	sentIncrement := 0
	if kind == SourceIncidentMailFrozen && status == "SENT" {
		sentIncrement = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE copy_trade_source_incidents SET `+column+`_mail_status=?,`+column+`_mail_attempts=`+column+`_mail_attempts+CASE WHEN ? IN ('SENT','FAILED') THEN 1 ELSE 0 END,
		frozen_mail_sent_count=frozen_mail_sent_count+?,
		`+column+`_mail_last_attempt_at=?,`+column+`_mail_next_attempt_at=?,last_error=CASE WHEN ?<>'' THEN ? ELSE last_error END,updated_at=? WHERE id=?`,
		status, status, sentIncrement, formatSourceHealthTime(&at), next, message, message, formatSourceHealthTime(&at), id)
	return err
}

func (s *CopyTradeStore) ListSourceIncidentMembers(id int64) ([]string, error) {
	return s.ListSourceIncidentMembersContext(context.Background(), id)
}

func (s *CopyTradeStore) ListSourceIncidentMembersContext(ctx context.Context, id int64) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT trader_id FROM copy_trade_source_incident_members WHERE incident_id=? ORDER BY trader_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}
