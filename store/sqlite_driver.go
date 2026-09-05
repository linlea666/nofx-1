// SQLite driver wrapper that keeps the pooled connection transaction-safe.
//
// Root cause being fixed: SQLite keeps a transaction OPEN when COMMIT fails
// with SQLITE_BUSY, but database/sql marks the *sql.Tx as done as soon as
// Commit is called and returns the connection to the pool regardless of the
// commit result. With SetMaxOpenConns(1) the whole process then reuses one
// connection that is permanently stuck inside a zombie transaction: every
// later Begin fails with "cannot start a transaction within a transaction"
// and every plain Exec silently joins the never-committed transaction (data
// lost on restart). Observed in production on 2026-09-05 when a long-running
// external reader made a COMMIT time out.
//
// The wrapper fixes this at the driver boundary so all ~80 existing Begin
// call sites keep working unchanged:
//
//   - Tx.Commit/Tx.Rollback failure → issue a best-effort ROLLBACK on the
//     same underlying connection before it is returned to the pool.
//   - If the cleanup ROLLBACK fails too, the connection is flagged broken;
//     driver.Validator/SessionResetter then make database/sql discard it
//     instead of pooling it.
//   - Conn.Begin self-heals a pre-existing leaked transaction (rolls back
//     and retries once) as defense in depth.
//   - Every new connection re-applies the session pragmas (busy_timeout,
//     foreign_keys, synchronous). Previously they were applied once in
//     store.New via db.Exec, so a recycled connection would silently lose
//     them.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"

	"nofx/logger"
)

// TxSafeDriverName is the database/sql driver name registered by this package.
const TxSafeDriverName = "sqlite-txsafe"

// sqliteBusyTimeoutMS is applied to every new connection. Tests may override
// it (before opening a database) to keep lock-contention scenarios fast.
var sqliteBusyTimeoutMS = 30000

func init() {
	// Obtain the driver instance modernc.org/sqlite registered as "sqlite"
	// (including any package-level UDF/collation registrations) instead of
	// constructing a fresh Driver value.
	probe, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(fmt.Sprintf("store: cannot resolve underlying sqlite driver: %v", err))
	}
	inner := probe.Driver()
	_ = probe.Close()
	sql.Register(TxSafeDriverName, &txSafeDriver{inner: inner})
}

type txSafeDriver struct {
	inner driver.Driver
}

func (d *txSafeDriver) Open(name string) (driver.Conn, error) {
	inner, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	c := &txSafeConn{inner: inner}
	// Session pragmas must hold on every pooled connection, not only the
	// first one created in store.New. Order matters: busy_timeout first so
	// the following pragmas tolerate a concurrently locked database.
	pragmas := []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d", sqliteBusyTimeoutMS),
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = FULL",
	}
	for _, pragma := range pragmas {
		if err := c.rawExec(pragma); err != nil {
			_ = inner.Close()
			return nil, fmt.Errorf("apply %q on new connection: %w", pragma, err)
		}
	}
	return c, nil
}

// txSafeConn wraps the modernc.org/sqlite connection. It forwards every
// optional driver interface the underlying conn implements today (Execer,
// Queryer, ExecerContext, QueryerContext, ConnPrepareContext, ConnBeginTx,
// Pinger, SessionResetter, Validator); TestTxSafeUnderlyingDriverContract
// pins that set so a driver upgrade adding further interfaces fails loudly
// instead of silently bypassing this wrapper.
type txSafeConn struct {
	inner driver.Conn
	// broken marks the connection for disposal after a failed transaction
	// cleanup. database/sql consults IsValid/ResetSession and discards the
	// connection instead of returning it to the pool.
	broken bool
}

func (c *txSafeConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(query)
}

func (c *txSafeConn) Close() error {
	return c.inner.Close()
}

func (c *txSafeConn) Begin() (driver.Tx, error) {
	return c.beginWithHeal(c.inner.Begin)
}

// BeginTx implements driver.ConnBeginTx; database/sql prefers it over Begin,
// so it must carry the same self-heal and Tx wrapping.
func (c *txSafeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.inner.(driver.ConnBeginTx)
	if !ok {
		return c.beginWithHeal(c.inner.Begin)
	}
	return c.beginWithHeal(func() (driver.Tx, error) {
		return beginner.BeginTx(ctx, opts)
	})
}

func (c *txSafeConn) beginWithHeal(begin func() (driver.Tx, error)) (driver.Tx, error) {
	tx, err := begin()
	if err != nil && isNestedTxError(err) {
		// A previous failure leaked an open transaction on this connection.
		// Roll it back and retry once so the process self-heals instead of
		// failing every write until restart.
		logger.Warnf("⚠️ SQLite 连接残留未完成事务，执行自愈回滚后重试 Begin: %v", err)
		if rbErr := c.forceRollback(); rbErr != nil {
			c.broken = true
			return nil, fmt.Errorf("begin failed on leaked transaction and cleanup rollback failed: %v (begin: %w)", rbErr, err)
		}
		tx, err = begin()
	}
	if err != nil {
		return nil, err
	}
	return &txSafeTx{conn: c, inner: tx}, nil
}

// PrepareContext implements driver.ConnPrepareContext.
func (c *txSafeConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.inner.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.inner.Prepare(query)
}

// ExecContext implements driver.ExecerContext.
func (c *txSafeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.inner.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

// QueryContext implements driver.QueryerContext.
func (c *txSafeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := c.inner.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

// Ping implements driver.Pinger.
func (c *txSafeConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// Exec implements driver.Execer, matching the underlying connection.
func (c *txSafeConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	execer, ok := c.inner.(driver.Execer)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.Exec(query, args)
}

// Query implements driver.Queryer, matching the underlying connection.
func (c *txSafeConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	queryer, ok := c.inner.(driver.Queryer)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.Query(query, args)
}

// ResetSession implements driver.SessionResetter. Returning ErrBadConn makes
// database/sql discard the connection before reuse.
func (c *txSafeConn) ResetSession(ctx context.Context) error {
	if c.broken {
		return driver.ErrBadConn
	}
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid implements driver.Validator: a connection whose transaction state
// could not be cleaned must never go back into the pool.
func (c *txSafeConn) IsValid() bool {
	if c.broken {
		return false
	}
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// rawExec runs a statement directly on the underlying connection, outside any
// database/sql-tracked transaction object.
func (c *txSafeConn) rawExec(query string) error {
	if e, ok := c.inner.(driver.ExecerContext); ok {
		_, err := e.ExecContext(context.Background(), query, nil)
		return err
	}
	if e, ok := c.inner.(driver.Execer); ok {
		_, err := e.Exec(query, nil)
		return err
	}
	return fmt.Errorf("underlying sqlite connection implements neither driver.ExecerContext nor driver.Execer")
}

// forceRollback clears any transaction left open on the underlying
// connection. "no transaction is active" means the connection is already
// clean (e.g. SQLite auto-rolled-back) and is treated as success.
func (c *txSafeConn) forceRollback() error {
	err := c.rawExec("ROLLBACK")
	if err == nil || isNoActiveTxError(err) {
		return nil
	}
	return err
}

type txSafeTx struct {
	conn  *txSafeConn
	inner driver.Tx
}

func (t *txSafeTx) Commit() error {
	err := t.inner.Commit()
	if err == nil {
		return nil
	}
	// SQLite keeps the transaction open when COMMIT fails (e.g. SQLITE_BUSY).
	// Clean it up before database/sql returns this connection to the pool.
	if rbErr := t.conn.forceRollback(); rbErr != nil {
		t.conn.broken = true
		logger.Errorf("❌ SQLite COMMIT 失败且清理回滚失败，连接将被丢弃: commit=%v rollback=%v", err, rbErr)
	} else {
		logger.Warnf("⚠️ SQLite COMMIT 失败，已回滚以保持连接干净: %v", err)
	}
	return err
}

func (t *txSafeTx) Rollback() error {
	err := t.inner.Rollback()
	if err == nil || isNoActiveTxError(err) {
		return nil
	}
	if rbErr := t.conn.forceRollback(); rbErr != nil {
		t.conn.broken = true
		logger.Errorf("❌ SQLite ROLLBACK 失败且清理回滚失败，连接将被丢弃: rollback=%v cleanup=%v", err, rbErr)
	}
	return err
}

func isNestedTxError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "within a transaction")
}

func isNoActiveTxError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no transaction is active")
}
