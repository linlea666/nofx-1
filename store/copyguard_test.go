package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCopyGuardCycleAndEventLedger(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-1", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 101, FollowerNotional: 1000, AccountEquity: 5000, LastObservedPrice: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.OpenCopyGuardAttempt(cycle.ID, 0, 101, 1000, 10, 2); err != nil {
		t.Fatal(err)
	}
	if err := cs.RecordCopyGuardStop(cycle, 2, 98, -30, 1, 2, "algo-1", map[string]interface{}{"quantity": 10.0}); err != nil {
		t.Fatal(err)
	}
	if err := cs.ReconcileCopyGuardAttempt(cycle.ID, 0, -25, 2, -1, 0); err != nil {
		t.Fatal(err)
	}
	cycle, _ = cs.GetCopyGuardCycle(cycle.ID)
	if cycle.ActualPnL != -25 || cycle.Fees != 2 || cycle.FundingFee != -1 {
		t.Fatalf("attempt reconciliation not reflected in cycle: %+v", cycle)
	}
	if err := cs.RecordCopyGuardReentryFilled(cycle, 99, 500, 5, 2, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != CopyGuardFollowingReentry || got.ReentryCount != 1 || got.StopCount != 1 {
		t.Fatalf("unexpected cycle: %+v", got)
	}
	events, err := cs.ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "STOP_TRIGGERED" || events[1].Type != "REENTRY_FILLED" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestCopyGuardProtectionHealthAndShadowLedger(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-1", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 100, FollowerNotional: 1000, AccountEquity: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardProtectionHealth(cycle.ID, CopyGuardProtectionUnknown, 0.4, "timeout", "follower-pos", "", true); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProtectionStatus != CopyGuardProtectionUnknown || got.ProtectionMissingAt == nil || got.ProtectionRetries != 1 {
		t.Fatalf("unexpected degraded health: %+v", got)
	}
	if _, err := cs.db.Exec(`UPDATE copy_guard_cycles SET protection_missing_at=datetime('now','-1 minute') WHERE id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardProtectionHealth(cycle.ID, CopyGuardProtectionVerified, 1, "", "follower-pos", "", false); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.GetCopyGuardCycle(cycle.ID)
	if got.ProtectionMissingAt != nil || got.ProtectionStatus != CopyGuardProtectionVerified || got.ProtectionMissingSeconds < 50 {
		t.Fatalf("health did not recover: %+v", got)
	}

	if err := cs.UpdateCopyGuardShadow(cycle.ID, 100, 100, 1000, 10); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardShadow(cycle.ID, 100, 110, 500, 5); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.GetCopyGuardCycle(cycle.ID)
	if got.BaselineRealizedPnL < 49.99 || got.BaselineRealizedPnL > 50.01 {
		t.Fatalf("shadow partial close pnl = %v, want 50", got.BaselineRealizedPnL)
	}
	if err := cs.RecordCopyGuardReentryFilled(got, 105, 500, 5, 2, map[string]interface{}{"test": true}); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.GetCopyGuardCycle(cycle.ID)
	attempts, err := cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != CopyGuardFollowingReentry || got.ReentryCount != 1 || len(attempts) != 1 {
		t.Fatalf("transactional reentry incomplete: cycle=%+v attempts=%+v", got, attempts)
	}
	summary, err := cs.CopyGuardSummary([]string{"trader-1"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), CopyGuardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectedCount != 1 || summary.AverageCoverage != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}
