package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
)

// Leader exits own the current account position, not a Copy Guard fill budget.
// Targets are frozen before submitting a partial reduction and survive restart.
type LeaderExitTarget struct {
	Key        string  `json:"key"`
	MarginMode string  `json:"margin_mode"`
	PositionID string  `json:"position_id"`
	Quantity   float64 `json:"quantity"`
}

type LeaderExitPlan struct {
	CycleID      int64              `json:"cycle_id,omitempty"`
	Cancelled    bool               `json:"cancelled,omitempty"`
	IntentID     int64              `json:"intent_id"`
	TraderID     string             `json:"trader_id"`
	LeaderPosID  string             `json:"leader_pos_id"`
	Symbol       string             `json:"symbol"`
	Side         string             `json:"side"`
	Ratio        float64            `json:"ratio"`
	SourceClosed bool               `json:"source_closed"`
	Targets      []LeaderExitTarget `json:"targets"`
	Completed    bool               `json:"completed"`
}

func (s *CopyTradeStore) initLeaderExitTables() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_execution_timings (intent_id INTEGER PRIMARY KEY,source_snapshot_ms INTEGER NOT NULL DEFAULT 0,signal_observed_ms INTEGER NOT NULL DEFAULT 0,first_submitted_ms INTEGER NOT NULL DEFAULT 0,exchange_filled_ms INTEGER NOT NULL DEFAULT 0);
	 CREATE TABLE IF NOT EXISTS copy_trade_leader_exits (
	 intent_id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL, leader_pos_id TEXT NOT NULL,
	 plan_json TEXT NOT NULL, completed INTEGER NOT NULL DEFAULT 0,
	 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, completed_at DATETIME);
	 CREATE TABLE IF NOT EXISTS copy_trade_leader_exit_requests (
	 intent_id INTEGER PRIMARY KEY,scope TEXT NOT NULL,ratio REAL NOT NULL);
	 CREATE TABLE IF NOT EXISTS copy_trade_source_lifecycles (
	 trader_id TEXT NOT NULL, leader_pos_id TEXT NOT NULL, opened_ms INTEGER NOT NULL DEFAULT 0,
	 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	 PRIMARY KEY(trader_id,leader_pos_id));
	 CREATE TABLE IF NOT EXISTS copy_trade_exit_rollouts (trader_id TEXT PRIMARY KEY,completed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
	 CREATE TABLE IF NOT EXISTS copy_trade_released_repairs (
	 intent_id INTEGER PRIMARY KEY,trader_id TEXT NOT NULL,leader_pos_id TEXT NOT NULL,
	 baseline_pending INTEGER NOT NULL DEFAULT 1,detail TEXT NOT NULL,
	 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);`)
	return err
}

func (s *CopyTradeStore) LeaderExitRequest(intentID int64) (string, float64, error) {
	var scope string
	var ratio float64
	err := s.db.QueryRow(`SELECT scope,ratio FROM copy_trade_leader_exit_requests WHERE intent_id=?`, intentID).Scan(&scope, &ratio)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return scope, ratio, err
}

func (s *CopyTradeStore) GetLeaderExitPlan(intentID int64) (*LeaderExitPlan, error) {
	var raw string
	var done bool
	if err := s.db.QueryRow(`SELECT plan_json,completed FROM copy_trade_leader_exits WHERE intent_id=?`, intentID).Scan(&raw, &done); err != nil {
		return nil, err
	}
	var plan LeaderExitPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return nil, err
	}
	plan.Completed = done
	return &plan, nil
}

func (s *CopyTradeStore) ExtendFullLeaderExit(plan LeaderExitPlan) error {
	if plan.Ratio != 1 {
		return fmt.Errorf("cannot expand partial leader exit")
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE copy_trade_leader_exits SET plan_json=? WHERE intent_id=? AND completed=0`, string(raw), plan.IntentID)
	return err
}

func (s *CopyTradeStore) PrepareLeaderExit(plan LeaderExitPlan) (*LeaderExitPlan, error) {
	if plan.IntentID <= 0 || plan.TraderID == "" || plan.LeaderPosID == "" || plan.Symbol == "" || (plan.Side != "long" && plan.Side != "short") || plan.Ratio <= 0 || plan.Ratio > 1 || math.IsNaN(plan.Ratio) {
		return nil, fmt.Errorf("invalid leader exit plan")
	}
	for _, target := range plan.Targets {
		if target.Key == "" || (target.MarginMode != "cross" && target.MarginMode != "isolated") || target.Quantity <= 0 || math.IsNaN(target.Quantity) || math.IsInf(target.Quantity, 0) {
			return nil, fmt.Errorf("invalid leader exit target")
		}
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = checkFollowSubmission(tx, plan.IntentID); err != nil {
		return nil, err
	}
	_, err = tx.Exec(`INSERT INTO copy_trade_leader_exits(intent_id,trader_id,leader_pos_id,plan_json) VALUES(?,?,?,?) ON CONFLICT(intent_id) DO NOTHING`, plan.IntentID, plan.TraderID, plan.LeaderPosID, string(raw))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetLeaderExitPlan(plan.IntentID)
}

// CompleteLeaderExit commits source progress even when no follower order was
// necessary. The caller must first prove all target orders terminal and the
// requested reduction/flat state from fresh exchange positions.
func (s *CopyTradeStore) CompleteLeaderExit(intentID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var traderID, posID, action, status string
	var revision int64
	var target float64
	if err = tx.QueryRow(`SELECT trader_id,leader_pos_id,action,status,source_revision,leader_target_size FROM copy_trade_execution_intents WHERE id=? AND source_kind='LEADER_TRANSITION'`, intentID).Scan(&traderID, &posID, &action, &status, &revision, &target); err != nil {
		return err
	}
	var raw string
	var done bool
	if err = tx.QueryRow(`SELECT plan_json,completed FROM copy_trade_leader_exits WHERE intent_id=?`, intentID).Scan(&raw, &done); err != nil {
		return err
	}
	if done {
		return tx.Commit()
	}
	var plan LeaderExitPlan
	if err = json.Unmarshal([]byte(raw), &plan); err != nil {
		return err
	}
	var pending int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts WHERE intent_id=? AND submitted_at IS NOT NULL AND terminal_at IS NULL`, intentID).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("leader exit has unsettled orders")
	}
	var currentRevision int64
	var mappingStatus string
	if err = tx.QueryRow(`SELECT source_revision,status FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, posID).Scan(&currentRevision, &mappingStatus); err != nil {
		return err
	}
	if currentRevision != revision-1 && currentRevision != revision {
		return fmt.Errorf("leader exit source revision changed: %d/%d", currentRevision, revision)
	}
	// A pause can race a submitted close. Acknowledge its real result without
	// resuming following or reviving a source lifecycle.
	next := mappingStatus
	if plan.SourceClosed && mappingStatus != MappingStatusManualStopped {
		next = MappingStatusClosed
	}
	if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET source_revision=?,last_known_size=?,status=?,closed_at=CASE WHEN ?='closed' THEN CURRENT_TIMESTAMP ELSE closed_at END,reduce_count=reduce_count+CASE WHEN ? OR NOT EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts WHERE intent_id=? AND filled_quantity>0) THEN 0 ELSE 1 END,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=?`, revision, target, next, next, plan.SourceClosed, intentID, traderID, posID); err != nil {
		return err
	}
	if plan.SourceClosed {
		if _, err = tx.Exec(`UPDATE copy_trade_position_custody SET state='RELEASED',reason='LEADER_CLOSE',released_at=COALESCE(released_at,CURRENT_TIMESTAMP) WHERE trader_id=? AND leader_pos_id=?`, traderID, posID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE copy_trade_execution_intents SET status='RECONCILING',reason_code='LEADER_EXIT_CONFIRMED',filled_quantity=(SELECT COALESCE(SUM(filled_quantity),0) FROM copy_trade_execution_order_attempts WHERE intent_id=?),terminal_at=CURRENT_TIMESTAMP,last_error='',updated_at=CURRENT_TIMESTAMP WHERE id=?`, intentID, intentID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='FILLED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, intentID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_leader_exits SET completed=1,completed_at=CURRENT_TIMESTAMP WHERE intent_id=?`, intentID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListExitFollowingMappings keeps a released follower lifecycle subscribed to
// source reductions. Explicit pause and ignored-at-entry remain exceptions.
func (s *CopyTradeStore) ListExitFollowingMappings(traderID string) ([]*CopyTradePositionMapping, error) {
	var result []*CopyTradePositionMapping
	for _, status := range []string{MappingStatusActive, MappingStatusStoppedByRisk, MappingStatusDetached} {
		rows, err := s.listMappings(traderID, status, 0)
		if err != nil {
			return nil, err
		}
		result = append(result, rows...)
	}
	return result, nil
}

func (s *CopyTradeStore) OtherSourceTransitionPending(traderID, symbol, side, posID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents WHERE trader_id=? AND symbol=? AND side=? AND leader_pos_id<>? AND source_kind='LEADER_TRANSITION' AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED')`, traderID, symbol, side, posID).Scan(&n)
	return n > 0, err
}

func (s *CopyTradeStore) SourceLifecycleOpenedMS(traderID, posID string) (int64, error) {
	var ms int64
	err := s.db.QueryRow(`SELECT opened_ms FROM copy_trade_source_lifecycles WHERE trader_id=? AND leader_pos_id=?`, traderID, posID).Scan(&ms)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return ms, err
}

func (s *CopyTradeStore) BindSourceLifecycle(traderID, posID string, openedMS int64) error {
	if openedMS <= 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO copy_trade_source_lifecycles(trader_id,leader_pos_id,opened_ms) VALUES(?,?,?) ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET opened_ms=excluded.opened_ms,updated_at=CURRENT_TIMESTAMP`, traderID, posID, openedMS)
	return err
}

// RepairReleasedCloseBlockers only acknowledges old, never-submitted, proven
// source-close skips. Its durable marker forces a no-chase fresh baseline
// before the engine may interpret today's reused position ID as a new entry.
func (s *CopyTradeStore) RepairReleasedCloseBlockers(traderID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT i.id,i.leader_pos_id,i.source_revision,COALESCE(p.cycle_id,0) FROM copy_trade_execution_intents i
	 JOIN copy_trade_position_mappings m ON m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id
	 JOIN copy_trade_position_custody p ON p.trader_id=i.trader_id AND p.leader_pos_id=i.leader_pos_id
	 WHERE i.trader_id=? AND i.source_kind='LEADER_TRANSITION' AND i.action IN ('close_long','close_short')
	 AND i.status='SKIPPED' AND i.reason_code='POSITION_CUSTODY_RELEASED' AND i.leader_target_size=0
	 AND i.submitted_at IS NULL AND i.filled_quantity=0 AND COALESCE(i.exchange_order_id,'')=''
	 AND m.status='active' AND m.source_revision=i.source_revision-1 AND p.state='RELEASED'
	 AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id AND (a.submitted_at IS NOT NULL OR a.filled_quantity>0 OR COALESCE(a.exchange_order_id,'')<>''))
	 AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_intents j JOIN copy_trade_execution_order_attempts a ON a.intent_id=j.id WHERE j.trader_id=i.trader_id AND j.leader_pos_id=i.leader_pos_id AND a.submitted_at IS NOT NULL AND a.terminal_at IS NULL)`, traderID)
	if err != nil {
		return 0, err
	}
	type repair struct {
		id, rev, cycleID int64
		pos              string
	}
	var repairs []repair
	for rows.Next() {
		var r repair
		if err = rows.Scan(&r.id, &r.pos, &r.rev, &r.cycleID); err != nil {
			rows.Close()
			return 0, err
		}
		repairs = append(repairs, r)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, r := range repairs {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO copy_trade_released_repairs(intent_id,trader_id,leader_pos_id,detail) VALUES(?,?,?,'released follower; source close was skipped without acknowledgement')`, r.id, traderID, r.pos); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET status='closed',last_known_size=0,source_revision=?,closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=?`, r.rev, traderID, r.pos); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='SKIPPED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, r.id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(`UPDATE copy_trade_execution_intents SET reason_code='RELEASED_CLOSE_REPAIRED',last_error='',updated_at=CURRENT_TIMESTAMP WHERE id=?`, r.id); err != nil {
			return 0, err
		}
		if r.cycleID > 0 {
			// Release of the original follower is already proven. Retire its
			// analytics lifecycle too, or a reused source id would bind the next
			// real entry to this old cycle and its immutable stop anchor. Exchange
			// protective orders stay visible until cancellation is actually verified.
			result, updateErr := tx.Exec(`UPDATE copy_guard_cycles SET status='DETACHED',closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),accounting_status='UNSCORABLE',accounting_error='RELEASED_CLOSE_REPAIRED',updated_at=CURRENT_TIMESTAMP WHERE id=? AND trader_id=? AND leader_pos_id=? AND closed_at IS NULL`, r.cycleID, traderID, r.pos)
			if updateErr != nil {
				return 0, updateErr
			}
			if n, _ := result.RowsAffected(); n > 0 {
				if err = terminalizeCopyGuardAuxiliaryStateTx(tx, r.cycleID, CopyGuardDetached); err != nil {
					return 0, err
				}
				if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,metadata_json) VALUES(?,?,'RELEASED_CLOSE_REPAIRED',?)`, r.cycleID, traderID, fmt.Sprintf(`{"intent_id":%d,"source_revision":%d,"exchange_order_sent":false}`, r.id, r.rev)); err != nil {
					return 0, err
				}
			}
		}
	}
	return len(repairs), tx.Commit()
}

// BaselineRepairedSource is called with one successfully decoded current
// snapshot. No exchange order is sent, including an obsolete historical close.
func (s *CopyTradeStore) BaselineRepairedSource(traderID string, current map[string]*CopyTradePositionMapping, opened map[string]int64, applyRollout bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT DISTINCT leader_pos_id FROM copy_trade_released_repairs WHERE trader_id=? AND baseline_pending=1`, traderID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var baselined int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_exit_rollouts WHERE trader_id=?`, traderID).Scan(&baselined); err != nil {
		return err
	}
	if baselined == 0 && applyRollout {
		for id, m := range current {
			if m.Symbol == "" || m.LeaderID == "" {
				continue
			}
			var pending int
			if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents WHERE trader_id=? AND leader_pos_id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED')`, traderID, id).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				continue
			}
			if _, err = tx.Exec(`INSERT INTO copy_trade_position_mappings(trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,last_known_size,source_revision,last_failure_reason,opened_at)
			 VALUES(?,?,?,?,?,?,'ignored',?,1,'ROLLOUT_NO_CHASE',CURRENT_TIMESTAMP)
			 ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET status='ignored',side=excluded.side,margin_mode=excluded.margin_mode,last_known_size=excluded.last_known_size,source_revision=source_revision+1,last_failure_reason='ROLLOUT_NO_CHASE',closed_at=NULL WHERE copy_trade_position_mappings.status='closed'`, traderID, m.LeaderID, id, m.Symbol, m.Side, m.MarginMode, m.LastKnownSize); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`INSERT INTO copy_trade_exit_rollouts(trader_id) VALUES(?)`, traderID); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if m := current[id]; m != nil {
			if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET status='ignored',side=?,margin_mode=?,last_known_size=?,source_revision=source_revision+1,last_failure_reason='ROLLOUT_NO_CHASE',closed_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='closed'`, m.Side, m.MarginMode, m.LastKnownSize, traderID, id); err != nil {
				return err
			}
			if _, err = tx.Exec(`INSERT INTO copy_trade_source_lifecycles(trader_id,leader_pos_id,opened_ms) VALUES(?,?,?) ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET opened_ms=excluded.opened_ms`, traderID, id, opened[id]); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`UPDATE copy_trade_released_repairs SET baseline_pending=0 WHERE trader_id=? AND leader_pos_id=?`, traderID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CancelLeaderExitAfterControlChange acknowledges fills already submitted at
// pause time without advancing or reviving the paused source mapping.
func (s *CopyTradeStore) CancelLeaderExitAfterControlChange(intentID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFollowSubmission(tx, intentID); err == nil {
		return fmt.Errorf("following control is still current")
	} else if err != ErrFollowControlChanged {
		return err
	}
	var n int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts WHERE intent_id=? AND submitted_at IS NOT NULL AND terminal_at IS NULL`, intentID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("paused exit still has unconfirmed orders")
	}
	if _, err = tx.Exec(`UPDATE copy_trade_execution_intents SET filled_quantity=(SELECT COALESCE(SUM(filled_quantity),0) FROM copy_trade_execution_order_attempts WHERE intent_id=?),status='SKIPPED',reason_code='FOLLOW_CONTROL_CHANGED',terminal_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, intentID, intentID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='SKIPPED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, intentID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_leader_exits SET completed=1,plan_json=json_set(plan_json,'$.cancelled',json('true')),completed_at=CURRENT_TIMESTAMP WHERE intent_id=?`, intentID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResumeLeaderExitSubmission permits another residual order only after every
// previous acknowledgement is terminal and the pause version is unchanged.
func (s *CopyTradeStore) ResumeLeaderExitSubmission(intentID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFollowSubmission(tx, intentID); err != nil {
		return err
	}
	var pending int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts WHERE intent_id=? AND submitted_at IS NOT NULL AND terminal_at IS NULL`, intentID).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("leader exit acknowledgement pending")
	}
	res, err := tx.Exec(`UPDATE copy_trade_execution_intents SET status='SUBMITTED',terminal_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED') AND EXISTS(SELECT 1 FROM copy_trade_leader_exits e WHERE e.intent_id=? AND e.completed=0)`, intentID, intentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("leader exit no longer actionable")
	}
	return tx.Commit()
}

// ObserveReleasedSourceIncrease consumes only a healthy same-lifecycle source
// increase. Released/risk-stopped followers must not buy back, but the next
// leader reduction still uses the leader's actual pre-reduction quantity.
func (s *CopyTradeStore) ObserveReleasedSourceIncrease(traderID, posID string, revision int64, oldSize, newSize float64) error {
	if newSize <= oldSize || math.IsNaN(newSize) || math.IsInf(newSize, 0) {
		return fmt.Errorf("invalid source increase")
	}
	_, err := s.db.Exec(`UPDATE copy_trade_position_mappings SET last_known_size=?,source_revision=source_revision+1,updated_at=CURRENT_TIMESTAMP
	 WHERE trader_id=? AND leader_pos_id=? AND source_revision=? AND last_known_size=? AND status IN ('detached','stopped_by_risk')
	 AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_intents i WHERE i.trader_id=? AND i.leader_pos_id=? AND i.status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED'))`, newSize, traderID, posID, revision, oldSize, traderID, posID)
	return err
}

// All already-submitted entries in the execution scope must become terminal
// before a full exit can be completed. A venue acknowledgement alone is not
// enough; a late entry fill would otherwise survive the source full close.
func (s *CopyTradeStore) HasUnsettledScopeEntries(traderID, symbol, side string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents i JOIN copy_trade_execution_order_attempts a ON a.intent_id=i.id WHERE i.trader_id=? AND i.symbol=? AND i.side=? AND i.action IN ('open_long','open_short') AND a.submitted_at IS NOT NULL AND a.terminal_at IS NULL`, traderID, symbol, side).Scan(&n)
	return n > 0, err
}
