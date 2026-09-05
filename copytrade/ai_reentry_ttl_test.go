package copytrade

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

func TestAIDecisionExpiryStartsAtModelCompletion(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(25 * time.Second)
	analysis := &store.ReentryAIAnalysis{SnapshotAt: now.Add(-2 * time.Minute), DecisionExpiresAt: &expires}
	if expired, _ := aiDecisionExpired(analysis, 30, now); expired {
		t.Fatal("slow model latency must not consume the post-completion decision TTL")
	}
	if expired, _ := aiDecisionExpired(analysis, 30, expires.Add(time.Millisecond)); !expired {
		t.Fatal("decision must expire after persisted completion-based deadline")
	}
}

func TestAIDecisionExpiryLegacyFallback(t *testing.T) {
	now := time.Now().UTC()
	analysis := &store.ReentryAIAnalysis{SnapshotAt: now.Add(-31 * time.Second)}
	if expired, _ := aiDecisionExpired(analysis, 30, now); !expired {
		t.Fatal("legacy rows without completion timestamps must retain snapshot TTL")
	}
}

func TestAIEnterPriceLeaseReusesExactUnsubmittedIntentUntilAuthoritativeExpiry(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "ai-entry-lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO traders
		(id,user_id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation)
		VALUES(?,?,?,?,?,?,1,?,1)`,
		"trader-1", "test-user", "test trader", "test-model", "test-exchange", 1000,
		store.TraderLifecycleRunning); err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{PolicySnapshot: `{"version":4}`,
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "short", MarginMode: "cross",
		Status: store.CopyGuardAIWaiting,
	})
	if err != nil {
		t.Fatal(err)
	}
	rs := st.ReentryAI()
	candidate, err := rs.EnsureReentryCandidate(&store.CopyGuardReentryCandidate{
		CycleID: cycle.ID, TraderID: "trader-1", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "short", MarginMode: "cross",
		FeatureHash: "lease", PendingTrigger: "STOP_RECOVERY",
	}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	candidate, claimed, err := rs.ClaimReentryCandidateReview(candidate.ID, 5*time.Minute, 12, 30)
	if err != nil || !claimed {
		t.Fatalf("claim candidate=%+v claimed=%v err=%v", candidate, claimed, err)
	}
	analysis, err := rs.SaveReentryAnalysis(&store.ReentryAIAnalysis{
		CandidateID: candidate.ID, TraderID: "trader-1", CycleID: cycle.ID,
		Symbol: "ETHUSDT", Side: "short", DecisionGeneration: candidate.DecisionGeneration,
		SystemPrompt: "sys", UserPrompt: "user", DatapackJSON: "{}",
		PromptVersion: "v2", SnapshotPrice: 1905,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = rs.CompleteReentryInternalResult(analysis.ID, `{}`, store.ReentryVerdictEnter, .84, `[]`, 30); err != nil {
		t.Fatal(err)
	}
	if err = rs.FinishReentryCandidateReview(candidate.ID, store.ReentryCandidateDecision{
		Decision: store.ReentryVerdictEnter, Confidence: .84, SizeFactor: .5,
		EntryPriceLow: 1900, EntryPriceHigh: 1910,
		NextReview: time.Now().Add(15 * time.Minute), AnalysisID: analysis.ID,
		TTLSeconds: 30, EnterApproved: true,
	}); err != nil {
		t.Fatal(err)
	}
	candidate, err = rs.GetReentryCandidate(candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent, reserved, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-pos", SourceRevision: 1_000_001,
		SourceKind: "AI_REENTRY", CanonicalKey: "ai|trader-1|lease",
		CycleID: cycle.ID, CandidateID: candidate.ID, AnalysisID: analysis.ID,
		AttemptNo: 1, DecisionGeneration: candidate.DecisionGeneration,
		Action: "open_short", Symbol: "ETHUSDT", Side: "short",
		ClientOrderID: "stable-ai-client",
	})
	if err != nil || !reserved {
		t.Fatalf("reserve intent=%+v reserved=%v err=%v", intent, reserved, err)
	}
	ti := &TraderIntegration{traderID: "trader-1", store: st}
	if !ti.canReplayAIEntryLease(intent) {
		t.Fatal("fresh exact ENTER lease was not replayable")
	}
	dec := &decision.Decision{
		ExecutionIntentID: intent.ID, EntryPrice: 1911,
		ClientOrderID: intent.ClientOrderID,
	}
	if !ti.deferAIEntryLeaseForPrice(dec, aiReentryPreflightError("PRICE_OUT_OF_RANGE", "price outside approved range")) {
		t.Fatal("temporary price excursion did not preserve ENTER lease")
	}
	stored, err := st.CopyTrade().GetExecutionIntentByID(intent.ID)
	if err != nil || stored.Status != store.ExecutionIntentReconciling ||
		stored.ReasonCode != "AI_ENTRY_LEASE_WAITING_PRICE" ||
		stored.ClientOrderID != intent.ClientOrderID {
		t.Fatalf("lease identity/state changed: %+v err=%v", stored, err)
	}
	attempts, err := st.CopyTrade().ListExecutionOrderAttempts(intent.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("price-only wait must not create an order attempt: %+v err=%v", attempts, err)
	}

	if _, err = st.DB().Exec(`UPDATE reentry_ai_analyses SET decision_expires_at=datetime('now','-1 second') WHERE id=?`, analysis.ID); err != nil {
		t.Fatal(err)
	}
	if ti.canReplayAIEntryLease(stored) {
		t.Fatal("expired authoritative analysis lease remained replayable")
	}
	if err = rs.ExpireEntryLease(candidate.ID); err != nil {
		t.Fatal(err)
	}
	ti.expireAIEntryExecutionLease(candidate)
	stored, err = st.CopyTrade().GetExecutionIntentByID(intent.ID)
	if err != nil || stored.Status != store.ExecutionIntentFailed ||
		stored.ReasonCode != "ENTER_WINDOW_EXPIRED" {
		t.Fatalf("expired ENTER lease did not terminalize safely: %+v err=%v", stored, err)
	}
}
