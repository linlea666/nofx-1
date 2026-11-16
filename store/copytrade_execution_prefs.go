package store

import (
	"database/sql"
	"fmt"
	"math"
)

// Leader-relative execution preferences of a copy trader. They live in their
// own table so the wide copy_trade_configs statements stay untouched. A nil
// field means "not provided": the stored value (or the default) is kept.
const (
	maxCopyPriceTolerancePct      = 5.0
	maxCopyTakeProfitWaitSeconds  = 3600
	defaultCopyAutoCorrectEnabled = true
)

func (s *CopyTradeStore) initExecutionPrefsTable() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_trade_execution_prefs (
	 trader_id TEXT PRIMARY KEY,
	 price_tolerance_major_pct REAL,
	 price_tolerance_alt_pct REAL,
	 take_profit_max_wait_seconds INTEGER NOT NULL DEFAULT 0,
	 auto_correct_enabled INTEGER NOT NULL DEFAULT 1,
	 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	return err
}

// ValidateExecutionPrefs rejects out-of-range values before anything is saved.
func ValidateExecutionPrefs(c *CopyTradeConfig) error {
	for name, v := range map[string]*float64{"BTC/ETH 价格偏移阈值": c.CopyPriceToleranceMajorPct, "山寨币价格偏移阈值": c.CopyPriceToleranceAltPct} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > maxCopyPriceTolerancePct) {
			return fmt.Errorf("%s需在 0%% 到 %.0f%% 之间", name, maxCopyPriceTolerancePct)
		}
	}
	if v := c.CopyTakeProfitMaxWaitSeconds; v != nil && (*v < 0 || *v > maxCopyTakeProfitWaitSeconds) {
		return fmt.Errorf("止盈减仓最长等待需在 0 到 %d 秒之间", maxCopyTakeProfitWaitSeconds)
	}
	return nil
}

func saveExecutionPrefsTx(tx *sql.Tx, c *CopyTradeConfig) error {
	if c.CopyPriceToleranceMajorPct == nil && c.CopyPriceToleranceAltPct == nil &&
		c.CopyTakeProfitMaxWaitSeconds == nil && c.CopyAutoCorrectOverweight == nil {
		return nil
	}
	if err := ValidateExecutionPrefs(c); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO copy_trade_execution_prefs(trader_id) VALUES(?) ON CONFLICT(trader_id) DO NOTHING`, c.TraderID); err != nil {
		return err
	}
	if v := c.CopyPriceToleranceMajorPct; v != nil {
		if _, err := tx.Exec(`UPDATE copy_trade_execution_prefs SET price_tolerance_major_pct=?,updated_at=CURRENT_TIMESTAMP WHERE trader_id=?`, *v, c.TraderID); err != nil {
			return err
		}
	}
	if v := c.CopyPriceToleranceAltPct; v != nil {
		if _, err := tx.Exec(`UPDATE copy_trade_execution_prefs SET price_tolerance_alt_pct=?,updated_at=CURRENT_TIMESTAMP WHERE trader_id=?`, *v, c.TraderID); err != nil {
			return err
		}
	}
	if v := c.CopyTakeProfitMaxWaitSeconds; v != nil {
		if _, err := tx.Exec(`UPDATE copy_trade_execution_prefs SET take_profit_max_wait_seconds=?,updated_at=CURRENT_TIMESTAMP WHERE trader_id=?`, *v, c.TraderID); err != nil {
			return err
		}
	}
	if v := c.CopyAutoCorrectOverweight; v != nil {
		if _, err := tx.Exec(`UPDATE copy_trade_execution_prefs SET auto_correct_enabled=?,updated_at=CURRENT_TIMESTAMP WHERE trader_id=?`, *v, c.TraderID); err != nil {
			return err
		}
	}
	return nil
}

// loadExecutionPrefs fills the preference fields; absent values stay nil
// except auto-correction, which reports its default (on).
func (s *CopyTradeStore) loadExecutionPrefs(c *CopyTradeConfig) error {
	var major, alt sql.NullFloat64
	var wait int
	autoCorrect := defaultCopyAutoCorrectEnabled
	err := s.db.QueryRow(`SELECT price_tolerance_major_pct,price_tolerance_alt_pct,take_profit_max_wait_seconds,auto_correct_enabled FROM copy_trade_execution_prefs WHERE trader_id=?`, c.TraderID).Scan(&major, &alt, &wait, &autoCorrect)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if major.Valid {
		c.CopyPriceToleranceMajorPct = &major.Float64
	}
	if alt.Valid {
		c.CopyPriceToleranceAltPct = &alt.Float64
	}
	if wait > 0 {
		c.CopyTakeProfitMaxWaitSeconds = &wait
	}
	c.CopyAutoCorrectOverweight = &autoCorrect
	return nil
}
