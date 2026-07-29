package reentryadvisor

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

func TestNextCandidateReviewAtUsesUnboundedTieredCadence(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO traders
		(id,user_id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation)
		VALUES('trader-1','user-1','trader','model','exchange',1000,1,?,1)`,
		store.TraderLifecycleRunning); err != nil {
		t.Fatal(err)
	}
	candidate, err := st.ReentryAI().EnsureReentryCandidate(&store.CopyGuardReentryCandidate{
		CycleID: 1, TraderID: "trader-1", LeaderPosID: "leader-pos",
		Symbol: "HYPEUSDT", Side: "long", FeatureHash: "tiered",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := st.ReentryAI().SaveReentryAnalysis(&store.ReentryAIAnalysis{
		CandidateID: candidate.ID, TraderID: "trader-1", CycleID: 1,
		Symbol: "HYPEUSDT", Side: "long", CallStatus: "RUNNING",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	if _, err = st.DB().Exec(`UPDATE reentry_ai_analyses SET created_at=? WHERE id=?`,
		first.Format("2006-01-02 15:04:05"), analysis.ID); err != nil {
		t.Fatal(err)
	}
	advisor := &Advisor{st: st}
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"before first 15 minute review", first.Add(5 * time.Minute), first.Add(15 * time.Minute)},
		{"second review remains at 45 minutes", first.Add(20 * time.Minute), first.Add(45 * time.Minute)},
		{"hourly review after 45 minutes", first.Add(45 * time.Minute), first.Add(105 * time.Minute)},
		{"hourly cadence reaches six hours", first.Add(350 * time.Minute), first.Add(6 * time.Hour)},
		{"reviews continue after six hours", first.Add(6 * time.Hour), first.Add(8 * time.Hour)},
		{"two hour cadence has no lifecycle ceiling", first.Add(620 * time.Minute), first.Add(12 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := advisor.nextCandidateReviewAt(candidate.ID, tt.now)
			if !got.Equal(tt.want) {
				t.Fatalf("next review=%s want=%s", got, tt.want)
			}
		})
	}
}

func TestMarketEventFlipsAreCoalescedAndMissingRecoveryIsNotAFlip(t *testing.T) {
	before := marketEventSnapshot{
		FundingState: "normal", OIState: "STABLE",
		CVDState: "falling|", ATRState: "NORMAL",
	}
	after := marketEventSnapshot{
		FundingState: "elevated", OIState: "RISING",
		CVDState: "rising|", ATRState: "EXPANDED",
	}
	want := []string{
		"ATR_REGIME_CHANGE",
		"CONTRACT_CVD_STATE_FLIP",
		"FUNDING_STATE_FLIP",
		"OI_STATE_FLIP",
	}
	if got := changedMarketEventTriggers(before, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("market event trigger set=%v want=%v", got, want)
	}
	if got := changedMarketEventTriggers(marketEventSnapshot{}, after); len(got) != 0 {
		t.Fatalf("missing data recovery was treated as reversal: %v", got)
	}
}

func TestATRRegimeUsesClosedTrueRange(t *testing.T) {
	klines := make([]market.Kline, 0, 16)
	for i := 0; i < 16; i++ {
		closePrice := 100.0 + float64(i)
		klines = append(klines, market.Kline{
			Open: closePrice - 1, High: closePrice + 2,
			Low: closePrice - 2, Close: closePrice,
		})
	}
	atr := atrFromClosedKlines(klines, 14)
	if atr <= 0 {
		t.Fatalf("closed-kline ATR unavailable: %f", atr)
	}
	if got := atrRegime(atr, atr/2); got != "EXPANDED" {
		t.Fatalf("ATR expansion not detected: %s", got)
	}
	if got := atrRegime(atr, atr*2); got != "CONTRACTED" {
		t.Fatalf("ATR contraction not detected: %s", got)
	}
}

func TestCandidateFailureBackoffIsExponentialAndBounded(t *testing.T) {
	want := []time.Duration{
		5 * time.Minute,
		15 * time.Minute,
		time.Hour,
		2 * time.Hour,
		4 * time.Hour,
		6 * time.Hour,
		6 * time.Hour,
	}
	for i, expected := range want {
		if got := candidateFailureBackoff(i + 1); got != expected {
			t.Fatalf("failure %d backoff=%s want=%s", i+1, got, expected)
		}
	}
}
