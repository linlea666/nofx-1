package reentryadvisor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"bare", `{"a":1}`, `{"a":1}`, true},
		{"fenced", "分析如下：\n```json\n{\"a\":1}\n```\n完毕", `{"a":1}`, true},
		{"nested", `text {"a":{"b":[1,2]},"c":"x"} tail`, `{"a":{"b":[1,2]},"c":"x"}`, true},
		{"brace-in-string", `{"a":"}{","b":1}`, `{"a":"}{","b":1}`, true},
		{"escaped-quote", `{"a":"say \"}\"","b":2}`, `{"a":"say \"}\"","b":2}`, true},
		{"none", "没有任何结构化输出", "", false},
		{"unbalanced", `{"a":1`, "", false},
	}
	for _, c := range cases {
		got, ok := extractJSONObject(c.in)
		if ok != c.ok || got != c.want {
			t.Fatalf("%s: got (%q,%v), want (%q,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestParseAIVerdict(t *testing.T) {
	raw := "结论：\n```json\n{\"decision\":\"enter\",\"confidence\":0.82,\"suggested_notional\":150.5,\"reasons\":[\"领航员仍持仓\"],\"risk_notes\":[\"funding 偏高\"]}\n```"
	pv, err := parseAIVerdict(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Verdict != "ENTER" || pv.Confidence != 0.82 || pv.SuggestedNotional != 150.5 {
		t.Fatalf("parsed = %+v", pv)
	}
	for _, want := range []string{"领航员仍持仓", "funding 偏高", "150.5"} {
		if !strings.Contains(pv.ReasonsJSON, want) {
			t.Fatalf("reasons json missing %q: %s", want, pv.ReasonsJSON)
		}
	}

	// WAIT 不携带建议金额
	pv2, err := parseAIVerdict(`{"decision":"WAIT","confidence":1.5,"suggested_notional":99,"reasons":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if pv2.SuggestedNotional != 0 || strings.Contains(pv2.ReasonsJSON, "suggested_notional") {
		t.Fatalf("WAIT should not carry suggested_notional: %+v", pv2)
	}
	if pv2.Confidence != 1 {
		t.Fatalf("conf should clamp to 1, got %v", pv2.Confidence)
	}

	// 置信度越界收敛到 [0,1]
	pv3, err := parseAIVerdict(`{"decision":"SKIP","confidence":-2}`)
	if err != nil || pv3.Confidence != 0 {
		t.Fatalf("conf clamp failed: %+v %v", pv3, err)
	}

	// 非法 decision
	if _, err := parseAIVerdict(`{"decision":"YOLO","confidence":0.5}`); err == nil {
		t.Fatal("expected error for invalid decision")
	}
	// 无 JSON
	if _, err := parseAIVerdict("模型闲聊，没有结构化输出"); err == nil {
		t.Fatal("expected error for missing json")
	}
}

// TestLegacyAnalysisExecutionRetired ensures the old manual-signal analyzer can
// never be interpreted as an execution route. auto_entry_enabled now only
// gates scheduler-managed ai_guarded candidates.
func TestLegacyAnalysisExecutionRetired(t *testing.T) {
	cfg := &store.ReentryAIConfig{
		Enabled: true, AIEnabled: true, AutoEntryEnabled: true,
		ConfidenceThreshold: 0.7, TimeoutSeconds: 60,
	}
	if note := legacyAnalysisExecutionNote(cfg, &parsedVerdict{Verdict: store.ReentryVerdictWait}); note != "" {
		t.Fatalf("WAIT should skip, got %q", note)
	}
	note := legacyAnalysisExecutionNote(cfg, &parsedVerdict{Verdict: store.ReentryVerdictEnter, Confidence: 0.99})
	if !strings.Contains(note, "已废弃") || !strings.Contains(note, "仅用于历史审计") {
		t.Fatalf("retired execution note = %q", note)
	}
}

func TestAnalyzeAnalysisRejectsSchedulerManagedCandidate(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "candidate-manual-trigger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	analysis, err := st.ReentryAI().SaveReentryAnalysis(&store.ReentryAIAnalysis{
		CandidateID: 42,
		TraderID:    "trader-a",
		CycleID:     7,
		Symbol:      "BTCUSDT",
		Side:        "long",
		CallStatus:  "PENDING",
	})
	if err != nil {
		t.Fatal(err)
	}

	defaultAdvisorMu.Lock()
	previous := defaultAdvisor
	defaultAdvisor = &Advisor{st: st, inflight: map[int64]bool{}, analyzeLast: map[int64]time.Time{}}
	defaultAdvisorMu.Unlock()
	t.Cleanup(func() {
		defaultAdvisorMu.Lock()
		defaultAdvisor = previous
		defaultAdvisorMu.Unlock()
	})

	if err := AnalyzeAnalysis(analysis.ID); err == nil || !strings.Contains(err.Error(), "持久化调度器") {
		t.Fatalf("scheduler-managed analysis should reject manual trigger, got %v", err)
	}
}
