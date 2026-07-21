package store

import (
	"path/filepath"
	"testing"
)

func TestExecutionIntentReservationIsCanonicalAndKeepsClientID(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "intent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, SourceFillID: "f1", Action: "open_short", Symbol: "BTCUSDT", ClientOrderID: "stable-id"}
	first, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed || first.ClientOrderID != "stable-id" {
		t.Fatalf("first=%+v claimed=%v err=%v", first, claimed, err)
	}
	duplicate, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, Action: "open_short", ClientOrderID: "different-id"})
	if err != nil || claimed || duplicate.ID != first.ID {
		t.Fatalf("duplicate=%+v claimed=%v err=%v", duplicate, claimed, err)
	}
	if err := cs.UpdateExecutionIntent(first.ID, ExecutionIntentFailed, "PRE_SUBMIT", "safe retry", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	reclaimed, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, SourceFillID: "f2", Action: "open_short", Symbol: "BTCUSDT", ClientOrderID: "new-id"})
	if err != nil || !claimed {
		t.Fatalf("reclaimed=%+v claimed=%v err=%v", reclaimed, claimed, err)
	}
	if reclaimed.ClientOrderID != "stable-id" {
		t.Fatalf("client id changed across retry: %q", reclaimed.ClientOrderID)
	}
	if err := cs.UpdateExecutionIntent(reclaimed.ID, ExecutionIntentFailed, "REAL_FAILURE", "exchange rejected", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	counted, err := cs.MarkExecutionIntentFailureCounted(reclaimed.ID)
	if err != nil || !counted {
		t.Fatalf("first failure count claim=%v err=%v", counted, err)
	}
	counted, err = cs.MarkExecutionIntentFailureCounted(reclaimed.ID)
	if err != nil || counted {
		t.Fatalf("duplicate failure count claim=%v err=%v", counted, err)
	}
}
