package reentryadvisor

import (
	"strings"
	"testing"
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
	verdict, conf, reasons, err := parseAIVerdict(raw)
	if err != nil {
		t.Fatal(err)
	}
	if verdict != "ENTER" || conf != 0.82 {
		t.Fatalf("verdict=%s conf=%v", verdict, conf)
	}
	for _, want := range []string{"领航员仍持仓", "funding 偏高", "150.5"} {
		if !strings.Contains(reasons, want) {
			t.Fatalf("reasons json missing %q: %s", want, reasons)
		}
	}

	// WAIT 不携带建议金额
	_, _, reasons2, err := parseAIVerdict(`{"decision":"WAIT","confidence":1.5,"suggested_notional":99,"reasons":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reasons2, "suggested_notional") {
		t.Fatalf("WAIT should not carry suggested_notional: %s", reasons2)
	}

	// 置信度越界收敛到 [0,1]
	_, conf3, _, err := parseAIVerdict(`{"decision":"SKIP","confidence":-2}`)
	if err != nil || conf3 != 0 {
		t.Fatalf("conf clamp failed: %v %v", conf3, err)
	}

	// 非法 decision
	if _, _, _, err := parseAIVerdict(`{"decision":"YOLO","confidence":0.5}`); err == nil {
		t.Fatal("expected error for invalid decision")
	}
	// 无 JSON
	if _, _, _, err := parseAIVerdict("模型闲聊，没有结构化输出"); err == nil {
		t.Fatal("expected error for missing json")
	}
}
