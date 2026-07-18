package store

import "testing"

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
