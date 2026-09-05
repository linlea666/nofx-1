package api

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

func TestBuildCopyGuardAttributionSeparatesAttempts(t *testing.T) {
	now := time.Now()
	cycle := &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, Side: "long", LeaderEntryPrice: 100, LastObservedPrice: 95, ActualPnL: -7, BaselinePnL: -20, Fees: 1, Slippage: .5, AccountingStatus: store.CopyGuardAccountingReconciled, ClosedAt: &now}
	attempts := []*store.CopyGuardAttempt{
		{AttemptNo: 0, PnL: -5, Reconciled: true},
		{AttemptNo: 1, PnL: -4, Reconciled: true},
		{AttemptNo: 2, PnL: 2, Reconciled: true},
	}
	events := []*store.CopyGuardEvent{{Type: "WATCH_SUMMARY", Metadata: map[string]interface{}{
		"attempt_recovery": []interface{}{map[string]interface{}{"attempt_no": float64(1), "first_recovery_seconds": float64(120), "max_favorable_excursion_usd": float64(3), "max_adverse_excursion_usd": float64(6)}},
	}}}
	got := buildCopyGuardAttribution(cycle, attempts, events)
	if !got.Final || got.StopOnlyPnL != -5 || got.FirstReentryPnL != -4 || got.SecondReentryPnL != 2 || got.ReentryContribution != -2 {
		t.Fatalf("attempt attribution mixed paths: %+v", got)
	}
	if got.StopSavings != 15 || got.MissedProfit != 0 || got.LeaderDirectionReturn != -.05 {
		t.Fatalf("counterfactual attribution incorrect: %+v", got)
	}
	if got.RealizedPathMaxDrawdownUSD != 9 || got.WorstAttemptPnL != -5 || got.MaxPostStopMFEUSD != 3 || got.MaxPostStopMAEUSD != 6 || got.Attempts[1].RecoverySec == nil || *got.Attempts[1].RecoverySec != 120 {
		t.Fatalf("drawdown or attempt excursion attribution incorrect: %+v", got)
	}

	cycle.AccountingStatus = store.CopyGuardAccountingPending
	got = buildCopyGuardAttribution(cycle, attempts, events)
	if got.Final || got.StopSavings != 0 || got.MissedProfit != 0 {
		t.Fatalf("unreconciled cycle must not produce final conclusions: %+v", got)
	}
}

func TestPositionMarginAuditUsesBackendFormulaAndSanitizedPolicy(t *testing.T) {
	now := time.Now().UTC()
	cfg := store.NewCopyGuardDefaults()
	cfg.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	cfg.RiskPositionMarginStopPct = .8
	cfg.RiskTriggerPriceType = "mark"
	cfg.RiskReentryEnabled = false
	cfg.RiskReentryDecisionMode = "disabled"
	cfg.RiskMaxReentries = 0
	snapshot, err := store.EncodeCopyGuardPolicySnapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cycle := &store.CopyGuardCycle{
		ID: 7, Side: "long", PolicySnapshot: snapshot, AccountEquity: 1000,
		LastObservedPrice: 101, ProtectionCoverage: 1,
		ProtectionStatus: store.CopyGuardProtectionClamped,
		AccountingStatus: store.CopyGuardAccountingOpen, UpdatedAt: now,
	}
	artifacts := &copyGuardCycleArtifacts{
		Attempts: []*store.CopyGuardAttempt{{
			AttemptNo: 0, EntryPrice: 100, Quantity: 2, ActualLeverage: 10, ATR: 2,
			CurrentMarkPrice: 101, CurrentMarkAt: &now,
			StopAnchorEntryPrice: 100, StopAnchorLeverage: 20,
			StopAnchorInitialMargin: 5, StopAnchorPrice: 96,
			StopConfiguredMarginLossPct: .8, FinalStopPrice: 97,
			StopAnchorSource: store.CopyGuardAnchorSourceInitialFill,
			GovernedBy:       "position_margin_liquidation_clamp",
		}},
		Protection: &store.CopyGuardProtectiveOrder{
			AlgoID: "hosted-1", TriggerType: "mark", TriggerPrice: 97,
			CoverageMode: store.CopyGuardCoverageCloseAll, Status: "live", UpdatedAt: now,
		},
	}
	audit := buildPositionMarginAudit(cycle, artifacts)
	if audit == nil {
		t.Fatal("fixed cycle did not produce position-margin audit")
	}
	for label, gotWant := range map[string][2]float64{
		"raw stop":         {audit.RawFormulaStopPrice, 96},
		"anchor risk":      {audit.StopAnchorTheoreticalRiskUSD, 4},
		"current margin":   {audit.CurrentMargin, 20},
		"current risk":     {audit.CurrentStopRiskUSD, 6},
		"current margin %": {audit.CurrentMarginLossPct, .3},
		"account loss %":   {audit.CurrentAccountLossPct, .006},
		"price move %":     {audit.EquivalentPriceMovePct, .03},
		"ATR distance":     {audit.DistanceATR, 1.5},
		"execution mark":   {audit.LastMarkPrice, 101},
	} {
		if math.Abs(gotWant[0]-gotWant[1]) > 1e-12 {
			t.Fatalf("%s=%v want %v", label, gotWant[0], gotWant[1])
		}
	}
	if audit.DataQuality != "VERIFIED" || !audit.LiquidationClamped || audit.CoverageMode != store.CopyGuardCoverageCloseAll || !audit.CostsExcludedFromTrigger {
		t.Fatalf("audit semantics lost: %+v", audit)
	}

	doc := copyGuardCycleDocument(cycle, artifacts)
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "cookie") || doc["schema_version"] != 9 {
		t.Fatalf("detail contract is unsafe or stale: %s", encoded)
	}
}

func TestPositionMarginAuditRejectsFutureMarkTimestamp(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Minute)
	cfg := store.NewCopyGuardDefaults()
	cfg.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	cfg.RiskPositionMarginStopPct = .8
	cfg.RiskTriggerPriceType = "mark"
	cfg.RiskReentryEnabled = false
	cfg.RiskReentryDecisionMode = "disabled"
	cfg.RiskMaxReentries = 0
	snapshot, err := store.EncodeCopyGuardPolicySnapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cycle := &store.CopyGuardCycle{
		ID: 8, Side: "long", PolicySnapshot: snapshot, AccountEquity: 1000,
		ProtectionCoverage: 1, ProtectionStatus: store.CopyGuardProtectionVerified,
		AccountingStatus: store.CopyGuardAccountingOpen, UpdatedAt: now,
	}
	artifacts := &copyGuardCycleArtifacts{
		Attempts: []*store.CopyGuardAttempt{{
			AttemptNo: 0, EntryPrice: 100, Quantity: 1, ActualLeverage: 10,
			CurrentMarkPrice: 101, CurrentMarkAt: &future,
			StopAnchorEntryPrice: 100, StopAnchorLeverage: 10,
			StopAnchorInitialMargin: 10, StopAnchorPrice: 92,
			StopConfiguredMarginLossPct: .8, FinalStopPrice: 92,
			StopAnchorSource: store.CopyGuardAnchorSourceInitialFill,
		}},
		Protection: &store.CopyGuardProtectiveOrder{
			AlgoID: "hosted-future", TriggerType: "mark", TriggerPrice: 92,
			CoverageMode: store.CopyGuardCoverageCloseAll, Status: "live", UpdatedAt: now,
		},
	}
	audit := buildPositionMarginAudit(cycle, artifacts)
	if audit == nil || audit.DataQuality != "PARTIAL" || audit.UnscorableReason == "" {
		t.Fatalf("future-dated mark was treated as verified: %+v", audit)
	}
}
