package copytrade

import (
	"math"
	"testing"
)

func TestComputeRiskDistanceV4VolatilityPriorityUsesNotionalWithoutLeverage(t *testing.T) {
	// v4.1：RiskAccountPct 是任何模式下的硬兜底（默认 20%），此处 0.2 → 账户
	// 距离 20 远宽于 ATR 3，ATR 主导。
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.2, RiskSlippageBufferBPS: 10, RiskATRCacheMaxAgeMinutes: 120}
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
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.2, RiskLeverageFallback: true, RiskLeverageMaxLoss: 0.3}
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

// TestComputeRiskDistanceV4NoiseFloorLoosensTightMarginCap reproduces the ETH
// cycle 40/50 churn: at 100x leverage the 30% margin cap collapses to a 0.3%
// price distance, far inside normal market noise, and the position is stopped
// and re-entered repeatedly. The noise floor widens a margin_cap-governed stop
// back to rawATR × RiskStopNoiseFloorATR.
func TestComputeRiskDistanceV4NoiseFloorLoosensTightMarginCap(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.2, RiskLeverageFallback: true, RiskLeverageMaxLoss: 0.3, RiskATRMultiplier: 1.5, RiskStopNoiseFloorATR: 1.0}
	// entry=100, leverage=100 → margin cap distance = 100×0.3/100 = 0.3
	// atrDistance=3 (raw ATR 2 × multiplier 1.5) → noise floor = 2
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-2) > 1e-9 || r.GovernedBy != "noise_floor" || r.NoiseConflict {
		t.Fatalf("expected noise_floor distance 2 without noise conflict, got %+v", r)
	}
	// Floor never loosens beyond the ATR baseline even when configured wider.
	c.RiskStopNoiseFloorATR = 5
	r, err = ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-3) > 1e-9 || r.GovernedBy != "noise_floor" {
		t.Fatalf("noise floor must be capped at the ATR baseline (3), got %+v", r)
	}
	// Floor disabled (0) keeps the raw margin cap.
	c.RiskStopNoiseFloorATR = 0
	r, err = ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.3) > 1e-9 || r.GovernedBy != "margin_cap" {
		t.Fatalf("floor=0 must keep the margin cap, got %+v", r)
	}
}

// TestComputeRiskDistanceV4AccountHardBackstopCapsEveryMode verifies the v4.1
// semantics: RiskAccountPct is a catastrophe backstop in every stop mode, and
// it outranks the noise floor.
func TestComputeRiskDistanceV4AccountHardBackstopCapsEveryMode(t *testing.T) {
	// volatility_priority, notional 2000 vs equity 1000, account pct 1% →
	// account distance = 1000×0.01/2000×100 = 0.5 < ATR 3.
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.01}
	r, err := ComputeRiskDistanceV4(c, 100, 2000, 1000, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account_hard" || !r.NoiseConflict {
		t.Fatalf("account hard backstop must cap the distance in volatility_priority, got %+v", r)
	}
	// The backstop also outranks the noise floor: 100x margin cap 0.3 →
	// floor would widen to 2, but the account line caps at 0.5.
	c.RiskLeverageFallback, c.RiskLeverageMaxLoss = true, 0.3
	c.RiskATRMultiplier, c.RiskStopNoiseFloorATR = 1.5, 1.0
	r, err = ComputeRiskDistanceV4(c, 100, 2000, 1000, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account_hard" {
		t.Fatalf("account hard backstop must outrank the noise floor, got %+v", r)
	}
}

func TestValidateRiskPolicyV4(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskATRPeriod: 14, RiskATRCacheMaxAgeMinutes: 120, RiskATRMultiplier: 1.5, RiskATRFallbackPct: 0.02, RiskTriggerPriceType: "mark", RiskAccountPct: 0.02, RiskLiquidationBufferATR: 0.5, RiskMaxReentries: 2, RiskReentryRatio: 0.5, RiskReentryBandATR: 0.5, RiskReentryCooldownSeconds: 60, RiskReentryMaxATRExpansion: 2}
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatal(err)
	}
}
