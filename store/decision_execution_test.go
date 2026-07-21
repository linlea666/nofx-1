package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDecisionReadReflectsLatestExecutionIntentTerminalState(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "decision-terminal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	intent, claimed, err := st.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, Action: "open_short", Symbol: "BTCUSDT", ClientOrderID: "stable",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	record := &DecisionRecord{TraderID: "t1", CycleNumber: 1, Timestamp: time.Now(), Success: true, Decisions: []DecisionAction{{Action: "open_short", Symbol: "BTCUSDT", Success: true, ExecutionIntentID: intent.ID, ExecutionStatus: ExecutionIntentReserved}}}
	if err = st.Decision().LogDecision(record); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().UpdateExecutionIntent(intent.ID, ExecutionIntentFailed, "GUARD_UNPROTECTABLE_EXITED", "opened then safely exited", "order-1", 0.37, 0.37, 0.37); err != nil {
		t.Fatal(err)
	}
	records, err := st.Decision().GetLatestRecords("t1", 1)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
	got := records[0]
	if got.Success || len(got.Decisions) != 1 || got.Decisions[0].Success || got.Decisions[0].ExecutionStatus != ExecutionIntentFailed || got.Decisions[0].ExecutionReasonCode != "GUARD_UNPROTECTABLE_EXITED" {
		t.Fatalf("dashboard retained stale success: %+v", got)
	}
}
