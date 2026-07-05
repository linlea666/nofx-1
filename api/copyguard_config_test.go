package api

import (
	"testing"

	"nofx/store"
)

func TestApplyCopyConfigRiskFieldsV4DefaultsAndExplicitZero(t *testing.T) {
	cfg := &store.CopyTradeConfig{}
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskPolicyVersion: 4})
	// v4.1：默认冷却 300s；v5：默认单周期最多重入 1 次
	if !cfg.RiskReentryEnabled || cfg.RiskMaxReentries != 1 || cfg.RiskReentryBandATR != 0.5 || cfg.RiskReentryCooldownSeconds != 300 || cfg.RiskSlippageBufferBPS != 10 {
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
