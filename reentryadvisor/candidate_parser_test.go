package reentryadvisor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

const validCandidateJSON = `{"decision":"ENTER_NOW","regime":"REVERSAL","multi_timeframe_trend":{"5m":"UP","15m":"UP","1h":"RANGE","4h":"DOWN","1d":"DOWN"},"market_phase":"TRANSITION","confidence":0.86,"size_factor":0.5,"entry_price_low":99,"entry_price_high":101,"ai_stop_price":97.5,"stop_basis":"1h swing low","close_invalidation":"15m close below 98","support_zones":[{"low":97,"high":98,"strength":82,"timeframes":["15m","1h"],"touches":3,"last_touch_at":"2026-08-01T00:00:00Z","role_reversal":true,"exhaustion":"TESTED"}],"resistance_zones":[{"low":104,"high":105,"strength":76,"timeframes":["1h"],"touches":2,"last_touch_at":"2026-07-31T00:00:00Z","role_reversal":false,"exhaustion":"FRESH"}],"target_zones":[{"low":104,"high":105,"basis":"1h resistance"}],"attention_price_low":97,"attention_price_high":102,"ttl_seconds":30,"next_review_seconds":900,"rearm_conditions":["15m close above 99"],"reasons":["closed candle reversal"],"risk_notes":[]}`

func TestParseAICandidateVerdictStrictSchema(t *testing.T) {
	got, err := parseAICandidateVerdict(validCandidateJSON)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "ENTER" || got.SizeFactor != 0.5 || got.TTLSeconds != 30 || got.AIStopPrice != 97.5 {
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
	for name, raw := range map[string]string{
		"invalid support timeframe": strings.Replace(validCandidateJSON, `"timeframes":["15m","1h"]`, `"timeframes":["2h"]`, 1),
		"invalid support time":      strings.Replace(validCandidateJSON, `"last_touch_at":"2026-08-01T00:00:00Z"`, `"last_touch_at":"yesterday"`, 1),
		"invalid target":            strings.Replace(validCandidateJSON, `"low":104,"high":105,"basis":"1h resistance"`, `"low":105,"high":104,"basis":""`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAICandidateVerdict(raw); err == nil {
				t.Fatal("v6 evidence schema must reject malformed structure")
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
