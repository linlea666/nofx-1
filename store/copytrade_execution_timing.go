package store

// ExchangeFilledMS stays zero when the venue has not supplied a confirmed
// fill timestamp. Local acknowledgement time must never stand in for it.
type ExecutionTiming struct {
	SourceSnapshotMS int64 `json:"source_snapshot_ms"`
	SignalObservedMS int64 `json:"signal_observed_ms"`
	FirstSubmittedMS int64 `json:"first_submitted_ms"`
	ExchangeFilledMS int64 `json:"exchange_filled_ms"`
}

func (s *CopyTradeStore) RecordExchangeFillTime(intentID, ms int64) error {
	if intentID <= 0 || ms <= 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO copy_trade_execution_timings(intent_id,exchange_filled_ms) VALUES(?,?) ON CONFLICT(intent_id) DO UPDATE SET exchange_filled_ms=MAX(exchange_filled_ms,excluded.exchange_filled_ms)`, intentID, ms)
	return err
}
func (s *CopyTradeStore) GetExecutionTiming(intentID int64) (*ExecutionTiming, error) {
	var t ExecutionTiming
	err := s.db.QueryRow(`SELECT source_snapshot_ms,signal_observed_ms,first_submitted_ms,exchange_filled_ms FROM copy_trade_execution_timings WHERE intent_id=?`, intentID).Scan(&t.SourceSnapshotMS, &t.SignalObservedMS, &t.FirstSubmittedMS, &t.ExchangeFilledMS)
	return &t, err
}
