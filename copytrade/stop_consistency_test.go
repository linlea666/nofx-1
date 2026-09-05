package copytrade

import (
	"testing"

	"nofx/store"
)

// The v8 stop rework demoted the position margin ceiling to a reporting
// threshold, but three independent exit paths still applied it. These tests pin
// the shared judgement so a future change cannot silently revive a
// fixed-margin exit on one path only.

func TestMigrationSkipsPositionsCopyGuardAlreadyHandles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cycle    *store.CopyGuardCycle
		wantSkip bool
	}{
		{
			name:     "verified stop covering the position",
			cycle:    &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ProtectionStatus: store.CopyGuardProtectionVerified, ProtectionCoverage: 1},
			wantSkip: true,
		},
		{
			// CLAMPED is tighter than policy wants but it is a live stop.
			name:     "clamped stop covering the position",
			cycle:    &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ProtectionStatus: store.CopyGuardProtectionClamped, ProtectionCoverage: 0.9995},
			wantSkip: true,
		},
		{
			name:     "verified stop covering only part of the position",
			cycle:    &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ProtectionStatus: store.CopyGuardProtectionVerified, ProtectionCoverage: 0.8},
			wantSkip: false,
		},
		{
			// A deliberate policy outcome: keep following under an alert. The
			// migration path must not override it.
			name:     "unprotected warning disposition",
			cycle:    &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ProtectionStatus: store.CopyGuardProtectionUnprotectedWarning},
			wantSkip: true,
		},
		{
			name:     "unprotectable disposition",
			cycle:    &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ProtectionStatus: store.CopyGuardProtectionUnprotectable},
			wantSkip: true,
		},
		{
			name:     "forced exit already in flight",
			cycle:    &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ProtectionStatus: store.CopyGuardProtectionForcedExitPending},
			wantSkip: true,
		},
		{
			// The genuine naked-position gap this reconciler exists for.
			name:     "pending with no live stop",
			cycle:    &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ProtectionStatus: store.CopyGuardProtectionPending},
			wantSkip: false,
		},
		{
			name:     "degraded with no live stop",
			cycle:    &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ProtectionStatus: store.CopyGuardProtectionDegraded, ProtectionCoverage: 1},
			wantSkip: false,
		},
		{
			name:     "unknown with no live stop",
			cycle:    &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ProtectionStatus: store.CopyGuardProtectionUnknown},
			wantSkip: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skip := cycleFullyProtected(tc.cycle) || cycleProtectionVerdictReached(tc.cycle)
			if skip != tc.wantSkip {
				t.Fatalf("skip = %v, want %v for status=%s coverage=%.4f",
					skip, tc.wantSkip, tc.cycle.ProtectionStatus, tc.cycle.ProtectionCoverage)
			}
		})
	}
	if cycleFullyProtected(nil) || cycleProtectionVerdictReached(nil) {
		t.Fatal("a missing cycle is not protected and has no verdict")
	}
}

// The AI reentry path and ordinary copying must reach the same verdict about
// the same stop distance. While the AI floor was hardcoded at 0.5 ATR the two
// disagreed for every distance between 0.5 and the configured floor.
func TestAIAndOrdinaryPathsShareTheStructuralFloor(t *testing.T) {
	const atr, entry = 2.0, 100.0
	for _, minRatio := range []float64{0.5, 1.0, 1.5} {
		for _, ratio := range []float64{0.3, 0.6, 0.9, 1.2, 1.8} {
			cfg := &CopyConfig{RiskSlippageBufferBPS: 5, RiskRoundTripFeeBPS: 10,
				RiskLiquidationBufferATR: 0.5, RiskMinStopATRRatio: minRatio}
			distance := atr * ratio
			_, aiErr := BuildAIProtectionPlanFromStop(cfg, AIProtectionPlanInput{
				Side: SideLong, EntryPrice: entry, CurrentPrice: entry,
				AIStopPrice: entry - distance, ATR: atr, Equity: 1000,
				AvailableRiskUSD: 500, PlannedNotional: 100, PriceTickSize: 0.001, Leverage: 5,
			})
			aiRejected := aiErr != nil
			ordinaryRejected := ratio < minRatio
			if aiRejected != ordinaryRejected {
				t.Fatalf("floor=%.1f distance=%.1f ATR: AI rejected=%v, structural floor rejects=%v (err=%v)",
					minRatio, ratio, aiRejected, ordinaryRejected, aiErr)
			}
		}
	}
}

// ValidateStoredRiskPolicy is the only validation gate on the three config save
// endpoints. It built its CopyConfig field by field, so a newly added field was
// silently unvalidated: an out-of-range value saved successfully and then made
// StartCopyTrading refuse to boot the trader.
func TestValidateStoredRiskPolicyRejectsOutOfRangeMinStopATRRatio(t *testing.T) {
	base := func() *store.CopyTradeConfig {
		c := &store.CopyTradeConfig{
			ProviderType:        string(ProviderOKX),
			RiskStopLossEnabled: true,
			RiskPolicyVersion:   4,
		}
		c.FillRiskDefaults()
		return c
	}

	ok := base()
	if err := ValidateStoredRiskPolicy(ok); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}

	tooWide := base()
	tooWide.RiskMinStopATRRatio = 4
	if err := ValidateStoredRiskPolicy(tooWide); err == nil {
		t.Fatal("an out-of-range floor must be rejected at save time, not at startup")
	}

	disabled := base()
	disabled.RiskMinStopATRRatio = 0
	disabled.RiskMinStopATRRatioExplicit = true
	if err := ValidateStoredRiskPolicy(disabled); err != nil {
		t.Fatalf("0 is an explicit choice to disable the floor: %v", err)
	}
}
