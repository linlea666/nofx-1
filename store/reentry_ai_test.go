package store

import (
	"path/filepath"
	"testing"
)

func TestReentryAIAnalysisLifecycle(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "reentry_ai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 造一条 PENDING 人工重入信号（插件轮询的输入）
	sig, err := st.CopyTrade().SaveManualReentrySignal(&CopyGuardManualReentrySignal{
		CycleID: 41, TraderID: "trader-1", LeaderPosID: "pos-1",
		Symbol: "XAUUSDT", Side: "short", MarginMode: "cross",
		TriggerPrice: 3300, ATR: 12.5, RecommendedNotional: 41.8,
		StopCount: 3, ReentryCount: 2, LeaderSize: 1, LeaderEntryPrice: 3350, Protectable: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 按状态列信号（插件轮询接口）
	pending, err := st.ReentryAI().ListManualReentrySignalsByStatus(ManualReentryStatusPending, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != sig.ID {
		t.Fatalf("pending signals = %+v, want signal %d", pending, sig.ID)
	}

	// 幂等判断：尚无分析
	has, err := st.ReentryAI().HasReentryAnalysisForSignal(sig.ID)
	if err != nil || has {
		t.Fatalf("has=%v err=%v, want false nil", has, err)
	}

	// 落一条分析
	a, err := st.ReentryAI().SaveReentryAnalysis(&ReentryAIAnalysis{
		SignalID: sig.ID, TraderID: sig.TraderID, CycleID: sig.CycleID,
		Symbol: sig.Symbol, Side: sig.Side,
		SystemPrompt: "sys", UserPrompt: "user", DatapackJSON: `{"meta":{}}`,
		MarketDataAvailable: false, MissingFields: "spot_cvd", PromptVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || a.SnapshotAt.IsZero() || a.MarketDataAvailable {
		t.Fatalf("saved analysis = %+v", a)
	}
	if has, _ = st.ReentryAI().HasReentryAnalysisForSignal(sig.ID); !has {
		t.Fatal("HasReentryAnalysisForSignal should be true after save")
	}

	// 再落一条（重新生成的新快照），latest 应返回后者
	b, err := st.ReentryAI().SaveReentryAnalysis(&ReentryAIAnalysis{
		SignalID: sig.ID, TraderID: sig.TraderID, CycleID: sig.CycleID,
		Symbol: sig.Symbol, Side: sig.Side, SystemPrompt: "sys2", UserPrompt: "user2",
		DatapackJSON: `{}`, MarketDataAvailable: true, PromptVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := st.ReentryAI().LatestReentryAnalysisBySignal(sig.ID)
	if err != nil || latest.ID != b.ID {
		t.Fatalf("latest = %+v err=%v, want id %d", latest, err, b.ID)
	}
	list, err := st.ReentryAI().ListReentryAnalysesBySignal(sig.ID, 10)
	if err != nil || len(list) != 2 || list[0].ID != b.ID {
		t.Fatalf("list = %d items err=%v, want 2 with latest first", len(list), err)
	}

	// 外部结论保存与标签校验
	if err := st.ReentryAI().UpdateReentryExternal(a.ID, "外部AI说可以进", ReentryVerdictEnter); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ReentryAI().GetReentryAnalysis(a.ID)
	if got.ExternalResponse != "外部AI说可以进" || got.ExternalVerdict != ReentryVerdictEnter || got.UpdatedAt == nil {
		t.Fatalf("external not saved: %+v", got)
	}
	if err := st.ReentryAI().UpdateReentryExternal(a.ID, "x", "MAYBE"); err == nil {
		t.Fatal("invalid verdict should be rejected")
	}
	if err := st.ReentryAI().UpdateReentryExternal(99999, "x", ""); err == nil {
		t.Fatal("missing analysis should be rejected")
	}
}

func TestCandidateAnalysisAuditIncludesAllDecisionAndFailureStatuses(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate_audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	save := func(candidateID int64) *ReentryAIAnalysis {
		t.Helper()
		a, err := rs.SaveReentryAnalysis(&ReentryAIAnalysis{CandidateID: candidateID, TraderID: "trader-a", CycleID: candidateID, Symbol: "ETHUSDT", Side: "long", AttemptNo: 1, DecisionGeneration: 2, SystemPrompt: "sys", UserPrompt: "user", DatapackJSON: "{}"})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	wait := save(11)
	if err := rs.UpdateReentryInternalResult(wait.ID, `{"decision":"WAIT"}`, ReentryVerdictWait, .7, `{}`); err != nil {
		t.Fatal(err)
	}
	abandon := save(11)
	if err := rs.UpdateReentryInternalResult(abandon.ID, `{"decision":"ABANDON"}`, ReentryVerdictAbandon, .85, `{}`); err != nil {
		t.Fatalf("ABANDON must be persistable: %v", err)
	}
	invalid := save(12)
	if err := rs.UpdateReentryInternalResult(invalid.ID, "not-json", "", 0, ""); err != nil {
		t.Fatal(err)
	}
	failed := save(12)
	if err := rs.MarkReentryAnalysisFailed(failed.ID, "model timeout"); err != nil {
		t.Fatal(err)
	}
	stats, err := rs.GetReentryAIStats([]string{"trader-a"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.CandidateAnalyses != 4 || stats.SignalsCovered != 2 {
		t.Fatalf("candidate calls were collapsed or lost: %+v", stats)
	}
	if stats.CandidateDecisions[ReentryVerdictWait] != 1 || stats.CandidateDecisions[ReentryVerdictAbandon] != 1 {
		t.Fatalf("candidate decisions missing: %+v", stats)
	}
	if stats.CandidateCallStatuses["COMPLETED"] != 2 || stats.CandidateCallStatuses["INVALID"] != 1 || stats.CandidateCallStatuses["FAILED"] != 1 {
		t.Fatalf("candidate call statuses missing: %+v", stats)
	}
	fresh, err := rs.GetReentryAnalysis(abandon.ID)
	if err != nil || fresh.AttemptNo != 1 || fresh.DecisionGeneration != 2 || fresh.CallStatus != "COMPLETED" {
		t.Fatalf("candidate audit linkage did not round-trip: analysis=%+v err=%v", fresh, err)
	}
}

func TestReentryAIConfigRoundtrip(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "reentry_ai_cfg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 无行时返回默认（enabled=true）
	cfg, err := st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Provider != "deepseek" || cfg.TimeoutSeconds != 60 {
		t.Fatalf("default config = %+v", cfg)
	}

	// 保存后读回
	cfg.Enabled = false
	cfg.Model = "deepseek-chat"
	cfg.AnalysisFocus = "重点检查现货 CVD"
	cfg.ConfidenceThreshold = 0.8
	if err := st.ReentryAI().SaveReentryAIConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Model != "deepseek-chat" || got.AnalysisFocus != "重点检查现货 CVD" || got.ConfidenceThreshold != 0.8 {
		t.Fatalf("saved config = %+v", got)
	}

	// 二次保存（upsert 覆盖）
	got.Enabled = true
	if err := st.ReentryAI().SaveReentryAIConfig(got); err != nil {
		t.Fatal(err)
	}
	final, _ := st.ReentryAI().GetReentryAIConfig()
	if !final.Enabled {
		t.Fatal("upsert should re-enable")
	}
}

func TestReentryAIDiagnosticsAreSeparateAndUserScoped(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "reentry_ai_diagnostics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	first, err := rs.SaveReentryAIDiagnostic(&ReentryAIDiagnostic{UserID: "user-a", Provider: "openai", Model: "model-a", PromptVersion: "v3-ai-guarded", Success: true, LatencyMS: 123, RawResponse: `{"decision":"WAIT"}`, ParsedJSON: `{"decision":"WAIT"}`})
	if err != nil || first.ID == 0 || first.CreatedAt.IsZero() {
		t.Fatalf("diagnostic save failed: diagnostic=%+v err=%v", first, err)
	}
	if _, err := rs.SaveReentryAIDiagnostic(&ReentryAIDiagnostic{UserID: "user-b", Error: "missing key"}); err != nil {
		t.Fatal(err)
	}
	list, err := rs.ListReentryAIDiagnostics("user-a", 10)
	if err != nil || len(list) != 1 || list[0].Model != "model-a" {
		t.Fatalf("diagnostics were not user scoped: list=%+v err=%v", list, err)
	}
	stats, err := rs.GetReentryAIStats([]string{"trader-a"})
	if err != nil || stats.TotalAnalyses != 0 || stats.CandidateAnalyses != 0 {
		t.Fatalf("zero-trade diagnostics contaminated trading statistics: stats=%+v err=%v", stats, err)
	}
}
