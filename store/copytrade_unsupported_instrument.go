package store

import "time"

type UnsupportedExecutionInstrument struct {
	LeaderPosID    string    `json:"leader_pos_id"`
	SourceSymbol   string    `json:"source_symbol"`
	ExecutionVenue string    `json:"execution_venue,omitempty"`
	Reason         string    `json:"reason"`
	LastSeenAt     time.Time `json:"last_seen_at"`
}

func (s *CopyTradeStore) initUnsupportedExecutionInstrumentTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_unsupported_instruments (
			trader_id TEXT NOT NULL,
			leader_pos_id TEXT NOT NULL,
			source_symbol TEXT NOT NULL,
			execution_venue TEXT DEFAULT '',
			reason TEXT DEFAULT '',
			last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY(trader_id, leader_pos_id)
		)
	`)
	return err
}

func (s *CopyTradeStore) MarkUnsupportedExecutionInstrument(traderID, leaderPosID, sourceSymbol, venue, reason string) error {
	_, err := s.db.Exec(`
		INSERT INTO copy_trade_unsupported_instruments
			(trader_id,leader_pos_id,source_symbol,execution_venue,reason,last_seen_at)
		VALUES(?,?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET
			source_symbol=excluded.source_symbol,execution_venue=excluded.execution_venue,
			reason=excluded.reason,last_seen_at=CURRENT_TIMESTAMP
	`, traderID, leaderPosID, sourceSymbol, venue, reason)
	return err
}

func (s *CopyTradeStore) ClearUnsupportedExecutionInstrument(traderID, leaderPosID string) error {
	_, err := s.db.Exec(`DELETE FROM copy_trade_unsupported_instruments WHERE trader_id=? AND leader_pos_id=?`, traderID, leaderPosID)
	return err
}

// ListUnsupportedExecutionInstruments returns only identities that still have
// a live/ignored source mapping. Closed source positions disappear from the
// current health view without deleting historical events.
func (s *CopyTradeStore) ListUnsupportedExecutionInstruments(traderID string) ([]UnsupportedExecutionInstrument, error) {
	rows, err := s.db.Query(`
		SELECT u.leader_pos_id,u.source_symbol,COALESCE(u.execution_venue,''),COALESCE(u.reason,''),u.last_seen_at
		FROM copy_trade_unsupported_instruments u
		JOIN copy_trade_position_mappings m
		  ON m.trader_id=u.trader_id AND m.leader_pos_id=u.leader_pos_id
		WHERE u.trader_id=? AND m.status IN ('active','ignored','stopped_by_risk')
		ORDER BY u.last_seen_at DESC
	`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UnsupportedExecutionInstrument, 0)
	for rows.Next() {
		var item UnsupportedExecutionInstrument
		if err := rows.Scan(&item.LeaderPosID, &item.SourceSymbol, &item.ExecutionVenue, &item.Reason, &item.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
