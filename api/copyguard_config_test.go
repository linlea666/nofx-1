package api

import (
	"testing"

	"nofx/store"
)

func TestApplyCopyConfigRiskFieldsV4DefaultsAndExplicitZero(t *testing.T) {
	cfg := &store.CopyTradeConfig{}
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskPolicyVersion: 4})
	if !cfg.RiskReentryEnabled || cfg.RiskMaxReentries != 2 || cfg.RiskReentryBandATR != 0.5 || cfg.RiskReentryCooldownSeconds != 60 || cfg.RiskSlippageBufferBPS != 10 {
		t.Fatalf("unexpected v4 defaults: %+v", cfg)
	}
	zeroInt := 0
	zeroFloat := 0.0
	cfg = &store.CopyTradeConfig{}
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{
		RiskPolicyVersion:          4,
		RiskMaxReentries:           &zeroInt,
		RiskReentryBandATR:         &zeroFloat,
		RiskReentryCooldownSeconds: &zeroInt,
		RiskReentryMaxChaseATR:     &zeroFloat,
		RiskWatchTimeoutMinutes:    &zeroInt,
		RiskSlippageBufferBPS:      &zeroFloat,
		RiskLiquidationBufferATR:   &zeroFloat,
	})
	if cfg.RiskMaxReentries != 0 || cfg.RiskReentryBandATR != 0 || cfg.RiskReentryCooldownSeconds != 0 || cfg.RiskSlippageBufferBPS != 0 || cfg.RiskLiquidationBufferATR != 0 {
		t.Fatalf("explicit zero was overwritten: %+v", cfg)
	}
}
