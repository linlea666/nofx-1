package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"nofx/logger"
)

// RetentionPolicy defines how many days each category of historical data is
// kept. A value of 0 disables cleanup for that category.
type RetentionPolicy struct {
	// PositionDays: closed positions (trader_positions, status=CLOSED).
	// Open positions are never touched. Note: lifetime stats (win rate,
	// total PnL) are computed from this table, so they only reflect the
	// retained window after cleanup.
	PositionDays int
	// DecisionDays: AI/copy-trade decision records (decision_records),
	// the largest rows in the database (CoT / prompt text).
	DecisionDays int
	// EquityFullDays: full-precision window for trader_equity_snapshots.
	// Rows older than this are downsampled to one snapshot per trader per
	// day (the first of the day) so the long-term equity curve survives.
	EquityFullDays int
	// CopyGuardEventDays: copy_guard_events belonging to CLOSED cycles.
	// Events of active cycles are kept regardless of age.
	CopyGuardEventDays int
	// CopyGuardCycleDays: closed copy_guard_cycles together with their
	// attempts / events / protective orders. Cycles whose accounting is
	// still PENDING/DELAYED get a doubled grace period before removal.
	CopyGuardCycleDays int
	// SignalLogDays: copy_trade_signal_logs.
	SignalLogDays int
	// DebateDays: completed/cancelled debate sessions (children cascade).
	DebateDays int
	// LogFileDays: data/nofx_YYYY-MM-DD.log files on disk.
	LogFileDays int
}

// DefaultRetentionPolicy returns the built-in defaults.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		PositionDays:       90,
		DecisionDays:       30,
		EquityFullDays:     90,
		CopyGuardEventDays: 30,
		CopyGuardCycleDays: 180,
		SignalLogDays:      30,
		DebateDays:         30,
		LogFileDays:        30,
	}
}

// LoadRetentionPolicyFromEnv returns the default policy with optional
// RETENTION_*_DAYS environment overrides (0 disables a category).
func LoadRetentionPolicyFromEnv() RetentionPolicy {
	p := DefaultRetentionPolicy()
	overrideDays := func(key string, target *int) {
		if v := os.Getenv(key); v != "" {
			if days, err := strconv.Atoi(v); err == nil && days >= 0 {
				*target = days
			}
		}
	}
	overrideDays("RETENTION_POSITION_DAYS", &p.PositionDays)
	overrideDays("RETENTION_DECISION_DAYS", &p.DecisionDays)
	overrideDays("RETENTION_EQUITY_FULL_DAYS", &p.EquityFullDays)
	overrideDays("RETENTION_COPYGUARD_EVENT_DAYS", &p.CopyGuardEventDays)
	overrideDays("RETENTION_COPYGUARD_CYCLE_DAYS", &p.CopyGuardCycleDays)
	overrideDays("RETENTION_SIGNAL_LOG_DAYS", &p.SignalLogDays)
	overrideDays("RETENTION_DEBATE_DAYS", &p.DebateDays)
	overrideDays("RETENTION_LOG_FILE_DAYS", &p.LogFileDays)
	return p
}

const (
	// retentionBatchSize keeps every DELETE short so trading writes are
	// never blocked for long (single-connection SQLite).
	retentionBatchSize = 500
	// retentionBatchPause yields the connection between batches.
	retentionBatchPause = 50 * time.Millisecond
	// retentionInterval is how often cleanup runs after the initial delay.
	retentionInterval = 24 * time.Hour
	// retentionInitialDelay lets the system finish starting up first.
	retentionInitialDelay = 5 * time.Minute
	// vacuumMinReclaimBytes / vacuumMinFreelistRatio: VACUUM only runs at
	// startup when at least this much space AND this fraction of the file
	// is reclaimable, so most restarts skip it.
	vacuumMinReclaimBytes  = 10 * 1024 * 1024
	vacuumMinFreelistRatio = 0.2
)

// RetentionService periodically deletes expired historical data.
type RetentionService struct {
	store  *Store
	policy RetentionPolicy
	logDir string
	stop   chan struct{}
}

// NewRetentionService creates a retention service. logDir is the directory
// holding nofx_YYYY-MM-DD.log files (pass "" to skip file cleanup).
func NewRetentionService(s *Store, policy RetentionPolicy, logDir string) *RetentionService {
	return &RetentionService{store: s, policy: policy, logDir: logDir, stop: make(chan struct{})}
}

// Start launches the background loop: first run after a short startup delay,
// then once per day.
func (r *RetentionService) Start() {
	go func() {
		select {
		case <-time.After(retentionInitialDelay):
		case <-r.stop:
			return
		}
		for {
			r.RunOnce()
			select {
			case <-time.After(retentionInterval):
			case <-r.stop:
				return
			}
		}
	}()
}

// Stop terminates the background loop.
func (r *RetentionService) Stop() {
	close(r.stop)
}

// cutoff returns a normalized UTC "YYYY-MM-DD HH:MM:SS" string usable for
// comparison against datetime(column). All stored formats (RFC3339 with
// offset, RFC3339 UTC, SQLite CURRENT_TIMESTAMP) normalize via datetime().
func cutoff(days int) string {
	return time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
}

// RunOnce executes one full cleanup pass and logs a summary.
func (r *RetentionService) RunOnce() {
	start := time.Now()
	total := int64(0)
	total += r.cleanPositions()
	total += r.cleanDecisions()
	total += r.downsampleEquity()
	total += r.cleanCopyGuardEvents()
	total += r.cleanCopyGuardCycles()
	total += r.cleanSignalLogs()
	total += r.cleanDebates()
	r.cleanLogFiles()
	if total > 0 {
		if _, err := r.store.db.Exec(`PRAGMA optimize`); err != nil {
			logger.Warnf("⚠️ Retention: PRAGMA optimize failed: %v", err)
		}
	}
	logger.Infof("🧹 Retention cleanup finished: %d rows removed in %s", total, time.Since(start).Round(time.Millisecond))
}

// batchDelete repeatedly executes stmt (which must limit itself to
// retentionBatchSize rows) until no rows remain, and returns the total.
func (r *RetentionService) batchDelete(desc, stmt string, args ...interface{}) int64 {
	var total int64
	for {
		res, err := r.store.db.Exec(stmt, args...)
		if err != nil {
			logger.Warnf("⚠️ Retention: failed to clean %s: %v", desc, err)
			return total
		}
		n, _ := res.RowsAffected()
		total += n
		if n < retentionBatchSize {
			break
		}
		time.Sleep(retentionBatchPause)
	}
	if total > 0 {
		logger.Infof("🧹 Retention: removed %d rows from %s", total, desc)
	}
	return total
}

func (r *RetentionService) cleanPositions() int64 {
	if r.policy.PositionDays <= 0 {
		return 0
	}
	// Only CLOSED positions; open positions are never deleted.
	return r.batchDelete("trader_positions", fmt.Sprintf(`
		DELETE FROM trader_positions WHERE id IN (
			SELECT id FROM trader_positions
			WHERE status = 'CLOSED' AND datetime(COALESCE(exit_time, updated_at)) < ?
			LIMIT %d)`, retentionBatchSize),
		cutoff(r.policy.PositionDays))
}

func (r *RetentionService) cleanDecisions() int64 {
	if r.policy.DecisionDays <= 0 {
		return 0
	}
	return r.batchDelete("decision_records", fmt.Sprintf(`
		DELETE FROM decision_records WHERE id IN (
			SELECT id FROM decision_records WHERE datetime(timestamp) < ? LIMIT %d)`, retentionBatchSize),
		cutoff(r.policy.DecisionDays))
}

// downsampleEquity keeps full precision inside EquityFullDays and exactly one
// snapshot per trader per day (the earliest) beyond it, so long-term equity
// curves keep working while the table stays small.
func (r *RetentionService) downsampleEquity() int64 {
	if r.policy.EquityFullDays <= 0 {
		return 0
	}
	c := cutoff(r.policy.EquityFullDays)
	return r.batchDelete("trader_equity_snapshots", fmt.Sprintf(`
		DELETE FROM trader_equity_snapshots WHERE id IN (
			SELECT id FROM trader_equity_snapshots
			WHERE datetime(timestamp) < ?
			  AND id NOT IN (
				SELECT MIN(id) FROM trader_equity_snapshots
				WHERE datetime(timestamp) < ?
				GROUP BY trader_id, date(timestamp))
			LIMIT %d)`, retentionBatchSize),
		c, c)
}

// cleanCopyGuardEvents removes old events of CLOSED cycles. Events of active
// cycles are always kept so live diagnosis is unaffected.
func (r *RetentionService) cleanCopyGuardEvents() int64 {
	if r.policy.CopyGuardEventDays <= 0 {
		return 0
	}
	return r.batchDelete("copy_guard_events", fmt.Sprintf(`
		DELETE FROM copy_guard_events WHERE id IN (
			SELECT e.id FROM copy_guard_events e
			JOIN copy_guard_cycles c ON c.id = e.cycle_id
			WHERE c.closed_at IS NOT NULL AND datetime(e.created_at) < ?
			LIMIT %d)`, retentionBatchSize),
		cutoff(r.policy.CopyGuardEventDays))
}

// copyGuardCycleCond selects closed cycles eligible for deletion: accounting
// must be settled (not PENDING/DELAYED) past the normal window; unresolved
// cycles get a doubled grace period so reconciliation evidence is preserved.
// prefix qualifies the column names (e.g. "c.") for use inside joins.
func copyGuardCycleCond(prefix string) string {
	return fmt.Sprintf(`%[1]sclosed_at IS NOT NULL AND (
	(datetime(%[1]sclosed_at) < ? AND COALESCE(%[1]saccounting_status,'') NOT IN ('PENDING','DELAYED'))
	OR datetime(%[1]sclosed_at) < ?)`, prefix)
}

func (r *RetentionService) cleanCopyGuardCycles() int64 {
	if r.policy.CopyGuardCycleDays <= 0 {
		return 0
	}
	c := cutoff(r.policy.CopyGuardCycleDays)
	c2 := cutoff(r.policy.CopyGuardCycleDays * 2)
	var total int64
	// Children first: these tables have no FK cascade to copy_guard_cycles.
	for _, child := range []struct{ table, key string }{
		{"copy_guard_attempts", "id"},
		{"copy_guard_events", "id"},
		{"copy_guard_protective_orders", "cycle_id"},
	} {
		total += r.batchDelete(child.table, fmt.Sprintf(`
			DELETE FROM %[1]s WHERE %[2]s IN (
				SELECT t.%[2]s FROM %[1]s t
				JOIN copy_guard_cycles c ON c.id = t.cycle_id
				WHERE %[3]s
				LIMIT %[4]d)`, child.table, child.key, copyGuardCycleCond("c."), retentionBatchSize),
			c, c2)
	}
	total += r.batchDelete("copy_guard_cycles", fmt.Sprintf(`
		DELETE FROM copy_guard_cycles WHERE id IN (
			SELECT id FROM copy_guard_cycles WHERE %s LIMIT %d)`, copyGuardCycleCond(""), retentionBatchSize),
		c, c2)
	return total
}

func (r *RetentionService) cleanSignalLogs() int64 {
	if r.policy.SignalLogDays <= 0 {
		return 0
	}
	return r.batchDelete("copy_trade_signal_logs", fmt.Sprintf(`
		DELETE FROM copy_trade_signal_logs WHERE id IN (
			SELECT id FROM copy_trade_signal_logs WHERE datetime(created_at) < ? LIMIT %d)`, retentionBatchSize),
		cutoff(r.policy.SignalLogDays))
}

// cleanDebates removes finished debate sessions; participants/messages/votes
// are removed by FK ON DELETE CASCADE. Debate tables are created lazily by
// the API layer, so skip silently when they do not exist yet.
func (r *RetentionService) cleanDebates() int64 {
	if r.policy.DebateDays <= 0 {
		return 0
	}
	var exists int
	err := r.store.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='debate_sessions'`).Scan(&exists)
	if err != nil || exists == 0 {
		return 0
	}
	return r.batchDelete("debate_sessions", fmt.Sprintf(`
		DELETE FROM debate_sessions WHERE id IN (
			SELECT id FROM debate_sessions
			WHERE status IN ('completed','cancelled') AND datetime(created_at) < ?
			LIMIT %d)`, retentionBatchSize),
		cutoff(r.policy.DebateDays))
}

// cleanLogFiles deletes data/nofx_YYYY-MM-DD.log files older than
// LogFileDays. Today's file (currently open by the logger) is never in range.
func (r *RetentionService) cleanLogFiles() {
	if r.policy.LogFileDays <= 0 || r.logDir == "" {
		return
	}
	matches, err := filepath.Glob(filepath.Join(r.logDir, "nofx_*.log"))
	if err != nil {
		return
	}
	cutoffDay := time.Now().AddDate(0, 0, -r.policy.LogFileDays)
	removed := 0
	for _, path := range matches {
		name := filepath.Base(path)
		dateStr := name[len("nofx_") : len(name)-len(".log")]
		day, err := time.ParseInLocation("2006-01-02", dateStr, time.Local)
		if err != nil {
			continue
		}
		if day.Before(cutoffDay) {
			if err := os.Remove(path); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		logger.Infof("🧹 Retention: removed %d old log files from %s", removed, r.logDir)
	}
}

// VacuumIfNeeded reclaims disk space freed by earlier cleanups. It should be
// called during startup before traders begin trading, because VACUUM briefly
// rewrites the whole database. It only runs when a meaningful amount of space
// is reclaimable, so most startups skip it instantly.
func (r *RetentionService) VacuumIfNeeded() {
	var pageCount, freelistCount, pageSize int64
	if err := r.store.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil || pageCount == 0 {
		return
	}
	if err := r.store.db.QueryRow(`PRAGMA freelist_count`).Scan(&freelistCount); err != nil {
		return
	}
	if err := r.store.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return
	}
	reclaimable := freelistCount * pageSize
	ratio := float64(freelistCount) / float64(pageCount)
	if reclaimable < vacuumMinReclaimBytes || ratio < vacuumMinFreelistRatio {
		return
	}
	logger.Infof("🧹 Retention: running VACUUM to reclaim %.1f MB (%.0f%% of database)...",
		float64(reclaimable)/1024/1024, ratio*100)
	start := time.Now()
	if _, err := r.store.db.Exec(`VACUUM`); err != nil {
		logger.Warnf("⚠️ Retention: VACUUM failed: %v", err)
		return
	}
	logger.Infof("✅ Retention: VACUUM finished in %s", time.Since(start).Round(time.Millisecond))
}
