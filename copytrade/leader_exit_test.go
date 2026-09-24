package copytrade

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

type leaderExitExecutor struct {
	*positionMarginLifecycleExecutor
	requests    []trader.ScopedPositionCloseRequest
	orders      map[string]map[string]interface{}
	firstFill   float64
	before      func()
	ackOnly     bool
	lookupErr   error
	lookupReply map[string]interface{}
}

func (e *leaderExitExecutor) CloseScopedPosition(r trader.ScopedPositionCloseRequest) (map[string]interface{}, error) {
	if e.before != nil {
		e.before()
		e.before = nil
	}
	if err := r.BeforeSubmit(); err != nil {
		return nil, err
	}
	e.requests = append(e.requests, r)
	q := r.Quantity
	state := "FILLED"
	if e.firstFill > 0 {
		q = e.firstFill
		e.firstFill = 0
		state = "CANCELED"
	}
	for _, p := range e.positions {
		if p["symbol"] == r.Symbol && p["side"] == r.Side && p["marginMode"] == r.MarginMode && p["posId"] == r.PositionID {
			p["positionAmt"] = math.Max(0, getFloatField(p, "positionAmt")-q)
		}
	}
	order := map[string]interface{}{"orderId": fmt.Sprintf("exit-%d", len(e.requests)), "status": state, "executedQty": q}
	if e.orders == nil {
		e.orders = map[string]map[string]interface{}{}
	}
	e.orders[r.ClientOrderID] = order
	if e.ackOnly {
		return map[string]interface{}{"orderId": order["orderId"], "status": "FILLED"}, nil
	}
	return order, nil
}
func (e *leaderExitExecutor) GetOrderStatusByClientID(_, id string) (map[string]interface{}, error) {
	if e.lookupErr != nil {
		return nil, e.lookupErr
	}
	if e.lookupReply != nil {
		return e.lookupReply, nil
	}
	o, ok := e.orders[id]
	if !ok {
		return nil, fmt.Errorf("unknown order")
	}
	return o, nil
}
func exitPosition(mode, id, side string, qty float64) map[string]interface{} {
	return map[string]interface{}{"symbol": "ETHUSDT", "side": side, "marginMode": mode, "posId": id, "positionAmt": qty}
}
func leaderExitFixture(t *testing.T, ratio float64) (*TraderIntegration, *leaderExitExecutor, *decision.Decision) {
	t.Helper()
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	if _, err := st.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation) VALUES(?,'exit-test','','e',1000,1,'RUNNING',1)`, e.traderID); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderID: "leader", LeaderPosID: "p", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 10, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	action, kind := "reduce_long", "reduce"
	if ratio == 1 {
		action, kind = "close_long", "close"
	}
	i, claimed, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: e.traderID, LeaderPosID: "p", SourceRevision: 2, SourceKind: "LEADER_TRANSITION", Action: action, Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: 10 * (1 - ratio), LeaderExitScope: leaderAccountExitScope, LeaderExitRatio: ratio, ClientOrderID: "exit-fixture", SourceFillID: "snapshot-exit"})
	if err != nil || !claimed {
		t.Fatalf("reserve: %v %+v", err, i)
	}
	ex := &leaderExitExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}
	ti := NewTraderIntegration(e.traderID, ex, st)
	ti.engine = e
	dec := &decision.Decision{IsCopyTrade: true, CopyTradeAction: kind, Action: action, Symbol: "ETHUSDT", LeaderPosID: "p", SourceRevision: 2, LeaderPosSize: 10 * (1 - ratio), CloseRatio: ratio, ExecutionIntentID: i.ID, LeaderExitScope: leaderAccountExitScope}
	return ti, ex, dec
}
func TestLeaderFullExitIncludesManualAllMarginModesAndCompletesOnce(t *testing.T) {
	ti, ex, d := leaderExitFixture(t, 1)
	ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 3), exitPosition("isolated", "i", "long", 2), exitPosition("cross", "s", "short", 7)}
	// Ownership may already be released; only source following authorizes exit.
	_, err := ti.store.DB().Exec(`INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,symbol,side,margin_mode,state) VALUES(?,'p',1,'ETHUSDT','long','cross','RELEASED')`, ti.traderID)
	if err != nil {
		t.Fatal(err)
	}
	if err = ti.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 2 || getFloatField(ex.positions[2], "positionAmt") != 7 {
		t.Fatalf("wrong scopes: %+v", ex.requests)
	}
	revision, _ := ti.store.CopyTrade().GetSourceSnapshotRevision(ti.traderID, "p")
	if revision != 2 {
		t.Fatalf("source not consumed: %d", revision)
	}
	ex.positions[0]["positionAmt"] = 4.0
	if err = ti.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 2 || getFloatField(ex.positions[0], "positionAmt") != 4 {
		t.Fatal("duplicate old close touched a new manual position")
	}
}
func TestLeaderPartialExitFreezesTargetAcrossPartialFillAndRestart(t *testing.T) {
	ti, ex, d := leaderExitFixture(t, .5)
	ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 10)}
	ex.firstFill = 2
	if err := ti.executeAccountLeaderExit(d, true); err == nil {
		t.Fatal("partial fill falsely completed")
	}
	ex.positions[0]["positionAmt"] = 10.0 // manual add while reconciling
	restarted := NewTraderIntegration(ti.traderID, ex, ti.store)
	restarted.engine = ti.engine
	if err := restarted.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 2 || ex.requests[1].Quantity != 3 || getFloatField(ex.positions[0], "positionAmt") != 7 {
		t.Fatalf("retry recalculated ratio: %+v", ex.requests)
	}
}
func TestLeaderExitPauseAtSubmissionDoesNotSendOrBlockResume(t *testing.T) {
	ti, ex, d := leaderExitFixture(t, .5)
	ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 10)}
	ex.before = func() {
		if _, err := ti.store.CopyTrade().MarkManualStopped(ti.traderID, "p"); err != nil {
			t.Fatal(err)
		}
	}
	if err := ti.executeAccountLeaderExit(d, true); err == nil {
		t.Fatal("pause must interrupt submission")
	}
	if err := ti.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 0 {
		t.Fatal("paused exit submitted")
	}
	i, _ := ti.store.CopyTrade().GetExecutionIntentByID(d.ExecutionIntentID)
	if i.Status != store.ExecutionIntentSkipped || i.ReasonCode != "FOLLOW_CONTROL_CHANGED" {
		t.Fatalf("not durably cancelled: %+v", i)
	}
}
func TestLeaderReductionAggregationNeverTreatsSmallRemainingAsFlat(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	for _, id := range []string{"cross-source", "isolated-source"} {
		if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderPosID: id, LeaderID: "leader", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 100, OpenedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	e.leaderState.Positions["cross-source"] = &Position{PosID: "cross-source", Symbol: "ETHUSDT", Side: SideLong, Size: 1}
	e.leaderState.Positions["isolated-source"] = &Position{PosID: "isolated-source", Symbol: "ETHUSDT", Side: SideLong, Size: 100}
	signal := &TradeSignal{Fill: &Fill{LeaderPosID: "cross-source", Symbol: "ETHUSDT", PositionSide: SideLong, Action: ActionReduce}}
	match := e.matchAccountLeaderReduction(signal, e.buildLeaderPosMap())
	ratio := e.accountLeaderReductionRatio(signal, match)
	if match.Action != ActionReduce || math.Abs(ratio-.495) > 1e-12 {
		t.Fatalf("wrong aggregate reduction %+v %v", match, ratio)
	}
}

func TestReleasedSourceIncreaseUpdatesReductionBaselineWithoutBuying(t *testing.T) {
	for _, state := range []string{store.MappingStatusDetached, store.MappingStatusStoppedByRisk} {
		t.Run(state, func(t *testing.T) {
			e, st := newTestCopyTradeEngine(t, ProviderOKX)
			if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderPosID: "p", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 10, OpenedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE copy_trade_position_mappings SET status=?`, state); err != nil {
				t.Fatal(err)
			}
			e.leaderState.Positions["p"] = &Position{PosID: "p", Symbol: "ETHUSDT", Side: SideLong, Size: 20}
			if fills := e.detectBinancePositionSnapshotFills(); len(fills) != 0 {
				t.Fatalf("released source increase caused order: %+v", fills)
			}
			e.leaderState.Positions["p"].Size = 15
			fills := e.detectBinancePositionSnapshotFills()
			if len(fills) != 1 || fills[0].Action != ActionReduce {
				t.Fatalf("reduction lost after source added: %+v", fills)
			}
			signal := e.buildSignal(&fills[0])
			match := e.matchSignalWithMapping(signal)
			if r := e.accountLeaderReductionRatio(signal, match); r != .25 {
				t.Fatalf("ratio=%v want .25", r)
			}
		})
	}
}

func TestSameSideSourceReopenUsesNewLifecycleEvenWithReusedPositionID(t *testing.T) {
	ti, _, _ := leaderExitFixture(t, 1)
	if err := ti.store.CopyTrade().BindSourceLifecycle(ti.traderID, "p", 1000); err != nil {
		t.Fatal(err)
	}
	ti.engine.leaderState.Positions["p"] = &Position{PosID: "p", Symbol: "ETHUSDT", Side: SideLong, Size: 10, OpenedMS: 2000}
	fills := ti.engine.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].Action != ActionClose || sourceFillMarksLeaderReversal(fills[0].ID) {
		t.Fatalf("same-side lifecycle identity lost: %+v", fills)
	}
	signal := ti.engine.buildSignal(&fills[0])
	match := ti.engine.matchSignalWithMapping(signal)
	if !match.ShouldFollow || match.LeaderReversed || match.LeaderPosition != nil {
		t.Fatalf("wrong same-side reopen match: %+v", match)
	}
}

func TestLeaderFullExitWaitsForOwnLateEntryFill(t *testing.T) {
	ti, ex, d := leaderExitFixture(t, 1)
	res, err := ti.store.DB().Exec(`INSERT INTO copy_trade_execution_intents(trader_id,leader_pos_id,source_revision,source_kind,action,symbol,side,status,client_order_id,source_fill_id) VALUES(?,'older-entry',1,'LEADER_TRANSITION','open_long','ETHUSDT','long','SUBMITTED','late-entry','late-fill')`, ti.traderID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err = ti.store.CopyTrade().PrepareExecutionOrderAttemptRecordWithKind(id, "late-entry", "ENTRY", 2, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = ti.store.CopyTrade().MarkExecutionOrderAttemptSubmitted(id, "late-entry"); err != nil {
		t.Fatal(err)
	}
	if err = ti.executeAccountLeaderExit(d, true); err == nil {
		t.Fatal("flat snapshot completed before in-flight entry settled")
	}
	ex.positions = []map[string]interface{}{exitPosition("cross", "late", "long", 2)}
	if err = ti.store.CopyTrade().CompleteExecutionOrderAttempt(id, "late-entry", store.ExecutionOrderAttemptFilled, "late-exchange", "FILLED", "", 2); err != nil {
		t.Fatal(err)
	}
	if err = ti.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 1 || ex.requests[0].Quantity != 2 {
		t.Fatalf("late entry not drained: %+v", ex.requests)
	}
}

func TestLeaderExitUsesPersistedExecutionContractWithoutRemapping(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderPosID: "p", Symbol: "BTCUSDC", SourceSymbol: "BTCUSDT", ExecutionSymbol: "BTCUSDC", Side: "long", MarginMode: "cross", LastKnownSize: 10, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	e.leaderState.Positions["p"] = &Position{PosID: "p", Symbol: "BTCUSDT", Side: SideLong, Size: 5}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 {
		t.Fatal(fills)
	}
	signal := e.buildSignal(&fills[0])
	match := e.matchSignalWithMapping(signal)
	if !match.ShouldFollow {
		t.Fatal(match)
	}
	dec := e.buildDecisionV2(signal, match, 0)
	if dec.Symbol != "BTCUSDC" || dec.SourceSymbol != "BTCUSDT" || dec.CloseRatio != .5 {
		t.Fatalf("execution contract changed: %+v", dec)
	}
}

func TestLeaderExitUnknownReceiptDoesNotSubmitAgain(t *testing.T) {
	ti, ex, d := leaderExitFixture(t, .5)
	ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 10)}
	plan, err := ti.prepareLeaderExit(d)
	if err != nil {
		t.Fatal(err)
	}
	key := "LEADER_EXIT:" + strings.ToUpper(stableClientOrderID(ti.traderID, plan.Targets[0].Key, "scope"))
	if _, err = ti.store.CopyTrade().PrepareExecutionOrderAttemptRecordWithKind(d.ExecutionIntentID, "ambiguous", key, 5, 5); err != nil {
		t.Fatal(err)
	}
	if _, err = ti.store.CopyTrade().MarkExecutionOrderAttemptSubmitted(d.ExecutionIntentID, "ambiguous"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = ti.executeAccountLeaderExit(d, true); err == nil {
			t.Fatal("unknown acknowledgement treated as terminal")
		}
	}
	if len(ex.requests) != 0 {
		t.Fatal("uncertain order blindly retried")
	}
	ex.orders = map[string]map[string]interface{}{"ambiguous": {"status": "FILLED", "executedQty": 5.0, "updateTime": int64(1790100000123)}}
	ex.positions[0]["positionAmt"] = 5.0
	if err = ti.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 0 {
		t.Fatal("already-filled reduction sent again")
	}
	timing, err := ti.store.CopyTrade().GetExecutionTiming(d.ExecutionIntentID)
	if err != nil || timing.FirstSubmittedMS <= 0 || timing.ExchangeFilledMS != 1790100000123 {
		t.Fatalf("exchange timing not retained: %+v %v", timing, err)
	}
}
