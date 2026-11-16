package store

import (
	"errors"
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

// Production shape (trader 788e3e43, intent 4664223): a catch-up receipt said
// FILLED without a quantity after the mapping had moved on, so the reconciler
// parked the entry forever and the leader exit waited on it.
func TestHistoricalEntryEvidenceRepairBooksWithoutTouchingSource(t *testing.T) {
	s := resolutionStore(t)
	resolutionMapping(t, s, "t", "p", MappingStatusStoppedByRisk, 48, 5000)
	i := resolutionIntent(t, s, "t", "p", "open_long", ExecutionIntentReconciling, 40, 3885)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET filled_quantity=0.041,quantized_quantity=0.042,target_quantity=0.042,submitted_at=CURRENT_TIMESTAMP,reason_code='HISTORICAL_FILL_EVIDENCE_PENDING' WHERE id=?`, i.ID)
	resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,exchange_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,1,'ct-1','o-1','FILLED','FILLED',0.041,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID)
	resolutionExec(t, s, `INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional) VALUES(?,'ct-1',0.041,75)`, i.ID)
	resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,exchange_order_id,status,exchange_state,filled_quantity,submitted_at) VALUES(?,2,'ct-2','o-2','SUBMITTED','FILLED',0,CURRENT_TIMESTAMP)`, i.ID)

	gaps, err := s.CopyTrade().ListHistoricalEntryEvidenceGaps("t", 0)
	if err != nil || len(gaps) != 1 || gaps[0].ID != i.ID {
		t.Fatalf("gap not listed: %+v %v", gaps, err)
	}
	if err = s.CopyTrade().BookRecoveredHistoricalEntry(i.ID); !errors.Is(err, ErrSourceTransitionUnresolved) {
		t.Fatalf("unproven receipt must stay pending, got %v", err)
	}
	if err = s.CopyTrade().CompleteExecutionOrderAttemptWithEvidence(i.ID, "ct-2", ExecutionOrderAttemptFilled, "o-2", "FILLED", "", 0.001, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.CopyTrade().BookRecoveredHistoricalEntry(i.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.CopyTrade().GetExecutionIntentByID(i.ID)
	if err != nil || got.Status != ExecutionIntentFilled || got.TerminalAt == nil || !sameSourceQuantity(got.FilledQuantity, 0.042) {
		t.Fatalf("intent not finished: %+v %v", got, err)
	}
	m, _ := s.CopyTrade().GetMapping("t", "p")
	if m.SourceRevision != 48 || m.Status != MappingStatusStoppedByRisk {
		t.Fatalf("repair rewound the advanced source: %+v", m)
	}
	if gaps, err = s.CopyTrade().ListHistoricalEntryEvidenceGaps("t", 0); err != nil || len(gaps) != 0 {
		t.Fatalf("gap still listed: %+v %v", gaps, err)
	}
	var pending int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts a WHERE a.intent_id=? AND `+unsettledExecutionAttemptSQL("a"), i.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("exit barrier still sees %d unsettled receipts (%v)", pending, err)
	}
}

func TestHistoricalEntryEvidenceRepairIgnoresCurrentRevision(t *testing.T) {
	s := resolutionStore(t)
	resolutionMapping(t, s, "t", "p", "active", 40, 10)
	i := resolutionIntent(t, s, "t", "p", "open_long", ExecutionIntentReconciling, 40, 10)
	resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,exchange_order_id,status,exchange_state,filled_quantity,submitted_at) VALUES(?,1,'ct-1','o-1','SUBMITTED','FILLED',0,CURRENT_TIMESTAMP)`, i.ID)
	gaps, err := s.CopyTrade().ListHistoricalEntryEvidenceGaps("t", 0)
	if err != nil || len(gaps) != 0 {
		t.Fatalf("current revision belongs to normal reconciliation: %+v %v", gaps, err)
	}
	if err = s.CopyTrade().BookRecoveredHistoricalEntry(i.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.CopyTrade().GetExecutionIntentByID(i.ID)
	if got.Status != ExecutionIntentReconciling || got.TerminalAt != nil {
		t.Fatalf("current-revision intent was finished: %+v", got)
	}
}
