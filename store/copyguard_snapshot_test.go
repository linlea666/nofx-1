package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyGuardPolicySnapshotCanonicalRoundTripExcludesCredentials(t *testing.T) {
	cfg := NewCopyGuardDefaults()
	cfg.TraderID = "trader-1"
	cfg.ProviderType = "binance"
	cfg.BinanceP20T = "secret-p20t"
	cfg.BinanceCSRFToken = "secret-csrf"
	cfg.RiskATRMultiplier = 2.75
	cfg.RiskRoundTripFeeBPS = 19
	cfg.RiskMaxReentries = 3
	cfg.RiskManualReentryEnabled = true

	raw, err := EncodeCopyGuardPolicySnapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-p20t", "secret-csrf", "binance_p20t", "leader_id", "copy_ratio"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("canonical policy leaked %q: %s", forbidden, raw)
		}
	}
	policy, err := DecodeCopyGuardPolicySnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	var restored CopyTradeConfig
	applyCopyGuardPolicyToConfig(&restored, policy)
	if policy.SnapshotSchemaVersion != 2 || restored.RiskATRMultiplier != 2.75 || restored.RiskRoundTripFeeBPS != 19 ||
		restored.RiskMaxReentries != 3 || !restored.RiskManualReentryEnabled {
		t.Fatalf("canonical policy changed risk fields: policy=%+v restored=%+v", policy, restored)
	}
}

func TestCopyGuardATRProfileRoundTripAcrossFixedMode(t *testing.T) {
	atr := NewCopyGuardDefaults()
	atr.RiskTriggerPriceType = "index"
	atr.RiskReentryEnabled = true
	atr.RiskReentryDecisionMode = "ai_guarded"
	atr.RiskManualReentryEnabled = true
	atr.RiskMaxReentries = 3

	fixed := *atr
	fixed.RiskProtectionMode = RiskProtectionModePositionMarginPct
	NormalizeCopyGuardProtectionModeTransition(&fixed, atr)
	if fixed.RiskATRProfile == nil || fixed.RiskATRProfile.TriggerPriceType != "index" ||
		fixed.RiskATRProfile.MaxReentries != 3 || !fixed.RiskATRProfile.ManualReentryEnabled {
		t.Fatalf("ATR profile was not captured: %+v", fixed.RiskATRProfile)
	}
	if fixed.RiskTriggerPriceType != "mark" || fixed.RiskReentryEnabled || fixed.RiskManualReentryEnabled ||
		fixed.RiskReentryDecisionMode != "disabled" || fixed.RiskMaxReentries != 0 {
		t.Fatalf("fixed effective settings were not isolated: %+v", fixed)
	}

	restored := fixed
	restored.RiskProtectionMode = RiskProtectionModeATRStructure
	NormalizeCopyGuardProtectionModeTransition(&restored, &fixed)
	if restored.RiskTriggerPriceType != "index" || !restored.RiskReentryEnabled || !restored.RiskManualReentryEnabled ||
		restored.RiskReentryDecisionMode != "ai_guarded" || restored.RiskMaxReentries != 3 {
		t.Fatalf("ATR profile was not restored: %+v", restored)
	}
}

func TestATRProfileDefaultMigrationIsAtomicAndAudited(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "atr-profile-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO traders
		(id,user_id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation)
		VALUES('trader-profile','user-1','Trader','model-1','exchange-1',1000,0,?,1)`, TraderLifecycleStopped); err != nil {
		t.Fatal(err)
	}

	cfg := NewCopyGuardDefaults()
	cfg.TraderID = "trader-profile"
	cfg.ProviderType = "binance"
	cfg.LeaderID = "leader-1"
	cfg.RiskProtectionMode = RiskProtectionModePositionMarginPct
	cfg.RiskATRProfile = nil
	if err = st.CopyTrade().UpsertWithATRProfileDefaultMigration(cfg, true); err == nil {
		t.Fatal("missing repaired ATR profile must abort the policy transaction")
	}
	var count int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_configs WHERE trader_id=?`, cfg.TraderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed migration left a partial config row: count=%d", count)
	}

	defaults := NewCopyGuardDefaults()
	cfg.RiskATRProfile = copyGuardATRProfileFromConfig(defaults)
	cfg.BinanceP20T = "cookie-must-not-enter-event"
	cfg.BinanceCSRFToken = "csrf-must-not-enter-event"
	if err = st.CopyTrade().UpsertWithATRProfileDefaultMigration(cfg, true); err != nil {
		t.Fatal(err)
	}
	// A retry must remain idempotent and keep exactly one durable audit record.
	if err = st.CopyTrade().UpsertWithATRProfileDefaultMigration(cfg, true); err != nil {
		t.Fatal(err)
	}
	var eventType, detail string
	if err = st.DB().QueryRow(`SELECT COUNT(*),MAX(event_type),MAX(detail_json)
		FROM copy_trade_events WHERE trader_id=? AND event_type=?`,
		cfg.TraderID, CopyEventTypeATRProfileDefaulted).Scan(&count, &eventType, &detail); err != nil {
		t.Fatal(err)
	}
	if count != 1 || eventType != CopyEventTypeATRProfileDefaulted {
		t.Fatalf("unexpected migration audit count/type: count=%d type=%q", count, eventType)
	}
	for _, secret := range []string{cfg.BinanceP20T, cfg.BinanceCSRFToken} {
		if strings.Contains(detail, secret) {
			t.Fatalf("migration audit leaked a credential %q: %s", secret, detail)
		}
	}
	stored, err := st.CopyTrade().GetByTraderID(cfg.TraderID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RiskATRProfile == nil || stored.RiskATRProfile.TriggerPriceType != defaults.RiskTriggerPriceType {
		t.Fatalf("repaired ATR profile was not committed with its event: %+v", stored.RiskATRProfile)
	}
}

func TestCopyGuardPolicySnapshotDecodesLegacyRuntimeShape(t *testing.T) {
	cfg := NewCopyGuardDefaults()
	cfg.BinanceP20T = "legacy-cookie"
	cfg.BinanceCSRFToken = "legacy-csrf"
	cfg.RiskATRMultiplier = 3.25
	cfg.RiskCycleLossBudgetPct = 0.07
	cfg.RiskAIDailyCallLimit = 17
	legacy, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalizeCopyGuardPolicySnapshot(string(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(canonical, "legacy-cookie") || strings.Contains(canonical, "legacy-csrf") {
		t.Fatalf("legacy credentials survived canonicalization: %s", canonical)
	}
	policy, err := DecodeCopyGuardPolicySnapshot(canonical)
	if err != nil {
		t.Fatal(err)
	}
	var restored CopyTradeConfig
	applyCopyGuardPolicyToConfig(&restored, policy)
	if restored.RiskATRMultiplier != 3.25 || restored.RiskCycleLossBudgetPct != 0.07 || restored.RiskAIDailyCallLimit != 17 {
		t.Fatalf("legacy runtime policy did not round-trip: %+v", restored)
	}
	if policy.DefaultsVersion != 0 {
		t.Fatalf("legacy runtime snapshot was mislabeled as current defaults generation: %+v", policy)
	}
}

func TestCopyGuardSnapshotMigrationNormalizesLegacyPolicyInOneStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot-policy-one-start.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	exchangeID, err := st.Exchange().Create(
		"user-1", "okx", "main", true, "", "", "", false,
		"", "", "", "", "", "", "", 0,
	)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err = st.Trader().Create(&Trader{
		ID: "legacy-trader", UserID: "user-1", Name: "legacy", AIModelID: "ai", ExchangeID: exchangeID,
	}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	legacy := `{"risk_policy_version":4,"risk_protection_mode":"position_margin_pct","risk_position_margin_stop_pct":0.8,"risk_unprotectable_action":"close","binance_p20t":"secret"}`
	if _, err = st.db.Exec(`INSERT INTO copy_guard_policies(trader_id,policy_json) VALUES('legacy-trader',?)`, legacy); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var raw string
	if err = st.db.QueryRow(`SELECT policy_json FROM copy_guard_policies WHERE trader_id='legacy-trader'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	policy, err := DecodeCopyGuardPolicySnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "secret") || policy.SnapshotSchemaVersion != 2 ||
		policy.DefaultsVersion != copyGuardPolicyDefaultsVersion ||
		policy.UnprotectableDisposition != "warn" || policy.UnprotectableAction != "follow" {
		t.Fatalf("legacy policy was not fully normalized in one startup: raw=%s policy=%+v", raw, policy)
	}
	if policy.ATRProfile == nil {
		t.Fatal("historical fixed policy did not receive a dormant ATR profile")
	}
	var eventCount int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_events
		WHERE trader_id='legacy-trader' AND event_type=?`, CopyEventTypeATRProfileDefaulted).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("historical fixed policy migration audit count=%d, want 1", eventCount)
	}
}

func TestCopyGuardSnapshotStartupMigrationSanitizesCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot-migration.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := NewCopyGuardDefaults()
	cfg.BinanceP20T = "cycle-cookie"
	cfg.BinanceCSRFToken = "cycle-csrf"
	legacy, _ := json.Marshal(cfg)
	if _, err = st.db.Exec(`INSERT INTO copy_guard_cycles(trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot) VALUES('trader','leader','pos','BTCUSDT','long','cross','FOLLOWING',?)`, string(legacy)); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var raw string
	if err = st.db.QueryRow(`SELECT policy_snapshot FROM copy_guard_cycles WHERE leader_pos_id='pos'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "cycle-cookie") || strings.Contains(raw, "cycle-csrf") || !strings.Contains(raw, `"snapshot_schema_version":2`) {
		t.Fatalf("cycle snapshot was not sanitized: %s", raw)
	}
}

func TestCopyGuardSnapshotStartupMigrationRejectsMalformedCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot-invalid.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec(`INSERT INTO copy_guard_cycles(trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot) VALUES('trader','leader','broken','BTCUSDT','long','cross','FOLLOWING','{broken')`); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, reopenErr := New(path); reopenErr == nil {
		reopened.Close()
		t.Fatal("malformed lifecycle snapshot must fail startup")
	}
}
