package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Pending authorization is not position ownership. Only the ordinary confirmed
// fill transaction may replace it with an active mapping and acquire custody.
const MappingStatusCopyPending = "copy_pending"

type CurrentPositionCopy struct {
	RequestID  string                    `json:"request_id"`
	Status     string                    `json:"status"`
	Reason     string                    `json:"reason"`
	CreatedAt  string                    `json:"created_at"`
	SnapshotAt string                    `json:"snapshot_at"`
	Tasks      []CurrentPositionCopyTask `json:"tasks"`
}
type CurrentPositionCopyTask struct {
	SnapshotAt     string  `json:"-"`
	ID             int64   `json:"id"`
	LeaderPosID    string  `json:"leader_pos_id"`
	Symbol         string  `json:"symbol"`
	Side           string  `json:"side"`
	MarginMode     string  `json:"margin_mode"`
	OpenedMS       int64   `json:"opened_ms"`
	Size           float64 `json:"source_size"`
	EntryPrice     float64 `json:"leader_entry_price"`
	ReferencePrice float64 `json:"reference_price"`
	Notional       float64 `json:"estimated_notional"`
	Leverage       int     `json:"leverage"`
	Status         string  `json:"status"`
	Reason         string  `json:"reason"`
	IntentID       int64   `json:"intent_id"`
	FilledQuantity float64 `json:"filled_quantity"`
}

func (s *CopyTradeStore) initCurrentPositionCopyTables() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_current_position_requests (
 request_id TEXT PRIMARY KEY,trader_id TEXT NOT NULL,provider TEXT NOT NULL,leader_id TEXT NOT NULL,
 source_generation INTEGER NOT NULL,exchange_id TEXT NOT NULL,saved_generation INTEGER NOT NULL,
 start_generation INTEGER NOT NULL DEFAULT 0,status TEXT NOT NULL DEFAULT 'PENDING',reason TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,snapshot_at TEXT NOT NULL DEFAULT '');
 CREATE UNIQUE INDEX IF NOT EXISTS idx_current_copy_pending ON copy_trade_current_position_requests(trader_id) WHERE status IN ('PENDING','SEALED');
 CREATE TABLE IF NOT EXISTS copy_trade_current_position_tasks (
 id INTEGER PRIMARY KEY AUTOINCREMENT,request_id TEXT NOT NULL,trader_id TEXT NOT NULL,leader_pos_id TEXT NOT NULL,
 symbol TEXT NOT NULL,side TEXT NOT NULL,margin_mode TEXT NOT NULL,opened_ms INTEGER NOT NULL,source_size REAL NOT NULL,
 entry_price REAL NOT NULL,reference_price REAL NOT NULL,notional REAL NOT NULL,leverage INTEGER NOT NULL,
 status TEXT NOT NULL,reason TEXT NOT NULL DEFAULT '',intent_id INTEGER NOT NULL DEFAULT 0,
 UNIQUE(request_id,leader_pos_id));
 CREATE INDEX IF NOT EXISTS idx_current_copy_intent ON copy_trade_current_position_tasks(intent_id);
 CREATE INDEX IF NOT EXISTS idx_current_copy_source ON copy_trade_current_position_tasks(trader_id,leader_pos_id);`)
	return err
}

// Registration and configuration commit together. An omitted flag from an old
// client preserves pending authorization, while a scope change invalidates it.
func saveCurrentPositionCopyTx(tx *sql.Tx, c *CopyTradeConfig) error {
	var account, status string
	var generation int64
	err := tx.QueryRow(`SELECT exchange_id,lifecycle_status,lifecycle_generation FROM traders WHERE id=?`, c.TraderID).Scan(&account, &status, &generation)
	if err == sql.ErrNoRows && c.CopyCurrentPositionsOnce == nil {
		return nil
	} // standalone legacy config
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_current_position_requests SET status='CANCELLED',reason='CONFIGURATION_CHANGED'
 WHERE trader_id=? AND status='PENDING' AND (provider<>? OR leader_id<>? OR source_generation<>? OR exchange_id<>?)`, c.TraderID, c.ProviderType, c.LeaderID, c.SourceGeneration, account); err != nil {
		return err
	}
	if c.CopyCurrentPositionsOnce == nil {
		return nil
	}
	if !*c.CopyCurrentPositionsOnce {
		_, err = tx.Exec(`UPDATE copy_trade_current_position_requests SET status='CANCELLED',reason='OPERATOR_CANCELLED' WHERE trader_id=? AND status='PENDING'`, c.TraderID)
		return err
	}
	if c.ProviderType != "okx" {
		return fmt.Errorf("复制当前仓位首版仅支持 OKX 领航员")
	}
	if _, err = uuid.Parse(c.CopyCurrentPositionsRequestID); err != nil {
		return fmt.Errorf("复制当前仓位需要有效的请求幂等编号")
	}
	// A response lost after saving or after execution cannot register another batch.
	var owner, provider, leader, previousAccount string
	var source int
	err = tx.QueryRow(`SELECT trader_id,provider,leader_id,source_generation,exchange_id FROM copy_trade_current_position_requests WHERE request_id=?`, c.CopyCurrentPositionsRequestID).Scan(&owner, &provider, &leader, &source, &previousAccount)
	if err == nil {
		if owner != c.TraderID || provider != c.ProviderType || leader != c.LeaderID || source != c.SourceGeneration || previousAccount != account {
			return fmt.Errorf("复制请求配置身份已改变，请重新开启复制开关")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if status != TraderLifecycleStopped {
		return fmt.Errorf("请先停止交易员，再登记下次启动复制仓位")
	}
	if _, err = tx.Exec(`UPDATE copy_trade_current_position_requests SET status='CANCELLED',reason='OPERATOR_REPLACED' WHERE trader_id=? AND status='PENDING'`, c.TraderID); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO copy_trade_current_position_requests(request_id,trader_id,provider,leader_id,source_generation,exchange_id,saved_generation) VALUES(?,?,?,?,?,?,?)`, c.CopyCurrentPositionsRequestID, c.TraderID, c.ProviderType, c.LeaderID, c.SourceGeneration, account, generation)
	return err
}

func (s *CopyTradeStore) CurrentPositionCopy(traderID string) (*CurrentPositionCopy, error) {
	return s.currentPositionCopy(traderID, false)
}
func (s *CopyTradeStore) ActiveCurrentPositionCopy(traderID string) (*CurrentPositionCopy, error) {
	return s.currentPositionCopy(traderID, true)
}
func (s *CopyTradeStore) currentPositionCopy(traderID string, activeOnly bool) (*CurrentPositionCopy, error) {
	r := &CurrentPositionCopy{Tasks: []CurrentPositionCopyTask{}}
	query := `SELECT request_id,status,reason,created_at,snapshot_at FROM copy_trade_current_position_requests WHERE trader_id=?`
	if activeOnly {
		query += ` AND status IN ('PENDING','SEALED')`
	}
	query += ` ORDER BY rowid DESC LIMIT 1`
	err := s.db.QueryRow(query, traderID).Scan(&r.RequestID, &r.Status, &r.Reason, &r.CreatedAt, &r.SnapshotAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT t.id,t.leader_pos_id,t.symbol,t.side,t.margin_mode,t.opened_ms,t.source_size,t.entry_price,t.reference_price,t.notional,t.leverage,
 CASE WHEN t.intent_id>0 THEN COALESCE(i.status,t.status) ELSE t.status END,
 CASE WHEN t.intent_id>0 THEN COALESCE(NULLIF(i.reason_code,''),t.reason) ELSE t.reason END,t.intent_id,COALESCE(i.filled_quantity,0)
 FROM copy_trade_current_position_tasks t LEFT JOIN copy_trade_execution_intents i ON i.id=t.intent_id WHERE t.request_id=? ORDER BY t.id`, r.RequestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t CurrentPositionCopyTask
		if err = rows.Scan(&t.ID, &t.LeaderPosID, &t.Symbol, &t.Side, &t.MarginMode, &t.OpenedMS, &t.Size, &t.EntryPrice, &t.ReferencePrice, &t.Notional, &t.Leverage, &t.Status, &t.Reason, &t.IntentID, &t.FilledQuantity); err != nil {
			return nil, err
		}
		r.Tasks = append(r.Tasks, t)
	}
	return r, rows.Err()
}

// CurrentCopyEligibility intentionally rejects every previously participated
// lifecycle, including released custody. Group permission never clears risk or
// manual controls, and no API request can authorize importing an unknown ID.
func (s *CopyTradeStore) CurrentCopyEligibility(traderID, leaderID string, p CurrentPositionCopyTask) (string, error) {
	return currentCopyEligibility(s.db, traderID, leaderID, p)
}
func currentCopyEligibility(q interface {
	QueryRow(string, ...interface{}) *sql.Row
}, traderID, leaderID string, p CurrentPositionCopyTask) (string, error) {
	if p.LeaderPosID == "" || p.OpenedMS <= 0 || p.Symbol == "" || (p.Side != "long" && p.Side != "short") || (p.MarginMode != "cross" && p.MarginMode != "isolated") || !finiteNonnegative(p.Size) || p.Size <= 0 {
		return "SOURCE_IDENTITY_UNAVAILABLE", nil
	}
	var count int
	if err := q.QueryRow(`SELECT (SELECT COUNT(*) FROM copy_trade_position_custody WHERE trader_id=? AND leader_pos_id=?)+
 (SELECT COUNT(*) FROM copy_trade_execution_intents WHERE trader_id=? AND leader_pos_id=? AND (filled_quantity>0 OR terminal_at IS NULL))+
 (SELECT COUNT(*) FROM copy_trade_follow_controls WHERE trader_id=? AND leader_pos_id=? AND version>0)`, traderID, p.LeaderPosID, traderID, p.LeaderPosID, traderID, p.LeaderPosID).Scan(&count); err != nil {
		return "", err
	}
	if count > 0 {
		return "ALREADY_PARTICIPATED_OR_UNSETTLED", nil
	}
	var status, leader, symbol, side, margin, reason string
	err := q.QueryRow(`SELECT status,leader_id,symbol,side,margin_mode,COALESCE(last_failure_reason,'') FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, p.LeaderPosID).Scan(&status, &leader, &symbol, &side, &margin, &reason)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if err == nil {
		if status != MappingStatusIgnored {
			return "SOURCE_NOT_INITIAL_BASELINE", nil
		}
		if leader != leaderID || symbol != p.Symbol || side != p.Side || margin != p.MarginMode {
			return "SOURCE_IDENTITY_CHANGED", nil
		}
		if reason != "" && reason != "ROLLOUT_NO_CHASE" && reason != "STOPPED_SOURCE_BASELINE" {
			return "SOURCE_PREVIOUSLY_SKIPPED", nil
		}
	}
	var blocked bool
	err = q.QueryRow(`SELECT paused OR risk_blocked OR source_ended OR entry_block_reason<>'' OR (conflict_reason<>'' AND conflict_reason<>'GROUP_PARTICIPATION_CONFLICT') FROM copy_trade_follow_groups WHERE trader_id=? AND symbol=? AND side=? AND source_ended=0`, traderID, p.Symbol, p.Side).Scan(&blocked)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if blocked {
		return "GROUP_ENTRY_BLOCKED", nil
	}
	return "", nil
}

// ValidateCurrentCopyCandidates applies the complete direction-group rule to a
// preview. Sealing repeats it inside the write transaction to close races.
func (s *CopyTradeStore) ValidateCurrentCopyCandidates(traderID, leaderID string, positions []CurrentPositionCopyTask) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = validateCurrentCopyCandidatesTx(tx, traderID, leaderID, positions); err != nil {
		return err
	}
	return tx.Commit()
}

func validateCurrentCopyCandidatesTx(tx *sql.Tx, traderID, leaderID string, positions []CurrentPositionCopyTask) error {
	eligible := map[string]bool{}
	blockedDirection := map[string]bool{}
	for i := range positions {
		p := &positions[i]
		reason, err := currentCopyEligibility(tx, traderID, leaderID, *p)
		if err != nil {
			return err
		}
		if p.Reason == "" {
			p.Reason = reason
		}
		eligible[p.LeaderPosID] = p.Reason == ""
		if p.Reason != "" {
			var status string
			err = tx.QueryRow(`SELECT status FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, p.LeaderPosID).Scan(&status)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err == sql.ErrNoRows || status == MappingStatusIgnored {
				blockedDirection[p.Symbol+"|"+p.Side] = true
			}
		}
	}
	rows, err := tx.Query(`SELECT leader_pos_id,symbol,side FROM copy_trade_position_mappings WHERE trader_id=? AND status='ignored'`, traderID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, symbol, side string
		if err = rows.Scan(&id, &symbol, &side); err != nil {
			rows.Close()
			return err
		}
		if !eligible[id] {
			blockedDirection[symbol+"|"+side] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i := range positions {
		p := &positions[i]
		if p.Reason == "" && blockedDirection[p.Symbol+"|"+p.Side] {
			p.Reason = "GROUP_PARTICIPATION_CONFLICT"
		}
		p.Status = "READY"
		if p.Reason != "" {
			p.Status = "SKIPPED"
		}
	}
	return nil
}

// SealCurrentPositionCopy runs after normal startup baselines, before group
// publication. The complete scope, baseline authorization and cleared ignored
// group gates become visible together. No fill or custody is fabricated.
func (s *CopyTradeStore) SealCurrentPositionCopy(traderID, leaderID string, sourceGeneration int, positions []CurrentPositionCopyTask, observed time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var request, account string
	var generation int64
	err = tx.QueryRow(`SELECT r.request_id,t.exchange_id,t.lifecycle_generation FROM copy_trade_current_position_requests r JOIN traders t ON t.id=r.trader_id
 JOIN copy_trade_configs c ON c.trader_id=t.id WHERE r.trader_id=? AND r.status='PENDING' AND r.provider='okx' AND r.leader_id=? AND r.source_generation=?
 AND r.exchange_id=t.exchange_id AND c.provider_type=r.provider AND c.leader_id=r.leader_id AND c.source_generation=r.source_generation
 AND t.lifecycle_status='RUNNING' AND t.is_running=1 AND t.decision_mode='copy_trade' AND t.lifecycle_generation>r.saved_generation
 AND EXISTS(SELECT 1 FROM trader_lifecycle_events e WHERE e.trader_id=t.id AND e.generation=t.lifecycle_generation AND e.reason_code='OPERATOR_START')`, traderID, leaderID, sourceGeneration).Scan(&request, &account, &generation)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if err = validateCurrentCopyCandidatesTx(tx, traderID, leaderID, positions); err != nil {
		return err
	}
	for _, p := range positions {
		status := "READY"
		if p.Reason != "" {
			status = "SKIPPED"
		}
		if _, err = tx.Exec(`INSERT INTO copy_trade_current_position_tasks(request_id,trader_id,leader_pos_id,symbol,side,margin_mode,opened_ms,source_size,entry_price,reference_price,notional,leverage,status,reason) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, request, traderID, p.LeaderPosID, p.Symbol, p.Side, p.MarginMode, p.OpenedMS, p.Size, p.EntryPrice, p.ReferencePrice, p.Notional, p.Leverage, status, p.Reason); err != nil {
			return err
		}
		if status != "READY" {
			continue
		}
		if _, err = tx.Exec(`INSERT INTO copy_trade_position_mappings(trader_id,leader_pos_id,leader_id,symbol,side,margin_mode,status,source_revision,last_known_size,opened_at)
 VALUES(?,?,?,?,?,?,'copy_pending',1,?,CURRENT_TIMESTAMP) ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET status='copy_pending',last_known_size=excluded.last_known_size,updated_at=CURRENT_TIMESTAMP`, traderID, p.LeaderPosID, leaderID, p.Symbol, p.Side, p.MarginMode, p.Size); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE copy_trade_follow_groups SET ignored=0,conflict_reason='',version=version+1 WHERE trader_id=? AND account_id=? AND symbol=? AND side=? AND source_ended=0 AND NOT paused AND NOT risk_blocked AND entry_block_reason='' AND conflict_reason IN ('','GROUP_PARTICIPATION_CONFLICT')`, traderID, account, p.Symbol, p.Side); err != nil {
			return err
		}
	}
	reason := ""
	if len(positions) == 0 {
		reason = "NO_CURRENT_POSITIONS"
	}
	if _, err = tx.Exec(`UPDATE copy_trade_current_position_requests SET status='SEALED',reason=?,start_generation=?,snapshot_at=? WHERE request_id=? AND status='PENDING'`, reason, generation, observed.UTC().Format(time.RFC3339Nano), request); err != nil {
		return err
	}
	if err = refreshCurrentPositionCopyTx(tx, traderID); err != nil {
		return err
	}
	return tx.Commit()
}

// An authorization may bind to exactly one durable ordinary opening intent.
func bindCurrentPositionCopyTx(tx *sql.Tx, intent *CopyTradeExecutionIntent, id int64) error {
	if intent.CurrentPositionTaskID == 0 {
		return nil
	}
	if intent.SourceKind != "LEADER_TRANSITION" || (intent.Action != "open_long" && intent.Action != "open_short") {
		return fmt.Errorf("invalid current-position entry intent")
	}
	res, err := tx.Exec(`UPDATE copy_trade_current_position_tasks SET intent_id=? WHERE id=? AND trader_id=? AND leader_pos_id=? AND opened_ms=? AND side=? AND status='READY' AND (intent_id=0 OR intent_id=?)
 AND EXISTS(SELECT 1 FROM copy_trade_current_position_requests r JOIN traders t ON t.id=r.trader_id WHERE r.request_id=copy_trade_current_position_tasks.request_id AND r.status='SEALED' AND r.exchange_id=t.exchange_id AND r.start_generation=t.lifecycle_generation AND t.lifecycle_status='RUNNING')`, id, intent.CurrentPositionTaskID, intent.TraderID, intent.LeaderPosID, intent.SourceOpenedMS, intent.Side, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("current-position copy authorization changed")
	}
	return nil
}
func (s *CopyTradeStore) CurrentPositionCopyTask(traderID, posID string) (*CurrentPositionCopyTask, error) {
	t := &CurrentPositionCopyTask{}
	err := s.db.QueryRow(`SELECT t.id,t.opened_ms,t.side,t.margin_mode,t.source_size,t.intent_id,r.snapshot_at FROM copy_trade_current_position_tasks t
 JOIN copy_trade_current_position_requests r ON r.request_id=t.request_id JOIN traders owner ON owner.id=t.trader_id JOIN copy_trade_configs cfg ON cfg.trader_id=owner.id
 WHERE t.trader_id=? AND t.leader_pos_id=? AND t.status='READY' AND r.status='SEALED' AND r.start_generation=owner.lifecycle_generation AND owner.lifecycle_status='RUNNING' AND owner.exchange_id=r.exchange_id AND owner.decision_mode='copy_trade' AND cfg.provider_type=r.provider AND cfg.leader_id=r.leader_id AND cfg.source_generation=r.source_generation ORDER BY t.id DESC LIMIT 1`, traderID, posID).Scan(&t.ID, &t.OpenedMS, &t.Side, &t.MarginMode, &t.Size, &t.IntentID, &t.SnapshotAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}
func (s *CopyTradeStore) RefreshCurrentPositionCopy(traderID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = refreshCurrentPositionCopyTx(tx, traderID); err != nil {
		return err
	}
	return tx.Commit()
}
func refreshCurrentPositionCopyTx(tx *sql.Tx, traderID string) error {
	// A terminal UI status is not a source acknowledgement. Failed local writes
	// remain replayable, and confirmed venue fills must first be booked normally.
	rows, err := tx.Query(`SELECT t.id,t.intent_id FROM copy_trade_current_position_tasks t JOIN copy_trade_execution_intents i ON i.id=t.intent_id WHERE t.trader_id=? AND t.status='READY' AND i.terminal_at IS NOT NULL`, traderID)
	if err != nil {
		return err
	}
	type taskIntent struct{ task, intent int64 }
	var candidates []taskIntent
	for rows.Next() {
		var c taskIntent
		if err = rows.Scan(&c.task, &c.intent); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range candidates {
		facts, err := inspectLeaderTransitionTx(tx, c.intent)
		if err != nil {
			return err
		}
		if facts.Effect == SourceEffectUnresolved || !facts.MappingExists || facts.MappingRevision < facts.Revision {
			continue
		}
		if _, err = tx.Exec(`UPDATE copy_trade_current_position_tasks SET status='DONE' WHERE id=? AND status='READY'`, c.task); err != nil {
			return err
		}
	}
	// Failed members in a participated direction retain its source denominator
	// and exits, but can never open/add. All-zero directions remain ignored.
	_, err = tx.Exec(`UPDATE copy_trade_position_mappings AS m SET status=CASE WHEN EXISTS(
 SELECT 1 FROM copy_trade_position_custody p WHERE p.trader_id=m.trader_id AND p.symbol=m.symbol AND p.side=m.side AND EXISTS(SELECT 1 FROM copy_trade_follow_group_members gm JOIN copy_trade_follow_groups g ON g.id=gm.group_id WHERE g.trader_id=m.trader_id AND g.source_ended=0 AND gm.leader_pos_id=p.leader_pos_id)) THEN 'detached' ELSE 'ignored' END,
 last_failure_reason='CURRENT_POSITION_COPY_NO_FILL',updated_at=CURRENT_TIMESTAMP
 WHERE m.trader_id=? AND m.status='copy_pending'
 AND NOT EXISTS(SELECT 1 FROM copy_trade_current_position_tasks t WHERE t.trader_id=m.trader_id AND t.leader_pos_id=m.leader_pos_id AND t.status='READY')
 AND (EXISTS(SELECT 1 FROM copy_trade_position_custody p JOIN copy_trade_follow_group_members gm ON gm.leader_pos_id=p.leader_pos_id JOIN copy_trade_follow_groups g ON g.id=gm.group_id WHERE p.trader_id=m.trader_id AND g.trader_id=m.trader_id AND g.symbol=m.symbol AND g.side=m.side AND g.source_ended=0)
 OR NOT EXISTS(SELECT 1 FROM copy_trade_current_position_tasks t WHERE t.trader_id=m.trader_id AND t.symbol=m.symbol AND t.side=m.side AND t.status='READY'))`, traderID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE copy_trade_current_position_requests SET status='DONE' WHERE trader_id=? AND status='SEALED' AND NOT EXISTS(SELECT 1 FROM copy_trade_current_position_tasks t WHERE t.request_id=copy_trade_current_position_requests.request_id AND t.status='READY')`, traderID)
	return err
}

func cancelCurrentPositionCopyTx(tx *sql.Tx, traderID, reason string) error {
	if _, err := tx.Exec(`UPDATE copy_trade_current_position_requests SET status='CANCELLED',reason=? WHERE trader_id=? AND status='PENDING'`, reason, traderID); err != nil {
		return err
	}
	return cancelCurrentPositionCopyTasksTx(tx, traderID, reason)
}

// Background stopped-state cleanup and a failed startup retire sealed work,
// but must preserve a newly registered request that has not acquired a snapshot.
func cancelCurrentPositionCopyTasksTx(tx *sql.Tx, traderID, reason string) error {
	// Bound but unsent work is also cancelled. A queued decision is not an
	// exchange result; submitted/unknown work remains with the normal reconciler.
	rows, err := tx.Query(`SELECT intent_id FROM copy_trade_current_position_tasks WHERE trader_id=? AND status='READY' AND intent_id>0`, traderID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		facts, err := inspectLeaderTransitionTx(tx, id)
		if err != nil {
			return err
		}
		if facts.Resolution != "" || (facts.Effect != SourceEffectUnsubmitted && facts.Effect != SourceEffectZeroFill) {
			continue
		}
		if err = finishLeaderSourceTransitionTx(tx, facts, FinishLeaderSourceTransitionRequest{IntentID: id, TraderID: traderID, Disposition: SourceSupersedeNoFill, Reason: reason, Evidence: "operator invalidated one-shot entry authorization before a confirmed fill"}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE copy_trade_current_position_tasks SET status='SKIPPED',reason=? WHERE trader_id=? AND status='READY' AND intent_id=0`, reason, traderID); err != nil {
		return err
	}
	return refreshCurrentPositionCopyTx(tx, traderID)
}

func (s *CopyTradeStore) SkipCurrentPositionCopyTask(taskID int64, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var trader string
	if err = tx.QueryRow(`SELECT trader_id FROM copy_trade_current_position_tasks WHERE id=?`, taskID).Scan(&trader); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_current_position_tasks SET status='SKIPPED',reason=? WHERE id=? AND intent_id=0`, strings.TrimSpace(reason), taskID); err != nil {
		return err
	}
	if err = refreshCurrentPositionCopyTx(tx, trader); err != nil {
		return err
	}
	return tx.Commit()
}

// Lookup is independent of the current runtime generation: recovery must retain
// provenance even while submission remains fenced by the original authorization.
func (s *CopyTradeStore) CurrentPositionTaskForIntent(intentID int64) (*CurrentPositionCopyTask, error) {
	t := &CurrentPositionCopyTask{}
	err := s.db.QueryRow(`SELECT id,leader_pos_id,opened_ms,margin_mode FROM copy_trade_current_position_tasks WHERE intent_id=? AND intent_id>0`, intentID).Scan(&t.ID, &t.LeaderPosID, &t.OpenedMS, &t.MarginMode)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// RefreshUnsubmittedCurrentCopyTarget keeps the sealed source scope unchanged.
// A healthy size change can supersede a proven unsent/zero-fill intent through
// the existing source finisher, then use its successor revision. Unknown or
// already filled orders can never be replaced here.
func (s *CopyTradeStore) RefreshUnsubmittedCurrentCopyTarget(taskID, intentID int64, size float64) error {
	if !finiteNonnegative(size) || size <= 0 {
		return fmt.Errorf("invalid current copy source size")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var traderID, leaderID string
	err = tx.QueryRow(`SELECT t.trader_id,r.leader_id FROM copy_trade_current_position_tasks t JOIN copy_trade_current_position_requests r ON r.request_id=t.request_id JOIN traders owner ON owner.id=t.trader_id
 WHERE t.id=? AND t.intent_id=? AND t.status='READY' AND r.status='SEALED' AND owner.lifecycle_status='RUNNING' AND owner.lifecycle_generation=r.start_generation AND owner.exchange_id=r.exchange_id AND owner.decision_mode='copy_trade'`, taskID, intentID).Scan(&traderID, &leaderID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	facts, err := inspectLeaderTransitionTx(tx, intentID)
	if err != nil {
		return err
	}
	if facts.Resolution != "" || (facts.Effect != SourceEffectUnsubmitted && facts.Effect != SourceEffectZeroFill) {
		return nil
	}
	if err = finishLeaderSourceTransitionTx(tx, facts, FinishLeaderSourceTransitionRequest{IntentID: intentID, TraderID: traderID, LeaderID: leaderID, Disposition: SourceSupersedeNoFill, Reason: "CURRENT_POSITION_COPY_TARGET_REFRESHED", Evidence: fmt.Sprintf("sealed copy task %d remains authorized for the same source lifecycle; healthy source size changed to %.12g before any fill", taskID, size)}); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_current_position_tasks SET intent_id=0,source_size=? WHERE id=? AND intent_id=?`, size, taskID, intentID); err != nil {
		return err
	}
	return tx.Commit()
}
