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
	cfg.ConfidenceThreshold = 0.8
	if err := st.ReentryAI().SaveReentryAIConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Model != "deepseek-chat" || got.ConfidenceThreshold != 0.8 {
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
