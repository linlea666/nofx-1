package api

import (
	"testing"
	"time"

	"nofx/store"
)

func TestBuildCopyGuardAttributionSeparatesAttempts(t *testing.T) {
	now := time.Now()
	cycle := &store.CopyGuardCycle{Side: "long", LeaderEntryPrice: 100, LastObservedPrice: 95, ActualPnL: -7, BaselinePnL: -20, Fees: 1, Slippage: .5, AccountingStatus: store.CopyGuardAccountingReconciled, ClosedAt: &now}
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
