package copytrade

import (
	"math"
	"testing"
)

func TestAccountProtectionUsesStructureThenHardLossCap(t *testing.T) {
	cfg := &CopyConfig{RiskSlippageBufferBPS: 0, RiskRoundTripFeeBPS: 0}
	got, err := ComputeAccountProtectionDistance(
		cfg, SideLong, 100, 1000, 100,
		2, 4, 90, 0.10,
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
		2, 4, 110, 0.30,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.GovernedBy != "structure" || math.Abs(got.Distance-10.5) > 1e-9 {
		t.Fatalf("structure should remain when inside account cap: %+v", got)
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
