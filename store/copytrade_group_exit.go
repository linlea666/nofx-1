package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

type FollowGroupExitMember struct {
	LeaderPosID  string  `json:"leader_pos_id"`
	Revision     int64   `json:"revision"` // acknowledged revision BEFORE this batch
	TargetSize   float64 `json:"target_size"`
	SourceClosed bool    `json:"source_closed"`
}

// One successful snapshot gives one frozen reduction, even when several
// members reduced. Applying their percentages to successive residuals is wrong.
type FollowGroupExitBatch struct {
	GroupID       int64                   `json:"group_id"`
	MainPosID     string                  `json:"main_pos_id"`
	Ratio         float64                 `json:"ratio"`
	FullGroupExit bool                    `json:"full_group_exit"`
	Members       []FollowGroupExitMember `json:"members"`
}

func getFollowGroupExitBatchTx(q interface {
	QueryRow(string, ...interface{}) *sql.Row
}, intentID int64) (*FollowGroupExitBatch, error) {
	var raw string
	if err := q.QueryRow(`SELECT batch_json FROM copy_trade_follow_group_exits WHERE intent_id=?`, intentID).Scan(&raw); err != nil {
		return nil, err
	}
	var b FollowGroupExitBatch
	err := json.Unmarshal([]byte(raw), &b)
	return &b, err
}

func (s *CopyTradeStore) GetFollowGroupExitBatch(intentID int64) (*FollowGroupExitBatch, error) {
	return getFollowGroupExitBatchTx(s.db, intentID)
}

func (s *CopyTradeStore) BindFollowGroupExitBatch(intentID int64, b FollowGroupExitBatch) error {
	if b.GroupID <= 0 || b.MainPosID == "" || len(b.Members) == 0 || b.Ratio <= 0 || b.Ratio > 1 || math.IsNaN(b.Ratio) {
		return fmt.Errorf("invalid follow group exit batch")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = getFollowGroupExitBatchTx(tx, intentID); err == nil {
		return tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var traderID, posID string
	var revision int64
	if err = tx.QueryRow(`SELECT trader_id,leader_pos_id,source_revision FROM copy_trade_execution_intents WHERE id=?`, intentID).Scan(&traderID, &posID, &revision); err != nil {
		return err
	}
	if posID != b.MainPosID {
		return fmt.Errorf("group batch primary source changed")
	}
	for _, m := range b.Members {
		var r int64
		if err = tx.QueryRow(`SELECT source_revision FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, m.LeaderPosID).Scan(&r); err != nil {
			return err
		}
		if r != m.Revision || m.TargetSize < 0 || math.IsNaN(m.TargetSize) || math.IsInf(m.TargetSize, 0) {
			return fmt.Errorf("group member source revision changed")
		}
		if m.LeaderPosID == posID && revision != r+1 {
			return fmt.Errorf("group primary revision changed")
		}
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO copy_trade_follow_group_exits(intent_id,group_id,full_group_exit,batch_json) VALUES(?,?,?,?)`, intentID, b.GroupID, b.FullGroupExit, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

// completeFollowGroupExitTx is part of CompleteLeaderExit's transaction. It
// acknowledges every member using the same physical execution and never
// creates duplicate business intents or invents fills for sibling members.
func completeFollowGroupExitTx(tx *sql.Tx, intentID int64) (bool, bool, error) {
	b, err := getFollowGroupExitBatchTx(tx, intentID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return true, false, err
	}
	var traderID string
	if err = tx.QueryRow(`SELECT trader_id FROM copy_trade_follow_groups WHERE id=?`, b.GroupID).Scan(&traderID); err != nil {
		return true, b.FullGroupExit, err
	}
	for _, m := range b.Members {
		var ack int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_trade_follow_group_member_acks WHERE intent_id=? AND leader_pos_id=?`, intentID, m.LeaderPosID).Scan(&ack); err != nil {
			return true, b.FullGroupExit, err
		}
		if ack > 0 {
			continue
		}
		var revision int64
		if err = tx.QueryRow(`SELECT source_revision FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, m.LeaderPosID).Scan(&revision); err != nil {
			return true, b.FullGroupExit, err
		}
		if revision != m.Revision && (m.LeaderPosID != b.MainPosID || revision != m.Revision+1) {
			return true, b.FullGroupExit, fmt.Errorf("group member changed before exit acknowledgement")
		}
		if m.LeaderPosID != b.MainPosID {
			if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET source_revision=?,last_known_size=?,status=CASE WHEN ? AND status<>'manual_stopped' THEN 'closed' ELSE status END,closed_at=CASE WHEN ? AND status<>'manual_stopped' THEN CURRENT_TIMESTAMP ELSE closed_at END,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=?`, m.Revision+1, m.TargetSize, m.SourceClosed, m.SourceClosed, traderID, m.LeaderPosID); err != nil {
				return true, b.FullGroupExit, err
			}
		}
		if _, err = tx.Exec(`INSERT INTO copy_trade_follow_group_member_acks(intent_id,leader_pos_id,source_revision,target_size) VALUES(?,?,?,?)`, intentID, m.LeaderPosID, m.Revision+1, m.TargetSize); err != nil {
			return true, b.FullGroupExit, err
		}
		if _, err = tx.Exec(`UPDATE copy_trade_follow_group_members SET source_closed=? WHERE group_id=? AND leader_pos_id=?`, m.SourceClosed, b.GroupID, m.LeaderPosID); err != nil {
			return true, b.FullGroupExit, err
		}
	}
	if b.FullGroupExit {
		if _, err = tx.Exec(`UPDATE copy_trade_follow_groups SET source_ended=1,updated_at=CURRENT_TIMESTAMP WHERE id=?`, b.GroupID); err != nil {
			return true, true, err
		}
		if _, err = tx.Exec(`UPDATE copy_trade_position_custody SET state='RELEASED',reason='LEADER_GROUP_CLOSE',released_at=COALESCE(released_at,CURRENT_TIMESTAMP) WHERE trader_id=? AND leader_pos_id IN(SELECT leader_pos_id FROM copy_trade_follow_group_members WHERE group_id=?)`, traderID, b.GroupID); err != nil {
			return true, true, err
		}
	}
	_, err = tx.Exec(`UPDATE copy_trade_follow_group_exits SET completed=1 WHERE intent_id=?`, intentID)
	return true, b.FullGroupExit, err
}

func (s *CopyTradeStore) CompletedLeaderExitsPendingProtection(traderID string) ([]*LeaderExitPlan, error) {
	rows, err := s.db.Query(`SELECT e.plan_json FROM copy_trade_leader_exits e WHERE e.trader_id=? AND e.completed=1 AND (
 EXISTS(SELECT 1 FROM copy_guard_cycles c WHERE c.trader_id=e.trader_id AND c.leader_pos_id=e.leader_pos_id AND c.closed_at IS NULL AND json_extract(e.plan_json,'$.source_closed')=1 AND NOT EXISTS(SELECT 1 FROM copy_trade_follow_group_exits b WHERE b.intent_id=e.intent_id))
 OR EXISTS(SELECT 1 FROM copy_trade_follow_group_exits b JOIN copy_trade_follow_group_guards bg ON bg.group_id=b.group_id JOIN copy_guard_cycles c ON c.id=bg.cycle_id WHERE b.intent_id=e.intent_id AND b.full_group_exit=1 AND c.closed_at IS NULL)) ORDER BY e.intent_id LIMIT 100`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LeaderExitPlan
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var p LeaderExitPlan
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		p.Completed = true
		out = append(out, &p)
	}
	return out, rows.Err()
}
