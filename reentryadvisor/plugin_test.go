package reentryadvisor

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

func TestLifecycleReentryConfigSurvivesTraderModeSwitch(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "lifecycle-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "position-1",
		Symbol: "ETHUSDT", Side: "long", Status: store.CopyGuardAIWaiting,
		PolicySnapshot: `{
			"version":4,
			"risk_protection_mode":"atr_structure",
			"reentry_enabled":true,
			"max_reentries":2,
			"reentry_decision_mode":"ai_guarded",
			"reentry_min_notional":9,
			"ai_min_review_seconds":900,
			"ai_daily_call_limit":7,
			"ai_lifecycle_call_limit":3
		}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	fallback := store.NewCopyGuardDefaults()
	fallback.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	fallback.RiskReentryEnabled = false
	fallback.RiskReentryDecisionMode = "disabled"
	fallback.RiskMaxReentries = 0

	advisor := &Advisor{st: st}
	got, err := advisor.lifecycleReentryConfig(&store.CopyGuardReentryCandidate{CycleID: cycle.ID}, fallback)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RiskReentryEnabled || got.RiskReentryDecisionMode != "ai_guarded" ||
		got.RiskMaxReentries != 2 || got.RiskReentryMinNotional != 9 ||
		got.RiskAIMinReviewSeconds != 900 || got.RiskAIDailyCallLimit != 7 ||
		got.RiskAILifecycleCallLimit != 3 {
		t.Fatalf("advisor inherited the later fixed template: %+v", got)
	}
}

// TestBackfillOutcomes 已执行信号 → 重入尝试闭合并对账后，分析记录回填
// 结局净额（OKX 已含费的 pnl，不重复扣 fee），随后统计口径正确。
func TestBackfillOutcomes(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "backfill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sig := newTestSignal(t, st, "ZZZNOSUCHCOINUSDT") // ReentryCount=2 → 目标 attempt_no=3
	cs := st.CopyTrade()

	analysis, err := st.ReentryAI().SaveReentryAnalysis(&store.ReentryAIAnalysis{
		SignalID: sig.ID, TraderID: sig.TraderID, CycleID: sig.CycleID,
		Symbol: sig.Symbol, Side: sig.Side,
		SystemPrompt: "sys", UserPrompt: "user", DatapackJSON: "{}", PromptVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 信号进入 EXECUTED
	if ok, err := cs.ClaimManualReentrySignal(sig.ID, "tester", 3336); err != nil || !ok {
		t.Fatalf("claim failed: ok=%v err=%v", ok, err)
	}
	if err := cs.MarkManualReentrySignalOutcome(sig.ID, store.ManualReentryStatusExecuted, ""); err != nil {
		t.Fatal(err)
	}

	a := &Advisor{st: st, bn: newBinanceClient(), inflight: map[int64]bool{}}

	// 尝试尚未开出/闭合：不回填
	a.backfillOutcomes()
	got, _ := st.ReentryAI().GetReentryAnalysis(analysis.ID)
	if got.OutcomePnL != nil {
		t.Fatalf("outcome should be nil before attempt closes, got %v", *got.OutcomePnL)
	}

	// 重入成交（attempt_no=3）→ 止损闭合 → 对账
	if err := cs.OpenCopyGuardAttempt(sig.CycleID, 3, 3336, 41.8, 0.0125, 12); err != nil {
		t.Fatal(err)
	}
	cycleRef := &store.CopyGuardCycle{ID: sig.CycleID, TraderID: sig.TraderID, ReentryCount: 3}
	if err := cs.RecordCopyGuardStop(cycleRef, 12, 3350, -5, 0.5, 0, "algo-3", map[string]interface{}{"quantity": 0.0125}); err != nil {
		t.Fatal(err)
	}

	// 闭合但未对账：仍不回填
	a.backfillOutcomes()
	got, _ = st.ReentryAI().GetReentryAnalysis(analysis.ID)
	if got.OutcomePnL != nil {
		t.Fatal("outcome should stay nil until reconciled")
	}

	if err := cs.ReconcileCopyGuardAttempt(sig.CycleID, 3, -5, 0.5, 0, 0); err != nil {
		t.Fatal(err)
	}
	a.backfillOutcomes()
	got, _ = st.ReentryAI().GetReentryAnalysis(analysis.ID)
	if got.OutcomePnL == nil || *got.OutcomePnL != -5 {
		t.Fatalf("outcome = %v, want -5", got.OutcomePnL)
	}

	// 内外部结论 + 统计：ENTER 且亏损 → 内部错误；SKIP 且亏损 → 外部正确
	if err := st.ReentryAI().UpdateReentryInternalResult(analysis.ID, "raw", store.ReentryVerdictEnter, 0.8, `{"reasons":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := st.ReentryAI().UpdateReentryExternal(analysis.ID, "外部说别进", store.ReentryVerdictSkip); err != nil {
		t.Fatal(err)
	}
	owned := []string{sig.TraderID}
	stats, err := st.ReentryAI().GetReentryAIStats(owned)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalAnalyses != 1 || stats.ScoredCount != 1 {
		t.Fatalf("stats totals = %+v", stats)
	}
	if stats.InternalVerdicts[store.ReentryVerdictEnter] != 1 || stats.InternalScored != 1 || stats.InternalCorrect != 0 {
		t.Fatalf("internal stats = %+v", stats)
	}
	if stats.ExternalVerdicts[store.ReentryVerdictSkip] != 1 || stats.ExternalScored != 1 || stats.ExternalCorrect != 1 {
		t.Fatalf("external stats = %+v", stats)
	}

	// 按信号去重：同信号再落第二个快照（同为 ENTER），统计样本数不应翻倍，
	// 且结局回填对新快照生效后样本仍按信号计 1
	analysis2, err := st.ReentryAI().SaveReentryAnalysis(&store.ReentryAIAnalysis{
		SignalID: sig.ID, TraderID: sig.TraderID, CycleID: sig.CycleID,
		Symbol: sig.Symbol, Side: sig.Side,
		SystemPrompt: "sys2", UserPrompt: "user2", DatapackJSON: "{}", PromptVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReentryAI().UpdateReentryInternalResult(analysis2.ID, "raw2", store.ReentryVerdictEnter, 0.9, `{"reasons":[]}`); err != nil {
		t.Fatal(err)
	}
	a.backfillOutcomes()
	stats, err = st.ReentryAI().GetReentryAIStats(owned)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalAnalyses != 2 || stats.SignalsCovered != 1 || stats.ScoredCount != 1 {
		t.Fatalf("dedupe totals = %+v", stats)
	}
	if stats.InternalVerdicts[store.ReentryVerdictEnter] != 1 || stats.InternalScored != 1 {
		t.Fatalf("dedupe internal stats = %+v", stats)
	}
	// 外部结论只在第一个快照上，去重后仍取"最新带外部结论的快照"= 快照 1
	if stats.ExternalVerdicts[store.ReentryVerdictSkip] != 1 || stats.ExternalCorrect != 1 {
		t.Fatalf("dedupe external stats = %+v", stats)
	}

	// 归属过滤：非本人 trader 看不到任何统计
	other, err := st.ReentryAI().GetReentryAIStats([]string{"someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	if other.TotalAnalyses != 0 || len(other.InternalVerdicts) != 0 {
		t.Fatalf("foreign trader should see empty stats: %+v", other)
	}
	if empty, err := st.ReentryAI().GetReentryAIStats(nil); err != nil || empty.TotalAnalyses != 0 {
		t.Fatalf("nil trader list should be empty stats: %+v %v", empty, err)
	}

	// 幂等：再跑一次不报错不重复
	a.backfillOutcomes()
}

// TestSpawnAnalysisRefusesAfterStop 保留自动补跑测试中唯一仍有生产路径的
// 断言：Stop 之后不得再拉起分析协程（否则会向已关闭资源写入）。
func TestSpawnAnalysisRefusesAfterStop(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "airetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sig := newTestSignal(t, st, "ZZZNOSUCHCOINUSDT")
	analysis, err := st.ReentryAI().SaveReentryAnalysis(&store.ReentryAIAnalysis{
		SignalID: sig.ID, TraderID: sig.TraderID, CycleID: sig.CycleID,
		Symbol: sig.Symbol, Side: sig.Side,
		SystemPrompt: "sys", UserPrompt: "user", DatapackJSON: "{}", PromptVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}

	a := &Advisor{
		st: st, bn: newBinanceClient(), stopCh: make(chan struct{}),
		inflight: map[int64]bool{}, analyzeLast: map[int64]time.Time{},
	}
	a.started = true

	a.Stop()
	if a.spawnAnalysis(analysis.ID, false) {
		t.Fatal("spawnAnalysis should refuse after Stop")
	}
}
