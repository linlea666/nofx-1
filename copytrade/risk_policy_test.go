package copytrade

import (
	"math"
	"testing"
)

func TestComputeRiskDistanceV4VolatilityPriorityUsesNotionalWithoutLeverage(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.02, RiskSlippageBufferBPS: 10, RiskATRCacheMaxAgeMinutes: 120}
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 1)
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
	r, err := ComputeRiskDistanceV4(c, 100, 2000, 1000, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account" || !r.NoiseConflict {
		t.Fatalf("unexpected result: %+v", r)
	}
}

// TestComputeRiskDistanceV4MarginCapBoundsStopDistance reproduces the WLD
// incident: 20x leverage with an ATR distance of ~3% of price meant the stop
// only fired around -60% margin loss. With the margin-loss cap (reused v3
// risk_leverage_max_loss semantics) the distance is bounded by
// entry × maxLoss / leverage regardless of how wide the ATR is.
func TestComputeRiskDistanceV4MarginCapBoundsStopDistance(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.02, RiskLeverageFallback: true, RiskLeverageMaxLoss: 0.3}
	// entry=100, leverage=20, maxLoss=30% → cap distance = 100×0.3/20 = 1.5 < ATR 3.
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 20)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-1.5) > 1e-9 || r.GovernedBy != "margin_cap" || !r.NoiseConflict {
		t.Fatalf("expected margin_cap distance 1.5 with noise conflict, got %+v", r)
	}
	// Cap wider than ATR must not loosen the stop.
	r, err = ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 5) // cap = 100×0.3/5 = 6 > 3
	if err != nil {
		t.Fatal(err)
	}
	if r.Distance != 3 || r.GovernedBy != "atr" {
		t.Fatalf("expected atr-governed distance 3, got %+v", r)
	}
	// Disabled fallback keeps pure ATR behaviour.
	c.RiskLeverageFallback = false
	r, err = ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 20)
	if err != nil {
		t.Fatal(err)
	}
	if r.Distance != 3 || r.GovernedBy != "atr" {
		t.Fatalf("expected atr-governed distance with fallback disabled, got %+v", r)
	}
}

func TestValidateRiskPolicyV4(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskATRPeriod: 14, RiskATRCacheMaxAgeMinutes: 120, RiskATRMultiplier: 1.5, RiskATRFallbackPct: 0.02, RiskTriggerPriceType: "mark", RiskAccountPct: 0.02, RiskLiquidationBufferATR: 0.5, RiskMaxReentries: 2, RiskReentryRatio: 0.5, RiskReentryBandATR: 0.5, RiskReentryCooldownSeconds: 60, RiskReentryMaxATRExpansion: 2}
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatal(err)
	}
}
