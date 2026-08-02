package manager

import (
	"testing"
	"time"
)

func TestGetCachedCompetitionDataHonorsAgeAndReturnsCopy(t *testing.T) {
	tm := NewTraderManager()
	tm.competitionCache.data = map[string]interface{}{
		"traders": []map[string]interface{}{{"trader_id": "one", "total_equity": 101.0}},
		"count":   1,
	}
	tm.competitionCache.timestamp = time.Now()

	cached, ok := tm.GetCachedCompetitionData(time.Minute)
	if !ok {
		t.Fatal("fresh cache was not returned")
	}
	cached["traders"].([]map[string]interface{})[0]["total_equity"] = 0.0
	again, ok := tm.GetCachedCompetitionData(time.Minute)
	if !ok || again["traders"].([]map[string]interface{})[0]["total_equity"] != 101.0 {
		t.Fatal("caller mutation leaked into competition cache")
	}

	tm.competitionCache.timestamp = time.Now().Add(-2 * time.Minute)
	if _, ok := tm.GetCachedCompetitionData(time.Minute); ok {
		t.Fatal("stale cache must not be returned")
	}
}
