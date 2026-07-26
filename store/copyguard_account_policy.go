package store

import (
	"database/sql"
	"fmt"
	"time"
)

const DefaultCopyGuardMaxPositionLossPct = 0.10

type CopyGuardAccountPolicy struct {
	ExchangeID         string    `json:"exchange_id"`
	MaxPositionLossPct float64   `json:"copy_guard_max_position_loss_pct"`
	Persisted          bool      `json:"persisted"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type EffectiveCopyGuardStopPolicy struct {
	ExchangeID         string  `json:"exchange_id"`
	MaxPositionLossPct float64 `json:"effective_risk_stop_max_account_loss_pct"`
	Source             string  `json:"risk_stop_pct_source"`
}

func validateCopyGuardPositionLossPct(value float64) error {
	if value < 0.001 || value > 0.30 {
		return fmt.Errorf("copy guard max position loss pct %.6f must be between 0.001 and 0.30", value)
	}
	return nil
}

func (s *CopyTradeStore) GetCopyGuardAccountPolicy(exchangeID string) (*CopyGuardAccountPolicy, error) {
	if exchangeID == "" {
		return nil, fmt.Errorf("exchange id is required")
	}
	policy := &CopyGuardAccountPolicy{
		ExchangeID:         exchangeID,
		MaxPositionLossPct: DefaultCopyGuardMaxPositionLossPct,
	}
	var updated sql.NullString
	err := s.db.QueryRow(`SELECT max_position_loss_pct,updated_at FROM copy_guard_account_policies WHERE exchange_id=?`, exchangeID).
		Scan(&policy.MaxPositionLossPct, &updated)
	if err == sql.ErrNoRows {
		return policy, nil
	}
	if err != nil {
		return nil, err
	}
	policy.Persisted = true
	if updated.Valid {
		policy.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updated.String)
	}
	return policy, nil
}

func (s *CopyTradeStore) UpsertCopyGuardAccountPolicy(exchangeID string, maxPositionLossPct float64) error {
	if exchangeID == "" {
		return fmt.Errorf("exchange id is required")
	}
	if err := validateCopyGuardPositionLossPct(maxPositionLossPct); err != nil {
		return err
	}
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM exchanges WHERE id=?`, exchangeID).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return fmt.Errorf("exchange account %s does not exist", exchangeID)
	}
	_, err := s.db.Exec(`INSERT INTO copy_guard_account_policies(exchange_id,max_position_loss_pct,updated_at)
		VALUES(?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(exchange_id) DO UPDATE SET max_position_loss_pct=excluded.max_position_loss_pct,updated_at=CURRENT_TIMESTAMP`,
		exchangeID, maxPositionLossPct)
	return err
}

func (s *CopyTradeStore) EffectiveCopyGuardStopPolicy(traderID string, traderOverride float64) (*EffectiveCopyGuardStopPolicy, error) {
	if traderID == "" {
		return nil, fmt.Errorf("trader id is required")
	}
	var exchangeID string
	if err := s.db.QueryRow(`SELECT exchange_id FROM traders WHERE id=?`, traderID).Scan(&exchangeID); err != nil {
		return nil, err
	}
	if traderOverride > 0 {
		if err := validateCopyGuardPositionLossPct(traderOverride); err != nil {
			return nil, err
		}
		return &EffectiveCopyGuardStopPolicy{
			ExchangeID: exchangeID, MaxPositionLossPct: traderOverride, Source: "trader_override",
		}, nil
	}
	account, err := s.GetCopyGuardAccountPolicy(exchangeID)
	if err != nil {
		return nil, err
	}
	return &EffectiveCopyGuardStopPolicy{
		ExchangeID: exchangeID, MaxPositionLossPct: account.MaxPositionLossPct, Source: "account_default",
	}, nil
}
