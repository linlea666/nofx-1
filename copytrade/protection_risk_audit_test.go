package copytrade

import (
	"path/filepath"
	"testing"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

// Ordinary leader-following must record the same risk basis as AI reentry.
// While the audit write sat behind an AI-only branch, all 496 production
// attempts carried zeroes in actual_leverage and governed_by, which are the
// only evidence of whether ATR, the margin ceiling or the liquidation clamp
// chose a stop — without them no stop policy change can be evaluated.
func TestOrdinaryCopyWritesRiskAudit(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mock := &mockStopMgr{byID: map[string]*trader.ProtectiveStopOrder{}}
	executor := &stopMgrExecutor{mockStopMgr: mock, positions: []map[string]interface{}{
		{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "entryPrice": 1717.33, "positionAmt": 0.128, "posId": "follower-pos"},
	}}
	ti := NewTraderIntegration("trader-1", executor, st)
	ti.engine = &Engine{config: &CopyConfig{ProviderType: ProviderOKX, LeaderID: "leader", RiskPolicyVersion: 4, RiskStopLossEnabled: true, RiskTriggerPriceType: "mark"}}
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 1717.33, 219.8, 0.128, 7.59); err != nil {
		t.Fatal(err)
	}
	mock.byID["new-algo"] = &trader.ProtectiveStopOrder{AlgoID: "new-algo", ClientID: "cg", PositionSide: "long", MarginMode: "cross", Quantity: 0.128, TriggerPrice: 1711.63, State: "live"}

	// No CopyTradeAction and no reentry wording: this is plain leader copying.
	dec := &decision.Decision{Symbol: "ETHUSDT", Action: "open_long", LeaderPosID: "leader-pos", MarginMode: "cross", EntryPrice: 1717.33, Leverage: 20}
	ti.upsertV4Protection(dec, "long", 0.128, 1717.33, &StopLossCalcResult{
		SLPrice: 1711.63, TickSize: 0.01, QuantityStep: 0.001,
		GovernedBy: "atr", ExpectedMarginLossPct: 0.42,
	})

	attempts, err := st.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) == 0 {
		t.Fatalf("attempt row missing: %v %+v", err, attempts)
	}
	got := attempts[0]
	if got.ActualLeverage != 20 {
		t.Fatalf("actual_leverage must be recorded for ordinary copy: %+v", got)
	}
	// 1717.33 × 0.128 / 20x.
	if wantMargin := 1717.33 * 0.128 / 20; got.InitialMarginBasis < wantMargin-1e-6 || got.InitialMarginBasis > wantMargin+1e-6 {
		t.Fatalf("initial_margin_basis = %.6f, want %.6f", got.InitialMarginBasis, wantMargin)
	}
	if got.GovernedBy != "atr" || got.ExpectedPositionLossPct != 0.42 {
		t.Fatalf("stop provenance must be recorded: %+v", got)
	}
	if got.FinalStopPrice != 1711.63 {
		t.Fatalf("final_stop_price = %.4f, want the armed trigger", got.FinalStopPrice)
	}
	// AI sizing fields describe a promotion that never happened here; inventing
	// values would make ordinary copies look like AI-resized entries in the
	// funnel stats.
	if got.PlannedNotional != 0 || got.PromotedNotional != 0 || got.PromotionReason != "" || got.AIStopPrice != 0 {
		t.Fatalf("AI-only fields must stay zero on the ordinary path: %+v", got)
	}
}
