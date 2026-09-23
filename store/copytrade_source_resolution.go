package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	SourceEffectUnsubmitted = "UNSUBMITTED"
	SourceEffectZeroFill    = "TERMINAL_ZERO_FILL"
	SourceEffectBookedFill  = "BOOKED_FILL"
	SourceEffectUnresolved  = "UNRESOLVED"
	SourceConsumeNoFill     = "consume_no_fill"
	SourceSupersedeNoFill   = "supersede_no_fill"
	SourceDetachNoFill      = "detach_no_fill"
	SourceAcknowledgeFill   = "acknowledged_fill"
)

var ErrSourceTransitionUnresolved = errors.New("source transition exchange evidence is unresolved")
var ErrSourceTransitionChanged = errors.New("source transition changed since inspection")

// LeaderTransitionFacts describes exchange evidence separately from the intent's
// display status. A terminal display status is deliberately not a proof of either
// a zero-fill outcome or a committed source acknowledgement.
type LeaderTransitionFacts struct {
	IntentID            int64   `json:"intent_id"`
	TraderID            string  `json:"trader_id"`
	LeaderPosID         string  `json:"leader_pos_id"`
	SourceKind          string  `json:"source_kind"`
	Action              string  `json:"action"`
	Symbol              string  `json:"symbol"`
	Side                string  `json:"side"`
	MarginMode          string  `json:"margin_mode"`
	Revision            int64   `json:"revision"`
	Target              float64 `json:"target"`
	Status              string  `json:"status"`
	Reason              string  `json:"reason"`
	Submitted           bool    `json:"submitted"`
	ExchangeOrderID     string  `json:"exchange_order_id"`
	FilledQuantity      float64 `json:"filled_quantity"`
	TargetQuantity      float64 `json:"target_quantity"`
	BookedQuantity      float64 `json:"booked_quantity"`
	AttemptQuantity     float64 `json:"attempt_quantity"`
	AttemptCount        int     `json:"attempt_count"`
	UnresolvedAttempts  int     `json:"unresolved_attempts"`
	Effect              string  `json:"effect"`
	MappingExists       bool    `json:"mapping_exists"`
	MappingStatus       string  `json:"mapping_status"`
	MappingRevision     int64   `json:"mapping_revision"`
	MappingSize         float64 `json:"mapping_size"`
	CustodyState        string  `json:"custody_state"`
	CustodyCycleID      int64   `json:"custody_cycle_id"`
	FollowVersion       int64   `json:"follow_version"`
	IntentFollowVersion int64   `json:"intent_follow_version"`
	TraderGeneration    int64   `json:"trader_generation"`
	TraderStatus        string  `json:"trader_status"`
	SourceOpenedMS      int64   `json:"source_opened_ms"`
	Resolution          string  `json:"resolution"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
	IssueReason         string  `json:"issue_reason"`
	Fingerprint         string  `json:"fingerprint"`
}

type FinishLeaderSourceTransitionRequest struct {
	IntentID            int64
	TraderID            string
	LeaderID            string
	ExpectedFingerprint string
	Disposition         string
	Reason              string
	Evidence            string
}

func (s *CopyTradeStore) initSourceResolutionTables() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_source_resolutions (
	 intent_id INTEGER PRIMARY KEY,trader_id TEXT NOT NULL,leader_pos_id TEXT NOT NULL,
	 disposition TEXT NOT NULL,reason TEXT NOT NULL,evidence TEXT NOT NULL DEFAULT '',
	 before_json TEXT NOT NULL,after_json TEXT NOT NULL,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
	 CREATE INDEX IF NOT EXISTS idx_source_resolution_trader ON copy_trade_source_resolutions(trader_id);`)
	return err
}

func (s *CopyTradeStore) InspectLeaderTransition(intentID int64) (*LeaderTransitionFacts, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	f, err := inspectLeaderTransitionTx(tx, intentID)
	if err != nil {
		return nil, err
	}
	return f, tx.Commit()
}

func inspectLeaderTransitionTx(tx *sql.Tx, intentID int64) (*LeaderTransitionFacts, error) {
	f := &LeaderTransitionFacts{IntentID: intentID, Effect: SourceEffectUnresolved}
	var mappingID sql.NullString
	var mappingRev sql.NullInt64
	err := tx.QueryRow(`SELECT i.trader_id,i.leader_pos_id,COALESCE(i.source_kind,'LEADER_TRANSITION'),i.action,COALESCE(i.symbol,''),COALESCE(i.side,''),COALESCE(i.margin_mode,''),
	 i.source_revision,i.leader_target_size,i.status,COALESCE(i.reason_code,''),i.submitted_at IS NOT NULL,COALESCE(i.exchange_order_id,''),
	 COALESCE(i.filled_quantity,0),COALESCE(NULLIF(i.target_quantity,0),NULLIF(i.quantized_quantity,0),i.requested_quantity,0),
	 m.trader_id,m.source_revision,COALESCE(m.status,''),COALESCE(m.last_known_size,0),
	 COALESCE(p.state,''),COALESCE(p.cycle_id,0),COALESCE(c.version,0),COALESCE(i.follow_control_version,0),
	 COALESCE(t.lifecycle_generation,0),COALESCE(t.lifecycle_status,''),COALESCE(l.opened_ms,0),COALESCE(r.disposition,''),COALESCE(CAST(i.created_at AS TEXT),''),COALESCE(CAST(i.updated_at AS TEXT),'')
	 FROM copy_trade_execution_intents i
	 LEFT JOIN copy_trade_position_mappings m ON m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id
	 LEFT JOIN copy_trade_position_custody p ON p.trader_id=i.trader_id AND p.leader_pos_id=i.leader_pos_id
	 LEFT JOIN copy_trade_follow_controls c ON c.trader_id=i.trader_id AND c.leader_pos_id=i.leader_pos_id
	 LEFT JOIN traders t ON t.id=i.trader_id
	 LEFT JOIN copy_trade_source_lifecycles l ON l.trader_id=i.trader_id AND l.leader_pos_id=i.leader_pos_id
	 LEFT JOIN copy_trade_source_resolutions r ON r.intent_id=i.id WHERE i.id=?`, intentID).Scan(
		&f.TraderID, &f.LeaderPosID, &f.SourceKind, &f.Action, &f.Symbol, &f.Side, &f.MarginMode, &f.Revision, &f.Target, &f.Status, &f.Reason, &f.Submitted, &f.ExchangeOrderID,
		&f.FilledQuantity, &f.TargetQuantity, &mappingID, &mappingRev, &f.MappingStatus, &f.MappingSize, &f.CustodyState, &f.CustodyCycleID, &f.FollowVersion, &f.IntentFollowVersion,
		&f.TraderGeneration, &f.TraderStatus, &f.SourceOpenedMS, &f.Resolution, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return nil, err
	}
	f.MappingExists, f.MappingRevision = mappingID.Valid, mappingRev.Int64
	if parsed, e := parseDBTime(f.CreatedAt); e == nil {
		f.CreatedAt = parsed.UTC().Format(time.RFC3339Nano)
	}
	if parsed, e := parseDBTime(f.UpdatedAt); e == nil {
		f.UpdatedAt = parsed.UTC().Format(time.RFC3339Nano)
	}
	rows, err := tx.Query(`SELECT client_order_id,COALESCE(exchange_order_id,''),status,COALESCE(exchange_state,''),filled_quantity,submitted_at IS NOT NULL,terminal_at IS NOT NULL FROM copy_trade_execution_order_attempts WHERE intent_id=? ORDER BY id`, intentID)
	if err != nil {
		return nil, err
	}
	type attempt struct {
		Client, Order, Status, State string
		Quantity                     float64
		Submitted, Terminal          bool
	}
	var attempts []attempt
	for rows.Next() {
		var a attempt
		if err = rows.Scan(&a.Client, &a.Order, &a.Status, &a.State, &a.Quantity, &a.Submitted, &a.Terminal); err != nil {
			rows.Close()
			return nil, err
		}
		attempts = append(attempts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	f.AttemptCount = len(attempts)
	allLocal, allBooked := true, true
	for _, a := range attempts {
		if !finiteNonnegative(a.Quantity) {
			f.UnresolvedAttempts++
			allBooked = false
			allLocal = false
			continue
		}
		f.AttemptQuantity += a.Quantity
		local := !a.Submitted && a.Order == "" && a.Quantity == 0 && (a.Status == ExecutionOrderAttemptPrepared || a.Status == ExecutionOrderAttemptTerminalNoFill)
		if local {
			continue
		}
		allLocal = false
		state := strings.ToUpper(a.State)
		terminal := a.Terminal && (state == "FILLED" || state == "CANCELED" || state == "CANCELLED" || state == "REJECTED" || state == "EXPIRED" || state == "FAILED")
		if !terminal || (state == "FILLED" && a.Quantity == 0) {
			f.UnresolvedAttempts++
			allBooked = false
		}
		if a.Quantity > 0 {
			var n int
			var quantity float64
			if err = tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(filled_quantity),0) FROM copy_trade_execution_fill_commits WHERE intent_id=? AND fill_key IN (?,?)`, intentID, a.Client, a.Order).Scan(&n, &quantity); err != nil {
				return nil, err
			}
			if n != 1 || !sameSourceQuantity(quantity, a.Quantity) {
				allBooked = false
			}
		}
	}
	if err = tx.QueryRow(`SELECT COALESCE(SUM(filled_quantity),0) FROM copy_trade_execution_fill_commits WHERE intent_id=?`, intentID).Scan(&f.BookedQuantity); err != nil {
		return nil, err
	}
	if finiteNonnegative(f.FilledQuantity) && finiteNonnegative(f.BookedQuantity) {
		switch {
		case !f.Submitted && f.ExchangeOrderID == "" && f.FilledQuantity == 0 && f.BookedQuantity == 0 && allLocal:
			f.Effect = SourceEffectUnsubmitted
		case f.AttemptCount > 0 && f.UnresolvedAttempts == 0 && !allLocal && f.AttemptQuantity == 0 && f.FilledQuantity == 0 && f.BookedQuantity == 0:
			f.Effect = SourceEffectZeroFill
		case f.AttemptCount > 0 && f.UnresolvedAttempts == 0 && allBooked && f.FilledQuantity > 0 && sameSourceQuantity(f.BookedQuantity, f.FilledQuantity) && sameSourceQuantity(f.AttemptQuantity, f.FilledQuantity):
			f.Effect = SourceEffectBookedFill
		}
	}
	switch {
	case f.Effect == SourceEffectUnresolved:
		f.IssueReason = "交易所订单或成交入账证据待核实；禁止按等待时间重发"
	case f.Effect == SourceEffectBookedFill && f.MappingRevision >= f.Revision:
		f.IssueReason = "成交已逐笔入账，源状态收尾待完成"
	case f.MappingRevision < f.Revision:
		f.IssueReason = "源修订尚未确认；须按无成交事实及跟随规则收尾"
	}
	// Include concrete attempt identities in the compare-and-swap fingerprint;
	// replacing one terminal attempt with another is not equivalent evidence.
	raw, _ := json.Marshal(struct {
		Facts    *LeaderTransitionFacts
		Attempts []attempt
	}{f, attempts})
	f.Fingerprint = fmt.Sprintf("%x", sha256.Sum256(raw))
	return f, nil
}

func finiteNonnegative(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
func sameSourceQuantity(a, b float64) bool {
	return finiteNonnegative(a) && finiteNonnegative(b) && math.Abs(a-b) <= math.Max(1e-12, math.Max(a, b)*1e-9)
}

func (s *CopyTradeStore) FinishLeaderSourceTransition(req FinishLeaderSourceTransitionRequest) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, err := inspectLeaderTransitionTx(tx, req.IntentID)
	if err != nil {
		return err
	}
	if err = finishLeaderSourceTransitionTx(tx, f, req); err != nil {
		return err
	}
	return tx.Commit()
}

func finishLeaderSourceTransitionTx(tx *sql.Tx, f *LeaderTransitionFacts, req FinishLeaderSourceTransitionRequest) error {
	if f.SourceKind != "LEADER_TRANSITION" || req.TraderID != f.TraderID || !finiteNonnegative(f.Target) {
		return fmt.Errorf("invalid leader source transition identity")
	}
	if f.Resolution != "" {
		if f.Resolution == req.Disposition {
			return nil
		}
		return fmt.Errorf("source transition already resolved as %s", f.Resolution)
	}
	if req.ExpectedFingerprint != "" && req.ExpectedFingerprint != f.Fingerprint {
		return ErrSourceTransitionChanged
	}
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("source resolution reason is required")
	}
	before, _ := json.Marshal(f)
	status := ExecutionIntentSkipped
	if req.Disposition == SourceAcknowledgeFill {
		if f.Effect != SourceEffectBookedFill || !f.MappingExists || f.MappingRevision < f.Revision {
			return ErrSourceTransitionUnresolved
		}
		if f.MappingRevision == f.Revision && f.MappingStatus == MappingStatusActive && f.CustodyState != "RELEASED" && f.Status != ExecutionIntentCompletedPartial && f.TargetQuantity > 0 && !sameSourceQuantity(f.FilledQuantity, f.TargetQuantity) && f.FilledQuantity < f.TargetQuantity {
			return ErrSourceTransitionUnresolved // A live partial fill still owns its normal catch-up contract.
		}
		status = ExecutionIntentFilled
		if f.TargetQuantity > 0 && f.FilledQuantity+math.Max(1e-12, f.TargetQuantity*1e-9) < f.TargetQuantity {
			status = ExecutionIntentCompletedPartial
		}
		if f.Status == ExecutionIntentProtected {
			status = ExecutionIntentProtected
		}
	} else {
		if req.Disposition != SourceConsumeNoFill && req.Disposition != SourceSupersedeNoFill && req.Disposition != SourceDetachNoFill {
			return fmt.Errorf("invalid source disposition %q", req.Disposition)
		}
		if f.Effect != SourceEffectUnsubmitted && f.Effect != SourceEffectZeroFill {
			return ErrSourceTransitionUnresolved
		}
		if req.Disposition == SourceConsumeNoFill && (strings.HasPrefix(f.Action, "close_") || strings.HasPrefix(f.Action, "reduce_")) {
			return fmt.Errorf("zero-fill exit requires confirmed reduction/flat proof through leader exit completion")
		}
		if req.Disposition == SourceDetachNoFill && f.CustodyState != "RELEASED" {
			return fmt.Errorf("detaching requires durable release of the original follower")
		}
		if f.MappingExists && f.MappingRevision < f.Revision-1 {
			return fmt.Errorf("source acknowledgement has a revision gap")
		}
		if !f.MappingExists {
			if f.Revision != 1 || req.LeaderID == "" {
				return fmt.Errorf("initial source acknowledgement lacks baseline identity")
			}
			mappingStatus, size := MappingStatusIgnored, f.Target
			if req.Disposition == SourceSupersedeNoFill {
				mappingStatus, size = MappingStatusClosed, 0
			}
			_, err := tx.Exec(`INSERT INTO copy_trade_position_mappings(trader_id,leader_pos_id,leader_id,symbol,side,margin_mode,status,source_revision,last_known_size,last_failure_reason,opened_at,closed_at) VALUES(?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP,CASE WHEN ?='closed' THEN CURRENT_TIMESTAMP ELSE NULL END)`, f.TraderID, f.LeaderPosID, req.LeaderID, f.Symbol, f.Side, f.MarginMode, mappingStatus, f.Revision, size, req.Reason, mappingStatus)
			if err != nil {
				return err
			}
		} else if f.MappingRevision == f.Revision-1 {
			size, next := f.MappingSize, f.MappingStatus
			if req.Disposition != SourceSupersedeNoFill && next != MappingStatusClosed && next != MappingStatusManualStopped {
				size = f.Target
			}
			if req.Disposition == SourceDetachNoFill && next == MappingStatusActive {
				next = MappingStatusDetached
				if strings.HasPrefix(f.Action, "close_") && f.Target == 0 {
					next = MappingStatusClosed
				}
			}
			if _, err := tx.Exec(`UPDATE copy_trade_position_mappings SET source_revision=?,last_known_size=?,status=?,closed_at=CASE WHEN ?='closed' THEN COALESCE(closed_at,CURRENT_TIMESTAMP) ELSE closed_at END,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND source_revision=?`, f.Revision, size, next, next, f.TraderID, f.LeaderPosID, f.MappingRevision); err != nil {
				return err
			}
		}
	}
	// Retire local queued attempts in every final disposition. None may cross
	// the submission boundary after its business instruction has been consumed.
	if _, err := tx.Exec(`UPDATE copy_trade_execution_order_attempts SET status='TERMINAL_NO_FILL',terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),last_error=?,updated_at=CURRENT_TIMESTAMP WHERE intent_id=? AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')='' AND filled_quantity=0 AND status='PREPARED'`, req.Reason, f.IntentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_trade_execution_intents SET status=?,reason_code=?,last_error='',terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, req.Reason, f.IntentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP WHERE intent_id=? AND status<>'SOURCE_REPLAY_PENDING'`, status, f.IntentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_guard_risk_reservations SET status='RELEASED',released_at=CURRENT_TIMESTAMP WHERE intent_id=? AND status='ACTIVE'`, f.IntentID); err != nil {
		return err
	}
	after, err := inspectLeaderTransitionTx(tx, f.IntentID)
	if err != nil {
		return err
	}
	afterJSON, _ := json.Marshal(after)
	_, err = tx.Exec(`INSERT INTO copy_trade_source_resolutions(intent_id,trader_id,leader_pos_id,disposition,reason,evidence,before_json,after_json) VALUES(?,?,?,?,?,?,?,?)`, f.IntentID, f.TraderID, f.LeaderPosID, req.Disposition, req.Reason, req.Evidence, string(before), string(afterJSON))
	return err
}

// ResolveAcknowledgedLeaderIntent ends only proven, already-booked business.
// Missing protection is a separate job; a newer mapping alone proves nothing.
func (s *CopyTradeStore) ResolveAcknowledgedLeaderIntent(intentID int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	f, err := inspectLeaderTransitionTx(tx, intentID)
	if err != nil {
		return false, err
	}
	if f.Resolution == SourceAcknowledgeFill {
		return true, tx.Commit()
	}
	if f.Effect != SourceEffectBookedFill || !f.MappingExists || f.MappingRevision < f.Revision {
		return false, nil
	}
	if f.MappingRevision == f.Revision && f.MappingStatus == MappingStatusActive && f.CustodyState != "RELEASED" && f.Status != ExecutionIntentCompletedPartial && f.TargetQuantity > 0 && f.FilledQuantity < f.TargetQuantity && !sameSourceQuantity(f.FilledQuantity, f.TargetQuantity) {
		return false, nil
	}
	err = finishLeaderSourceTransitionTx(tx, f, FinishLeaderSourceTransitionRequest{IntentID: intentID, TraderID: f.TraderID, Disposition: SourceAcknowledgeFill, Reason: "SOURCE_FILL_ALREADY_ACKNOWLEDGED", Evidence: "terminal order attempts match durable per-order fill commits; source mapping is not behind"})
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListLeaderSourceRepairCandidates is read-only. It reports latent stopped
// records too, but deliberately does not call them missed trades or authorize
// any mutation solely because a display status is terminal.
func (s *CopyTradeStore) ListLeaderSourceRepairCandidates(traderID string) ([]*LeaderTransitionFacts, error) {
	rows, err := s.db.Query(`SELECT i.id FROM copy_trade_execution_intents i LEFT JOIN copy_trade_position_mappings m ON m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id
	 WHERE i.trader_id=? AND i.source_kind='LEADER_TRANSITION' AND NOT EXISTS(SELECT 1 FROM copy_trade_source_resolutions r WHERE r.intent_id=i.id) AND
	 ((i.status IN ('SKIPPED','FAILED','FILLED','PROTECTED','COMPLETED_PARTIAL') AND i.source_revision=COALESCE(m.source_revision,0)+1)
	 OR (i.status='RECONCILING' AND i.source_revision<=m.source_revision)) ORDER BY i.id`, traderID)
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
	var result []*LeaderTransitionFacts
	for _, id := range ids {
		f, e := s.InspectLeaderTransition(id)
		if e != nil {
			return nil, e
		}
		result = append(result, f)
	}
	return result, nil
}

// ListLeaderTransitionIssues is the runtime view of the same evidence used by
// recovery. Stopped/archived candidates remain in the repair inventory, not in
// a live missed-trade count. A legitimate pause/ignored baseline is not a fault.
func (s *CopyTradeStore) ListLeaderTransitionIssues(traderID string) ([]*LeaderTransitionFacts, error) {
	rows, err := s.db.Query(`SELECT i.id FROM copy_trade_execution_intents i LEFT JOIN copy_trade_position_mappings m ON m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id
	 JOIN traders t ON t.id=i.trader_id WHERE i.trader_id=? AND t.lifecycle_status='RUNNING' AND t.is_running=1 AND i.source_kind='LEADER_TRANSITION'
	 AND NOT EXISTS(SELECT 1 FROM copy_trade_source_resolutions r WHERE r.intent_id=i.id)
	 AND ((i.status IN ('SKIPPED','FAILED','FILLED','PROTECTED','COMPLETED_PARTIAL') AND i.source_revision=COALESCE(m.source_revision,0)+1 AND COALESCE(m.status,'') NOT IN ('manual_stopped','ignored'))
	 OR (i.status='RECONCILING' AND COALESCE(m.status,'') NOT IN ('manual_stopped','ignored'))
	 OR EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id AND a.submitted_at IS NOT NULL AND a.terminal_at IS NULL)) ORDER BY i.id`, traderID)
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
	var result []*LeaderTransitionFacts
	for _, id := range ids {
		f, e := s.InspectLeaderTransition(id)
		if e != nil {
			return nil, e
		}
		result = append(result, f)
	}
	return result, nil
}

// RepairReleasedSourceBlockers consumes only a proved no-fill transition after
// custody of its original follower has ended. An add stays an add in history:
// it detaches the source relationship and requests a fresh no-chase baseline,
// rather than inventing a leader close or submitting an expired order.
func (s *CopyTradeStore) RepairReleasedSourceBlockers(traderID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT i.id FROM copy_trade_execution_intents i
	 JOIN copy_trade_position_mappings m ON m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id
	 JOIN copy_trade_position_custody p ON p.trader_id=i.trader_id AND p.leader_pos_id=i.leader_pos_id
	 WHERE i.trader_id=? AND i.source_kind='LEADER_TRANSITION' AND i.status IN ('SKIPPED','FAILED')
	 AND m.status='active' AND m.source_revision=i.source_revision-1 AND p.state='RELEASED'
	 AND NOT EXISTS(SELECT 1 FROM copy_trade_source_resolutions r WHERE r.intent_id=i.id)
	 ORDER BY i.id`, traderID)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		f, e := inspectLeaderTransitionTx(tx, id)
		if e != nil {
			return 0, e
		}
		if f.Effect != SourceEffectUnsubmitted && f.Effect != SourceEffectZeroFill {
			continue
		}
		var unresolved int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents i WHERE i.trader_id=? AND i.leader_pos_id=? AND i.id<>? AND
		 ((i.submitted_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id))
		 OR EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id AND a.submitted_at IS NOT NULL AND a.terminal_at IS NULL))`, traderID, f.LeaderPosID, id).Scan(&unresolved); err != nil {
			return 0, err
		}
		if unresolved > 0 {
			continue
		}
		reason := "RELEASED_SOURCE_REPAIRED"
		if strings.HasPrefix(f.Action, "close_") && f.Target == 0 {
			reason = "RELEASED_CLOSE_REPAIRED"
		}
		if err = finishLeaderSourceTransitionTx(tx, f, FinishLeaderSourceTransitionRequest{IntentID: id, TraderID: traderID, Disposition: SourceDetachNoFill, Reason: reason, Evidence: "original follower custody released; all attempts proved no fill; fresh source baseline required"}); err != nil {
			return 0, err
		}
		if strings.HasPrefix(f.Action, "close_") && f.Target == 0 {
			if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET status='closed',closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP) WHERE trader_id=? AND leader_pos_id=? AND source_revision=? AND status='detached'`, traderID, f.LeaderPosID, f.Revision); err != nil {
				return 0, err
			}
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO copy_trade_released_repairs(intent_id,trader_id,leader_pos_id,detail) VALUES(?,?,?,?)`, id, traderID, f.LeaderPosID, "no-fill source acknowledgement repaired; original action preserved; no exchange order sent"); err != nil {
			return 0, err
		}
		if f.CustodyCycleID > 0 {
			result, e := tx.Exec(`UPDATE copy_guard_cycles SET status='DETACHED',closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),accounting_status='UNSCORABLE',accounting_error=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND trader_id=? AND leader_pos_id=? AND closed_at IS NULL AND NOT EXISTS(SELECT 1 FROM copy_trade_follow_group_guards gg JOIN copy_trade_follow_groups g ON g.id=gg.group_id JOIN copy_trade_follow_group_members gm ON gm.group_id=g.id JOIN copy_trade_position_mappings mm ON mm.trader_id=g.trader_id AND mm.leader_pos_id=gm.leader_pos_id WHERE gg.cycle_id=copy_guard_cycles.id AND g.source_ended=0 AND gm.leader_pos_id<>copy_guard_cycles.leader_pos_id AND mm.status IN ('active','manual_stopped'))`, reason, f.CustodyCycleID, traderID, f.LeaderPosID)
			if e != nil {
				return 0, e
			}
			if n, _ := result.RowsAffected(); n > 0 {
				if err = terminalizeCopyGuardAuxiliaryStateTx(tx, f.CustodyCycleID, CopyGuardDetached); err != nil {
					return 0, err
				}
				if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,metadata_json) VALUES(?,?,?,?)`, f.CustodyCycleID, traderID, reason, fmt.Sprintf(`{"intent_id":%d,"source_revision":%d,"exchange_order_sent":false}`, id, f.Revision)); err != nil {
					return 0, err
				}
			}
		}
		count++
	}
	return count, tx.Commit()
}

// RepairStoppedSourceBlockers is deliberately limited to an explicit historical
// stop boundary. It settles only risk-increasing work whose venue outcome is
// proven empty; submitted/unknown work remains visible for reconciliation.
func (s *CopyTradeStore) RepairStoppedSourceBlockers(traderID, leaderID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := finishStoppedSourceTransitionsTx(tx, traderID, leaderID, true)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func finishStoppedSourceTransitionsTx(tx *sql.Tx, traderID, leaderID string, historical bool) (int, error) {
	if leaderID == "" {
		err := tx.QueryRow(`SELECT leader_id FROM copy_trade_configs WHERE trader_id=?`, traderID).Scan(&leaderID)
		if err != nil && err != sql.ErrNoRows {
			return 0, err
		}
	}
	rows, err := tx.Query(`SELECT id FROM copy_trade_execution_intents i WHERE trader_id=? AND source_kind='LEADER_TRANSITION' AND action IN ('open_long','open_short')
 AND NOT EXISTS(SELECT 1 FROM copy_trade_source_resolutions r WHERE r.intent_id=i.id)
 AND ((? AND reason_code IN ('TRADER_STOPPED_PRE_SUBMIT','TRADER_STOPPED_TERMINAL_NO_FILL') AND status IN ('SKIPPED','FAILED')) OR (NOT ? AND status IN ('RESERVED','RECONCILING','FAILED'))) ORDER BY source_revision,id`, traderID, historical, historical)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		f, e := inspectLeaderTransitionTx(tx, id)
		if e != nil {
			return 0, e
		}
		if f.Effect != SourceEffectUnsubmitted && f.Effect != SourceEffectZeroFill {
			continue
		}
		// Legacy rows without a source identity stay diagnosable, not guessed.
		if !f.MappingExists && leaderID == "" {
			continue
		}
		if f.MappingRevision < f.Revision-1 {
			continue
		}
		reason := "TRADER_STOPPED_PRE_SUBMIT"
		if f.Effect == SourceEffectZeroFill {
			reason = "TRADER_STOPPED_TERMINAL_NO_FILL"
		}
		if e = finishLeaderSourceTransitionTx(tx, f, FinishLeaderSourceTransitionRequest{IntentID: id, TraderID: traderID, LeaderID: leaderID, Disposition: SourceConsumeNoFill, Reason: reason, Evidence: "explicit trader stop; no venue fill; paused risk increase consumed without an order"}); e != nil {
			return 0, e
		}
		n++
	}
	return n, nil
}

// IsLeaderSourceRevalidationReason mirrors the pre-submit retry reasons used
// by ReserveExecutionIntent. Callers must prove the absence of venue effects
// first; this predicate never authorizes an uncertain order to be reissued.
func IsLeaderSourceRevalidationReason(reason string) bool {
	if strings.HasPrefix(reason, "PRECHECK_") {
		return true
	}
	switch reason {
	case "PRE_SUBMIT", "DECISION_CHANNEL_BUSY", "STARTUP_REPLAY_REQUIRED", "SOURCE_REVALIDATION_REQUIRED", "SOURCE_DATA_UNAVAILABLE", "SOURCE_VALUE_UNAVAILABLE", "MIGRATION_RECONCILING":
		return true
	}
	return false
}
