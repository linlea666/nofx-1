package copytrade

import (
	"errors"
	"math"
	"path/filepath"
	"testing"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

type positionMarginLifecycleExecutor struct {
	positions   []map[string]interface{}
	marketPrice float64
	order       *trader.ProtectiveStopOrder
	requests    []trader.ProtectiveStopRequest
	amendErr    error
	closeCalls  int
}

func (e *positionMarginLifecycleExecutor) ExecuteDecision(*decision.Decision) error { return nil }
func (e *positionMarginLifecycleExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{"total_equity": 1000.0}, nil
}
func (e *positionMarginLifecycleExecutor) GetPositions() ([]map[string]interface{}, error) {
	return e.positions, nil
}
func (e *positionMarginLifecycleExecutor) GetPositionsFresh() ([]map[string]interface{}, error) {
	return e.positions, nil
}
func (e *positionMarginLifecycleExecutor) ResolveExecutionInstrument(string) (*trader.ExecutionInstrument, error) {
	return &trader.ExecutionInstrument{PriceTickSize: .1, BaseQuantityStep: .01}, nil
}
func (e *positionMarginLifecycleExecutor) SetStopLoss(string, string, float64, float64) error {
	return nil
}
func (e *positionMarginLifecycleExecutor) CancelStopLossOrders(string) error { return nil }
func (e *positionMarginLifecycleExecutor) GetMarketPrice(string) (float64, error) {
	return e.marketPrice, nil
}
func (e *positionMarginLifecycleExecutor) PlaceProtectiveStop(req trader.ProtectiveStopRequest) (*trader.ProtectiveStopOrder, error) {
	e.requests = append(e.requests, req)
	e.order = &trader.ProtectiveStopOrder{
		AlgoID: "fixed-stop", ClientID: req.ClientID, Symbol: req.Symbol,
		PositionSide: req.PositionSide, MarginMode: req.MarginMode,
		Quantity: req.Quantity, TriggerPrice: req.TriggerPrice, State: "live",
	}
	copy := *e.order
	return &copy, nil
}
func (e *positionMarginLifecycleExecutor) AmendProtectiveStop(_ string, req trader.ProtectiveStopRequest) error {
	e.requests = append(e.requests, req)
	if e.amendErr != nil {
		return e.amendErr
	}
	if e.order == nil {
		return trader.ErrProtectiveStopNotFound
	}
	e.order.Symbol, e.order.PositionSide, e.order.MarginMode = req.Symbol, req.PositionSide, req.MarginMode
	e.order.Quantity, e.order.TriggerPrice, e.order.State = req.Quantity, req.TriggerPrice, "live"
	return nil
}
func (e *positionMarginLifecycleExecutor) GetProtectiveStop(algoID, _ string) (*trader.ProtectiveStopOrder, error) {
	if e.order == nil || e.order.AlgoID != algoID {
		return nil, trader.ErrProtectiveStopNotFound
	}
	copy := *e.order
	return &copy, nil
}
func (e *positionMarginLifecycleExecutor) GetProtectiveStopByClientID(clientID, _ string) (*trader.ProtectiveStopOrder, error) {
	if e.order == nil || e.order.ClientID != clientID {
		return nil, trader.ErrProtectiveStopNotFound
	}
	copy := *e.order
	return &copy, nil
}
func (e *positionMarginLifecycleExecutor) CancelProtectiveStop(algoID, _ string) error {
	if e.order == nil || e.order.AlgoID != algoID {
		return trader.ErrProtectiveStopNotFound
	}
	e.order.State = "canceled"
	return nil
}
func (e *positionMarginLifecycleExecutor) ClosePositionMarket(string, string) (string, error) {
	e.closeCalls++
	return "fixed-stop-exit", nil
}

func (e *positionMarginLifecycleExecutor) setPosition(entry, quantity float64, leverage int, mark, liquidation float64) {
	e.marketPrice = mark
	e.positions = []map[string]interface{}{{
		"symbol": "ETHUSDT", "side": "long", "marginMode": "cross",
		"positionAmt": quantity, "entryPrice": entry, "leverage": leverage,
		"markPrice": mark, "liquidationPrice": liquidation, "posId": "follower-position",
	}}
}

func TestFixedPositionMarginLifecycleKeepsPriceAndSynchronizesOnlyQuantity(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "fixed-stop-lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const traderID = "fixed-stop-trader"
	mapping := &store.CopyTradePositionMapping{
		TraderID: traderID, LeaderID: "leader", LeaderPosID: "leader-position",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		OpenPrice: 100, OpenSizeUSD: 100, LastKnownSize: 1,
	}
	if err = st.CopyTrade().SavePositionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: traderID, LeaderID: "leader", LeaderPosID: mapping.LeaderPosID,
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot:     `{"risk_protection_mode":"position_margin_pct","risk_position_margin_stop_pct":0.8,"risk_liquidation_buffer_atr":0.5}`,
		FollowerEntryPrice: 100, FollowerNotional: 100, AccountEquity: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 100, 100, 1, 0); err != nil {
		t.Fatal(err)
	}
	cfg := &CopyConfig{
		ProviderType: ProviderOKX, LeaderID: "leader", RiskPolicyVersion: 4,
		RiskStopLossEnabled: true, RiskProtectionMode: store.RiskProtectionModePositionMarginPct,
		RiskPositionMarginStopPct: .80, RiskTriggerPriceType: "mark",
		RiskReentryDecisionMode: "disabled",
	}
	cfg.FillRiskDefaults()
	executor := &positionMarginLifecycleExecutor{}
	executor.setPosition(100, 1, 10, 100, 80)
	ti := NewTraderIntegration(traderID, executor, st)
	ti.engine = &Engine{traderID: traderID, config: cfg, store: st}

	initial := &decision.Decision{
		Symbol: "ETHUSDT", Action: "open_long", CopyTradeAction: "open",
		LeaderPosID: mapping.LeaderPosID, MarginMode: "cross", Leverage: 10,
	}
	ti.refreshStopLossAfterExecute(initial)
	if executor.order == nil || executor.order.TriggerPrice != 92 || executor.order.Quantity != 1 {
		t.Fatalf("initial fixed stop=%+v, want price=92 quantity=1", executor.order)
	}
	if len(executor.requests) != 1 || executor.requests[0].TriggerType != "mark" {
		t.Fatalf("fixed stop was not placed once with mark trigger: %+v", executor.requests)
	}
	attempts, err := st.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("initial fixed attempt: %+v err=%v", attempts, err)
	}
	anchor := attempts[0]
	if anchor.StopAnchorEntryPrice != 100 || anchor.StopAnchorLeverage != 10 ||
		anchor.StopAnchorInitialMargin != 10 || anchor.StopAnchorPrice != 92 ||
		anchor.StopConfiguredMarginLossPct != .80 {
		t.Fatalf("first-fill anchor was not frozen: %+v", anchor)
	}

	// An add changes weighted entry, quantity and leverage, but only the hosted
	// protection quantity may change. The absolute strategy stop remains 92.
	executor.setPosition(105, 2, 20, 106, 90)
	ti.refreshStopLossAfterExecute(&decision.Decision{
		Symbol: "ETHUSDT", Action: "open_long", CopyTradeAction: "add",
		LeaderPosID: mapping.LeaderPosID, MarginMode: "cross", Leverage: 20,
	})
	if executor.order.TriggerPrice != 92 || executor.order.Quantity != 2 {
		t.Fatalf("add changed fixed price or missed quantity expansion: %+v", executor.order)
	}

	// A venue amend failure keeps the older live protection and never invokes a
	// forced exit. The next retry adopts the same anchor and catches quantity up.
	executor.amendErr = errors.New("temporary amend failure")
	executor.setPosition(106, 3, 20, 107, 90)
	ti.refreshStopLossAfterExecute(&decision.Decision{
		Symbol: "ETHUSDT", Action: "open_long", CopyTradeAction: "add",
		LeaderPosID: mapping.LeaderPosID, MarginMode: "cross", Leverage: 20,
	})
	if executor.closeCalls != 0 || executor.order.TriggerPrice != 92 || executor.order.Quantity != 2 {
		t.Fatalf("failed amend disturbed leader flow or old protection: close=%d order=%+v", executor.closeCalls, executor.order)
	}
	executor.amendErr = nil
	ti.refreshStopLossAfterExecute(&decision.Decision{
		Symbol: "ETHUSDT", Action: "open_long", CopyTradeAction: "add",
		LeaderPosID: mapping.LeaderPosID, MarginMode: "cross", Leverage: 20,
		Reasoning: "Copy Guard protection retry",
	})
	if executor.order.TriggerPrice != 92 || executor.order.Quantity != 3 {
		t.Fatalf("protection retry did not recover quantity at fixed price: %+v", executor.order)
	}

	// A reduction shrinks coverage without moving price.
	executor.setPosition(103, .5, 20, 104, 80)
	ti.refreshStopLossAfterExecute(&decision.Decision{
		Symbol: "ETHUSDT", Action: "reduce_long", CopyTradeAction: "reduce",
		LeaderPosID: mapping.LeaderPosID, MarginMode: "cross", Leverage: 20,
	})
	if executor.order.TriggerPrice != 92 || executor.order.Quantity != .5 {
		t.Fatalf("reduction changed fixed price or missed quantity shrink: %+v", executor.order)
	}

	// Liquidation safety may tighten once. When the liquidation line later
	// recovers, both the durable audit and exchange order must retain 95.2.
	executor.setPosition(103, .5, 20, 100, 95)
	ti.refreshStopLossAfterExecute(&decision.Decision{
		Symbol: "ETHUSDT", Action: "open_long", CopyTradeAction: "add",
		LeaderPosID: mapping.LeaderPosID, MarginMode: "cross", Leverage: 20,
	})
	if math.Abs(executor.order.TriggerPrice-95.2) > 1e-9 {
		t.Fatalf("liquidation safety did not tighten stop: %+v", executor.order)
	}
	executor.setPosition(103, .5, 20, 100, 80)
	ti.refreshStopLossAfterExecute(&decision.Decision{
		Symbol: "ETHUSDT", Action: "open_long", CopyTradeAction: "add",
		LeaderPosID: mapping.LeaderPosID, MarginMode: "cross", Leverage: 20,
	})
	if math.Abs(executor.order.TriggerPrice-95.2) > 1e-9 {
		t.Fatalf("recovered liquidation line widened stop: %+v", executor.order)
	}
	finalStop, err := st.CopyTrade().GetCopyGuardAttemptFinalStop(cycle.ID, 0)
	if err != nil || math.Abs(finalStop-95.2) > 1e-9 {
		t.Fatalf("durable one-way stop=%v err=%v, want 95.2", finalStop, err)
	}
	attempts, err = st.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil || attempts[0].StopAnchorPrice != 92 || attempts[0].StopAnchorEntryPrice != 100 {
		t.Fatalf("position lifecycle overwrote immutable anchor: %+v err=%v", attempts, err)
	}
}
