package copytrade

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

// Reproduce the production state: the mapping and immutable fill ledger have
// already advanced, while an old intent is still retrying protection/revision
// recovery. No venue query or new order is needed to prove business completion.
func TestAcknowledgedFillRecoveryNeverRewindsOrAdoptsLaterManualPosition(t *testing.T) {
	for _, laterRevision := range []bool{false, true} {
		t.Run(fmt.Sprint(laterRevision), func(t *testing.T) {
			e, st := newTestCopyTradeEngine(t, ProviderOKX)
			e.config.RiskPolicyVersion = 4
			e.config.RiskLiquidationGuardEnabled = true
			if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderID: "leader", LeaderPosID: "old", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 100, OpenedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			i, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: e.traderID, LeaderPosID: "old", SourceRevision: 3, SourceKind: "LEADER_TRANSITION", CanonicalKey: "leader|old|3", Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", ClientOrderID: "ct-old", LeaderTargetSize: 100})
			if err != nil {
				t.Fatal(err)
			}
			exec := func(q string, args ...interface{}) {
				t.Helper()
				if _, err := st.DB().Exec(q, args...); err != nil {
					t.Fatal(err)
				}
			}
			rev, status := int64(3), store.MappingStatusActive
			if laterRevision {
				rev, status = 20, store.MappingStatusClosed
			}
			exec(`UPDATE copy_trade_position_mappings SET source_revision=?,status=? WHERE trader_id=? AND leader_pos_id='old'`, rev, status, e.traderID)
			exec(`UPDATE copy_trade_execution_intents SET status='RECONCILING',reason_code='REVISION_CONFLICT',filled_quantity=1.052,quantized_quantity=1.052,submitted_at=CURRENT_TIMESTAMP WHERE id=?`, i.ID)
			for n, qty := range []float64{1.051, .001} {
				client := fmt.Sprintf("ct-%d", n)
				exec(`INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,?,?,'FILLED','FILLED',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID, n+1, client, qty)
				exec(`INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional) VALUES(?,?,?,?)`, i.ID, client, qty, qty*2000)
			}
			exec(`INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,symbol,side,state) VALUES(?,'old',?,'ETHUSDT','long','RELEASED')`, e.traderID, i.ID)
			ti := NewTraderIntegration(e.traderID, &flatExecutor{}, st)
			ti.engine = e
			for n := 0; n < 3; n++ {
				ti.reconcileExecutionIntents(false)
			}
			got, err := st.CopyTrade().GetExecutionIntentByID(i.ID)
			if err != nil || got.Status != store.ExecutionIntentFilled || got.FilledQuantity != 1.052 {
				t.Fatalf("business remains stuck or changed: %+v %v", got, err)
			}
			m, err := st.CopyTrade().GetMappingForReconciliation(e.traderID, "old")
			if err != nil || m.SourceRevision != rev || m.Status != status {
				t.Fatalf("mapping rewound: %+v %v", m, err)
			}
			var cycles int
			if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_cycles WHERE trader_id=?`, e.traderID).Scan(&cycles); err != nil || cycles != 0 {
				t.Fatalf("later manual position adopted: %d %v", cycles, err)
			}
			issues, err := st.CopyTrade().ListRuntimeIssues(e.traderID)
			if err != nil || len(issues) != 0 {
				t.Fatalf("released custody created false protection retry: %+v %v", issues, err)
			}
		})
	}
}

func TestLeaderFailureFinisherDistinguishesRetryUnknownAndAbandonedRisk(t *testing.T) {
	for _, tc := range []struct {
		name, action, code string
		submitted, consume bool
	}{
		{"permanent", "open_long", "INVALID_CONFIGURATION", false, true},
		{"temporary_price", "open_long", "PRECHECK_PRICE_UNAVAILABLE", false, false},
		{"pre_submit", "open_long", "PRE_SUBMIT", false, false},
		{"unknown", "open_long", "EXECUTION_FAILED", true, false},
		{"unfilled_reduce", "reduce_long", "EXECUTION_FAILED", false, false},
		{"unfilled_close", "close_long", "EXECUTION_FAILED", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st := newTestCopyTradeEngine(t, ProviderOKX)
			if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderID: "leader", LeaderPosID: "p", Symbol: "ETHUSDT", Side: "long", LastKnownSize: 10, OpenedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			i, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: e.traderID, LeaderPosID: "p", SourceKind: "LEADER_TRANSITION", SourceRevision: 2, CanonicalKey: "leader|p|2", Action: tc.action, Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: 20})
			if err != nil {
				t.Fatal(err)
			}
			if tc.submitted {
				if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET submitted_at=CURRENT_TIMESTAMP WHERE id=?`, i.ID); err != nil {
					t.Fatal(err)
				}
			}
			ti := NewTraderIntegration(e.traderID, &flatExecutor{}, st)
			ti.engine = e
			d := &decision.Decision{ExecutionIntentID: i.ID, LeaderPosID: "p", Action: tc.action, SourceRevision: 2}
			if !ti.finishFailedLeaderSource(d, &ReasonCodedError{Code: tc.code, Cause: errors.New("failure")}) {
				t.Fatal("source was not classified")
			}
			m, err := st.CopyTrade().GetMappingForReconciliation(e.traderID, "p")
			if err != nil {
				t.Fatal(err)
			}
			if tc.consume {
				if m.SourceRevision != 2 || m.LastKnownSize != 20 || d.ExecutionStatus != store.ExecutionIntentSkipped {
					t.Fatalf("abandoned risk blocks next source: %+v %+v", m, d)
				}
			} else if m.SourceRevision != 1 || m.LastKnownSize != 10 || d.ExecutionStatus != store.ExecutionIntentReconciling {
				t.Fatalf("unresolved or retryable source consumed: %+v %+v", m, d)
			}
		})
	}
}

func TestAcknowledgedLegacyCloseRetriesProtectionWithoutReopeningBusiness(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	c := seedRiskExitGateLifecycle(t, st, e.traderID)
	i, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: e.traderID, LeaderPosID: c.LeaderPosID, SourceKind: "LEADER_TRANSITION", SourceRevision: 1, CanonicalKey: "legacy-close", Action: "close_long", Symbol: "ETHUSDT", Side: "long"})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE copy_trade_position_mappings SET status='closed',source_revision=1 WHERE trader_id=?`,
		`UPDATE copy_trade_execution_intents SET status='FILLED',filled_quantity=1,quantized_quantity=1 WHERE trader_id=?`,
	} {
		if _, err = st.DB().Exec(q, e.traderID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,1,'legacy-close','FILLED','FILLED',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional) VALUES(?,'legacy-close',1,100)`, i.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: c.ID, TraderID: e.traderID, AlgoID: "still-live", Symbol: "ETHUSDT", Side: "long", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration(e.traderID, &flatExecutor{}, st)
	ti.engine = e
	ti.retryAcknowledgedCloseProtections()
	got, _ := st.CopyTrade().GetExecutionIntentByID(i.ID)
	if got.Status != store.ExecutionIntentFilled {
		t.Fatalf("cancel failure reopened business: %+v", got)
	}
	issues, _ := st.CopyTrade().ListRuntimeIssues(e.traderID)
	if len(issues) != 1 || issues[0].Code != "COMPLETED_EXIT_PROTECTION_PENDING" {
		t.Fatalf("cancel failure lost: %+v", issues)
	}
	// Independent cleanup can finish later without issuing or replaying a close.
	if _, err = st.DB().Exec(`DELETE FROM copy_guard_protective_orders WHERE cycle_id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	ti.retryAcknowledgedCloseProtections()
	c, err = st.CopyTrade().GetCopyGuardCycle(c.ID)
	if err != nil || c.ClosedAt == nil {
		t.Fatalf("completed close left cycle live: %+v %v", c, err)
	}
	got, _ = st.CopyTrade().GetExecutionIntentByID(i.ID)
	if got.Status != store.ExecutionIntentFilled {
		t.Fatal("protection completion mutated business")
	}
}

func TestCommittedLegacyCloseFirstCallbackCannotReopenBusiness(t *testing.T) {
	for _, failure := range []string{"protection_cleanup", "accounting_begin"} {
		t.Run(failure, func(t *testing.T) {
			e, st := newTestCopyTradeEngine(t, ProviderOKX)
			e.config.RiskPolicyVersion = 4
			c := seedRiskExitGateLifecycle(t, st, e.traderID)
			cs := st.CopyTrade()
			m, err := cs.GetMappingForReconciliation(e.traderID, c.LeaderPosID)
			if err != nil {
				t.Fatal(err)
			}
			revision := m.SourceRevision + 1
			intent, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: e.traderID, LeaderPosID: c.LeaderPosID, SourceKind: "LEADER_TRANSITION", SourceRevision: revision, CanonicalKey: "first-committed-close", Action: "close_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", ClientOrderID: "first-close-order", TargetQuantity: 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "first-close-order", 1); err != nil {
				t.Fatal(err)
			}
			if err = cs.CommitLeaderExecutionFill(store.LeaderExecutionCommit{IntentID: intent.ID, TraderID: e.traderID, LeaderID: "leader", LeaderPosID: c.LeaderPosID, SourceRevision: revision, Action: "close_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: "first-close-order", ExchangeOrderID: "confirmed-close-order", ExchangeState: "FILLED", AttemptQuantity: 1, OrderTerminal: true}); err != nil {
				t.Fatal(err)
			}
			if failure == "protection_cleanup" {
				if err = cs.UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: c.ID, TraderID: e.traderID, AlgoID: "live-old-stop", Symbol: "ETHUSDT", Side: "long", Status: "live"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = st.DB().Exec(`CREATE TRIGGER fail_committed_close_accounting BEFORE UPDATE OF accounting_status ON copy_guard_cycles WHEN NEW.accounting_status='PENDING' BEGIN SELECT RAISE(ABORT,'accounting store unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			ti := NewTraderIntegration(e.traderID, &flatExecutor{}, st)
			ti.engine = e
			ti.protectionAsync.Store(failure == "accounting_begin")
			dec := &decision.Decision{IsCopyTrade: true, Action: "close_long", Symbol: "ETHUSDT", LeaderPosID: c.LeaderPosID, MarginMode: "cross", EntryPrice: 100, ExecutionIntentID: intent.ID, SourceRevision: revision, ExecutionStatus: store.ExecutionIntentFilled, ExchangeOrderID: "confirmed-close-order", FilledQuantity: 1}
			ti.updatePositionMapping(dec, true)
			got, err := cs.GetExecutionIntentByID(intent.ID)
			if err != nil || got.Status != store.ExecutionIntentFilled || got.FilledQuantity != 1 {
				t.Fatalf("first callback reopened committed business: %+v %v", got, err)
			}
			if dec.ExecutionStatus != store.ExecutionIntentFilled || dec.ExecutionIntentID != intent.ID {
				t.Fatalf("protection callback changed original decision: %+v", dec)
			}
			m, err = cs.GetMappingForReconciliation(e.traderID, c.LeaderPosID)
			if err != nil || m.Status != store.MappingStatusClosed || m.SourceRevision != revision {
				t.Fatalf("protection callback changed confirmed source: %+v %v", m, err)
			}
			issues, err := cs.ListRuntimeIssues(e.traderID)
			if err != nil || len(issues) != 1 || issues[0].Code != "COMPLETED_EXIT_PROTECTION_PENDING" {
				t.Fatalf("protection failure not independently retained: %+v %v", issues, err)
			}
			if failure == "protection_cleanup" {
				_, err = st.DB().Exec(`DELETE FROM copy_guard_protective_orders WHERE cycle_id=?`, c.ID)
			} else {
				_, err = st.DB().Exec(`DROP TRIGGER fail_committed_close_accounting`)
			}
			if err != nil {
				t.Fatal(err)
			}
			ti.retryAcknowledgedCloseProtections()
			got, err = cs.GetExecutionIntentByID(intent.ID)
			if err != nil || got.Status != store.ExecutionIntentFilled {
				t.Fatalf("retry changed business: %+v %v", got, err)
			}
			issues, err = cs.ListRuntimeIssues(e.traderID)
			if err != nil || len(issues) != 0 {
				t.Fatalf("resolved cleanup issue remains active: %+v %v", issues, err)
			}
		})
	}
}

func TestUncommittedConfirmedFlatCompensationRetainsFailureBarrier(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	e.config.RiskPolicyVersion = 4
	c := seedRiskExitGateLifecycle(t, st, e.traderID)
	cs := st.CopyTrade()
	intent, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: e.traderID, LeaderPosID: c.LeaderPosID, SourceKind: "LEADER_TRANSITION", SourceRevision: 2, CanonicalKey: "uncommitted-close", Action: "close_long", Symbol: "ETHUSDT", Side: "long"})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: c.ID, TraderID: e.traderID, AlgoID: "live-compensation-stop", Symbol: "ETHUSDT", Side: "long", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration(e.traderID, &flatExecutor{}, st)
	ti.engine = e
	dec := &decision.Decision{IsCopyTrade: true, Action: "close_long", Symbol: "ETHUSDT", LeaderPosID: c.LeaderPosID, ExecutionIntentID: intent.ID, ExecutionStatus: store.ExecutionIntentReserved}
	if _, err = ti.finalizeCopyGuardCycleState(dec, true); err == nil {
		t.Fatal("unconfirmed protection cleanup was accepted")
	}
	got, err := cs.GetExecutionIntentByID(intent.ID)
	if err != nil || got.Status != store.ExecutionIntentReconciling {
		t.Fatalf("uncommitted compensation failure was hidden: %+v %v", got, err)
	}
	m, err := cs.GetMappingForReconciliation(e.traderID, c.LeaderPosID)
	if err != nil || m.Status != store.MappingStatusActive {
		t.Fatalf("failed compensation advanced mapping: %+v %v", m, err)
	}
}
