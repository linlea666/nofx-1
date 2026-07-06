package reentryadvisor

import (
	"path/filepath"
	"testing"

	"nofx/store"
)

// TestBackfillOutcomes 已执行信号 → 重入尝试闭合并对账后，分析记录回填
// 结局净额（pnl − fee），随后统计口径正确。
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
	if got.OutcomePnL == nil || *got.OutcomePnL != -5.5 {
		t.Fatalf("outcome = %v, want -5.5", got.OutcomePnL)
	}

	// 内外部结论 + 统计：ENTER 且亏损 → 内部错误；SKIP 且亏损 → 外部正确
	if err := st.ReentryAI().UpdateReentryInternalResult(analysis.ID, "raw", store.ReentryVerdictEnter, 0.8, `{"reasons":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := st.ReentryAI().UpdateReentryExternal(analysis.ID, "外部说别进", store.ReentryVerdictSkip); err != nil {
		t.Fatal(err)
	}
	stats, err := st.ReentryAI().GetReentryAIStats()
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

	// 幂等：再跑一次不报错不重复
	a.backfillOutcomes()
}
