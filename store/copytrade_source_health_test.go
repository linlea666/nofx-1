package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCopyTradeSourceHealthStateMachine(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := time.Now().UTC().Truncate(time.Second)
	record := func(obs SourceHealthObservation) *CopyTradeSourceHealth {
		h, _, err := cs.RecordSourceHealthObservation("trader", "leader", "smart_money", 1, obs)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := record(SourceHealthObservation{Status: SourceHealthHealthy, CompleteSnapshot: true, CheckedAt: base})
	if h.Status != SourceHealthHealthy || h.LastCompleteSnapshotAt == nil {
		t.Fatalf("initial healthy: %+v", h)
	}
	for i := 1; i <= 2; i++ {
		h = record(SourceHealthObservation{Status: "ERROR", Error: "temporary", CheckedAt: base.Add(time.Duration(i) * time.Second)})
		if h.Status != SourceHealthHealthy {
			t.Fatalf("failure %d should retain healthy, got %s", i, h.Status)
		}
	}
	h = record(SourceHealthObservation{Status: "ERROR", Error: "temporary", CheckedAt: base.Add(3 * time.Second)})
	if h.Status != SourceHealthDegraded || h.ConsecutiveFailures != 3 {
		t.Fatalf("third failure: %+v", h)
	}
	h = record(SourceHealthObservation{Status: SourceHealthPrivate, Error: "hidden", CheckedAt: base.Add(4 * time.Second)})
	if h.Status != SourceHealthPrivate || !h.Frozen() {
		t.Fatalf("private must freeze: %+v", h)
	}
	for i := 5; i <= 8; i++ {
		h = record(SourceHealthObservation{Status: "ERROR", Error: "network after private", CheckedAt: base.Add(time.Duration(i) * time.Second)})
		if h.Status != SourceHealthPrivate || !h.Frozen() {
			t.Fatalf("private must remain sticky through transient failure %d: %+v", i, h)
		}
	}
	h = record(SourceHealthObservation{Status: SourceHealthHealthy, CompleteSnapshot: false, CheckedAt: base.Add(9 * time.Second)})
	if h.Status == SourceHealthHealthy {
		t.Fatalf("incomplete snapshot must not recover: %+v", h)
	}
	h = record(SourceHealthObservation{Status: SourceHealthHealthy, CompleteSnapshot: true, CheckedAt: base.Add(10 * time.Second)})
	if h.Status != SourceHealthHealthy || h.ConsecutiveFailures != 0 || h.LastError != "" {
		t.Fatalf("complete recovery: %+v", h)
	}
	h = record(SourceHealthObservation{Status: "ERROR", Error: "old snapshot", CheckedAt: base.Add(71 * time.Second)})
	if h.Status != SourceHealthStale || !h.Frozen() {
		t.Fatalf("stale must freeze: %+v", h)
	}
}

func TestCopyTradeSourceHealthGenerationAndNotificationPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health-restart.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	checked := time.Now().UTC().Truncate(time.Second)
	h, _, err := st.CopyTrade().RecordSourceHealthObservation("trader", "leader-a", "smart_money", 1, SourceHealthObservation{Status: SourceHealthPrivate, Error: "hidden", CheckedAt: checked})
	if err != nil {
		t.Fatal(err)
	}
	notified := checked.Add(time.Minute)
	if err := st.CopyTrade().MarkSourceHealthNotified("trader", notified); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h, err = st.CopyTrade().GetSourceHealth("trader")
	if err != nil || h.LastNotifiedAt == nil || h.LastNotifiedAt.Unix() != notified.Unix() {
		t.Fatalf("restart persistence: %+v err=%v", h, err)
	}
	h, transitioned, err := st.CopyTrade().RecordSourceHealthObservation("trader", "leader-b", "smart_money", 2, SourceHealthObservation{Status: SourceHealthHealthy, CompleteSnapshot: true, CheckedAt: checked.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if transitioned || h.SourceGeneration != 2 || h.LeaderID != "leader-b" || h.LastNotifiedAt != nil {
		t.Fatalf("generation must reset state: %+v transitioned=%v", h, transitioned)
	}
}
