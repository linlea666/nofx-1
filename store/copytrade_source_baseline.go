package store

import (
	"database/sql"
	"fmt"
)

// CopyTradeBaselinePosition is the minimum source-position identity required
// to establish or recover a no-chase baseline.
type CopyTradeBaselinePosition struct {
	LeaderPosID string
	Symbol      string
	Side        string
	MarginMode  string
	Size        float64
}

func (s *CopyTradeStore) initSourceBaselineTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_source_baselines (
			trader_id TEXT NOT NULL,
			leader_id TEXT NOT NULL,
			source_mode TEXT NOT NULL,
			source_generation INTEGER NOT NULL,
			completed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY(trader_id, source_generation)
		)
	`)
	return err
}

func (s *CopyTradeStore) IsSourceBaselineComplete(traderID, leaderID, sourceMode string, generation int) (bool, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(1) FROM copy_trade_source_baselines
		WHERE trader_id=? AND leader_id=? AND source_mode=? AND source_generation=?
	`, traderID, leaderID, sourceMode, generation).Scan(&count)
	return count > 0, err
}

// InitializeSourceBaseline writes every ignored source position and the
// generation-level completion marker in one transaction. A partial baseline
// is more dangerous than no baseline, so any conflict/error rolls everything
// back and startup fails closed.
func (s *CopyTradeStore) InitializeSourceBaseline(traderID, leaderID, sourceMode string, generation int, positions []CopyTradeBaselinePosition) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, position := range positions {
		if position.LeaderPosID == "" || position.Symbol == "" || position.Size < 0 {
			return fmt.Errorf("invalid source baseline position: %+v", position)
		}
		var status string
		err := tx.QueryRow(`SELECT status FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, position.LeaderPosID).Scan(&status)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && (status == MappingStatusActive || status == MappingStatusStoppedByRisk) {
			return fmt.Errorf("cannot replace live mapping %s while initializing source generation %d", position.LeaderPosID, generation)
		}
		_, err = tx.Exec(`
			INSERT INTO copy_trade_position_mappings
				(trader_id,leader_pos_id,leader_id,symbol,source_revision,side,margin_mode,status,opened_at,open_price,open_size_usd,last_known_size,add_count,reduce_count,updated_at)
			VALUES(?,?,?,?,1,?,?,'ignored',CURRENT_TIMESTAMP,0,0,?,0,0,CURRENT_TIMESTAMP)
			ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET
				leader_id=excluded.leader_id,symbol=excluded.symbol,side=excluded.side,
				margin_mode=excluded.margin_mode,status='ignored',last_known_size=excluded.last_known_size,
				source_revision=COALESCE(copy_trade_position_mappings.source_revision, 0) + 1,
				closed_at=NULL,close_price=0,updated_at=CURRENT_TIMESTAMP
		`, traderID, position.LeaderPosID, leaderID, position.Symbol, position.Side, position.MarginMode, position.Size)
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO copy_trade_source_baselines(trader_id,leader_id,source_mode,source_generation,completed_at)
		VALUES(?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(trader_id,source_generation) DO UPDATE SET
			leader_id=excluded.leader_id,source_mode=excluded.source_mode,completed_at=CURRENT_TIMESTAMP
	`, traderID, leaderID, sourceMode, generation); err != nil {
		return err
	}
	return tx.Commit()
}

// RebaselineSourceRecovery atomically absorbs only risk-increasing changes
// seen after a visibility/staleness gap. Reductions are intentionally left at
// their previous LastKnownSize so the next diff remains actionable.
func (s *CopyTradeStore) RebaselineSourceRecovery(traderID, leaderID string, positions []CopyTradeBaselinePosition) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, position := range positions {
		if position.LeaderPosID == "" || position.Symbol == "" || position.Size <= 0 {
			return fmt.Errorf("invalid recovery baseline position: %+v", position)
		}
		var status string
		var lastKnown float64
		err := tx.QueryRow(`SELECT status,last_known_size FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, position.LeaderPosID).Scan(&status, &lastKnown)
		switch {
		case err == sql.ErrNoRows || status == MappingStatusClosed:
			_, err = tx.Exec(`
				INSERT INTO copy_trade_position_mappings
					(trader_id,leader_pos_id,leader_id,symbol,source_revision,side,margin_mode,status,opened_at,open_price,open_size_usd,last_known_size,add_count,reduce_count,updated_at)
				VALUES(?,?,?,?,1,?,?,'ignored',CURRENT_TIMESTAMP,0,0,?,0,0,CURRENT_TIMESTAMP)
				ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET
					leader_id=excluded.leader_id,symbol=excluded.symbol,side=excluded.side,
					margin_mode=excluded.margin_mode,status='ignored',last_known_size=excluded.last_known_size,
					source_revision=COALESCE(copy_trade_position_mappings.source_revision, 0) + 1,
					closed_at=NULL,close_price=0,updated_at=CURRENT_TIMESTAMP
			`, traderID, position.LeaderPosID, leaderID, position.Symbol, position.Side, position.MarginMode, position.Size)
		case err != nil:
			return err
		case status == MappingStatusActive && position.Size > lastKnown:
			_, err = tx.Exec(`UPDATE copy_trade_position_mappings SET last_known_size=?,source_revision=COALESCE(source_revision, 0) + 1,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='active'`, position.Size, traderID, position.LeaderPosID)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
