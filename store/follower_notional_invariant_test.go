package store

import (
	"path/filepath"
	"testing"
)

func newNotionalGuardCycle(t *testing.T, cs *CopyTradeStore, equity float64) *CopyGuardCycle {
	t.Helper()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-1",
		Symbol: "BTCUSDT", Side: "long", MarginMode: "cross",
		Status: CopyGuardFollowing, PolicySnapshot: "{}",
		LeaderEntryPrice: 100, FollowerEntryPrice: 100, FollowerNotional: 100,
		AccountEquity: equity, LastObservedPrice: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cycle
}

// A notional above 125x equity cannot exist on any supported venue, so it can
// only be accumulation damage. Left in place it silently scales baseline_pnl
// and net_guard_effect, corrupting the numbers used to judge Copy Guard.
func TestFollowerNotionalInvariantClampsImpossibleValue(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "notional.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle := newNotionalGuardCycle(t, cs, 68)

	if err := cs.UpdateCopyGuardFollowerPosition(cycle.ID, "follower-pos", 100, 44587); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := 68 * maxFollowerNotionalLeverage
	if got.FollowerNotional != want {
		t.Fatalf("notional = %.4f, want clamped to %.4f", got.FollowerNotional, want)
	}

	events, err := cs.ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type != "FOLLOWER_NOTIONAL_INVARIANT_BREACH" {
			continue
		}
		found = true
		// The pre-clamp value must survive in the event or the write race that
		// produced it becomes undiagnosable.
		if observed, ok := e.Metadata["observed_notional"].(float64); !ok || observed != 44587 {
			t.Fatalf("breach event lost the observed value: %+v", e.Metadata)
		}
	}
	if !found {
		t.Fatalf("clamp must leave an audit trail, events=%d", len(events))
	}
}

func TestFollowerNotionalInvariantLeavesPlausibleValueAlone(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "notional-ok.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle := newNotionalGuardCycle(t, cs, 68)

	if err := cs.UpdateCopyGuardFollowerPosition(cycle.ID, "follower-pos", 100, 637); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FollowerNotional != 637 {
		t.Fatalf("ordinary notional must be untouched, got %.4f", got.FollowerNotional)
	}
	events, err := cs.ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "FOLLOWER_NOTIONAL_INVARIANT_BREACH" {
			t.Fatalf("a 9x position must not be flagged: %+v", e.Metadata)
		}
	}
}

// Equity is backfilled asynchronously when the open-time snapshot was rate
// limited. Without it there is no basis to call any notional impossible, and
// guessing would clamp legitimate positions to zero.
func TestFollowerNotionalInvariantSkippedWithoutEquity(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "notional-noequity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle := newNotionalGuardCycle(t, cs, 0)

	if err := cs.UpdateCopyGuardFollowerPosition(cycle.ID, "follower-pos", 100, 44587); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FollowerNotional != 44587 {
		t.Fatalf("unjudgeable notional must be left as-is, got %.4f", got.FollowerNotional)
	}
}
