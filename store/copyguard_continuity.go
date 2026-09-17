package store

import (
	"fmt"
	"math"
	"time"
)

type CopyGuardContinuityFill struct {
	TradeID, OrderID, Side, PositionSide string
	Quantity                             float64
	TimeMS                               int64
}

func (s *CopyTradeStore) CopyGuardContinuityStart(cycleID int64, attempt int, entryID string, opened time.Time) (time.Time, error) {
	var latest int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(checked_at_ms),0) FROM copy_guard_continuity_checks WHERE cycle_id=? AND attempt_no=? AND EXISTS(SELECT 1 FROM copy_guard_continuity_fills WHERE cycle_id=? AND attempt_no=? AND order_id=?)`, cycleID, attempt, cycleID, attempt, entryID).Scan(&latest)
	start := opened.Add(-2 * time.Second)
	// Overlap prevents a temporarily delayed fill from being lost at a page boundary.
	if recent := time.UnixMilli(latest).Add(-2 * time.Minute); latest > 0 && recent.After(start) {
		start = recent
	}
	return start, err
}

func (s *CopyTradeStore) MergeCopyGuardContinuityFills(cycleID int64, attempt int, checkedAt time.Time, fills []CopyGuardContinuityFill) ([]CopyGuardContinuityFill, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if checkedAt.IsZero() {
		return nil, fmt.Errorf("continuity observation time unavailable")
	}
	for _, f := range fills {
		if f.TradeID == "" || f.OrderID == "" || invalidPositiveNumber(f.Quantity) || f.TimeMS <= 0 {
			return nil, fmt.Errorf("invalid position continuity fill")
		}
		_, err = tx.Exec(`INSERT OR IGNORE INTO copy_guard_continuity_fills(cycle_id,attempt_no,trade_id,order_id,side,position_side,quantity,time_ms) VALUES(?,?,?,?,?,?,?,?)`, cycleID, attempt, f.TradeID, f.OrderID, f.Side, f.PositionSide, f.Quantity, f.TimeMS)
		if err != nil {
			return nil, err
		}
		var old CopyGuardContinuityFill
		err = tx.QueryRow(`SELECT order_id,side,position_side,quantity,time_ms FROM copy_guard_continuity_fills WHERE cycle_id=? AND attempt_no=? AND trade_id=?`, cycleID, attempt, f.TradeID).Scan(&old.OrderID, &old.Side, &old.PositionSide, &old.Quantity, &old.TimeMS)
		if err != nil {
			return nil, err
		}
		if old.OrderID != f.OrderID || old.Side != f.Side || old.PositionSide != f.PositionSide || math.Abs(old.Quantity-f.Quantity) > 1e-12 || old.TimeMS != f.TimeMS {
			return nil, fmt.Errorf("immutable continuity fill changed: %s", f.TradeID)
		}
	}
	rows, err := tx.Query(`SELECT trade_id,order_id,side,position_side,quantity,time_ms FROM copy_guard_continuity_fills WHERE cycle_id=? AND attempt_no=? ORDER BY time_ms,trade_id`, cycleID, attempt)
	if err != nil {
		return nil, err
	}
	var result []CopyGuardContinuityFill
	for rows.Next() {
		var f CopyGuardContinuityFill
		if err = rows.Scan(&f.TradeID, &f.OrderID, &f.Side, &f.PositionSide, &f.Quantity, &f.TimeMS); err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO copy_guard_continuity_checks(cycle_id,attempt_no,checked_at_ms) VALUES(?,?,?) ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET checked_at_ms=MAX(checked_at_ms,excluded.checked_at_ms)`, cycleID, attempt, checkedAt.UnixMilli()); err != nil {
		return nil, err
	}
	return result, tx.Commit()
}
