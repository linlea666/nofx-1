package copytrade

import (
	"testing"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

func TestCopyGuardSettlementMissingContractUnitsCannotBypassEvidence(t *testing.T) {
	ti := &TraderIntegration{}
	if err := ti.verifyCopyGuardSettlement(nil, 0, &trader.ClosedPnLRecord{RequiresScopedSettlementProof: true}); err == nil {
		t.Fatal("missing OKX unit conversion was treated as a verified legacy settlement")
	}
}

func TestLiquidationOnlyKeepsManualStopAndUnknownDataNeverExits(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	policy := store.NewCopyGuardDefaults()
	policy.RiskStopLossEnabled = false
	policy.RiskLiquidationGuardEnabled = true
	policy.RiskProtectionMode = store.RiskProtectionModeATRStructure
	raw, err := store.EncodeCopyGuardPolicySnapshot(policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_guard_cycles SET policy_snapshot=? WHERE id=?`, raw, c.ID); err != nil {
		t.Fatal(err)
	}
	ex := &positionMarginLifecycleExecutor{}
	ex.setPosition(100, 1, 10, 100, 90)
	ti := controlIntegration(st, ex)
	ti.engine.config.RiskStopLossEnabled = false
	ti.engine.config.RiskLiquidationGuardEnabled = true
	ti.engine.config.FollowExitPolicyVersion = 2
	d := &decision.Decision{IsCopyTrade: true, LeaderPosID: "p", Symbol: "ETHUSDT", Action: "open_long", MarginMode: "cross", Leverage: 10}
	ti.refreshStopLossAfterExecute(d)
	if ex.order == nil || ex.order.TriggerPrice < 90.2 || ex.order.TriggerType != "mark" {
		t.Fatalf("independent guard missing: %+v", ex.order)
	}
	// Manual tight and wide revisions share the same order and persist on restart.
	for _, price := range []float64{95, 93} {
		ex.order.TriggerPrice = price
		ti.refreshStopLossAfterExecute(d)
		if ex.order.TriggerPrice != price {
			t.Fatalf("manual override reverted %+v", ex.order)
		}
	}
	ti = controlIntegration(st, ex)
	ti.refreshStopLossAfterExecute(d)
	if ex.order.TriggerPrice != 93 {
		t.Fatal("manual liquidation-only stop lost at restart")
	}
	ex.positions[0]["liquidationPrice"] = 0.0
	ti.refreshStopLossAfterExecute(d)
	if ex.closeCalls != 0 || ex.order.TriggerPrice != 93 {
		t.Fatal("missing liquidation price changed protection or exited")
	}
	ex.positions[0]["liquidationPrice"] = 96.0
	ti.refreshStopLossAfterExecute(d)
	if ex.order.TriggerPrice <= 96 || ex.order.TriggerPrice >= 100 {
		t.Fatalf("manual override exceeded safety line: %+v", ex.order)
	}
}

func TestExitContractDisablesReentryWithoutChangingFrozenAnchorPolicy(t *testing.T) {
	p := store.NewCopyGuardDefaults()
	p.RiskReentryEnabled = true
	p.RiskReentryDecisionMode = "ai_guarded"
	p.RiskMaxReentries = 2
	raw, err := store.EncodeCopyGuardPolicySnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg := copyGuardLifecycleConfig(&store.CopyGuardCycle{PolicySnapshot: raw}, &CopyConfig{FollowExitPolicyVersion: 2})
	if cfg.RiskReentryEnabled || cfg.RiskManualReentryEnabled || cfg.RiskMaxReentries != 0 || cfg.RiskReentryDecisionMode != "disabled" {
		t.Fatalf("same-cycle reentry enabled: %+v", cfg)
	}
	if cfg.RiskProtectionMode != p.RiskProtectionMode || cfg.RiskPositionMarginStopPct != p.RiskPositionMarginStopPct {
		t.Fatal("exit contract changed the protection policy")
	}
}
