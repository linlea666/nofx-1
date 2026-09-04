package copytrade

import (
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

type positionMarginLifecycleExecutor struct {
	positions    []map[string]interface{}
	marketPrice  float64
	order        *trader.ProtectiveStopOrder
	requests     []trader.ProtectiveStopRequest
	coverageMode string
	amendErr     error
	closeCalls   int
	freshCalls   int
	freshErr     error
}

type positionMarginShadowMarkExecutor struct {
	*positionMarginLifecycleExecutor
	authoritativeMark float64
	markCalls         int
}

func (e *positionMarginShadowMarkExecutor) GetMarkPrice(string) (float64, error) {
	e.markCalls++
	return e.authoritativeMark, nil
}

func (e *positionMarginLifecycleExecutor) ExecuteDecision(*decision.Decision) error { return nil }
func (e *positionMarginLifecycleExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{"total_equity": 1000.0}, nil
}
func (e *positionMarginLifecycleExecutor) GetPositions() ([]map[string]interface{}, error) {
	return e.positions, nil
}
func (e *positionMarginLifecycleExecutor) GetPositionsFresh() ([]map[string]interface{}, error) {
	e.freshCalls++
	return e.positions, e.freshErr
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
func (e *positionMarginLifecycleExecutor) ProtectiveStopCoverageMode() string {
	if e.coverageMode == "" {
		return trader.ProtectiveStopCoverageExactQuantity
	}
	return e.coverageMode
}
func (e *positionMarginLifecycleExecutor) PlaceProtectiveStop(req trader.ProtectiveStopRequest) (*trader.ProtectiveStopOrder, error) {
	e.requests = append(e.requests, req)
	e.order = &trader.ProtectiveStopOrder{
		AlgoID: "fixed-stop", ClientID: req.ClientID, Symbol: req.Symbol,
		PositionSide: req.PositionSide, MarginMode: req.MarginMode,
		Quantity: req.Quantity, TriggerPrice: req.TriggerPrice, TriggerType: req.TriggerType,
		CoverageMode: req.CoverageMode, State: "live",
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
	e.order.TriggerType, e.order.CoverageMode = req.TriggerType, req.CoverageMode
	return nil
}

func TestBinanceCloseAllProtectionDoesNotReplaceOnQuantityChange(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "close-all-protection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const traderID = "binance-close-all"
	policy := store.NewCopyGuardDefaults()
	policy.TraderID = traderID
	policy.ProviderType = string(ProviderBinance)
	policy.LeaderID = "leader"
	policy.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	policy.RiskPositionMarginStopPct = .80
	policy.RiskTriggerPriceType = "mark"
	policy.RiskReentryEnabled = false
	policy.RiskReentryDecisionMode = "disabled"
	policy.RiskManualReentryEnabled = false
	policy.RiskMaxReentries = 0
	snapshot, err := store.EncodeCopyGuardPolicySnapshot(policy)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: traderID, LeaderID: "leader", LeaderPosID: "leader-close-all",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot: snapshot, AccountEquity: 1000, FollowerEntryPrice: 100, FollowerNotional: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 100, 100, 1, 0); err != nil {
		t.Fatal(err)
	}
	cfg := &CopyConfig{
		ProviderType: ProviderBinance, LeaderID: "leader", RiskPolicyVersion: 4,
		RiskStopLossEnabled: true, RiskProtectionMode: store.RiskProtectionModePositionMarginPct,
		RiskPositionMarginStopPct: .80, RiskTriggerPriceType: "mark", RiskReentryDecisionMode: "disabled",
	}
	cfg.FillRiskDefaults()
	executor := &positionMarginLifecycleExecutor{coverageMode: trader.ProtectiveStopCoverageCloseAll}
	executor.setPosition(100, 1, 10, 100, 80)
	ti := NewTraderIntegration(traderID, executor, st)
	ti.engine = &Engine{traderID: traderID, config: cfg, store: st}
	dec := &decision.Decision{Symbol: "ETHUSDT", Action: "open_long", CopyTradeAction: "open", LeaderPosID: cycle.LeaderPosID, MarginMode: "cross", Leverage: 10}
	result := &StopLossCalcResult{SLPrice: 92, TickSize: .1, QuantityStep: .01, ExpectedLossUSD: 8, ExpectedMarginLossPct: .80, GovernedBy: "position_margin_pct"}

	ti.upsertV4Protection(dec, "long", 1, 100, result)
	if len(executor.requests) != 1 || executor.requests[0].CoverageMode != trader.ProtectiveStopCoverageCloseAll {
		t.Fatalf("initial close-all stop was not placed exactly once: %+v", executor.requests)
	}
	executor.setPosition(102, 2, 10, 102, 80)
	ti.upsertV4Protection(dec, "long", 2, 102, result)
	if len(executor.requests) != 1 {
		t.Fatalf("quantity-only change replaced Binance close-all stop: requests=%+v", executor.requests)
	}
	stored, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CoverageMode != store.CopyGuardCoverageCloseAll || stored.Quantity != 2 || stored.AlgoID != "fixed-stop" {
		t.Fatalf("close-all audit state was not updated without replacement: %+v", stored)
	}
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

func TestPositionMarginShadowPrefersDedicatedMarkOverCachedPositionMark(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "shadow-dedicated-mark.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const traderID = "shadow-dedicated-mark"
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: traderID, LeaderID: "leader", LeaderPosID: "leader-shadow-mark",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot: "{}", FollowerEntryPrice: 100, FollowerNotional: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	shadow := &store.CopyGuardPositionMarginShadow{
		CycleID: cycle.ID, TraderID: traderID, Side: "long",
		AnchorEntryPrice: 100, AnchorLeverage: 10, AnchorInitialMargin: 10,
		AnchorStopPrice: 92, ConfiguredMarginLossPct: .8,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 10,
		EffectiveStopPrice: 92, LastLeaderSize: 1,
	}
	if _, _, err = st.CopyTrade().InitializeCopyGuardPositionMarginShadow(shadow); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CopyTrade().InitializeCopyGuardPositionMarginShadowV2(&store.CopyGuardPositionMarginShadowV2{
		CycleID: cycle.ID, TraderID: traderID, Side: "long",
		AnchorEntryPrice: 100, AnchorLeverage: 10, AnchorInitialMargin: 10,
		AnchorStopPrice: 92, ConfiguredMarginLossPct: .8,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 10,
		EffectiveStopPrice: 92, LastLeaderSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	baseExecutor := &positionMarginLifecycleExecutor{}
	// The ordinary account snapshot still says 100, while the dedicated mark
	// endpoint has already crossed the fixed long stop at 92.
	baseExecutor.setPosition(100, 1, 10, 100, 80)
	executor := &positionMarginShadowMarkExecutor{
		positionMarginLifecycleExecutor: baseExecutor,
		authoritativeMark:               91,
	}
	cfg := &CopyConfig{RiskLiquidationBufferATR: .5}
	ti := NewTraderIntegration(traderID, executor, st)
	ti.engine = &Engine{traderID: traderID, config: cfg, store: st}

	ti.observePositionMarginStopShadows()

	observed, err := st.CopyTrade().GetCopyGuardPositionMarginShadow(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Status != store.CopyGuardPositionMarginShadowCrossed || observed.CrossedPrice != 91 {
		t.Fatalf("cached position mark hid dedicated-mark crossing: %+v", observed)
	}
	observedV2, err := st.CopyTrade().GetCopyGuardPositionMarginShadowV2(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observedV2.Status != store.CopyGuardPositionMarginShadowCrossed || observedV2.CrossingSource != "LIVE_MARK" {
		t.Fatalf("v2 ledger did not record dedicated mark crossing: %+v", observedV2)
	}
	if executor.markCalls != 1 {
		t.Fatalf("dedicated mark must be queried once per symbol and pass, calls=%d", executor.markCalls)
	}
}

func TestFixedPositionMarginLifecycleKeepsPriceAndSynchronizesOnlyQuantity(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "fixed-stop-lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const traderID = "fixed-stop-trader"
	const leaderPosID = "leader-position"
	policy := store.NewCopyGuardDefaults()
	policy.TraderID, policy.ProviderType, policy.LeaderID = traderID, string(ProviderOKX), "leader"
	policy.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	policy.RiskPositionMarginStopPct = .80
	policy.RiskLiquidationBufferATR = .5
	policy.RiskTriggerPriceType = "mark"
	policy.RiskReentryEnabled = false
	policy.RiskReentryDecisionMode = "disabled"
	policy.RiskManualReentryEnabled = false
	policy.RiskMaxReentries = 0
	policySnapshot, err := store.EncodeCopyGuardPolicySnapshot(policy)
	if err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: traderID, LeaderPosID: leaderPosID,
		SourceRevision: 1, SourceKind: "LEADER_TRANSITION", CanonicalKey: "leader|fixed-stop-trader|leader-position|1",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 1, RequestedQuantity: 1, QuantizedQuantity: 1, ClientOrderID: "initial-open",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve initial intent: claimed=%v err=%v", claimed, err)
	}
	if err = st.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentSubmitted, "", "", "initial-order", 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().CommitLeaderExecutionFill(store.LeaderExecutionCommit{
		IntentID: intent.ID, TraderID: traderID, LeaderID: "leader", LeaderPosID: leaderPosID,
		SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", SourceSymbol: "ETHUSDT",
		ExecutionSymbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 1,
		FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: "initial-open",
		ExchangeOrderID: "initial-order", ExchangeState: "FILLED", OrderTerminal: true,
		InitialCopyGuard: &store.InitialCopyGuardLifecycle{
			PolicySnapshot: policySnapshot, LeaderEntryPrice: 100, AccountEquity: 1000,
		},
	}); err != nil {
		t.Fatal(err)
	}
	mapping, err := st.CopyTrade().GetMapping(traderID, leaderPosID)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().GetOpenCopyGuardCycle(traderID, leaderPosID)
	if err != nil {
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
		ExecutionIntentID: intent.ID,
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

	// Venue liquidation/leverage facts can change without a leader signal. The
	// periodic safety sweep must use one fresh snapshot, tighten through the same
	// planner, and remain throttled between 15-second checkpoints.
	executor.setPosition(103, .5, 20, 100, 96)
	freshCallsBefore := executor.freshCalls
	ti.lastPositionMarginSafetyRecheck = time.Time{}
	ti.recheckPositionMarginSafety()
	if executor.freshCalls != freshCallsBefore+1 || math.Abs(executor.order.TriggerPrice-96.2) > 1e-9 {
		t.Fatalf("periodic safety sweep did not tighten from one fresh snapshot: calls=%d order=%+v", executor.freshCalls-freshCallsBefore, executor.order)
	}
	executor.setPosition(103, .5, 20, 100, 97)
	ti.recheckPositionMarginSafety()
	if executor.freshCalls != freshCallsBefore+1 || math.Abs(executor.order.TriggerPrice-96.2) > 1e-9 {
		t.Fatalf("periodic safety sweep ignored 15-second throttle: calls=%d order=%+v", executor.freshCalls-freshCallsBefore, executor.order)
	}
	ti.lastPositionMarginSafetyRecheck = time.Now().Add(-positionMarginSafetyRecheckInterval)
	ti.recheckPositionMarginSafety()
	if executor.freshCalls != freshCallsBefore+2 || math.Abs(executor.order.TriggerPrice-97.2) > 1e-9 {
		t.Fatalf("due periodic safety sweep did not apply the next one-way clamp: calls=%d order=%+v", executor.freshCalls-freshCallsBefore, executor.order)
	}

	// A missing authoritative mark disables only local crossing. Even when the
	// last-trade endpoint would report a crossed value, the hosted MARK_PRICE
	// stop remains armed and no emergency market close is submitted.
	executor.setPosition(103, .5, 20, 0, 80)
	executor.marketPrice = 50
	closeCallsBefore := executor.closeCalls
	ti.lastPositionMarginSafetyRecheck = time.Now().Add(-positionMarginSafetyRecheckInterval)
	ti.recheckPositionMarginSafety()
	if executor.closeCalls != closeCallsBefore || math.Abs(executor.order.TriggerPrice-97.2) > 1e-9 {
		t.Fatalf("last price substituted for missing mark or widened protection: close=%d order=%+v", executor.closeCalls-closeCallsBefore, executor.order)
	}
	attempts, err = st.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil || attempts[0].StopAnchorPrice != 92 || attempts[0].StopAnchorEntryPrice != 100 {
		t.Fatalf("position lifecycle overwrote immutable anchor: %+v err=%v", attempts, err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET updated_at=datetime('now','-1 minute') WHERE id=?`, intent.ID); err != nil {
		t.Fatal(err)
	}
	ti.auditCopyGuardPositionOwnership(cycle, 0, .01)
	cycle, err = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil || cycle.AccountingStatus != store.CopyGuardAccountingUnscorable {
		t.Fatalf("unexplained exchange quantity remained scoreable: cycle=%+v err=%v", cycle, err)
	}
	events, err := st.CopyTrade().ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundOwnershipAnomaly := false
	for _, event := range events {
		if event.Type == "POSITION_OWNERSHIP_ANOMALY" {
			foundOwnershipAnomaly = true
			break
		}
	}
	if !foundOwnershipAnomaly {
		t.Fatal("unexplained exchange quantity did not emit POSITION_OWNERSHIP_ANOMALY")
	}
}

func TestPositionMarginShadowFinalizationFlushesLifecycleCheckpoint(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "shadow-finalize-flush.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "shadow-finalize", LeaderID: "leader", LeaderPosID: "shadow-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CopyTrade().InitializeCopyGuardPositionMarginShadowV2(&store.CopyGuardPositionMarginShadowV2{
		CycleID: cycle.ID, TraderID: cycle.TraderID, Side: "long",
		AnchorEntryPrice: 100, AnchorLeverage: 10, AnchorInitialMargin: 10,
		AnchorStopPrice: 92, ConfiguredMarginLossPct: .8,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 10,
		EffectiveStopPrice: 92, LastLeaderSize: 1, ConfiguredCostBPS: 10,
	}); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration(cycle.TraderID, &positionMarginLifecycleExecutor{}, st)
	now := time.Now().UTC()
	ti.shadowMarks[cycle.ID] = &shadowMarkAccumulator{
		bucketAt: now.Truncate(time.Minute), firstObservedAt: now.Add(-9 * time.Second),
		lastObservedAt: now, minimumMark: 91, maximumMark: 101, lastMark: 93,
		observationCount: 4, coveredSeconds: 9,
	}
	if err = ti.finalizeCopyGuardPositionMarginShadow(cycle.ID, 105); err != nil {
		t.Fatal(err)
	}
	shadow, err := st.CopyTrade().GetCopyGuardPositionMarginShadowV2(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.Status != store.CopyGuardPositionMarginShadowFinalized || shadow.MarkObservationCount != 4 ||
		shadow.MinimumMark != 91 || shadow.MaximumMark != 101 || shadow.CrossingSource != "LIVE_MARK" {
		t.Fatalf("lifecycle close lost the in-memory mark checkpoint: %+v", shadow)
	}
	evaluations, err := st.CopyTrade().ListCopyGuardShadowEvaluations(cycle.ID)
	if err != nil || len(evaluations) != 1 || !evaluations[0].StopCrossed || !evaluations[0].CrossingVerified {
		t.Fatalf("flushed crossing was not finalized as verified: evaluations=%+v err=%v", evaluations, err)
	}
}

func TestPositionMarginShadowCheckpointRetainsCoverageClock(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "shadow-checkpoint-clock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "shadow-clock", LeaderID: "leader", LeaderPosID: "clock-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CopyTrade().InitializeCopyGuardPositionMarginShadowV2(&store.CopyGuardPositionMarginShadowV2{
		CycleID: cycle.ID, TraderID: cycle.TraderID, Side: "long",
		AnchorEntryPrice: 100, AnchorLeverage: 10, AnchorInitialMargin: 10,
		AnchorStopPrice: 92, ConfiguredMarginLossPct: .8,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 10,
		EffectiveStopPrice: 92, LastLeaderSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration(cycle.TraderID, &positionMarginLifecycleExecutor{}, st)
	last := time.Now().UTC().Add(-3 * time.Second)
	acc := &shadowMarkAccumulator{
		bucketAt: last.Truncate(time.Minute), firstObservedAt: last.Add(-9 * time.Second),
		lastObservedAt: last, minimumMark: 99, maximumMark: 101, lastMark: 100,
		observationCount: 4, coveredSeconds: 9,
	}
	ti.shadowMarks[cycle.ID] = acc
	ti.shadowMarkMu.Lock()
	_, err = ti.persistPositionMarginShadowAccumulatorLocked(cycle.ID, 92, acc)
	ti.shadowMarkMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	continued := ti.shadowMarks[cycle.ID]
	if continued == nil || !continued.lastObservedAt.Equal(last) || continued.observationCount != 0 {
		t.Fatalf("checkpoint discarded the coverage clock: %+v", continued)
	}
	if _, err = ti.checkpointPositionMarginShadowMark(cycle.ID, 92, 100, true); err != nil {
		t.Fatal(err)
	}
	shadow, err := st.CopyTrade().GetCopyGuardPositionMarginShadowV2(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.MarkCoveredSeconds < 11 {
		t.Fatalf("interval between consecutive checkpoints was not accounted for: %+v", shadow)
	}
}
