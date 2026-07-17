package api

import (
	"encoding/json"
	"testing"
)

func TestReentryAIConfigRequestDistinguishesMissingAndExplicitFocus(t *testing.T) {
	var oldClient reentryAIConfigRequest
	if err := json.Unmarshal([]byte(`{"enabled":true,"model":"openai-a"}`), &oldClient); err != nil {
		t.Fatal(err)
	}
	if !oldClient.Enabled || oldClient.Model != "openai-a" || oldClient.AnalysisFocus != nil {
		t.Fatalf("old client payload did not preserve missing-field semantics: %+v", oldClient)
	}
	var clear reentryAIConfigRequest
	if err := json.Unmarshal([]byte(`{"enabled":true,"analysis_focus":""}`), &clear); err != nil {
		t.Fatal(err)
	}
	if clear.AnalysisFocus == nil || *clear.AnalysisFocus != "" {
		t.Fatalf("explicit empty focus must remain distinguishable: %+v", clear)
	}
}
