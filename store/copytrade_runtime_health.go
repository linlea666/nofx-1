package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Runtime follow-ups are separate from business execution. A protection or
// accounting retry must never turn an acknowledged exchange fill into an
// unresolved business order.
type CopyRuntimeIssue struct {
	TraderID    string    `json:"trader_id"`
	Area        string    `json:"area"`
	ResourceID  string    `json:"resource_id"`
	LeaderPosID string    `json:"leader_pos_id"`
	Symbol      string    `json:"symbol"`
	Side        string    `json:"side"`
	Code        string    `json:"code"`
	Detail      string    `json:"detail"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
}

type CopyRuntimeSource struct {
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastFailureAt *time.Time `json:"last_failure_at"`
	LastError     string     `json:"last_error"`
}

func (s *CopyTradeStore) initRuntimeHealthTables() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_runtime_issues (
 trader_id TEXT NOT NULL,area TEXT NOT NULL,resource_id TEXT NOT NULL,
 leader_pos_id TEXT NOT NULL DEFAULT '',symbol TEXT NOT NULL DEFAULT '',side TEXT NOT NULL DEFAULT '',
 code TEXT NOT NULL,detail TEXT NOT NULL DEFAULT '',first_seen DATETIME NOT NULL,last_seen DATETIME NOT NULL,
 resolved_at DATETIME,PRIMARY KEY(trader_id,area,resource_id));
 CREATE INDEX IF NOT EXISTS idx_copy_runtime_issue_active ON copy_trade_runtime_issues(trader_id,resolved_at);
 CREATE TABLE IF NOT EXISTS copy_trade_runtime_source (
 trader_id TEXT PRIMARY KEY,last_success_at DATETIME,last_failure_at DATETIME,last_error TEXT NOT NULL DEFAULT '');`)
	return err
}

// Returns true only when a new incident or a changed reason should be logged.
// Identical retries update the heartbeat at most once a minute.
func (s *CopyTradeStore) RecordRuntimeIssue(in CopyRuntimeIssue) (bool, error) {
	if in.TraderID == "" || in.Area == "" || in.ResourceID == "" || in.Code == "" {
		return false, fmt.Errorf("invalid runtime issue identity")
	}
	now := time.Now().UTC()
	var changed bool
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var oldCode string
	var resolved *string
	err = tx.QueryRow(`SELECT code,resolved_at FROM copy_trade_runtime_issues WHERE trader_id=? AND area=? AND resource_id=?`, in.TraderID, in.Area, in.ResourceID).Scan(&oldCode, &resolved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	changed = err != nil || oldCode != in.Code || resolved != nil
	_, err = tx.Exec(`INSERT INTO copy_trade_runtime_issues(trader_id,area,resource_id,leader_pos_id,symbol,side,code,detail,first_seen,last_seen)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(trader_id,area,resource_id) DO UPDATE SET
 leader_pos_id=excluded.leader_pos_id,symbol=excluded.symbol,side=excluded.side,code=excluded.code,detail=excluded.detail,
 first_seen=CASE WHEN copy_trade_runtime_issues.code<>excluded.code OR copy_trade_runtime_issues.resolved_at IS NOT NULL THEN excluded.first_seen ELSE copy_trade_runtime_issues.first_seen END,
 last_seen=excluded.last_seen,resolved_at=NULL
 WHERE copy_trade_runtime_issues.code<>excluded.code OR copy_trade_runtime_issues.resolved_at IS NOT NULL OR copy_trade_runtime_issues.last_seen<?`,
		in.TraderID, in.Area, in.ResourceID, in.LeaderPosID, in.Symbol, strings.ToLower(in.Side), in.Code, in.Detail, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Add(-time.Minute).Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	return changed, tx.Commit()
}

func (s *CopyTradeStore) ResolveRuntimeIssue(traderID, area, resourceID string) error {
	_, err := s.db.Exec(`UPDATE copy_trade_runtime_issues SET resolved_at=? WHERE trader_id=? AND area=? AND resource_id=? AND resolved_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), traderID, area, resourceID)
	return err
}

func (s *CopyTradeStore) ListRuntimeIssues(traderID string) ([]CopyRuntimeIssue, error) {
	rows, err := s.db.Query(`SELECT trader_id,area,resource_id,leader_pos_id,symbol,side,code,detail,first_seen,last_seen FROM copy_trade_runtime_issues WHERE trader_id=? AND resolved_at IS NULL ORDER BY first_seen,area,resource_id`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []CopyRuntimeIssue{}
	for rows.Next() {
		var issue CopyRuntimeIssue
		var first, last string
		if err = rows.Scan(&issue.TraderID, &issue.Area, &issue.ResourceID, &issue.LeaderPosID, &issue.Symbol, &issue.Side, &issue.Code, &issue.Detail, &first, &last); err != nil {
			return nil, err
		}
		if issue.FirstSeen, err = parseDBTime(first); err != nil {
			return nil, err
		}
		if issue.LastSeen, err = parseDBTime(last); err != nil {
			return nil, err
		}
		result = append(result, issue)
	}
	return result, rows.Err()
}

// The source heartbeat is intentionally independent of Binance's provider-
// specific health state. All source adapters can report full snapshot outcomes.
func (s *CopyTradeStore) ObserveRuntimeSource(traderID string, observedAt time.Time, cause error) error {
	if traderID == "" || observedAt.IsZero() {
		return fmt.Errorf("invalid source observation")
	}
	at := observedAt.UTC().Format(time.RFC3339Nano)
	if cause != nil {
		_, err := s.db.Exec(`INSERT INTO copy_trade_runtime_source(trader_id,last_failure_at,last_error) VALUES(?,?,?) ON CONFLICT(trader_id) DO UPDATE SET last_failure_at=excluded.last_failure_at,last_error=excluded.last_error WHERE (last_failure_at IS NULL OR last_failure_at<excluded.last_failure_at) AND (last_success_at IS NULL OR last_success_at<=excluded.last_failure_at)`, traderID, at, cause.Error())
		return err
	}
	_, err := s.db.Exec(`INSERT INTO copy_trade_runtime_source(trader_id,last_success_at) VALUES(?,?) ON CONFLICT(trader_id) DO UPDATE SET last_success_at=excluded.last_success_at,last_error=CASE WHEN last_failure_at IS NULL OR last_failure_at<=excluded.last_success_at THEN '' ELSE last_error END WHERE last_success_at IS NULL OR last_success_at<? OR (last_error<>'' AND (last_failure_at IS NULL OR last_failure_at<=excluded.last_success_at))`, traderID, at, observedAt.Add(-10*time.Second).UTC().Format(time.RFC3339Nano))
	return err
}

func (s *CopyTradeStore) GetRuntimeSource(traderID string) (*CopyRuntimeSource, error) {
	var success, failure *string
	out := &CopyRuntimeSource{}
	if err := s.db.QueryRow(`SELECT last_success_at,last_failure_at,last_error FROM copy_trade_runtime_source WHERE trader_id=?`, traderID).Scan(&success, &failure, &out.LastError); err != nil {
		return nil, err
	}
	if success != nil {
		t, err := parseDBTime(*success)
		if err != nil {
			return nil, err
		}
		out.LastSuccessAt = &t
	}
	if failure != nil {
		t, err := parseDBTime(*failure)
		if err != nil {
			return nil, err
		}
		out.LastFailureAt = &t
	}
	return out, nil
}

// Multiple live cycle anchors for one actual position are ambiguous. Preserve
// hosted orders and evidence; callers must not let competing retry jobs amend
// each other's protection. Released/ended positions are not competing owners.
func (s *CopyTradeStore) ProtectionScopeConflicts(cycleID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT c.id FROM copy_guard_cycles c JOIN copy_guard_cycles target ON target.id=?
 WHERE c.trader_id=target.trader_id AND c.id<>target.id AND c.closed_at IS NULL
 AND c.symbol=target.symbol AND c.side=target.side
 AND (c.margin_mode=target.margin_mode OR c.margin_mode='' OR target.margin_mode='')
 AND NOT EXISTS(SELECT 1 FROM copy_trade_position_custody p WHERE p.trader_id=c.trader_id AND p.leader_pos_id=c.leader_pos_id AND (p.state='RELEASED' OR p.cycle_id<>c.id OR p.attempt_no<>c.reentry_count))
 AND NOT EXISTS(SELECT 1 FROM copy_guard_events e WHERE e.cycle_id=c.id AND e.type='FOLLOWER_POSITION_ABSENT') ORDER BY c.id`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}
