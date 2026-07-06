package reentryadvisor

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"nofx/store"
)

// newTestSignal 造一条完整的周期+attempts+PENDING 信号（cycle-41 场景：
// 空单 3 次止损后自动次数用尽转人工）
func newTestSignal(t *testing.T, st *store.Store, symbol string) *store.CopyGuardManualReentrySignal {
	t.Helper()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-41",
		Symbol: symbol, Side: "short", MarginMode: "cross",
		Status: "ATTEMPTS_EXHAUSTED", PolicySnapshot: `{"risk_atr_timeframe":"1h","risk_reentry_max_chase_atr":2.0,"min_trade_warn":10,"other_field":"ignored"}`,
		LeaderEntryPrice: 3350, FollowerEntryPrice: 3340, FollowerNotional: 41.8,
		BaselineLeaderSize: 2, AccountEquity: 102, ATRAtEntry: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.OpenCopyGuardAttempt(cycle.ID, 0, 3340, 41.8, 0.0125, 10); err != nil {
		t.Fatal(err)
	}
	// 空单止损在上方 3360
	if err := cs.RecordCopyGuardStop(cycle, 12, 3360, -0.25, 0.02, 0, "algo-0", map[string]interface{}{"quantity": 0.0125}); err != nil {
		t.Fatal(err)
	}
	sig, err := cs.SaveManualReentrySignal(&store.CopyGuardManualReentrySignal{
		CycleID: cycle.ID, TraderID: "trader-1", LeaderPosID: "pos-41",
		Symbol: symbol, Side: "short", MarginMode: "cross",
		TriggerPrice: 3336, ATR: 12, DistanceATRRatio: 1.6, ReentryBoundary: 3345,
		RecommendedNotional: 41.8, StopCount: 3, ReentryCount: 2,
		LeaderSize: 1.5, LeaderEntryPrice: 3350, Protectable: true, Reason: "gates passed",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// TestBuildDataPackGuardLayerWithMarketUnavailable 用 Binance 必然不存在的
// symbol 验证：市场层整体降级为 null，仓位层完整且计算正确。
func TestBuildDataPackGuardLayerWithMarketUnavailable(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "advisor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sig := newTestSignal(t, st, "ZZZNOSUCHCOINUSDT")

	a := &Advisor{st: st, bn: newBinanceClient()}
	analysis, err := a.generateForSignal(sig, &store.ReentryAIConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.MarketDataAvailable {
		t.Fatal("market should be unavailable for nonexistent binance symbol")
	}
	if analysis.SystemPrompt == "" || !strings.Contains(analysis.UserPrompt, "ZZZNOSUCHCOINUSDT") {
		t.Fatalf("prompts not generated: sys=%d chars", len(analysis.SystemPrompt))
	}
	if analysis.PromptVersion != promptVersion {
		t.Fatalf("prompt version = %q", analysis.PromptVersion)
	}

	var pack DataPack
	if err := json.Unmarshal([]byte(analysis.DatapackJSON), &pack); err != nil {
		t.Fatalf("datapack json invalid: %v", err)
	}
	if pack.Market != nil {
		t.Fatal("market section should be null")
	}
	g := pack.CopyGuard
	if g.SignalID != sig.ID || g.RecommendedNotional != 41.8 || g.StopCount != 3 {
		t.Fatalf("guard basics = %+v", g)
	}
	if len(g.Attempts) != 1 || g.Attempts[0].Status != "STOPPED" {
		t.Fatalf("attempts = %+v", g.Attempts)
	}
	// 策略快照只提取 risk_* / min_trade_warn / copy_ratio
	if _, ok := g.Policy["other_field"]; ok {
		t.Fatal("policy should not contain unrelated fields")
	}
	if g.Policy["min_trade_warn"] == nil || g.Policy["risk_reentry_max_chase_atr"] == nil {
		t.Fatalf("policy missing risk fields: %+v", g.Policy)
	}
	// 空单：当前价 3336 低于止损价 3360 → 沿方向恢复为正
	if g.LastStop == nil || g.LastStop.Price != 3360 || g.LastStop.DistanceFromCurrentATR <= 0 {
		t.Fatalf("last stop = %+v", g.LastStop)
	}
	// 领航员仍持仓；BaselineLeaderSize 由引擎运行期写入（EnsureCopyGuardCycle
	// 不落该列），本测试未经引擎 → 基线未知，ratio 应为 0（不误算）
	if !g.Leader.StillHolding || g.Leader.SizeVsCycleBaseline != 0 {
		t.Fatalf("leader = %+v", g.Leader)
	}
	// 空单且价格低于领航员均价 → 领航员浮盈为正
	if g.Leader.UnrealizedPnLPct <= 0 {
		t.Fatalf("leader upnl = %v, want > 0 (short in profit)", g.Leader.UnrealizedPnLPct)
	}

	// 幂等辅助：已有分析
	has, err := st.ReentryAI().HasReentryAnalysisForSignal(sig.ID)
	if err != nil || !has {
		t.Fatalf("has=%v err=%v", has, err)
	}
}

// TestLastStopInfoCluster 止损簇间距：两次止损价差 / ATR
func TestLastStopInfoCluster(t *testing.T) {
	attempts := []*store.CopyGuardAttempt{
		{AttemptNo: 0, Status: "STOPPED", StopFillPrice: 100},
		{AttemptNo: 1, Status: "STOPPED", StopFillPrice: 102},
		{AttemptNo: 2, Status: "OPEN"},
	}
	info := buildLastStopInfo(attempts, "long", 105, 4)
	if info == nil || info.Price != 102 {
		t.Fatalf("info = %+v, want last stop 102", info)
	}
	// 多单：当前价 105 高于止损 102 → 恢复 (105-102)/4 = 0.75
	if info.DistanceFromCurrentATR != 0.75 {
		t.Fatalf("distance = %v, want 0.75", info.DistanceFromCurrentATR)
	}
	// 簇间距 (102-100)/4 = 0.5
	if info.StopClusterSpreadATR != 0.5 {
		t.Fatalf("cluster spread = %v, want 0.5", info.StopClusterSpreadATR)
	}
	if buildLastStopInfo(nil, "long", 100, 1) != nil {
		t.Fatal("no stopped attempts should return nil")
	}
}
