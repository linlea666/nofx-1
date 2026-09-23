package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func resolutionStore(t *testing.T) *Store {
	t.Helper()
	s, e := New(filepath.Join(t.TempDir(), "source-resolution.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func resolutionIntent(t *testing.T, s *Store, trader, pos, action, status string, rev int64, target float64) *CopyTradeExecutionIntent {
	t.Helper()
	i, _, e := s.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: trader, LeaderPosID: pos, SourceRevision: rev, SourceKind: "LEADER_TRANSITION", CanonicalKey: fmt.Sprintf("leader|%s|%s|%d", trader, pos, rev), Action: action, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: target, SourceFillID: fmt.Sprintf("f-%s-%d", pos, rev), ClientOrderID: fmt.Sprintf("client-%s-%d", pos, rev)})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB().Exec(`UPDATE copy_trade_execution_intents SET status=? WHERE id=?`, status, i.ID); e != nil {
		t.Fatal(e)
	}
	return i
}
func resolutionMapping(t *testing.T, s *Store, trader, pos, status string, rev int64, size float64) {
	t.Helper()
	e := s.CopyTrade().SavePositionMapping(&CopyTradePositionMapping{TraderID: trader, LeaderID: "leader", LeaderPosID: pos, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LastKnownSize: size, OpenedAt: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB().Exec(`UPDATE copy_trade_position_mappings SET source_revision=?,status=? WHERE trader_id=? AND leader_pos_id=?`, rev, status, trader, pos); e != nil {
		t.Fatal(e)
	}
}
func resolutionExec(t *testing.T, s *Store, q string, args ...interface{}) {
	t.Helper()
	if _, e := s.DB().Exec(q, args...); e != nil {
		t.Fatal(e)
	}
}

func TestSourceFactsTerminalDisplayNeverProvesExchangeOutcome(t *testing.T) {
	for _, status := range []string{ExecutionIntentSkipped, ExecutionIntentFailed, ExecutionIntentFilled, ExecutionIntentProtected, ExecutionIntentCompletedPartial} {
		t.Run(status, func(t *testing.T) {
			s := resolutionStore(t)
			i := resolutionIntent(t, s, "t", "p", "open_long", status, 2, 20)
			resolutionMapping(t, s, "t", "p", "active", 1, 10)
			resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET submitted_at=CURRENT_TIMESTAMP,exchange_order_id='unknown' WHERE id=?`, i.ID)
			f, e := s.CopyTrade().InspectLeaderTransition(i.ID)
			if e != nil || f.Effect != SourceEffectUnresolved {
				t.Fatalf("false zero-fill proof: %+v %v", f, e)
			}
			e = s.CopyTrade().FinishLeaderSourceTransition(FinishLeaderSourceTransitionRequest{IntentID: i.ID, TraderID: "t", Disposition: SourceConsumeNoFill, Reason: "test"})
			if !errors.Is(e, ErrSourceTransitionUnresolved) {
				t.Fatalf("consumed unknown exchange instruction: %v", e)
			}
			m, _ := s.CopyTrade().GetMapping("t", "p")
			if m.SourceRevision != 1 {
				t.Fatalf("advanced unknown: %+v", m)
			}
		})
	}
}

func TestReleasedAddIncidentsAcknowledgeOriginalActionAndBaselineWithoutReplay(t *testing.T) {
	for _, c := range []struct {
		name                 string
		rev                  int64
		old, target, current float64
	}{{"huachuang", 9, 598, 599, 500}, {"muzi", 2, 10, 25, 50}} {
		t.Run(c.name, func(t *testing.T) {
			s := resolutionStore(t)
			resolutionMapping(t, s, "t", c.name, "active", c.rev, c.old)
			i := resolutionIntent(t, s, "t", c.name, "open_long", ExecutionIntentSkipped, c.rev+1, c.target)
			resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET reason_code='POSITION_CUSTODY_RELEASED' WHERE id=?`, i.ID)
			resolutionExec(t, s, `INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,symbol,side,state) VALUES('t',?,?,'ETHUSDT','long','RELEASED')`, c.name, i.ID)
			if n, e := s.CopyTrade().RepairReleasedSourceBlockers("t"); e != nil || n != 1 {
				t.Fatalf("repair %d %v", n, e)
			}
			m, _ := s.CopyTrade().GetMapping("t", c.name)
			if m.SourceRevision != c.rev+1 || m.Status != MappingStatusDetached {
				t.Fatalf("bad source ack: %+v", m)
			}
			var action string
			var orders int
			resolutionQuery := s.DB().QueryRow(`SELECT action,(SELECT COUNT(*) FROM copy_trade_execution_order_attempts WHERE intent_id=i.id) FROM copy_trade_execution_intents i WHERE id=?`, i.ID)
			if e := resolutionQuery.Scan(&action, &orders); e != nil {
				t.Fatal(e)
			}
			if action != "open_long" || orders != 0 {
				t.Fatal("historical add rewritten or traded")
			}
			if e := s.CopyTrade().BaselineRepairedSource("t", map[string]*CopyTradePositionMapping{c.name: {LeaderID: "leader", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LastKnownSize: c.current}}, map[string]int64{c.name: 3000}, false); e != nil {
				t.Fatal(e)
			}
			m, _ = s.CopyTrade().GetMapping("t", c.name)
			if m.Status != MappingStatusIgnored || m.LastKnownSize != c.current {
				t.Fatalf("missed live source was chased: %+v", m)
			}
			if n, e := s.CopyTrade().RepairReleasedSourceBlockers("t"); e != nil || n != 0 {
				t.Fatalf("not idempotent %d %v", n, e)
			}
		})
	}
}

func TestReleasedRepairRejectsUnknownPeerOrder(t *testing.T) {
	s := resolutionStore(t)
	resolutionMapping(t, s, "t", "p", "active", 2, 10)
	i := resolutionIntent(t, s, "t", "p", "open_long", ExecutionIntentSkipped, 3, 25)
	resolutionExec(t, s, `INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,symbol,side,state) VALUES('t','p',?,'ETHUSDT','long','RELEASED')`, i.ID)
	old := resolutionIntent(t, s, "t", "p", "open_long", ExecutionIntentReconciling, 1, 10)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET submitted_at=CURRENT_TIMESTAMP WHERE id=?`, old.ID)
	if n, e := s.CopyTrade().RepairReleasedSourceBlockers("t"); e != nil || n != 0 {
		t.Fatalf("unknown peer bypassed %d %v", n, e)
	}
}

func TestOldAcknowledgedFillRequiresEachTerminalOrderCommit(t *testing.T) {
	for _, bad := range []string{"", "missing_commit", "wrong_order", "unterminated", "overstated_intent"} {
		t.Run(bad, func(t *testing.T) {
			s := resolutionStore(t)
			resolutionMapping(t, s, "t", "p", "closed", 20, 0)
			i := resolutionIntent(t, s, "t", "p", "open_long", ExecutionIntentReconciling, 3, 100)
			resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET filled_quantity=1.052,quantized_quantity=1.052,submitted_at=CURRENT_TIMESTAMP,reason_code='REVISION_CONFLICT' WHERE id=?`, i.ID)
			for n, q := range []float64{1.051, .001} {
				key := fmt.Sprintf("ct-proof-%d", n)
				resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,?,?,'FILLED','FILLED',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID, n+1, key, q)
				if bad != "missing_commit" || n == 0 {
					resolutionExec(t, s, `INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional) VALUES(?,?,?,?)`, i.ID, key, q, q*2000)
				}
			}
			switch bad {
			case "wrong_order":
				resolutionExec(t, s, `UPDATE copy_trade_execution_fill_commits SET fill_key='alien' WHERE intent_id=? AND fill_key='ct-proof-1'`, i.ID)
			case "unterminated":
				resolutionExec(t, s, `UPDATE copy_trade_execution_order_attempts SET terminal_at=NULL WHERE intent_id=? AND attempt_no=2`, i.ID)
			case "overstated_intent":
				resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET filled_quantity=1.053 WHERE id=?`, i.ID)
			}
			got, e := s.CopyTrade().ResolveAcknowledgedLeaderIntent(i.ID)
			if e != nil || got != (bad == "") {
				t.Fatalf("booked proof %q resolved=%v err=%v", bad, got, e)
			}
			var rev int64
			var state string
			if e = s.DB().QueryRow(`SELECT source_revision,status FROM copy_trade_position_mappings WHERE trader_id='t' AND leader_pos_id='p'`).Scan(&rev, &state); e != nil {
				t.Fatal(e)
			}
			if rev != 20 || state != "closed" {
				t.Fatalf("rewound later source %d %s", rev, state)
			}
			if bad == "" {
				again, e := s.CopyTrade().ResolveAcknowledgedLeaderIntent(i.ID)
				if !again || e != nil {
					t.Fatalf("repeat ack %v %v", again, e)
				}
			}
		})
	}
}

func TestSourceFinisherCASAndTransactionRollback(t *testing.T) {
	s := resolutionStore(t)
	resolutionMapping(t, s, "t", "p", "active", 1, 10)
	i := resolutionIntent(t, s, "t", "p", "open_long", ExecutionIntentSkipped, 2, 20)
	f, e := s.CopyTrade().InspectLeaderTransition(i.ID)
	if e != nil {
		t.Fatal(e)
	}
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET reason_code='new-proof' WHERE id=?`, i.ID)
	req := FinishLeaderSourceTransitionRequest{IntentID: i.ID, TraderID: "t", ExpectedFingerprint: f.Fingerprint, Disposition: SourceConsumeNoFill, Reason: "RISK_CAP"}
	if e = s.CopyTrade().FinishLeaderSourceTransition(req); !errors.Is(e, ErrSourceTransitionChanged) {
		t.Fatalf("stale CAS accepted: %v", e)
	}
	req.ExpectedFingerprint = ""
	resolutionExec(t, s, `CREATE TRIGGER reject_source_ack BEFORE UPDATE ON copy_trade_source_transitions BEGIN SELECT RAISE(ABORT,'test source failure'); END`)
	if e = s.CopyTrade().FinishLeaderSourceTransition(req); e == nil {
		t.Fatal("source failure ignored")
	}
	m, _ := s.CopyTrade().GetMapping("t", "p")
	if m.SourceRevision != 1 || m.LastKnownSize != 10 {
		t.Fatalf("partial transaction: %+v", m)
	}
	resolutionExec(t, s, `DROP TRIGGER reject_source_ack`)
	if e = s.CopyTrade().FinishLeaderSourceTransition(req); e != nil {
		t.Fatal(e)
	}
	m, _ = s.CopyTrade().GetMapping("t", "p")
	if m.SourceRevision != 2 || m.LastKnownSize != 20 {
		t.Fatalf("missing atomic progress %+v", m)
	}
	if _, claimed, e := s.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 2, CanonicalKey: i.CanonicalKey, Action: "open_long", SourceFillID: "later", LeaderTargetSize: 30}); e != nil || claimed {
		t.Fatalf("resolved identity reused: %v %v", claimed, e)
	}
}

func TestZeroFillCloseRequiresFreshExitProof(t *testing.T) {
	s := resolutionStore(t)
	resolutionMapping(t, s, "t", "p", "active", 1, 10)
	i := resolutionIntent(t, s, "t", "p", "close_long", ExecutionIntentFailed, 2, 0)
	if e := s.CopyTrade().FinishLeaderSourceTransition(FinishLeaderSourceTransitionRequest{IntentID: i.ID, TraderID: "t", Disposition: SourceConsumeNoFill, Reason: "EXCHANGE_TERMINAL_NO_FILL"}); e == nil {
		t.Fatal("zero-fill close consumed while follower may remain")
	}
}

func TestSourceRecoveryStopGenerationDoesNotSwallowCrashReduction(t *testing.T) {
	s := resolutionStore(t)
	createLifecycleTestTrader(t, s, "t", TraderLifecycleRunning, 1)
	resolutionMapping(t, s, "t", "p", "active", 1, 10)
	positions := []CopyTradeBaselinePosition{{LeaderPosID: "p", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Size: 5}, {LeaderPosID: "new", Symbol: "BTCUSDT", Side: "long", Size: 2}}
	if applied, e := s.CopyTrade().ApplyStoppedSourceRecoveryBaseline("t", "leader", positions); e != nil || applied {
		t.Fatalf("crash restart baselined %v %v", applied, e)
	}
	resolutionExec(t, s, `INSERT INTO trader_lifecycle_events(trader_id,generation,from_status,to_status,reason_code) VALUES('t',1,'STOPPED','STARTING','OPERATOR_START')`)
	if applied, e := s.CopyTrade().ApplyStoppedSourceRecoveryBaseline("t", "leader", positions); e != nil || !applied {
		t.Fatalf("stop recovery %v %v", applied, e)
	}
	m, _ := s.CopyTrade().GetMapping("t", "p")
	if m.LastKnownSize != 10 {
		t.Fatal("owed reduction swallowed")
	}
	m, _ = s.CopyTrade().GetMapping("t", "new")
	if m == nil || m.Status != MappingStatusIgnored {
		t.Fatal("stopped gap source chased")
	}
	positions[0].Size = 20
	if applied, e := s.CopyTrade().ApplyStoppedSourceRecoveryBaseline("t", "leader", positions); e != nil || applied {
		t.Fatalf("same generation baselined twice %v %v", applied, e)
	}
}

func TestStopConsumesPreparedAddButPreservesSubmittedUnknown(t *testing.T) {
	s := resolutionStore(t)
	createLifecycleTestTrader(t, s, "t", TraderLifecycleRunning, 1)
	resolutionMapping(t, s, "t", "local", "active", 1, 10)
	resolutionMapping(t, s, "t", "unknown", "active", 1, 10)
	local := resolutionIntent(t, s, "t", "local", "open_long", ExecutionIntentReserved, 2, 20)
	unknown := resolutionIntent(t, s, "t", "unknown", "open_long", ExecutionIntentSubmitted, 2, 20)
	for _, i := range []*CopyTradeExecutionIntent{local, unknown} {
		resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status) VALUES(?,1,?,'PREPARED')`, i.ID, i.ClientOrderID)
	}
	resolutionExec(t, s, `UPDATE copy_trade_execution_order_attempts SET status='SUBMITTED',submitted_at=CURRENT_TIMESTAMP WHERE intent_id=?`, unknown.ID)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET submitted_at=CURRENT_TIMESTAMP WHERE id=?`, unknown.ID)
	if _, e := s.Trader().BeginStop("user-1", "t"); e != nil {
		t.Fatal(e)
	}
	f, e := s.CopyTrade().InspectLeaderTransition(local.ID)
	if e != nil || f.MappingRevision != 2 || f.Status != ExecutionIntentSkipped || f.Resolution != SourceConsumeNoFill {
		t.Fatalf("prepared stop not atomic: %+v %v", f, e)
	}
	f, e = s.CopyTrade().InspectLeaderTransition(unknown.ID)
	if e != nil || f.MappingRevision != 1 || f.Effect != SourceEffectUnresolved {
		t.Fatalf("unknown order erased: %+v %v", f, e)
	}
	if _, e = s.CopyTrade().MarkExecutionOrderAttemptSubmitted(local.ID, local.ClientOrderID); e == nil {
		t.Fatal("retired local order later submitted")
	}
}

func TestRunningAccountClaimRejectsOldUncertainInstructionsAndAccountChange(t *testing.T) {
	s := resolutionStore(t)
	createLifecycleTestTrader(t, s, "old", TraderLifecycleRunning, 1)
	if e := s.Trader().ClaimRunningCopyAccount("old", 1, "exchange-1"); e != nil {
		t.Fatal(e)
	}
	resolutionExec(t, s, `UPDATE traders SET lifecycle_status='STOPPED',is_running=0 WHERE id='old'`)
	i := resolutionIntent(t, s, "old", "p", "open_long", ExecutionIntentSkipped, 1, 10)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET submitted_at=CURRENT_TIMESTAMP WHERE id=?`, i.ID)
	createLifecycleTestTrader(t, s, "new", TraderLifecycleRunning, 1)
	if e := s.Trader().ClaimRunningExecutionAccount("new", 1, "exchange-1", false); e == nil {
		t.Fatal("automatic plain restart bypassed unknown copy order")
	}
	resolutionExec(t, s, `UPDATE traders SET exchange_id='exchange-2',lifecycle_status='RUNNING',is_running=1 WHERE id='old'`)
	if e := s.Trader().ClaimRunningCopyAccount("old", 1, "exchange-2"); e == nil {
		t.Fatal("account change abandoned legacy unknown order")
	}
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET terminal_at=CURRENT_TIMESTAMP,exchange_state='CANCELED' WHERE id=?`, i.ID)
	if e := s.Trader().ClaimRunningCopyAccount("old", 1, "exchange-2"); e != nil {
		t.Fatalf("settled previous account not released: %v", e)
	}
	if e := s.Trader().ClaimRunningCopyAccount("old", 0, "exchange-2"); !errors.Is(e, ErrTraderLifecycleConflict) {
		t.Fatalf("old generation claimed account: %v", e)
	}
}

func TestAcknowledgedPartialPreservesCatchupUntilCustodyReleased(t *testing.T) {
	s := resolutionStore(t)
	resolutionMapping(t, s, "t", "p", "active", 3, 100)
	i := resolutionIntent(t, s, "t", "p", "open_long", ExecutionIntentPartiallyFilled, 3, 100)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET target_quantity=2,filled_quantity=1,submitted_at=CURRENT_TIMESTAMP WHERE id=?`, i.ID)
	resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,1,'ct-live','FILLED','FILLED',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID)
	resolutionExec(t, s, `INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional) VALUES(?,'ct-live',1,2000)`, i.ID)
	if ok, e := s.CopyTrade().ResolveAcknowledgedLeaderIntent(i.ID); e != nil || ok {
		t.Fatalf("normal catchup prematurely ended: %v %v", ok, e)
	}
	resolutionExec(t, s, `INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,symbol,side,state) VALUES('t','p',?,'ETHUSDT','long','RELEASED')`, i.ID)
	if ok, e := s.CopyTrade().ResolveAcknowledgedLeaderIntent(i.ID); e != nil || !ok {
		t.Fatalf("released follower kept stale catchup: %v %v", ok, e)
	}
	f, e := s.CopyTrade().InspectLeaderTransition(i.ID)
	if e != nil || f.Status != ExecutionIntentCompletedPartial {
		t.Fatalf("partial outcome lost: %+v %v", f, e)
	}
}

func TestTerminalZeroFillUsesOrderProofAndAdvancesOnlyOriginalSource(t *testing.T) {
	s := resolutionStore(t)
	resolutionMapping(t, s, "t", "p", "active", 1, 10)
	i := resolutionIntent(t, s, "t", "p", "open_long", ExecutionIntentFailed, 2, 20)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET submitted_at=CURRENT_TIMESTAMP WHERE id=?`, i.ID)
	resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,exchange_state,submitted_at,terminal_at) VALUES(?,1,'rejected','TERMINAL_NO_FILL','REJECTED',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID)
	f, e := s.CopyTrade().InspectLeaderTransition(i.ID)
	if e != nil || f.Effect != SourceEffectZeroFill {
		t.Fatalf("no-fill proof %+v %v", f, e)
	}
	if e = s.CopyTrade().FinishLeaderSourceTransition(FinishLeaderSourceTransitionRequest{IntentID: i.ID, TraderID: "t", Disposition: SourceConsumeNoFill, Reason: "EXCHANGE_TERMINAL_NO_FILL"}); e != nil {
		t.Fatal(e)
	}
	m, _ := s.CopyTrade().GetMapping("t", "p")
	if m.SourceRevision != 2 || m.LastKnownSize != 20 {
		t.Fatalf("zero-fill source not consumed: %+v", m)
	}
}

func TestReleasedRepairPreservesOnlyConfirmedSameLifecycleFutureExits(t *testing.T) {
	for _, proof := range []string{"legacy_lazy", "confirmed_same", "confirmed_other"} {
		t.Run(proof, func(t *testing.T) {
			s := resolutionStore(t)
			resolutionMapping(t, s, "t", "p", "active", 1, 10)
			i := resolutionIntent(t, s, "t", "p", "open_long", ExecutionIntentSkipped, 2, 20)
			resolutionExec(t, s, `INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,symbol,side,state) VALUES('t','p',?,'ETHUSDT','long','RELEASED')`, i.ID)
			resolutionExec(t, s, `INSERT INTO copy_trade_source_lifecycles(trader_id,leader_pos_id,opened_ms) VALUES('t','p',1000)`)
			if proof != "legacy_lazy" {
				opened := 1000
				if proof == "confirmed_other" {
					opened = 500
				}
				resolutionExec(t, s, `INSERT INTO copy_trade_confirmed_source_lifecycles(trader_id,leader_pos_id,opened_ms,intent_id) VALUES('t','p',?,?)`, opened, i.ID)
			}
			if _, e := s.CopyTrade().RepairReleasedSourceBlockers("t"); e != nil {
				t.Fatal(e)
			}
			if e := s.CopyTrade().BaselineRepairedSource("t", map[string]*CopyTradePositionMapping{"p": {LeaderID: "leader", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 15}}, map[string]int64{"p": 1000}, false); e != nil {
				t.Fatal(e)
			}
			m, _ := s.CopyTrade().GetMapping("t", "p")
			want := MappingStatusIgnored
			if proof == "confirmed_same" {
				want = MappingStatusDetached
			}
			if m.Status != want || m.LastKnownSize != 15 {
				t.Fatalf("continuity/no-chase baseline wrong: %+v", m)
			}
		})
	}
}

func TestAccountChangeCannotMoveManagedCustodyWithoutOrders(t *testing.T) {
	s := resolutionStore(t)
	createLifecycleTestTrader(t, s, "t", TraderLifecycleRunning, 1)
	if e := s.Trader().ClaimRunningCopyAccount("t", 1, "exchange-1"); e != nil {
		t.Fatal(e)
	}
	resolutionExec(t, s, `INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,symbol,side,state) VALUES('t','p',1,'ETHUSDT','long','MANAGED')`)
	resolutionExec(t, s, `UPDATE traders SET exchange_id='exchange-2' WHERE id='t'`)
	if e := s.Trader().ClaimRunningCopyAccount("t", 1, "exchange-2"); e == nil {
		t.Fatal("moved managed position to different account")
	}
	resolutionExec(t, s, `UPDATE copy_trade_position_custody SET state='RELEASED' WHERE trader_id='t'`)
	if e := s.Trader().ClaimRunningCopyAccount("t", 1, "exchange-2"); e != nil {
		t.Fatalf("released ownership still locks account: %v", e)
	}
}

func TestSharedGuardClosedPrimaryDoesNotRebuildOwnershipWithLivePeer(t *testing.T) {
	s := resolutionStore(t)
	cs := s.CopyTrade()
	resolutionMapping(t, s, "t", "primary", "closed", 3, 0)
	resolutionMapping(t, s, "t", "peer", "manual_stopped", 2, 10)
	policy, e := EncodeCopyGuardPolicySnapshot(NewCopyGuardDefaults())
	if e != nil {
		t.Fatal(e)
	}
	cycle, e := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "t", LeaderID: "leader", LeaderPosID: "primary", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: policy, ProtectionStatus: CopyGuardProtectionVerified})
	if e != nil {
		t.Fatal(e)
	}
	for _, pos := range []string{"primary", "peer"} {
		if _, e = cs.EnsureFollowGroupMember("t", "okx", "leader", "ETHUSDT", "long", pos, 1000, MappingStatusActive); e != nil {
			t.Fatal(e)
		}
	}
	if e = cs.BindFollowGroupGuard(cycle.ID); e != nil {
		t.Fatal(e)
	}
	if gaps, e := cs.ListOpenCycleOwnershipGaps("t"); e != nil || len(gaps) != 0 {
		t.Fatalf("shared protection ownership falsely missing: %+v %v", gaps, e)
	}
	report, e := cs.GetExecutionReconciliationReport("t")
	if e != nil || report.OpenCyclesWithoutMapping != 0 || report.AmbiguousOwnershipGaps != 0 {
		t.Fatalf("shared protection health wrong: %+v %v", report, e)
	}
	resolutionExec(t, s, `UPDATE copy_trade_position_mappings SET status='closed' WHERE trader_id='t' AND leader_pos_id='peer'`)
	if gaps, e := cs.ListOpenCycleOwnershipGaps("t"); e != nil || len(gaps) != 1 {
		t.Fatalf("group existence hid real ownership gap: %+v %v", gaps, e)
	}
}

func TestStartingClaimsFenceConcurrentAccountOwners(t *testing.T) {
	s := resolutionStore(t)
	for _, id := range []string{"first", "second"} {
		createLifecycleTestTrader(t, s, id, TraderLifecycleStarting, 2)
	}
	if e := s.Trader().ClaimRunningCopyAccount("first", 2, "exchange-1"); e != nil {
		t.Fatal(e)
	}
	if e := s.Trader().ClaimRunningCopyAccount("second", 2, "exchange-1"); e == nil {
		t.Fatal("second STARTING runtime acquired same account")
	}
	if e := s.Trader().ClaimRunningExecutionAccount("second", 2, "exchange-1", false); e == nil {
		t.Fatal("plain STARTING runtime bypassed durable exclusive claim")
	}
	if e := s.Trader().FailStart("user-1", "first", 2, "test startup failure"); e != nil {
		t.Fatal(e)
	}
	if e := s.Trader().ClaimRunningCopyAccount("second", 2, "exchange-1"); e != nil {
		t.Fatalf("failed startup did not release claim: %v", e)
	}
}

func TestPausedExitBooksRealFillWithoutChangingSourceBaseline(t *testing.T) {
	for _, quantity := range []float64{0, 1, 2} {
		t.Run(fmt.Sprint(quantity), func(t *testing.T) {
			s := resolutionStore(t)
			cs := s.CopyTrade()
			resolutionMapping(t, s, "t", "p", "active", 1, 10)
			i := resolutionIntent(t, s, "t", "p", "reduce_long", ExecutionIntentReserved, 2, 5)
			if _, e := cs.PrepareLeaderExit(LeaderExitPlan{IntentID: i.ID, TraderID: "t", LeaderPosID: "p", Symbol: "ETHUSDT", Side: "long", Ratio: .5, Targets: []LeaderExitTarget{{Key: "cross", MarginMode: "cross", Quantity: 2}}}); e != nil {
				t.Fatal(e)
			}
			state := "CANCELED"
			if quantity == 2 {
				state = "FILLED"
			}
			resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,1,'exit-receipt','FILLED',?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID, state, quantity)
			resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET status='SUBMITTED',submitted_at=CURRENT_TIMESTAMP WHERE id=?`, i.ID)
			resolutionExec(t, s, `UPDATE copy_trade_position_mappings SET status='manual_stopped' WHERE trader_id='t' AND leader_pos_id='p'`)
			resolutionExec(t, s, `INSERT INTO copy_trade_follow_controls(trader_id,leader_pos_id,version) VALUES('t','p',1)`)
			if e := cs.CancelLeaderExitAfterControlChange(i.ID); e != nil {
				t.Fatal(e)
			}
			f, e := cs.InspectLeaderTransition(i.ID)
			if e != nil {
				t.Fatal(e)
			}
			expected := ExecutionIntentSkipped
			if quantity == 1 {
				expected = ExecutionIntentCompletedPartial
			}
			if quantity == 2 {
				expected = ExecutionIntentFilled
			}
			if f.Status != expected || f.MappingStatus != MappingStatusManualStopped || f.MappingSize != 10 || f.MappingRevision != 2 || f.FilledQuantity != quantity {
				t.Fatalf("lost real fill or advanced paused target: %+v", f)
			}
			if e = cs.CancelLeaderExitAfterControlChange(i.ID); e != nil {
				t.Fatalf("repeat pause exit not idempotent %v", e)
			}
		})
	}
}

func TestOtherSourcePendingDoesNotConfuseProtectionWithExecution(t *testing.T) {
	s := resolutionStore(t)
	cs := s.CopyTrade()
	resolutionMapping(t, s, "t", "old", "closed", 20, 0)
	i := resolutionIntent(t, s, "t", "old", "open_long", ExecutionIntentReconciling, 3, 100)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET filled_quantity=1,submitted_at=CURRENT_TIMESTAMP WHERE id=?`, i.ID)
	resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,1,'old-fill','FILLED','FILLED',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID)
	resolutionExec(t, s, `INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional) VALUES(?,'old-fill',1,2000)`, i.ID)
	if pending, e := cs.OtherSourceTransitionPending("t", "ETHUSDT", "long", "new"); e != nil || pending {
		t.Fatalf("acknowledged fill blocked another source: %v %v", pending, e)
	}
	resolutionExec(t, s, `UPDATE copy_trade_execution_order_attempts SET terminal_at=NULL WHERE intent_id=?`, i.ID)
	if pending, e := cs.OtherSourceTransitionPending("t", "ETHUSDT", "long", "new"); e != nil || !pending {
		t.Fatalf("unknown old order did not fence other source: %v %v", pending, e)
	}
}
