package api

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"nofx/copytrade"
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

func TestValidateStopLossPctConfirmation(t *testing.T) {
	if err := validateStopLossPctConfirmation(0, false, nil); err != nil {
		t.Fatalf("zero trader override must inherit the account policy: %v", err)
	}
	if err := validateStopLossPctConfirmation(0.001, false, nil); err != nil {
		t.Fatalf("0.1%% lower boundary must pass: %v", err)
	}
	if err := validateStopLossPctConfirmation(0.10, false, nil); err != nil {
		t.Fatalf("10%% account default boundary must not require confirmation: %v", err)
	}
	if err := validateStopLossPctConfirmation(0.20, false, nil); err == nil {
		t.Fatal("stop cap above 10% must require explicit confirmation")
	}
	exact, wrong := 20.0, 19.9
	if err := validateStopLossPctConfirmation(0.20, true, &wrong); err == nil {
		t.Fatal("mismatched typed stop cap must be rejected")
	}
	if err := validateStopLossPctConfirmation(0.20, true, &exact); err != nil {
		t.Fatalf("matching typed stop cap must pass: %v", err)
	}
	if err := validateStopLossPctConfirmation(0.301, true, &exact); err == nil {
		t.Fatal("stop cap above 30% must always be rejected")
	}
}

func TestApplyCopyConfigRiskFieldsV4DefaultsAndExplicitZero(t *testing.T) {
	// Copy Guard 风控字段对 SupportsCopyGuard 数据源（OKX/Binance）生效；
	// 本用例只覆盖 OKX。不支持的 provider 会被整体剥离（见下一个用例）
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
	cfg = store.NewCopyGuardDefaults()
	cfg.ProviderType = "okx"
	cfg.RiskReentryMinRecoveryATR = 0.5
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{
		RiskPolicyVersion:         4,
		RiskReentryMinRecoveryATR: &zeroFloat,
	})
	if cfg.RiskReentryMinRecoveryATR != 0 {
		t.Fatalf("valid zero recovery threshold was ignored: %+v", cfg)
	}
}

func TestCopyGuardSaveEntryDefaultsShareCanonicalFactory(t *testing.T) {
	cfg := &store.CopyTradeConfig{ProviderType: "okx", RiskPolicyVersion: 4, RiskReentryDecisionMode: "ai_guarded"}
	applyCopyGuardV4Request(cfg, nil, &CopyTradeConfigRequest{})
	defaults := store.NewCopyGuardDefaults()
	if cfg.RiskMaxReentries != defaults.RiskMaxReentries ||
		cfg.RiskReentryCooldownSeconds != defaults.RiskReentryCooldownSeconds ||
		cfg.RiskReentryBandATR != defaults.RiskReentryBandATR ||
		cfg.RiskReentryMaxChaseATR != defaults.RiskReentryMaxChaseATR {
		t.Fatalf("standalone config endpoint defaults drifted from trader create/update endpoint: got=%+v defaults=%+v", cfg, defaults)
	}
}

// 不支持 Copy Guard 的数据源（Hyperliquid）即使前端携带 risk_policy_version=4
// 也必须被剥离，否则后续 "v4 only for OKX/Binance" 校验会把正常跟单保存整体
// 400 拒绝（P0 修复回归钉）。
func TestApplyCopyConfigRiskFieldsStripsUnsupportedProvider(t *testing.T) {
	cfg := &store.CopyTradeConfig{
		ProviderType:             "hyperliquid",
		RiskManualReentryEnabled: true,
		RiskUnprotectableAction:  "follow",
	}
	manual := true
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{
		RiskPolicyVersion:        4,
		RiskManualReentryEnabled: &manual,
		RiskUnprotectableAction:  "follow",
	})
	if cfg.RiskPolicyVersion != 0 {
		t.Fatalf("[hyperliquid] risk_policy_version should be stripped, got %d", cfg.RiskPolicyVersion)
	}
	if cfg.RiskStopLossEnabled || cfg.RiskReentryEnabled {
		t.Fatalf("[hyperliquid] risk switches should stay off: %+v", cfg)
	}
	if cfg.RiskManualReentryEnabled || cfg.RiskUnprotectableDisposition != "warn" || cfg.RiskUnprotectableAction != "follow" {
		t.Fatalf("[hyperliquid] retired settings must be normalized on every write: %+v", cfg)
	}
}

func TestApplyCopyConfigRiskFieldsMapsCanonicalAndLegacyUnprotectablePolicy(t *testing.T) {
	cfg := store.NewCopyGuardDefaults()
	cfg.ProviderType = "okx"
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskUnprotectableDisposition: "close"})
	if cfg.RiskUnprotectableDisposition != "close" || cfg.RiskUnprotectableAction != "close" {
		t.Fatalf("canonical close was not persisted: %+v", cfg)
	}
	cfg = store.NewCopyGuardDefaults()
	cfg.ProviderType = "okx"
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskUnprotectableAction: "follow"})
	if cfg.RiskUnprotectableDisposition != "warn" || cfg.RiskUnprotectableAction != "follow" {
		t.Fatalf("legacy follow was not mapped to warn: %+v", cfg)
	}
	cfg = store.NewCopyGuardDefaults()
	cfg.ProviderType = "okx"
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{RiskUnprotectableDisposition: "typo"})
	if err := copytrade.ValidateStoredRiskPolicy(cfg); err == nil {
		t.Fatalf("invalid canonical disposition must reach shared validation instead of being silently normalized: %+v", cfg)
	}
	old := store.NewCopyGuardDefaults()
	old.RiskUnprotectableDisposition = "close"
	old.RiskUnprotectableAction = "close"
	cfg = store.NewCopyGuardDefaults()
	applyCopyGuardV4Request(cfg, old, &CopyTradeConfigRequest{})
	if cfg.RiskUnprotectableDisposition != "close" || cfg.RiskUnprotectableAction != "close" {
		t.Fatalf("partial config update must preserve an existing explicit close policy: %+v", cfg)
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

func TestApplyCopyConfigRiskFieldsPositionMarginModeRequiresIsolatedSettings(t *testing.T) {
	mode := store.RiskProtectionModePositionMarginPct
	pct := 0.80
	disabled := false
	zero := 0
	disabledMode := "disabled"
	cfg := store.NewCopyGuardDefaults()
	cfg.ProviderType = "binance"
	applyCopyConfigRiskFields(cfg, &CopyConfigReq{
		RiskPolicyVersion:         4,
		RiskProtectionMode:        &mode,
		RiskPositionMarginStopPct: &pct,
		RiskTriggerPriceType:      "mark",
		RiskReentryEnabled:        &disabled,
		RiskReentryDecisionMode:   &disabledMode,
		RiskMaxReentries:          &zero,
	})
	if cfg.RiskProtectionMode != mode || cfg.RiskPositionMarginStopPct != pct {
		t.Fatalf("fixed stop fields were not applied: %+v", cfg)
	}
	if err := copytrade.ValidateStoredRiskPolicy(cfg); err != nil {
		t.Fatalf("valid fixed stop request was rejected: %v", err)
	}
	cfg.RiskReentryEnabled = true
	if err := copytrade.ValidateStoredRiskPolicy(cfg); err == nil {
		t.Fatal("fixed mode accepted a contradictory reentry request")
	}
}

func TestConfigSwitchToFixedKeepsActiveATRLifecycleCandidate(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "copyguard-config-switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO traders
		(id,user_id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation)
		VALUES('trader-1','user-1','Trader 1','model-1','exchange-1',1000,1,?,1)`, store.TraderLifecycleRunning); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`INSERT INTO copy_guard_cycles
		(id,trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot)
		VALUES(1,'trader-1','leader-1','position-1','BTCUSDT','long','cross','AI_WAITING',?)`,
		`{"version":4,"risk_protection_mode":"atr_structure","reentry_enabled":true,"reentry_decision_mode":"ai_guarded","max_reentries":2}`); err != nil {
		t.Fatal(err)
	}
	candidate, err := st.ReentryAI().EnsureReentryCandidate(&store.CopyGuardReentryCandidate{
		CycleID: 1, TraderID: "trader-1", LeaderPosID: "position-1", Symbol: "BTCUSDT", Side: "long",
		FeatureHash: "snapshot-switch", Protectable: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	old := store.NewCopyGuardDefaults()
	old.TraderID = "trader-1"
	old.RiskStopLossEnabled = true
	old.RiskReentryEnabled = true
	old.RiskReentryDecisionMode = "ai_guarded"
	current := *old
	current.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	current.RiskReentryEnabled = false
	current.RiskReentryDecisionMode = "disabled"
	current.RiskMaxReentries = 0
	disableReentryCandidatesAfterConfigSave(st, old, &current)

	kept, err := st.ReentryAI().GetReentryCandidate(candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Status == store.ReentryCandidateInvalidated {
		t.Fatalf("template mode switch invalidated an active ATR lifecycle candidate: %+v", kept)
	}

	current.RiskStopLossEnabled = false
	disableReentryCandidatesAfterConfigSave(st, old, &current)
	disabled, err := st.ReentryAI().GetReentryCandidate(candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != store.ReentryCandidateInvalidated {
		t.Fatalf("master protection disable did not invalidate an unsubmitted candidate: %+v", disabled)
	}
}
