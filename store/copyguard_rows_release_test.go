package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCopyGuardIDListsReleaseRowsBeforeLoadingCycles(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-row-release.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance) VALUES('trader','Trader','model','exchange',100)`); err != nil {
		t.Fatal(err)
	}
	insertCycle := func(posID, status, accounting string, closed bool) int64 {
		t.Helper()
		closedAt := interface{}(nil)
		if closed {
			closedAt = time.Now().UTC().Format(time.RFC3339)
		}
		result, err := st.DB().Exec(`INSERT INTO copy_guard_cycles(
			trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,
			accounting_status,closed_at
		) VALUES('trader','leader',?,'BTCUSDT','long','cross',?,?,?)`, posID, status, accounting, closedAt)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	summaryID := insertCycle("summary", CopyGuardLeaderClosed, CopyGuardAccountingReconciled, true)
	if _, err := st.DB().Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type) VALUES(?,'trader','CYCLE_CLOSED_SUMMARY')`, summaryID); err != nil {
		t.Fatal(err)
	}
	unreconciledID := insertCycle("unreconciled", CopyGuardStoppedWatching, CopyGuardAccountingPending, false)
	if _, err := st.DB().Exec(`INSERT INTO copy_guard_attempts(cycle_id,attempt_no,status,reconciled) VALUES(?,1,'STOPPED',0)`, unreconciledID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		cycles []*CopyGuardCycle
		err    error
	}
	assertReturns := func(name string, call func() ([]*CopyGuardCycle, error), wantID int64) {
		t.Helper()
		ch := make(chan result, 1)
		go func() {
			cycles, err := call()
			ch <- result{cycles: cycles, err: err}
		}()
		select {
		case got := <-ch:
			if got.err != nil {
				t.Fatalf("%s: %v", name, got.err)
			}
			if len(got.cycles) != 1 || got.cycles[0].ID != wantID {
				t.Fatalf("%s cycles=%v want id=%d", name, got.cycles, wantID)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s deadlocked by querying cycles before releasing ID rows", name)
		}
	}

	assertReturns("pending summary", func() ([]*CopyGuardCycle, error) {
		return st.CopyTrade().ListCopyGuardCyclesPendingSummaryEmail("trader", 10)
	}, summaryID)
	assertReturns("unreconciled stops", func() ([]*CopyGuardCycle, error) {
		return st.CopyTrade().ListCopyGuardCyclesWithUnreconciledStops("trader")
	}, unreconciledID)
}
