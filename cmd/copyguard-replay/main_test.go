package main

import (
	"math"
	"testing"
)

func TestAnalyzeKeepsFeesInsideRealizedPnLAndSplitsAttempts(t *testing.T) {
	exports := []cycleExport{{
		SchemaVersion: 3,
		Cycle:         replayCycle{AccountingStatus: "RECONCILED", ClosedAt: "2026-07-01T00:00:00Z", ActualPnL: -7, BaselinePnL: -20, Fees: 1, StopCount: 2, ReentryCount: 1},
		Attempts:      []replayAttempt{{AttemptNo: 0, PnL: -5, Fee: .6, Reconciled: true}, {AttemptNo: 1, PnL: -2, Fee: .4, Reconciled: true}},
	}}
	r := analyze(exports)
	if r.ActualCopyGuardPnL != -7 || r.StopOnlyPnL != -5 || r.AllReentryContribution != -2 {
		t.Fatalf("unexpected attribution: %+v", r)
	}
	if r.NetGuardEffect != 13 || r.AttemptAccountingMismatch != 0 {
		t.Fatalf("unexpected reconciliation: %+v", r)
	}
	if r.Fees != 1 || math.Abs(r.Attempts[1].Sum-(-2)) > 1e-9 {
		t.Fatalf("fee was double-subtracted or attempt split failed: %+v", r)
	}
}

func TestAnalyzeExcludesUnreconciledAndDoesNotInventAIReplay(t *testing.T) {
	exports := []cycleExport{
		{SchemaVersion: 3, Cycle: replayCycle{AccountingStatus: "PENDING", ClosedAt: "x", ActualPnL: 99}},
		{SchemaVersion: 3, Cycle: replayCycle{AccountingStatus: "RECONCILED", ClosedAt: "x"}},
	}
	r := analyze(exports)
	if r.FinalCycles != 1 || r.ExcludedCycles != 1 {
		t.Fatalf("final-cycle filter failed: %+v", r)
	}
	if r.AIReplayComplete || r.AIReplayCycles != 0 {
		t.Fatalf("missing AI history must not be inferred: %+v", r)
	}
}
