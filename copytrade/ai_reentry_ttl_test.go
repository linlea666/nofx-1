package copytrade

import (
	"testing"
	"time"

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
