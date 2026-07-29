package store

import (
	"database/sql"
	"fmt"
	"math"
	"time"
)

const (
	ReentryCandidateWatching        = "WATCHING"
	ReentryCandidateReviewing       = "REVIEWING"
	ReentryCandidateWaiting         = "WAITING"
	ReentryCandidateEntryPending    = "ENTRY_PENDING"
	ReentryCandidateReentered       = "REENTERED"
	ReentryCandidateAbandoned       = "ABANDONED"
	ReentryCandidateExpired         = "EXPIRED"
	ReentryCandidateBudgetSuspended = "BUDGET_SUSPENDED"
	ReentryCandidateInvalidated     = "INVALIDATED"
	ReentryCandidatePaused          = "PAUSED"
)

type CopyGuardReentryCandidate struct {
	ID          int64  `json:"id"`
	CycleID     int64  `json:"cycle_id"`
	TraderID    string `json:"trader_id"`
	LeaderPosID string `json:"leader_pos_id"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	MarginMode  string `json:"margin_mode"`
	Status      string `json:"status"`

	TriggerPrice            float64    `json:"trigger_price"`
	ATR                     float64    `json:"atr"`
	MaxNotional             float64    `json:"max_notional"`
	StopCount               int        `json:"stop_count"`
	ReentryCount            int        `json:"reentry_count"`
	LeaderSize              float64    `json:"leader_size"`
	LeaderEntryPrice        float64    `json:"leader_entry_price"`
	LastStopPrice           float64    `json:"last_stop_price"`
	DistanceATRRatio        float64    `json:"distance_atr_ratio"`
	Protectable             bool       `json:"protectable"`
	FeatureHash             string     `json:"feature_hash"`
	PendingTrigger          string     `json:"pending_trigger"`
	DecisionGeneration      int        `json:"decision_generation"`
	ReviewCount             int        `json:"review_count"`
	FailureCount            int        `json:"failure_count"`
	LastDecision            string     `json:"last_decision"`
	Regime                  string     `json:"regime"`
	Confidence              float64    `json:"confidence"`
	SizeFactor              float64    `json:"size_factor"`
	EntryPriceLow           float64    `json:"entry_price_low"`
	EntryPriceHigh          float64    `json:"entry_price_high"`
	AttentionPriceLow       float64    `json:"attention_price_low"`
	AttentionPriceHigh      float64    `json:"attention_price_high"`
	ConsecutiveAbandons     int        `json:"consecutive_abandons"`
	LastAbandonCandle       string     `json:"last_abandon_candle"`
	LastAnalysisID          int64      `json:"last_analysis_id"`
	DecisionTTLSeconds      int        `json:"decision_ttl_seconds"`
	LastError               string     `json:"last_error"`
	FailureBackoffUntil     *time.Time `json:"failure_backoff_until,omitempty"`
	LastUnactionableCode    string     `json:"last_unactionable_code,omitempty"`
	LastUnactionableEventAt *time.Time `json:"last_unactionable_event_at,omitempty"`
	DecisionExpiresAt       *time.Time `json:"decision_expires_at,omitempty"`
	RegularReviewAt         *time.Time `json:"regular_review_at,omitempty"`
	EventReviewAt           *time.Time `json:"event_review_at,omitempty"`
	BudgetBlockedUntil      *time.Time `json:"budget_blocked_until,omitempty"`

	// Effective per-trader policy is populated by the API and is not persisted
	// on the candidate row. This prevents the dashboard from presenting the
	// retired global 0.7 field or hard-coded quota values as production truth.
	AIConfidenceThreshold     float64 `json:"ai_confidence_threshold,omitempty"`
	AIMinReviewSeconds        int     `json:"ai_min_review_seconds,omitempty"`
	AIDailyCallLimit          int     `json:"ai_daily_call_limit,omitempty"`
	AIDailyCallsUsed          int     `json:"ai_daily_calls_used,omitempty"`
	AILifecycleCallLimit      int     `json:"ai_lifecycle_call_limit,omitempty"`
	TraderLifecycleGeneration int64   `json:"trader_lifecycle_generation,omitempty"`
	AICallLimitsDeprecated    bool    `json:"ai_call_limits_deprecated"`
	AICostWarningExceeded     bool    `json:"ai_cost_warning_exceeded"`

	SnapshotAt   time.Time  `json:"snapshot_at"`
	LastReviewAt *time.Time `json:"last_review_at,omitempty"`
	NextReviewAt time.Time  `json:"next_review_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	ClosedAt     *time.Time `json:"closed_at,omitempty"`
}

func (s *ReentryAIStore) initReentryCandidateTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_guard_reentry_candidates (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			cycle_id INTEGER NOT NULL UNIQUE,
			trader_id TEXT NOT NULL,
			leader_pos_id TEXT NOT NULL,
			symbol TEXT NOT NULL,
			side TEXT NOT NULL,
			margin_mode TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'WATCHING',
			trigger_price REAL DEFAULT 0, atr REAL DEFAULT 0, max_notional REAL DEFAULT 0,
			stop_count INTEGER DEFAULT 0, reentry_count INTEGER DEFAULT 0,
			leader_size REAL DEFAULT 0, leader_entry_price REAL DEFAULT 0,
			last_stop_price REAL DEFAULT 0, distance_atr_ratio REAL DEFAULT 0,
			protectable BOOLEAN DEFAULT 1, feature_hash TEXT DEFAULT '', pending_trigger TEXT DEFAULT '',
			decision_generation INTEGER DEFAULT 0, review_count INTEGER DEFAULT 0, failure_count INTEGER DEFAULT 0,
			last_decision TEXT DEFAULT '', regime TEXT DEFAULT '', confidence REAL DEFAULT 0, size_factor REAL DEFAULT 0,
			entry_price_low REAL DEFAULT 0, entry_price_high REAL DEFAULT 0,
			attention_price_low REAL DEFAULT 0, attention_price_high REAL DEFAULT 0,
			consecutive_abandons INTEGER DEFAULT 0, last_abandon_candle TEXT DEFAULT '',
			last_analysis_id INTEGER DEFAULT 0, decision_ttl_seconds INTEGER DEFAULT 30, last_error TEXT DEFAULT '',
			failure_backoff_until DATETIME, last_unactionable_code TEXT DEFAULT '', last_unactionable_event_at DATETIME,
			decision_expires_at DATETIME, regular_review_at DATETIME, event_review_at DATETIME, budget_blocked_until DATETIME,
			snapshot_at DATETIME DEFAULT CURRENT_TIMESTAMP, last_review_at DATETIME,
			next_review_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP, closed_at DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_cg_reentry_candidate_due ON copy_guard_reentry_candidates(status,next_review_at);
		CREATE INDEX IF NOT EXISTS idx_cg_reentry_candidate_trader ON copy_guard_reentry_candidates(trader_id,status);
		CREATE TABLE IF NOT EXISTS copy_guard_risk_reservations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL, account_key TEXT NOT NULL DEFAULT '', cycle_id INTEGER NOT NULL, attempt_no INTEGER NOT NULL,
			intent_id INTEGER NOT NULL DEFAULT 0, replace_cycle_id INTEGER NOT NULL DEFAULT 0, replace_attempt_no INTEGER NOT NULL DEFAULT 0,
			target_risk_usd REAL NOT NULL DEFAULT 0, attempt_override BOOLEAN NOT NULL DEFAULT 0,
			risk_usd REAL NOT NULL, status TEXT NOT NULL DEFAULT 'ACTIVE',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, released_at DATETIME,
			UNIQUE(cycle_id,attempt_no)
		);
		CREATE INDEX IF NOT EXISTS idx_cg_risk_reservation_active ON copy_guard_risk_reservations(status,trader_id);
	`)
	if err != nil {
		return err
	}
	for _, migration := range []struct{ table, name, definition string }{
		{"copy_guard_reentry_candidates", "decision_ttl_seconds", "INTEGER DEFAULT 30"},
		{"copy_guard_reentry_candidates", "failure_backoff_until", "DATETIME"},
		{"copy_guard_reentry_candidates", "last_unactionable_code", "TEXT DEFAULT ''"},
		{"copy_guard_reentry_candidates", "last_unactionable_event_at", "DATETIME"},
		{"copy_guard_reentry_candidates", "decision_expires_at", "DATETIME"},
		{"copy_guard_reentry_candidates", "regular_review_at", "DATETIME"},
		{"copy_guard_reentry_candidates", "event_review_at", "DATETIME"},
		{"copy_guard_reentry_candidates", "budget_blocked_until", "DATETIME"},
		{"copy_guard_risk_reservations", "account_key", "TEXT NOT NULL DEFAULT ''"},
		{"copy_guard_risk_reservations", "intent_id", "INTEGER NOT NULL DEFAULT 0"},
		{"copy_guard_risk_reservations", "replace_cycle_id", "INTEGER NOT NULL DEFAULT 0"},
		{"copy_guard_risk_reservations", "replace_attempt_no", "INTEGER NOT NULL DEFAULT 0"},
		{"copy_guard_risk_reservations", "target_risk_usd", "REAL NOT NULL DEFAULT 0"},
		{"copy_guard_risk_reservations", "attempt_override", "BOOLEAN NOT NULL DEFAULT 0"},
	} {
		if err = ensureSQLiteColumn(s.db, migration.table, migration.name, migration.definition); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", migration.table, migration.name, err)
		}
	}
	if _, err = s.db.Exec(`UPDATE copy_guard_reentry_candidates SET regular_review_at=next_review_at WHERE regular_review_at IS NULL`); err != nil {
		return err
	}
	// Legacy call quotas are retained as cost-warning configuration only.
	// They must never strand a live observation cycle. Running traders resume
	// scheduled reviews, stopped traders remain explicitly paused, and rows
	// whose trader was physically deleted become auditable invalidations.
	if _, err = s.db.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=CASE
		      WHEN EXISTS(SELECT 1 FROM traders t
		                  WHERE t.id=copy_guard_reentry_candidates.trader_id
		                    AND t.lifecycle_status='RUNNING') THEN ?
		      WHEN EXISTS(SELECT 1 FROM traders t
		                  WHERE t.id=copy_guard_reentry_candidates.trader_id) THEN ?
		      ELSE ?
		    END,
		    budget_blocked_until=NULL,
		    pending_trigger=CASE
		      WHEN EXISTS(SELECT 1 FROM traders t
		                  WHERE t.id=copy_guard_reentry_candidates.trader_id
		                    AND t.lifecycle_status='RUNNING')
		        THEN 'SOFT_COST_WARNING_MIGRATED'
		      WHEN EXISTS(SELECT 1 FROM traders t
		                  WHERE t.id=copy_guard_reentry_candidates.trader_id)
		        THEN 'TRADER_STOPPED'
		      ELSE 'TRADER_ARCHIVED'
		    END,
		    closed_at=CASE
		      WHEN EXISTS(SELECT 1 FROM traders t
		                  WHERE t.id=copy_guard_reentry_candidates.trader_id
		                    AND t.lifecycle_status='RUNNING') THEN NULL
		      WHEN EXISTS(SELECT 1 FROM traders t
		                  WHERE t.id=copy_guard_reentry_candidates.trader_id) THEN closed_at
		      ELSE COALESCE(closed_at,CURRENT_TIMESTAMP)
		    END,
		    next_review_at=CASE
		      WHEN EXISTS(SELECT 1 FROM traders t
		                  WHERE t.id=copy_guard_reentry_candidates.trader_id
		                    AND t.lifecycle_status='RUNNING') THEN CURRENT_TIMESTAMP
		      ELSE next_review_at
		    END,
		    updated_at=CURRENT_TIMESTAMP
		WHERE budget_blocked_until IS NOT NULL OR status=?`,
		ReentryCandidateWaiting, ReentryCandidatePausedByTrader,
		ReentryCandidateInvalidatedTraderArchived,
		ReentryCandidateBudgetSuspended); err != nil {
		return err
	}
	if _, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_cg_risk_reservation_intent ON copy_guard_risk_reservations(intent_id,status)`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`UPDATE copy_guard_risk_reservations SET account_key=COALESCE((SELECT exchange_id FROM traders WHERE traders.id=copy_guard_risk_reservations.trader_id),trader_id) WHERE account_key=''`); err != nil {
		return err
	}
	// v7 removes the human approval path. Preserve any old pending signal as a
	// durable AI candidate so it is neither silently executed nor lost. Legacy
	// traders remain inert until explicitly switched to ai_guarded.
	if _, err = s.db.Exec(`INSERT INTO copy_guard_reentry_candidates
		(cycle_id,trader_id,leader_pos_id,symbol,side,margin_mode,status,trigger_price,atr,max_notional,stop_count,reentry_count,leader_size,leader_entry_price,last_stop_price,distance_atr_ratio,protectable,feature_hash,pending_trigger,next_review_at,snapshot_at)
		SELECT cycle_id,trader_id,leader_pos_id,symbol,side,margin_mode,'WATCHING',trigger_price,atr,recommended_notional,stop_count,reentry_count,leader_size,leader_entry_price,trigger_price,distance_atr_ratio,protectable,'migrated-manual-'||id,'MIGRATED_MANUAL',CURRENT_TIMESTAMP,created_at
		FROM copy_guard_manual_reentry_signals WHERE status='PENDING'
		ON CONFLICT(cycle_id) DO NOTHING`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`UPDATE copy_guard_manual_reentry_signals SET status='MIGRATED',error='migrated to AI candidate; manual confirmation endpoint retired' WHERE status='PENDING' AND EXISTS (SELECT 1 FROM copy_guard_reentry_candidates c WHERE c.cycle_id=copy_guard_manual_reentry_signals.cycle_id)`); err != nil {
		return err
	}
	return s.reconcileReentryCandidateLifecycleState()
}

// reconcileReentryCandidateLifecycleState repairs legacy rows created before
// cycle termination and trader lifecycle were one authority. It delegates
// terminal cycles to the existing all-or-nothing auxiliary cleanup, then uses
// the same stop transaction helper as the API for still-open observations.
func (s *ReentryAIStore) reconcileReentryCandidateLifecycleState() error {
	rows, err := s.db.Query(`SELECT DISTINCT c.cycle_id,g.status
		FROM copy_guard_reentry_candidates c
		JOIN copy_guard_cycles g ON g.id=c.cycle_id
		WHERE c.status IN ('WATCHING','WAITING','REVIEWING','ENTRY_PENDING','PAUSED','PAUSED_BY_TRADER')
		  AND (
			g.closed_at IS NOT NULL
			OR g.status NOT IN (
				'STOPPED_WATCHING','AI_WATCHING','AI_REVIEWING','AI_WAITING',
				'ATTEMPTS_EXHAUSTED','BUDGET_SUSPENDED','REENTRY_PENDING'
			)
		  )
		ORDER BY c.cycle_id`)
	if err != nil {
		return err
	}
	type terminalCycle struct {
		id     int64
		status string
	}
	var terminal []terminalCycle
	for rows.Next() {
		var cycle terminalCycle
		if err = rows.Scan(&cycle.id, &cycle.status); err != nil {
			_ = rows.Close()
			return err
		}
		terminal = append(terminal, cycle)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, cycle := range terminal {
		tx, txErr := s.db.Begin()
		if txErr != nil {
			return txErr
		}
		if txErr = terminalizeCopyGuardAuxiliaryStateTx(tx, cycle.id, cycle.status); txErr != nil {
			_ = tx.Rollback()
			return txErr
		}
		if txErr = tx.Commit(); txErr != nil {
			return txErr
		}
	}

	if _, err = s.db.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=?,pending_trigger='TRADER_ARCHIVED',
		    last_error='candidate invalidated because trader is archived',
		    closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE status IN ('WATCHING','WAITING','REVIEWING','ENTRY_PENDING','PAUSED_BY_TRADER')
		  AND EXISTS (
			SELECT 1 FROM traders t
			WHERE t.id=copy_guard_reentry_candidates.trader_id
			  AND t.lifecycle_status='ARCHIVED'
		  )`, ReentryCandidateInvalidatedTraderArchived); err != nil {
		return err
	}

	rows, err = s.db.Query(`SELECT DISTINCT t.id
		FROM traders t
		JOIN copy_guard_reentry_candidates c ON c.trader_id=t.id
		WHERE t.lifecycle_status NOT IN ('RUNNING','ARCHIVED')
		  AND c.status IN ('WATCHING','WAITING','REVIEWING','ENTRY_PENDING')
		ORDER BY t.id`)
	if err != nil {
		return err
	}
	var stoppedTraderIDs []string
	for rows.Next() {
		var traderID string
		if err = rows.Scan(&traderID); err != nil {
			_ = rows.Close()
			return err
		}
		stoppedTraderIDs = append(stoppedTraderIDs, traderID)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, traderID := range stoppedTraderIDs {
		tx, txErr := s.db.Begin()
		if txErr != nil {
			return txErr
		}
		if txErr = pauseTraderRiskIncreaseTx(tx, traderID); txErr != nil {
			_ = tx.Rollback()
			return txErr
		}
		if txErr = tx.Commit(); txErr != nil {
			return txErr
		}
	}
	return nil
}

const reentryCandidateColumns = `id,cycle_id,trader_id,leader_pos_id,symbol,side,margin_mode,status,
	trigger_price,atr,max_notional,stop_count,reentry_count,leader_size,leader_entry_price,last_stop_price,
	distance_atr_ratio,protectable,feature_hash,pending_trigger,decision_generation,review_count,failure_count,
	last_decision,regime,confidence,size_factor,entry_price_low,entry_price_high,attention_price_low,attention_price_high,
	consecutive_abandons,last_abandon_candle,last_analysis_id,decision_ttl_seconds,last_error,
	failure_backoff_until,COALESCE(last_unactionable_code,''),last_unactionable_event_at,
	decision_expires_at,regular_review_at,event_review_at,budget_blocked_until,
	snapshot_at,last_review_at,next_review_at,
	created_at,updated_at,closed_at`

func scanReentryCandidate(row rowScanner) (*CopyGuardReentryCandidate, error) {
	var c CopyGuardReentryCandidate
	var snapshot, next, created, updated string
	var last, closed, failureBackoff, unactionableEvent, decisionExpires, regularReview, eventReview, budgetBlocked sql.NullString
	if err := row.Scan(&c.ID, &c.CycleID, &c.TraderID, &c.LeaderPosID, &c.Symbol, &c.Side, &c.MarginMode, &c.Status,
		&c.TriggerPrice, &c.ATR, &c.MaxNotional, &c.StopCount, &c.ReentryCount, &c.LeaderSize, &c.LeaderEntryPrice, &c.LastStopPrice,
		&c.DistanceATRRatio, &c.Protectable, &c.FeatureHash, &c.PendingTrigger, &c.DecisionGeneration, &c.ReviewCount, &c.FailureCount,
		&c.LastDecision, &c.Regime, &c.Confidence, &c.SizeFactor, &c.EntryPriceLow, &c.EntryPriceHigh, &c.AttentionPriceLow, &c.AttentionPriceHigh,
		&c.ConsecutiveAbandons, &c.LastAbandonCandle, &c.LastAnalysisID, &c.DecisionTTLSeconds, &c.LastError,
		&failureBackoff, &c.LastUnactionableCode, &unactionableEvent, &decisionExpires, &regularReview, &eventReview, &budgetBlocked,
		&snapshot, &last, &next, &created, &updated, &closed); err != nil {
		return nil, err
	}
	var err error
	if c.SnapshotAt, err = parseDBTime(snapshot); err != nil {
		return nil, err
	}
	if c.NextReviewAt, err = parseDBTime(next); err != nil {
		return nil, err
	}
	if c.CreatedAt, err = parseDBTime(created); err != nil {
		return nil, err
	}
	if c.UpdatedAt, err = parseDBTime(updated); err != nil {
		return nil, err
	}
	if c.LastReviewAt, err = parseNullableDBTime(last); err != nil {
		return nil, err
	}
	if c.FailureBackoffUntil, err = parseNullableDBTime(failureBackoff); err != nil {
		return nil, err
	}
	if c.LastUnactionableEventAt, err = parseNullableDBTime(unactionableEvent); err != nil {
		return nil, err
	}
	if c.DecisionExpiresAt, err = parseNullableDBTime(decisionExpires); err != nil {
		return nil, err
	}
	if c.RegularReviewAt, err = parseNullableDBTime(regularReview); err != nil {
		return nil, err
	}
	if c.EventReviewAt, err = parseNullableDBTime(eventReview); err != nil {
		return nil, err
	}
	if c.BudgetBlockedUntil, err = parseNullableDBTime(budgetBlocked); err != nil {
		return nil, err
	}
	if c.ClosedAt, err = parseNullableDBTime(closed); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *ReentryAIStore) GetReentryCandidate(id int64) (*CopyGuardReentryCandidate, error) {
	return scanReentryCandidate(s.db.QueryRow(`SELECT `+reentryCandidateColumns+` FROM copy_guard_reentry_candidates WHERE id=?`, id))
}

func (s *ReentryAIStore) GetReentryCandidateByCycle(cycleID int64) (*CopyGuardReentryCandidate, error) {
	return scanReentryCandidate(s.db.QueryRow(`SELECT `+reentryCandidateColumns+` FROM copy_guard_reentry_candidates WHERE cycle_id=?`, cycleID))
}

func (s *ReentryAIStore) ListPendingEntryLeases(traderID string, limit int) ([]*CopyGuardReentryCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+reentryCandidateColumns+` FROM copy_guard_reentry_candidates
		WHERE trader_id=? AND status=? ORDER BY id LIMIT ?`, traderID, ReentryCandidateEntryPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyGuardReentryCandidate
	for rows.Next() {
		c, scanErr := scanReentryCandidate(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *ReentryAIStore) ExpireEntryLease(id int64) error {
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_error='ENTER_WINDOW_EXPIRED',pending_trigger='ENTER_WINDOW_EXPIRED',next_review_at=CURRENT_TIMESTAMP,decision_expires_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`,
		ReentryCandidateWaiting, id, ReentryCandidateEntryPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate entry lease is no longer pending")
	}
	return nil
}

// EnsureReentryCandidate 创建候选或在同周期再次止损后重置为观察态。
func (s *ReentryAIStore) EnsureReentryCandidate(c *CopyGuardReentryCandidate, firstReview time.Time) (*CopyGuardReentryCandidate, error) {
	if c == nil || c.CycleID <= 0 {
		return nil, fmt.Errorf("invalid reentry candidate")
	}
	_, err := s.db.Exec(`INSERT INTO copy_guard_reentry_candidates
		(cycle_id,trader_id,leader_pos_id,symbol,side,margin_mode,status,trigger_price,atr,max_notional,stop_count,reentry_count,leader_size,leader_entry_price,last_stop_price,distance_atr_ratio,protectable,feature_hash,pending_trigger,next_review_at,regular_review_at,snapshot_at)
		VALUES(?,?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?,?,?,?, ?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(cycle_id) DO UPDATE SET trader_id=excluded.trader_id,leader_pos_id=excluded.leader_pos_id,
		symbol=excluded.symbol,side=excluded.side,margin_mode=excluded.margin_mode,status=CASE WHEN copy_guard_reentry_candidates.reentry_count<>excluded.reentry_count THEN 'WATCHING' ELSE copy_guard_reentry_candidates.status END,
		trigger_price=excluded.trigger_price,atr=excluded.atr,max_notional=excluded.max_notional,stop_count=excluded.stop_count,reentry_count=excluded.reentry_count,
		leader_size=excluded.leader_size,leader_entry_price=excluded.leader_entry_price,last_stop_price=excluded.last_stop_price,
		distance_atr_ratio=excluded.distance_atr_ratio,protectable=excluded.protectable,
		pending_trigger=CASE WHEN copy_guard_reentry_candidates.feature_hash<>excluded.feature_hash THEN excluded.pending_trigger ELSE copy_guard_reentry_candidates.pending_trigger END,
		next_review_at=CASE WHEN copy_guard_reentry_candidates.reentry_count<>excluded.reentry_count THEN excluded.next_review_at ELSE copy_guard_reentry_candidates.next_review_at END,
		regular_review_at=CASE WHEN copy_guard_reentry_candidates.reentry_count<>excluded.reentry_count THEN excluded.regular_review_at ELSE copy_guard_reentry_candidates.regular_review_at END,
		event_review_at=CASE WHEN copy_guard_reentry_candidates.reentry_count<>excluded.reentry_count THEN NULL ELSE copy_guard_reentry_candidates.event_review_at END,
		feature_hash=excluded.feature_hash,
		closed_at=CASE WHEN copy_guard_reentry_candidates.reentry_count<>excluded.reentry_count THEN NULL ELSE copy_guard_reentry_candidates.closed_at END,
		snapshot_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
		WHERE copy_guard_reentry_candidates.status IN ('WATCHING','WAITING') OR copy_guard_reentry_candidates.reentry_count<>excluded.reentry_count`,
		c.CycleID, c.TraderID, c.LeaderPosID, c.Symbol, c.Side, c.MarginMode, ReentryCandidateWatching, c.TriggerPrice, c.ATR, c.MaxNotional, c.StopCount, c.ReentryCount, c.LeaderSize, c.LeaderEntryPrice, c.LastStopPrice, c.DistanceATRRatio, c.Protectable, c.FeatureHash, c.PendingTrigger, firstReview.UTC(), firstReview.UTC())
	if err != nil {
		return nil, err
	}
	return s.GetReentryCandidateByCycle(c.CycleID)
}

func (s *ReentryAIStore) ListDueReentryCandidates(limit int) ([]*CopyGuardReentryCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+reentryCandidateColumns+` FROM copy_guard_reentry_candidates
			WHERE status IN ('WATCHING','WAITING') AND next_review_at<=CURRENT_TIMESTAMP
			AND EXISTS(SELECT 1 FROM traders t
				WHERE t.id=copy_guard_reentry_candidates.trader_id
				  AND t.lifecycle_status='RUNNING' AND t.is_running=1)
			AND EXISTS(SELECT 1 FROM copy_guard_cycles g
				WHERE g.id=copy_guard_reentry_candidates.cycle_id
				  AND g.closed_at IS NULL
				  AND g.status IN (
					'STOPPED_WATCHING','AI_WATCHING','AI_REVIEWING','AI_WAITING',
					'ATTEMPTS_EXHAUSTED','BUDGET_SUSPENDED'
				  ))
			AND (failure_backoff_until IS NULL OR failure_backoff_until<=CURRENT_TIMESTAMP)
			ORDER BY next_review_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyGuardReentryCandidate
	for rows.Next() {
		c, err := scanReentryCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *ReentryAIStore) CountReentryCandidateCalls24h(candidateID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reentry_ai_analyses WHERE candidate_id=? AND call_status IN ('RUNNING','COMPLETED','INVALID','FAILED') AND created_at>=datetime('now','-24 hours')`, candidateID).Scan(&count)
	return count, err
}

// ClaimReentryCandidateReview atomically leases a review. The legacy daily and
// lifecycle values are accepted for one compatibility version but are soft
// cost-warning lines only and never affect eligibility.
func (s *ReentryAIStore) ClaimReentryCandidateReview(id int64, minInterval time.Duration, dailyLimit, lifecycleLimit int) (*CopyGuardReentryCandidate, bool, error) {
	_ = dailyLimit
	_ = lifecycleLimit
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var status string
	var count int
	var last sql.NullString
	var lifecycleStatus string
	var lifecycleGeneration int64
	var cycleReviewable bool
	if err = tx.QueryRow(`SELECT c.status,c.review_count,c.last_review_at,
			COALESCE(t.lifecycle_status,''),COALESCE(t.lifecycle_generation,0),
			EXISTS(SELECT 1 FROM copy_guard_cycles g
				WHERE g.id=c.cycle_id AND g.closed_at IS NULL
				  AND g.status IN (
					'STOPPED_WATCHING','AI_WATCHING','AI_REVIEWING','AI_WAITING',
					'ATTEMPTS_EXHAUSTED','BUDGET_SUSPENDED'
				  ))
			FROM copy_guard_reentry_candidates c
			LEFT JOIN traders t ON t.id=c.trader_id WHERE c.id=?`, id).
		Scan(&status, &count, &last, &lifecycleStatus, &lifecycleGeneration, &cycleReviewable); err != nil {
		return nil, false, err
	}
	if status != ReentryCandidateWatching && status != ReentryCandidateWaiting {
		return nil, false, nil
	}
	if lifecycleStatus != TraderLifecycleRunning || !cycleReviewable {
		return nil, false, nil
	}
	if last.Valid {
		if t, e := parseDBTime(last.String); e == nil && time.Since(t) < minInterval {
			return nil, false, nil
		}
	}
	res, err := tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,decision_generation=decision_generation+1,review_count=review_count+1,last_review_at=CURRENT_TIMESTAMP,next_review_at=?,event_review_at=NULL,budget_blocked_until=NULL,pending_trigger='',updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('WATCHING','WAITING')`, ReentryCandidateReviewing, time.Now().Add(minInterval).UTC(), id)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, false, nil
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	c, err := s.GetReentryCandidate(id)
	if c != nil {
		c.TraderLifecycleGeneration = lifecycleGeneration
	}
	return c, true, err
}

func (s *ReentryAIStore) GetFirstCandidateModelCallAt(candidateID int64) (*time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRow(`SELECT MIN(created_at) FROM reentry_ai_analyses
		WHERE candidate_id=? AND call_status IN ('RUNNING','COMPLETED','INVALID','FAILED')`,
		candidateID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	return parseNullableDBTime(raw)
}

type ReentryCandidateDecision struct {
	Decision, Regime                      string
	Confidence, SizeFactor                float64
	EntryPriceLow, EntryPriceHigh         float64
	AttentionPriceLow, AttentionPriceHigh float64
	NextReview                            time.Time
	AnalysisID                            int64
	TTLSeconds                            int
	CandleKey                             string
	ConfirmAbandon                        bool
	EnterApproved                         bool
}

func (s *ReentryAIStore) FinishReentryCandidateReview(id int64, d ReentryCandidateDecision) error {
	status := ReentryCandidateWaiting
	if d.Decision == ReentryVerdictEnter && d.EnterApproved {
		status = ReentryCandidateEntryPending
	}
	if d.Decision == ReentryVerdictAbandon {
		status = ReentryCandidateWaiting
	}
	var decisionExpires interface{}
	if status == ReentryCandidateEntryPending {
		ttl := d.TTLSeconds
		if ttl < 15 {
			ttl = 15
		}
		if ttl > 60 {
			ttl = 60
		}
		d.TTLSeconds = ttl
		var storedExpiry sql.NullString
		if err := s.db.QueryRow(`SELECT decision_expires_at FROM reentry_ai_analyses WHERE id=? AND candidate_id=?`,
			d.AnalysisID, id).Scan(&storedExpiry); err != nil {
			return fmt.Errorf("read authoritative AI decision expiry: %w", err)
		}
		if !storedExpiry.Valid || storedExpiry.String == "" {
			return fmt.Errorf("AI analysis %d has no authoritative decision expiry", d.AnalysisID)
		}
		expiresAt, err := parseDBTime(storedExpiry.String)
		if err != nil {
			return fmt.Errorf("parse authoritative AI decision expiry: %w", err)
		}
		decisionExpires = expiresAt.UTC()
	}
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_decision=?,regime=?,confidence=?,size_factor=?,entry_price_low=?,entry_price_high=?,attention_price_low=?,attention_price_high=?,next_review_at=?,regular_review_at=?,event_review_at=NULL,last_analysis_id=?,decision_ttl_seconds=?,decision_expires_at=?,failure_count=0,last_error='',failure_backoff_until=NULL,consecutive_abandons=CASE WHEN ? AND last_abandon_candle<>? THEN consecutive_abandons+1 WHEN ? THEN consecutive_abandons ELSE 0 END,last_abandon_candle=CASE WHEN ? THEN ? ELSE '' END,updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status=? AND EXISTS(
			SELECT 1 FROM traders t WHERE t.id=copy_guard_reentry_candidates.trader_id
			  AND t.lifecycle_status='RUNNING' AND t.is_running=1
		)`, status, d.Decision, d.Regime, d.Confidence, d.SizeFactor, d.EntryPriceLow, d.EntryPriceHigh, d.AttentionPriceLow, d.AttentionPriceHigh, d.NextReview.UTC(), d.NextReview.UTC(), d.AnalysisID, d.TTLSeconds, decisionExpires, d.ConfirmAbandon, d.CandleKey, d.ConfirmAbandon, d.ConfirmAbandon, d.CandleKey, id, ReentryCandidateReviewing)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate review is no longer active")
	}
	return nil
}

func (s *ReentryAIStore) FailReentryCandidateReview(id int64, message string, retry time.Duration) error {
	retryAt := time.Now().Add(retry).UTC()
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,failure_count=failure_count+1,last_error=?,next_review_at=?,failure_backoff_until=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN (?,?)`, ReentryCandidateWaiting, message, retryAt, retryAt, id, ReentryCandidateReviewing, ReentryCandidateEntryPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate review is no longer active")
	}
	return nil
}

// RejectReentryCandidatePreflight returns an approved AI decision to watching
// when deterministic execution state changed (price drift, leader position,
// account position, budget, etc.). This is expected market behavior, not an AI
// failure, so failure_count and the completed analysis remain untouched.
func (s *ReentryAIStore) RejectReentryCandidatePreflight(id int64, message string, retry time.Duration) error {
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_error=?,next_review_at=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`, ReentryCandidateWaiting, message, time.Now().Add(retry).UTC(), id, ReentryCandidateEntryPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate entry is no longer pending")
	}
	return nil
}

// DeferReentryCandidateUnactionable records deterministic inability to trade
// without charging model quota or treating it as an AI/system failure.
func (s *ReentryAIStore) DeferReentryCandidateUnactionable(candidateID, analysisID int64, message string, retry time.Duration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if analysisID > 0 {
		if _, err = tx.Exec(`UPDATE reentry_ai_analyses SET call_status='UNACTIONABLE',call_error=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND candidate_id=? AND call_status='PENDING'`, message, analysisID, candidateID); err != nil {
			return err
		}
	}
	retryAt := time.Now().Add(retry).UTC()
	res, err := tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,review_count=CASE WHEN status=? THEN max(review_count-1,0) ELSE review_count END,last_error=?,next_review_at=?,failure_backoff_until=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN (?,?,?)`,
		ReentryCandidateWaiting, ReentryCandidateReviewing, message, retryAt, retryAt, candidateID, ReentryCandidateWatching, ReentryCandidateWaiting, ReentryCandidateReviewing)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate is no longer actionable")
	}
	return tx.Commit()
}

// ShouldRecordReentryCandidateUnactionable suppresses identical deterministic
// noise while retaining a periodic heartbeat. Scheduling remains owned by
// DeferReentryCandidateUnactionable.
func (s *ReentryAIStore) ShouldRecordReentryCandidateUnactionable(candidateID int64, reasonCode string, heartbeat time.Duration) (bool, error) {
	if candidateID <= 0 || reasonCode == "" {
		return false, fmt.Errorf("invalid unactionable event identity")
	}
	if heartbeat <= 0 {
		heartbeat = time.Hour
	}
	cutoff := time.Now().Add(-heartbeat).UTC()
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates
		SET last_unactionable_code=?,last_unactionable_event_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND (
			COALESCE(last_unactionable_code,'')<>? OR
			last_unactionable_event_at IS NULL OR
			last_unactionable_event_at<=?
		)`, reasonCode, candidateID, reasonCode, cutoff)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FailReentryCandidateBeforeModel returns a claimed review that failed during
// datapack/model-client preparation. Because no model request was made, it
// remains fully audited but does not consume daily or lifecycle call quota.
func (s *ReentryAIStore) FailReentryCandidateBeforeModel(candidateID, analysisID int64, message string, retry time.Duration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if analysisID > 0 {
		res, updateErr := tx.Exec(`UPDATE reentry_ai_analyses SET call_status='PREPARE_FAILED',call_error=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND candidate_id=? AND call_status='PENDING'`, message, analysisID, candidateID)
		if updateErr != nil {
			return updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("candidate analysis is no longer pending")
		}
	}
	retryAt := time.Now().Add(retry).UTC()
	res, err := tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,review_count=max(review_count-1,0),failure_count=failure_count+1,last_error=?,next_review_at=?,failure_backoff_until=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`, ReentryCandidateWaiting, message, retryAt, retryAt, candidateID, ReentryCandidateReviewing)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate review is no longer active")
	}
	return tx.Commit()
}

// SkipDuplicateCandidateReview records a generated-but-not-billed snapshot
// and returns the lease without consuming lifecycle quota.
func (s *ReentryAIStore) SkipDuplicateCandidateReview(candidateID, analysisID int64, next time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE reentry_ai_analyses SET call_status='SKIPPED',call_error='duplicate data hash; model call suppressed',updated_at=CURRENT_TIMESTAMP WHERE id=? AND candidate_id=? AND call_status='PENDING'`, analysisID, candidateID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate analysis is no longer pending")
	}
	res, err = tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,review_count=max(review_count-1,0),last_analysis_id=?,last_error='',pending_trigger='SAME_DATA_SKIPPED',
		next_review_at=CASE WHEN regular_review_at IS NOT NULL AND regular_review_at>CURRENT_TIMESTAMP THEN regular_review_at ELSE ? END,
		regular_review_at=CASE WHEN regular_review_at IS NOT NULL AND regular_review_at>CURRENT_TIMESTAMP THEN regular_review_at ELSE ? END,
		updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`,
		ReentryCandidateWaiting, analysisID, next.UTC(), next.UTC(), candidateID, ReentryCandidateReviewing)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate review is no longer active")
	}
	return tx.Commit()
}

func (s *ReentryAIStore) MarkReentryCandidateStatus(id int64, status, message string) error {
	closed := ""
	if status == ReentryCandidateReentered || status == ReentryCandidateAbandoned || status == ReentryCandidateExpired || status == ReentryCandidateBudgetSuspended || status == ReentryCandidateInvalidated {
		closed = ",closed_at=CURRENT_TIMESTAMP"
	}
	_, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_error=?,updated_at=CURRENT_TIMESTAMP`+closed+` WHERE id=?`, status, message, id)
	return err
}

// CompleteReentryCandidate performs the one-way commit after exchange
// protection is verified. The conditional transition makes restart recovery
// and notification idempotent.
func (s *ReentryAIStore) CompleteReentryCandidate(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_error='entry filled and protection verified',closed_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`, ReentryCandidateReentered, id, ReentryCandidateEntryPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RecoverStaleReentryCandidateLeases returns work abandoned by a crashed
// analyzer/executor to WAITING. ENTRY_PENDING is recovered only while the
// trading cycle itself is still a flat observation state; an in-flight or
// filled order is owned by the exchange-order recovery path.
func (s *ReentryAIStore) RecoverStaleReentryCandidateLeases(maxAge time.Duration) (int64, error) {
	if maxAge < time.Minute {
		maxAge = 10 * time.Minute
	}
	cutoff := time.Now().Add(-maxAge).UTC().Format("2006-01-02 15:04:05")
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	const staleReviewError = "stale review lease recovered after restart"
	// PENDING means no paid request started. Close the orphan analysis and give
	// its lifecycle/daily quota back by decrementing review_count below.
	if _, err = tx.Exec(`UPDATE reentry_ai_analyses SET call_status='PREPARE_FAILED',call_error=?,updated_at=CURRENT_TIMESTAMP
		WHERE call_status='PENDING' AND id IN (
			SELECT MAX(a.id) FROM reentry_ai_analyses a
			JOIN copy_guard_reentry_candidates c ON c.id=a.candidate_id
			WHERE c.status=? AND datetime(c.updated_at)<datetime(?) GROUP BY a.candidate_id
		)`, staleReviewError, ReentryCandidateReviewing, cutoff); err != nil {
		return 0, err
	}
	// RUNNING means the request may already have reached the model provider;
	// preserve its quota consumption and make the uncertain call explicit.
	if _, err = tx.Exec(`UPDATE reentry_ai_analyses SET call_status='FAILED',call_error=?,updated_at=CURRENT_TIMESTAMP
		WHERE call_status='RUNNING' AND id IN (
			SELECT MAX(a.id) FROM reentry_ai_analyses a
			JOIN copy_guard_reentry_candidates c ON c.id=a.candidate_id
			WHERE c.status=? AND datetime(c.updated_at)<datetime(?) GROUP BY a.candidate_id
		)`, staleReviewError, ReentryCandidateReviewing, cutoff); err != nil {
		return 0, err
	}
	res1, err := tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,
		review_count=max(review_count-CASE WHEN EXISTS (
			SELECT 1 FROM reentry_ai_analyses a WHERE a.candidate_id=copy_guard_reentry_candidates.id
			AND a.id=(SELECT MAX(a2.id) FROM reentry_ai_analyses a2 WHERE a2.candidate_id=copy_guard_reentry_candidates.id)
			AND a.call_status='PREPARE_FAILED' AND a.call_error=?
		) THEN 1 ELSE 0 END,0),
		failure_count=failure_count+1,last_error=?,pending_trigger='LEASE_RECOVERED',next_review_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
		WHERE status=? AND datetime(updated_at)<datetime(?)`, ReentryCandidateWaiting, staleReviewError, staleReviewError, ReentryCandidateReviewing, cutoff)
	if err != nil {
		return 0, err
	}
	res2, err := tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,failure_count=failure_count+1,last_error='stale entry lease recovered before order submission',pending_trigger='LEASE_RECOVERED',next_review_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE status=? AND datetime(updated_at)<datetime(?) AND EXISTS (SELECT 1 FROM copy_guard_cycles c WHERE c.id=copy_guard_reentry_candidates.cycle_id AND c.status IN ('STOPPED_WATCHING','AI_WATCHING','AI_WAITING','ATTEMPTS_EXHAUSTED'))`, ReentryCandidateWaiting, ReentryCandidateEntryPending, cutoff)
	if err != nil {
		return 0, err
	}
	n1, _ := res1.RowsAffected()
	n2, _ := res2.RowsAffected()
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET status='AI_WAITING',updated_at=CURRENT_TIMESTAMP WHERE closed_at IS NULL AND status='AI_REVIEWING' AND id IN (SELECT cycle_id FROM copy_guard_reentry_candidates WHERE status='WAITING' AND pending_trigger='LEASE_RECOVERED')`); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return n1 + n2, nil
}

func (s *ReentryAIStore) PauseReentryCandidate(id int64) error {
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_error='paused by operator',updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN (?,?,?)`, ReentryCandidatePaused, id, ReentryCandidateWatching, ReentryCandidateWaiting, ReentryCandidateReviewing)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate cannot be paused while entry is pending or after it is terminal")
	}
	return nil
}

func (s *ReentryAIStore) TerminateReentryCandidate(id int64) error {
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_error='terminated by operator',closed_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN (?,?,?,?)`, ReentryCandidateInvalidated, id, ReentryCandidateWatching, ReentryCandidateWaiting, ReentryCandidateReviewing, ReentryCandidatePaused)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate cannot be terminated while an entry may be in flight or after it is terminal")
	}
	return nil
}

// DisableReentryCandidatesForTrader closes every candidate that cannot have
// reached the exchange. ENTRY_PENDING is also closed only when no submitted or
// filled execution intent exists; in-flight exchange work remains owned by the
// durable reconciliation path.
func (s *ReentryAIStore) DisableReentryCandidatesForTrader(traderID, reason string) error {
	if traderID == "" {
		return fmt.Errorf("trader id is required")
	}
	if reason == "" {
		reason = "account protection disabled"
	}
	_, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=?,last_error=?,closed_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND (
			status IN (?,?,?,?) OR
			(status=? AND NOT EXISTS (
				SELECT 1 FROM copy_trade_execution_intents i
				WHERE i.candidate_id=copy_guard_reentry_candidates.id
				  AND i.status IN ('SUBMITTED','FILLED','PROTECTED','RECONCILING')
			))
		)`,
		ReentryCandidateInvalidated, reason, traderID,
		ReentryCandidateWatching, ReentryCandidateWaiting, ReentryCandidateReviewing, ReentryCandidatePaused,
		ReentryCandidateEntryPending,
	)
	return err
}

func (s *ReentryAIStore) ResumeReentryCandidate(id int64) error {
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_error='',next_review_at=CURRENT_TIMESTAMP,pending_trigger='OPERATOR_RESUME',closed_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`, ReentryCandidateWaiting, id, ReentryCandidatePaused)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate is not paused")
	}
	return nil
}

func (s *ReentryAIStore) PauseReentryCandidateForTraderStop(id int64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=?,last_error='discarded because trader lifecycle stopped',
		    pending_trigger='TRADER_STOPPED',updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN ('REVIEWING','WATCHING','WAITING','ENTRY_PENDING')`,
		ReentryCandidatePausedByTrader, id)
	return err
}

// ResumeTraderCandidatesForStart revives only observations whose cycle and
// ownership mapping are still authoritative. Stale paused rows become explicit
// invalidations instead of silently surviving an archive/reversal.
func (s *ReentryAIStore) ResumeTraderCandidatesForStart(traderID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	valid := `EXISTS(
		SELECT 1 FROM copy_guard_cycles c
		JOIN copy_trade_position_mappings m
		  ON m.trader_id=c.trader_id AND m.leader_pos_id=c.leader_pos_id
		WHERE c.id=copy_guard_reentry_candidates.cycle_id
		  AND c.trader_id=copy_guard_reentry_candidates.trader_id
		  AND c.closed_at IS NULL
		  AND c.status IN ('STOPPED_WATCHING','AI_WATCHING','AI_REVIEWING','AI_WAITING','ATTEMPTS_EXHAUSTED')
		  AND m.status='stopped_by_risk'
	)`
	if _, err = tx.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=?,last_error='',pending_trigger='TRADER_RESTARTED',
		    next_review_at=CURRENT_TIMESTAMP,regular_review_at=CURRENT_TIMESTAMP,
		    closed_at=NULL,updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND status=? AND `+valid,
		ReentryCandidateWaiting, traderID, ReentryCandidatePausedByTrader); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=?,last_error='paused candidate no longer has an active leader cycle',
		    pending_trigger='TRADER_RESTART_INVALIDATED',
		    closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND status=? AND NOT (`+valid+`)`,
		ReentryCandidateInvalidated, traderID, ReentryCandidatePausedByTrader); err != nil {
		return err
	}
	return tx.Commit()
}

// RequestImmediateReentryCandidateReview schedules a production review at the
// earliest time permitted by the trader's minimum interval. It never calls a
// model or changes execution state; the normal scheduler still owns
// feature-hash dedupe, soft cost warnings, leases and preflight.
func (s *ReentryAIStore) RequestImmediateReentryCandidateReview(id int64, minInterval time.Duration) (*CopyGuardReentryCandidate, error) {
	return s.ScheduleReentryCandidateEventReview(id, "OPERATOR_REVIEW_REQUEST", minInterval)
}

// ScheduleReentryCandidateEventReview is the only event-review scheduler.
// Event signals may pull a regular review forward, while failure backoff
// remains a hard lower bound that feature changes cannot bypass. Legacy budget
// fields are cost warnings and never participate in eligibility.
func (s *ReentryAIStore) ScheduleReentryCandidateEventReview(id int64, trigger string, minInterval time.Duration) (*CopyGuardReentryCandidate, error) {
	if minInterval < 5*time.Minute {
		minInterval = 5 * time.Minute
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var status string
	var reviewAllowed bool
	var last, regular, failureBackoff sql.NullString
	if err = tx.QueryRow(`SELECT c.status,c.last_review_at,c.regular_review_at,c.failure_backoff_until,
			EXISTS(SELECT 1 FROM traders t
				WHERE t.id=c.trader_id AND t.lifecycle_status='RUNNING' AND t.is_running=1)
			AND EXISTS(SELECT 1 FROM copy_guard_cycles g
				WHERE g.id=c.cycle_id AND g.closed_at IS NULL
				  AND g.status IN (
					'STOPPED_WATCHING','AI_WATCHING','AI_REVIEWING','AI_WAITING',
					'ATTEMPTS_EXHAUSTED','BUDGET_SUSPENDED'
				  ))
			FROM copy_guard_reentry_candidates c WHERE c.id=?`, id).
		Scan(&status, &last, &regular, &failureBackoff, &reviewAllowed); err != nil {
		return nil, err
	}
	if status != ReentryCandidateWatching && status != ReentryCandidateWaiting {
		return nil, fmt.Errorf("candidate status %s cannot be manually reviewed", status)
	}
	if !reviewAllowed {
		return nil, fmt.Errorf("candidate trader or cycle is no longer reviewable")
	}
	eligible := time.Now().UTC()
	if last.Valid {
		lastReview, parseErr := parseDBTime(last.String)
		if parseErr != nil {
			return nil, parseErr
		}
		if next := lastReview.Add(minInterval); next.After(eligible) {
			eligible = next.UTC()
		}
	}
	next := eligible
	if regular.Valid {
		if regularAt, parseErr := parseDBTime(regular.String); parseErr != nil {
			return nil, parseErr
		} else if regularAt.Before(next) {
			next = regularAt
		}
	}
	for _, blocked := range []sql.NullString{failureBackoff} {
		if !blocked.Valid {
			continue
		}
		blockedAt, parseErr := parseDBTime(blocked.String)
		if parseErr != nil {
			return nil, parseErr
		}
		if blockedAt.After(next) {
			next = blockedAt
		}
	}
	res, err := tx.Exec(`UPDATE copy_guard_reentry_candidates SET
		event_review_at=?,next_review_at=?,pending_trigger=?,last_error='',updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN (?,?)`,
		eligible, next.UTC(), trigger, id, ReentryCandidateWatching, ReentryCandidateWaiting)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("candidate was claimed or changed before review request")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetReentryCandidate(id)
}

func (s *ReentryAIStore) ListReentryCandidatesByCycle(cycleID int64) ([]*CopyGuardReentryCandidate, error) {
	c, err := s.GetReentryCandidateByCycle(cycleID)
	if err == sql.ErrNoRows {
		return []*CopyGuardReentryCandidate{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []*CopyGuardReentryCandidate{c}, nil
}

// ListReentryCandidatesByTraders powers the active-candidate dashboard while
// preserving the same trader ownership boundary as the cycle APIs.
func (s *ReentryAIStore) ListReentryCandidatesByTraders(traderIDs []string, statuses []string, limit int) ([]*CopyGuardReentryCandidate, error) {
	out := []*CopyGuardReentryCandidate{}
	if len(traderIDs) == 0 {
		return out, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	args := make([]interface{}, 0, len(traderIDs)+len(statuses)+1)
	for _, id := range traderIDs {
		args = append(args, id)
	}
	q := `SELECT ` + reentryCandidateColumns + ` FROM copy_guard_reentry_candidates WHERE trader_id IN (` + sqlMarks(len(traderIDs)) + `)`
	if len(statuses) > 0 {
		q += ` AND status IN (` + sqlMarks(len(statuses)) + `)`
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	q += ` ORDER BY updated_at DESC,id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		candidate, err := scanReentryCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

// ReserveCopyGuardRisk 在同一 SQLite 事务内校验并预留单次、周期和组合风险，
// 防止多个交易员的 AI 决策同时通过各自的内存检查。
func (s *ReentryAIStore) ReserveCopyGuardRisk(traderID string, cycleID int64, attemptNo int, riskUSD, equity, attemptPct, cyclePct, portfolioPct float64) error {
	return s.reserveCopyGuardRisk(traderID, cycleID, attemptNo, 0, riskUSD, equity, attemptPct, cyclePct, portfolioPct, false)
}

// ReserveCopyGuardRiskFollowFirst permits only the discrete execution-step
// exception selected by the operator: the attempt target may be exceeded, but
// cycle and account-portfolio budgets remain hard transactional limits.
func (s *ReentryAIStore) ReserveCopyGuardRiskFollowFirst(traderID string, cycleID int64, attemptNo int, riskUSD, equity, attemptPct, cyclePct, portfolioPct float64) error {
	return s.reserveCopyGuardRisk(traderID, cycleID, attemptNo, 0, riskUSD, equity, attemptPct, cyclePct, portfolioPct, true)
}

func (s *ReentryAIStore) reserveCopyGuardRisk(traderID string, cycleID int64, attemptNo int, intentID int64, riskUSD, equity, attemptPct, cyclePct, portfolioPct float64, allowAttemptStepOverride bool) error {
	if riskUSD <= 0 || equity <= 0 {
		return fmt.Errorf("invalid risk reservation")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	accountKey := traderID
	var exchangeID sql.NullString
	if queryErr := tx.QueryRow(`SELECT exchange_id FROM traders WHERE id=?`, traderID).Scan(&exchangeID); queryErr == nil && exchangeID.Valid && exchangeID.String != "" {
		accountKey = exchangeID.String
	}
	var cycleUsed, portfolioUsed float64
	// Cycle budget is cumulative: a stopped attempt has consumed its reserved
	// risk even though it no longer contributes to the account's live exposure.
	// Portfolio budget, by contrast, is an aggregate of currently ACTIVE risk.
	if err = tx.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE cycle_id=? AND status IN ('ACTIVE','CONSUMED') AND attempt_no<>?`, cycleID, attemptNo).Scan(&cycleUsed); err != nil {
		return err
	}
	if err = tx.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE account_key=? AND status='ACTIVE' AND NOT (cycle_id=? AND attempt_no=?)`, accountKey, cycleID, attemptNo).Scan(&portfolioUsed); err != nil {
		return err
	}
	if !allowAttemptStepOverride && riskUSD > equity*attemptPct+1e-9 {
		return fmt.Errorf("attempt risk %.4f exceeds %.4f", riskUSD, equity*attemptPct)
	}
	if cycleUsed+riskUSD > equity*cyclePct+1e-9 {
		return fmt.Errorf("cycle risk %.4f exceeds remaining budget %.4f", riskUSD, equity*cyclePct-cycleUsed)
	}
	if portfolioUsed+riskUSD > equity*portfolioPct+1e-9 {
		return fmt.Errorf("portfolio risk %.4f exceeds remaining budget %.4f", riskUSD, equity*portfolioPct-portfolioUsed)
	}
	_, err = tx.Exec(`INSERT INTO copy_guard_risk_reservations
		(trader_id,account_key,cycle_id,attempt_no,intent_id,target_risk_usd,risk_usd,status)
		VALUES(?,?,?,?,?,?,?,'ACTIVE')
		ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET
			trader_id=excluded.trader_id,account_key=excluded.account_key,intent_id=excluded.intent_id,
			target_risk_usd=excluded.target_risk_usd,risk_usd=excluded.risk_usd,
			status='ACTIVE',released_at=NULL`,
		traderID, accountKey, cycleID, attemptNo, intentID, riskUSD, riskUSD)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ReserveCopyGuardIntentRisk atomically reserves the target risk before a
// leader open/add reaches the exchange. For an add, the existing cycle attempt
// remains active and the temporary row contributes only the positive delta, so
// concurrent account checks see the exact target rather than double-counting.
func (s *ReentryAIStore) ReserveCopyGuardIntentRisk(traderID string, intentID, replaceCycleID int64, replaceAttemptNo int, targetRiskUSD, equity, attemptPct, cyclePct, portfolioPct float64, allowAttemptStepOverride bool) error {
	if intentID <= 0 || targetRiskUSD <= 0 || equity <= 0 {
		return fmt.Errorf("invalid execution intent risk reservation")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	accountKey := traderID
	var exchangeID sql.NullString
	if queryErr := tx.QueryRow(`SELECT exchange_id FROM traders WHERE id=?`, traderID).Scan(&exchangeID); queryErr == nil && exchangeID.Valid && exchangeID.String != "" {
		accountKey = exchangeID.String
	}
	var oldRisk, cycleUsed, portfolioUsed float64
	if replaceCycleID > 0 {
		oldErr := tx.QueryRow(`SELECT COALESCE(risk_usd,0) FROM copy_guard_risk_reservations WHERE cycle_id=? AND attempt_no=? AND status='ACTIVE'`, replaceCycleID, replaceAttemptNo).Scan(&oldRisk)
		if oldErr != nil && oldErr != sql.ErrNoRows {
			return oldErr
		}
		if err = tx.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE cycle_id=? AND status IN ('ACTIVE','CONSUMED') AND attempt_no<>?`, replaceCycleID, replaceAttemptNo).Scan(&cycleUsed); err != nil {
			return err
		}
	}
	if err = tx.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE account_key=? AND status='ACTIVE' AND cycle_id<>? AND NOT (cycle_id=? AND attempt_no=?)`, accountKey, -intentID, replaceCycleID, replaceAttemptNo).Scan(&portfolioUsed); err != nil {
		return err
	}
	if !allowAttemptStepOverride && targetRiskUSD > equity*attemptPct+1e-9 {
		return fmt.Errorf("attempt risk %.4f exceeds %.4f", targetRiskUSD, equity*attemptPct)
	}
	if cycleUsed+targetRiskUSD > equity*cyclePct+1e-9 {
		return fmt.Errorf("cycle risk %.4f exceeds remaining budget %.4f", targetRiskUSD, equity*cyclePct-cycleUsed)
	}
	if portfolioUsed+targetRiskUSD > equity*portfolioPct+1e-9 {
		return fmt.Errorf("portfolio risk %.4f exceeds remaining budget %.4f", targetRiskUSD, equity*portfolioPct-portfolioUsed)
	}
	additionalRisk := math.Max(0, targetRiskUSD-oldRisk)
	_, err = tx.Exec(`INSERT INTO copy_guard_risk_reservations
		(trader_id,account_key,cycle_id,attempt_no,intent_id,replace_cycle_id,replace_attempt_no,target_risk_usd,attempt_override,risk_usd,status)
		VALUES(?,?,?,0,?,?,?,?,?,?,'ACTIVE')
		ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET trader_id=excluded.trader_id,account_key=excluded.account_key,intent_id=excluded.intent_id,
			replace_cycle_id=excluded.replace_cycle_id,replace_attempt_no=excluded.replace_attempt_no,target_risk_usd=excluded.target_risk_usd,
			attempt_override=excluded.attempt_override,risk_usd=excluded.risk_usd,status='ACTIVE',released_at=NULL`,
		traderID, accountKey, -intentID, intentID, replaceCycleID, replaceAttemptNo, targetRiskUSD, allowAttemptStepOverride, additionalRisk)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// PromoteCopyGuardIntentRisk transfers the live pre-submit reservation to the
// real cycle after the exchange fill and protective stop are confirmed. It
// revalidates the actual fill risk while excluding the temporary row, so the
// promotion cannot overbook the account under concurrent traders.
func (s *ReentryAIStore) PromoteCopyGuardIntentRisk(traderID string, intentID, cycleID int64, attemptNo int, riskUSD, equity, attemptPct, cyclePct, portfolioPct float64, allowAttemptStepOverride bool) (bool, error) {
	if intentID <= 0 || cycleID <= 0 || riskUSD <= 0 || equity <= 0 {
		return false, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var reservationID int64
	var accountKey string
	var reservedTarget float64
	var replaceCycleID int64
	var replaceAttemptNo int
	var storedOverride bool
	if err = tx.QueryRow(`SELECT id,account_key,target_risk_usd,replace_cycle_id,replace_attempt_no,attempt_override FROM copy_guard_risk_reservations WHERE intent_id=? AND cycle_id=? AND status='ACTIVE'`, intentID, -intentID).Scan(&reservationID, &accountKey, &reservedTarget, &replaceCycleID, &replaceAttemptNo, &storedOverride); err == sql.ErrNoRows {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if replaceCycleID != 0 && (replaceCycleID != cycleID || replaceAttemptNo != attemptNo) {
		return false, fmt.Errorf("intent risk target changed: reserved cycle=%d/%d actual=%d/%d", replaceCycleID, replaceAttemptNo, cycleID, attemptNo)
	}
	var cycleUsed, portfolioUsed float64
	if err = tx.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE cycle_id=? AND status IN ('ACTIVE','CONSUMED') AND attempt_no<>?`, cycleID, attemptNo).Scan(&cycleUsed); err != nil {
		return false, err
	}
	if err = tx.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE account_key=? AND status='ACTIVE' AND id<>? AND NOT (cycle_id=? AND attempt_no=?)`, accountKey, reservationID, cycleID, attemptNo).Scan(&portfolioUsed); err != nil {
		return false, err
	}
	// A temporary reservation above the attempt target is durable evidence that
	// the one-step exception passed the hard caps before submission. Preserve
	// it across a process restart where the in-memory decision flag is lost.
	allowAttemptStepOverride = allowAttemptStepOverride || storedOverride || reservedTarget > equity*attemptPct+1e-9
	if !allowAttemptStepOverride && riskUSD > equity*attemptPct+1e-9 {
		return false, fmt.Errorf("attempt risk %.4f exceeds %.4f", riskUSD, equity*attemptPct)
	}
	if cycleUsed+riskUSD > equity*cyclePct+1e-9 {
		return false, fmt.Errorf("cycle risk %.4f exceeds remaining budget %.4f", riskUSD, equity*cyclePct-cycleUsed)
	}
	if portfolioUsed+riskUSD > equity*portfolioPct+1e-9 {
		return false, fmt.Errorf("portfolio risk %.4f exceeds remaining budget %.4f", riskUSD, equity*portfolioPct-portfolioUsed)
	}
	if _, err = tx.Exec(`INSERT INTO copy_guard_risk_reservations
		(trader_id,account_key,cycle_id,attempt_no,intent_id,target_risk_usd,risk_usd,status)
		VALUES(?,?,?,?,0,?,?,'ACTIVE')
		ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET
			trader_id=excluded.trader_id,account_key=excluded.account_key,intent_id=0,
			target_risk_usd=excluded.target_risk_usd,risk_usd=excluded.risk_usd,
			status='ACTIVE',released_at=NULL`,
		traderID, accountKey, cycleID, attemptNo, riskUSD, riskUSD); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_risk_reservations SET status='RELEASED',released_at=CURRENT_TIMESTAMP WHERE id=? AND status='ACTIVE'`, reservationID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *ReentryAIStore) ReleaseCopyGuardIntentRisk(intentID int64) error {
	if intentID <= 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE copy_guard_risk_reservations SET status='RELEASED',released_at=CURRENT_TIMESTAMP WHERE intent_id=? AND status='ACTIVE'`, intentID)
	return err
}

func (s *ReentryAIStore) ReleaseCopyGuardRisk(cycleID int64, attemptNo int) error {
	_, err := s.db.Exec(`UPDATE copy_guard_risk_reservations SET status='RELEASED',released_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND attempt_no=? AND status='ACTIVE'`, cycleID, attemptNo)
	return err
}

// ReduceCopyGuardRiskToFill releases only the unused share of an AI attempt's
// reservation after the exchange has terminally completed with a partial fill.
// target_risk_usd is the immutable pre-fill base. Legacy rows backfill that
// base from their current risk on the first reduction. Replaying the same
// terminal exchange snapshot therefore writes the same absolute value instead
// of multiplying the ratio repeatedly.
func (s *ReentryAIStore) ReduceCopyGuardRiskToFill(cycleID int64, attemptNo int, filledQuantity, requestedQuantity float64) error {
	if cycleID <= 0 || attemptNo <= 0 || filledQuantity <= 0 || requestedQuantity <= 0 ||
		filledQuantity >= requestedQuantity {
		return nil
	}
	ratio := filledQuantity / requestedQuantity
	_, err := s.db.Exec(`UPDATE copy_guard_risk_reservations
		SET target_risk_usd=CASE WHEN target_risk_usd>0 THEN target_risk_usd ELSE risk_usd END,
			risk_usd=(CASE WHEN target_risk_usd>0 THEN target_risk_usd ELSE risk_usd END)*?
		WHERE cycle_id=? AND attempt_no=? AND status='ACTIVE'`,
		ratio, cycleID, attemptNo)
	return err
}

// ConsumeCopyGuardRisk closes live portfolio exposure after a confirmed stop
// or forced exit while retaining the attempt's planned risk against the open
// cycle's cumulative budget. It is idempotent and never revives released risk.
func (s *ReentryAIStore) ConsumeCopyGuardRisk(cycleID int64, attemptNo int) error {
	_, err := s.db.Exec(`UPDATE copy_guard_risk_reservations SET status='CONSUMED',released_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND attempt_no=? AND status='ACTIVE'`, cycleID, attemptNo)
	return err
}

type CopyGuardRiskUsage struct {
	CycleUsedUSD     float64 `json:"cycle_used_usd"`
	PortfolioUsedUSD float64 `json:"portfolio_used_usd"`
}

func (s *ReentryAIStore) GetCopyGuardRiskUsage(traderID string, cycleID int64) (CopyGuardRiskUsage, error) {
	usage := CopyGuardRiskUsage{}
	accountKey := traderID
	var exchangeID sql.NullString
	if err := s.db.QueryRow(`SELECT exchange_id FROM traders WHERE id=?`, traderID).Scan(&exchangeID); err == nil && exchangeID.Valid && exchangeID.String != "" {
		accountKey = exchangeID.String
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE cycle_id=? AND status IN ('ACTIVE','CONSUMED')`, cycleID).Scan(&usage.CycleUsedUSD); err != nil {
		return usage, err
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE account_key=? AND status='ACTIVE'`, accountKey).Scan(&usage.PortfolioUsedUSD); err != nil {
		return usage, err
	}
	return usage, nil
}

// GetCopyGuardRiskUsageExcludingAttempt returns the risk already occupying the
// cycle/account budgets other than the attempt being (re)sized. Add-ons replace
// the reservation for their current attempt, so counting it here would shrink
// the same risk twice. New reentry attempts naturally exclude zero rows.
func (s *ReentryAIStore) GetCopyGuardRiskUsageExcludingAttempt(traderID string, cycleID int64, attemptNo int) (CopyGuardRiskUsage, error) {
	usage := CopyGuardRiskUsage{}
	accountKey := traderID
	var exchangeID sql.NullString
	if err := s.db.QueryRow(`SELECT exchange_id FROM traders WHERE id=?`, traderID).Scan(&exchangeID); err == nil && exchangeID.Valid && exchangeID.String != "" {
		accountKey = exchangeID.String
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE cycle_id=? AND status IN ('ACTIVE','CONSUMED') AND attempt_no<>?`, cycleID, attemptNo).Scan(&usage.CycleUsedUSD); err != nil {
		return usage, err
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(risk_usd),0) FROM copy_guard_risk_reservations WHERE account_key=? AND status='ACTIVE' AND NOT (cycle_id=? AND attempt_no=?)`, accountKey, cycleID, attemptNo).Scan(&usage.PortfolioUsedUSD); err != nil {
		return usage, err
	}
	return usage, nil
}
