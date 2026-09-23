package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
)

// FollowGroup coordinates source participation, not ownership of exchange
// quantities. Protective cycles retain their immutable prices and identities.
type FollowGroup struct {
	ID               int64  `json:"id"`
	TraderID         string `json:"trader_id"`
	AccountID        string `json:"account_id"`
	Provider         string `json:"provider"`
	LeaderID         string `json:"leader_id"`
	Symbol           string `json:"symbol"`
	Side             string `json:"side"`
	Paused           bool   `json:"paused"`
	RiskBlocked      bool   `json:"risk_blocked"`
	Ignored          bool   `json:"ignored"`
	SourceEnded      bool   `json:"source_ended"`
	Version          int64  `json:"version"`
	ConflictReason   string `json:"conflict_reason,omitempty"`
	EntryBlockReason string `json:"entry_block_reason,omitempty"`
}

// InitFollowGroupTables is additive; no historical order or stop is rewritten.
func (s *CopyTradeStore) InitFollowGroupTables() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_follow_groups (
 id INTEGER PRIMARY KEY AUTOINCREMENT,trader_id TEXT NOT NULL,account_id TEXT NOT NULL,
 provider TEXT NOT NULL,leader_id TEXT NOT NULL,symbol TEXT NOT NULL,side TEXT NOT NULL,
 paused INTEGER NOT NULL DEFAULT 0,risk_blocked INTEGER NOT NULL DEFAULT 0,
 ignored INTEGER NOT NULL DEFAULT 0,source_ended INTEGER NOT NULL DEFAULT 0,
 version INTEGER NOT NULL DEFAULT 0,conflict_reason TEXT NOT NULL DEFAULT '',entry_block_reason TEXT NOT NULL DEFAULT '',created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
 CREATE UNIQUE INDEX IF NOT EXISTS idx_follow_group_active ON copy_trade_follow_groups(trader_id,symbol,side) WHERE source_ended=0;
 CREATE TABLE IF NOT EXISTS copy_trade_follow_group_members (
 group_id INTEGER NOT NULL,leader_pos_id TEXT NOT NULL,opened_ms INTEGER NOT NULL DEFAULT 0,
 observed_size REAL NOT NULL DEFAULT 0,source_closed INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(group_id,leader_pos_id));
 CREATE TABLE IF NOT EXISTS copy_trade_follow_group_guards (
 cycle_id INTEGER PRIMARY KEY,group_id INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS copy_trade_follow_group_intents (
 intent_id INTEGER PRIMARY KEY,group_id INTEGER NOT NULL,control_version INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS copy_trade_follow_group_exits (
 intent_id INTEGER PRIMARY KEY,group_id INTEGER NOT NULL,full_group_exit INTEGER NOT NULL,
 batch_json TEXT NOT NULL,completed INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS copy_trade_follow_group_member_acks (
 intent_id INTEGER NOT NULL,leader_pos_id TEXT NOT NULL,source_revision INTEGER NOT NULL,target_size REAL NOT NULL,
 PRIMARY KEY(intent_id,leader_pos_id));
 CREATE TABLE IF NOT EXISTS copy_trade_confirmed_source_lifecycles (
 trader_id TEXT NOT NULL,leader_pos_id TEXT NOT NULL,opened_ms INTEGER NOT NULL,intent_id INTEGER NOT NULL,
 PRIMARY KEY(trader_id,leader_pos_id));`)
	if err != nil {
		return err
	}
	for _, name := range []string{"conflict_reason", "entry_block_reason"} {
		if err = ensureSQLiteColumn(s.db, "copy_trade_follow_groups", name, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	return nil
}

func (s *CopyTradeStore) BlockFollowGroupEntry(groupID int64, reason string) error {
	_, err := s.db.Exec(`UPDATE copy_trade_follow_groups SET entry_block_reason=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND NOT source_ended`, reason, groupID)
	return err
}

// Unlike the legacy lifecycle table this evidence is written only while a new
// source open is reserved, never by observing today's cTime beside an old row.
func (s *CopyTradeStore) ConfirmSourceLifecycleForIntent(intentID, openedMS int64) error {
	if openedMS <= 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO copy_trade_confirmed_source_lifecycles(trader_id,leader_pos_id,opened_ms,intent_id)
 SELECT trader_id,leader_pos_id,?,id FROM copy_trade_execution_intents WHERE id=? AND source_kind='LEADER_TRANSITION' AND action IN('open_long','open_short')
 ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET opened_ms=excluded.opened_ms,intent_id=excluded.intent_id`, openedMS, intentID)
	return err
}

func (s *CopyTradeStore) ConfirmedSourceLifecycleOpenedMS(traderID, posID string) (int64, error) {
	var ms int64
	err := s.db.QueryRow(`SELECT opened_ms FROM copy_trade_confirmed_source_lifecycles WHERE trader_id=? AND leader_pos_id=?`, traderID, posID).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return ms, err
}

const followGroupColumns = `g.id,g.trader_id,g.account_id,g.provider,g.leader_id,g.symbol,g.side,g.paused,g.risk_blocked,g.ignored,g.source_ended,g.version,g.conflict_reason,g.entry_block_reason`

func scanFollowGroup(row interface{ Scan(...interface{}) error }) (*FollowGroup, error) {
	g := &FollowGroup{}
	err := row.Scan(&g.ID, &g.TraderID, &g.AccountID, &g.Provider, &g.LeaderID, &g.Symbol, &g.Side, &g.Paused, &g.RiskBlocked, &g.Ignored, &g.SourceEnded, &g.Version, &g.ConflictReason, &g.EntryBlockReason)
	return g, err
}

func (s *CopyTradeStore) GetFollowGroupForPosition(traderID, posID string) (*FollowGroup, error) {
	return scanFollowGroup(s.db.QueryRow(`SELECT `+followGroupColumns+` FROM copy_trade_follow_groups g JOIN copy_trade_follow_group_members m ON m.group_id=g.id WHERE g.trader_id=? AND m.leader_pos_id=? ORDER BY g.id DESC LIMIT 1`, traderID, posID))
}

func (s *CopyTradeStore) GetFollowGroupForCycle(cycleID int64) (*FollowGroup, error) {
	return scanFollowGroup(s.db.QueryRow(`SELECT `+followGroupColumns+` FROM copy_trade_follow_groups g JOIN copy_trade_follow_group_guards b ON b.group_id=g.id WHERE b.cycle_id=?`, cycleID))
}

func (s *CopyTradeStore) GetFollowGroup(id int64) (*FollowGroup, error) {
	return scanFollowGroup(s.db.QueryRow(`SELECT `+followGroupColumns+` FROM copy_trade_follow_groups g WHERE g.id=?`, id))
}

// EnsureFollowGroupMember attaches only a source identity. It cannot create a
// follower mapping, take custody, place protection, or clear a paused/risk gate.
func (s *CopyTradeStore) EnsureFollowGroupMember(traderID, provider, leaderID, symbol, side, posID string, openedMS int64, status string) (*FollowGroup, error) {
	if traderID == "" || symbol == "" || posID == "" || (side != "long" && side != "short") {
		return nil, fmt.Errorf("invalid follow group identity")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	accountID := ""
	err = tx.QueryRow(`SELECT exchange_id FROM traders WHERE id=?`, traderID).Scan(&accountID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// In-process legacy engines have no trader row. Production always has one.
	if accountID == "" {
		accountID = "trader:" + traderID
	}
	g, err := scanFollowGroup(tx.QueryRow(`SELECT `+followGroupColumns+` FROM copy_trade_follow_groups g WHERE trader_id=? AND symbol=? AND side=? AND source_ended=0`, traderID, symbol, side))
	if errors.Is(err, sql.ErrNoRows) {
		res, e := tx.Exec(`INSERT INTO copy_trade_follow_groups(trader_id,account_id,provider,leader_id,symbol,side,paused,risk_blocked,ignored) VALUES(?,?,?,?,?,?,?,?,?)`, traderID, accountID, provider, leaderID, symbol, side, status == MappingStatusManualStopped, status == MappingStatusStoppedByRisk, status == MappingStatusIgnored)
		if e != nil {
			return nil, e
		}
		id, e := res.LastInsertId()
		if e != nil {
			return nil, e
		}
		g = &FollowGroup{ID: id, TraderID: traderID, AccountID: accountID, Provider: provider, LeaderID: leaderID, Symbol: symbol, Side: side, Paused: status == MappingStatusManualStopped, RiskBlocked: status == MappingStatusStoppedByRisk, Ignored: status == MappingStatusIgnored}
	} else if err != nil {
		return nil, err
	}
	if g.AccountID != accountID || g.Provider != provider || g.LeaderID != leaderID {
		return nil, fmt.Errorf("follow group owner changed; original account/source requires reconciliation")
	}
	if _, err = tx.Exec(`INSERT INTO copy_trade_follow_group_members(group_id,leader_pos_id,opened_ms) VALUES(?,?,?) ON CONFLICT(group_id,leader_pos_id) DO NOTHING`, g.ID, posID, openedMS); err != nil {
		return nil, err
	}
	// Legacy per-leg participation may disagree. Do not silently promote an
	// ignored/manual leg, or suppress an existing participating one by row order.
	var ignored, paused, participating int
	if err = tx.QueryRow(`SELECT COALESCE(SUM(m.status='ignored'),0),COALESCE(SUM(m.status='manual_stopped'),0),COALESCE(SUM(m.status IN('active','detached','stopped_by_risk')),0) FROM copy_trade_position_mappings m JOIN copy_trade_follow_group_members gm ON gm.leader_pos_id=m.leader_pos_id WHERE m.trader_id=? AND gm.group_id=?`, traderID, g.ID).Scan(&ignored, &paused, &participating); err != nil {
		return nil, err
	}
	if (ignored > 0 && (paused > 0 || participating > 0)) || (paused > 0 && participating > 0) {
		g.ConflictReason = "GROUP_PARTICIPATION_CONFLICT"
		if _, err = tx.Exec(`UPDATE copy_trade_follow_groups SET conflict_reason=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, g.ConflictReason, g.ID); err != nil {
			return nil, err
		}
	}
	// A no-fill source finisher can establish the first ignored baseline
	// after this group was reserved. Persist group-wide participation once every
	// acknowledged member agrees; mixed states remain an explicit conflict.
	if ignored > 0 && paused == 0 && participating == 0 && !g.Ignored {
		g.Ignored = true
		g.Version++
		if _, err = tx.Exec(`UPDATE copy_trade_follow_groups SET ignored=1,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE id=? AND ignored=0`, g.ID); err != nil {
			return nil, err
		}
	}
	if paused > 0 && ignored == 0 && participating == 0 && !g.Paused {
		g.Paused = true
		g.Version++
		if _, err = tx.Exec(`UPDATE copy_trade_follow_groups SET paused=1,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE id=? AND paused=0`, g.ID); err != nil {
			return nil, err
		}
	}
	if status == MappingStatusStoppedByRisk && !g.RiskBlocked {
		g.RiskBlocked = true
		if _, err = tx.Exec(`UPDATE copy_trade_follow_groups SET risk_blocked=1,updated_at=CURRENT_TIMESTAMP WHERE id=?`, g.ID); err != nil {
			return nil, err
		}
	}
	// Preserve old cycle identity and frozen policy, including a source member
	// whose mapping ended while its shared actual position is still protected.
	if _, err = tx.Exec(`INSERT OR IGNORE INTO copy_trade_follow_group_guards(cycle_id,group_id) SELECT id,? FROM copy_guard_cycles WHERE trader_id=? AND leader_pos_id=? AND closed_at IS NULL`, g.ID, traderID, posID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return g, nil
}

func (s *CopyTradeStore) BindFollowGroupIntent(intentID, groupID int64) error {
	_, err := s.db.Exec(`INSERT INTO copy_trade_follow_group_intents(intent_id,group_id,control_version) SELECT ?,id,version FROM copy_trade_follow_groups WHERE id=? ON CONFLICT(intent_id) DO NOTHING`, intentID, groupID)
	return err
}

func checkFollowGroupSubmission(q interface {
	QueryRow(string, ...interface{}) *sql.Row
}, intentID int64) error {
	var allowed bool
	err := q.QueryRow(`SELECT NOT g.paused AND NOT g.ignored AND NOT g.source_ended AND g.conflict_reason='' AND gi.control_version=g.version
 AND (i.action IN ('close_long','close_short','reduce_long','reduce_short') OR (NOT g.risk_blocked AND g.entry_block_reason=''))
 FROM copy_trade_follow_group_intents gi JOIN copy_trade_follow_groups g ON g.id=gi.group_id JOIN copy_trade_execution_intents i ON i.id=gi.intent_id WHERE gi.intent_id=?`, intentID).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !allowed {
		return ErrFollowControlChanged
	}
	return nil
}

// MarkFollowGroupPausedTx is called by the existing manual-stop transaction.
// All members receive the same control invalidation, including already queued
// orders. RiskBlocked is intentionally untouched by pause and resume.
func MarkFollowGroupPausedTx(tx *sql.Tx, traderID, posID string) (bool, error) {
	var groupID int64
	err := tx.QueryRow(`SELECT g.id FROM copy_trade_follow_groups g JOIN copy_trade_follow_group_members m ON m.group_id=g.id WHERE g.trader_id=? AND m.leader_pos_id=? AND g.source_ended=0 ORDER BY g.id DESC LIMIT 1`, traderID, posID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := tx.Exec(`UPDATE copy_trade_follow_groups SET paused=1,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE id=? AND paused=0`, groupID)
	if err != nil {
		return true, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return true, err
	}
	if _, err = tx.Exec(`INSERT INTO copy_trade_follow_controls(trader_id,leader_pos_id,version) SELECT ?,leader_pos_id,1 FROM copy_trade_follow_group_members WHERE group_id=? ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET version=version+1,updated_at=CURRENT_TIMESTAMP`, traderID, groupID); err != nil {
		return true, err
	}
	_, err = tx.Exec(`UPDATE copy_trade_position_mappings SET status='manual_stopped',stopped_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id IN(SELECT leader_pos_id FROM copy_trade_follow_group_members WHERE group_id=?) AND status IN('active','detached','stopped_by_risk')`, traderID, groupID)
	return true, err
}

func (s *CopyTradeStore) BlockFollowGroupRiskByCycle(cycleID int64) error {
	if err := s.BindFollowGroupGuard(cycleID); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE copy_trade_follow_groups SET risk_blocked=1,updated_at=CURRENT_TIMESTAMP WHERE id IN(SELECT group_id FROM copy_trade_follow_group_guards WHERE cycle_id=?)`, cycleID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET status='stopped_by_risk',updated_at=CURRENT_TIMESTAMP WHERE status='active' AND (trader_id,leader_pos_id) IN(SELECT g.trader_id,m.leader_pos_id FROM copy_trade_follow_group_guards b JOIN copy_trade_follow_groups g ON g.id=b.group_id JOIN copy_trade_follow_group_members m ON m.group_id=g.id WHERE b.cycle_id=?)`, cycleID); err != nil {
		return err
	}
	return tx.Commit()
}

type FollowGroupObservation struct {
	GroupID     int64
	LeaderPosID string
	MarginMode  string
	Size        float64
}

// PublishFollowGroupSnapshot replaces observations only when the entire source
// image can be installed. A failed member/baseline write rolls back the reset,
// and cannot manufacture a source-flat boundary for paused or ignored groups.
func (s *CopyTradeStore) PublishFollowGroupSnapshot(traderID string, observations []FollowGroupObservation) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE copy_trade_follow_group_members SET observed_size=0 WHERE group_id IN(SELECT id FROM copy_trade_follow_groups WHERE trader_id=? AND NOT source_ended)`, traderID); err != nil {
		return err
	}
	for _, o := range observations {
		if o.Size <= 0 || math.IsNaN(o.Size) || math.IsInf(o.Size, 0) {
			return fmt.Errorf("invalid complete source observation")
		}
		var valid bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM copy_trade_follow_group_members m JOIN copy_trade_follow_groups g ON g.id=m.group_id WHERE g.id=? AND m.leader_pos_id=? AND g.trader_id=? AND NOT g.source_ended)`, o.GroupID, o.LeaderPosID, traderID).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("source observation membership changed")
		}
		if err = observeFollowGroupMemberTx(tx, o); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE copy_trade_follow_groups SET source_ended=1,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND NOT source_ended AND (paused OR ignored OR (entry_block_reason<>'' AND NOT EXISTS(SELECT 1 FROM copy_trade_follow_group_members gm JOIN copy_trade_position_mappings pm ON pm.leader_pos_id=gm.leader_pos_id AND pm.trader_id=copy_trade_follow_groups.trader_id WHERE gm.group_id=copy_trade_follow_groups.id AND pm.status IN('active','detached','stopped_by_risk') AND pm.last_known_size>0))) AND NOT EXISTS(SELECT 1 FROM copy_trade_follow_group_members m WHERE m.group_id=copy_trade_follow_groups.id AND observed_size>0)`, traderID); err != nil {
		return err
	}
	// The boundary incident is resolved by a complete source-flat image, not
	// by a timer or an operator restart. Historical diagnostics stay recorded.
	if _, err = tx.Exec(`UPDATE copy_trade_runtime_issues SET resolved_at=CURRENT_TIMESTAMP WHERE trader_id=? AND area='source' AND resolved_at IS NULL AND (resource_id IN(SELECT 'group_boundary:'||id FROM copy_trade_follow_groups WHERE trader_id=? AND source_ended) OR resource_id IN(SELECT 'group:'||id FROM copy_trade_follow_groups WHERE trader_id=? AND source_ended))`, traderID, traderID, traderID); err != nil {
		return err
	}
	return tx.Commit()
}

func observeFollowGroupMemberTx(tx *sql.Tx, o FollowGroupObservation) error {
	if _, err := tx.Exec(`UPDATE copy_trade_follow_group_members SET observed_size=? WHERE group_id=? AND leader_pos_id=?`, o.Size, o.GroupID, o.LeaderPosID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO copy_trade_position_mappings(trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,last_known_size,source_revision,opened_at,updated_at)
 SELECT trader_id,leader_id,?,symbol,side,?,CASE WHEN paused THEN 'manual_stopped' WHEN ignored THEN 'ignored' ELSE 'stopped_by_risk' END,?,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP FROM copy_trade_follow_groups
 WHERE id=? AND (paused OR ignored OR risk_blocked) AND ?>0
 ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET status=excluded.status,last_known_size=excluded.last_known_size,source_revision=source_revision+1,closed_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE copy_trade_position_mappings.status='closed'`, o.LeaderPosID, o.MarginMode, o.Size, o.GroupID, o.Size)
	return err
}

// ObserveFollowGroupMember is a single-member helper for explicit membership
// control and tests. Runtime polling publishes a full snapshot transactionally.
func (s *CopyTradeStore) ObserveFollowGroupMember(groupID int64, posID, marginMode string, size float64) error {
	if size < 0 || math.IsNaN(size) || math.IsInf(size, 0) {
		return fmt.Errorf("invalid source observation")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = observeFollowGroupMemberTx(tx, FollowGroupObservation{groupID, posID, marginMode, size}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CopyTradeStore) EndInactiveFollowGroups(traderID string) error {
	_, err := s.db.Exec(`UPDATE copy_trade_follow_groups SET source_ended=1,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND NOT source_ended AND (paused OR ignored OR (entry_block_reason<>'' AND NOT EXISTS(SELECT 1 FROM copy_trade_follow_group_members gm JOIN copy_trade_position_mappings pm ON pm.leader_pos_id=gm.leader_pos_id AND pm.trader_id=copy_trade_follow_groups.trader_id WHERE gm.group_id=copy_trade_follow_groups.id AND pm.status IN('active','detached','stopped_by_risk') AND pm.last_known_size>0))) AND NOT EXISTS(SELECT 1 FROM copy_trade_follow_group_members m WHERE m.group_id=copy_trade_follow_groups.id AND observed_size>0)`, traderID)
	return err
}

func (s *CopyTradeStore) ResetFollowGroupObservations(traderID string) error {
	_, err := s.db.Exec(`UPDATE copy_trade_follow_group_members SET observed_size=0 WHERE group_id IN(SELECT id FROM copy_trade_follow_groups WHERE trader_id=? AND NOT source_ended)`, traderID)
	return err
}

func bindFollowGroupGuardTx(tx *sql.Tx, cycleID int64) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO copy_trade_follow_group_guards(cycle_id,group_id)
 SELECT c.id,g.id FROM copy_guard_cycles c JOIN copy_trade_follow_group_members m ON m.leader_pos_id=c.leader_pos_id JOIN copy_trade_follow_groups g ON g.id=m.group_id AND g.trader_id=c.trader_id
 WHERE c.id=? AND NOT g.source_ended ORDER BY g.id DESC LIMIT 1`, cycleID)
	return err
}

func (s *CopyTradeStore) BindFollowGroupGuard(cycleID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = bindFollowGroupGuardTx(tx, cycleID); err != nil {
		return err
	}
	return tx.Commit()
}

func followGroupProtectionCycleIDTx(q interface {
	QueryRow(string, ...interface{}) *sql.Row
}, traderID, posID, marginMode string) (int64, error) {
	var count int
	var id int64
	err := q.QueryRow(`SELECT COUNT(DISTINCT c.id),COALESCE(MIN(c.id),0) FROM copy_trade_follow_groups g JOIN copy_trade_follow_group_members m ON m.group_id=g.id JOIN copy_trade_follow_group_guards b ON b.group_id=g.id JOIN copy_guard_cycles c ON c.id=b.cycle_id
 WHERE g.trader_id=? AND m.leader_pos_id=? AND NOT g.source_ended AND c.closed_at IS NULL AND c.margin_mode=?`, traderID, posID, marginMode).Scan(&count, &id)
	if err != nil {
		return 0, err
	}
	if count > 1 {
		return 0, fmt.Errorf("multiple protective cycles cover one follow group margin scope")
	}
	return id, nil
}

func (s *CopyTradeStore) GetFollowGroupProtectionCycle(traderID, posID, marginMode string) (*CopyGuardCycle, error) {
	id, err := followGroupProtectionCycleIDTx(s.db, traderID, posID, marginMode)
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, sql.ErrNoRows
	}
	return s.GetCopyGuardCycle(id)
}

func (s *CopyTradeStore) IgnoreFollowGroup(traderID, posID string) error {
	_, err := s.db.Exec(`UPDATE copy_trade_follow_groups SET ignored=1,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE source_ended=0 AND trader_id=? AND id IN(SELECT group_id FROM copy_trade_follow_group_members WHERE leader_pos_id=?)`, traderID, posID)
	return err
}

func (s *CopyTradeStore) FollowGroupMemberIDs(groupID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT leader_pos_id FROM copy_trade_follow_group_members WHERE group_id=? ORDER BY leader_pos_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *CopyTradeStore) FollowGroupGuardCycles(groupID int64) ([]*CopyGuardCycle, error) {
	rows, err := s.db.Query(`SELECT cycle_id FROM copy_trade_follow_group_guards WHERE group_id=? ORDER BY cycle_id`, groupID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []*CopyGuardCycle
	for _, id := range ids {
		c, e := s.GetCopyGuardCycle(id)
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, nil
}

// FollowGroupKeepsProtection distinguishes a source-member close from the end
// of the protected follower position. Paused groups retain protection even
// after their source ended; actual continuity remains the authority.
func (s *CopyTradeStore) FollowGroupKeepsProtection(cycleID int64) (bool, error) {
	var keep bool
	err := s.db.QueryRow(`SELECT (NOT g.source_ended OR g.paused) FROM copy_trade_follow_group_guards b JOIN copy_trade_follow_groups g ON g.id=b.group_id WHERE b.cycle_id=?`, cycleID).Scan(&keep)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return keep, err
}

func (s *CopyTradeStore) FollowGroupHasManagedPeer(traderID, posID, symbol, side string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_follow_group_members mine JOIN copy_trade_follow_groups g ON g.id=mine.group_id
 JOIN copy_trade_follow_group_members peer ON peer.group_id=g.id JOIN copy_trade_position_custody p ON p.trader_id=g.trader_id AND p.leader_pos_id=peer.leader_pos_id
 WHERE g.trader_id=? AND mine.leader_pos_id=? AND g.symbol=? AND g.side=? AND NOT g.source_ended AND NOT g.paused AND NOT g.ignored AND NOT g.risk_blocked AND p.state='MANAGED' AND peer.leader_pos_id<>?`, traderID, posID, symbol, strings.ToLower(side), posID).Scan(&n)
	return n > 0, err
}
