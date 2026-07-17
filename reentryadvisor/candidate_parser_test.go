package reentryadvisor

import (
	"fmt"
	"testing"
	"time"

	"nofx/store"
)

const validCandidateJSON = `{"decision":"ENTER_NOW","regime":"REVERSAL","confidence":0.86,"size_factor":0.5,"entry_price_low":99,"entry_price_high":101,"attention_price_low":97,"attention_price_high":102,"ttl_seconds":30,"next_review_seconds":900,"reasons":["closed candle reversal"],"risk_notes":[]}`

func TestParseAICandidateVerdictStrictSchema(t *testing.T) {
	got, err := parseAICandidateVerdict(validCandidateJSON)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "ENTER" || got.SizeFactor != 0.5 || got.TTLSeconds != 30 {
		t.Fatalf("unexpected verdict: %+v", got)
	}
	for name, raw := range map[string]string{
		"markdown":                  "```json\n" + validCandidateJSON + "\n```",
		"unknown field":             `{"decision":"WAIT","regime":"CHOP","confidence":0.5,"size_factor":0,"entry_price_low":0,"entry_price_high":0,"attention_price_low":0,"attention_price_high":0,"ttl_seconds":30,"next_review_seconds":900,"reasons":[],"risk_notes":[],"order_now":true}`,
		"legacy suggested notional": `{"decision":"WAIT","regime":"CHOP","confidence":0.5,"size_factor":0,"entry_price_low":0,"entry_price_high":0,"attention_price_low":0,"attention_price_high":0,"ttl_seconds":30,"next_review_seconds":900,"reasons":[],"risk_notes":[],"suggested_notional":10}`,
		"low ttl":                   `{"decision":"WAIT","regime":"CHOP","confidence":0.5,"size_factor":0,"entry_price_low":0,"entry_price_high":0,"attention_price_low":0,"attention_price_high":0,"ttl_seconds":5,"next_review_seconds":900,"reasons":[],"risk_notes":[]}`,
		"wait size":                 `{"decision":"WAIT","regime":"CHOP","confidence":0.5,"size_factor":0.2,"entry_price_low":0,"entry_price_high":0,"attention_price_low":0,"attention_price_high":0,"ttl_seconds":30,"next_review_seconds":900,"reasons":[],"risk_notes":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAICandidateVerdict(raw); err == nil {
				t.Fatal("strict candidate schema must reject invalid output")
			}
		})
	}
}

func TestCandidateClosed5mCandleKeyUsesPersistedMarketBar(t *testing.T) {
	open := time.Date(2026, 7, 17, 10, 5, 0, 0, time.UTC)
	analysis := &store.ReentryAIAnalysis{
		SnapshotAt: open.Add(9 * time.Minute),
		DatapackJSON: fmt.Sprintf(
			`{"market":{"klines":{"5m":{"bars":[[%d,1,2,0.5,1.5,10]],"pct_change_window":0,"volume_ratio_5_20":1}}}}`,
			open.UnixMilli(),
		),
	}
	if got := candidateClosed5mCandleKey(analysis); got != open.Format(time.RFC3339) {
		t.Fatalf("candle key=%q want=%q", got, open.Format(time.RFC3339))
	}
	analysis.DatapackJSON = `{"market":{"klines":{}}}`
	if got := candidateClosed5mCandleKey(analysis); got != "" {
		t.Fatalf("missing closed 5m data must not fall back to wall time: %q", got)
	}
}
