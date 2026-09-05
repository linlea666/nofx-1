package copytrade

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

type triggeredVerificationManager struct {
	mockStopMgr
	queries int
}

func (m *triggeredVerificationManager) GetProtectiveStop(_, _ string) (*trader.ProtectiveStopOrder, error) {
	m.queries++
	return &trader.ProtectiveStopOrder{AlgoID: "fired", State: "effective", TriggerPrice: 92}, nil
}

func TestProtectionVerificationReturnsTrustedTriggerImmediately(t *testing.T) {
	mgr := &triggeredVerificationManager{}
	ti := &TraderIntegration{}
	actual, err := ti.verifyProtectiveStopWithGrace(mgr, "fired", "ETHUSDT", "long", "cross", 1, 92, .1, .1, trader.ProtectiveStopCoverageExactQuantity, "mark")
	if err != nil || actual == nil || !isProtectiveStopFired(actual.State) || mgr.queries != 1 {
		t.Fatalf("trusted trigger waited for amend settlement: %+v queries=%d err=%v", actual, mgr.queries, err)
	}
}

func TestAdoptionAndCancellationGateTrustedTriggerDuringDatabaseFailure(t *testing.T) {
	for _, path := range []string{"adopt", "cancel"} {
		t.Run(path, func(t *testing.T) {
			st, cycle, _ := seedHardeningFill(t)
			ti := NewTraderIntegration("t", &blockingRiskExitExecutor{}, st)
			actual := &trader.ProtectiveStopOrder{AlgoID: "fired", ClientID: "client", Symbol: cycle.Symbol, PositionSide: cycle.Side, State: "effective", TriggerPrice: 92}
			mgr := &mockStopMgr{byID: map[string]*trader.ProtectiveStopOrder{"fired": actual}, byClient: map[string]*trader.ProtectiveStopOrder{"client": actual}}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if path == "adopt" {
				_, _ = ti.adoptProtectiveOrderByClientID(mgr, cycle, trader.ProtectiveStopRequest{ClientID: "client", Symbol: cycle.Symbol, PositionSide: cycle.Side, Quantity: 1, TriggerPrice: 92}, .1, .1)
			} else {
				_ = ti.cancelProtectiveOrderForCycle(mgr, cycle, &store.CopyGuardProtectiveOrder{AlgoID: "fired", Symbol: cycle.Symbol, Status: "live", Quantity: 1, TriggerPrice: 92})
			}
			dec := &decision.Decision{LeaderPosID: cycle.LeaderPosID, Symbol: cycle.Symbol, Action: "open_long"}
			if err := ti.executeDecisionUnderRiskExitGate(dec, dec); !errors.Is(err, errCopyGuardRiskExitGate) {
				t.Fatalf("%s trigger did not gate queued leader order: %v", path, err)
			}
		})
	}
}

func TestProtectionCancellationQueryFailureIsNotAbsence(t *testing.T) {
	st, cycle, _ := seedHardeningFill(t)
	ti := NewTraderIntegration("t", &blockingRiskExitExecutor{}, st)
	mgr := &mockStopMgr{idErr: errors.New("query unavailable")}
	if err := ti.cancelProtectiveOrderForCycle(mgr, cycle, &store.CopyGuardProtectiveOrder{AlgoID: "live", Symbol: cycle.Symbol, Status: "live"}); err == nil {
		t.Fatal("failed query with nil response was treated as terminal cancellation")
	}
}

func seedHardeningFill(t *testing.T) (*store.Store, *store.CopyGuardCycle, int64) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "hardening.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	p := store.NewCopyGuardDefaults()
	p.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	p.RiskReentryEnabled = false
	p.RiskReentryDecisionMode = "disabled"
	p.RiskMaxReentries = 0
	snapshot, err := store.EncodeCopyGuardPolicySnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	i, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 1, SourceKind: "LEADER_TRANSITION", CanonicalKey: "leader|t|p|1", Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 1, RequestedQuantity: 1, QuantizedQuantity: 1, ClientOrderID: "open1"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().RecordCopyGuardLeverageReceipt(i.ID, "open1", 10); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().UpdateExecutionIntent(i.ID, store.ExecutionIntentSubmitted, "", "", "order1", 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().CommitLeaderExecutionFill(store.LeaderExecutionCommit{IntentID: i.ID, TraderID: "t", LeaderID: "l", LeaderPosID: "p", SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", SourceSymbol: "ETHUSDT", ExecutionSymbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 1, FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: "open1", ExchangeOrderID: "order1", ExchangeState: "FILLED", OrderTerminal: true, InitialCopyGuard: &store.InitialCopyGuardLifecycle{PolicySnapshot: snapshot, LeaderEntryPrice: 100, AccountEquity: 1000}}); err != nil {
		t.Fatal(err)
	}
	c, err := st.CopyTrade().GetOpenCopyGuardCycle("t", "p")
	if err != nil {
		t.Fatal(err)
	}
	return st, c, i.ID
}

func TestRiskExitWaitsForLateLeaderFillsWithoutReopening(t *testing.T) {
	for _, action := range []string{"open_long", "reduce_long", "close_long"} {
		t.Run(action, func(t *testing.T) {
			st, cycle, _ := seedHardeningFill(t)
			cs := st.CopyTrade()
			i, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 2, SourceKind: "LEADER_TRANSITION", CanonicalKey: "late|" + action, Action: action, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", TargetQuantity: 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = cs.PrepareExecutionOrderAttemptRecordWithKind(i.ID, "late1", "ADD", 1, 1); err != nil {
				t.Fatal(err)
			}
			if _, err = cs.MarkExecutionOrderAttemptSubmitted(i.ID, "late1"); err != nil {
				t.Fatal(err)
			}
			begin := store.CopyGuardRiskExitBegin{CycleID: cycle.ID, TraderID: "t", LeaderPosID: "p", AttemptNo: 0, TriggerPrice: 92, Quantity: 1, TriggerSource: "verified_mark_crossing"}
			if _, err = cs.BeginCopyGuardRiskExit(begin); err != nil {
				t.Fatal(err)
			}
			final := store.CopyGuardRiskExitFinalize{CopyGuardRiskExitBegin: begin, ExitPrice: 92}
			if _, err = cs.FinalizeCopyGuardRiskExit(final); err == nil {
				t.Fatal("flat finalized before in-flight order settled")
			}
			fill := store.LeaderExecutionCommit{IntentID: i.ID, TraderID: "t", LeaderID: "l", LeaderPosID: "p", SourceRevision: 2, Action: action, IsAdd: true, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", FillPrice: 91, FilledQuantity: .5, FilledNotional: 45.5, ClientOrderID: "late1", ExchangeOrderID: "late-order", ExchangeState: "PARTIALLY_FILLED", AttemptQuantity: 1}
			if err = cs.CommitLeaderExecutionFill(fill); err != nil {
				t.Fatal(err)
			}
			if _, err = cs.FinalizeCopyGuardRiskExit(final); err == nil {
				t.Fatal("partial in-flight fill allowed premature finalization")
			}
			fill.FilledQuantity, fill.FilledNotional, fill.OrderTerminal, fill.ExchangeState = 1, 91, true, "FILLED"
			if err = cs.CommitLeaderExecutionFill(fill); err != nil {
				t.Fatal(err)
			}
			if err = cs.CommitLeaderExecutionFill(fill); err != nil {
				t.Fatal("duplicate late fill:", err)
			}
			mapping, _ := cs.GetMapping("t", "p")
			if mapping.Status != store.MappingStatusStoppedByRisk || mapping.SourceRevision != 1 {
				t.Fatalf("late fill changed leader mapping: %+v", mapping)
			}
			if _, err = cs.FinalizeCopyGuardRiskExit(final); err != nil {
				t.Fatal(err)
			}
			var events int
			_ = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE type='RISK_EXIT_INFLIGHT_FILL'`).Scan(&events)
			if events != 2 {
				t.Fatalf("duplicate/missing fill events: %d", events)
			}
		})
	}
}

func TestLeverageReceiptFreezesAtSubmissionAndInitialFill(t *testing.T) {
	st, cycle, firstID := seedHardeningFill(t)
	cs := st.CopyTrade()
	if err := cs.RecordCopyGuardLeverageReceipt(firstID, "open1", 20); err != nil {
		t.Fatal(err)
	}
	e, err := cs.GetCopyGuardInitialFillEvidence(cycle.ID)
	if err != nil || e.Leverage != 10 {
		t.Fatalf("first fill evidence mutated: %+v %v", e, err)
	}
	i, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "other", SourceRevision: 1, SourceKind: "LEADER_TRANSITION", CanonicalKey: "receipt-other", Action: "open_long"})
	if err != nil {
		t.Fatal(err)
	}
	for _, lev := range []int{5, 10} {
		if err = cs.RecordCopyGuardLeverageReceipt(i.ID, "receipt1", lev); err != nil {
			t.Fatal(err)
		}
	}
	var leverage int
	if err = st.DB().QueryRow(`SELECT leverage FROM copy_guard_leverage_receipts WHERE intent_id=?`, i.ID).Scan(&leverage); err != nil || leverage != 10 {
		t.Fatalf("pre-submit receipt did not follow acknowledged leverage: %d %v", leverage, err)
	}
	if _, err = cs.PrepareExecutionOrderAttemptRecordWithKind(i.ID, "receipt1", "INITIAL_OPEN", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.MarkExecutionOrderAttemptSubmitted(i.ID, "receipt1"); err != nil {
		t.Fatal(err)
	}
	if err = cs.RecordCopyGuardLeverageReceipt(i.ID, "receipt1", 30); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT leverage FROM copy_guard_leverage_receipts WHERE intent_id=?`, i.ID).Scan(&leverage); err != nil || leverage != 10 {
		t.Fatalf("submitted receipt changed: %d %v", leverage, err)
	}
}

func TestPendingRiskExitCannotBeRetiredByReentryOrLeaderClose(t *testing.T) {
	st, cycle, _ := seedHardeningFill(t)
	cs := st.CopyTrade()
	ti := NewTraderIntegration("t", &blockingRiskExitExecutor{}, st)
	begin := store.CopyGuardRiskExitBegin{CycleID: cycle.ID, TraderID: "t", LeaderPosID: "p", AttemptNo: 0, TriggerPrice: 92, Quantity: 1, TriggerSource: "exchange_hosted"}
	if err := ti.establishRiskExitGate(begin); err != nil {
		t.Fatal(err)
	}
	// A triggered order can acquire its actual fill identity on a later poll.
	// Restart hydration must not swallow that richer venue evidence.
	ti = NewTraderIntegration("t", &blockingRiskExitExecutor{}, st)
	ti.hydrateRiskExitGates()
	begin.Metadata = map[string]interface{}{"actual_order_id": "hosted-fill"}
	if err := ti.establishRiskExitGate(begin); err != nil {
		t.Fatal(err)
	}
	e := &Engine{traderID: "t", store: st, config: &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4}, getFollowerEquity: func() float64 { return 1000 }, leaderState: &AccountState{Positions: map[string]*Position{}}}
	for _, status := range []string{store.CopyGuardStopPendingFlat, store.CopyGuardStopPartial} {
		if status == store.CopyGuardStopPartial {
			if _, err := cs.MarkCopyGuardRiskExitPartial(cycle.ID, 0, .2, 91, nil); err != nil {
				t.Fatal(err)
			}
		}
		e.checkReentryConditions()
		mapping, _ := cs.GetMapping("t", "p")
		if e.closeCopyGuardCycleAtLeaderExit(mapping) {
			t.Fatal("leader exit retired pending risk lifecycle")
		}
		if err := cs.CloseCopyGuardCycle(cycle.ID, store.CopyGuardLeaderClosed, 0, 0, 0, 0, 0, 0); err == nil {
			t.Fatal("store allowed premature cycle close")
		}
		if err := cs.BeginCopyGuardAccounting(cycle.ID, store.CopyGuardLeaderClosed, "", 0); err == nil {
			t.Fatal("accounting bypassed risk settlement")
		}
		if err := cs.UpdateCopyGuardObservation(cycle.ID, store.CopyGuardAttemptsExhausted, 100, 91, 0); err != nil {
			t.Fatal(err)
		}
		current, _ := cs.GetCopyGuardCycle(cycle.ID)
		if current.Status != status || current.ClosedAt != nil {
			t.Fatalf("pending exit overwritten: %+v", current)
		}
	}
	attempts, _ := cs.ListCopyGuardAttempts(cycle.ID)
	if attempts[0].ExitOrderID != "hosted-fill" {
		t.Fatal("trusted hosted fill identity was not persisted for restart")
	}
	if _, err := cs.FinalizeCopyGuardRiskExit(store.CopyGuardRiskExitFinalize{CopyGuardRiskExitBegin: begin, ExitPrice: 91}); err != nil {
		t.Fatal(err)
	}
	mapping, _ := cs.GetMapping("t", "p")
	if !e.closeCopyGuardCycleAtLeaderExit(mapping) {
		t.Fatal("settled stop cannot finish leader lifecycle")
	}
}

func TestActionLogsUseOriginalIdentityAfterMappingCommit(t *testing.T) {
	st, _, _ := seedHardeningFill(t) // An active mapping already exists.
	if _, err := st.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation) VALUES('t','t','','e',1000,1,'RUNNING',1)`); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration("t", &positionMarginLifecycleExecutor{}, st)
	ti.lifecycleGeneration = 1
	for _, tc := range []struct{ action, status, event, wantStatus string }{
		{"open", "executed", store.CopyEventTypeOpen, "success"},
		{"add", "partially_filled", store.CopyEventTypeAdd, "partially_filled"},
		{"add", "reconciling", store.CopyEventTypeAdd, "reconciling"},
	} {
		ti.recordActionEvent(&decision.Decision{Action: "open_long", CopyTradeAction: tc.action, Symbol: "ETHUSDT", LeaderPosID: "p", SourceFillID: tc.status}, tc.status, "", tc.status)
		var event, status string
		if err := st.DB().QueryRow(`SELECT event_type,status FROM copy_trade_events WHERE signal_id=?`, tc.status).Scan(&event, &status); err != nil {
			t.Fatal(err)
		}
		if event != tc.event || status != tc.wantStatus {
			t.Fatalf("log misclassified: %s/%s want %s/%s", event, status, tc.event, tc.wantStatus)
		}
	}
}

type hardeningExecutor struct {
	*positionMarginLifecycleExecutor
	precisionErr error
}

func TestProtectionIdentityIsTickAndVenueScopeAware(t *testing.T) {
	if sameProtectivePrice(92, 92.1, .1) || !sameProtectivePrice(92.1, 92.10000000000001, .1) {
		t.Fatal("whole tick accepted as float tolerance")
	}
	order := &trader.ProtectiveStopOrder{Symbol: "ETHUSDT", PositionSide: "long", TriggerType: "last", CoverageMode: trader.ProtectiveStopCoverageExactQuantity}
	if protectiveTriggerMatches(order, "ETHUSDT", "mark") {
		t.Fatal("last price accepted as mark")
	}
	if protectiveMarginScopeMatches(order, "cross") {
		t.Fatal("OKX missing scope accepted")
	}
	order.MarginMode = "isolated"
	if protectiveMarginScopeMatches(order, "cross") {
		t.Fatal("OKX different scope accepted")
	}
	order.MarginMode = ""
	order.CoverageMode = trader.ProtectiveStopCoverageCloseAll
	if !protectiveMarginScopeMatches(order, "cross") {
		t.Fatal("Binance direction-wide CLOSE_ALL requires nonexistent margin field")
	}
}

func (e *hardeningExecutor) ResolveExecutionInstrument(s string) (*trader.ExecutionInstrument, error) {
	if e.precisionErr != nil {
		return nil, e.precisionErr
	}
	return e.positionMarginLifecycleExecutor.ResolveExecutionInstrument(s)
}

func TestFirstFillEvidenceRecoversProtectionWithoutOriginalDecision(t *testing.T) {
	for _, fault := range []string{"positions", "precision", "anchor_write"} {
		t.Run(fault, func(t *testing.T) {
			st, cycle, intentID := seedHardeningFill(t)
			ex := &hardeningExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}
			ex.setPosition(100, 1, 10, 100, 80)
			ti := NewTraderIntegration("t", ex, st)
			ti.engine = &Engine{traderID: "t", store: st, config: &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopLossEnabled: true}}
			switch fault {
			case "positions":
				ex.freshErr = errors.New("positions unavailable")
			case "precision":
				ex.precisionErr = errors.New("precision unavailable")
			case "anchor_write":
				if _, err := st.DB().Exec(`CREATE TRIGGER fail_anchor BEFORE UPDATE OF stop_anchor_entry_price ON copy_guard_attempts BEGIN SELECT RAISE(ABORT,'anchor write unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			ti.refreshStopLossAfterExecute(&decision.Decision{LeaderPosID: "p", Symbol: "ETHUSDT", Action: "open_long", MarginMode: "cross", CopyTradeAction: "open", ExecutionIntentID: intentID})
			if len(ex.requests) != 0 {
				t.Fatal("fault unexpectedly installed protection")
			}
			ex.freshErr = nil
			ex.precisionErr = nil
			if fault == "anchor_write" {
				if _, err := st.DB().Exec(`DROP TRIGGER fail_anchor`); err != nil {
					t.Fatal(err)
				}
			}
			// A restart/retry sees a different average, quantity and leverage. Only
			// the original immutable fill may determine the fixed price.
			ex.setPosition(97, 2, 20, 99, 80)
			ti = NewTraderIntegration("t", ex, st)
			ti.engine = &Engine{traderID: "t", store: st, config: &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopLossEnabled: true}}
			ti.refreshStopLossAfterExecute(&decision.Decision{LeaderPosID: "p", Symbol: "ETHUSDT", Action: "open_long", MarginMode: "cross", Reasoning: "Copy Guard protection retry"})
			a, err := st.CopyTrade().GetCopyGuardStopAnchor(cycle.ID, 0)
			if err != nil || a.EntryPrice != 100 || a.Leverage != 10 || a.InitialMargin != 10 || a.Price != 92 {
				t.Fatalf("anchor=%+v err=%v", a, err)
			}
			if ex.order == nil || ex.order.TriggerPrice != 92 || ex.order.Quantity != 2 {
				t.Fatalf("recovered protection=%+v", ex.order)
			}
		})
	}
}

type timedHardeningExecutor struct {
	*positionMarginLifecycleExecutor
	mark trader.MarkPriceObservation
}

func (e *timedHardeningExecutor) GetMarkPriceObservation(string) (trader.MarkPriceObservation, error) {
	return e.mark, nil
}

func TestFixedMarkRejectsStaleFutureAndMissingTimestampWithoutLastFallback(t *testing.T) {
	for _, observed := range []time.Time{time.Time{}, time.Now().Add(-time.Minute), time.Now().Add(time.Minute)} {
		ex := &timedHardeningExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{marketPrice: 80}, mark: trader.MarkPriceObservation{Price: 80, ObservedAt: observed, Source: "venue_mark"}}
		ti := &TraderIntegration{executor: ex}
		if price, _ := ti.fixedStopReferencePrice("ETHUSDT", 80); price != 0 {
			t.Fatalf("untrusted mark triggered: %v at %s", price, observed)
		}
	}
}

func TestFixedLiquidationCanCrossEntryButMustNotLoosen(t *testing.T) {
	in := PositionMarginStopEvaluationInput{Side: SideLong, CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 10, AnchorStopPrice: 92, MarkPrice: 110, LiquidationPrice: 105, PriceTickSize: .1, BaseQuantityStep: .01, ATRValue: 50, LiquidationBufferATR: 5}
	got, err := EvaluatePositionMarginStop(in)
	if err != nil || !got.Clamped || got.StopPrice != 105.2 {
		t.Fatalf("cross-entry liquidation ignored or ATR leaked: %+v %v", got, err)
	}
	in.ExistingStopPrice = got.StopPrice
	in.LiquidationPrice = 80
	got, err = EvaluatePositionMarginStop(in)
	if err != nil || got.StopPrice != 105.2 {
		t.Fatalf("safety recovery loosened stop: %+v %v", got, err)
	}
	if sameProtectivePrice(92, 92.1, .1) {
		t.Fatal("one tick safety tightening was considered equal")
	}
}

func TestTrustedTriggerGatesQueuedOrderBeforeSubmissionEvenWithDatabaseFailure(t *testing.T) {
	st, cycle, intentID := seedHardeningFill(t)
	ex := &positionMarginLifecycleExecutor{}
	ti := NewTraderIntegration("t", ex, st)
	d := &decision.Decision{ExecutionIntentID: intentID, LeaderPosID: "p", Action: "open_long", CopyTradeAction: "add", Symbol: "ETHUSDT"}
	ti.bindExecutionAttemptRecorder(d)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	ti.observeTrustedProtectiveTrigger(cycle, &store.CopyGuardProtectiveOrder{AlgoID: "sl", TriggerPrice: 92, Quantity: 1}, &trader.ProtectiveStopOrder{AlgoID: "sl", State: "triggered", TriggerPrice: 92, Quantity: 1})
	if err := d.BeforeExchangeSubmit("queued1"); !errors.Is(err, errCopyGuardRiskExitGate) {
		t.Fatalf("queued order escaped gate: %v", err)
	}
}

type scopedExitTestExecutor struct {
	*positionMarginLifecycleExecutor
	requests  []trader.CopyGuardExitRequest
	lookupErr error
	state     string
}

func (e *scopedExitTestExecutor) CloseCopyGuardPosition(req trader.CopyGuardExitRequest) (map[string]interface{}, error) {
	if err := req.BeforeSubmit(); err != nil {
		return nil, err
	}
	e.requests = append(e.requests, req)
	return nil, errors.New("transport timeout")
}
func (e *scopedExitTestExecutor) GetOrderStatusByClientID(string, string) (map[string]interface{}, error) {
	if e.lookupErr != nil {
		return nil, e.lookupErr
	}
	return map[string]interface{}{"status": e.state, "orderId": "exit1", "executedQty": .5}, nil
}

func TestScopedRiskExitReconcilesTimeoutAndResidualWithDurableIdentity(t *testing.T) {
	st, cycle, _ := seedHardeningFill(t)
	ex := &scopedExitTestExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}
	ex.setPosition(100, 1, 10, 90, 80)
	ti := NewTraderIntegration("t", ex, st)
	if _, err := ti.closeCopyGuardPosition(cycle); err == nil {
		t.Fatal("expected submit timeout")
	}
	if len(ex.requests) != 1 || ex.requests[0].MarginMode != "cross" || ex.requests[0].CycleID != cycle.ID || ex.requests[0].Quantity != 1 {
		t.Fatalf("lost exit scope: %+v", ex.requests)
	}
	// Recreate integration to prove no in-memory order identity is required.
	ti = NewTraderIntegration("t", ex, st)
	ex.lookupErr = errors.New("lookup unavailable")
	_, _ = ti.closeCopyGuardPosition(cycle)
	if len(ex.requests) != 1 {
		t.Fatal("timeout lookup failure resubmitted exit")
	}
	ex.lookupErr = nil
	ex.state = "NEW"
	_, _ = ti.closeCopyGuardPosition(cycle)
	if len(ex.requests) != 1 {
		t.Fatal("live order was duplicated")
	}
	ex.state = "FILLED"
	ex.setPosition(100, .5, 10, 90, 80)
	_, _ = ti.closeCopyGuardPosition(cycle)
	if len(ex.requests) != 2 || ex.requests[1].Quantity != .5 || ex.requests[0].ClientOrderID == ex.requests[1].ClientOrderID {
		t.Fatalf("residual exit not scoped/idempotent: %+v", ex.requests)
	}
}

func TestAbsentFollowerDetachesWithoutStopOrRearm(t *testing.T) {
	st, cycle, _ := seedHardeningFill(t)
	ex := &positionMarginLifecycleExecutor{}
	ti := NewTraderIntegration("t", ex, st)
	if _, err := st.DB().Exec(`UPDATE copy_trade_execution_intents SET updated_at=datetime('now','-1 minute')`); err != nil {
		t.Fatal(err)
	}
	cycle.OpenedAt = time.Now().Add(-time.Minute)
	if !ti.confirmCopyGuardFollowerAbsent(cycle) {
		t.Fatal("fresh absent position was not detached")
	}
	fresh, err := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil || fresh.StopCount != 0 || fresh.ClosedAt != nil || fresh.ProtectionStatus != store.CopyGuardProtectionPositionAbsent {
		t.Fatalf("absence created fake stop/terminal lifecycle: %+v %v", fresh, err)
	}
	m, err := st.CopyTrade().GetMapping("t", "p")
	if err != nil || m.Status != store.MappingStatusDetached {
		t.Fatalf("old lifecycle can reopen: %+v %v", m, err)
	}
}
