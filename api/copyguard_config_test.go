package api

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/copytrade"
	"nofx/store"
)

func TestSanitizedCopyGuardCycleStripsHistoricalCredentials(t *testing.T) {
	raw, err := json.Marshal(&store.CopyTradeConfig{
		TraderID:                  "trader-secret",
		LeaderID:                  "leader-secret",
		CopyRatio:                 2,
		BinanceP20T:               "cookie-secret",
		BinanceCSRFToken:          "csrf-secret",
		RiskPolicyVersion:         4,
		RiskProtectionMode:        store.RiskProtectionModePositionMarginPct,
		RiskPositionMarginStopPct: 0.8,
	})
	if err != nil {
		t.Fatal(err)
	}
	cycle := sanitizedCopyGuardCycle(&store.CopyGuardCycle{ID: 7, PolicySnapshot: string(raw)})
	if cycle.ID != 7 {
		t.Fatalf("sanitizer changed cycle identity: %+v", cycle)
	}
	for _, secret := range []string{"cookie-secret", "csrf-secret", "leader-secret", "copy_ratio"} {
		if strings.Contains(cycle.PolicySnapshot, secret) {
			t.Fatalf("sanitized cycle leaked %q: %s", secret, cycle.PolicySnapshot)
		}
	}
	policy, err := store.DecodeCopyGuardPolicySnapshot(cycle.PolicySnapshot)
	if err != nil || policy.SnapshotSchemaVersion != 2 || policy.PositionMarginStopPct != 0.8 {
		t.Fatalf("sanitized policy is not canonical: policy=%+v err=%v", policy, err)
	}
}

func TestAccountWorstCaseRiskUsesActiveSnapshotsAfterMasterSwitchOff(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "account-risk-active.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	exchangeID, err := st.Exchange().Create(
		"user-1", "okx", "main", true, "", "", "", false,
		"", "", "", "", "", "", "", 0,
	)
	if err != nil {
		t.Fatal(err)
	}

	createTrader := func(id string) {
		t.Helper()
		if createErr := st.Trader().Create(&store.Trader{
			ID: id, UserID: "user-1", Name: id, AIModelID: "ai", ExchangeID: exchangeID,
		}); createErr != nil {
			t.Fatal(createErr)
		}
		cfg := store.NewCopyGuardDefaults()
		cfg.TraderID, cfg.ProviderType, cfg.LeaderID = id, "okx", "leader-"+id
		cfg.Enabled = true
		cfg.RiskStopLossEnabled = false
		if createErr := st.CopyTrade().Upsert(cfg); createErr != nil {
			t.Fatal(createErr)
		}
	}
	createTrader("atr-trader")
	createTrader("fixed-trader")

	atrPolicy := store.NewCopyGuardDefaults()
	atrPolicy.RiskStopMaxAccountLossPct = 0.12
	atrSnapshot, err := store.EncodeCopyGuardPolicySnapshot(atrPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "atr-trader", LeaderID: "leader-atr", LeaderPosID: "atr-pos",
		Symbol: "BTCUSDT", Side: string(copytrade.SideLong), MarginMode: "cross",
		Status: store.CopyGuardFollowing, PolicySnapshot: atrSnapshot,
		FollowerEntryPrice: 100, FollowerNotional: 500, AccountEquity: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	fixedPolicy := store.NewCopyGuardDefaults()
	fixedPolicy.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	fixedSnapshot, err := store.EncodeCopyGuardPolicySnapshot(fixedPolicy)
	if err != nil {
		t.Fatal(err)
	}
	fixedCycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "fixed-trader", LeaderID: "leader-fixed", LeaderPosID: "fixed-pos",
		Symbol: "ETHUSDT", Side: string(copytrade.SideLong), MarginMode: "cross",
		Status: store.CopyGuardFollowing, PolicySnapshot: fixedSnapshot,
		FollowerEntryPrice: 110, FollowerNotional: 220, AccountEquity: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().OpenCopyGuardAttempt(fixedCycle.ID, 0, 110, 220, 2, 5); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_guard_attempts SET
		stop_anchor_entry_price=100,stop_anchor_leverage=20,stop_anchor_initial_margin=10,
		stop_anchor_price=96,stop_configured_margin_loss_pct=.8,
		stop_anchor_source='INITIAL_FILL',stop_anchor_source_intent_id=1,final_stop_price=96
		WHERE cycle_id=? AND attempt_no=0`, fixedCycle.ID); err != nil {
		t.Fatal(err)
	}

	handler := NewCopyTradeHandler(st, nil)
	count, worst, err := handler.accountWorstCaseRisk("user-1", exchangeID, 0.10)
	if err != nil {
		t.Fatal(err)
	}
	// ATR uses its frozen 12% account cap (120); fixed uses the actual current
	// stop distance (110-96)*2 = 28 and never consumes the account cap.
	if count != 2 || math.Abs(worst-148) > 1e-9 {
		t.Fatalf("active lifecycle risk count=%d worst=%.8f, want 2 / 148", count, worst)
	}
}

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
	if disabled.Status == store.ReentryCandidateInvalidated {
		t.Fatalf("future-only master switch invalidated an active lifecycle candidate: %+v", disabled)
	}
}

func TestLegacyReentryValidationAllowsDormantProfileRestoration(t *testing.T) {
	fixed := store.NewCopyGuardDefaults()
	fixed.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	fixed.RiskReentryDecisionMode = "disabled"
	fixed.RiskATRProfile = &store.CopyGuardATRProfile{ReentryDecisionMode: "legacy_rule"}

	if err := validateLegacyReentrySelection(fixed, "legacy_rule"); err != nil {
		t.Fatalf("restoring a dormant legacy ATR contract must remain compatible: %v", err)
	}
	fixed.RiskATRProfile.ReentryDecisionMode = "ai_guarded"
	if err := validateLegacyReentrySelection(fixed, "legacy_rule"); err == nil {
		t.Fatal("fixed mode without a dormant legacy contract accepted a new legacy selection")
	}
}
