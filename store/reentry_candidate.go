package store

import (
	"database/sql"
	"fmt"
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

	TriggerPrice        float64 `json:"trigger_price"`
	ATR                 float64 `json:"atr"`
	MaxNotional         float64 `json:"max_notional"`
	StopCount           int     `json:"stop_count"`
	ReentryCount        int     `json:"reentry_count"`
	LeaderSize          float64 `json:"leader_size"`
	LeaderEntryPrice    float64 `json:"leader_entry_price"`
	LastStopPrice       float64 `json:"last_stop_price"`
	DistanceATRRatio    float64 `json:"distance_atr_ratio"`
	Protectable         bool    `json:"protectable"`
	FeatureHash         string  `json:"feature_hash"`
	PendingTrigger      string  `json:"pending_trigger"`
	DecisionGeneration  int     `json:"decision_generation"`
	ReviewCount         int     `json:"review_count"`
	FailureCount        int     `json:"failure_count"`
	LastDecision        string  `json:"last_decision"`
	Regime              string  `json:"regime"`
	Confidence          float64 `json:"confidence"`
	SizeFactor          float64 `json:"size_factor"`
	EntryPriceLow       float64 `json:"entry_price_low"`
	EntryPriceHigh      float64 `json:"entry_price_high"`
	AttentionPriceLow   float64 `json:"attention_price_low"`
	AttentionPriceHigh  float64 `json:"attention_price_high"`
	ConsecutiveAbandons int     `json:"consecutive_abandons"`
	LastAbandonCandle   string  `json:"last_abandon_candle"`
	LastAnalysisID      int64   `json:"last_analysis_id"`
	DecisionTTLSeconds  int     `json:"decision_ttl_seconds"`
	LastError           string  `json:"last_error"`

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
			snapshot_at DATETIME DEFAULT CURRENT_TIMESTAMP, last_review_at DATETIME,
			next_review_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP, closed_at DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_cg_reentry_candidate_due ON copy_guard_reentry_candidates(status,next_review_at);
		CREATE INDEX IF NOT EXISTS idx_cg_reentry_candidate_trader ON copy_guard_reentry_candidates(trader_id,status);
		CREATE TABLE IF NOT EXISTS copy_guard_risk_reservations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL, account_key TEXT NOT NULL DEFAULT '', cycle_id INTEGER NOT NULL, attempt_no INTEGER NOT NULL,
			risk_usd REAL NOT NULL, status TEXT NOT NULL DEFAULT 'ACTIVE',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, released_at DATETIME,
			UNIQUE(cycle_id,attempt_no)
		);
		CREATE INDEX IF NOT EXISTS idx_cg_risk_reservation_active ON copy_guard_risk_reservations(status,trader_id);
	`)
	if err != nil {
		return err
	}
	_, _ = s.db.Exec(`ALTER TABLE copy_guard_reentry_candidates ADD COLUMN decision_ttl_seconds INTEGER DEFAULT 30`)
	_, _ = s.db.Exec(`ALTER TABLE copy_guard_risk_reservations ADD COLUMN account_key TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`UPDATE copy_guard_risk_reservations SET account_key=COALESCE((SELECT exchange_id FROM traders WHERE traders.id=copy_guard_risk_reservations.trader_id),trader_id) WHERE account_key=''`)
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
	_, err = s.db.Exec(`UPDATE copy_guard_manual_reentry_signals SET status='MIGRATED',error='migrated to AI candidate; manual confirmation endpoint retired' WHERE status='PENDING' AND EXISTS (SELECT 1 FROM copy_guard_reentry_candidates c WHERE c.cycle_id=copy_guard_manual_reentry_signals.cycle_id)`)
	return err
}

const reentryCandidateColumns = `id,cycle_id,trader_id,leader_pos_id,symbol,side,margin_mode,status,
	trigger_price,atr,max_notional,stop_count,reentry_count,leader_size,leader_entry_price,last_stop_price,
	distance_atr_ratio,protectable,feature_hash,pending_trigger,decision_generation,review_count,failure_count,
	last_decision,regime,confidence,size_factor,entry_price_low,entry_price_high,attention_price_low,attention_price_high,
	consecutive_abandons,last_abandon_candle,last_analysis_id,decision_ttl_seconds,last_error,snapshot_at,last_review_at,next_review_at,
	created_at,updated_at,closed_at`

func scanReentryCandidate(row rowScanner) (*CopyGuardReentryCandidate, error) {
	var c CopyGuardReentryCandidate
	var snapshot, next, created, updated string
	var last, closed sql.NullString
	if err := row.Scan(&c.ID, &c.CycleID, &c.TraderID, &c.LeaderPosID, &c.Symbol, &c.Side, &c.MarginMode, &c.Status,
		&c.TriggerPrice, &c.ATR, &c.MaxNotional, &c.StopCount, &c.ReentryCount, &c.LeaderSize, &c.LeaderEntryPrice, &c.LastStopPrice,
		&c.DistanceATRRatio, &c.Protectable, &c.FeatureHash, &c.PendingTrigger, &c.DecisionGeneration, &c.ReviewCount, &c.FailureCount,
		&c.LastDecision, &c.Regime, &c.Confidence, &c.SizeFactor, &c.EntryPriceLow, &c.EntryPriceHigh, &c.AttentionPriceLow, &c.AttentionPriceHigh,
		&c.ConsecutiveAbandons, &c.LastAbandonCandle, &c.LastAnalysisID, &c.DecisionTTLSeconds, &c.LastError, &snapshot, &last, &next, &created, &updated, &closed); err != nil {
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

// EnsureReentryCandidate 创建候选或在同周期再次止损后重置为观察态。
func (s *ReentryAIStore) EnsureReentryCandidate(c *CopyGuardReentryCandidate, firstReview time.Time) (*CopyGuardReentryCandidate, error) {
	if c == nil || c.CycleID <= 0 {
		return nil, fmt.Errorf("invalid reentry candidate")
	}
	_, err := s.db.Exec(`INSERT INTO copy_guard_reentry_candidates
		(cycle_id,trader_id,leader_pos_id,symbol,side,margin_mode,status,trigger_price,atr,max_notional,stop_count,reentry_count,leader_size,leader_entry_price,last_stop_price,distance_atr_ratio,protectable,feature_hash,pending_trigger,next_review_at,snapshot_at)
		VALUES(?,?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?,?,?,?, ?,CURRENT_TIMESTAMP)
		ON CONFLICT(cycle_id) DO UPDATE SET trader_id=excluded.trader_id,leader_pos_id=excluded.leader_pos_id,
		symbol=excluded.symbol,side=excluded.side,margin_mode=excluded.margin_mode,status=CASE WHEN copy_guard_reentry_candidates.reentry_count<>excluded.reentry_count THEN 'WATCHING' ELSE copy_guard_reentry_candidates.status END,
		trigger_price=excluded.trigger_price,atr=excluded.atr,max_notional=excluded.max_notional,stop_count=excluded.stop_count,reentry_count=excluded.reentry_count,
		leader_size=excluded.leader_size,leader_entry_price=excluded.leader_entry_price,last_stop_price=excluded.last_stop_price,
		distance_atr_ratio=excluded.distance_atr_ratio,protectable=excluded.protectable,
		pending_trigger=CASE WHEN copy_guard_reentry_candidates.feature_hash<>excluded.feature_hash THEN excluded.pending_trigger ELSE copy_guard_reentry_candidates.pending_trigger END,
		next_review_at=CASE WHEN copy_guard_reentry_candidates.feature_hash<>excluded.feature_hash THEN CASE WHEN excluded.next_review_at>CURRENT_TIMESTAMP THEN excluded.next_review_at ELSE CURRENT_TIMESTAMP END ELSE copy_guard_reentry_candidates.next_review_at END,
		feature_hash=excluded.feature_hash,
		closed_at=CASE WHEN copy_guard_reentry_candidates.reentry_count<>excluded.reentry_count THEN NULL ELSE copy_guard_reentry_candidates.closed_at END,
		snapshot_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
		WHERE copy_guard_reentry_candidates.status IN ('WATCHING','WAITING') OR copy_guard_reentry_candidates.reentry_count<>excluded.reentry_count`,
		c.CycleID, c.TraderID, c.LeaderPosID, c.Symbol, c.Side, c.MarginMode, ReentryCandidateWatching, c.TriggerPrice, c.ATR, c.MaxNotional, c.StopCount, c.ReentryCount, c.LeaderSize, c.LeaderEntryPrice, c.LastStopPrice, c.DistanceATRRatio, c.Protectable, c.FeatureHash, c.PendingTrigger, firstReview.UTC())
	if err != nil {
		return nil, err
	}
	return s.GetReentryCandidateByCycle(c.CycleID)
}

func (s *ReentryAIStore) ListDueReentryCandidates(limit int) ([]*CopyGuardReentryCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+reentryCandidateColumns+` FROM copy_guard_reentry_candidates WHERE status IN ('WATCHING','WAITING') AND next_review_at<=CURRENT_TIMESTAMP ORDER BY next_review_at,id LIMIT ?`, limit)
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

// ClaimReentryCandidateReview 原子领取一次 AI 调用额度。
func (s *ReentryAIStore) ClaimReentryCandidateReview(id int64, minInterval time.Duration, dailyLimit, lifecycleLimit int) (*CopyGuardReentryCandidate, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var status string
	var count int
	var last sql.NullString
	if err = tx.QueryRow(`SELECT status,review_count,last_review_at FROM copy_guard_reentry_candidates WHERE id=?`, id).Scan(&status, &count, &last); err != nil {
		return nil, false, err
	}
	if status != ReentryCandidateWatching && status != ReentryCandidateWaiting {
		return nil, false, nil
	}
	if count >= lifecycleLimit {
		_, err = tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_error='AI lifecycle budget exhausted',closed_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, ReentryCandidateBudgetSuspended, id)
		if err != nil {
			return nil, false, err
		}
		if err = tx.Commit(); err != nil {
			return nil, false, err
		}
		c, _ := s.GetReentryCandidate(id)
		return c, false, nil
	}
	var daily int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM reentry_ai_analyses WHERE candidate_id=? AND call_status NOT IN ('SKIPPED','PREPARE_FAILED') AND created_at>=datetime('now','-24 hours')`, id).Scan(&daily); err != nil {
		return nil, false, err
	}
	if daily >= dailyLimit {
		_, err = tx.Exec(`UPDATE copy_guard_reentry_candidates SET next_review_at=datetime('now','+2 hours'),pending_trigger='DAILY_BUDGET',updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
		if err != nil {
			return nil, false, err
		}
		if err = tx.Commit(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	if last.Valid {
		if t, e := parseDBTime(last.String); e == nil && time.Since(t) < minInterval {
			return nil, false, nil
		}
	}
	res, err := tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,decision_generation=decision_generation+1,review_count=review_count+1,last_review_at=CURRENT_TIMESTAMP,next_review_at=?,pending_trigger='',updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('WATCHING','WAITING')`, ReentryCandidateReviewing, time.Now().Add(minInterval).UTC(), id)
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
	return c, true, err
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
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_decision=?,regime=?,confidence=?,size_factor=?,entry_price_low=?,entry_price_high=?,attention_price_low=?,attention_price_high=?,next_review_at=?,last_analysis_id=?,decision_ttl_seconds=?,failure_count=0,last_error='',consecutive_abandons=CASE WHEN ? AND last_abandon_candle<>? THEN consecutive_abandons+1 WHEN ? THEN consecutive_abandons ELSE 0 END,last_abandon_candle=CASE WHEN ? THEN ? ELSE '' END,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`, status, d.Decision, d.Regime, d.Confidence, d.SizeFactor, d.EntryPriceLow, d.EntryPriceHigh, d.AttentionPriceLow, d.AttentionPriceHigh, d.NextReview.UTC(), d.AnalysisID, d.TTLSeconds, d.ConfirmAbandon, d.CandleKey, d.ConfirmAbandon, d.ConfirmAbandon, d.CandleKey, id, ReentryCandidateReviewing)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate review is no longer active")
	}
	return nil
}

func (s *ReentryAIStore) FailReentryCandidateReview(id int64, message string, retry time.Duration) error {
	res, err := s.db.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,failure_count=failure_count+1,last_error=?,next_review_at=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN (?,?)`, ReentryCandidateWaiting, message, time.Now().Add(retry).UTC(), id, ReentryCandidateReviewing, ReentryCandidateEntryPending)
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
	res, err := tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,review_count=max(review_count-1,0),failure_count=failure_count+1,last_error=?,next_review_at=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`, ReentryCandidateWaiting, message, time.Now().Add(retry).UTC(), candidateID, ReentryCandidateReviewing)
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
	res, err = tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,review_count=max(review_count-1,0),last_analysis_id=?,last_error='',pending_trigger='SAME_DATA_SKIPPED',next_review_at=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`, ReentryCandidateWaiting, analysisID, next.UTC(), candidateID, ReentryCandidateReviewing)
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
	if riskUSD > equity*attemptPct+1e-9 {
		return fmt.Errorf("attempt risk %.4f exceeds %.4f", riskUSD, equity*attemptPct)
	}
	if cycleUsed+riskUSD > equity*cyclePct+1e-9 {
		return fmt.Errorf("cycle risk %.4f exceeds remaining budget %.4f", riskUSD, equity*cyclePct-cycleUsed)
	}
	if portfolioUsed+riskUSD > equity*portfolioPct+1e-9 {
		return fmt.Errorf("portfolio risk %.4f exceeds remaining budget %.4f", riskUSD, equity*portfolioPct-portfolioUsed)
	}
	_, err = tx.Exec(`INSERT INTO copy_guard_risk_reservations(trader_id,account_key,cycle_id,attempt_no,risk_usd,status) VALUES(?,?,?,?,?, 'ACTIVE') ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET trader_id=excluded.trader_id,account_key=excluded.account_key,risk_usd=excluded.risk_usd,status='ACTIVE',released_at=NULL`, traderID, accountKey, cycleID, attemptNo, riskUSD)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *ReentryAIStore) ReleaseCopyGuardRisk(cycleID int64, attemptNo int) error {
	_, err := s.db.Exec(`UPDATE copy_guard_risk_reservations SET status='RELEASED',released_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND attempt_no=? AND status='ACTIVE'`, cycleID, attemptNo)
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
