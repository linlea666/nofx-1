package copytrade

// 第一批资金安全修复的回归测试：
//   - F2 保护单替换窗口互斥（替换退休未完成前禁止再次 Place/Ensure）
//   - S8 silent_close / 映射熔断闭合 Copy Guard cycle（撤保护单 + 记账收尾）
//   - S1 Binance 同步失败 fail-closed（本轮不消费 trade-history 成交）

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

// F2：stored 保护单处于 replacement_pending 时，upsertV4Protection 只能推进
// 退休流程，绝不能再次 Place/Ensure（Binance 无 amend，每次都会新建
// closePosition 单并把旧单挤出跟踪）。新单已 live 时健康态必须保持受保护
// （VERIFIED），不得 DEGRADED——否则重试链会再次进入 upsert 形成连环挂单。
func TestUpsertV4ProtectionOnlyRetiresWhileReplacementPending(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "replacement-upsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mock := &mockStopMgr{byID: map[string]*trader.ProtectiveStopOrder{
		"new": {AlgoID: "new", ClientID: "new-client", PositionSide: "long", MarginMode: "cross", Quantity: 0.128, TriggerPrice: 1711.63, State: "live"},
		"old": {AlgoID: "old", ClientID: "old-client", PositionSide: "long", MarginMode: "cross", Quantity: 0.128, TriggerPrice: 1710, State: "live"},
	}, cancelErr: errors.New("temporary cancel failure")}
	executor := &stopMgrExecutor{mockStopMgr: mock}
	ti := NewTraderIntegration("trader-1", executor, st)
	ti.engine = &Engine{config: &CopyConfig{ProviderType: ProviderBinance, LeaderID: "leader", RiskPolicyVersion: 4, RiskStopLossEnabled: true, RiskTriggerPriceType: "mark"}}
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{
		CycleID: cycle.ID, TraderID: "trader-1", AlgoID: "new", AlgoClientID: "new-client",
		PreviousAlgoID: "old", PreviousAlgoClientID: "old-client", ReplacementPending: true,
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 0.128,
		TriggerPrice: 1711.63, TriggerType: "mark", Status: "live",
	}); err != nil {
		t.Fatal(err)
	}

	dec := &decision.Decision{Symbol: "ETHUSDT", Action: "open_long", LeaderPosID: "leader-pos", MarginMode: "cross", EntryPrice: 1717.33}
	calc := &StopLossCalcResult{SLPrice: 1711.63, TickSize: 0.01, QuantityStep: 0.001}
	ti.upsertV4Protection(dec, "long", 0.128, 1717.33, calc)

	if len(mock.placed) != 0 || len(mock.amended) != 0 {
		t.Fatalf("pending replacement must not place/amend another stop: placed=%v amended=%v", mock.placed, mock.amended)
	}
	if len(mock.canceled) != 1 || mock.canceled[0] != "old" {
		t.Fatalf("must only retry retiring the old order: %v", mock.canceled)
	}
	stored, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil || !stored.ReplacementPending || stored.AlgoID != "new" || stored.PreviousAlgoID != "old" {
		t.Fatalf("both ids must stay durable while retirement is pending: stored=%+v err=%v", stored, err)
	}
	got, err := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProtectionStatus != store.CopyGuardProtectionVerified {
		t.Fatalf("live replacement keeps the position protected; status must not feed the degraded retry chain: %s", got.ProtectionStatus)
	}

	// 旧单在交易所侧确认终结后：退休完成并回到常规校验链路，
	// 全程不允许新建保护单。
	mock.cancelErr = nil
	mock.byID["old"].State = "canceled"
	ti.upsertV4Protection(dec, "long", 0.128, 1717.33, calc)
	if len(mock.placed) != 0 {
		t.Fatalf("completed retirement must never create another stop: %v", mock.placed)
	}
	stored, err = st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil || stored.ReplacementPending || stored.PreviousAlgoID != "" || stored.AlgoID != "new" {
		t.Fatalf("replacement must complete cleanly: stored=%+v err=%v", stored, err)
	}
}

// S8：良性 close 自愈路径必须闭合 Copy Guard 生命周期（撤保护单 + 记账收尾），
// 否则映射关闭后周期成为孤儿，遗留保护单可能在未来仓位上误触发。
func TestBenignCloseFailureFinalizesCopyGuardCycle(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "benign-close-cycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mock := &mockStopMgr{}
	executor := &stopMgrExecutor{mockStopMgr: mock}
	ti := NewTraderIntegration("trader-1", executor, st)
	ti.engine = &Engine{config: &CopyConfig{ProviderType: ProviderOKX, LeaderID: "leader", RiskPolicyVersion: 4}}
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: "trader-1", LeaderPosID: "leader-pos", LeaderID: "leader",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", OpenedAt: time.Now(),
		OpenPrice: 1717.33, OpenSizeUSD: 110, LastKnownSize: 0.128,
	}); err != nil {
		t.Fatal(err)
	}
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{
		CycleID: cycle.ID, TraderID: "trader-1", AlgoID: "stop-1", Symbol: "ETHUSDT",
		Side: "long", MarginMode: "cross", Quantity: 0.128, TriggerPrice: 1700, TriggerType: "mark", Status: "live",
	}); err != nil {
		t.Fatal(err)
	}

	dec := &decision.Decision{Action: "close_long", Symbol: "ETHUSDT", LeaderPosID: "leader-pos", EntryPrice: 1800, MarginMode: "cross"}
	ti.handleBenignCloseFailure(dec, errors.New("long position not found for ETHUSDT"))

	if len(mock.canceled) != 1 || mock.canceled[0] != "stop-1" {
		t.Fatalf("silent_close must cancel the protective stop: %v", mock.canceled)
	}
	if _, err := st.CopyTrade().GetOpenCopyGuardCycle("trader-1", "leader-pos"); err == nil {
		t.Fatal("silent_close must close the Copy Guard cycle")
	}
	if mapping, _ := st.CopyTrade().GetActiveMapping("trader-1", "leader-pos"); mapping != nil {
		t.Fatalf("mapping must be closed: %+v", mapping)
	}
}

// S8：映射熔断路径同样必须闭合 Copy Guard 生命周期，避免遗留孤儿保护单。
func TestMappingCircuitBreakFinalizesCopyGuardCycle(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "circuit-cycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mock := &mockStopMgr{}
	executor := &stopMgrExecutor{mockStopMgr: mock}
	ti := NewTraderIntegration("trader-1", executor, st)
	ti.engine = &Engine{config: &CopyConfig{ProviderType: ProviderOKX, LeaderID: "leader", RiskPolicyVersion: 4}}
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: "trader-1", LeaderPosID: "leader-pos", LeaderID: "leader",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", OpenedAt: time.Now(),
		OpenPrice: 1717.33, OpenSizeUSD: 110, LastKnownSize: 0.128,
	}); err != nil {
		t.Fatal(err)
	}
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{
		CycleID: cycle.ID, TraderID: "trader-1", AlgoID: "stop-1", Symbol: "ETHUSDT",
		Side: "long", MarginMode: "cross", Quantity: 0.128, TriggerPrice: 1700, TriggerType: "mark", Status: "live",
	}); err != nil {
		t.Fatal(err)
	}

	dec := &decision.Decision{Action: "close_long", Symbol: "ETHUSDT", LeaderPosID: "leader-pos", EntryPrice: 1800, MarginMode: "cross"}
	for i := 0; i < mappingFailureCircuitThreshold; i++ {
		ti.checkAndTripMappingCircuit(dec, errors.New("insufficient margin"))
	}

	if len(mock.canceled) != 1 || mock.canceled[0] != "stop-1" {
		t.Fatalf("circuit break must cancel the protective stop: %v", mock.canceled)
	}
	if _, err := st.CopyTrade().GetOpenCopyGuardCycle("trader-1", "leader-pos"); err == nil {
		t.Fatal("circuit break must close the Copy Guard cycle")
	}
	if mapping, _ := st.CopyTrade().GetActiveMapping("trader-1", "leader-pos"); mapping != nil {
		t.Fatalf("mapping must be closed: %+v", mapping)
	}
}

// S1：Binance 实时持仓同步失败时本轮必须 fail-closed——不消费 trade-history
// 兜底成交（不 markSeen、不产生决策），信号下轮重取。
func TestBinancePollFailsClosedWhenStateSyncFails(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.MinTradeWarn = 1
	saveActiveMapping(t, st, posID, 0.01)
	provider := &binancePollTestProvider{
		fills: []Fill{{
			ID: "delayed-history-fill", Symbol: "ETHUSDT", Side: "sell", PositionSide: SideLong,
			Action: ActionClose, Price: 2096.58, Size: 0.01, Value: 20.9658, Timestamp: time.Now(),
		}},
		stateErr: errors.New("binance temporary 5xx"),
	}
	e.provider = provider

	e.poll()

	if provider.stateCalls == 0 {
		t.Fatal("poll must attempt the realtime sync")
	}
	if e.isSeen("delayed-history-fill") {
		t.Fatal("sync failure must not consume trade-history fills (fail-closed)")
	}
	select {
	case dec := <-e.decisionCh:
		t.Fatalf("no decision may be emitted on sync failure: %+v", dec.Decisions)
	default:
	}

	// 同步恢复后同一批成交仍可被处理（信号未丢失）。
	provider.stateErr = nil
	provider.state = &AccountState{TotalEquity: 1000, Positions: map[string]*Position{}}
	e.poll()
	if !e.isSeen("delayed-history-fill") {
		t.Fatal("recovered poll must process the retained fill")
	}
}
