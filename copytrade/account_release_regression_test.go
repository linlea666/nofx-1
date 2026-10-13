package copytrade

import (
	"errors"
	"testing"

	"nofx/store"
)

type flatProtectedExecutor struct {
	flatExecutor
	mockStopMgr
}

func TestMarkCrossingFinalizationPreservesVerifiedProtectionTerminalState(t *testing.T) {
	for _, status := range []string{"live", "canceled", "order_failed", "mark_crossed_exit"} {
		t.Run(status, func(t *testing.T) {
			st, cycle, _ := seedHardeningFill(t)
			if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{
				CycleID: cycle.ID, TraderID: "t", AlgoID: "stop", Symbol: cycle.Symbol, Side: cycle.Side, MarginMode: cycle.MarginMode, Quantity: 1, Status: status,
			}); err != nil {
				t.Fatal(err)
			}
			executor := &flatProtectedExecutor{}
			ti := NewTraderIntegration("t", executor, st)
			if !ti.finalizePositionMarginStopExit(cycle, 92) {
				t.Fatal("flat exit did not finalize")
			}
			if status == "mark_crossed_exit" && len(executor.canceled) != 1 {
				t.Fatal("unknown legacy status was accepted without venue confirmation")
			}
			order, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
			if err != nil || order.Status != "canceled" {
				t.Fatalf("verified cancellation overwritten: %+v %v", order, err)
			}
			// Repeated completion cannot change the final order outcome either.
			if !ti.finalizePositionMarginStopExit(cycle, 92) {
				t.Fatal("repeat exit failed")
			}
			order, err = st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
			if err != nil || order.Status != "canceled" {
				t.Fatalf("repeat overwrote terminal: %+v %v", order, err)
			}
		})
	}
}

func TestUnknownProtectionStateCannotBecomeCancelledWithoutVenueEvidence(t *testing.T) {
	st, cycle, _ := seedHardeningFill(t)
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{
		CycleID: cycle.ID, TraderID: "t", AlgoID: "stop", Symbol: cycle.Symbol, Side: cycle.Side, MarginMode: cycle.MarginMode, Quantity: 1, Status: "mark_crossed_exit",
	}); err != nil {
		t.Fatal(err)
	}
	executor := &flatProtectedExecutor{mockStopMgr: mockStopMgr{cancelErr: errors.New("venue timeout")}}
	ti := NewTraderIntegration("t", executor, st)
	if ti.finalizePositionMarginStopExit(cycle, 92) {
		t.Fatal("unverified cancellation accepted")
	}
	order, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil || order.Status != "mark_crossed_exit" {
		t.Fatalf("invented cancellation: %+v %v", order, err)
	}
}
