package copytrade

import (
	"math"
	"testing"
)

func TestComputeRiskDistanceV4VolatilityPriorityUsesNotionalWithoutLeverage(t *testing.T) {
	c := &CopyConfig{RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.02, RiskSlippageBufferBPS: 10}
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.Distance != 3 || r.GovernedBy != "atr" {
		t.Fatalf("unexpected result: %+v", r)
	}
	// 3% of 1000 plus 10bps = 31 USD. Leverage must not enter this formula.
	if math.Abs(r.ExpectedLossUSD-31) > 1e-9 {
		t.Fatalf("expected 31, got %f", r.ExpectedLossUSD)
	}
}

func TestComputeRiskDistanceV4AccountHardLimitReportsNoiseConflict(t *testing.T) {
	c := &CopyConfig{RiskPolicyVersion: 4, RiskStopMode: "account_hard_limit", RiskAccountPct: 0.01}
	r, err := ComputeRiskDistanceV4(c, 100, 2000, 1000, 3)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account" || !r.NoiseConflict {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestValidateRiskPolicyV4(t *testing.T) {
	c := &CopyConfig{RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskATRPeriod: 14, RiskATRMultiplier: 1.5, RiskATRFallbackPct: 0.02, RiskTriggerPriceType: "mark", RiskAccountPct: 0.02, RiskLiquidationBufferATR: 0.5, RiskMaxReentries: 2, RiskReentryRatio: 0.5, RiskReentryBandATR: 0.5, RiskReentryCooldownSeconds: 60, RiskReentryMaxATRExpansion: 2}
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatal(err)
	}
}
