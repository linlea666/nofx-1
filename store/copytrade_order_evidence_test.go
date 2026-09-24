package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestExecutionReceiptNeedsFillQuantityAndMaintainsEvidence(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "order-evidence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := s.CopyTrade()
	i, _, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 1, Action: "close_long", Symbol: "ETHUSDT", Side: "long"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cs.PrepareExecutionOrderAttemptRecordWithKind(i.ID, "client", "LEADER_EXIT", 5, 5); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.MarkExecutionOrderAttemptSubmitted(i.ID, "client"); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(i.ID, "client", ExecutionOrderAttemptSubmitted, "order", "FILLED", "", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := cs.ListExecutionOrderAttempts(i.ID)
	if a[0].TerminalAt != nil || !ExecutionOrderAttemptNeedsReconciliation(a[0]) {
		t.Fatal("ACK was interpreted as filled")
	}
	// Recreate the old corruption, including a completed parent intent.
	if _, err = s.DB().Exec(`UPDATE copy_trade_execution_order_attempts SET terminal_at=CURRENT_TIMESTAMP WHERE intent_id=?`, i.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`UPDATE copy_trade_execution_intents SET status='FILLED',terminal_at=CURRENT_TIMESTAMP WHERE id=?`, i.ID); err != nil {
		t.Fatal(err)
	}
	blockers, err := s.Trader().GetStopBlockers("t")
	if err != nil || len(blockers) != 1 {
		t.Fatalf("legacy corrupt receipt bypassed lifecycle fence: %+v %v", blockers, err)
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		if err = cs.CompleteExecutionOrderAttempt(i.ID, "client", ExecutionOrderAttemptFilled, "order", "FILLED", "", bad); err == nil {
			t.Fatal("invalid quantity accepted")
		}
	}
	fillTime := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	if err = cs.CompleteExecutionOrderAttemptWithEvidence(i.ID, "client", ExecutionOrderAttemptFilled, "other", "FILLED", "", 5, fillTime); err == nil {
		t.Fatal("changed exchange identity accepted")
	}
	if err = cs.CompleteExecutionOrderAttemptWithEvidence(i.ID, "client", ExecutionOrderAttemptFilled, "order", "FILLED", "", 5, fillTime); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(i.ID, "client", ExecutionOrderAttemptSubmitted, "order", "NEW", "late ACK", 0); err != nil {
		t.Fatal(err)
	}
	a, _ = cs.ListExecutionOrderAttempts(i.ID)
	if a[0].FilledQuantity != 5 || a[0].TerminalAt == nil || a[0].FilledAt == nil || !a[0].FilledAt.Equal(fillTime) || ExecutionOrderAttemptNeedsReconciliation(a[0]) {
		t.Fatalf("confirmed evidence overwritten: %+v", a[0])
	}
	var audits int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_order_evidence_repairs WHERE intent_id=?`, i.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("missing repair audit: %d %v", audits, err)
	}
	blockers, err = s.Trader().GetStopBlockers("t")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("verified receipt still blocked: %+v %v", blockers, err)
	}
}

func TestUnsettledAttemptSQLMatchesReceiptProof(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "receipt-proof.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := s.CopyTrade()
	i, _, _ := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", Side: "long"})
	_, err = cs.PrepareExecutionOrderAttemptRecordWithKind(i.ID, "client", "ENTRY", 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status, state string
		qty           float64
	}{{"SUBMITTED", "FILLED", 0}, {"FILLED", "FILLED", 5}, {"FILLED", "CANCELED", 2}, {"TERMINAL_NO_FILL", "CANCELED", 0}, {"SUBMITTED", "NEW", 0}, {"UNKNOWN", "", 0}, {"FAILED", "", 0}, {"FILLED", "CANCELED", 0}, {"UNKNOWN", "FILLED", 2}} {
		if _, err = s.DB().Exec(`UPDATE copy_trade_execution_order_attempts SET submitted_at=CURRENT_TIMESTAMP,terminal_at=CURRENT_TIMESTAMP,status=?,exchange_state=?,filled_quantity=?`, tc.status, tc.state, tc.qty); err != nil {
			t.Fatal(err)
		}
		a, _ := cs.ListExecutionOrderAttempts(i.ID)
		pending, err := cs.HasUnsettledScopeEntries("t", "ETHUSDT", "long")
		if err != nil || pending != ExecutionOrderAttemptNeedsReconciliation(a[0]) {
			t.Fatalf("receipt fence disagreement %+v pending=%v err=%v", tc, pending, err)
		}
	}
}
