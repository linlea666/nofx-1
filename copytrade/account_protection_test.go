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

// The position margin ceiling reports, it never tightens. Tightening is what
// pushed high-leverage stops into the noise band and made Copy Guard lose more
// than holding to the leader's close.
func TestPositionMarginCapReportsWithoutTighteningDistance(t *testing.T) {
	cfg := &CopyConfig{
		RiskStopPriority:      "volatility_first",
		RiskLeverageFallback:  true,
		RiskLeverageMaxLoss:   0.50,
		RiskSlippageBufferBPS: 10,
		RiskRoundTripFeeBPS:   10,
	}
	// Initial margin=100, total cap=50. Friction=2, so the price loss budget is
	// 48 and the cap distance is 4.8 — the friction arithmetic this test has
	// always pinned. ATR=4 sits below it, so nothing is near the ceiling.
	got, err := ComputeAccountProtectionDistance(
		cfg, SideLong, 100, 1000, 1000,
		2, 4, 0, 0.30, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.GovernedBy != "atr" || math.Abs(got.ExpectedMarginLossPct-0.42) > 1e-9 {
		t.Fatalf("unexpected wider margin cap result: %+v", got)
	}
	if math.Abs(got.MarginCapDistance-4.8) > 1e-9 || got.MarginCapExceeded {
		t.Fatalf("cap distance must include friction and report no breach: %+v", got)
	}

	// ATR=8 now exceeds the 4.8 ceiling. The distance must stay at 8 and the
	// breach must surface as a flag, not as a rewritten stop.
	got, err = ComputeAccountProtectionDistance(
		cfg, SideLong, 100, 1000, 1000,
		2, 8, 0, 0.30, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.GovernedBy != "atr" || math.Abs(got.Distance-8) > 1e-9 {
		t.Fatalf("margin cap must not tighten the ATR distance: %+v", got)
	}
	if !got.MarginCapExceeded || math.Abs(got.MarginCapDistance-4.8) > 1e-9 {
		t.Fatalf("margin cap breach must be reported: %+v", got)
	}
	// 8/100 + 0.002 friction on 1000 notional against 100 initial margin.
	if math.Abs(got.ExpectedMarginLossPct-0.82) > 1e-9 {
		t.Fatalf("expected margin loss must reflect the untightened stop: %+v", got)
	}
}

// account_cap is the opt-in backward-compatible hard ceiling. It is the one
// mode still allowed to tighten, and must keep doing so.
func TestAccountCapPriorityStillTightens(t *testing.T) {
	cfg := &CopyConfig{
		RiskStopPriority:      "account_cap",
		RiskLeverageFallback:  true,
		RiskLeverageMaxLoss:   0.50,
		RiskSlippageBufferBPS: 10,
		RiskRoundTripFeeBPS:   10,
	}
	// Account budget = 1000 equity × 3% = 30; friction on 1000 notional = 2, so
	// the price budget is 28 and the cap distance 2.8 — tighter than ATR=8.
	got, err := ComputeAccountProtectionDistance(
		cfg, SideLong, 100, 1000, 1000,
		2, 8, 0, 0.03, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.GovernedBy != "account_cap" || math.Abs(got.Distance-2.8) > 1e-9 || !got.NoiseConflict {
		t.Fatalf("account_cap mode must still bound the distance: %+v", got)
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
			got, err := finalizeStopLossPrice(input, result, 0.25, 0)
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
