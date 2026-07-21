package copyguardmetrics

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

func TestEvaluatePathUsesDirectionATRAndClosedBuckets(t *testing.T) {
	start := time.Unix(1_800_000_000, 0).UTC().Truncate(5 * time.Minute)
	prices := []float64{100.2, 100.6, 100.7, 101.1, 100.8, 100.7}
	samples := make([]*store.CopyGuardWatchSample, 0, len(prices))
	for i, price := range prices {
		samples = append(samples, &store.CopyGuardWatchSample{
			AttemptNo: 0, MarkPrice: price, CreatedAt: start.Add(time.Duration(i) * 3 * time.Minute),
		})
	}
	got := evaluatePath(samples, 0, "long", 100, 1, start, start.Add(15*time.Minute), 3*time.Minute)
	if got.marketOutcome != store.ReentryMarketReversal || got.firstReversalAt == nil || got.mfeATR < 1 {
		t.Fatalf("expected confirmed long reversal, got %+v", got)
	}

	against := []*store.CopyGuardWatchSample{
		{AttemptNo: 1, MarkPrice: 100, CreatedAt: start},
		{AttemptNo: 1, MarkPrice: 100.4, CreatedAt: start.Add(3 * time.Minute)},
		{AttemptNo: 1, MarkPrice: 101.1, CreatedAt: start.Add(6 * time.Minute)},
		{AttemptNo: 1, MarkPrice: 101.2, CreatedAt: start.Add(9 * time.Minute)},
	}
	got = evaluatePath(against, 1, "short", 100, 1, start, start.Add(9*time.Minute), 3*time.Minute)
	if got.marketOutcome != store.ReentryMarketAgainst || got.maeATR < 1 {
		t.Fatalf("expected adverse continuation for short, got %+v", got)
	}
}

func TestClassifyDecisionOutcomeSeparatesMissedReversalAndRiskGate(t *testing.T) {
	wait := &store.ReentryAIAnalysis{Verdict: store.ReentryVerdictWait}
	if got := classifyDecisionOutcome(wait, nil, store.ReentryMarketReversal, "ACTIONABLE_SNAPSHOT", false, nil, nil); got != "WAIT_DELAYED_REVERSAL" {
		t.Fatalf("WAIT reversal classification=%s", got)
	}
	enter := &store.ReentryAIAnalysis{Verdict: store.ReentryVerdictEnter}
	if got := classifyDecisionOutcome(enter, nil, store.ReentryMarketAgainst, "PREFLIGHT_REJECTED", false, nil, nil); got != "RISK_GATE_SAVED_LOSS" {
		t.Fatalf("risk gate classification=%s", got)
	}
	abandon := &store.ReentryAIAnalysis{Verdict: store.ReentryVerdictAbandon}
	if got := classifyDecisionOutcome(abandon, nil, store.ReentryMarketAgainst, "ACTIONABLE_SNAPSHOT", false, nil, nil); got != "ABANDON_CORRECT" {
		t.Fatalf("ABANDON classification=%s", got)
	}
}

func TestEvaluateCyclePersistsImmutableOutcomeAndEventsOnce(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "evaluation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Trader().Create(&store.Trader{ID: "trader-1", UserID: "user-1", Name: "主账户-A", AIModelID: "ai", ExchangeID: "ex", InitialBalance: 1000, ScanIntervalMinutes: 1}); err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "position-1", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardAIWaiting, PolicySnapshot: "{}", AccountEquity: 1000, LastObservedPrice: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := st.ReentryAI().EnsureReentryCandidate(&store.CopyGuardReentryCandidate{CycleID: cycle.ID, TraderID: cycle.TraderID, LeaderPosID: cycle.LeaderPosID, Symbol: cycle.Symbol, Side: cycle.Side, TriggerPrice: 100, ATR: 1, MaxNotional: 50, Protectable: true, FeatureHash: "f"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := st.ReentryAI().SaveReentryAnalysis(&store.ReentryAIAnalysis{
		CandidateID: candidate.ID, TraderID: cycle.TraderID, CycleID: cycle.ID, Symbol: cycle.Symbol, Side: cycle.Side, AttemptNo: 1, DecisionGeneration: 1, CallStatus: "PENDING", DatapackJSON: `{"copy_guard":{"gate_atr_okx":1,"recommended_notional_usdt":50,"new_stop_protectable_precheck":true,"leader":{"still_holding_same_side":true}}}`, PromptVersion: "copy_guard_ai_v1", SnapshotPrice: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReentryAI().UpdateReentryInternalResult(analysis.ID, `{}`, store.ReentryVerdictAbandon, .9, `{}`); err != nil {
		t.Fatal(err)
	}
	completed, err := st.ReentryAI().GetReentryAnalysis(analysis.ID)
	if err != nil || completed.ModelCompletedAt == nil {
		t.Fatalf("completed analysis missing model timestamp: %+v err=%v", completed, err)
	}
	// SQLite timestamps are second-granularity; ensure the terminal decision
	// window is positive without reaching into Store internals.
	time.Sleep(1100 * time.Millisecond)
	if err := st.CopyTrade().CloseCopyGuardCycle(cycle.ID, store.CopyGuardLeaderClosed, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		summary, evalErr := EvaluateCycleAIDecisions(st, cycle.ID)
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		if summary.TotalDecisions != 1 || summary.UnscorableDecisions != 1 {
			t.Fatalf("unexpected summary: %+v", summary)
		}
	}
	evaluations, err := st.ReentryAI().ListReentryDecisionEvaluationsByCycle(cycle.ID)
	if err != nil || len(evaluations) != 3 {
		t.Fatalf("immutable evaluation count=%d err=%v", len(evaluations), err)
	}
	for _, evaluation := range evaluations {
		if evaluation.TraderNameSnapshot != "主账户-A" || evaluation.DecisionOutcome != "UNSCORABLE" || evaluation.DataQuality != "UNSCORABLE" {
			t.Fatalf("unexpected evaluation: %+v", evaluation)
		}
		if !evaluation.WindowStartAt.Equal(*completed.ModelCompletedAt) {
			t.Fatalf("evaluation used stale pre-model snapshot time: start=%s completed=%s", evaluation.WindowStartAt, *completed.ModelCompletedAt)
		}
	}
	stats, err := st.ReentryAI().GetReentryAIStats([]string{"trader-1"})
	if err != nil || stats.CandidateEvaluated != 1 || stats.CandidateUnscorable != 1 || stats.CandidateEvaluationOutcomes["UNSCORABLE"] != 1 {
		t.Fatalf("evaluation stats=%+v err=%v", stats, err)
	}
	events, err := st.CopyTrade().ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	if counts["AI_DECISION_OUTCOME_FINALIZED"] != 1 || counts["AI_CANDIDATE_OUTCOME_FINALIZED"] != 1 {
		t.Fatalf("outcome events not idempotent: %+v", counts)
	}
}
