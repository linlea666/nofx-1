package store

import (
	"path/filepath"
	"testing"
)

func TestCopyGuardProtectiveReplacementSurvivesRestartAndClearsSafely(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "copyguard-protective-replacement.db")
	st, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID:       "trader-replacement",
		LeaderID:       "leader",
		LeaderPosID:    "position-replacement",
		Symbol:         "BTCUSDT",
		Side:           "long",
		MarginMode:     "cross",
		Status:         CopyGuardFollowing,
		PolicySnapshot: `{"version":4}`,
	})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}

	pending := &CopyGuardProtectiveOrder{
		CycleID:              cycle.ID,
		TraderID:             "trader-replacement",
		AlgoID:               "new-algo-id",
		AlgoClientID:         "new-client-id",
		PreviousAlgoID:       "old-algo-id",
		PreviousAlgoClientID: "old-client-id",
		ReplacementPending:   true,
		Symbol:               "BTCUSDT",
		Side:                 "long",
		MarginMode:           "cross",
		Quantity:             0.125,
		QuantityStep:         0.001,
		TriggerPrice:         60000,
		TriggerType:          "mark",
		Status:               "live",
	}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(pending); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	assertCopyGuardReplacementPending(t, st.CopyTrade(), cycle.ID)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	assertCopyGuardReplacementPending(t, st.CopyTrade(), cycle.ID)

	active, err := st.CopyTrade().ListActiveCopyGuardProtectiveOrders("trader-replacement")
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].AlgoID != "new-algo-id" || active[0].PreviousAlgoID != "old-algo-id" || !active[0].ReplacementPending {
		_ = st.Close()
		t.Fatalf("active replacement state was not recoverable after restart: %+v", active)
	}

	if err := st.CopyTrade().CompleteCopyGuardProtectiveReplacement(cycle.ID); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	cleared, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if cleared.AlgoID != "new-algo-id" || cleared.AlgoClientID != "new-client-id" {
		_ = st.Close()
		t.Fatalf("completing replacement changed the current order identity: %+v", cleared)
	}
	if cleared.PreviousAlgoID != "" || cleared.PreviousAlgoClientID != "" || cleared.ReplacementPending {
		_ = st.Close()
		t.Fatalf("completed replacement retained retiring-order state: %+v", cleared)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cleared, err = st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.AlgoID != "new-algo-id" || cleared.AlgoClientID != "new-client-id" || cleared.PreviousAlgoID != "" || cleared.PreviousAlgoClientID != "" || cleared.ReplacementPending {
		t.Fatalf("cleared replacement state did not persist across restart: %+v", cleared)
	}
}

func assertCopyGuardReplacementPending(t *testing.T, cs *CopyTradeStore, cycleID int64) {
	t.Helper()
	got, err := cs.GetCopyGuardProtectiveOrder(cycleID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AlgoID != "new-algo-id" || got.AlgoClientID != "new-client-id" {
		t.Fatalf("current protective order identity = %q/%q, want new-algo-id/new-client-id", got.AlgoID, got.AlgoClientID)
	}
	if got.PreviousAlgoID != "old-algo-id" || got.PreviousAlgoClientID != "old-client-id" {
		t.Fatalf("retiring protective order identity = %q/%q, want old-algo-id/old-client-id", got.PreviousAlgoID, got.PreviousAlgoClientID)
	}
	if !got.ReplacementPending {
		t.Fatalf("replacement_pending = false, want true: %+v", got)
	}
}
