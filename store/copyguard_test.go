package store

import (
	"path/filepath"
	"testing"
)

func TestCopyGuardCycleAndEventLedger(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-1", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 101, FollowerNotional: 1000, AccountEquity: 5000, LastObservedPrice: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.MarkCopyGuardStopped(cycle.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := cs.SaveCopyGuardEvent(&CopyGuardEvent{CycleID: cycle.ID, TraderID: "trader-1", Type: "STOP_TRIGGERED", Price: 98}); err != nil {
		t.Fatal(err)
	}
	if err := cs.MarkCopyGuardReentrySucceeded(cycle.ID, 99, 500); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != CopyGuardFollowingReentry || got.ReentryCount != 1 || got.StopCount != 1 {
		t.Fatalf("unexpected cycle: %+v", got)
	}
	events, err := cs.ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "STOP_TRIGGERED" {
		t.Fatalf("unexpected events: %+v", events)
	}
}
