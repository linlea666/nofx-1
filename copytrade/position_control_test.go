package copytrade

import (
	"errors"
	"math"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

func controlIntegration(st *store.Store, ex DecisionExecutor) *TraderIntegration {
	ti := NewTraderIntegration("t", ex, st)
	cfg := &CopyConfig{ProviderType: ProviderOKX, LeaderID: "l", RiskPolicyVersion: 4, RiskStopLossEnabled: true, RiskProtectionMode: store.RiskProtectionModePositionMarginPct, RiskPositionMarginStopPct: .8, RiskTriggerPriceType: "mark", RiskReentryDecisionMode: "disabled"}
	cfg.FillRiskDefaults()
	ti.engine = &Engine{traderID: "t", config: cfg, store: st, leaderState: &AccountState{Positions: map[string]*Position{}}}
	return ti
}

func TestManualFixedStopSurvivesAddsReductionsAndRestart(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		t.Run(side, func(t *testing.T) {
			st, c, _ := seedHardeningFill(t)
			if side == "short" {
				for _, table := range []string{"copy_guard_cycles", "copy_trade_position_mappings", "copy_trade_position_custody"} {
					if _, err := st.DB().Exec("UPDATE " + table + " SET side='short'"); err != nil {
						t.Fatal(err)
					}
				}
			}
			ex := &positionMarginLifecycleExecutor{}
			ex.setPosition(100, 1, 10, 100, 0)
			ex.positions[0]["side"] = side
			ti := controlIntegration(st, ex)
			d := &decision.Decision{LeaderPosID: "p", Symbol: "ETHUSDT", Action: "open_" + side, MarginMode: "cross", Leverage: 10}
			ti.refreshStopLossAfterExecute(d)
			if ex.order == nil {
				t.Fatal("initial stop missing")
			}
			anchor, err := st.CopyTrade().GetCopyGuardStopAnchor(c.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			prices := []float64{90, 95}
			if side == "short" {
				prices = []float64{110, 105}
			}
			for revision, price := range prices {
				ex.order.TriggerPrice = price
				ti.refreshStopLossAfterExecute(d)
				if ex.order.TriggerPrice != price || ex.closeCalls != 0 {
					t.Fatalf("manual price reverted or exited: %+v", ex.order)
				}
				ctl, err := st.CopyTrade().GetCopyGuardStopControl(c.ID, 0)
				if err != nil || ctl.ManualPrice != price || ctl.Revision != int64(revision+1) {
					t.Fatalf("manual revision not persisted: %+v %v", ctl, err)
				}
				ti = controlIntegration(st, ex) // Restart loses every in-memory control.
				for _, qty := range []float64{2, .5} {
					ex.positions[0]["positionAmt"] = qty
					ex.positions[0]["entryPrice"] = 102.0
					ti.refreshStopLossAfterExecute(d)
					if ex.order.TriggerPrice != price || ex.order.Quantity != qty {
						t.Fatalf("quantity refresh replaced manual price: %+v", ex.order)
					}
				}
			}
			after, _ := st.CopyTrade().GetCopyGuardStopAnchor(c.ID, 0)
			if after.Price != anchor.Price || after.EntryPrice != anchor.EntryPrice {
				t.Fatal("manual override changed immutable first-entry anchor")
			}
		})
	}
}

func TestManualWidenIsObservedBeforeOldStopCrossing(t *testing.T) {
	st, _, _ := seedHardeningFill(t)
	ex := &positionMarginLifecycleExecutor{}
	ex.setPosition(100, 1, 10, 100, 80)
	ti := controlIntegration(st, ex)
	d := &decision.Decision{LeaderPosID: "p", Symbol: "ETHUSDT", Action: "open_long", MarginMode: "cross", Leverage: 10}
	ti.refreshStopLossAfterExecute(d)
	ex.order.TriggerPrice = 88
	ex.positions[0]["markPrice"] = 91.0
	ex.marketPrice = 91
	ti.refreshStopLossAfterExecute(d)
	if ex.closeCalls != 0 || ex.order.TriggerPrice != 88 {
		t.Fatalf("old stop caused an exit: closes=%d stop=%+v", ex.closeCalls, ex.order)
	}
	// The user may widen, but the liquidation boundary still tightens the stop.
	ex.positions[0]["markPrice"] = 100.0
	ex.marketPrice = 100
	ex.positions[0]["liquidationPrice"] = 96.0
	ti.refreshStopLossAfterExecute(d)
	protected := ex.order.TriggerPrice
	if protected <= 96 || protected >= 100 {
		t.Fatalf("liquidation boundary ignored: %v", protected)
	}
	ex.positions[0]["liquidationPrice"] = 80.0
	ti.refreshStopLossAfterExecute(d)
	if ex.order.TriggerPrice != protected {
		t.Fatalf("automatic safety clamp widened again: %v -> %v", protected, ex.order.TriggerPrice)
	}
	// After a confirmed clamp, a deliberate edit back to the earlier manual
	// price is a new revision even though that value appeared in history.
	ex.order.TriggerPrice = 88
	ti.refreshStopLossAfterExecute(d)
	if ex.order.TriggerPrice != 88 {
		t.Fatal("new manual revision could not restore an earlier manual price")
	}
}

type manualListingExecutor struct {
	*positionMarginLifecycleExecutor
	candidates []trader.ProtectiveStopOrder
}

func (e *manualListingExecutor) ListProtectiveStops(string) ([]trader.ProtectiveStopOrder, error) {
	return e.candidates, nil
}

func TestExternalStopReplacementRequiresUniqueFullCoverage(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			st, c, _ := seedHardeningFill(t)
			ex := &manualListingExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}
			ex.setPosition(100, 1, 10, 100, 80)
			ti := controlIntegration(st, ex)
			d := &decision.Decision{LeaderPosID: "p", Symbol: "ETHUSDT", Action: "open_long", MarginMode: "cross", Leverage: 10}
			ti.refreshStopLossAfterExecute(d)
			ex.order.TriggerPrice = 90
			ti.refreshStopLossAfterExecute(d)
			ex.order.State = "canceled"
			for n := 0; n < count; n++ {
				ex.candidates = append(ex.candidates, trader.ProtectiveStopOrder{AlgoID: string(rune('a' + n)), Symbol: "ETHUSDT", PositionSide: "long", MarginMode: "cross", Quantity: 1, TriggerPrice: 89, TriggerType: "mark", CoverageMode: trader.ProtectiveStopCoverageExactQuantity, State: "live"})
			}
			err := ti.reconcileManualStop(c, 1, .01)
			if count == 2 {
				if err == nil {
					t.Fatal("ambiguous orders adopted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ctl, _ := st.CopyTrade().GetCopyGuardStopControl(c.ID, 0)
			want := 90.0
			if count == 1 {
				want = 89
			}
			if ctl.ManualPrice != want {
				t.Fatalf("wrong replacement price: %+v", ctl)
			}
			if count == 0 {
				ti.refreshStopLossAfterExecute(d)
				if ex.order.TriggerPrice != 90 {
					t.Fatal("deleted stop restored immutable anchor instead of last user price")
				}
			}
		})
	}
}

type continuityExecutor struct {
	*positionMarginLifecycleExecutor
	fills      []trader.TradeRecord
	historyErr error
}

func (e *continuityExecutor) GetTradesForSymbol(string, time.Time, int) ([]trader.TradeRecord, error) {
	return e.fills, e.historyErr
}
func continuityFill(id, order, side string, q float64, at time.Time) trader.TradeRecord {
	return trader.TradeRecord{TradeID: id, OrderID: order, Symbol: "ETHUSDT", PositionSide: "LONG", Side: side, Quantity: q, Time: at}
}

func TestFlatAndManualReopenReusingVenuePositionIDNeverGetsClosed(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	base := &positionMarginLifecycleExecutor{}
	base.setPosition(100, 1, 10, 100, 80)
	ti := controlIntegration(st, base)
	d := &decision.Decision{LeaderPosID: "p", Symbol: "ETHUSDT", Action: "open_long", MarginMode: "cross", Leverage: 10, IsCopyTrade: true}
	ti.refreshStopLossAfterExecute(d)
	ex := &continuityExecutor{positionMarginLifecycleExecutor: base, fills: []trader.TradeRecord{
		continuityFill("1", "order1", "BUY", 1, c.OpenedAt), continuityFill("2", "manual-close", "SELL", 1, c.OpenedAt.Add(time.Second)), continuityFill("3", "manual-reopen", "BUY", .3, c.OpenedAt.Add(2*time.Second)),
	}}
	ex.setPosition(89, .3, 10, 89, 70) // Same posId; old 92 stop is crossed.
	ti = controlIntegration(st, ex)
	ti.refreshStopLossAfterExecute(d)
	if ex.closeCalls != 0 {
		t.Fatal("new manual position was closed by the old stop")
	}
	if allowed, e := st.CopyTrade().CopyGuardHasPositionAuthority(c.ID, 0); e != nil || allowed {
		t.Fatalf("old custody survived full close: %v %v", allowed, e)
	}
	ti = controlIntegration(st, ex)
	ti.pollV4ProtectiveStops()
	if ex.order.State != "canceled" || ex.closeCalls != 0 {
		t.Fatalf("old order was rearmed: %+v", ex.order)
	}
	if err := ti.preflightCopyPositionOwnership(d); ReasonCodeOf(err) != "POSITION_CUSTODY_RELEASED" && ReasonCodeOf(err) != "INDEPENDENT_POSITION_CONFLICT" {
		t.Fatalf("old following decision can reach manual position: %v", err)
	}
}

func TestManualAddStaysOwnedAndConfirmedFillProofSurvivesRestart(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	ex := &continuityExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}
	ex.setPosition(101, 1.3, 10, 101, 80)
	ex.fills = []trader.TradeRecord{continuityFill("1", "order1", "BUY", 1, c.OpenedAt), continuityFill("2", "manual-add", "BUY", .3, c.OpenedAt.Add(time.Minute))}
	ti := controlIntegration(st, ex)
	if err := ti.verifyCopyGuardContinuity(c); err != nil {
		t.Fatal(err)
	}
	ex.fills = []trader.TradeRecord{continuityFill("3", "reduce", "SELL", .5, c.OpenedAt.Add(2*time.Minute))}
	ti = controlIntegration(st, ex)
	if err := ti.verifyCopyGuardContinuity(c); err != nil {
		t.Fatalf("persisted initial entry was lost: %v", err)
	}
	ex.historyErr = errors.New("history unavailable")
	if err := ti.verifyCopyGuardContinuity(c); err == nil {
		t.Fatal("unknown history treated as owned")
	}
	if !ti.copyGuardOwnsPosition(c) {
		t.Fatal("transient history error released custody")
	}
}

func TestProtectionQueueCoalescesWithoutWaitingForProtectionLock(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	ex := &positionMarginLifecycleExecutor{}
	ex.setPosition(100, 2, 10, 100, 80)
	ti := controlIntegration(st, ex)
	ti.protectionAsync.Store(true)
	ti.protectionMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ti.queueProtectionRefresh(&decision.Decision{LeaderPosID: "p", Symbol: "ETHUSDT", Action: "open_long", MarginMode: "cross", Leverage: 10})
		ti.queueProtectionRefresh(&decision.Decision{LeaderPosID: "p", Symbol: "ETHUSDT", Action: "reduce_long", MarginMode: "cross", Leverage: 10})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		ti.protectionMu.Unlock()
		t.Fatal("protection blocked ordinary execution")
	}
	ti.protectionMu.Unlock()
	jobs, err := st.CopyTrade().ListCopyGuardProtectionJobs("t")
	if err != nil || len(jobs) != 1 || jobs[0].Revision != 2 || jobs[0].CycleID != c.ID {
		t.Fatalf("not durably coalesced: %+v %v", jobs, err)
	}
	ti.drainProtectionRefreshes()
	if ex.order == nil || math.Abs(ex.order.Quantity-2) > 1e-9 {
		t.Fatalf("worker did not protect fresh aggregate quantity: %+v", ex.order)
	}
	jobs, _ = st.CopyTrade().ListCopyGuardProtectionJobs("t")
	if len(jobs) != 0 {
		t.Fatal("completed job not removed")
	}
}

func TestRiskExitLateAcknowledgementIsRecordedAfterFlat(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	ex := &scopedExitTestExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}
	ex.setPosition(100, 1, 10, 90, 80)
	ti := controlIntegration(st, ex)
	_, _ = ti.closeCopyGuardPosition(c)
	if err := st.CopyTrade().ReleaseCopyGuardCustody(c.ID, 0, "CONFIRMED_FLAT_IN_FILLS"); err != nil {
		t.Fatal(err)
	}
	ex.state = "FILLED"
	ex.setPosition(101, 1, 10, 101, 80)
	if _, err := ti.closeCopyGuardPosition(c); err != nil {
		t.Fatal(err)
	}
	intents, _ := st.CopyTrade().ListExecutionIntentsByCycle(c.ID)
	for _, i := range intents {
		if i.SourceKind == copyGuardRiskExitSource {
			if i.FilledQuantity != .5 || i.ExchangeOrderID != "exit1" {
				t.Fatalf("exit evidence lost: %+v", i)
			}
		}
	}
	if len(ex.requests) != 1 {
		t.Fatal("new manual position increased old exit budget")
	}
}

type residualExitExecutor struct {
	*scopedExitTestExecutor
	fills []trader.TradeRecord
}

type zeroFillExitAckExecutor struct{ *scopedExitTestExecutor }

func (e *zeroFillExitAckExecutor) CloseCopyGuardPosition(req trader.CopyGuardExitRequest) (map[string]interface{}, error) {
	if err := req.BeforeSubmit(); err != nil {
		return nil, err
	}
	e.requests = append(e.requests, req)
	return map[string]interface{}{"orderId": "zero-ack", "status": "FILLED", "executedQty": 0.0}, nil
}

func TestRiskExitZeroQuantityFilledAckRemainsUnsettled(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	ex := &zeroFillExitAckExecutor{&scopedExitTestExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}}
	ex.setPosition(100, 1, 10, 90, 80)
	ti := controlIntegration(st, ex)
	if _, err := ti.closeCopyGuardPosition(c); err != nil {
		t.Fatal(err)
	}
	intents, err := st.CopyTrade().ListExecutionIntentsByCycle(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range intents {
		if i.SourceKind != copyGuardRiskExitSource {
			continue
		}
		attempts, e := st.CopyTrade().ListExecutionOrderAttempts(i.ID)
		if e != nil || len(attempts) != 1 || attempts[0].TerminalAt != nil || attempts[0].FilledQuantity != 0 || i.Status != store.ExecutionIntentReconciling {
			t.Fatalf("zero ACK became a confirmed fill: %+v %+v %v", i, attempts, e)
		}
	}
	ex.lookupErr = errors.New("acknowledgement unavailable")
	_, _ = ti.closeCopyGuardPosition(c)
	if len(ex.requests) != 1 {
		t.Fatal("unconfirmed zero ACK allowed another exit order")
	}
}

func TestUncommittedAIEntryUsesItsOwnExitProofAfterOldCustodyRelease(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	cs := st.CopyTrade()
	i, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 2, SourceKind: "AI_REENTRY", CanonicalKey: "uncommitted-ai", CycleID: c.ID, AttemptNo: 1, Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", TargetQuantity: .5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cs.PrepareExecutionOrderAttemptRecordWithKind(i.ID, "aifill", "OPEN", .5, .5); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.MarkExecutionOrderAttemptSubmitted(i.ID, "aifill"); err != nil {
		t.Fatal(err)
	}
	if err = cs.FinalizeUncommittedReentryExit(c.ID, 1); err == nil {
		t.Fatal("pending AI entry acknowledged as fully exited")
	}
	if err = cs.CompleteExecutionOrderAttempt(i.ID, "aifill", store.ExecutionOrderAttemptFilled, "new-ai-entry", "FILLED", "", .5); err != nil {
		t.Fatal(err)
	}
	if err = cs.ReleaseCopyGuardCustody(c.ID, 0, "CONFIRMED_FLAT_IN_FILLS"); err != nil {
		t.Fatal(err)
	}
	ex := &residualExitExecutor{scopedExitTestExecutor: &scopedExitTestExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}}
	ex.setPosition(100, .8, 10, 100, 80)
	ex.fills = []trader.TradeRecord{continuityFill("10", "new-ai-entry", "BUY", .5, i.CreatedAt.Add(time.Second)), continuityFill("11", "manual-add", "BUY", .3, i.CreatedAt.Add(2*time.Second))}
	ti := controlIntegration(st, ex)
	scope, quantity, err := ti.uncommittedReentryExitScope(&decision.Decision{ExecutionIntentID: i.ID, ExchangeOrderID: "new-ai-entry", FilledQuantity: .5}, c)
	if err != nil {
		t.Fatal(err)
	}
	if q, known := quantity(); !known || q != .5 {
		t.Fatalf("AI entry mistaken for old absent position: %v %v", q, known)
	}
	_, _ = ti.closeCopyGuardPositionWithQuantity(scope, quantity, false)
	if len(ex.requests) != 1 || ex.requests[0].AttemptNo != 1 || ex.requests[0].Quantity != .5 {
		t.Fatalf("wrong AI exit: %+v", ex.requests)
	}
	ex.state = "FILLED"
	ex.fills = append(ex.fills, continuityFill("12", "exit1", "SELL", .5, i.CreatedAt.Add(3*time.Second)))
	ex.setPosition(100, .3, 10, 100, 80)
	if _, err = ti.closeCopyGuardPositionWithQuantity(scope, quantity, false); err != nil {
		t.Fatal(err)
	}
	if q, known := quantity(); !known || q != 0 {
		t.Fatalf("manual residual prevents AI settlement: %v %v", q, known)
	}
	if err = cs.FinalizeUncommittedReentryExit(c.ID, 1); err != nil {
		t.Fatal(err)
	}
	if ti.copyGuardOwnsPosition(c) {
		t.Fatal("exceptional exit reacquired old custody")
	}
}

func (e *residualExitExecutor) GetTradesForSymbol(string, time.Time, int) ([]trader.TradeRecord, error) {
	return e.fills, nil
}

func TestRiskExitClosesOnlyProvenLateCopySliceAfterOriginalFlat(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	cs := st.CopyTrade()
	i, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 2, SourceKind: "LEADER_TRANSITION", CanonicalKey: "late-slice", Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", TargetQuantity: .5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cs.PrepareExecutionOrderAttemptRecordWithKind(i.ID, "late-slice", "ADD", .5, .5); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.MarkExecutionOrderAttemptSubmitted(i.ID, "late-slice"); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.BeginCopyGuardRiskExit(store.CopyGuardRiskExitBegin{CycleID: c.ID, TraderID: "t", LeaderPosID: "p", AttemptNo: 0, TriggerPrice: 92, Quantity: 1, TriggerSource: "exchange_hosted"}); err != nil {
		t.Fatal(err)
	}
	if err = cs.CommitLeaderExecutionFill(store.LeaderExecutionCommit{IntentID: i.ID, TraderID: "t", LeaderID: "l", LeaderPosID: "p", SourceRevision: 2, Action: "open_long", IsAdd: true, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", FillPrice: 91, FilledQuantity: .5, FilledNotional: 45.5, ClientOrderID: "late-slice", ExchangeOrderID: "late-order", ExchangeState: "FILLED", AttemptQuantity: .5, OrderTerminal: true}); err != nil {
		t.Fatal(err)
	}
	if err = cs.ReleaseCopyGuardCustody(c.ID, 0, "CONFIRMED_FLAT_IN_FILLS"); err != nil {
		t.Fatal(err)
	}
	// Keep the caller's pre-trigger snapshot: release/trigger can happen in
	// one polling pass, so residual authority must read the durable status.
	ex := &residualExitExecutor{scopedExitTestExecutor: &scopedExitTestExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}}
	ex.setPosition(100, .8, 10, 90, 80)
	ex.fills = []trader.TradeRecord{
		continuityFill("1", "order1", "BUY", 1, c.OpenedAt),
		continuityFill("2", "stop", "SELL", 1, c.OpenedAt.Add(time.Second)),
		continuityFill("3", "manual-new", "BUY", .3, c.OpenedAt.Add(2*time.Second)),
	}
	ti := controlIntegration(st, ex)
	if _, known := ti.copyGuardFollowerQuantity(c, true); known {
		t.Fatal("history lag was interpreted as zero residual")
	}
	ex.fills = append(ex.fills, continuityFill("4", "late-order", "BUY", .5, c.OpenedAt.Add(3*time.Second)))
	if q, known := ti.copyGuardFollowerQuantity(c, true); !known || q != .5 {
		t.Fatalf("wrong late slice: %v %v", q, known)
	}
	_, _ = ti.closeCopyGuardPosition(c)
	if len(ex.requests) != 1 || ex.requests[0].Quantity != .5 {
		t.Fatalf("manual funds included in exit: %+v", ex.requests)
	}
	ex.state = "FILLED"
	ex.fills = append(ex.fills, continuityFill("5", "exit1", "SELL", .5, c.OpenedAt.Add(4*time.Second)), continuityFill("6", "manual-later", "BUY", 2, c.OpenedAt.Add(5*time.Second)))
	ex.setPosition(100, 2.3, 10, 100, 80)
	ti = controlIntegration(st, ex)
	if _, err = ti.closeCopyGuardPosition(c); err != nil {
		t.Fatal(err)
	}
	if q, known := ti.copyGuardFollowerQuantity(c, true); !known || q != 0 {
		t.Fatalf("later manual entry revived exit authority: %v %v", q, known)
	}
	if len(ex.requests) != 1 {
		t.Fatal("duplicate exit reached later manual funds")
	}
}

func TestResumeAbsorbsPausedChangesAndInvalidatesQueuedOrders(t *testing.T) {
	st, _, _ := seedHardeningFill(t)
	cs := st.CopyTrade()
	queued, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 2, SourceKind: "LEADER_TRANSITION", CanonicalKey: "queued", Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cs.PrepareExecutionOrderAttemptRecordWithKind(queued.ID, "queued1", "ADD", 1, 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := cs.MarkManualStopped("t", "p"); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	if _, err = cs.MarkExecutionOrderAttemptSubmitted(queued.ID, "queued1"); !errors.Is(err, store.ErrFollowControlChanged) {
		t.Fatalf("queued order crossed paused boundary: %v", err)
	}
	if err = cs.ResumeFollowing("t", "p", 1, 7); err != nil {
		t.Fatal(err)
	}
	m, _ := cs.GetMapping("t", "p")
	if m.Status != store.MappingStatusActive || m.LastKnownSize != 7 || m.SourceRevision != 2 || m.AddCount != 0 || m.ReduceCount != 0 {
		t.Fatalf("resume replayed trading instead of setting baseline: %+v", m)
	}
	if err = cs.CheckFollowSubmission(queued.ID); !errors.Is(err, store.ErrFollowControlChanged) {
		t.Fatalf("resume revived queued order: %v", err)
	}
	old, _ := cs.GetExecutionIntentByID(queued.ID)
	if old.Status != store.ExecutionIntentSkipped || old.ReasonCode != "MANUAL_RESUME_BASELINE" {
		t.Fatalf("old catch-up survived resume: %+v", old)
	}
	cutoff, err := cs.PositionResumeCutoff("t", "p")
	if err != nil || cutoff == nil || time.Since(*cutoff) > time.Minute {
		t.Fatalf("missing restart-safe replay boundary: %v %v", cutoff, err)
	}
	next, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 3, SourceKind: "LEADER_TRANSITION", CanonicalKey: "future", Action: "reduce_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 6})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.CheckFollowSubmission(next.ID); err != nil {
		t.Fatalf("future leader action blocked: %v", err)
	}
}

func TestResumeWaitsForLateSubmittedFillAndRejectsEndedCustody(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	cs := st.CopyTrade()
	late, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 2, SourceKind: "LEADER_TRANSITION", CanonicalKey: "late-pause", Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cs.PrepareExecutionOrderAttemptRecordWithKind(late.ID, "latepause", "ADD", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.MarkExecutionOrderAttemptSubmitted(late.ID, "latepause"); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.MarkManualStopped("t", "p"); err != nil {
		t.Fatal(err)
	}
	if err = cs.ResumeFollowing("t", "p", 1, 3); err == nil {
		t.Fatal("unsettled order permitted resume")
	}
	if err = cs.CompleteExecutionOrderAttempt(late.ID, "latepause", store.ExecutionOrderAttemptFilled, "lateorder", "FILLED", "", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.ResumeFollowing("t", "p", 1, 3); err == nil {
		t.Fatal("uncommitted terminal fill permitted resume")
	}
	if err = cs.CommitLeaderExecutionFill(store.LeaderExecutionCommit{IntentID: late.ID, TraderID: "t", LeaderID: "l", LeaderPosID: "p", SourceRevision: 2, Action: "open_long", IsAdd: true, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 2, FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: "latepause", ExchangeOrderID: "lateorder", ExchangeState: "FILLED", OrderTerminal: true}); err != nil {
		t.Fatal(err)
	}
	m, _ := cs.GetMapping("t", "p")
	if m.Status != store.MappingStatusManualStopped {
		t.Fatal("late fill unpaused position")
	}
	if err = cs.ResumeFollowing("t", "p", m.SourceRevision, 3); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.MarkManualStopped("t", "p"); err != nil {
		t.Fatal(err)
	}
	if err = cs.ReleaseCopyGuardCustody(c.ID, 0, "CONFIRMED_FLAT_IN_FILLS"); err != nil {
		t.Fatal(err)
	}
	m, _ = cs.GetMapping("t", "p")
	if err = cs.ResumeFollowing("t", "p", m.SourceRevision, 3); err == nil {
		t.Fatal("new manual position could inherit old paused mapping")
	}
}

func TestOwnPendingAmendIsNotAManualRevision(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	ex := &positionMarginLifecycleExecutor{}
	ex.setPosition(100, 1, 10, 100, 80)
	ti := controlIntegration(st, ex)
	d := &decision.Decision{LeaderPosID: "p", Symbol: "ETHUSDT", Action: "open_long", MarginMode: "cross", Leverage: 10}
	ti.refreshStopLossAfterExecute(d)
	if err := st.CopyTrade().PrepareCopyGuardStopRequest(c.ID, 0, 95, ex.order.TriggerPrice); err != nil {
		t.Fatal(err)
	}
	if err := ti.reconcileManualStop(c, 1, .01); err != nil {
		t.Fatal(err)
	}
	ctl, _ := st.CopyTrade().GetCopyGuardStopControl(c.ID, 0)
	if ctl.Revision != 0 || !ctl.RequestPending {
		t.Fatalf("stale own amend classified manual: %+v", ctl)
	}
	ex.order.TriggerPrice = 95
	if err := ti.reconcileManualStop(c, 1, .01); err != nil {
		t.Fatal(err)
	}
	ctl, _ = st.CopyTrade().GetCopyGuardStopControl(c.ID, 0)
	if ctl.Revision != 0 || ctl.RequestPending {
		t.Fatalf("own acknowledgement classified manual: %+v", ctl)
	}
}
