package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	OwnershipGapStrongEvidence           = "STRONG_EVIDENCE"
	OwnershipGapMappingMissing           = "MAPPING_MISSING"
	OwnershipGapMappingNotClosed         = "MAPPING_NOT_CLOSED"
	OwnershipGapCycleStateIncompatible   = "CYCLE_STATE_INCOMPATIBLE"
	OwnershipGapIdentityMismatch         = "IDENTITY_MISMATCH"
	OwnershipGapEntryOrderUnavailable    = "ENTRY_ORDER_UNAVAILABLE"
	OwnershipGapEntryIntentMissing       = "CONFIRMED_ENTRY_INTENT_MISSING"
	OwnershipGapLocalPositionMissing     = "LOCAL_POSITION_IDENTITY_MISSING"
	OwnershipGapUnsafeRevision           = "UNSAFE_PENDING_REVISION"
	OwnershipRecoveredReason             = "SUPERSEDED_BY_RECOVERED_POSITION"
	OwnershipGapFollowerAlreadyFlat      = "FOLLOWER_POSITION_ALREADY_FLAT"
	OwnershipGapFollowerStateUnavailable = "FOLLOWER_POSITION_UNAVAILABLE"
)

// CopyTradeOwnershipGap is a read-only classification of an open Copy Guard
// cycle whose source mapping is no longer live. Recoverable is deliberately a
// database-only ownership proof; the caller must additionally confirm a fresh
// matching exchange position before calling RestoreConfirmedMappingOwnership.
type CopyTradeOwnershipGap struct {
	CycleID                 int64
	TraderID                string
	LeaderID                string
	LeaderPosID             string
	Symbol                  string
	Side                    string
	MarginMode              string
	CycleStatus             string
	FollowerPosID           string
	EntryOrderID            string
	FollowerEntryPrice      float64
	FollowerNotional        float64
	MappingID               int64
	MappingStatus           string
	MappingRevision         int64
	SourceSymbol            string
	ExecutionSymbol         string
	SourceQuoteAsset        string
	ExecutionSettleAsset    string
	EntryIntentID           int64
	EntryIntentRevision     int64
	EntryIntentLeaderTarget float64
	EntryIntentFilled       float64
	LocalPositionID         int64
	LocalPositionQuantity   float64
	Recoverable             bool
	ReasonCode              string
}

type RestoreMappingOwnershipRequest struct {
	TraderID         string
	LeaderPosID      string
	CycleID          int64
	EntryIntentID    int64
	FollowerQuantity float64
	FollowerPosID    string
	// CurrentLeaderSize is the latest healthy authoritative source size. It is
	// used only to absorb missed risk increases; reductions remain actionable.
	CurrentLeaderSize float64
	LeaderStillOpen   bool
	LeaderReversed    bool
}

type RestoreMappingOwnershipResult struct {
	MappingRevision        int64
	SupersededIntentIDs    []int64
	HistoricalLeaderTarget float64
	RestoredLeaderBaseline float64
	PendingRiskReduction   bool
}

type ResolveFlatOwnershipGapRequest struct {
	TraderID          string
	LeaderPosID       string
	CycleID           int64
	EntryIntentID     int64
	LeaderStillOpen   bool
	LeaderReversed    bool
	CurrentLeaderSize float64
	ReasonCode        string
}

type ownershipGapQueryer interface {
	QueryRow(query string, args ...interface{}) *sql.Row
	Query(query string, args ...interface{}) (*sql.Rows, error)
}

// ListOpenCycleOwnershipGaps classifies only cycles that have no live mapping.
// It never changes lifecycle state or infers an exchange position.
func (s *CopyTradeStore) ListOpenCycleOwnershipGaps(traderID string) ([]*CopyTradeOwnershipGap, error) {
	rows, err := s.db.Query(`SELECT c.id
		FROM copy_guard_cycles c
		WHERE c.trader_id=? AND c.closed_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM copy_trade_position_mappings m
			WHERE m.trader_id=c.trader_id AND m.leader_pos_id=c.leader_pos_id
			  AND m.status IN ('active','stopped_by_risk','detached')
		  )
		ORDER BY c.id`, traderID)
	if err != nil {
		return nil, err
	}
	var cycleIDs []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		cycleIDs = append(cycleIDs, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*CopyTradeOwnershipGap, 0, len(cycleIDs))
	for _, cycleID := range cycleIDs {
		gap, loadErr := loadCopyTradeOwnershipGap(s.db, traderID, cycleID)
		if loadErr != nil {
			return nil, loadErr
		}
		out = append(out, gap)
	}
	return out, nil
}

func loadCopyTradeOwnershipGap(q ownershipGapQueryer, traderID string, cycleID int64) (*CopyTradeOwnershipGap, error) {
	cycle, err := scanCopyGuardCycle(q.QueryRow(copyGuardCycleSelect+` WHERE id=? AND trader_id=? AND closed_at IS NULL`, cycleID, traderID))
	if err != nil {
		return nil, err
	}
	gap := &CopyTradeOwnershipGap{
		CycleID: cycle.ID, TraderID: cycle.TraderID, LeaderID: cycle.LeaderID,
		LeaderPosID: cycle.LeaderPosID, Symbol: cycle.Symbol, Side: strings.ToLower(cycle.Side),
		MarginMode: cycle.MarginMode, CycleStatus: cycle.Status, FollowerPosID: cycle.FollowerPosID,
		EntryOrderID: strings.TrimSpace(cycle.EntryOrderID), FollowerEntryPrice: cycle.FollowerEntryPrice,
		FollowerNotional: cycle.FollowerNotional, ReasonCode: OwnershipGapMappingMissing,
	}
	mapping, mappingErr := scanMapping(q.QueryRow(`SELECT `+mappingSelectColumns+`
		FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, cycle.LeaderPosID))
	if mappingErr == sql.ErrNoRows {
		return gap, nil
	}
	if mappingErr != nil {
		return nil, mappingErr
	}
	gap.MappingID = mapping.ID
	gap.MappingStatus = mapping.Status
	gap.MappingRevision = mapping.SourceRevision
	gap.SourceSymbol = mapping.SourceSymbol
	gap.ExecutionSymbol = mapping.ExecutionSymbol
	gap.SourceQuoteAsset = mapping.SourceQuoteAsset
	gap.ExecutionSettleAsset = mapping.ExecutionSettleAsset
	if mapping.Status != MappingStatusClosed {
		gap.ReasonCode = OwnershipGapMappingNotClosed
		return gap, nil
	}
	if cycle.Status != CopyGuardFollowing && cycle.Status != CopyGuardFollowingReentry {
		gap.ReasonCode = OwnershipGapCycleStateIncompatible
		return gap, nil
	}
	if !strings.EqualFold(mapping.Symbol, cycle.Symbol) || !strings.EqualFold(mapping.Side, cycle.Side) ||
		(mapping.MarginMode != "" && cycle.MarginMode != "" && !strings.EqualFold(mapping.MarginMode, cycle.MarginMode)) ||
		(mapping.LeaderID != "" && cycle.LeaderID != "" && mapping.LeaderID != cycle.LeaderID) {
		gap.ReasonCode = OwnershipGapIdentityMismatch
		return gap, nil
	}
	if gap.EntryOrderID == "" {
		gap.ReasonCode = OwnershipGapEntryOrderUnavailable
		return gap, nil
	}
	expectedAction := "open_long"
	if strings.EqualFold(cycle.Side, "short") {
		expectedAction = "open_short"
	}
	var intentSymbol, intentSide, intentMargin, intentOrder string
	err = q.QueryRow(`SELECT id,source_revision,COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),
		COALESCE(exchange_order_id,''),COALESCE(leader_target_size,0),COALESCE(filled_quantity,0)
		FROM copy_trade_execution_intents
		WHERE trader_id=? AND leader_pos_id=? AND source_kind='LEADER_TRANSITION'
		  AND action=? AND status IN ('FILLED','PROTECTED')
		  AND COALESCE(filled_quantity,0)>0 AND COALESCE(exchange_order_id,'')=?
		ORDER BY source_revision DESC,id DESC LIMIT 1`, traderID, cycle.LeaderPosID, expectedAction, gap.EntryOrderID).Scan(
		&gap.EntryIntentID, &gap.EntryIntentRevision, &intentSymbol, &intentSide, &intentMargin,
		&intentOrder, &gap.EntryIntentLeaderTarget, &gap.EntryIntentFilled,
	)
	if err == sql.ErrNoRows {
		gap.ReasonCode = OwnershipGapEntryIntentMissing
		return gap, nil
	}
	if err != nil {
		return nil, err
	}
	if intentOrder != gap.EntryOrderID || !strings.EqualFold(intentSymbol, cycle.Symbol) ||
		!strings.EqualFold(intentSide, cycle.Side) ||
		(intentMargin != "" && cycle.MarginMode != "" && !strings.EqualFold(intentMargin, cycle.MarginMode)) {
		gap.ReasonCode = OwnershipGapIdentityMismatch
		return gap, nil
	}
	executionSymbol := mapping.ExecutionSymbol
	if strings.TrimSpace(executionSymbol) == "" {
		executionSymbol = mapping.Symbol
	}
	err = q.QueryRow(`SELECT COALESCE(MIN(id),0),COALESCE(MAX(quantity),0)
		FROM trader_positions
		WHERE trader_id=? AND UPPER(symbol)=UPPER(?)
		  AND UPPER(side)=UPPER(?) AND entry_order_id=? AND quantity>0`,
		traderID, executionSymbol, cycle.Side, gap.EntryOrderID).Scan(&gap.LocalPositionID, &gap.LocalPositionQuantity)
	if err != nil {
		return nil, err
	}
	if gap.LocalPositionID <= 0 || gap.LocalPositionQuantity <= 0 {
		gap.ReasonCode = OwnershipGapLocalPositionMissing
		return gap, nil
	}
	gap.Recoverable = true
	gap.ReasonCode = OwnershipGapStrongEvidence
	return gap, nil
}

type supersedableOwnershipIntent struct {
	ID               int64
	Revision         int64
	Action           string
	Status           string
	ReasonCode       string
	LastError        string
	SubmittedAt      sql.NullString
	ExchangeOrderID  string
	FilledQuantity   float64
	LeaderTargetSize float64
	AttemptCount     int
	PositionConflict int
}

func loadSupersedableOwnershipIntents(tx *sql.Tx, traderID, leaderPosID string, afterRevision int64) ([]supersedableOwnershipIntent, error) {
	rows, err := tx.Query(`SELECT i.id,i.source_revision,i.action,i.status,COALESCE(i.reason_code,''),COALESCE(i.last_error,''),
		i.submitted_at,COALESCE(i.exchange_order_id,''),COALESCE(i.filled_quantity,0),COALESCE(i.leader_target_size,0),
		(SELECT COUNT(*) FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id),
		EXISTS(SELECT 1 FROM copy_trade_events e
			WHERE e.trader_id=i.trader_id AND e.leader_pos_id=i.leader_pos_id AND e.event_type='OPEN'
			  AND CAST(json_extract(e.detail_json,'$.intent_id') AS INTEGER)=i.id
			  AND json_extract(e.detail_json,'$.reason_code') IN ('POSITION_EXISTS_LOOKUP_PENDING','POSITION_EXISTS_LOOKUP_UNAVAILABLE'))
		FROM copy_trade_execution_intents i
		WHERE i.trader_id=? AND i.leader_pos_id=? AND i.source_kind='LEADER_TRANSITION'
		  AND i.source_revision>? ORDER BY i.source_revision,i.id`, traderID, leaderPosID, afterRevision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []supersedableOwnershipIntent
	for rows.Next() {
		var x supersedableOwnershipIntent
		if err = rows.Scan(&x.ID, &x.Revision, &x.Action, &x.Status, &x.ReasonCode, &x.LastError,
			&x.SubmittedAt, &x.ExchangeOrderID, &x.FilledQuantity, &x.LeaderTargetSize,
			&x.AttemptCount, &x.PositionConflict); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func ownershipIntentHasNoExchangeSideEffect(x supersedableOwnershipIntent) bool {
	if x.ExchangeOrderID != "" || x.FilledQuantity > 0 || x.AttemptCount != 0 {
		return false
	}
	switch x.Status {
	case ExecutionIntentReserved, ExecutionIntentSubmitted, ExecutionIntentReconciling, ExecutionIntentFailed:
	default:
		return false
	}
	if !x.SubmittedAt.Valid || strings.TrimSpace(x.SubmittedAt.String) == "" {
		return true
	}
	// Before durable-attempt persistence was made atomic, the integration wrote
	// SUBMITTED before AutoTrader's same-side position precheck. The historical
	// action event plus OKX 51603 prove that this canonical client id never
	// became an exchange order. Both are required; either signal alone is too
	// weak to retire a possibly submitted order.
	errText := strings.ToLower(x.LastError)
	return x.PositionConflict != 0 && strings.Contains(errText, "51603") && strings.Contains(errText, "order does not exist")
}

type ownershipIntentConsumption struct {
	Revision            int64
	HistoricalTarget    float64
	SupersededIntentIDs []int64
	SupersededEvents    []map[string]interface{}
}

// consumeSupersedableOwnershipIntentsTx advances only through contiguous
// source revisions that are proven never to have reached the exchange. The
// latest positive target is the last trustworthy source baseline before the
// ownership gap was repaired.
func consumeSupersedableOwnershipIntentsTx(tx *sql.Tx, gap *CopyTradeOwnershipGap) (*ownershipIntentConsumption, error) {
	if tx == nil || gap == nil {
		return nil, fmt.Errorf("invalid ownership intent consumption")
	}
	expectedAction := "open_long"
	if strings.EqualFold(gap.Side, "short") {
		expectedAction = "open_short"
	}
	pending, err := loadSupersedableOwnershipIntents(tx, gap.TraderID, gap.LeaderPosID, gap.MappingRevision)
	if err != nil {
		return nil, err
	}
	out := &ownershipIntentConsumption{
		Revision:         gap.MappingRevision,
		HistoricalTarget: gap.EntryIntentLeaderTarget,
	}
	for _, intent := range pending {
		if intent.Revision != out.Revision+1 {
			return nil, fmt.Errorf("%s: non-contiguous intent=%d revision=%d expected=%d", OwnershipGapUnsafeRevision, intent.ID, intent.Revision, out.Revision+1)
		}
		if intent.Action != expectedAction {
			return nil, fmt.Errorf("%s: intent=%d revision=%d action=%s", OwnershipGapUnsafeRevision, intent.ID, intent.Revision, intent.Action)
		}
		if !ownershipIntentHasNoExchangeSideEffect(intent) {
			return nil, fmt.Errorf("%s: intent=%d revision=%d status=%s", OwnershipGapUnsafeRevision, intent.ID, intent.Revision, intent.Status)
		}
		prior := strings.TrimSpace(intent.LastError)
		detail := "duplicate open superseded after confirmed follower ownership recovery"
		if prior != "" {
			detail += "; prior=" + prior
		}
		res, updateErr := tx.Exec(`UPDATE copy_trade_execution_intents SET
			status='SKIPPED',reason_code=?,last_error=?,terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
			WHERE id=? AND trader_id=? AND leader_pos_id=? AND source_revision=?`,
			OwnershipRecoveredReason, detail, intent.ID, gap.TraderID, gap.LeaderPosID, intent.Revision)
		if updateErr != nil {
			return nil, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, fmt.Errorf("ownership recovery lost intent race: %d", intent.ID)
		}
		if _, updateErr = tx.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, OwnershipRecoveredReason, intent.ID); updateErr != nil {
			return nil, updateErr
		}
		if _, updateErr = tx.Exec(`UPDATE copy_guard_risk_reservations SET status='RELEASED',released_at=CURRENT_TIMESTAMP WHERE intent_id=? AND status IN ('ACTIVE','CONSUMED')`, intent.ID); updateErr != nil {
			return nil, updateErr
		}
		metadata := normalizeCopyGuardEventMetadata(gap.CycleID, gap.TraderID, OwnershipRecoveredReason, map[string]interface{}{
			"reason_code": OwnershipRecoveredReason, "leader_pos_id": gap.LeaderPosID,
			"intent_id": intent.ID, "source_revision": intent.Revision,
			"prior_status": intent.Status, "prior_reason_code": intent.ReasonCode,
		})
		raw, _ := json.Marshal(metadata)
		if _, updateErr = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,metadata_json) VALUES(?,?,?,?)`,
			gap.CycleID, gap.TraderID, OwnershipRecoveredReason, string(raw)); updateErr != nil {
			return nil, updateErr
		}
		out.SupersededEvents = append(out.SupersededEvents, metadata)
		out.SupersededIntentIDs = append(out.SupersededIntentIDs, intent.ID)
		out.Revision = intent.Revision
		if intent.LeaderTargetSize > 0 {
			out.HistoricalTarget = intent.LeaderTargetSize
		}
	}
	if out.HistoricalTarget <= 0 {
		return nil, fmt.Errorf("%s: authoritative leader target is unavailable", OwnershipGapUnsafeRevision)
	}
	return out, nil
}

// RestoreConfirmedMappingOwnership atomically reopens the closed mapping and
// consumes only contiguous, proven-no-side-effect duplicate open intents. It
// does not place an order or manufacture a fill.
func (s *CopyTradeStore) RestoreConfirmedMappingOwnership(req RestoreMappingOwnershipRequest) (*RestoreMappingOwnershipResult, error) {
	if req.TraderID == "" || req.LeaderPosID == "" || req.CycleID <= 0 || req.EntryIntentID <= 0 || req.FollowerQuantity <= 0 ||
		req.CurrentLeaderSize < 0 || (req.LeaderStillOpen && req.CurrentLeaderSize <= 0) ||
		(req.LeaderStillOpen && req.LeaderReversed) {
		return nil, fmt.Errorf("invalid mapping ownership recovery")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	gap, err := loadCopyTradeOwnershipGap(tx, req.TraderID, req.CycleID)
	if err != nil {
		return nil, err
	}
	if !gap.Recoverable || gap.EntryIntentID != req.EntryIntentID || gap.LeaderPosID != req.LeaderPosID {
		return nil, fmt.Errorf("mapping ownership evidence changed: reason=%s", gap.ReasonCode)
	}
	consumed, err := consumeSupersedableOwnershipIntentsTx(tx, gap)
	if err != nil {
		return nil, err
	}
	currentLeaderSize := req.CurrentLeaderSize
	leaderStillOpen := req.LeaderStillOpen
	leaderTarget := consumed.HistoricalTarget
	if leaderStillOpen {
		leaderTarget = riskSafeRecoveryBaseline(consumed.HistoricalTarget, currentLeaderSize)
	}
	result := &RestoreMappingOwnershipResult{
		MappingRevision:        consumed.Revision,
		SupersededIntentIDs:    consumed.SupersededIntentIDs,
		HistoricalLeaderTarget: consumed.HistoricalTarget,
		RestoredLeaderBaseline: leaderTarget,
		PendingRiskReduction:   !leaderStillOpen || currentLeaderSize < consumed.HistoricalTarget,
	}
	res, err := tx.Exec(`UPDATE copy_trade_position_mappings SET
		leader_id=?,symbol=?,source_symbol=CASE WHEN COALESCE(source_symbol,'')='' THEN ? ELSE source_symbol END,
		execution_symbol=CASE WHEN COALESCE(execution_symbol,'')='' THEN ? ELSE execution_symbol END,
		source_revision=?,side=?,margin_mode=?,status='active',opened_at=(SELECT opened_at FROM copy_guard_cycles WHERE id=?),
		open_price=?,open_size_usd=?,last_known_size=?,closed_at=NULL,close_price=0,
		add_count=0,reduce_count=0,accumulated_reduce_ratio=0,consecutive_fail_count=0,
		last_failure_at=NULL,last_failure_reason='',stopped_at=NULL,leader_pnl_at_stop=0,
		leader_size_at_stop=0,add_count_at_stop=0,
		reentry_used=CASE WHEN (SELECT status FROM copy_guard_cycles WHERE id=?)=? THEN 1 ELSE 0 END,
		updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND trader_id=? AND leader_pos_id=? AND status='closed' AND COALESCE(source_revision,0)=?`,
		gap.LeaderID, gap.Symbol, gap.Symbol, gap.Symbol, consumed.Revision, gap.Side, gap.MarginMode, gap.CycleID,
		gap.FollowerEntryPrice, gap.FollowerNotional, leaderTarget,
		gap.CycleID, CopyGuardFollowingReentry,
		gap.MappingID, req.TraderID, req.LeaderPosID, gap.MappingRevision)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("ownership recovery lost mapping race")
	}
	if req.FollowerPosID != "" {
		if _, err = tx.Exec(`UPDATE copy_guard_cycles SET follower_pos_id=?,
			accounting_status=CASE WHEN accounting_error LIKE 'OWNERSHIP_AMBIGUOUS:%' THEN 'OPEN' ELSE accounting_status END,
			accounting_error=CASE WHEN accounting_error LIKE 'OWNERSHIP_AMBIGUOUS:%' THEN '' ELSE accounting_error END,
			updated_at=CURRENT_TIMESTAMP WHERE id=? AND closed_at IS NULL`, req.FollowerPosID, gap.CycleID); err != nil {
			return nil, err
		}
	} else if _, err = tx.Exec(`UPDATE copy_guard_cycles SET
		accounting_status=CASE WHEN accounting_error LIKE 'OWNERSHIP_AMBIGUOUS:%' THEN 'OPEN' ELSE accounting_status END,
		accounting_error=CASE WHEN accounting_error LIKE 'OWNERSHIP_AMBIGUOUS:%' THEN '' ELSE accounting_error END,
		updated_at=CURRENT_TIMESTAMP WHERE id=? AND closed_at IS NULL`, gap.CycleID); err != nil {
		return nil, err
	}
	metadata := normalizeCopyGuardEventMetadata(gap.CycleID, gap.TraderID, "MAPPING_OWNERSHIP_RECOVERED", map[string]interface{}{
		"leader_pos_id": gap.LeaderPosID, "entry_intent_id": gap.EntryIntentID,
		"entry_order_id": gap.EntryOrderID, "mapping_revision": consumed.Revision,
		"superseded_intent_ids":    result.SupersededIntentIDs,
		"follower_quantity":        req.FollowerQuantity,
		"historical_leader_target": consumed.HistoricalTarget,
		"leader_current_size":      currentLeaderSize, "leader_target_size": leaderTarget,
		"leader_reversed":        req.LeaderReversed,
		"pending_risk_reduction": result.PendingRiskReduction,
		"reason_code":            "MAPPING_OWNERSHIP_RECOVERED",
	})
	raw, _ := json.Marshal(metadata)
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,quantity,metadata_json) VALUES(?,?,?,?,?)`, gap.CycleID, gap.TraderID, "MAPPING_OWNERSHIP_RECOVERED", req.FollowerQuantity, string(raw)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	for _, eventMetadata := range consumed.SupersededEvents {
		s.mirrorGuardEventToCopyEvents(gap.CycleID, gap.TraderID, OwnershipRecoveredReason, 0, 0, 0, 0, eventMetadata)
	}
	s.mirrorGuardEventToCopyEvents(gap.CycleID, gap.TraderID, "MAPPING_OWNERSHIP_RECOVERED", 0, req.FollowerQuantity, gap.FollowerNotional, 0, metadata)
	return result, nil
}

// MarkCopyGuardOwnershipAmbiguous makes an unsafe ownership gap explicit while
// keeping the trading lifecycle open for later evidence/recovery. Repeated
// identical observations are idempotent and do not spam the event timeline.
func (s *CopyTradeStore) MarkCopyGuardOwnershipAmbiguous(cycleID int64, traderID, reasonCode, detail string) (bool, error) {
	reasonCode = strings.TrimSpace(reasonCode)
	detail = strings.TrimSpace(detail)
	if cycleID <= 0 || traderID == "" || reasonCode == "" {
		return false, fmt.Errorf("invalid ambiguous ownership gap")
	}
	message := "OWNERSHIP_AMBIGUOUS:" + reasonCode
	if detail != "" {
		message += ": " + detail
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var priorStatus, priorError string
	if err = tx.QueryRow(`SELECT accounting_status,COALESCE(accounting_error,'')
		FROM copy_guard_cycles WHERE id=? AND trader_id=? AND closed_at IS NULL`, cycleID, traderID).Scan(&priorStatus, &priorError); err != nil {
		return false, err
	}
	prefix := "OWNERSHIP_AMBIGUOUS:"
	priorCode := ""
	if strings.HasPrefix(priorError, prefix) {
		priorCode = strings.TrimSpace(strings.SplitN(strings.TrimPrefix(priorError, prefix), ":", 2)[0])
	}
	reasonChanged := priorStatus != CopyGuardAccountingUnscorable || priorCode != reasonCode
	if priorStatus == CopyGuardAccountingUnscorable && priorError == message {
		return false, tx.Commit()
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET accounting_status=?,accounting_error=?,updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND trader_id=? AND closed_at IS NULL`, CopyGuardAccountingUnscorable, message, cycleID, traderID); err != nil {
		return false, err
	}
	if !reasonChanged {
		return false, tx.Commit()
	}
	metadata := normalizeCopyGuardEventMetadata(cycleID, traderID, "OWNERSHIP_AMBIGUOUS", map[string]interface{}{
		"reason_code": reasonCode, "detail": detail,
	})
	raw, _ := json.Marshal(metadata)
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,metadata_json) VALUES(?,?,?,?)`, cycleID, traderID, "OWNERSHIP_AMBIGUOUS", string(raw)); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	s.mirrorGuardEventToCopyEvents(cycleID, traderID, "OWNERSHIP_AMBIGUOUS", 0, 0, 0, 0, metadata)
	return true, nil
}

// ResolveConfirmedOwnershipGapFlat consumes only safe duplicate-open intents
// after the caller has freshly proved the follower position is flat and every
// protective order is terminal. A still-open leader becomes detached so the
// old lifecycle cannot be reopened; a closed leader remains open only long
// enough for the normal LEADER_CLOSED accounting path to finish it.
func (s *CopyTradeStore) ResolveConfirmedOwnershipGapFlat(req ResolveFlatOwnershipGapRequest) (*RestoreMappingOwnershipResult, error) {
	if req.TraderID == "" || req.LeaderPosID == "" || req.CycleID <= 0 || req.EntryIntentID <= 0 ||
		req.CurrentLeaderSize < 0 || (req.LeaderStillOpen && req.CurrentLeaderSize <= 0) ||
		(req.LeaderStillOpen && req.LeaderReversed) {
		return nil, fmt.Errorf("invalid flat ownership gap resolution")
	}
	if strings.TrimSpace(req.ReasonCode) == "" {
		req.ReasonCode = OwnershipGapFollowerAlreadyFlat
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	gap, err := loadCopyTradeOwnershipGap(tx, req.TraderID, req.CycleID)
	if err != nil {
		return nil, err
	}
	if !gap.Recoverable || gap.EntryIntentID != req.EntryIntentID || gap.LeaderPosID != req.LeaderPosID {
		return nil, fmt.Errorf("flat ownership evidence changed: reason=%s", gap.ReasonCode)
	}
	consumed, err := consumeSupersedableOwnershipIntentsTx(tx, gap)
	if err != nil {
		return nil, err
	}
	result := &RestoreMappingOwnershipResult{
		MappingRevision:        consumed.Revision,
		SupersededIntentIDs:    consumed.SupersededIntentIDs,
		HistoricalLeaderTarget: consumed.HistoricalTarget,
	}
	terminalStatus := CopyGuardLeaderClosed
	if req.LeaderReversed {
		terminalStatus = CopyGuardLeaderReversed
	}
	if req.LeaderStillOpen {
		terminalStatus = CopyGuardDetached
		result.RestoredLeaderBaseline = req.CurrentLeaderSize
		res, updateErr := tx.Exec(`UPDATE copy_trade_position_mappings SET
			status='detached',source_revision=?,last_known_size=?,closed_at=NULL,close_price=0,
			last_failure_reason=?,consecutive_fail_count=0,updated_at=CURRENT_TIMESTAMP
			WHERE id=? AND trader_id=? AND leader_pos_id=? AND status='closed' AND COALESCE(source_revision,0)=?`,
			consumed.Revision, req.CurrentLeaderSize, req.ReasonCode,
			gap.MappingID, req.TraderID, req.LeaderPosID, gap.MappingRevision)
		if updateErr != nil {
			return nil, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, fmt.Errorf("flat ownership resolution lost mapping race")
		}
		if _, err = tx.Exec(`UPDATE copy_guard_cycles SET
			status=?,accounting_status=?,accounting_error=?,protection_status='CANCELED',protection_coverage=0,
			closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
			WHERE id=? AND trader_id=? AND closed_at IS NULL`,
			CopyGuardDetached, CopyGuardAccountingUnscorable, req.ReasonCode, req.CycleID, req.TraderID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`UPDATE copy_guard_attempts SET status='DETACHED',closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP)
			WHERE cycle_id=? AND status='OPEN'`, req.CycleID); err != nil {
			return nil, err
		}
	} else {
		res, updateErr := tx.Exec(`UPDATE copy_trade_position_mappings SET
			source_revision=?,last_known_size=0,updated_at=CURRENT_TIMESTAMP
			WHERE id=? AND trader_id=? AND leader_pos_id=? AND status='closed' AND COALESCE(source_revision,0)=?`,
			consumed.Revision, gap.MappingID, req.TraderID, req.LeaderPosID, gap.MappingRevision)
		if updateErr != nil {
			return nil, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, fmt.Errorf("flat ownership resolution lost mapping race")
		}
		if _, err = tx.Exec(`UPDATE copy_guard_cycles SET protection_status='CANCELED',protection_coverage=0,updated_at=CURRENT_TIMESTAMP
			WHERE id=? AND trader_id=? AND closed_at IS NULL`, req.CycleID, req.TraderID); err != nil {
			return nil, err
		}
	}
	if err = terminalizeCopyGuardAuxiliaryStateTx(tx, req.CycleID, terminalStatus); err != nil {
		return nil, err
	}
	metadata := normalizeCopyGuardEventMetadata(req.CycleID, req.TraderID, "OWNERSHIP_GAP_FLAT_RETIRED", map[string]interface{}{
		"reason_code": req.ReasonCode, "leader_pos_id": req.LeaderPosID,
		"leader_open": req.LeaderStillOpen, "leader_current_size": req.CurrentLeaderSize,
		"leader_reversed":  req.LeaderReversed,
		"mapping_revision": consumed.Revision, "superseded_intent_ids": consumed.SupersededIntentIDs,
	})
	raw, _ := json.Marshal(metadata)
	eventRes, err := tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,metadata_json)
		SELECT ?,?,?,? WHERE NOT EXISTS (
			SELECT 1 FROM copy_guard_events WHERE cycle_id=? AND type='OWNERSHIP_GAP_FLAT_RETIRED'
		)`, req.CycleID, req.TraderID, "OWNERSHIP_GAP_FLAT_RETIRED", string(raw), req.CycleID)
	if err != nil {
		return nil, err
	}
	eventInserted, _ := eventRes.RowsAffected()
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	for _, eventMetadata := range consumed.SupersededEvents {
		s.mirrorGuardEventToCopyEvents(gap.CycleID, gap.TraderID, OwnershipRecoveredReason, 0, 0, 0, 0, eventMetadata)
	}
	if eventInserted == 1 {
		s.mirrorGuardEventToCopyEvents(req.CycleID, req.TraderID, "OWNERSHIP_GAP_FLAT_RETIRED", 0, 0, 0, 0, metadata)
	}
	return result, nil
}
