package copytrade

import (
	"fmt"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

func TestReservationAuxiliaryFailureReplaysWithoutChangingOrderIdentity(t *testing.T) {
	for _, table := range []string{"copy_trade_follow_group_intents", "copy_trade_confirmed_source_lifecycles"} {
		for _, reclaim := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reclaim=%v", table, reclaim), func(t *testing.T) {
				e, st := newTestCopyTradeEngine(t, ProviderOKX)
				e.config.FollowExitPolicyVersion = 2
				e.leaderState.Positions["p"] = &Position{PosID: "p", Symbol: "ETHUSDT", Side: SideLong, Size: 10, OpenedMS: 1000}
				if _, err := st.DB().Exec(`CREATE TRIGGER fail_auxiliary BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected reservation auxiliary failure'); END`); err != nil {
					t.Fatal(err)
				}
				makeDecision := func() *decision.Decision {
					return &decision.Decision{IsCopyTrade: true, CopyTradeAction: "open", Action: "open_long", LeaderPosID: "p", Symbol: "ETHUSDT", MarginMode: "cross", LeaderPosSize: 10, SourceOpenedMS: 1000, SourceFillID: "source-first"}
				}
				d := makeDecision()
				if e.reserveExecutionIntent(d) {
					t.Fatal("incompletely bound order released to executor")
				}
				if d.ExecutionIntentID != 0 {
					t.Fatalf("failed readiness exposed executable identity: %+v", d)
				}
				intents, err := st.CopyTrade().ListUnfinishedExecutionIntents(e.traderID)
				if err != nil || len(intents) != 1 {
					t.Fatalf("reservation lost: %+v %v", intents, err)
				}
				original := intents[0]
				if original.Status != store.ExecutionIntentReconciling || original.ReasonCode != "SOURCE_REVALIDATION_REQUIRED" || original.SourceRevision != 1 || original.LeaderTargetSize != 10 {
					t.Fatalf("not replayable without consuming source: %+v", original)
				}
				var attempts int
				if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts WHERE intent_id=?`, original.ID).Scan(&attempts); err != nil || attempts != 0 {
					t.Fatalf("unexpected exchange side effects: %d %v", attempts, err)
				}
				if _, err = st.DB().Exec(`DROP TRIGGER fail_auxiliary`); err != nil {
					t.Fatal(err)
				}
				if reclaim {
					if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET status='FAILED',reason_code='MANUAL_REVIEW_REQUIRED' WHERE id=?`, original.ID); err != nil {
						t.Fatal(err)
					}
				}
				retry := makeDecision()
				if !e.reserveExecutionIntent(retry) {
					t.Fatal("healthy replay remained blocked")
				}
				if retry.ExecutionIntentID != original.ID || retry.ClientOrderID != original.ClientOrderID || retry.SourceRevision != original.SourceRevision {
					t.Fatalf("replay changed order identity: %+v vs %+v", retry, original)
				}
				confirmed, err := st.CopyTrade().ConfirmedSourceLifecycleOpenedMS(e.traderID, "p")
				if err != nil || confirmed != 1000 {
					t.Fatalf("reclaim bypassed trusted lifecycle binding: %d %v", confirmed, err)
				}
				if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_follow_group_intents WHERE intent_id=?`, original.ID).Scan(&attempts); err != nil || attempts != 1 {
					t.Fatalf("group binding missing: %d %v", attempts, err)
				}
				if err = st.CopyTrade().CheckFollowSubmission(original.ID); err != nil {
					t.Fatal(err)
				}
				if e.reserveExecutionIntent(makeDecision()) {
					t.Fatal("ready reservation was emitted twice")
				}
			})
		}
	}
}

func TestReservationExitBatchFailurePreservesSingleFrozenBudget(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	e.config.FollowExitPolicyVersion = 2
	for _, id := range []string{"p", "q"} {
		if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderID: e.config.LeaderID, LeaderPosID: id, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 10, OpenedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		e.leaderState.Positions[id] = &Position{PosID: id, Symbol: "ETHUSDT", Side: SideLong, Size: 5}
	}
	if err := e.synchronizeFollowGroups(e.leaderState); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`CREATE TRIGGER fail_exit_batch BEFORE INSERT ON copy_trade_follow_group_exits BEGIN SELECT RAISE(ABORT,'injected exit batch failure'); END`); err != nil {
		t.Fatal(err)
	}
	makeDecision := func() *decision.Decision {
		return &decision.Decision{IsCopyTrade: true, CopyTradeAction: "reduce", Action: "reduce_long", LeaderPosID: "p", Symbol: "ETHUSDT", MarginMode: "cross", LeaderPosSize: 5, LeaderExitScope: leaderAccountExitScope}
	}
	if e.reserveExecutionIntent(makeDecision()) {
		t.Fatal("unbound group budget could execute")
	}
	intents, err := st.CopyTrade().ListUnfinishedExecutionIntents(e.traderID)
	if err != nil || len(intents) != 1 {
		t.Fatalf("missing deferred exit: %+v %v", intents, err)
	}
	original := intents[0]
	if original.Status != store.ExecutionIntentReconciling || original.ReasonCode != "SOURCE_REVALIDATION_REQUIRED" {
		t.Fatalf("exit stranded: %+v", original)
	}
	if _, err = st.DB().Exec(`DROP TRIGGER fail_exit_batch`); err != nil {
		t.Fatal(err)
	}
	retry := makeDecision()
	if !e.reserveExecutionIntent(retry) {
		t.Fatal("exit retry blocked")
	}
	b, err := st.CopyTrade().GetFollowGroupExitBatch(original.ID)
	if err != nil || b.Ratio != .5 || len(b.Members) != 2 {
		t.Fatalf("batch budget lost: %+v %v", b, err)
	}
	if retry.ExecutionIntentID != original.ID || retry.ClientOrderID != original.ClientOrderID {
		t.Fatal("exit retry changed immutable identity")
	}
	for _, id := range []string{"p", "q"} {
		m, err := st.CopyTrade().GetMappingForReconciliation(e.traderID, id)
		if err != nil || m.SourceRevision != 1 || m.LastKnownSize != 10 {
			t.Fatalf("local binding failure consumed source: %+v %v", m, err)
		}
	}
}

func TestReservationLegacyLifecycleBindingFailureAfterExplicitReclaimIsRecoverable(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	e.config.FollowExitPolicyVersion = 2
	e.leaderState.Positions["p"] = &Position{PosID: "p", Symbol: "ETHUSDT", Side: SideLong, Size: 10, OpenedMS: 1000}
	original, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: e.traderID, LeaderPosID: "p", SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: 10, ClientOrderID: "legacy-canonical-order"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET status='FAILED',reason_code='MANUAL_REVIEW_REQUIRED' WHERE id=?`, original.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`CREATE TRIGGER fail_legacy_binding BEFORE INSERT ON copy_trade_source_lifecycles BEGIN SELECT RAISE(ABORT,'injected legacy binding failure'); END`); err != nil {
		t.Fatal(err)
	}
	makeDecision := func() *decision.Decision {
		return &decision.Decision{IsCopyTrade: true, CopyTradeAction: "open", Action: "open_long", LeaderPosID: "p", Symbol: "ETHUSDT", LeaderPosSize: 10, SourceOpenedMS: 1000}
	}
	if e.reserveExecutionIntent(makeDecision()) {
		t.Fatal("legacy source identity was not bound")
	}
	pending, err := st.CopyTrade().GetExecutionIntentByID(original.ID)
	if err != nil || pending.Status != store.ExecutionIntentReconciling || pending.ReasonCode != "SOURCE_REVALIDATION_REQUIRED" {
		t.Fatalf("legacy reclaim stranded: %+v %v", pending, err)
	}
	if _, err = st.DB().Exec(`DROP TRIGGER fail_legacy_binding`); err != nil {
		t.Fatal(err)
	}
	retry := makeDecision()
	if !e.reserveExecutionIntent(retry) {
		t.Fatal("legacy retry stuck")
	}
	if retry.ExecutionIntentID != original.ID || retry.ClientOrderID != original.ClientOrderID {
		t.Fatal("changed order identity on local persistence retry")
	}
	confirmed, err := st.CopyTrade().ConfirmedSourceLifecycleOpenedMS(e.traderID, "p")
	if err != nil || confirmed != 1000 {
		t.Fatalf("missing confirmed source evidence: %d %v", confirmed, err)
	}
}

func TestSourceHealthRecordsGroupPublicationFailure(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	e.config.FollowExitPolicyVersion = 2
	snapshot := &AccountState{TotalEquity: 1000, Positions: map[string]*Position{"p": {PosID: "p", Symbol: "ETHUSDT", Side: SideLong, Size: 10, OpenedMS: 1000}}}
	e.provider = &okxPollTestProvider{&binancePollTestProvider{state: snapshot}}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`CREATE TRIGGER fail_group_publish BEFORE UPDATE ON copy_trade_follow_group_members BEGIN SELECT RAISE(ABORT,'injected group publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err == nil {
		t.Fatal("group publication failure ignored")
	}
	health, err := st.CopyTrade().GetRuntimeSource(e.traderID)
	if err != nil || health.LastFailureAt == nil || health.LastError == "" || health.LastSuccessAt == nil {
		t.Fatalf("healthy badge hid local source failure: %+v %v", health, err)
	}
}

func TestReservationDoublePersistenceFailureRecoversOnlyKnownUnqueuedWork(t *testing.T) {
	for _, viaSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot=%v", viaSnapshot), func(t *testing.T) {
			e, st := newTestCopyTradeEngine(t, ProviderOKX)
			e.config.FollowExitPolicyVersion = 2
			snapshot := &AccountState{TotalEquity: 1000, Positions: map[string]*Position{
				"p":      {PosID: "p", Symbol: "ETHUSDT", Side: SideLong, Size: 10, OpenedMS: 1000},
				"queued": {PosID: "queued", Symbol: "BTCUSDT", Side: SideLong, Size: 1, OpenedMS: 1000},
			}}
			e.leaderState = snapshot
			e.provider = &okxPollTestProvider{&binancePollTestProvider{state: snapshot}}
			queued := &decision.Decision{IsCopyTrade: true, CopyTradeAction: "open", Action: "open_long", LeaderPosID: "queued", Symbol: "BTCUSDT", LeaderPosSize: 1, SourceOpenedMS: 1000}
			if !e.reserveExecutionIntent(queued) {
				t.Fatal("control reservation not ready")
			}
			if _, err := st.DB().Exec(`CREATE TRIGGER fail_unqueued_binding BEFORE INSERT ON copy_trade_confirmed_source_lifecycles WHEN NEW.leader_pos_id='p' BEGIN SELECT RAISE(ABORT,'injected auxiliary write failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`CREATE TRIGGER fail_unqueued_defer BEFORE UPDATE ON copy_trade_execution_intents WHEN NEW.reason_code='SOURCE_REVALIDATION_REQUIRED' BEGIN SELECT RAISE(ABORT,'injected deferral write failure'); END`); err != nil {
				t.Fatal(err)
			}
			makeDecision := func() *decision.Decision {
				return &decision.Decision{IsCopyTrade: true, CopyTradeAction: "open", Action: "open_long", LeaderPosID: "p", Symbol: "ETHUSDT", LeaderPosSize: 10, SourceOpenedMS: 1000}
			}
			if e.reserveExecutionIntent(makeDecision()) {
				t.Fatal("doubly failed reservation released")
			}
			var id int64
			if err := st.DB().QueryRow(`SELECT id FROM copy_trade_execution_intents WHERE trader_id=? AND leader_pos_id='p'`, e.traderID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			before, err := st.CopyTrade().GetExecutionIntentByID(id)
			if err != nil || before.Status != store.ExecutionIntentReserved {
				t.Fatalf("double failure fixture did not retain reserved: %+v %v", before, err)
			}
			e.reservationFailureMu.Lock()
			known := e.unqueuedReservationFailures[id] != nil
			queuedKnown := e.unqueuedReservationFailures[queued.ExecutionIntentID] != nil
			e.reservationFailureMu.Unlock()
			if !known || queuedKnown {
				t.Fatal("local retry ownership includes queued work or lost failed work")
			}
			if _, err = st.DB().Exec(`DROP TRIGGER fail_unqueued_binding`); err != nil {
				t.Fatal(err)
			}
			if _, err = st.DB().Exec(`DROP TRIGGER fail_unqueued_defer`); err != nil {
				t.Fatal(err)
			}
			if viaSnapshot {
				if err = e.syncLeaderState(); err != nil {
					t.Fatal(err)
				}
				pending, _ := st.CopyTrade().GetExecutionIntentByID(id)
				if pending.Status != store.ExecutionIntentReconciling || pending.ReasonCode != "SOURCE_REVALIDATION_REQUIRED" {
					t.Fatalf("healthy snapshot did not persist deferred failure: %+v", pending)
				}
			}
			retry := makeDecision()
			if !e.reserveExecutionIntent(retry) {
				t.Fatal("double failure remained permanently reserved")
			}
			if retry.ExecutionIntentID != before.ID || retry.ClientOrderID != before.ClientOrderID {
				t.Fatal("local retry changed order identity")
			}
			ready, err := st.CopyTrade().GetExecutionIntentByID(queued.ExecutionIntentID)
			if err != nil || ready.Status != store.ExecutionIntentReserved || ready.ReasonCode != "" {
				t.Fatalf("ordinary queued work was reclaimed: %+v %v", ready, err)
			}
			if e.reserveExecutionIntent(queued) {
				t.Fatal("ordinary ready reservation was emitted again")
			}
			e.reservationFailureMu.Lock()
			remaining := len(e.unqueuedReservationFailures)
			e.reservationFailureMu.Unlock()
			if remaining != 0 {
				t.Fatalf("local recovery marker leaked: %d", remaining)
			}
		})
	}
}
