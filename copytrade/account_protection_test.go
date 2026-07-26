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
