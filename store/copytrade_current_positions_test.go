package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCurrentPositionCopyStartupRollbackPreservesPendingRequest(t *testing.T) {
	s, _ := currentCopyFixture(t)
	currentCopyStart(t, s)
	running, err := s.Trader().GetLifecycle("t")
	if err != nil {
		t.Fatal(err)
	}
	stop, err := s.Trader().BeginFailedStartStop("user-1", "t", running.Generation, "complete follower snapshot unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Trader().CompleteStop("user-1", "t", stop.Generation); err != nil {
		t.Fatal(err)
	}
	// The background reentry cleanup also uses this shared stopped-state helper.
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = pauseTraderRiskIncreaseTx(tx, "t"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r, err := s.CopyTrade().CurrentPositionCopy("t")
	if err != nil || r == nil || r.Status != "PENDING" || len(r.Tasks) != 0 {
		t.Fatalf("startup rollback consumed pending request: %+v %v", r, err)
	}
	currentCopyStart(t, s)
	if _, err = s.Trader().BeginFailedStartStop("user-1", "t", running.Generation, "late stale failure"); !errors.Is(err, ErrTraderLifecycleConflict) {
		t.Fatalf("stale startup failure stopped a newer runtime: %v", err)
	}
	if err = s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{currentCopyCandidate("p")}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err = s.CopyTrade().CurrentPositionCopy("t")
	if err != nil || r.Status != "SEALED" || len(r.Tasks) != 1 {
		t.Fatalf("manual retry did not use the same authorization: %+v %v", r, err)
	}
	// Once a batch exists, even startup rollback must retire unsent work.
	running, _ = s.Trader().GetLifecycle("t")
	if _, err = s.Trader().BeginFailedStartStop("user-1", "t", running.Generation, "later startup failure"); err != nil {
		t.Fatal(err)
	}
	r, err = s.CopyTrade().CurrentPositionCopy("t")
	if err != nil || r.Status != "DONE" || r.Tasks[0].Status != "SKIPPED" {
		t.Fatalf("startup rollback left sealed tasks authorized: %+v %v", r, err)
	}
}

func TestCurrentPositionCopyStartupRollbackCannotUndoOperatorStop(t *testing.T) {
	s, _ := currentCopyFixture(t)
	currentCopyStart(t, s)
	running, err := s.Trader().GetLifecycle("t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Trader().BeginStop("user-1", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Trader().BeginFailedStartStop("user-1", "t", running.Generation, "late startup failure"); !errors.Is(err, ErrTraderLifecycleConflict) {
		t.Fatalf("startup rollback overwrote operator stop: %v", err)
	}
	r, err := s.CopyTrade().CurrentPositionCopy("t")
	if err != nil || r.Status != "CANCELLED" || r.Reason != "TRADER_STOPPED" {
		t.Fatalf("operator cancellation was undone: %+v %v", r, err)
	}
}

func currentCopyFixture(t *testing.T) (*Store, *CopyTradeConfig) {
	t.Helper()
	s := resolutionStore(t)
	createLifecycleTestTrader(t, s, "t", TraderLifecycleStopped, 0)
	if err := s.CopyTrade().UpdateDecisionMode("t", "copy_trade"); err != nil {
		t.Fatal(err)
	}
	on := true
	c := &CopyTradeConfig{TraderID: "t", ProviderType: "okx", LeaderID: "leader", CopyRatio: 1, SourceGeneration: 1, CopyCurrentPositionsOnce: &on, CopyCurrentPositionsRequestID: uuid.NewString()}
	if err := s.CopyTrade().Create(c); err != nil {
		t.Fatal(err)
	}
	return s, c
}
func currentCopyStart(t *testing.T, s *Store) {
	t.Helper()
	l, e := s.Trader().BeginStart("user-1", "t")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Trader().CompleteCopyGuardStart("user-1", "t", l.Generation, "exchange-1"); e != nil {
		t.Fatal(e)
	}
}
func currentCopyCandidate(id string) CurrentPositionCopyTask {
	return CurrentPositionCopyTask{LeaderPosID: id, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", OpenedMS: 1790000000000, Size: 10, EntryPrice: 110, ReferencePrice: 100, Notional: 100, Leverage: 10}
}
func currentCopyReserve(t *testing.T, s *Store, id string) *CopyTradeExecutionIntent {
	t.Helper()
	task, e := s.CopyTrade().CurrentPositionCopyTask("t", id)
	if e != nil || task == nil {
		t.Fatalf("task=%v %v", task, e)
	}
	m, e := s.CopyTrade().GetMapping("t", id)
	if e != nil {
		t.Fatal(e)
	}
	intent, claimed, e := s.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{CurrentPositionTaskID: task.ID, TraderID: "t", LeaderPosID: id, SourceRevision: m.SourceRevision + 1, SourceOpenedMS: task.OpenedMS, CanonicalKey: fmt.Sprintf("leader|t|%s|%d", id, m.SourceRevision+1), Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 10, TargetQuantity: 1, RequestedNotional: 100, ClientOrderID: "copy-" + id})
	if e != nil || !claimed {
		t.Fatalf("reserve=%v claimed=%v err=%v", intent, claimed, e)
	}
	return intent
}
func TestCurrentPositionCopyRegistrationIsAtomicAndIdempotent(t *testing.T) {
	s, c := currentCopyFixture(t)
	if err := s.CopyTrade().Update(c); err != nil {
		t.Fatal(err)
	}
	var n int
	s.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_current_position_requests`).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate requests %d", n)
	}
	c.CopyRatio = 2
	c.CopyCurrentPositionsRequestID = "invalid"
	if err := s.CopyTrade().Update(c); err == nil {
		t.Fatal("invalid request accepted")
	}
	got, err := s.CopyTrade().GetByTraderID("t")
	if err != nil || got.CopyRatio != 1 {
		t.Fatalf("partial config save %+v %v", got, err)
	}
	c.CopyCurrentPositionsOnce = nil
	c.LeaderID = "another-leader"
	if err = s.CopyTrade().Update(c); err != nil {
		t.Fatal(err)
	}
	r, err := s.CopyTrade().CurrentPositionCopy("t")
	if err != nil || r.Status != "CANCELLED" {
		t.Fatalf("scope was not cancelled: %v %v", r, err)
	}
}
func TestCurrentPositionCopySealsOnlyManualStartAndNeverExpands(t *testing.T) {
	s, _ := currentCopyFixture(t)
	p := currentCopyCandidate("p")
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{p}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, _ := s.CopyTrade().CurrentPositionCopy("t")
	if r.Status != "PENDING" {
		t.Fatal("saving created trading work")
	}
	currentCopyStart(t, s)
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{p}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{p, currentCopyCandidate("late")}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, _ = s.CopyTrade().CurrentPositionCopy("t")
	if len(r.Tasks) != 1 {
		t.Fatalf("scope expanded: %+v", r)
	}
	m, _ := s.CopyTrade().GetMapping("t", "p")
	if m.Status != MappingStatusCopyPending {
		t.Fatalf("mapping=%+v", m)
	}
	var custody int
	s.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_position_custody`).Scan(&custody)
	if custody != 0 {
		t.Fatal("fabricated ownership")
	}
	currentCopyReserve(t, s, "p")
	r, _ = s.CopyTrade().CurrentPositionCopy("t")
	if r.Tasks[0].IntentID == 0 {
		t.Fatal("intent and authorization did not commit together")
	}
}
func TestCurrentPositionCopyPartialGroupKeepsOrdinaryCustodyAndSourceBaseline(t *testing.T) {
	s, _ := currentCopyFixture(t)
	currentCopyStart(t, s)
	p, q := currentCopyCandidate("p"), currentCopyCandidate("q")
	q.MarginMode = "isolated"
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{p, q}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, member := range []CurrentPositionCopyTask{p, q} {
		if _, err := s.CopyTrade().EnsureFollowGroupMember("t", "okx", "leader", member.Symbol, member.Side, member.LeaderPosID, member.OpenedMS, MappingStatusCopyPending); err != nil {
			t.Fatal(err)
		}
	}
	i := currentCopyReserve(t, s, "p")
	if err := s.CopyTrade().CommitLeaderExecutionFill(LeaderExecutionCommit{IntentID: i.ID, TraderID: "t", LeaderID: "leader", LeaderPosID: "p", SourceRevision: i.SourceRevision, Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 10, FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: i.ClientOrderID, ExchangeOrderID: "fill-p", ExchangeState: "FILLED", OrderTerminal: true}); err != nil {
		t.Fatal(err)
	}
	j := currentCopyReserve(t, s, "q")
	if err := s.CopyTrade().CommitIgnoredLeaderTransition(IgnoredLeaderTransition{IntentID: j.ID, TraderID: "t", LeaderID: "leader", LeaderPosID: "q", SourceRevision: j.SourceRevision, Symbol: "ETHUSDT", Side: "long", MarginMode: "isolated", LeaderTargetSize: 10, ReasonCode: "MIN_NOTIONAL"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyTrade().RefreshCurrentPositionCopy("t"); err != nil {
		t.Fatal(err)
	}
	owned, err := s.CopyTrade().GetPositionCustody("t", "p")
	if err != nil || owned.State != "MANAGED" {
		t.Fatalf("ownership=%v %v", owned, err)
	}
	m, _ := s.CopyTrade().GetMapping("t", "q")
	if m.Status != MappingStatusDetached || m.LastKnownSize != 10 {
		t.Fatalf("unfilled member lost group baseline: %+v", m)
	}
	g, err := s.CopyTrade().EnsureFollowGroupMember("t", "okx", "leader", "ETHUSDT", "long", "q", q.OpenedMS, m.Status)
	if err != nil || g.Ignored || g.ConflictReason != "" {
		t.Fatalf("partial group conflicted: %+v %v", g, err)
	}
}
func TestCurrentPositionCopyStopAndUnknownReceipts(t *testing.T) {
	s, _ := currentCopyFixture(t)
	currentCopyStart(t, s)
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{currentCopyCandidate("p"), currentCopyCandidate("q")}, time.Now()); err != nil {
		t.Fatal(err)
	}
	i := currentCopyReserve(t, s, "p")
	if _, err := s.CopyTrade().PrepareExecutionOrderAttempt(i.ID, i.ClientOrderID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyTrade().CompleteExecutionOrderAttempt(i.ID, i.ClientOrderID, ExecutionOrderAttemptUnknown, "venue", "FILLED", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Trader().BeginStop("user-1", "t"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.CopyTrade().CurrentPositionCopy("t")
	if r.Status != "SEALED" || r.Tasks[1].Status != "SKIPPED" {
		t.Fatalf("stop lost unresolved receipt or kept unsubmitted work: %+v", r)
	}
	b, err := s.Trader().GetStopBlockers("t")
	if err != nil || len(b) == 0 {
		t.Fatalf("unknown receipt released: %v %v", b, err)
	}
}
func TestCurrentPositionCopyEmptyAndNoFillNeverTakeCustody(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			s, c := currentCopyFixture(t)
			currentCopyStart(t, s)
			positions := []CurrentPositionCopyTask{}
			if !empty {
				positions = append(positions, currentCopyCandidate("p"))
			}
			if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, positions, time.Now()); err != nil {
				t.Fatal(err)
			}
			if !empty {
				r, _ := s.CopyTrade().CurrentPositionCopy("t")
				if err := s.CopyTrade().SkipCurrentPositionCopyTask(r.Tasks[0].ID, "EXPIRED"); err != nil {
					t.Fatal(err)
				}
			}
			r, _ := s.CopyTrade().CurrentPositionCopy("t")
			if r.Status != "DONE" {
				t.Fatalf("not completed %+v", r)
			}
			if err := s.CopyTrade().Update(c); err != nil {
				t.Fatal(err)
			}
			r, _ = s.CopyTrade().CurrentPositionCopy("t")
			if r.Status != "DONE" {
				t.Fatal("retry rearmed completed authorization")
			}
			var n int
			s.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_position_custody`).Scan(&n)
			if n != 0 {
				t.Fatal("no-fill acquired custody")
			}
		})
	}
}

func TestCurrentPositionCopyLocalFailuresCannotFinishOrRebindBatch(t *testing.T) {
	for _, afterSubmit := range []bool{false, true} {
		t.Run(fmt.Sprint(afterSubmit), func(t *testing.T) {
			s, _ := currentCopyFixture(t)
			currentCopyStart(t, s)
			if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{currentCopyCandidate("p")}, time.Now()); err != nil {
				t.Fatal(err)
			}
			i := currentCopyReserve(t, s, "p")
			if afterSubmit {
				if _, err := s.CopyTrade().PrepareExecutionOrderAttempt(i.ID, i.ClientOrderID, 1); err != nil {
					t.Fatal(err)
				}
				if err := s.CopyTrade().CompleteExecutionOrderAttempt(i.ID, i.ClientOrderID, ExecutionOrderAttemptFilled, "venue", "FILLED", "", 1); err != nil {
					t.Fatal(err)
				}
			}
			commit := LeaderExecutionCommit{IntentID: i.ID, TraderID: "t", LeaderID: "leader", LeaderPosID: "p", SourceRevision: i.SourceRevision, Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 10, FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: i.ClientOrderID, ExchangeOrderID: "venue", ExchangeState: "FILLED", OrderTerminal: true}
			if afterSubmit {
				if _, err := s.DB().Exec(`CREATE TRIGGER copy_test_fail_fill BEFORE INSERT ON copy_trade_execution_fill_commits BEGIN SELECT RAISE(ABORT,'simulated disk failure'); END;`); err != nil {
					t.Fatal(err)
				}
				if err := s.CopyTrade().CommitLeaderExecutionFill(commit); err == nil {
					t.Fatal("fault injection did not fail")
				}
				if _, err := s.DB().Exec(`DROP TRIGGER copy_test_fail_fill`); err != nil {
					t.Fatal(err)
				}
			} else if err := s.CopyTrade().UpdateExecutionIntent(i.ID, ExecutionIntentFailed, "PRE_SUBMIT", "injected local write failure", "", 0, 0, 0); err != nil {
				t.Fatal(err)
			}
			if err := s.CopyTrade().RefreshCurrentPositionCopy("t"); err != nil {
				t.Fatal(err)
			}
			r, _ := s.CopyTrade().CurrentPositionCopy("t")
			task, err := s.CopyTrade().CurrentPositionCopyTask("t", "p")
			if err != nil || task == nil || r.Status != "SEALED" {
				t.Fatalf("terminal display state hid unfinished source: %+v %+v %v", r, task, err)
			}
			if !afterSubmit {
				retry := currentCopyReserve(t, s, "p")
				if retry.ID != i.ID || retry.ClientOrderID != i.ClientOrderID {
					t.Fatal("local failure created new order identity")
				}
				if err := s.CopyTrade().FinishLeaderSourceTransition(FinishLeaderSourceTransitionRequest{IntentID: i.ID, TraderID: "t", LeaderID: "leader", Disposition: SourceConsumeNoFill, Reason: "STOPPED"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.CopyTrade().CommitLeaderExecutionFill(LeaderExecutionCommit{IntentID: i.ID, TraderID: "t", LeaderID: "leader", LeaderPosID: "p", SourceRevision: i.SourceRevision, Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 10, FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: i.ClientOrderID, ExchangeOrderID: "venue", ExchangeState: "FILLED", OrderTerminal: true}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.CopyTrade().RefreshCurrentPositionCopy("t"); err != nil {
				t.Fatal(err)
			}
			r, _ = s.CopyTrade().CurrentPositionCopy("t")
			if r.Status != "DONE" {
				t.Fatalf("acknowledged batch stuck: %+v", r)
			}
		})
	}
}

func TestCurrentPositionCopyGroupEligibilityIsAtomic(t *testing.T) {
	s, _ := currentCopyFixture(t)
	currentCopyStart(t, s)
	p, q := currentCopyCandidate("p"), currentCopyCandidate("q")
	if err := s.CopyTrade().SaveIgnoredPosition("t", "leader", "q", "ETHUSDT", "long", "isolated"); err != nil {
		t.Fatal(err)
	}
	q.MarginMode = "isolated"
	q.Reason = "EXECUTION_INSTRUMENT_UNAVAILABLE"
	positions := []CurrentPositionCopyTask{p, q}
	if err := s.CopyTrade().ValidateCurrentCopyCandidates("t", "leader", positions); err != nil {
		t.Fatal(err)
	}
	if positions[0].Reason != "GROUP_PARTICIPATION_CONFLICT" {
		t.Fatalf("partial direction advertised executable: %+v", positions)
	}
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, positions, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, _ := s.CopyTrade().CurrentPositionCopy("t")
	if r.Status != "DONE" || r.Tasks[0].Status != "SKIPPED" {
		t.Fatalf("mixed ignored direction imported: %+v", r)
	}
}

func TestCurrentPositionCopyReplacementAndScopeCancellation(t *testing.T) {
	for _, change := range []string{"account", "mode", "leader", "off"} {
		t.Run(change, func(t *testing.T) {
			s, c := currentCopyFixture(t)
			old := c.CopyCurrentPositionsRequestID
			c.CopyCurrentPositionsRequestID = uuid.NewString()
			if err := s.CopyTrade().Update(c); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := s.DB().QueryRow(`SELECT status FROM copy_trade_current_position_requests WHERE request_id=?`, old).Scan(&state); err != nil || state != "CANCELLED" {
				t.Fatalf("old authorization remained: %s %v", state, err)
			}
			c.CopyCurrentPositionsOnce = nil
			var err error
			switch change {
			case "account":
				tr, _ := s.Trader().GetByID("t")
				tr.ExchangeID = "new-account"
				err = s.Trader().Update(tr)
			case "mode":
				err = s.CopyTrade().UpdateDecisionMode("t", "ai")
			case "leader":
				c.LeaderID = "changed"
				err = s.CopyTrade().Update(c)
			case "off":
				off := false
				c.CopyCurrentPositionsOnce = &off
				err = s.CopyTrade().Update(c)
			}
			if err != nil {
				t.Fatal(err)
			}
			r, _ := s.CopyTrade().CurrentPositionCopy("t")
			if r.Status != "CANCELLED" {
				t.Fatalf("authorization survived %s: %+v", change, r)
			}
		})
	}
}

func TestCurrentPositionCopyDecisionModeSwitchConsumesQueuedWork(t *testing.T) {
	s, _ := currentCopyFixture(t)
	currentCopyStart(t, s)
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{currentCopyCandidate("p")}, time.Now()); err != nil {
		t.Fatal(err)
	}
	i := currentCopyReserve(t, s, "p")
	if _, err := s.CopyTrade().PrepareExecutionOrderAttemptRecordWithKind(i.ID, i.ClientOrderID, "INITIAL_OPEN", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyTrade().UpdateDecisionMode("t", "ai"); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyTrade().CheckFollowSubmission(i.ID); err == nil {
		t.Fatal("mode change left queued copy executable")
	}
	r, err := s.CopyTrade().CurrentPositionCopy("t")
	if err != nil || r.Status != "DONE" {
		t.Fatalf("mode change left batch waiting: %+v %v", r, err)
	}
	facts, err := s.CopyTrade().InspectLeaderTransition(i.ID)
	if err != nil || facts.MappingRevision < facts.Revision {
		t.Fatalf("mode change stranded source revision: %+v %v", facts, err)
	}
}
