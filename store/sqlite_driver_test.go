package store

import (
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"testing"
)

// openTxSafeTestDB opens a fresh database through the tx-safe wrapper with the
// same pool shape production uses (a single shared connection).
func openTxSafeTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "txsafe.db")
	db, err := sql.Open(TxSafeDriverName, path)
	if err != nil {
		t.Fatalf("open tx-safe db: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE guard_test (x INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO guard_test (x) VALUES (1)`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	return db, path
}

func countGuardRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM guard_test`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// TestTxSafeCommitBusyDoesNotPoisonConnection reproduces the 2026-09-05
// production incident: an external reader holds a SHARED lock, COMMIT times
// out with SQLITE_BUSY, and previously the pooled connection stayed inside
// the open transaction forever ("cannot start a transaction within a
// transaction" on every later Begin, silent uncommitted writes).
func TestTxSafeCommitBusyDoesNotPoisonConnection(t *testing.T) {
	oldTimeout := sqliteBusyTimeoutMS
	sqliteBusyTimeoutMS = 200
	t.Cleanup(func() { sqliteBusyTimeoutMS = oldTimeout })

	db, path := openTxSafeTestDB(t)

	// External reader (plain driver, like a backup script or sqlite3 CLI)
	// holding a read transaction: SHARED lock blocks COMMIT (EXCLUSIVE).
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	readerTx, err := reader.Begin()
	if err != nil {
		t.Fatalf("reader begin: %v", err)
	}
	var n int
	if err := readerTx.QueryRow(`SELECT COUNT(*) FROM guard_test`).Scan(&n); err != nil {
		t.Fatalf("reader select: %v", err)
	}

	// Writer transaction: INSERT succeeds (RESERVED is compatible with
	// SHARED) but COMMIT must fail with SQLITE_BUSY.
	writerTx, err := db.Begin()
	if err != nil {
		t.Fatalf("writer begin: %v", err)
	}
	if _, err := writerTx.Exec(`INSERT INTO guard_test (x) VALUES (2)`); err != nil {
		t.Fatalf("writer insert: %v", err)
	}
	commitErr := writerTx.Commit()
	if commitErr == nil {
		t.Fatal("expected COMMIT to fail while an external reader holds the lock")
	}

	// The connection must be clean immediately, even before the reader goes
	// away: Begin must not report a nested transaction.
	probeTx, err := db.Begin()
	if err != nil {
		t.Fatalf("connection poisoned after failed commit: %v", err)
	}
	if err := probeTx.Rollback(); err != nil {
		t.Fatalf("probe rollback: %v", err)
	}

	if err := readerTx.Rollback(); err != nil {
		t.Fatalf("release reader: %v", err)
	}

	// The failed transaction's write must have been rolled back...
	if got := countGuardRows(t, db); got != 1 {
		t.Fatalf("failed commit leaked data: got %d rows, want 1", got)
	}
	// ...and normal writes must work again.
	if _, err := db.Exec(`INSERT INTO guard_test (x) VALUES (3)`); err != nil {
		t.Fatalf("write after recovery: %v", err)
	}
	if got := countGuardRows(t, db); got != 2 {
		t.Fatalf("post-recovery write missing: got %d rows, want 2", got)
	}
}

// TestTxSafeBeginSelfHealsLeakedTransaction verifies the defense-in-depth
// path: if a transaction was leaked onto the pooled connection by any other
// means (here: a manual BEGIN through Exec), the next Begin rolls it back and
// retries instead of failing until process restart.
func TestTxSafeBeginSelfHealsLeakedTransaction(t *testing.T) {
	db, _ := openTxSafeTestDB(t)

	// Poison the single pooled connection: database/sql does not track
	// manual BEGIN, so the connection returns to the pool inside an open
	// transaction.
	if _, err := db.Exec(`BEGIN`); err != nil {
		t.Fatalf("manual BEGIN: %v", err)
	}
	// This write silently joins the zombie transaction.
	if _, err := db.Exec(`INSERT INTO guard_test (x) VALUES (99)`); err != nil {
		t.Fatalf("write inside zombie transaction: %v", err)
	}

	// Without the wrapper this fails with "cannot start a transaction
	// within a transaction".
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin did not self-heal leaked transaction: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// The zombie transaction's write must be gone: self-heal rolled it back.
	if got := countGuardRows(t, db); got != 1 {
		t.Fatalf("zombie transaction data survived: got %d rows, want 1", got)
	}
}

// TestTxSafeUnderlyingDriverContract pins the set of optional driver
// interfaces implemented by modernc.org/sqlite's connection. txSafeConn
// mirrors exactly this set; if a driver upgrade adds context-aware variants
// (ExecerContext, ConnBeginTx, ...), database/sql would bypass the wrapper's
// transaction safety, so this test must fail to force a wrapper update.
func TestTxSafeUnderlyingDriverContract(t *testing.T) {
	probe, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("resolve driver: %v", err)
	}
	innerDriver := probe.Driver()
	_ = probe.Close()

	conn, err := innerDriver.Open(filepath.Join(t.TempDir(), "contract.db"))
	if err != nil {
		t.Fatalf("open raw conn: %v", err)
	}
	defer conn.Close()

	mustImplement := map[string]bool{
		"driver.Execer":             implementsExecer(conn),
		"driver.Queryer":            implementsQueryer(conn),
		"driver.SessionResetter":    implementsSessionResetter(conn),
		"driver.Validator":          implementsValidator(conn),
		"driver.ExecerContext":      implementsExecerContext(conn),
		"driver.QueryerContext":     implementsQueryerContext(conn),
		"driver.ConnPrepareContext": implementsConnPrepareContext(conn),
		"driver.ConnBeginTx":        implementsConnBeginTx(conn),
		"driver.Pinger":             implementsPinger(conn),
	}
	for name, ok := range mustImplement {
		if !ok {
			t.Errorf("underlying sqlite conn no longer implements %s; update txSafeConn accordingly", name)
		}
	}

	mustNotImplement := map[string]bool{
		"driver.NamedValueChecker": implementsNamedValueChecker(conn),
	}
	for name, ok := range mustNotImplement {
		if ok {
			t.Errorf("underlying sqlite conn now implements %s which txSafeConn does not forward; update txSafeConn so behavior is not silently changed", name)
		}
	}

	// The wrapper must expose the same set: a compile-time-checked mirror.
	var wrapped driver.Conn = &txSafeConn{}
	for name, ok := range map[string]bool{
		"driver.Execer":             implementsExecer(wrapped),
		"driver.Queryer":            implementsQueryer(wrapped),
		"driver.SessionResetter":    implementsSessionResetter(wrapped),
		"driver.Validator":          implementsValidator(wrapped),
		"driver.ExecerContext":      implementsExecerContext(wrapped),
		"driver.QueryerContext":     implementsQueryerContext(wrapped),
		"driver.ConnPrepareContext": implementsConnPrepareContext(wrapped),
		"driver.ConnBeginTx":        implementsConnBeginTx(wrapped),
		"driver.Pinger":             implementsPinger(wrapped),
	} {
		if !ok {
			t.Errorf("txSafeConn does not implement %s while the underlying conn does", name)
		}
	}
}

func implementsExecer(c driver.Conn) bool             { _, ok := c.(driver.Execer); return ok }
func implementsQueryer(c driver.Conn) bool            { _, ok := c.(driver.Queryer); return ok }
func implementsSessionResetter(c driver.Conn) bool    { _, ok := c.(driver.SessionResetter); return ok }
func implementsValidator(c driver.Conn) bool          { _, ok := c.(driver.Validator); return ok }
func implementsExecerContext(c driver.Conn) bool      { _, ok := c.(driver.ExecerContext); return ok }
func implementsQueryerContext(c driver.Conn) bool     { _, ok := c.(driver.QueryerContext); return ok }
func implementsConnPrepareContext(c driver.Conn) bool { _, ok := c.(driver.ConnPrepareContext); return ok }
func implementsConnBeginTx(c driver.Conn) bool        { _, ok := c.(driver.ConnBeginTx); return ok }
func implementsPinger(c driver.Conn) bool             { _, ok := c.(driver.Pinger); return ok }
func implementsNamedValueChecker(c driver.Conn) bool  { _, ok := c.(driver.NamedValueChecker); return ok }
