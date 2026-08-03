package copytrade

import (
	"math"
	"testing"
)

// noiseFloorInput builds a long position whose ATR-derived stop the liquidation
// buffer will have to clamp, which is the only way a distance can end up inside
// the noise band once the margin ceiling stopped tightening.
func noiseFloorInput(liquidationPrice float64) *StopLossCalcInput {
	return &StopLossCalcInput{
		Symbol: "TESTUSDT", Side: SideLong, EntryPrice: 100, PositionValue: 1000,
		FollowerEquity: 100, Leverage: 100, PriceTickSize: 0.01, BaseQuantityStep: 0.001,
		LiquidationPrice: liquidationPrice, MaxAccountLossPct: 0.10,
	}
}

func TestStructuralNoiseFloorMarksHighLeveragePositionUnprotectable(t *testing.T) {
	// Liquidation 0.6% away at 100x leaves roughly 0.4 ATR of room once the
	// buffer is reserved — below the 1.0 ATR floor, so no stop is placed.
	input := noiseFloorInput(99.4)
	result := &StopLossCalcResult{SLPrice: 98.5, SLDistance: 1.5, ATRValue: 0.75}

	got, err := finalizeStopLossPrice(input, result, 0.5, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Unprotectable || !got.UnprotectableStructural {
		t.Fatalf("noise-band stop must be structurally unprotectable: %+v", got)
	}
	if got.SLPrice != 0 {
		t.Fatalf("no trigger price may be published for an unprotectable position: %+v", got)
	}
	if got.DistanceATRRatio >= 1.0 {
		t.Fatalf("test setup no longer lands inside the noise band: ratio=%.4f", got.DistanceATRRatio)
	}
}

func TestStructuralNoiseFloorLeavesRoomyStopAlone(t *testing.T) {
	// Same instrument, liquidation far away: the ATR stop survives untouched.
	input := noiseFloorInput(80)
	result := &StopLossCalcResult{SLPrice: 98.5, SLDistance: 1.5, ATRValue: 0.75}

	got, err := finalizeStopLossPrice(input, result, 0.5, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Unprotectable || got.UnprotectableStructural {
		t.Fatalf("a 2 ATR stop must stay protectable: %+v", got)
	}
	if math.Abs(got.SLPrice-98.5) > 1e-9 {
		t.Fatalf("protectable stop must keep its trigger: %+v", got)
	}
}

// Zero disables the floor entirely, preserving pre-v8 behaviour for anyone who
// opts out. The clamped stop is then published even though it sits in noise.
func TestStructuralNoiseFloorDisabledByZero(t *testing.T) {
	input := noiseFloorInput(99.4)
	result := &StopLossCalcResult{SLPrice: 98.5, SLDistance: 1.5, ATRValue: 0.75}

	got, err := finalizeStopLossPrice(input, result, 0.5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.UnprotectableStructural {
		t.Fatalf("floor must be inert at 0: %+v", got)
	}
	if !got.Clamped || got.SLPrice <= 0 {
		t.Fatalf("legacy behaviour still publishes the clamped stop: %+v", got)
	}
}

// A position with no ATR reading cannot be judged against an ATR floor; it must
// fall through to the existing liquidation logic rather than be declared
// unprotectable on missing data.
func TestStructuralNoiseFloorIgnoredWithoutATR(t *testing.T) {
	input := noiseFloorInput(80)
	result := &StopLossCalcResult{SLPrice: 98.5, SLDistance: 1.5, ATRValue: 0}

	got, err := finalizeStopLossPrice(input, result, 0.5, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if got.UnprotectableStructural || got.SLPrice <= 0 {
		t.Fatalf("missing ATR must not fabricate an unprotectable verdict: %+v", got)
	}
}

func TestValidateRiskPolicyBoundsMinStopATRRatio(t *testing.T) {
	base := func() *CopyConfig {
		c := &CopyConfig{RiskPolicyVersion: 4, ProviderType: ProviderOKX}
		c.FillRiskDefaults()
		return c
	}
	if cfg := base(); cfg.RiskMinStopATRRatio != 1.0 {
		t.Fatalf("default floor must be 1.0 ATR, got %.4f", cfg.RiskMinStopATRRatio)
	}
	for _, ratio := range []float64{0, 1.0, 3} {
		cfg := base()
		cfg.RiskMinStopATRRatio = ratio
		cfg.RiskMinStopATRRatioExplicit = true
		if err := ValidateRiskPolicyV4(cfg); err != nil {
			t.Fatalf("ratio %.2f must be accepted: %v", ratio, err)
		}
	}
	for _, ratio := range []float64{-0.1, 3.1} {
		cfg := base()
		cfg.RiskMinStopATRRatio = ratio
		cfg.RiskMinStopATRRatioExplicit = true
		if err := ValidateRiskPolicyV4(cfg); err == nil {
			t.Fatalf("ratio %.2f must be rejected", ratio)
		}
	}
}

// An explicit 0 is a deliberate opt-out and must survive the defaulting pass,
// which cannot otherwise tell it apart from an unset field.
func TestExplicitZeroMinStopATRRatioSurvivesDefaults(t *testing.T) {
	cfg := &CopyConfig{RiskMinStopATRRatio: 0, RiskMinStopATRRatioExplicit: true, RiskPolicyVersion: 4}
	cfg.FillRiskDefaults()
	if cfg.RiskMinStopATRRatio != 0 {
		t.Fatalf("explicit disable was overwritten with %.4f", cfg.RiskMinStopATRRatio)
	}
}
