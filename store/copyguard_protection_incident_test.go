package store

import (
	"path/filepath"
	"testing"
	"time"
)

func protectionIncidentFixture(t *testing.T) (*Store, *CopyGuardCycle, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "protection-mail.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	policy, _ := EncodeCopyGuardPolicySnapshot(NewCopyGuardDefaults())
	c, err := s.CopyTrade().EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "mail-trader", LeaderID: "leader", LeaderPosID: "source", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: policy, ProtectionStatus: CopyGuardProtectionDegraded})
	if err != nil {
		t.Fatal(err)
	}
	return s, c, path
}

func TestProtectionIncidentDeliverySurvivesHourBoundaryAndRestart(t *testing.T) {
	s, c, path := protectionIncidentFixture(t)
	cs := s.CopyTrade()
	now := time.Date(2026, 1, 1, 12, 59, 40, 0, time.UTC)
	claim, err := cs.ClaimProtectionMail(c.ID, 0, 1, false, now)
	if err != nil || claim == nil {
		t.Fatalf("first: %+v %v", claim, err)
	}
	if c2, err := cs.ClaimProtectionMail(c.ID, 0, 2, false, now.Add(time.Second)); err != nil || c2 != nil {
		t.Fatal("in-flight mail duplicated")
	}
	if err = cs.RecordProtectionMailDelivery(claim, "sent", "", now); err != nil {
		t.Fatal(err)
	}
	for _, later := range []time.Time{now.Add(30 * time.Second), now.Add(40 * time.Minute)} {
		if next, err := cs.ClaimProtectionMail(c.ID, 0, 1, false, later); err != nil || next != nil {
			t.Fatal("same incident crossed wall-clock bucket")
		}
	}
	// Reopening storage loses all process memory, not the delivered identity.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cs = reopened.CopyTrade()
	if next, err := cs.ClaimProtectionMail(c.ID, 0, 1, false, now.Add(41*time.Minute)); err != nil || next != nil {
		t.Fatal("restart resent incident")
	}
	escalated, err := cs.ClaimProtectionMail(c.ID, 0, 2, false, now.Add(42*time.Minute))
	if err != nil || escalated == nil || escalated.ID != claim.ID {
		t.Fatalf("severity escalation missing: %+v %v", escalated, err)
	}
	if err = cs.RecordProtectionMailDelivery(escalated, "sent", "", now.Add(42*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if next, err := cs.ClaimProtectionMail(c.ID, 0, 2, false, now.Add(time.Hour)); err != nil || next != nil {
		t.Fatal("reminder did not use last successful delivery")
	}
	if next, err := cs.ClaimProtectionMail(c.ID, 0, 2, false, now.Add(103*time.Minute)); err != nil || next == nil {
		t.Fatal("persistent incident never reminded")
	}
}

func TestProtectionIncidentFailureRecoveryAndStaleQueue(t *testing.T) {
	s, c, _ := protectionIncidentFixture(t)
	cs := s.CopyTrade()
	now := time.Now()
	claim, err := cs.ClaimProtectionMail(c.ID, 0, 1, false, now)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	if err = cs.RecordProtectionMailDelivery(claim, "failed", "SMTP timeout", now); err != nil {
		t.Fatal(err)
	}
	if next, err := cs.ClaimProtectionMail(c.ID, 0, 1, false, now.Add(20*time.Second)); err != nil || next != nil {
		t.Fatal("SMTP failure busy-loop")
	}
	retry, err := cs.ClaimProtectionMail(c.ID, 0, 1, false, now.Add(61*time.Second))
	if err != nil || retry == nil || retry.ID != claim.ID {
		t.Fatal("SMTP failure not retryable")
	}
	if cs.ProtectionMailStillRelevant(claim, now.Add(62*time.Second)) {
		t.Fatal("old claim survived replacement")
	}
	if err = cs.RecordProtectionMailDelivery(retry, "sent", "", now.Add(62*time.Second)); err != nil {
		t.Fatal(err)
	}
	setHealth := func(status string) {
		t.Helper()
		if err := cs.UpdateCopyGuardProtectionHealth(c.ID, status, 1, "", "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	setHealth(CopyGuardProtectionVerified)
	if cs.ProtectionMailStillRelevant(retry, now.Add(63*time.Second)) {
		t.Fatal("recovered position retained warning")
	}
	if next, err := cs.ClaimProtectionMail(c.ID, 0, 0, true, now.Add(63*time.Second)); err != nil || next != nil {
		t.Fatal("unstable recovery immediately split incident")
	}
	setHealth(CopyGuardProtectionDegraded)
	if next, err := cs.ClaimProtectionMail(c.ID, 0, 1, false, now.Add(90*time.Second)); err != nil || next != nil {
		t.Fatal("flapping generated a new first mail")
	}
	setHealth(CopyGuardProtectionVerified)
	_, _ = cs.ClaimProtectionMail(c.ID, 0, 0, true, now.Add(100*time.Second))
	recovered, err := cs.ClaimProtectionMail(c.ID, 0, 0, true, now.Add(161*time.Second))
	if err != nil || recovered == nil {
		t.Fatalf("stable recovery missing %v", err)
	}
	if !cs.ProtectionMailStillRelevant(recovered, now.Add(162*time.Second)) {
		t.Fatal("recovery claim invalid")
	}
	if err = cs.RecordProtectionMailDelivery(recovered, "sent", "", now.Add(162*time.Second)); err != nil {
		t.Fatal(err)
	}
	if again, err := cs.ClaimProtectionMail(c.ID, 0, 0, true, now.Add(240*time.Second)); err != nil || again != nil {
		t.Fatal("duplicate recovery")
	}
	setHealth(CopyGuardProtectionDegraded)
	newClaim, err := cs.ClaimProtectionMail(c.ID, 0, 1, false, now.Add(241*time.Second))
	if err != nil || newClaim == nil || newClaim.ID == claim.ID {
		t.Fatal("new episode remained muted")
	}
	if _, err = s.DB().Exec(`UPDATE copy_guard_cycles SET protection_status='FLAT_RECONCILING' WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if cs.ProtectionMailStillRelevant(newClaim, now.Add(242*time.Second)) {
		t.Fatal("flat queued warning was not canceled")
	}
	if next, err := cs.ClaimProtectionMail(c.ID, 0, 2, false, now.Add(300*time.Second)); err != nil || next != nil {
		t.Fatal("flat pending exits emitted naked-position warning")
	}
	var resolved int64
	if err = s.DB().QueryRow(`SELECT resolved_ms FROM copy_guard_protection_incidents WHERE id=?`, newClaim.ID).Scan(&resolved); err != nil || resolved == 0 {
		t.Fatal("flat did not resolve incident")
	}
}

func TestProtectionIncidentPendingLeaseRetriesAfterCrashAndClosedCycleInvalidates(t *testing.T) {
	s, c, _ := protectionIncidentFixture(t)
	cs := s.CopyTrade()
	now := time.Now()
	claim, _ := cs.ClaimProtectionMail(c.ID, 0, 1, false, now)
	if claim == nil {
		t.Fatal("no initial claim")
	}
	if next, _ := cs.ClaimProtectionMail(c.ID, 0, 1, false, now.Add(4*time.Minute)); next != nil {
		t.Fatal("live lease ignored")
	}
	retry, _ := cs.ClaimProtectionMail(c.ID, 0, 1, false, now.Add(6*time.Minute))
	if retry == nil || retry.Sequence == claim.Sequence {
		t.Fatal("dead lease not recovered")
	}
	if _, err := s.DB().Exec(`UPDATE copy_guard_cycles SET closed_at=CURRENT_TIMESTAMP WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if cs.ProtectionMailStillRelevant(retry, now.Add(7*time.Minute)) {
		t.Fatal("closed cycle remained eligible")
	}
}
