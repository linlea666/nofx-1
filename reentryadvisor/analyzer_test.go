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

// TestMaybeAutoEnterGuards Phase 3 自动入场的层层护栏：
// 开关关 / 非 ENTER / 置信度不足 / 快照过时 / 执行链拒绝（引擎未运行）。
func TestMaybeAutoEnterGuards(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "autoentry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sig := newTestSignal(t, st, "ZZZNOSUCHCOINUSDT")
	a := &Advisor{st: st, bn: newBinanceClient(), inflight: map[int64]bool{}}
	analysis := &store.ReentryAIAnalysis{
		ID: 1, SignalID: sig.ID, TraderID: sig.TraderID,
		Symbol: sig.Symbol, Side: sig.Side, SnapshotAt: time.Now(),
	}
	enter := &parsedVerdict{Verdict: store.ReentryVerdictEnter, Confidence: 0.9}
	cfg := &store.ReentryAIConfig{
		Enabled: true, AIEnabled: true, AutoEntryEnabled: true,
		ConfidenceThreshold: 0.7, TimeoutSeconds: 60,
	}

	// 开关关闭：不尝试
	off := *cfg
	off.AutoEntryEnabled = false
	if note := a.maybeAutoEnter(analysis, &off, enter); note != "" {
		t.Fatalf("disabled switch should skip, got %q", note)
	}
	// 非 ENTER：不尝试
	wait := &parsedVerdict{Verdict: store.ReentryVerdictWait, Confidence: 0.99}
	if note := a.maybeAutoEnter(analysis, cfg, wait); note != "" {
		t.Fatalf("WAIT should skip, got %q", note)
	}
	// 置信度不足：留说明
	low := &parsedVerdict{Verdict: store.ReentryVerdictEnter, Confidence: 0.5}
	if note := a.maybeAutoEnter(analysis, cfg, low); !strings.Contains(note, "低于门槛") {
		t.Fatalf("low confidence note = %q", note)
	}
	// 快照过时：留说明
	stale := *analysis
	stale.SnapshotAt = time.Now().Add(-time.Hour)
	if note := a.maybeAutoEnter(&stale, cfg, enter); !strings.Contains(note, "过时") {
		t.Fatalf("stale snapshot note = %q", note)
	}
	// 全部护栏通过 → 走 ConfirmManualReentryForTrader（测试环境无运行中
	// 引擎，被硬校验拒绝），信号保持 PENDING
	note := a.maybeAutoEnter(analysis, cfg, enter)
	if !strings.Contains(note, "自动入场未执行") {
		t.Fatalf("engine-off note = %q", note)
	}
	got, err := st.CopyTrade().GetManualReentrySignal(sig.ID)
	if err != nil || got.Status != store.ManualReentryStatusPending {
		t.Fatalf("signal should stay PENDING, got %+v err=%v", got, err)
	}
}
