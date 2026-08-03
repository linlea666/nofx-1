package copytrade

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

// ============================================================================
// mocks
// ============================================================================

type mockStopMgr struct {
	byID      map[string]*trader.ProtectiveStopOrder
	byClient  map[string]*trader.ProtectiveStopOrder
	idErr     error
	clientErr error
	placeErr  error
	amendErr  error
	cancelErr error
	placed    []trader.ProtectiveStopRequest
	amended   []string
	canceled  []string
}

type settlingStopMgr struct {
	mockStopMgr
	calls int
}

func (m *settlingStopMgr) GetProtectiveStop(_, symbol string) (*trader.ProtectiveStopOrder, error) {
	m.calls++
	quantity := 0.5
	trigger := 98.0
	if m.calls > 1 {
		quantity = 1
		trigger = 97
	}
	return &trader.ProtectiveStopOrder{
		AlgoID: "algo", Symbol: symbol, PositionSide: "long", MarginMode: "cross",
		Quantity: quantity, TriggerPrice: trigger, State: "live",
	}, nil
}

func notFoundErr(what string) error {
	return fmt.Errorf("%s: %w", what, trader.ErrProtectiveStopNotFound)
}

func (m *mockStopMgr) PlaceProtectiveStop(req trader.ProtectiveStopRequest) (*trader.ProtectiveStopOrder, error) {
	m.placed = append(m.placed, req)
	if m.placeErr != nil {
		return nil, m.placeErr
	}
	return &trader.ProtectiveStopOrder{AlgoID: "new-algo", ClientID: req.ClientID, State: "live"}, nil
}
func (m *mockStopMgr) AmendProtectiveStop(algoID string, req trader.ProtectiveStopRequest) error {
	m.amended = append(m.amended, algoID)
	return m.amendErr
}
func (m *mockStopMgr) GetProtectiveStop(algoID, symbol string) (*trader.ProtectiveStopOrder, error) {
	if m.idErr != nil {
		return nil, m.idErr
	}
	if o, ok := m.byID[algoID]; ok {
		return o, nil
	}
	return nil, notFoundErr("algo " + algoID)
}
func (m *mockStopMgr) GetProtectiveStopByClientID(clientID, symbol string) (*trader.ProtectiveStopOrder, error) {
	if m.clientErr != nil {
		return nil, m.clientErr
	}
	if o, ok := m.byClient[clientID]; ok {
		return o, nil
	}
	return nil, notFoundErr("client " + clientID)
}
func (m *mockStopMgr) CancelProtectiveStop(algoID, symbol string) error {
	m.canceled = append(m.canceled, algoID)
	return m.cancelErr
}

// flatExecutor: 仓位为空的最小执行器
type flatExecutor struct{}

func (flatExecutor) ExecuteDecision(*decision.Decision) error { return nil }
func (flatExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
func (flatExecutor) GetPositions() ([]map[string]interface{}, error) { return nil, nil }

// posIDExecutor: 支持按 posId 精确查询的对账执行器
type posIDExecutor struct {
	accountingV4Executor
	posIDRecords  []trader.ClosedPnLRecord
	posIDQueried  bool
	lastQueriedID string
}

func (e *posIDExecutor) GetClosedPnLByPositionID(symbol, posID string, limit int) ([]trader.ClosedPnLRecord, error) {
	e.posIDQueried = true
	e.lastQueriedID = posID
	return e.posIDRecords, nil
}

func newTestCopyGuardCycle(t *testing.T, st *store.Store, traderID string) *store.CopyGuardCycle {
	t.Helper()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: traderID, LeaderID: "leader", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}", FollowerEntryPrice: 1717.33, FollowerNotional: 110})
	if err != nil {
		t.Fatal(err)
	}
	return cycle
}

// ============================================================================
// 强平价方向合理性（cycle 10 根因）
// ============================================================================

func TestLiquidationPriceDirectionValidation(t *testing.T) {
	// cycle 10 实况：多单 entry=1717.33，OKX 返回"强平价"2630.40（方向不合理）
	if isLiquidationPriceDirectionValid(SideLong, 1717.33, 2630.40) {
		t.Fatal("long liquidation above entry must be rejected")
	}
	if !isLiquidationPriceDirectionValid(SideLong, 1717.33, 1500) {
		t.Fatal("long liquidation below entry is valid")
	}
	if isLiquidationPriceDirectionValid(SideShort, 1717.33, 1500) {
		t.Fatal("short liquidation below entry must be rejected")
	}
	if !isLiquidationPriceDirectionValid(SideShort, 1717.33, 2000) {
		t.Fatal("short liquidation above entry is valid")
	}
	if isLiquidationPriceDirectionValid(SideLong, 1717.33, 0) {
		t.Fatal("zero liquidation price is not usable")
	}
}

func TestFinalizeStopLossIgnoresImplausibleLiquidation(t *testing.T) {
	input := &StopLossCalcInput{Symbol: "ETHUSDT", Side: SideLong, EntryPrice: 1717.33, PositionValue: 110, FollowerEquity: 22.14, LiquidationPrice: 2630.40}
	result := &StopLossCalcResult{SLPrice: 1711.63, SLDistance: 1717.33 - 1711.63, ATRValue: 7.59}
	out, err := finalizeStopLossPrice(input, result, 0.5, 0)
	if err != nil {
		t.Fatalf("implausible liquidation must not block the ATR stop: %v", err)
	}
	if !out.LiquidationPriceIgnored {
		t.Fatal("the ignored liquidation price must be flagged for diagnostics")
	}
	if out.SLPrice <= 0 {
		t.Fatalf("stop price must be preserved, got %v", out.SLPrice)
	}
	// 方向合理时：多单止损价落入强平缓冲区 → v5 不再拒单，而是钳到强平
	// 安全线上（Clamped），保护真实存在。
	// buffer = min(0.5×ATR=3.795, 0.15%×entry≈2.576) ≈ 2.58 → 安全线 ≈ 1712.6，
	// 原 SL 1711.63 落在线下 → 钳到 1712.6 附近
	input.LiquidationPrice = 1710
	out, err = finalizeStopLossPrice(input, &StopLossCalcResult{SLPrice: 1711.63, SLDistance: 5.7, ATRValue: 7.59}, 0.5, 0)
	if err != nil {
		t.Fatalf("stop inside the liquidation buffer must clamp, not error: %v", err)
	}
	if !out.Clamped || out.SLPrice <= 1710 || out.SLPrice >= input.EntryPrice || out.GovernedBy != "clamp" {
		t.Fatalf("stop must be clamped above the liquidation safety line: %+v", out)
	}
}

// v5 不可保护：clamp 后距离 < 0.1%（强平价紧贴入场价）→ Unprotectable，
// 调用方必须走 GUARD_UNPROTECTABLE 处置
func TestFinalizeStopLossUnprotectableWhenClampTooTight(t *testing.T) {
	input := &StopLossCalcInput{Symbol: "ETHUSDT", Side: SideLong, EntryPrice: 1717.33, PositionValue: 110, FollowerEquity: 22.14, LiquidationPrice: 1716.9}
	out, err := finalizeStopLossPrice(input, &StopLossCalcResult{SLPrice: 1711.63, SLDistance: 5.7, ATRValue: 7.59}, 0.5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Unprotectable || out.SLPrice != 0 {
		t.Fatalf("clamped distance below 0.1%% must be flagged unprotectable: %+v", out)
	}
}

// ============================================================================
// 保护单实态解析与 51068 接管
// ============================================================================

func TestResolveProtectiveOrderContract(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "resolve.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ti := NewTraderIntegration("trader-1", flatExecutor{}, st)

	live := &trader.ProtectiveStopOrder{AlgoID: "a1", ClientID: "cg1a0", State: "live"}
	mgr := &mockStopMgr{byID: map[string]*trader.ProtectiveStopOrder{"a1": live}}
	if got, err := ti.resolveProtectiveOrder(mgr, "a1", "cg1a0", "ETHUSDT"); err != nil || got != live {
		t.Fatalf("existing order must be returned: %v %v", got, err)
	}
	// algoId 查不到但 clientId 命中 → 返回订单（接管场景）
	mgr = &mockStopMgr{byClient: map[string]*trader.ProtectiveStopOrder{"cg1a0": live}}
	if got, err := ti.resolveProtectiveOrder(mgr, "a1", "cg1a0", "ETHUSDT"); err != nil || got != live {
		t.Fatalf("client id fallback must be used: %v %v", got, err)
	}
	// 两条路径都确认不存在 → (nil, nil)
	mgr = &mockStopMgr{}
	if got, err := ti.resolveProtectiveOrder(mgr, "a1", "cg1a0", "ETHUSDT"); err != nil || got != nil {
		t.Fatalf("confirmed absence must be (nil,nil): %v %v", got, err)
	}
	// 查询失败 → 必须返回错误，禁止误判为不存在（cycle 10 死循环根因）
	mgr = &mockStopMgr{idErr: errors.New("network down"), clientErr: errors.New("network down")}
	if _, err := ti.resolveProtectiveOrder(mgr, "a1", "cg1a0", "ETHUSDT"); err == nil {
		t.Fatal("query failure must surface as an error, not absence")
	}
}

func TestAdoptProtectiveOrderByClientID(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "adopt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ti := NewTraderIntegration("trader-1", flatExecutor{}, st)
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	req := trader.ProtectiveStopRequest{Symbol: "ETHUSDT", PositionSide: "long", MarginMode: "cross", Quantity: 0.064, TriggerPrice: 1711.63, TriggerType: "mark", ClientID: fmt.Sprintf("cg%da0", cycle.ID)}

	// 已存在 live 订单且价格/数量不一致 → 接管 + amend
	existing := &trader.ProtectiveStopOrder{AlgoID: "okx-1", ClientID: req.ClientID, Quantity: 0.05, TriggerPrice: 1700, State: "live"}
	mgr := &mockStopMgr{byClient: map[string]*trader.ProtectiveStopOrder{req.ClientID: existing}}
	adopted, err := ti.adoptProtectiveOrderByClientID(mgr, cycle, req, 0.01, 0.001)
	if err != nil || adopted == nil || adopted.AlgoID != "okx-1" {
		t.Fatalf("live conflicting order must be adopted: %v %v", adopted, err)
	}
	if len(mgr.amended) != 1 || mgr.amended[0] != "okx-1" {
		t.Fatalf("diverging trigger/quantity must be amended: %v", mgr.amended)
	}
	stored, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil || stored.AlgoID != "okx-1" || stored.Status != "live" {
		t.Fatalf("adoption must take over local bookkeeping: %+v %v", stored, err)
	}

	// 已触发的订单 → 接管记录但不 amend，由轮询记录止损
	fired := &trader.ProtectiveStopOrder{AlgoID: "okx-2", ClientID: req.ClientID, Quantity: 0.064, TriggerPrice: 1711.63, State: "effective"}
	mgr = &mockStopMgr{byClient: map[string]*trader.ProtectiveStopOrder{req.ClientID: fired}}
	adopted, err = ti.adoptProtectiveOrderByClientID(mgr, cycle, req, 0.01, 0.001)
	if err != nil || adopted == nil || !isProtectiveStopFired(adopted.State) {
		t.Fatalf("fired order must be adopted as-is: %v %v", adopted, err)
	}
	if len(mgr.amended) != 0 {
		t.Fatal("a fired order must not be amended")
	}

	// 终态订单（已撤销）→ 返回 (nil, nil)，调用方换新 clientID 重挂
	canceledOrder := &trader.ProtectiveStopOrder{AlgoID: "okx-3", ClientID: req.ClientID, State: "canceled"}
	mgr = &mockStopMgr{byClient: map[string]*trader.ProtectiveStopOrder{req.ClientID: canceledOrder}}
	adopted, err = ti.adoptProtectiveOrderByClientID(mgr, cycle, req, 0.01, 0.001)
	if err != nil || adopted != nil {
		t.Fatalf("terminal order cannot be adopted: %v %v", adopted, err)
	}
}

// ============================================================================
// 51400 终态处理
// ============================================================================

func TestCancelProtectiveOrderTerminal51400(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ti := NewTraderIntegration("trader-1", flatExecutor{}, st)
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	order := &store.CopyGuardProtectiveOrder{CycleID: cycle.ID, TraderID: "trader-1", AlgoID: "okx-1", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 0.064, TriggerPrice: 1711.63, TriggerType: "mark", Status: "live"}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(order); err != nil {
		t.Fatal(err)
	}

	// 51400 filled/canceled 且实时仓位为空 → 正常终态，不算保护异常
	mgr := &mockStopMgr{cancelErr: errors.New("OKX cancel amend failed: [{51400 Order cancellation failed as the order has been filled, canceled or does not exist}]")}
	if err := ti.cancelProtectiveOrderForCycle(mgr, cycle, order); err != nil {
		t.Fatalf("51400 with a flat position must be a normal terminal state: %v", err)
	}
	stored, _ := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if stored.Status != "canceled" {
		t.Fatalf("order must be marked canceled, got %s", stored.Status)
	}

	// 本地已是终态 → 不再调用交易所
	mgr = &mockStopMgr{cancelErr: errors.New("should not be called")}
	if err := ti.cancelProtectiveOrderForCycle(mgr, cycle, stored); err != nil {
		t.Fatalf("terminal order must be a no-op: %v", err)
	}
	if len(mgr.canceled) != 0 {
		t.Fatal("terminal order must not trigger an exchange cancel call")
	}

	// 非 51400 错误仍然上报
	order2 := &store.CopyGuardProtectiveOrder{CycleID: cycle.ID, TraderID: "trader-1", AlgoID: "okx-1", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 0.064, TriggerPrice: 1711.63, TriggerType: "mark", Status: "live"}
	mgr = &mockStopMgr{cancelErr: errors.New("OKX API error: code=50011 rate limited")}
	if err := ti.cancelProtectiveOrderForCycle(mgr, cycle, order2); err == nil {
		t.Fatal("non-terminal cancel failure must be surfaced")
	}
}

// ============================================================================
// 对账：posId 优先 + fallback 匹配 + DELAYED/UNRECOVERABLE 语义
// ============================================================================

func TestLookupClosedPnLPrefersPositionID(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "lookup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	executor := &posIDExecutor{}
	executor.posIDRecords = []trader.ClosedPnLRecord{{Symbol: "ETHUSDT", Side: "long", ExchangeID: "pos-1", ExitPrice: 1746.73, RealizedPnL: 1.61, ExitTime: now}}
	executor.records = []trader.ClosedPnLRecord{{Symbol: "ETHUSDT", Side: "long", ExchangeID: "other", ExitPrice: 1, ExitTime: now}}
	ti := NewTraderIntegration("trader-1", executor, st)
	got, err := ti.lookupClosedPnLRecord("ETHUSDT", "long", "cross", "pos-1", now.Add(-time.Hour), nil, 0)
	if err != nil || got == nil || got.ExchangeID != "pos-1" {
		t.Fatalf("posId query must win: %v %v", got, err)
	}
	if !executor.posIDQueried || executor.lastQueriedID != "pos-1" {
		t.Fatal("precise posId endpoint must be used when a posId is known")
	}
}

func TestLookupClosedPnLFallbackTolerances(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	executor := &accountingV4Executor{records: []trader.ClosedPnLRecord{
		{Symbol: "ETHUSDT", Side: "long", MarginMode: "isolated", Quantity: 0.064, ExitTime: now, RealizedPnL: -5},  // 保证金模式不符
		{Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 0.5, ExitTime: now, RealizedPnL: -7},       // 数量超容差
		{Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 0.064, ExitTime: now, RealizedPnL: 1.61},   // 正确
		{Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Quantity: 0.064, ExitTime: now, RealizedPnL: 99},     // 币种不符
		{Symbol: "ETHUSDT", Side: "short", MarginMode: "cross", Quantity: 0.064, ExitTime: now, RealizedPnL: -3.14}, // 方向不符
	}}
	ti := NewTraderIntegration("trader-1", executor, st)
	got, err := ti.lookupClosedPnLRecord("ETHUSDT", "long", "cross", "", now.Add(-time.Hour), &now, 0.064)
	if err != nil || got == nil || got.RealizedPnL != 1.61 {
		t.Fatalf("fallback matching must respect margin mode and quantity tolerance: %+v %v", got, err)
	}
}

func TestAccountingDelayedThenUnrecoverable(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "delayed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 1717.33, 110, 0.064, 7.59); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().UpdateCopyGuardFollowerPosition(cycle.ID, "follower-pos", 1717.33, 110); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().BeginCopyGuardAccounting(cycle.ID, store.CopyGuardLeaderClosed, "exit-order", 0); err != nil {
		t.Fatal(err)
	}
	executor := &accountingV4Executor{status: "FILLED"} // 历史记录尚未返回
	ti := NewTraderIntegration("trader-1", executor, st)

	// 刚平仓（<10 分钟）：保持 PENDING，自动重试
	pending, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	ti.reconcileV4CycleAccounting(pending)
	pending, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if pending.AccountingStatus != store.CopyGuardAccountingPending {
		t.Fatalf("fresh close must stay PENDING, got %s", pending.AccountingStatus)
	}

	// 超过 10 分钟：DELAYED（自动重试继续，不再是"需人工核对"）
	old := time.Now().Add(-20 * time.Minute)
	pending.ClosedAt = &old
	ti.reconcileV4CycleAccounting(pending)
	delayed, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if delayed.AccountingStatus != store.CopyGuardAccountingDelayed {
		t.Fatalf("late data must be DELAYED, got %s", delayed.AccountingStatus)
	}
	list, err := st.CopyTrade().ListCopyGuardCyclesPendingAccounting("trader-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("DELAYED cycles must stay in the automatic retry queue: %v %v", list, err)
	}

	// DELAYED 状态下数据补全后仍能对账
	executor.records = []trader.ClosedPnLRecord{{Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", ExchangeID: "follower-pos", ExitPrice: 1746.73, RealizedPnL: 1.61, Fee: 0.29, ExitTime: time.Now()}}
	delayed.ClosedAt = &old
	ti.reconcileV4CycleAccounting(delayed)
	settled, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if settled.AccountingStatus != store.CopyGuardAccountingReconciled || settled.ActualPnL != 1.61 {
		t.Fatalf("DELAYED cycle must reconcile once data arrives: %+v", settled)
	}
}

func TestAccountingUnrecoverableLeavesRetryQueue(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "unrecoverable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().BeginCopyGuardAccounting(cycle.ID, store.CopyGuardLeaderClosed, "", 0); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration("trader-1", &accountingV4Executor{status: "FILLED"}, st)
	stale, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	old := time.Now().Add(-25 * time.Hour)
	stale.ClosedAt = &old
	// posId 与 exit order 均缺失且超过 24 小时 → UNRECOVERABLE
	ti.reconcileV4CycleAccounting(stale)
	got, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if got.AccountingStatus != store.CopyGuardAccountingUnrecoverable {
		t.Fatalf("permanently missing identifiers must be UNRECOVERABLE, got %s", got.AccountingStatus)
	}
	list, err := st.CopyTrade().ListCopyGuardCyclesPendingAccounting("trader-1")
	if err != nil || len(list) != 0 {
		t.Fatalf("UNRECOVERABLE cycles must leave the retry queue: %v %v", list, err)
	}
}

// ============================================================================
// 领航员公共历史仓位
// ============================================================================

func TestConvertOKXPositionHistory(t *testing.T) {
	rows := []OKXPositionHistoryRawEntry{
		{PosId: "p-old", InstId: "ETH-USDT-SWAP", PosSide: "long", MgnMode: "cross", Lever: "10", OpenAvgPx: "1700", CloseAvgPx: "1650", CloseAmount: "5", CtVal: "0.1", Pnl: "-25", RealizedPnl: "-26", Fee: "-0.5", FundingFee: "-0.5", CloseType: "2", CTime: "1751500000000", UTime: "1751530000000"},
		{PosId: "p-new", InstId: "JTO-USDT-SWAP", PosSide: "long", MgnMode: "cross", Lever: "5", OpenAvgPx: "0.7766", CloseAvgPx: "0.80", CloseAmount: "27", CtVal: "1", Pnl: "0.63", RealizedPnl: "0.6", Fee: "-0.02", FundingFee: "-0.01", CloseType: "1", CTime: "1751600000000", UTime: "1751660000000"},
	}
	out := convertOKXPositionHistory(rows)
	if len(out) != 2 {
		t.Fatalf("expected 2 records, got %d", len(out))
	}
	// 本地按 uTime 倒序，防接口排序漂移
	if out[0].PosID != "p-new" || out[1].PosID != "p-old" {
		t.Fatalf("records must be sorted by uTime desc locally: %v %v", out[0].PosID, out[1].PosID)
	}
	if out[1].Symbol != "ETHUSDT" || out[1].Quantity != 0.5 || out[1].ExitPrice != 1650 {
		t.Fatalf("normalization failed: %+v", out[1])
	}
	if out[0].Quantity != 27 {
		t.Fatalf("ctVal=1 quantity must stay 27, got %v", out[0].Quantity)
	}
}

func TestMatchLeaderHistoryRecord(t *testing.T) {
	records := []OKXLeaderPositionHistoryRecord{
		{PosID: "p-1", Symbol: "ETHUSDT", Side: "long", ExitPrice: 1650},
		{PosID: "p-2", Symbol: "ETHUSDT", Side: "short", ExitPrice: 1600},
		{PosID: "p-3", Symbol: "BTCUSDT", Side: "net", ExitPrice: 50000},
	}
	if got := matchLeaderHistoryRecord(records, "p-1", "ETHUSDT", "long"); got == nil || got.ExitPrice != 1650 {
		t.Fatalf("posId+symbol+side match failed: %+v", got)
	}
	if got := matchLeaderHistoryRecord(records, "p-2", "ETHUSDT", "long"); got != nil {
		t.Fatal("direction mismatch must not match")
	}
	// 单向模式 posSide=net 时只按 posId+symbol 匹配
	if got := matchLeaderHistoryRecord(records, "p-3", "BTCUSDT", "long"); got == nil {
		t.Fatal("net posSide must match by posId+symbol")
	}
	if got := matchLeaderHistoryRecord(records, "missing", "ETHUSDT", "long"); got != nil {
		t.Fatal("unknown posId must not match")
	}
}

func TestProtectiveVerificationWaitsForAcknowledgedTargetToSettle(t *testing.T) {
	mgr := &settlingStopMgr{}
	ti := &TraderIntegration{}
	got, err := ti.verifyProtectiveStopWithGrace(mgr, "algo", "ETHUSDT", "long", "cross", 1, 97, 0.1, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	if mgr.calls < 2 {
		t.Fatal("successful but stale exchange state must be retried")
	}
	if got.Quantity != 1 || got.TriggerPrice != 97 {
		t.Fatalf("verification returned stale target: %+v", got)
	}
}
