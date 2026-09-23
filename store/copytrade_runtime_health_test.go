package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeIssuesKeepIncidentIdentityWithoutCouplingExecution(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	issue := CopyRuntimeIssue{TraderID: "t", Area: "protection", ResourceID: "p", Symbol: "ETHUSDT", Side: "long", Code: "RECOVERY_PENDING", Detail: "missing original fill evidence"}
	if changed, err := cs.RecordRuntimeIssue(issue); err != nil || !changed {
		t.Fatalf("first issue: %v %v", changed, err)
	}
	first, err := cs.ListRuntimeIssues("t")
	if err != nil || len(first) != 1 {
		t.Fatalf("issues: %+v %v", first, err)
	}
	if changed, err := cs.RecordRuntimeIssue(issue); err != nil || changed {
		t.Fatalf("duplicate should not alert: %v %v", changed, err)
	}
	again, _ := cs.ListRuntimeIssues("t")
	if !again[0].FirstSeen.Equal(first[0].FirstSeen) {
		t.Fatal("retry reset waiting age")
	}
	if other, err := cs.ListRuntimeIssues("other"); err != nil || len(other) != 0 {
		t.Fatalf("cross-trader issue leak: %+v %v", other, err)
	}
	if err := cs.ResolveRuntimeIssue("t", "protection", "p"); err != nil {
		t.Fatal(err)
	}
	if rows, err := cs.ListRuntimeIssues("t"); err != nil || len(rows) != 0 {
		t.Fatalf("resolved still active: %+v %v", rows, err)
	}
	if changed, err := cs.RecordRuntimeIssue(issue); err != nil || !changed {
		t.Fatalf("new incident suppressed: %v %v", changed, err)
	}
	var orders int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents`).Scan(&orders); err != nil || orders != 0 {
		t.Fatal("protection issue mutated business execution", orders, err)
	}
}

func TestRuntimeSourceLateFailureCannotOverwriteNewSuccessfulSnapshot(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "source-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := cs.ObserveRuntimeSource("t", now, errors.New("temporary source outage")); err != nil {
		t.Fatal(err)
	}
	if err := cs.ObserveRuntimeSource("t", now.Add(time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if err := cs.ObserveRuntimeSource("t", now.Add(-time.Second), errors.New("late stale error")); err != nil {
		t.Fatal(err)
	}
	health, err := cs.GetRuntimeSource("t")
	if err != nil || health.LastError != "" || health.LastSuccessAt == nil || !health.LastSuccessAt.Equal(now.Add(time.Second)) {
		t.Fatalf("out-of-order health %+v %v", health, err)
	}
	if err := cs.ObserveRuntimeSource("t", now.Add(2*time.Second), errors.New("fresh failure")); err != nil {
		t.Fatal(err)
	}
	health, err = cs.GetRuntimeSource("t")
	if err != nil || health.LastError != "fresh failure" || health.LastSuccessAt == nil {
		t.Fatalf("lost last successful evidence %+v %v", health, err)
	}
}
