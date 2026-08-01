package store

import (
	"path/filepath"
	"testing"
)

// S10 回归：Binance 领航员的存量 v3 止损配置也必须升级为 v4（Copy Guard），
// 否则前端按 SupportsCopyGuard 展示"已启用"而后端仍停留在 v3，虚显保护。
func TestUpgradeLegacyRiskPolicyCoversAllCopyGuardProviders(t *testing.T) {
	for _, provider := range []string{"okx", "binance"} {
		c := &CopyTradeConfig{ProviderType: provider, RiskStopLossEnabled: true, RiskPolicyVersion: 3, RiskAccountPct: 0.5, RiskLeverageMaxLoss: 0.5}
		upgradeLegacyRiskPolicy(c)
		if c.RiskPolicyVersion != 4 || !c.RiskMigrationConfirmed {
			t.Fatalf("%s: legacy v3 config must upgrade to v4, got version=%d confirmed=%v", provider, c.RiskPolicyVersion, c.RiskMigrationConfirmed)
		}
		if c.RiskAccountPct != 0 || c.RiskLeverageMaxLoss != 0 {
			t.Fatalf("%s: aggressive v3 defaults must be reset for FillRiskDefaults, got account=%.2f margin=%.2f", provider, c.RiskAccountPct, c.RiskLeverageMaxLoss)
		}
	}

	hl := &CopyTradeConfig{ProviderType: "hyperliquid", RiskStopLossEnabled: true, RiskPolicyVersion: 3}
	upgradeLegacyRiskPolicy(hl)
	if hl.RiskPolicyVersion != 3 {
		t.Fatalf("hyperliquid must not be upgraded, got version=%d", hl.RiskPolicyVersion)
	}

	v4 := &CopyTradeConfig{ProviderType: "binance", RiskStopLossEnabled: true, RiskPolicyVersion: 4, RiskAccountPct: 0.5}
	upgradeLegacyRiskPolicy(v4)
	if v4.RiskAccountPct != 0.5 {
		t.Fatal("already-v4 config must be left untouched")
	}
}

func TestPositionMarginStopMigrationIsSelectiveAndIdempotent(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "margin-stop-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	type seed struct {
		id          string
		stopEnabled bool
		capEnabled  bool
		maxLoss     float64
	}
	seeds := []seed{
		{id: "legacy-off", stopEnabled: true, capEnabled: false, maxLoss: 0.2},
		{id: "explicit-20", stopEnabled: true, capEnabled: true, maxLoss: 0.2},
		{id: "explicit-50", stopEnabled: true, capEnabled: true, maxLoss: 0.5},
		{id: "global-off", stopEnabled: false, capEnabled: false, maxLoss: 0.2},
	}
	for _, item := range seeds {
		if _, err = st.DB().Exec(`INSERT INTO traders(id, name, ai_model_id, exchange_id, initial_balance) VALUES(?,?,?,?,0)`, item.id, item.id, "m", "e"); err != nil {
			t.Fatal(err)
		}
		if _, err = st.DB().Exec(`
			INSERT INTO copy_trade_configs
				(trader_id, provider_type, leader_id, risk_stop_loss_enabled,
				 risk_leverage_fallback, risk_leverage_max_loss,
				 risk_margin_stop_migration_version)
			VALUES (?, 'okx', 'leader', ?, ?, ?, 0)
		`, item.id, item.stopEnabled, item.capEnabled, item.maxLoss); err != nil {
			t.Fatal(err)
		}
	}

	if err = st.CopyTrade().migratePositionMarginStops(); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().migratePositionMarginStops(); err != nil {
		t.Fatalf("second migration must be a no-op: %v", err)
	}

	for _, item := range seeds {
		var stopEnabled, capEnabled bool
		var maxLoss float64
		var version int
		if err = st.DB().QueryRow(`
			SELECT risk_stop_loss_enabled, risk_leverage_fallback,
			       risk_leverage_max_loss, risk_margin_stop_migration_version
			FROM copy_trade_configs WHERE trader_id = ?
		`, item.id).Scan(&stopEnabled, &capEnabled, &maxLoss, &version); err != nil {
			t.Fatal(err)
		}
		if version != positionMarginStopMigrationVersion {
			t.Fatalf("%s migration version=%d", item.id, version)
		}
		switch item.id {
		case "legacy-off":
			if !stopEnabled || !capEnabled || maxLoss != 0.5 {
				t.Fatalf("legacy disabled cap not migrated: stop=%v cap=%v loss=%.2f", stopEnabled, capEnabled, maxLoss)
			}
		default:
			if stopEnabled != item.stopEnabled || capEnabled != item.capEnabled || maxLoss != item.maxLoss {
				t.Fatalf("%s explicit values changed: stop=%v cap=%v loss=%.2f", item.id, stopEnabled, capEnabled, maxLoss)
			}
		}
	}
	var events int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_events WHERE event_type='POSITION_MARGIN_STOP_MIGRATED'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("expected one durable migration event, got %d", events)
	}
}

func TestNewCopyGuardDefaultsEnableFiftyPercentPositionStop(t *testing.T) {
	cfg := NewCopyGuardDefaults()
	if !cfg.RiskLeverageFallback || cfg.RiskLeverageMaxLoss != 0.5 {
		t.Fatalf("new defaults must enable 50%% position stop: %+v", cfg)
	}
	if cfg.RiskMarginStopMigrationVersion != positionMarginStopMigrationVersion {
		t.Fatalf("new config must carry current migration version, got %d", cfg.RiskMarginStopMigrationVersion)
	}
}
