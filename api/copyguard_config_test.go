package api

import (
	"math"
	"testing"

	"nofx/store"
)

func TestValidateRiskConfirmation(t *testing.T) {
	if err := validateRiskConfirmation(0.04, false, nil); err != nil {
		t.Fatalf("recommended boundary must not require confirmation: %v", err)
	}
	if err := validateRiskConfirmation(0.05, false, nil); err == nil {
		t.Fatal("risk above 4% must require explicit confirmation")
	}
	if err := validateRiskConfirmation(0.05, true, nil); err != nil {
		t.Fatalf("4%-8% boolean confirmation should pass: %v", err)
	}
	if err := validateRiskConfirmation(0.09, true, nil); err == nil {
		t.Fatal("risk above 8% must require the typed value")
	}
	wrong, exact := 8.99, 9.0
	if err := validateRiskConfirmation(0.09, true, &wrong); err == nil {
		t.Fatal("mismatched typed value must be rejected")
	}
	if err := validateRiskConfirmation(0.09, true, &exact); err != nil {
		t.Fatalf("matching typed value should pass: %v", err)
	}
	nan := math.NaN()
	if err := validateRiskConfirmation(0.09, true, &nan); err == nil {
		t.Fatal("NaN confirmation must be rejected")
	}
}

func TestApplyCopyConfigRiskFieldsV4DefaultsAndExplicitZero(t *testing.T) {
	// Copy Guard 风控字段仅 OKX 生效：config 必须带 ProviderType=okx
	// 才会进入透传分支（非 OKX 会被整体剥离，见下一个用例）
	cfg := store.NewCopyGuardDefaults()
	cfg.ProviderType = "okx"
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskPolicyVersion: 4})
	// v4.1：默认冷却 300s；v5 代次 5：默认单周期最多重入 2 次
	if !cfg.RiskReentryEnabled || cfg.RiskMaxReentries != 2 || cfg.RiskReentryBandATR != 0.5 || cfg.RiskReentryCooldownSeconds != 300 || cfg.RiskSlippageBufferBPS != 10 {
		t.Fatalf("unexpected v4 defaults: %+v", cfg)
	}
	zeroInt := 0
	zeroFloat := 0.0
	cfg = store.NewCopyGuardDefaults()
	cfg.ProviderType = "okx"
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

// 不支持 Copy Guard 的数据源（Hyperliquid）即使前端携带 risk_policy_version=4
// 也必须被剥离，否则后续 "v4 only for OKX/Binance" 校验会把正常跟单保存整体
// 400 拒绝（P0 修复回归钉）。
func TestApplyCopyConfigRiskFieldsStripsUnsupportedProvider(t *testing.T) {
	cfg := &store.CopyTradeConfig{ProviderType: "hyperliquid"}
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskPolicyVersion: 4})
	if cfg.RiskPolicyVersion != 0 {
		t.Fatalf("[hyperliquid] risk_policy_version should be stripped, got %d", cfg.RiskPolicyVersion)
	}
	if cfg.RiskStopLossEnabled || cfg.RiskReentryEnabled {
		t.Fatalf("[hyperliquid] risk switches should stay off: %+v", cfg)
	}
}

// Binance 领航员数据源支持 Copy Guard：risk_policy_version 必须保留，且风控
// 默认值应与 OKX 路径一致（保护单挂在跟随者执行交易所）。
func TestApplyCopyConfigRiskFieldsKeepsBinance(t *testing.T) {
	cfg := store.NewCopyGuardDefaults()
	cfg.ProviderType = "binance"
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskPolicyVersion: 4})
	if cfg.RiskPolicyVersion != 4 {
		t.Fatalf("[binance] risk_policy_version should be kept, got %d", cfg.RiskPolicyVersion)
	}
	if !cfg.RiskReentryEnabled || cfg.RiskMaxReentries != 2 || cfg.RiskReentryCooldownSeconds != 300 || cfg.RiskSlippageBufferBPS != 10 {
		t.Fatalf("[binance] unexpected v4 defaults: %+v", cfg)
	}
}
