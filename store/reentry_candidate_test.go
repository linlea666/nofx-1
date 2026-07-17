package store

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReentryCandidateLifecycleAndRiskReservation(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()

	firstReview := time.Now().Add(5 * time.Minute).UTC()
	candidate, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{
		CycleID: 7, TraderID: "trader-a", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		TriggerPrice: 2000, ATR: 40, MaxNotional: 100, StopCount: 1, ReentryCount: 0,
		LeaderSize: 2, LeaderEntryPrice: 1980, LastStopPrice: 1900, Protectable: true,
		FeatureHash: "initial", PendingTrigger: "STOP_FLAT_CONFIRMED",
	}, firstReview)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Status != ReentryCandidateWatching || candidate.FeatureHash != "initial" {
		t.Fatalf("unexpected candidate: %+v", candidate)
	}

	// A market event during cooldown may update the candidate but must not call
	// the model before the configured first-review timestamp.
	candidate, err = rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{
		CycleID: 7, TraderID: "trader-a", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		TriggerPrice: 2010, ATR: 41, MaxNotional: 90, StopCount: 1, ReentryCount: 0,
		LeaderSize: 2, LeaderEntryPrice: 1980, LastStopPrice: 1900, Protectable: true,
		FeatureHash: "recovery", PendingTrigger: "STOP_RECOVERY",
	}, firstReview)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.NextReviewAt.Before(firstReview.Add(-time.Second)) {
		t.Fatalf("event bypassed cooldown: next=%s first=%s", candidate.NextReviewAt, firstReview)
	}

	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET next_review_at=CURRENT_TIMESTAMP WHERE id=?`, candidate.ID); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := rs.ClaimReentryCandidateReview(candidate.ID, 5*time.Minute, 12, 30)
	if err != nil || !ok || claimed == nil || claimed.Status != ReentryCandidateReviewing {
		t.Fatalf("claim failed: ok=%v candidate=%+v err=%v", ok, claimed, err)
	}
	analysis, err := rs.SaveReentryAnalysis(&ReentryAIAnalysis{CandidateID: candidate.ID, TraderID: "trader-a", CycleID: 7, Symbol: "ETHUSDT", Side: "long", SystemPrompt: "sys", UserPrompt: "user", DatapackJSON: "{}", PromptVersion: "v2", SnapshotPrice: 2010})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.FinishReentryCandidateReview(candidate.ID, ReentryCandidateDecision{Decision: ReentryVerdictEnter, Regime: "REVERSAL", Confidence: .86, SizeFactor: .5, EntryPriceLow: 2000, EntryPriceHigh: 2020, AttentionPriceLow: 1980, AttentionPriceHigh: 1990, NextReview: time.Now().Add(15 * time.Minute), AnalysisID: analysis.ID, TTLSeconds: 30, CandleKey: "candle-1", EnterApproved: true}); err != nil {
		t.Fatal(err)
	}
	candidate, err = rs.GetReentryCandidate(candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Status != ReentryCandidateEntryPending || candidate.DecisionTTLSeconds != 30 || candidate.LastAnalysisID != analysis.ID || candidate.SizeFactor != .5 {
		t.Fatalf("decision not persisted: %+v", candidate)
	}
	if completed, err := rs.CompleteReentryCandidate(candidate.ID); err != nil || !completed {
		t.Fatalf("first protected completion must win exactly once: completed=%v err=%v", completed, err)
	}
	if completed, err := rs.CompleteReentryCandidate(candidate.ID); err != nil || completed {
		t.Fatalf("duplicate protected completion must be a no-op: completed=%v err=%v", completed, err)
	}

	if err := rs.ReserveCopyGuardRisk("trader-a", 7, 1, 2, 100, .02, .05, .08); err != nil {
		t.Fatalf("valid reservation rejected: %v", err)
	}
	if excluding, err := rs.GetCopyGuardRiskUsageExcludingAttempt("trader-a", 7, 1); err != nil || excluding.CycleUsedUSD != 0 || excluding.PortfolioUsedUSD != 0 {
		t.Fatalf("resizing an active attempt must exclude its own reservation: usage=%+v err=%v", excluding, err)
	}
	if other, err := rs.GetCopyGuardRiskUsageExcludingAttempt("trader-a", 7, 2); err != nil || other.CycleUsedUSD != 2 || other.PortfolioUsedUSD != 2 {
		t.Fatalf("a new attempt must see existing cycle and portfolio risk: usage=%+v err=%v", other, err)
	}
	if err := rs.ConsumeCopyGuardRisk(7, 1); err != nil {
		t.Fatal(err)
	}
	usage, err := rs.GetCopyGuardRiskUsage("trader-a", 7)
	if err != nil || usage.CycleUsedUSD != 2 || usage.PortfolioUsedUSD != 0 {
		t.Fatalf("consumed attempt must remain in cycle budget but leave live portfolio exposure: usage=%+v err=%v", usage, err)
	}
	if err := rs.ReserveCopyGuardRisk("trader-a", 7, 2, 4, 100, .04, .05, .08); err == nil {
		t.Fatal("cycle budget must reject cumulative risk above 5% after the prior stop")
	}
}

func TestLowConfidenceEnterRemainsWaitingWithoutFailure(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate-low-confidence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	candidate, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 701, TraderID: "trader-a", LeaderPosID: "p", Symbol: "BTCUSDT", Side: "long", FeatureHash: "a"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=? WHERE id=?`, ReentryCandidateReviewing, candidate.ID); err != nil {
		t.Fatal(err)
	}
	decision := ReentryCandidateDecision{Decision: ReentryVerdictEnter, Confidence: .74, SizeFactor: .5, NextReview: time.Now().Add(15 * time.Minute), AnalysisID: 9, TTLSeconds: 30, EnterApproved: false}
	if err := rs.FinishReentryCandidateReview(candidate.ID, decision); err != nil {
		t.Fatal(err)
	}
	fresh, err := rs.GetReentryCandidate(candidate.ID)
	if err != nil || fresh.Status != ReentryCandidateWaiting || fresh.FailureCount != 0 || fresh.LastDecision != ReentryVerdictEnter {
		t.Fatalf("low-confidence ENTER should remain an audited non-failure WAIT: candidate=%+v err=%v", fresh, err)
	}
}

func TestCandidateOperatorTransitionsRejectInFlightEntry(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate-operator-transition.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	candidate, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 711, TraderID: "trader-a", Symbol: "BTCUSDT", Side: "long", FeatureHash: "a"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=? WHERE id=?`, ReentryCandidateEntryPending, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.PauseReentryCandidate(candidate.ID); err == nil {
		t.Fatal("ENTRY_PENDING candidate must not be paused while its order may be in flight")
	}
	if err := rs.TerminateReentryCandidate(candidate.ID); err == nil {
		t.Fatal("ENTRY_PENDING candidate must not be terminated while its order may be in flight")
	}
	fresh, _ := rs.GetReentryCandidate(candidate.ID)
	if fresh.Status != ReentryCandidateEntryPending {
		t.Fatalf("operator race changed in-flight status: %s", fresh.Status)
	}
}

func TestOperatorReviewRequestUsesSchedulerAndMinimumInterval(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate-request-review.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	candidate, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 712, TraderID: "trader-a", Symbol: "BTCUSDT", Side: "long", FeatureHash: "a"}, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	lastReview := time.Now().UTC().Truncate(time.Second)
	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_review_at=?,next_review_at=? WHERE id=?`, ReentryCandidateWaiting, lastReview, time.Now().Add(2*time.Hour), candidate.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := rs.RequestImmediateReentryCandidateReview(candidate.ID, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := lastReview.Add(5 * time.Minute)
	if fresh.Status != ReentryCandidateWaiting || fresh.PendingTrigger != "OPERATOR_REVIEW_REQUEST" || fresh.NextReviewAt.Before(want.Add(-time.Second)) || fresh.NextReviewAt.After(want.Add(time.Second)) {
		t.Fatalf("review request bypassed scheduler/min interval: candidate=%+v want=%s", fresh, want)
	}
	if fresh.ReviewCount != 0 || fresh.DecisionGeneration != 0 {
		t.Fatalf("HTTP-side request must not claim quota or a review lease: %+v", fresh)
	}
	if err := rs.PauseReentryCandidate(candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.RequestImmediateReentryCandidateReview(candidate.ID, 5*time.Minute); err == nil {
		t.Fatal("paused candidate must reject production review request")
	}
}

func TestPreflightRejectionReturnsToWaitingWithoutAIFailure(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate-preflight.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	candidate, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 702, TraderID: "trader-a", LeaderPosID: "p", Symbol: "ETHUSDT", Side: "short", FeatureHash: "a"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,failure_count=0 WHERE id=?`, ReentryCandidateEntryPending, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.RejectReentryCandidatePreflight(candidate.ID, "price left AI entry range", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	fresh, err := rs.GetReentryCandidate(candidate.ID)
	if err != nil || fresh.Status != ReentryCandidateWaiting || fresh.FailureCount != 0 || fresh.LastError == "" {
		t.Fatalf("preflight rejection must be an audited non-failure WAIT: candidate=%+v err=%v", fresh, err)
	}
}

func TestCandidateStaleLeaseRecovery(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate-lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-a", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", Status: CopyGuardAIWaiting})
	if err != nil {
		t.Fatal(err)
	}
	c, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: cycle.ID, TraderID: "trader-a", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", FeatureHash: "same"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-20 * time.Minute).UTC().Format("2006-01-02 15:04:05")
	analysis, err := rs.SaveReentryAnalysis(&ReentryAIAnalysis{CandidateID: c.ID, TraderID: c.TraderID, CycleID: c.CycleID, Symbol: c.Symbol, Side: c.Side, DataHash: "stale-preparation"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,review_count=1,updated_at=? WHERE id=?`, ReentryCandidateReviewing, old, c.ID); err != nil {
		t.Fatal(err)
	}
	// Engine ticks may refresh market features while a model call is in flight;
	// they must not overwrite the leased snapshot or keep its lease alive.
	if _, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: cycle.ID, TraderID: "trader-a", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", FeatureHash: "new-tick"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := rs.RecoverStaleReentryCandidateLeases(10 * time.Minute); err != nil || n != 1 {
		t.Fatalf("stale review lease not recovered: n=%d err=%v", n, err)
	}
	fresh, err := rs.GetReentryCandidate(c.ID)
	if err != nil || fresh.Status != ReentryCandidateWaiting || fresh.PendingTrigger != "LEASE_RECOVERED" {
		t.Fatalf("unexpected recovered candidate: candidate=%+v err=%v", fresh, err)
	}
	if fresh.ReviewCount != 0 {
		t.Fatalf("pre-model stale lease must return lifecycle quota: %+v", fresh)
	}
	recoveredAnalysis, err := rs.GetReentryAnalysis(analysis.ID)
	if err != nil || recoveredAnalysis.CallStatus != "PREPARE_FAILED" {
		t.Fatalf("stale pending analysis was not closed: analysis=%+v err=%v", recoveredAnalysis, err)
	}
	running, err := rs.SaveReentryAnalysis(&ReentryAIAnalysis{CandidateID: c.ID, TraderID: c.TraderID, CycleID: c.CycleID, Symbol: c.Symbol, Side: c.Side, DataHash: "stale-running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.MarkReentryAnalysisRunning(running.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,review_count=1,updated_at=? WHERE id=?`, ReentryCandidateReviewing, old, c.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := rs.RecoverStaleReentryCandidateLeases(10 * time.Minute); err != nil || n != 1 {
		t.Fatalf("stale running lease not recovered: n=%d err=%v", n, err)
	}
	fresh, _ = rs.GetReentryCandidate(c.ID)
	if fresh.ReviewCount != 1 {
		t.Fatalf("a possibly billed model call must retain quota: %+v", fresh)
	}
	running, err = rs.GetReentryAnalysis(running.ID)
	if err != nil || running.CallStatus != "FAILED" {
		t.Fatalf("stale running analysis was not failed: analysis=%+v err=%v", running, err)
	}

	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,updated_at=? WHERE id=?`, ReentryCandidateEntryPending, old, c.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := rs.RecoverStaleReentryCandidateLeases(10 * time.Minute); err != nil || n != 1 {
		t.Fatalf("pre-order entry lease not recovered: n=%d err=%v", n, err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,updated_at=? WHERE id=?`, ReentryCandidateEntryPending, old, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, CopyGuardReentryPending, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := rs.RecoverStaleReentryCandidateLeases(10 * time.Minute); err != nil || n != 0 {
		t.Fatalf("in-flight order state must remain owned by order recovery: n=%d err=%v", n, err)
	}
}

func TestCandidateDataHashDedupeDoesNotConsumeQuota(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate-hash.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	c, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 77, TraderID: "trader-a", LeaderPosID: "pos", Symbol: "BTCUSDT", Side: "long", FeatureHash: "heartbeat"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	first, err := rs.SaveReentryAnalysis(&ReentryAIAnalysis{CandidateID: c.ID, TraderID: c.TraderID, CycleID: c.CycleID, DataHash: "same-market-state", DatapackJSON: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.UpdateReentryInternalResult(first.ID, `{"decision":"WAIT"}`, ReentryVerdictWait, .8, "[]"); err != nil {
		t.Fatal(err)
	}
	second, err := rs.SaveReentryAnalysis(&ReentryAIAnalysis{CandidateID: c.ID, TraderID: c.TraderID, CycleID: c.CycleID, DataHash: "same-market-state", DatapackJSON: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := rs.HasCompletedCandidateDataHash(c.ID, second.ID, second.DataHash)
	if err != nil || !duplicate {
		t.Fatalf("completed identical snapshot not detected: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,review_count=1 WHERE id=?`, ReentryCandidateReviewing, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.SkipDuplicateCandidateReview(c.ID, second.ID, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	fresh, err := rs.GetReentryCandidate(c.ID)
	if err != nil || fresh.Status != ReentryCandidateWaiting || fresh.ReviewCount != 0 {
		t.Fatalf("duplicate snapshot consumed lifecycle quota: candidate=%+v err=%v", fresh, err)
	}
	skipped, err := rs.GetReentryAnalysis(second.ID)
	if err != nil || skipped.CallStatus != "SKIPPED" || skipped.DataHash != "same-market-state" {
		t.Fatalf("skipped analysis audit missing: analysis=%+v err=%v", skipped, err)
	}
}

func TestCandidatePreparationFailureDoesNotConsumeCallQuota(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate-prepare-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	candidate, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 78, TraderID: "trader-a", LeaderPosID: "p", Symbol: "BTCUSDT", Side: "long", FeatureHash: "prepare"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := rs.ClaimReentryCandidateReview(candidate.ID, 0, 1, 1)
	if err != nil || !ok || claimed == nil {
		t.Fatalf("initial claim failed: candidate=%+v ok=%v err=%v", claimed, ok, err)
	}
	analysis, err := rs.SaveReentryAnalysis(&ReentryAIAnalysis{CandidateID: candidate.ID, TraderID: candidate.TraderID, CycleID: candidate.CycleID, CallStatus: "PENDING"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.FailReentryCandidateBeforeModel(candidate.ID, analysis.ID, "datapack unavailable", 0); err != nil {
		t.Fatal(err)
	}
	fresh, err := rs.GetReentryCandidate(candidate.ID)
	if err != nil || fresh.ReviewCount != 0 || fresh.Status != ReentryCandidateWaiting {
		t.Fatalf("pre-call failure consumed lifecycle quota: candidate=%+v err=%v", fresh, err)
	}
	audit, err := rs.GetReentryAnalysis(analysis.ID)
	if err != nil || audit.CallStatus != "PREPARE_FAILED" {
		t.Fatalf("pre-call audit status missing: analysis=%+v err=%v", audit, err)
	}
	claimed, ok, err = rs.ClaimReentryCandidateReview(candidate.ID, 0, 1, 1)
	if err != nil || !ok || claimed == nil {
		t.Fatalf("pre-call failure incorrectly consumed daily quota: candidate=%+v ok=%v err=%v", claimed, ok, err)
	}
}

func TestPendingManualSignalMigratesToAICandidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	signal, err := st.CopyTrade().SaveManualReentrySignal(&CopyGuardManualReentrySignal{CycleID: 99, TraderID: "trader-a", LeaderPosID: "leader-pos", Symbol: "BTCUSDT", Side: "short", MarginMode: "cross", TriggerPrice: 60000, ATR: 1200, RecommendedNotional: 50, StopCount: 1, Protectable: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	candidate, err := st.ReentryAI().GetReentryCandidateByCycle(99)
	if err != nil || candidate.MaxNotional != 50 || candidate.PendingTrigger != "MIGRATED_MANUAL" {
		t.Fatalf("pending signal not migrated: candidate=%+v err=%v", candidate, err)
	}
	migrated, err := st.CopyTrade().GetManualReentrySignal(signal.ID)
	if err != nil || migrated.Status != "MIGRATED" {
		t.Fatalf("legacy signal status not retired: signal=%+v err=%v", migrated, err)
	}
}

func TestListReentryCandidatesFiltersByTraderAndStatus(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "candidate-list.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	watching, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 501, TraderID: "trader-a", LeaderPosID: "a", Symbol: "BTCUSDT", Side: "long", FeatureHash: "a"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	paused, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 502, TraderID: "trader-a", LeaderPosID: "b", Symbol: "ETHUSDT", Side: "short", FeatureHash: "b"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.MarkReentryCandidateStatus(paused.ID, ReentryCandidatePaused, "operator pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 503, TraderID: "trader-b", LeaderPosID: "c", Symbol: "SOLUSDT", Side: "long", FeatureHash: "c"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := rs.ListReentryCandidatesByTraders([]string{"trader-a"}, []string{ReentryCandidateWatching}, 10)
	if err != nil || len(got) != 1 || got[0].ID != watching.ID {
		t.Fatalf("status/trader filter mismatch: candidates=%+v err=%v", got, err)
	}
}

func TestAbandonRequiresTwoHighConfidenceDistinctClosedCandles(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "abandon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	c, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 8, TraderID: "trader-a", Symbol: "BTCUSDT", Side: "long", FeatureHash: "a"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	apply := func(confident bool, candle string) *CopyGuardReentryCandidate {
		t.Helper()
		if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=? WHERE id=?`, ReentryCandidateReviewing, c.ID); err != nil {
			t.Fatal(err)
		}
		if err := rs.FinishReentryCandidateReview(c.ID, ReentryCandidateDecision{Decision: ReentryVerdictAbandon, Confidence: .8, NextReview: time.Now(), TTLSeconds: 30, CandleKey: candle, ConfirmAbandon: confident}); err != nil {
			t.Fatal(err)
		}
		fresh, err := rs.GetReentryCandidate(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return fresh
	}
	if got := apply(false, "10:00"); got.ConsecutiveAbandons != 0 {
		t.Fatalf("low confidence ABANDON must not count: %+v", got)
	}
	if got := apply(true, "10:05"); got.ConsecutiveAbandons != 1 {
		t.Fatalf("first qualified candle must count once: %+v", got)
	}
	if got := apply(true, "10:05"); got.ConsecutiveAbandons != 1 {
		t.Fatalf("same closed candle must not count twice: %+v", got)
	}
	if got := apply(true, "10:10"); got.ConsecutiveAbandons != 2 {
		t.Fatalf("second distinct qualified candle must confirm: %+v", got)
	}
}

func TestLateAIResultCannotReviveTerminatedCandidate(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "late-result.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := st.ReentryAI()
	c, err := rs.EnsureReentryCandidate(&CopyGuardReentryCandidate{CycleID: 44, TraderID: "trader-a", Symbol: "ETHUSDT", Side: "short", FeatureHash: "initial"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=? WHERE id=?`, ReentryCandidateReviewing, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.MarkReentryCandidateStatus(c.ID, ReentryCandidateInvalidated, "terminated by operator"); err != nil {
		t.Fatal(err)
	}
	decision := ReentryCandidateDecision{Decision: ReentryVerdictEnter, Confidence: .9, SizeFactor: .5, NextReview: time.Now(), TTLSeconds: 30}
	if err := rs.FinishReentryCandidateReview(c.ID, decision); err == nil {
		t.Fatal("late AI result must be rejected after operator termination")
	}
	if err := rs.FailReentryCandidateReview(c.ID, "late failure", time.Minute); err == nil {
		t.Fatal("late failure must not revive a terminal candidate")
	}
	fresh, err := rs.GetReentryCandidate(c.ID)
	if err != nil || fresh.Status != ReentryCandidateInvalidated {
		t.Fatalf("terminal candidate was revived: candidate=%+v err=%v", fresh, err)
	}
}

func TestPortfolioReservationIsAtomicAndScopedToExchangeAccount(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "portfolio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, trader := range []*Trader{
		{ID: "a", UserID: "u", Name: "a", ExchangeID: "shared"},
		{ID: "b", UserID: "u", Name: "b", ExchangeID: "shared"},
		{ID: "c", UserID: "u", Name: "c", ExchangeID: "other"},
	} {
		if err := st.Trader().Create(trader); err != nil {
			t.Fatal(err)
		}
	}
	rs := st.ReentryAI()
	if err := rs.ReserveCopyGuardRisk("a", 101, 1, 5, 100, .05, .05, .08); err != nil {
		t.Fatal(err)
	}
	if err := rs.ReserveCopyGuardRisk("b", 102, 1, 4, 100, .05, .05, .08); err == nil {
		t.Fatal("two traders sharing one exchange account must not exceed 8% aggregate risk")
	}
	if err := rs.ReserveCopyGuardRisk("c", 103, 1, 4, 100, .05, .05, .08); err != nil {
		t.Fatalf("an independent exchange account must have an independent portfolio budget: %v", err)
	}
}

func TestConcurrentPortfolioReservationCannotPierceBudget(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "portfolio-concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, trader := range []*Trader{
		{ID: "a", UserID: "u", Name: "a", ExchangeID: "shared"},
		{ID: "b", UserID: "u", Name: "b", ExchangeID: "shared"},
	} {
		if err := st.Trader().Create(trader); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var succeeded atomic.Int32
	for i, traderID := range []string{"a", "b"} {
		wg.Add(1)
		go func(traderID string, cycleID int64) {
			defer wg.Done()
			<-start
			if st.ReentryAI().ReserveCopyGuardRisk(traderID, cycleID, 1, 5, 100, .05, .05, .08) == nil {
				succeeded.Add(1)
			}
		}(traderID, int64(200+i))
	}
	close(start)
	wg.Wait()
	if got := succeeded.Load(); got != 1 {
		t.Fatalf("exactly one concurrent 5%% reservation may fit an 8%% shared-account budget, got %d", got)
	}
}

func TestCycleCloseReleasesRiskReservation(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "reservation-close.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-a", LeaderPosID: "pos", Symbol: "ETHUSDT", Side: "long", Status: CopyGuardFollowing})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReentryAI().ReserveCopyGuardRisk("trader-a", cycle.ID, 0, 2, 100, .02, .05, .08); err != nil {
		t.Fatal(err)
	}
	if err := st.ReentryAI().ConsumeCopyGuardRisk(cycle.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().CloseCopyGuardCycle(cycle.ID, CopyGuardLeaderClosed, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	usage, err := st.ReentryAI().GetCopyGuardRiskUsage("trader-a", cycle.ID)
	if err != nil || usage.CycleUsedUSD != 0 || usage.PortfolioUsedUSD != 0 {
		t.Fatalf("closed cycle leaked risk budget: usage=%+v err=%v", usage, err)
	}
}
