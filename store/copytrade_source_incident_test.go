package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceIncidentEmitsOneRecoveryAfterStableHealth(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "source-incident.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := time.Now().UTC().Truncate(time.Second)
	observe := func(obs SourceIncidentObservation) (*CopyTradeSourceIncident, string) {
		t.Helper()
		incident, action, observeErr := st.CopyTrade().RecordSourceIncidentObservation(obs)
		if observeErr != nil {
			t.Fatal(observeErr)
		}
		return incident, action
	}

	incident, action := observe(SourceIncidentObservation{
		ScopeKey: "binance:credential:default", ScopeKind: SourceIncidentScopeCredential,
		TraderID: "trader-a", LeaderID: "leader-a", Cause: "RATE_LIMIT_429",
		Error: "429", Failed: true, ObservedAt: base,
	})
	if incident == nil || action != SourceIncidentMailFrozen {
		t.Fatalf("first failure must open and claim one frozen notification: incident=%+v action=%s", incident, action)
	}
	if err = st.CopyTrade().MarkSourceIncidentMailDelivery(incident.ID, SourceIncidentMailFrozen, "sent", "", base); err != nil {
		t.Fatal(err)
	}

	// Momentary complete snapshots do not close the incident or emit recovery.
	for _, offset := range []time.Duration{time.Minute, 5 * time.Minute, 14 * time.Minute} {
		current, got := observe(SourceIncidentObservation{
			ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind, TraderID: "trader-a", LeaderID: "leader-a",
			Healthy: true, ObservedAt: base.Add(offset),
		})
		if got != "" || current.Status == SourceIncidentClosed {
			t.Fatalf("unstable health emitted recovery at %s: incident=%+v action=%s", offset, current, got)
		}
	}

	// Another 429 belongs to the same open incident and resets stabilization.
	same, action := observe(SourceIncidentObservation{
		ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind, TraderID: "trader-b", LeaderID: "leader-b",
		Cause: "RATE_LIMIT_429", Error: "429 again", Failed: true, ObservedAt: base.Add(14*time.Minute + time.Second),
	})
	if same.ID != incident.ID || action != "" {
		t.Fatalf("flap created another incident or mail: first=%d same=%+v action=%s", incident.ID, same, action)
	}

	lastFailure := base.Add(14*time.Minute + time.Second)
	for index, offset := range []time.Duration{time.Minute, 8 * time.Minute, sourceIncidentRecoveryStableAfter} {
		current, got := observe(SourceIncidentObservation{
			ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind, TraderID: "trader-a", LeaderID: "leader-a",
			Healthy: true, ObservedAt: lastFailure.Add(offset),
		})
		if index < 2 && got != "" {
			t.Fatalf("early recovery action=%s at %s", got, offset)
		}
		if index == 2 {
			if got != SourceIncidentMailRecovered || current.Status != SourceIncidentClosed {
				t.Fatalf("stable recovery not atomically closed/claimed: incident=%+v action=%s", current, got)
			}
			if err = st.CopyTrade().MarkSourceIncidentMailDelivery(current.ID, SourceIncidentMailRecovered, "sent", "", lastFailure.Add(offset)); err != nil {
				t.Fatal(err)
			}
		}
	}

	if current, got := observe(SourceIncidentObservation{
		ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind, Healthy: true,
		TraderID: "trader-b", LeaderID: "leader-b", ObservedAt: lastFailure.Add(20 * time.Minute),
	}); got != "" || current.ID != incident.ID {
		t.Fatalf("closed incident emitted duplicate recovery: incident=%+v action=%s", current, got)
	}

	second, action := observe(SourceIncidentObservation{
		ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind, Cause: "RATE_LIMIT_429", Error: "new outage",
		TraderID: "trader-a", LeaderID: "leader-a", Failed: true, ObservedAt: lastFailure.Add(30 * time.Minute),
	})
	if second.ID == incident.ID || action != SourceIncidentMailFrozen {
		t.Fatalf("a later real outage must receive a new incident: first=%d second=%+v action=%s", incident.ID, second, action)
	}
}

func TestSourceIncidentSentDeliverySchedulesBoundedReminder(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "source-incident-dedup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Second)
	incident, action, err := st.CopyTrade().RecordSourceIncidentObservation(SourceIncidentObservation{
		ScopeKey: "binance:credential:dedup", ScopeKind: SourceIncidentScopeCredential,
		TraderID: "trader", LeaderID: "leader", Cause: "HTTP_429", Error: "429",
		Failed: true, ObservedAt: now,
	})
	if err != nil || action != SourceIncidentMailFrozen {
		t.Fatalf("open incident: incident=%+v action=%s err=%v", incident, action, err)
	}
	if err = st.CopyTrade().MarkSourceIncidentMailDelivery(incident.ID, SourceIncidentMailFrozen, "sent", "", now); err != nil {
		t.Fatal(err)
	}
	incident, action, err = st.CopyTrade().RecordSourceIncidentObservation(SourceIncidentObservation{
		ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind,
		TraderID: "trader", LeaderID: "leader", Cause: "HTTP_429", Error: "429 again",
		Failed: true, ObservedAt: now.Add(10 * time.Minute),
	})
	if err != nil || action != "" || incident.FrozenMailStatus != "SENT" || incident.FrozenMailSentCount != 1 || incident.FrozenMailNextAttempt == nil {
		t.Fatalf("sent delivery did not enter reminder schedule: incident=%+v action=%s err=%v", incident, action, err)
	}
	incident, action, err = st.CopyTrade().RecordSourceIncidentObservation(SourceIncidentObservation{
		ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind,
		TraderID: "trader", LeaderID: "leader", Cause: "HTTP_429", Error: "still limited",
		Failed: true, ObservedAt: now.Add(SourceIncidentFirstReminderAfter),
	})
	if err != nil || action != SourceIncidentMailFrozen || incident.FrozenMailSentCount != 1 {
		t.Fatalf("first reminder was not claimed at one hour: incident=%+v action=%s err=%v", incident, action, err)
	}
	if err = st.CopyTrade().MarkSourceIncidentMailDelivery(incident.ID, SourceIncidentMailFrozen, "sent", "", now.Add(SourceIncidentFirstReminderAfter)); err != nil {
		t.Fatal(err)
	}
	incident, action, err = st.CopyTrade().RecordSourceIncidentObservation(SourceIncidentObservation{
		ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind,
		TraderID: "trader", LeaderID: "leader", Cause: "HTTP_429", Error: "still limited",
		Failed: true, ObservedAt: now.Add(SourceIncidentFirstReminderAfter + 5*time.Hour),
	})
	if err != nil || action != "" {
		t.Fatalf("recurring reminder fired before six hours: incident=%+v action=%s err=%v", incident, action, err)
	}
	incident, action, err = st.CopyTrade().RecordSourceIncidentObservation(SourceIncidentObservation{
		ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind,
		TraderID: "trader", LeaderID: "leader", Cause: "HTTP_429", Error: "still limited",
		Failed: true, ObservedAt: now.Add(SourceIncidentFirstReminderAfter + SourceIncidentReminderAfter),
	})
	if err != nil || action != SourceIncidentMailFrozen || incident.FrozenMailSentCount != 2 {
		t.Fatalf("recurring reminder was not claimed at six hours: incident=%+v action=%s err=%v", incident, action, err)
	}
}

func TestSourceIncidentDedupedDeliveryWaitsForRealSMTPResult(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "source-incident-dedup-queued.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Second)
	incident, action, err := st.CopyTrade().RecordSourceIncidentObservation(SourceIncidentObservation{
		ScopeKey: "binance:credential:dedup-queued", ScopeKind: SourceIncidentScopeCredential,
		TraderID: "trader", LeaderID: "leader", Cause: "HTTP_429", Error: "429", Failed: true, ObservedAt: now,
	})
	if err != nil || action != SourceIncidentMailFrozen {
		t.Fatalf("initial notification was not claimed: incident=%+v action=%s err=%v", incident, action, err)
	}
	if err = st.CopyTrade().MarkSourceIncidentMailDelivery(incident.ID, action, "DEDUPED", "", now); err != nil {
		t.Fatal(err)
	}
	incident, err = scanSourceIncident(st.DB().QueryRow(`SELECT `+sourceIncidentSelect+` FROM copy_trade_source_incidents WHERE id=?`, incident.ID))
	if err != nil {
		t.Fatal(err)
	}
	if incident.FrozenMailStatus != "QUEUED" || incident.FrozenMailSentCount != 0 || incident.FrozenMailNextAttempt == nil {
		t.Fatalf("deduped queue state was mistaken for SMTP delivery: %+v", incident)
	}
}

func TestSourceIncidentMigrationPreservesPreviouslySentFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source-incident-sent-migration.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	_, err = st.DB().Exec(`INSERT INTO copy_trade_source_incidents
		(scope_key,scope_kind,status,cause,last_error,opened_at,last_failure_at,frozen_mail_status,
		 frozen_mail_sent_count,frozen_mail_last_attempt_at,recovery_mail_status)
		VALUES(?,?,'OPEN','HTTP_429','legacy',?,?, 'SENT',0,?,'NONE')`,
		"binance:credential:legacy-sent", SourceIncidentScopeCredential, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var sentCount int
	var nextAttempt sql.NullString
	if err = reopened.DB().QueryRow(`SELECT frozen_mail_sent_count,CAST(frozen_mail_next_attempt_at AS TEXT)
		FROM copy_trade_source_incidents WHERE scope_key=?`, "binance:credential:legacy-sent").Scan(&sentCount, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if sentCount != 1 || !nextAttempt.Valid || strings.TrimSpace(nextAttempt.String) == "" {
		t.Fatalf("legacy sent incident was not migrated: sent=%d next=%+v", sentCount, nextAttempt)
	}
}

func TestTransientSourceIncidentDoesNotSendFailureOrRecoveryMail(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "source-incident-transient.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := time.Now().UTC().Truncate(time.Second)
	incident, action, err := st.CopyTrade().RecordSourceIncidentObservation(SourceIncidentObservation{
		ScopeKey: "binance:credential:transient", ScopeKind: SourceIncidentScopeCredential,
		TraderID: "trader", LeaderID: "leader", Cause: "HTTP_429", Error: "single 429",
		Failed: true, ObservedAt: base, InitialNotificationDelay: 5 * time.Minute,
	})
	if err != nil || action != "" || incident.FrozenMailNextAttempt == nil {
		t.Fatalf("transient incident was not held in grace window: incident=%+v action=%s err=%v", incident, action, err)
	}
	for _, offset := range []time.Duration{time.Minute, 2 * time.Minute, 15 * time.Minute} {
		incident, action, err = st.CopyTrade().RecordSourceIncidentObservation(SourceIncidentObservation{
			ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind, TraderID: "trader", LeaderID: "leader",
			Healthy: true, ObservedAt: base.Add(offset),
		})
		if err != nil || action != "" {
			t.Fatalf("transient recovery emitted mail at %s: incident=%+v action=%s err=%v", offset, incident, action, err)
		}
	}
	if incident.Status != SourceIncidentClosed || incident.FrozenMailStatus != "CANCELLED" || incident.RecoveryMailStatus != "CANCELLED" {
		t.Fatalf("transient incident did not close silently: %+v", incident)
	}
}
