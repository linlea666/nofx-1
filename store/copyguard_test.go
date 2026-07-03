package store

import (
	"database/sql"
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
	if err := cs.UpdateCopyGuardAttemptIdentity(cycle.ID, 0, "follower-pos-0", "entry-0", ""); err != nil {
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
	if err := cs.UpdateCopyGuardAttemptIdentity(cycle.ID, 1, "follower-pos-1", "entry-1", ""); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != CopyGuardFollowingReentry || got.ReentryCount != 1 || got.StopCount != 1 {
		t.Fatalf("unexpected cycle: %+v", got)
	}
	attempts, err := cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 2 || attempts[0].FollowerPosID != "follower-pos-0" || attempts[1].FollowerPosID != "follower-pos-1" {
		t.Fatalf("attempt identities were overwritten: %+v, %v", attempts, err)
	}
	events, err := cs.ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "STOP_TRIGGERED" || events[1].Type != "REENTRY_FILLED" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestParseDBTimeSupportsSQLiteAndRFC3339(t *testing.T) {
	for _, value := range []string{"2026-07-03 13:05:43", "2026-07-03T13:05:43Z", "2026-07-03T13:05:43.123456789Z"} {
		got, err := parseDBTime(value)
		if err != nil || got.Year() != 2026 {
			t.Fatalf("parseDBTime(%q) = %v, %v", value, got, err)
		}
	}
	if _, err := parseDBTime("not-a-time"); err == nil {
		t.Fatal("invalid database timestamp must not silently become zero")
	}
	if got, err := parseNullableDBTime(sql.NullString{}); err != nil || got != nil {
		t.Fatalf("null timestamp = %v, %v", got, err)
	}
}

func TestCopyGuardAccountingNoStopHasZeroGuardEffect(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-accounting.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-accounting", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 1734, FollowerEntryPrice: 1735.19, FollowerNotional: 286.99, AccountEquity: 18})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.OpenCopyGuardAttempt(cycle.ID, 0, 1735.19, 286.99, 0.165, 8); err != nil {
		t.Fatal(err)
	}
	if err := cs.BeginCopyGuardAccounting(cycle.ID, CopyGuardLeaderClosed, "exit-order", 1.986091); err != nil {
		t.Fatal(err)
	}
	pendingSummary, err := cs.CopyGuardSummary([]string{"trader-1"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), CopyGuardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if pendingSummary.ActualPnL != 0 || pendingSummary.BaselinePnL != 0 || pendingSummary.AccountingPendingCount != 1 {
		t.Fatalf("pending accounting polluted summary: %+v", pendingSummary)
	}
	if err := cs.CompleteCopyGuardAccounting(cycle.ID, 0, 1746.73, 1.61, 0.29, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountingStatus != CopyGuardAccountingReconciled || got.ActualPnL != 1.61 || got.NetGuardEffect != 0 {
		t.Fatalf("unexpected reconciled cycle: %+v", got)
	}
	if got.TrackingDifference > -0.37 || got.TrackingDifference < -0.39 {
		t.Fatalf("tracking difference = %v, want about -0.376", got.TrackingDifference)
	}
	summary, err := cs.CopyGuardSummary([]string{"trader-1"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), CopyGuardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ActualPnL != 1.61 || summary.OpportunityCost != 0 || summary.NetGuardEffect != 0 {
		t.Fatalf("no-stop cycle polluted guard metrics: %+v", summary)
	}
}

func TestCopyGuardProtectionRetryIsAtomic(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-retry", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := cs.BeginCopyGuardProtectionRetry(cycle, 0)
	if err != nil || !claimed {
		t.Fatalf("first retry claim = %v, %v", claimed, err)
	}
	claimed, err = cs.BeginCopyGuardProtectionRetry(cycle, 0)
	if err != nil || claimed {
		t.Fatalf("stale retry claim = %v, %v", claimed, err)
	}
	got, _ := cs.GetCopyGuardCycle(cycle.ID)
	events, _ := cs.ListCopyGuardEvents(cycle.ID)
	if got.ProtectionRetries != 1 || len(events) != 1 || events[0].Type != "PROTECTION_RETRY" {
		t.Fatalf("retry was not atomic: cycle=%+v events=%+v", got, events)
	}
}

func TestExistingClosedCopyGuardCycleBecomesLegacyUnverified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copyguard-legacy.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "legacy-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_cycles SET status='LEADER_CLOSED',closed_at=CURRENT_TIMESTAMP,accounting_status='OPEN' WHERE id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountingStatus != CopyGuardAccountingLegacyUnverified {
		t.Fatalf("legacy cycle status = %s", got.AccountingStatus)
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
