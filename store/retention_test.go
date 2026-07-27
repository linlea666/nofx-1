package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newRetentionTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "retention_test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func dbTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&n); err != nil {
		t.Fatalf("failed to count %s: %v", table, err)
	}
	return n
}

func TestRetentionKeepsOpenPositions(t *testing.T) {
	st := newRetentionTestStore(t)
	old := time.Now().AddDate(0, 0, -120)

	// Old CLOSED position: should be deleted.
	oldResult, err := st.db.Exec(`
		INSERT INTO trader_positions (trader_id, exchange_id, symbol, side, quantity, entry_price, entry_time, exit_time, status, updated_at)
		VALUES ('t1','account-1','BTC','LONG',1,100,?,?,'CLOSED',?)`,
		old.Format(time.RFC3339), old.Format(time.RFC3339), old.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	oldPositionID, err := oldResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	fillResult, err := st.db.Exec(`INSERT INTO position_close_fills
		(trader_id,exchange_id,exchange_trade_id,symbol,side,quantity,exit_price,realized_pnl,fill_time)
		VALUES('t1','account-1','old-fill','BTC','LONG',1,90,-10,?)`, old.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	fillID, err := fillResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec(`INSERT INTO position_close_allocations
		(fill_id,exchange_id,exchange_trade_id,position_id,symbol,side,quantity,exit_price,realized_pnl)
		VALUES(?,'account-1','old-fill',?,'BTC','LONG',1,90,-10)`, fillID, oldPositionID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec(`INSERT INTO position_accounting_audits(position_id,reason_code)
		VALUES(?,'TEST_OLD')`, oldPositionID); err != nil {
		t.Fatal(err)
	}
	// Equally old but still OPEN: must survive.
	_, err = st.db.Exec(`
		INSERT INTO trader_positions (trader_id, symbol, side, quantity, entry_price, entry_time, status, updated_at)
		VALUES ('t1','ETH','LONG',1,100,?,'OPEN',?)`,
		old.Format(time.RFC3339), old.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	// Recent CLOSED position: inside the window, must survive.
	recent := time.Now().AddDate(0, 0, -5)
	_, err = st.db.Exec(`
		INSERT INTO trader_positions (trader_id, symbol, side, quantity, entry_price, entry_time, exit_time, status, updated_at)
		VALUES ('t1','SOL','SHORT',1,100,?,?,'CLOSED',?)`,
		recent.Format(time.RFC3339), recent.Format(time.RFC3339), recent.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}

	r := NewRetentionService(st, RetentionPolicy{PositionDays: 90}, "")
	r.RunOnce()

	if got := countRows(t, st, "trader_positions"); got != 2 {
		t.Fatalf("expected 2 surviving positions (open + recent), got %d", got)
	}
	for _, table := range []string{"position_close_allocations", "position_close_fills", "position_accounting_audits"} {
		if got := countRows(t, st, table); got != 0 {
			t.Fatalf("expected old position accounting rows to be removed from %s, got %d", table, got)
		}
	}
	var status string
	if err := st.db.QueryRow(`SELECT status FROM trader_positions WHERE symbol='ETH'`).Scan(&status); err != nil {
		t.Fatalf("open position was deleted: %v", err)
	}
}

func TestRetentionDownsamplesEquity(t *testing.T) {
	st := newRetentionTestStore(t)

	// Two old days with 3 snapshots each, plus one recent snapshot.
	for dayOffset := 100; dayOffset <= 101; dayOffset++ {
		base := time.Now().UTC().AddDate(0, 0, -dayOffset).Truncate(24 * time.Hour)
		for i := 0; i < 3; i++ {
			ts := base.Add(time.Duration(i) * time.Hour)
			_, err := st.db.Exec(`
				INSERT INTO trader_equity_snapshots (trader_id, timestamp, total_equity, balance, unrealized_pnl)
				VALUES ('t1', ?, 1000, 1000, 0)`, ts.Format(time.RFC3339))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	_, err := st.db.Exec(`
		INSERT INTO trader_equity_snapshots (trader_id, timestamp, total_equity, balance, unrealized_pnl)
		VALUES ('t1', ?, 1000, 1000, 0)`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}

	r := NewRetentionService(st, RetentionPolicy{EquityFullDays: 90}, "")
	r.RunOnce()

	// 2 old days downsampled to 1 row each + 1 recent row = 3.
	if got := countRows(t, st, "trader_equity_snapshots"); got != 3 {
		t.Fatalf("expected 3 snapshots after downsampling, got %d", got)
	}
	// Each old day must retain exactly its earliest snapshot.
	var perDay int
	err = st.db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT date(timestamp) d, COUNT(*) c FROM trader_equity_snapshots
			WHERE datetime(timestamp) < datetime('now', '-90 days')
			GROUP BY trader_id, d HAVING c != 1)`).Scan(&perDay)
	if err != nil {
		t.Fatal(err)
	}
	if perDay != 0 {
		t.Fatalf("expected exactly one snapshot per old day, found %d violating days", perDay)
	}
}

func TestRetentionCopyGuardCyclesAndEvents(t *testing.T) {
	st := newRetentionTestStore(t)
	oldTime := dbTime(time.Now().AddDate(0, 0, -200))
	veryOld := dbTime(time.Now().AddDate(0, 0, -400))

	cycleSeq := 0
	insertCycle := func(closedAt interface{}, accounting string) int64 {
		cycleSeq++
		res, err := st.db.Exec(`
			INSERT INTO copy_guard_cycles (trader_id, leader_id, leader_pos_id, symbol, side, margin_mode, status, accounting_status, opened_at, closed_at)
			VALUES ('t1','L1',?,'BTC','long','cross','LEADER_CLOSED',?,?,?)`,
			fmt.Sprintf("pos-%d", cycleSeq), accounting, veryOld, closedAt)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	insertEvent := func(cycleID int64, createdAt string) {
		if _, err := st.db.Exec(`
			INSERT INTO copy_guard_events (cycle_id, trader_id, type, created_at)
			VALUES (?, 't1', 'TEST', ?)`, cycleID, createdAt); err != nil {
			t.Fatal(err)
		}
	}

	reconciledOld := insertCycle(oldTime, CopyGuardAccountingReconciled) // deletable
	delayedOld := insertCycle(oldTime, CopyGuardAccountingDelayed)       // grace period, kept
	delayedVeryOld := insertCycle(veryOld, CopyGuardAccountingDelayed)   // beyond 2x window, deletable
	activeCycle := insertCycle(nil, CopyGuardAccountingOpen)             // still open, kept

	insertEvent(reconciledOld, oldTime)
	insertEvent(delayedOld, oldTime)
	insertEvent(activeCycle, veryOld) // ancient event but cycle active: kept
	if _, err := st.db.Exec(`
		INSERT INTO copy_guard_attempts (cycle_id, attempt_no, status, opened_at)
		VALUES (?, 1, 'STOPPED', ?)`, reconciledOld, oldTime); err != nil {
		t.Fatal(err)
	}

	r := NewRetentionService(st, RetentionPolicy{CopyGuardEventDays: 30, CopyGuardCycleDays: 180}, "")
	r.RunOnce()

	var exists int
	for _, tc := range []struct {
		id   int64
		want int
		desc string
	}{
		{reconciledOld, 0, "reconciled old cycle should be deleted"},
		{delayedOld, 1, "delayed cycle inside grace period should be kept"},
		{delayedVeryOld, 0, "delayed cycle beyond doubled window should be deleted"},
		{activeCycle, 1, "active cycle should be kept"},
	} {
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_cycles WHERE id=?`, tc.id).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists != tc.want {
			t.Errorf("%s (id=%d)", tc.desc, tc.id)
		}
	}
	// Attempts of the deleted cycle must be gone too.
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_attempts WHERE cycle_id=?`, reconciledOld).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Error("attempts of a deleted cycle should be deleted")
	}
	// Event of the active cycle must survive even though it is ancient.
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=?`, activeCycle).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 1 {
		t.Error("events of active cycles must never be deleted")
	}
}

func TestRetentionDecisionsAndSignalLogs(t *testing.T) {
	st := newRetentionTestStore(t)
	old := time.Now().AddDate(0, 0, -60)

	if _, err := st.db.Exec(`
		INSERT INTO decision_records (trader_id, cycle_number, timestamp, success)
		VALUES ('t1', 1, ?, 1)`, old.UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`
		INSERT INTO decision_records (trader_id, cycle_number, timestamp, success)
		VALUES ('t1', 2, ?, 1)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`
		INSERT INTO copy_trade_signal_logs (trader_id, leader_id, provider_type, signal_id, symbol, action, position_side, created_at)
		VALUES ('t1','L1','okx','s1','BTC','open','long',?)`, dbTime(old)); err != nil {
		t.Fatal(err)
	}

	r := NewRetentionService(st, RetentionPolicy{DecisionDays: 30, SignalLogDays: 30}, "")
	r.RunOnce()

	if got := countRows(t, st, "decision_records"); got != 1 {
		t.Fatalf("expected 1 surviving decision record, got %d", got)
	}
	if got := countRows(t, st, "copy_trade_signal_logs"); got != 0 {
		t.Fatalf("expected old signal log deleted, got %d rows", got)
	}
}

func TestRetentionLogFiles(t *testing.T) {
	st := newRetentionTestStore(t)
	logDir := t.TempDir()

	oldName := filepath.Join(logDir, "nofx_"+time.Now().AddDate(0, 0, -60).Format("2006-01-02")+".log")
	newName := filepath.Join(logDir, "nofx_"+time.Now().Format("2006-01-02")+".log")
	for _, f := range []string{oldName, newName} {
		if err := os.WriteFile(f, []byte("log"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	r := NewRetentionService(st, RetentionPolicy{LogFileDays: 30}, logDir)
	r.RunOnce()

	if _, err := os.Stat(oldName); !os.IsNotExist(err) {
		t.Error("old log file should be deleted")
	}
	if _, err := os.Stat(newName); err != nil {
		t.Error("current log file must be kept")
	}
}

func TestRetentionDisabledByZero(t *testing.T) {
	st := newRetentionTestStore(t)
	old := time.Now().AddDate(0, 0, -365)
	if _, err := st.db.Exec(`
		INSERT INTO trader_positions (trader_id, symbol, side, quantity, entry_price, entry_time, exit_time, status, updated_at)
		VALUES ('t1','BTC','LONG',1,100,?,?,'CLOSED',?)`,
		old.Format(time.RFC3339), old.Format(time.RFC3339), old.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	r := NewRetentionService(st, RetentionPolicy{}, "") // all zero: everything disabled
	r.RunOnce()

	if got := countRows(t, st, "trader_positions"); got != 1 {
		t.Fatalf("zero policy must not delete anything, got %d rows", got)
	}
}
