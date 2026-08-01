package copytrade

import (
	"math"
	"testing"
)

func TestAccountProtectionUsesStructureThenHardLossCap(t *testing.T) {
	cfg := &CopyConfig{RiskSlippageBufferBPS: 0, RiskRoundTripFeeBPS: 0, RiskStopPriority: "account_cap"}
	got, err := ComputeAccountProtectionDistance(
		cfg, SideLong, 100, 1000, 100,
		2, 4, 90, 0.10, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.GovernedBy != "account_cap" || math.Abs(got.Distance-1) > 1e-9 {
		t.Fatalf("hard cap must tighten the structural stop: %+v", got)
	}
	if math.Abs(got.ExpectedLossUSD-10) > 1e-9 || math.Abs(got.ExpectedLossPct-0.10) > 1e-9 {
		t.Fatalf("unexpected account-capped loss: %+v", got)
	}
}

func TestAccountProtectionKeepsDistantStructureWithinCap(t *testing.T) {
	cfg := &CopyConfig{RiskSlippageBufferBPS: 0, RiskRoundTripFeeBPS: 0}
	got, err := ComputeAccountProtectionDistance(
		cfg, SideShort, 100, 100, 100,
		2, 4, 110, 0.30, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.GovernedBy != "structure" || math.Abs(got.Distance-10.5) > 1e-9 {
		t.Fatalf("structure should remain when inside account cap: %+v", got)
	}
}

func TestAccountProtectionVolatilityFirstWarnsWithoutCompressingATR(t *testing.T) {
	cfg := &CopyConfig{
		RiskStopPriority:  "volatility_first",
		RiskATRMultiplier: 2,
	}
	got, err := ComputeAccountProtectionDistance(
		cfg, SideShort, 1974.41, 2918, 100,
		11, 22, 0, 0.10, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.Distance-22) > 1e-9 || got.GovernedBy != "atr" {
		t.Fatalf("volatility-first must preserve 2 ATR distance: %+v", got)
	}
	if !got.AccountRiskThresholdExceeded || got.NoiseConflict {
		t.Fatalf("10%% must be a warning, not a noise-producing cap: %+v", got)
	}
	if got.DistanceATRRatio < 1.99 {
		t.Fatalf("expected roughly 2 ATR, got %.4f", got.DistanceATRRatio)
	}
}

func TestAccountProtectionIncludesFeesInPositionMarginCap(t *testing.T) {
	cfg := &CopyConfig{
		RiskStopPriority:      "volatility_first",
		RiskLeverageFallback:  true,
		RiskLeverageMaxLoss:   0.50,
		RiskSlippageBufferBPS: 10,
		RiskRoundTripFeeBPS:   10,
	}
	got, err := ComputeAccountProtectionDistance(
		cfg, SideLong, 100, 1000, 1000,
		2, 4, 0, 0.30, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Initial margin=100, total cap=50. Friction=2, so price loss budget=48
	// and the trigger distance is 4.8. ATR=4 remains tighter in this case.
	if got.GovernedBy != "atr" || math.Abs(got.ExpectedMarginLossPct-0.42) > 1e-9 {
		t.Fatalf("unexpected wider margin cap result: %+v", got)
	}

	got, err = ComputeAccountProtectionDistance(
		cfg, SideLong, 100, 1000, 1000,
		2, 8, 0, 0.30, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.GovernedBy != "margin_cap" || math.Abs(got.Distance-4.8) > 1e-9 || math.Abs(got.ExpectedMarginLossPct-0.50) > 1e-9 {
		t.Fatalf("position margin cap must include friction exactly: %+v", got)
	}
}

func TestFinalizeAccountStopRoundsTowardSafetyAndRecomputesRisk(t *testing.T) {
	for _, tc := range []struct {
		name string
		side SideType
		want float64
	}{
		{name: "long rounds upward", side: SideLong, want: 91},
		{name: "short rounds downward", side: SideShort, want: 109},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &StopLossCalcInput{
				Symbol: "TESTUSDT", Side: tc.side, EntryPrice: 100, PositionValue: 100,
				FollowerEquity: 100, Leverage: 1, PriceTickSize: 1, BaseQuantityStep: 1,
				MaxAccountLossPct: 0.10,
			}
			result := &StopLossCalcResult{SLDistance: 9.4, ATRValue: 2}
			got, err := finalizeStopLossPrice(input, result, 0.25)
			if err != nil {
				t.Fatal(err)
			}
			if got.SLPrice != tc.want || math.Abs(got.SLDistance-9) > 1e-9 {
				t.Fatalf("unsafe tick alignment: price=%.4f distance=%.4f", got.SLPrice, got.SLDistance)
			}
			if math.Abs(got.ExpectedLossUSD-9) > 1e-9 || math.Abs(got.ExpectedLossPct-0.09) > 1e-9 {
				t.Fatalf("risk must use aligned trigger: %+v", got)
			}
		})
	}
}
