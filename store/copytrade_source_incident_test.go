package store

import (
	"path/filepath"
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

func TestSourceIncidentDedupedDeliveryDoesNotScheduleAnotherRetry(t *testing.T) {
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
	if err = st.CopyTrade().MarkSourceIncidentMailDelivery(incident.ID, SourceIncidentMailFrozen, "deduped", "", now); err != nil {
		t.Fatal(err)
	}
	incident, action, err = st.CopyTrade().RecordSourceIncidentObservation(SourceIncidentObservation{
		ScopeKey: incident.ScopeKey, ScopeKind: incident.ScopeKind,
		TraderID: "trader", LeaderID: "leader", Cause: "HTTP_429", Error: "429 again",
		Failed: true, ObservedAt: now.Add(10 * time.Minute),
	})
	if err != nil || action != "" || incident.FrozenMailStatus != "SENT" || incident.FrozenMailNextAttempt != nil {
		t.Fatalf("deduped delivery retried: incident=%+v action=%s err=%v", incident, action, err)
	}
}
