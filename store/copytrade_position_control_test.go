package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPositionControlMigrationNeverReacquiresHistoricalAbsentPosition(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	snapshot, err := EncodeCopyGuardPolicySnapshot(NewCopyGuardDefaults())
	if err != nil {
		t.Fatal(err)
	}
	c, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "t", LeaderID: "l", LeaderPosID: "p", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec(`UPDATE copy_guard_cycles SET initial_intent_id=99 WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err = cs.SaveCopyGuardEvent(&CopyGuardEvent{CycleID: c.ID, TraderID: "t", Type: "FOLLOWER_POSITION_ABSENT"}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err = cs.initPositionControlTables(); err != nil {
			t.Fatal(err)
		}
		p, e := cs.GetPositionCustody("t", "p")
		if e != nil || p.State != "RELEASED" {
			t.Fatalf("historical absence lost on migration: %+v %v", p, e)
		}
		if allowed, e := cs.CopyGuardHasPositionAuthority(c.ID, 0); e != nil || allowed {
			t.Fatalf("migration revived old authority: %v %v", allowed, e)
		}
	}
}

func TestPositionControlDurableGenerationAndQueueCAS(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	id, err := cs.CopyGuardProtectionClientID(626, 0, false)
	if err != nil || id != "cg626a0" {
		t.Fatalf("legacy identity changed: %s %v", id, err)
	}
	next, err := cs.CopyGuardProtectionClientID(626, 0, true)
	if err != nil || next == id || len(next) > 32 {
		t.Fatalf("bad replacement identity: %s %v", next, err)
	}
	retry, err := cs.CopyGuardProtectionClientID(626, 0, false)
	if err != nil || retry != next {
		t.Fatalf("timeout retry changed identity: %s %v", retry, err)
	}
	if err = cs.QueueCopyGuardProtection("t", "p", 1, `{"action":"open_long"}`); err != nil {
		t.Fatal(err)
	}
	jobs, err := cs.ListCopyGuardProtectionJobs("t")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("missing job: %+v %v", jobs, err)
	}
	if err = cs.QueueCopyGuardProtection("t", "p", 2, `{"action":"reduce_long"}`); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteCopyGuardProtectionJob("t", "p", jobs[0].Revision); err != nil {
		t.Fatal(err)
	}
	jobs, err = cs.ListCopyGuardProtectionJobs("t")
	if err != nil || len(jobs) != 1 || jobs[0].CycleID != 2 || jobs[0].Revision != 2 {
		t.Fatalf("old worker deleted newer lifecycle job: %+v %v", jobs, err)
	}
}

func TestPositionContinuityCursorPreservesProofAndRejectsChangedFills(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "proof.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	opened := time.Now().Add(-24 * time.Hour).Truncate(time.Millisecond)
	checked := time.Now().Truncate(time.Millisecond)
	f := CopyGuardContinuityFill{TradeID: "1", OrderID: "entry", Side: "BUY", PositionSide: "LONG", Quantity: 1, TimeMS: opened.UnixMilli()}
	if _, err = cs.MergeCopyGuardContinuityFills(1, 0, checked, []CopyGuardContinuityFill{f}); err != nil {
		t.Fatal(err)
	}
	start, err := cs.CopyGuardContinuityStart(1, 0, "entry", opened)
	if err != nil || !start.Equal(checked.Add(-2*time.Minute)) {
		t.Fatalf("cursor did not advance safely: %v %v", start, err)
	}
	f.Quantity = 2
	if _, err = cs.MergeCopyGuardContinuityFills(1, 0, checked.Add(time.Minute), []CopyGuardContinuityFill{f}); err == nil {
		t.Fatal("changed immutable fill accepted")
	}
	proof, err := cs.MergeCopyGuardContinuityFills(1, 0, checked.Add(-time.Hour), nil)
	if err != nil || len(proof) != 1 || proof[0].Quantity != 1 {
		t.Fatalf("proof not retained: %+v %v", proof, err)
	}
	start, _ = cs.CopyGuardContinuityStart(1, 0, "entry", opened)
	if !start.Equal(checked.Add(-2 * time.Minute)) {
		t.Fatal("failed or older observations moved cursor")
	}
	start, _ = cs.CopyGuardContinuityStart(1, 0, "unconfirmed-entry", opened)
	if !start.Equal(opened.Add(-2 * time.Second)) {
		t.Fatal("missing initial entry skipped history")
	}
}
