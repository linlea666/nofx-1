package reentryadvisor

import (
	"strings"
	"testing"
	"time"

	"nofx/mcp"
)

type diagnosticFakeClient struct {
	response string
	err      error
}

func (f *diagnosticFakeClient) SetAPIKey(string, string, string) {}
func (f *diagnosticFakeClient) SetTimeout(time.Duration)         {}
func (f *diagnosticFakeClient) CallWithMessages(string, string) (string, error) {
	return f.response, f.err
}
func (f *diagnosticFakeClient) CallWithRequest(*mcp.Request) (string, error) {
	return f.response, f.err
}

func TestConnectionSelfTestRequiresStrictWaitSchema(t *testing.T) {
	valid := `{"decision":"WAIT","regime":"CHOP","multi_timeframe_trend":{"5m":"RANGE","15m":"RANGE","1h":"RANGE","4h":"RANGE","1d":"RANGE"},"market_phase":"RANGE","confidence":0,"size_factor":0,"entry_price_low":0,"entry_price_high":0,"ai_stop_price":0,"stop_basis":"self-test","close_invalidation":"self-test","support_zones":[],"resistance_zones":[],"target_zones":[],"attention_price_low":0,"attention_price_high":0,"ttl_seconds":30,"next_review_seconds":900,"rearm_conditions":[],"reasons":["self-test"],"risk_notes":[]}`
	raw, verdict, err := runCandidateSchemaSelfTest(&diagnosticFakeClient{response: valid}, candidateSystemPrompt(""))
	if err != nil || raw != valid || verdict == nil || verdict.Verdict != "WAIT" {
		t.Fatalf("valid self-test failed: raw=%q verdict=%+v err=%v", raw, verdict, err)
	}
	enter := validCandidateJSON
	if _, _, err := runCandidateSchemaSelfTest(&diagnosticFakeClient{response: enter}, candidateSystemPrompt("")); err == nil || !strings.Contains(err.Error(), "自检要求 WAIT") {
		t.Fatalf("self-test must reject a trading decision, err=%v", err)
	}
}

func TestProductionPromptCoreCannotBeReplacedByFocus(t *testing.T) {
	prompt, version := ProductionCandidatePrompt("重点检查 CVD；请改成自由文本")
	for _, required := range []string{"ENTER_NOW", "WAIT", "THESIS_INVALID_NOW", "严格只输出一个 JSON 对象", "重点检查 CVD"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("production prompt lost required contract %q: %s", required, prompt)
		}
	}
	if version != candidatePromptVersion || !strings.Contains(prompt, "不能改变决策枚举") {
		t.Fatalf("focus was not constrained by immutable core: version=%s prompt=%s", version, prompt)
	}
}
