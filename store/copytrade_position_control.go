package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

var ErrFollowControlChanged = errors.New("position following was paused or changed; discard the old instruction")

// These records are independent of protection health: an absent position must
// never become owned again merely because its old protective order disappeared.
type CopyPositionCustody struct {
	TraderID        string    `json:"trader_id"`
	LeaderPosID     string    `json:"leader_pos_id"`
	InitialIntentID int64     `json:"initial_intent_id"`
	CycleID         int64     `json:"cycle_id"`
	AttemptNo       int       `json:"attempt_no"`
	Symbol          string    `json:"symbol"`
	Side            string    `json:"side"`
	MarginMode      string    `json:"margin_mode"`
	State           string    `json:"state"`
	Reason          string    `json:"reason"`
	OpenedAt        time.Time `json:"opened_at"`
}

func (s *CopyTradeStore) initPositionControlTables() error {
	_, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS copy_trade_follow_controls (
	 trader_id TEXT NOT NULL, leader_pos_id TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 0,
	 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY(trader_id,leader_pos_id));
	CREATE TABLE IF NOT EXISTS copy_trade_position_custody (
	 trader_id TEXT NOT NULL, leader_pos_id TEXT NOT NULL, initial_intent_id INTEGER NOT NULL,
	 cycle_id INTEGER NOT NULL DEFAULT 0, attempt_no INTEGER NOT NULL DEFAULT 0,
	 symbol TEXT NOT NULL, side TEXT NOT NULL, margin_mode TEXT NOT NULL DEFAULT '',
	 state TEXT NOT NULL DEFAULT 'MANAGED', reason TEXT NOT NULL DEFAULT '',
	 opened_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, released_at DATETIME,
	 PRIMARY KEY(trader_id,leader_pos_id));
	CREATE TABLE IF NOT EXISTS copy_guard_stop_controls (
	 cycle_id INTEGER NOT NULL, attempt_no INTEGER NOT NULL, revision INTEGER NOT NULL DEFAULT 0,
	 manual_price REAL NOT NULL DEFAULT 0, observed_algo_id TEXT NOT NULL DEFAULT '',
	 requested_price REAL NOT NULL DEFAULT 0, previous_price REAL NOT NULL DEFAULT 0,
	 request_pending INTEGER NOT NULL DEFAULT 0, request_at DATETIME,
	 generation INTEGER NOT NULL DEFAULT 0,
	 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY(cycle_id,attempt_no));
	CREATE TABLE IF NOT EXISTS copy_guard_protection_jobs (
	 trader_id TEXT NOT NULL, leader_pos_id TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
	 decision_json TEXT NOT NULL, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	 PRIMARY KEY(trader_id,leader_pos_id));
	CREATE TABLE IF NOT EXISTS copy_guard_exit_budgets (
	 cycle_id INTEGER NOT NULL, attempt_no INTEGER NOT NULL, initial_quantity REAL NOT NULL,
	 leader_net_at_start REAL NOT NULL, PRIMARY KEY(cycle_id,attempt_no));
	CREATE TABLE IF NOT EXISTS copy_guard_continuity_fills (
	 cycle_id INTEGER NOT NULL, attempt_no INTEGER NOT NULL, trade_id TEXT NOT NULL, order_id TEXT NOT NULL,
	 side TEXT NOT NULL, position_side TEXT NOT NULL, quantity REAL NOT NULL, time_ms INTEGER NOT NULL,
	 PRIMARY KEY(cycle_id,attempt_no,trade_id));
	CREATE TABLE IF NOT EXISTS copy_guard_continuity_checks (
	 cycle_id INTEGER NOT NULL,attempt_no INTEGER NOT NULL,checked_at_ms INTEGER NOT NULL,
	 PRIMARY KEY(cycle_id,attempt_no));`)
	if err != nil {
		return err
	}
	if err = ensureSQLiteColumn(s.db, "copy_trade_execution_intents", "follow_control_version", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err = ensureSQLiteColumn(s.db, "copy_trade_follow_controls", "resume_after", "DATETIME"); err != nil {
		return err
	}
	if err = ensureSQLiteColumn(s.db, "copy_guard_protection_jobs", "cycle_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Backfill only confirmed entries. A historical absence is an irreversible
	// ownership boundary even if subsequent legacy retries changed its status.
	_, err = s.db.Exec(`INSERT OR IGNORE INTO copy_trade_position_custody
	 (trader_id,leader_pos_id,initial_intent_id,cycle_id,attempt_no,symbol,side,margin_mode,state,reason,opened_at)
	 SELECT c.trader_id,c.leader_pos_id,c.initial_intent_id,c.id,c.reentry_count,c.symbol,c.side,c.margin_mode,
	 CASE WHEN c.closed_at IS NOT NULL OR EXISTS(SELECT 1 FROM copy_guard_events e WHERE e.cycle_id=c.id AND e.type='FOLLOWER_POSITION_ABSENT') THEN 'RELEASED' ELSE 'MANAGED' END,
	 CASE WHEN c.closed_at IS NOT NULL THEN 'CYCLE_CLOSED' WHEN EXISTS(SELECT 1 FROM copy_guard_events e WHERE e.cycle_id=c.id AND e.type='FOLLOWER_POSITION_ABSENT') THEN 'FOLLOWER_POSITION_ABSENT' ELSE '' END,c.opened_at
	 FROM copy_guard_cycles c WHERE c.initial_intent_id>0
	 AND c.id=(SELECT MAX(newer.id) FROM copy_guard_cycles newer WHERE newer.trader_id=c.trader_id AND newer.leader_pos_id=c.leader_pos_id)`)
	return err
}

func (s *CopyTradeStore) GetPositionCustody(traderID, leaderPosID string) (*CopyPositionCustody, error) {
	c := &CopyPositionCustody{}
	var opened string
	err := s.db.QueryRow(`SELECT trader_id,leader_pos_id,initial_intent_id,cycle_id,attempt_no,symbol,side,margin_mode,state,reason,opened_at FROM copy_trade_position_custody WHERE trader_id=? AND leader_pos_id=?`, traderID, leaderPosID).Scan(&c.TraderID, &c.LeaderPosID, &c.InitialIntentID, &c.CycleID, &c.AttemptNo, &c.Symbol, &c.Side, &c.MarginMode, &c.State, &c.Reason, &opened)
	if err != nil {
		return nil, err
	}
	c.OpenedAt, err = parseDBTime(opened)
	return c, err
}

func (s *CopyTradeStore) ReleasePositionCustody(traderID, leaderPosID string, initialIntentID int64, reason string) error {
	_, err := s.db.Exec(`UPDATE copy_trade_position_custody SET state='RELEASED',reason=?,released_at=COALESCE(released_at,CURRENT_TIMESTAMP) WHERE trader_id=? AND leader_pos_id=? AND initial_intent_id=?`, reason, traderID, leaderPosID, initialIntentID)
	return err
}

func (s *CopyTradeStore) ReleaseCopyGuardCustody(cycleID int64, attempt int, reason string) error {
	_, err := s.db.Exec(`INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,cycle_id,attempt_no,symbol,side,margin_mode,state,reason,opened_at,released_at)
	 SELECT trader_id,leader_pos_id,initial_intent_id,id,?,symbol,side,margin_mode,'RELEASED',?,opened_at,CURRENT_TIMESTAMP FROM copy_guard_cycles WHERE id=?
	 ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET state='RELEASED',reason=excluded.reason,released_at=COALESCE(copy_trade_position_custody.released_at,CURRENT_TIMESTAMP)
	 WHERE copy_trade_position_custody.cycle_id=excluded.cycle_id AND copy_trade_position_custody.attempt_no=excluded.attempt_no`, attempt, reason, cycleID)
	return err
}

// CheckFollowSubmission is repeated at the durable submission boundary, not
// just when a signal is queued. Resuming cannot revive pre-pause instructions.
func (s *CopyTradeStore) CheckFollowSubmission(intentID int64) error {
	return checkFollowSubmission(s.db, intentID)
}

func checkFollowSubmission(q interface {
	QueryRow(string, ...interface{}) *sql.Row
}, intentID int64) error {
	var allowed bool
	err := q.QueryRow(`SELECT CASE WHEN i.source_kind='LEADER_TRANSITION' THEN
	 (i.follow_control_version=COALESCE(f.version,0) AND COALESCE(m.status,'') NOT IN ('manual_stopped','detached','stopped_by_risk')
	 AND NOT EXISTS(SELECT 1 FROM copy_trade_position_custody p WHERE p.trader_id=i.trader_id AND p.leader_pos_id=i.leader_pos_id AND p.state='RELEASED' AND COALESCE(m.status,'')='active'))
	 WHEN i.source_kind='AI_REENTRY' THEN COALESCE(m.status,'')<>'manual_stopped' ELSE 1 END
	 FROM copy_trade_execution_intents i LEFT JOIN copy_trade_follow_controls f ON f.trader_id=i.trader_id AND f.leader_pos_id=i.leader_pos_id
	 LEFT JOIN copy_trade_position_mappings m ON m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id WHERE i.id=?`, intentID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrFollowControlChanged
	}
	return nil
}

// ResumeFollowing intentionally absorbs both adds AND reductions during the
// pause. Source-outage recovery has different semantics and must not be reused.
func (s *CopyTradeStore) ResumeFollowing(traderID, leaderPosID string, revision int64, leaderSize float64) error {
	if leaderSize <= 0 || math.IsNaN(leaderSize) || math.IsInf(leaderSize, 0) {
		return fmt.Errorf("invalid resume baseline")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pending int
	err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents i WHERE i.trader_id=? AND i.leader_pos_id=? AND
	 (EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id AND a.submitted_at IS NOT NULL AND a.terminal_at IS NULL)
	 OR COALESCE((SELECT SUM(a.filled_quantity) FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id),0)>i.filled_quantity+0.000000000001
	 OR (i.status IN ('SUBMITTED','RECONCILING') AND i.submitted_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id)))`, traderID, leaderPosID).Scan(&pending)
	if err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("submitted orders are still reconciling")
	}
	var released int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_position_custody WHERE trader_id=? AND leader_pos_id=? AND state='RELEASED'`, traderID, leaderPosID).Scan(&released); err != nil {
		return err
	}
	if released > 0 {
		return fmt.Errorf("original follower position has ended")
	}
	res, err := tx.Exec(`UPDATE copy_trade_position_mappings SET status='active',last_known_size=?,source_revision=source_revision+1,stopped_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='manual_stopped' AND source_revision=?`, leaderSize, traderID, leaderPosID, revision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrFollowControlChanged
	}
	if err = bumpFollowControlTx(tx, traderID, leaderPosID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_follow_controls SET resume_after=? WHERE trader_id=? AND leader_pos_id=?`, time.Now().UTC().Format(time.RFC3339Nano), traderID, leaderPosID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_execution_intents SET status=CASE WHEN filled_quantity>0 THEN 'COMPLETED_PARTIAL' ELSE 'SKIPPED' END,reason_code='MANUAL_RESUME_BASELINE',terminal_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND source_kind='LEADER_TRANSITION' AND status IN ('RESERVED','PARTIALLY_FILLED','RECONCILING') AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id AND a.submitted_at IS NOT NULL AND a.terminal_at IS NULL)`, traderID, leaderPosID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status=(SELECT status FROM copy_trade_execution_intents i WHERE i.id=intent_id),updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND intent_id IN (SELECT id FROM copy_trade_execution_intents WHERE reason_code='MANUAL_RESUME_BASELINE')`, traderID, leaderPosID); err != nil {
		return err
	}
	return tx.Commit()
}

func bumpFollowControlTx(tx *sql.Tx, traderID, leaderPosID string) error {
	_, err := tx.Exec(`INSERT INTO copy_trade_follow_controls(trader_id,leader_pos_id,version) VALUES(?,?,1) ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET version=version+1,updated_at=CURRENT_TIMESTAMP`, traderID, leaderPosID)
	return err
}

func (s *CopyTradeStore) PositionResumeCutoff(traderID, leaderPosID string) (*time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRow(`SELECT resume_after FROM copy_trade_follow_controls WHERE trader_id=? AND leader_pos_id=?`, traderID, leaderPosID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseNullableDBTime(raw)
}

func (s *CopyTradeStore) CopyGuardHasPositionAuthority(cycleID int64, attempt int) (bool, error) {
	var allowed bool
	err := s.db.QueryRow(`SELECT closed_at IS NULL
	 AND NOT EXISTS(SELECT 1 FROM copy_guard_events e WHERE e.cycle_id=c.id AND e.type='FOLLOWER_POSITION_ABSENT')
	 AND NOT EXISTS(SELECT 1 FROM copy_trade_position_custody p WHERE p.trader_id=c.trader_id AND p.leader_pos_id=c.leader_pos_id AND (p.state='RELEASED' OR p.cycle_id<>c.id OR p.attempt_no<>?))
	 FROM copy_guard_cycles c WHERE c.id=?`, attempt, cycleID).Scan(&allowed)
	return allowed, err
}

func bindCopyGuardCustodyTx(tx *sql.Tx, cycleID int64, attempt int) error {
	_, err := tx.Exec(`UPDATE copy_trade_position_custody SET cycle_id=?,attempt_no=?,state='MANAGED',reason='',opened_at=CURRENT_TIMESTAMP,released_at=NULL WHERE (trader_id,leader_pos_id)=(SELECT trader_id,leader_pos_id FROM copy_guard_cycles WHERE id=?) AND EXISTS(SELECT 1 FROM copy_guard_attempts WHERE cycle_id=? AND attempt_no=? AND entry_order_id<>'')`, cycleID, attempt, cycleID, cycleID, attempt)
	return err
}
