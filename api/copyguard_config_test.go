package api

import (
	"testing"

	"nofx/store"
)

func TestApplyCopyConfigRiskFieldsV4DefaultsAndExplicitZero(t *testing.T) {
	// Copy Guard 风控字段仅 OKX 生效：config 必须带 ProviderType=okx
	// 才会进入透传分支（非 OKX 会被整体剥离，见下一个用例）
	cfg := &store.CopyTradeConfig{ProviderType: "okx"}
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskPolicyVersion: 4})
	// v4.1：默认冷却 300s；v5 代次 5：默认单周期最多重入 2 次
	if !cfg.RiskReentryEnabled || cfg.RiskMaxReentries != 2 || cfg.RiskReentryBandATR != 0.5 || cfg.RiskReentryCooldownSeconds != 300 || cfg.RiskSlippageBufferBPS != 10 {
		t.Fatalf("unexpected v4 defaults: %+v", cfg)
	}
	zeroInt := 0
	zeroFloat := 0.0
	cfg = &store.CopyTradeConfig{ProviderType: "okx"}
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

// 非 OKX（Hyperliquid/Binance）不支持 Copy Guard：即使前端携带
// risk_policy_version=4 也必须被剥离，否则后续 "v4 only for OKX"
// 校验会把正常跟单保存整体 400 拒绝（P0 修复回归钉）。
func TestApplyCopyConfigRiskFieldsStripsNonOKX(t *testing.T) {
	for _, provider := range []string{"hyperliquid", "binance"} {
		cfg := &store.CopyTradeConfig{ProviderType: provider}
		applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskPolicyVersion: 4})
		if cfg.RiskPolicyVersion != 0 {
			t.Fatalf("[%s] risk_policy_version should be stripped, got %d", provider, cfg.RiskPolicyVersion)
		}
		if cfg.RiskStopLossEnabled || cfg.RiskReentryEnabled {
			t.Fatalf("[%s] risk switches should stay off: %+v", provider, cfg)
		}
	}
}
