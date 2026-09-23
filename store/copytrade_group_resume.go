package store

import (
	"fmt"
	"math"
	"time"
)

// ResumeFollowGroup atomically absorbs ALL source movements while paused.
// The caller proves surviving original follower continuity and source identity.
// Pausing never erases the separate no-buyback gate.
func (s *CopyTradeStore) ResumeFollowGroup(traderID string, groupID, version int64, targets map[string]float64) error {
	if len(targets) == 0 {
		return fmt.Errorf("source group has ended")
	}
	for _, v := range targets {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("invalid source group resume baseline")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var valid bool
	var leaderID string
	if err = tx.QueryRow(`SELECT leader_id FROM copy_trade_follow_groups WHERE id=?`, groupID).Scan(&leaderID); err != nil {
		return err
	}
	if err = tx.QueryRow(`SELECT trader_id=? AND paused AND NOT ignored AND NOT source_ended AND conflict_reason='' AND version=? FROM copy_trade_follow_groups WHERE id=?`, traderID, version, groupID).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrFollowControlChanged
	}
	// Full membership is mandatory, including zero-size members. A partial
	// request must not clear pause while leaving old source baselines behind.
	rows, err := tx.Query(`SELECT leader_pos_id FROM copy_trade_follow_group_members WHERE group_id=?`, groupID)
	if err != nil {
		return err
	}
	memberCount := 0
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if _, ok := targets[id]; !ok {
			rows.Close()
			return fmt.Errorf("missing source member resume baseline")
		}
		memberCount++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if memberCount != len(targets) {
		return fmt.Errorf("unexpected source member resume baseline")
	}
	// Reuse order-by-order acknowledgement proof. Aggregate quantity alone can
	// conceal one unbooked fill behind another order's bookkeeping.
	rows, err = tx.Query(`SELECT id FROM copy_trade_execution_intents WHERE trader_id=? AND source_kind='LEADER_TRANSITION' AND leader_pos_id IN(SELECT leader_pos_id FROM copy_trade_follow_group_members WHERE group_id=?) AND status IN('RESERVED','SUBMITTED','PARTIALLY_FILLED','RECONCILING')`, traderID, groupID)
	if err != nil {
		return err
	}
	var pendingIDs []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		pendingIDs = append(pendingIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range pendingIDs {
		facts, e := inspectLeaderTransitionTx(tx, id)
		if e != nil {
			return e
		}
		if facts.Effect == SourceEffectUnresolved {
			return ErrSourceTransitionUnresolved
		}
		if facts.Resolution != "" {
			continue
		}
		disposition := SourceSupersedeNoFill
		if facts.Effect == SourceEffectBookedFill {
			disposition = SourceAcknowledgeFill
		}
		if err = finishLeaderSourceTransitionTx(tx, facts, FinishLeaderSourceTransitionRequest{IntentID: id, TraderID: traderID, LeaderID: leaderID, Disposition: disposition, Reason: "MANUAL_RESUME_BASELINE", Evidence: "whole group pause resumed; terminal per-order venue evidence verified; current source image becomes baseline"}); err != nil {
			return err
		}
	}
	for id, qty := range targets {
		if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET status=CASE WHEN ?=0 THEN 'closed' WHEN EXISTS(SELECT 1 FROM copy_trade_follow_groups WHERE id=? AND risk_blocked) THEN 'stopped_by_risk' WHEN EXISTS(SELECT 1 FROM copy_trade_position_custody p WHERE p.trader_id=? AND p.leader_pos_id=? AND p.state='RELEASED') THEN 'detached' ELSE 'active' END,last_known_size=?,source_revision=source_revision+1,stopped_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='manual_stopped' AND leader_pos_id IN(SELECT leader_pos_id FROM copy_trade_follow_group_members WHERE group_id=?)`, qty, groupID, traderID, id, qty, traderID, id, groupID); err != nil {
			return err
		}
		if err = bumpFollowControlTx(tx, traderID, id); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE copy_trade_follow_controls SET resume_after=? WHERE trader_id=? AND leader_pos_id=?`, time.Now().UTC().Format(time.RFC3339Nano), traderID, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE copy_trade_follow_groups SET paused=0,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE id=?`, groupID); err != nil {
		return err
	}
	return tx.Commit()
}
