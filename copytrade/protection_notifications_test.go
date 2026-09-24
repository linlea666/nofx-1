package copytrade

import (
	"testing"
	"time"

	"nofx/notifier"
	"nofx/store"
	"nofx/trader"
)

func TestProtectionFailureKindsShareIncidentAndFlatQueueIsCanceled(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	c.OpenedAt = time.Now().Add(-time.Minute)
	ex := &positionMarginLifecycleExecutor{positions: []map[string]interface{}{exitPosition("cross", "current", "long", 1)}}
	ti := NewTraderIntegration("t", ex, st)
	if err := st.CopyTrade().UpdateCopyGuardProtectionHealth(c.ID, store.CopyGuardProtectionDegraded, 0, "cannot verify protection", "", "", false); err != nil {
		t.Fatal(err)
	}
	captured := &notifier.CaptureNotifier{}
	t.Cleanup(notifier.SetGlobalForTesting(captured, false))
	ti.notifyProtection(c, "protection failed", "detail", "unprotected_warning")
	ti.notifyProtection(c, "repeated re-arm", "detail", "rearm_throttled")
	ti.notifyProtection(c, "missing too long", "detail", "missing_escalation")
	if len(captured.Alerts) != 1 {
		t.Fatalf("one pending incident generated %d emails", len(captured.Alerts))
	}
	alert := captured.Alerts[0]
	if alert.BeforeSend == nil || !alert.BeforeSend() || alert.StatusHook == nil {
		t.Fatal("missing durable relevance/delivery hook")
	}
	// A subsequent leader reduction is still waiting on exchange evidence.
	if _, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 2, Action: "reduce_long", Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: .5}); err != nil {
		t.Fatal(err)
	}
	ex.positions = nil
	if !ti.confirmCopyGuardFollowerAbsent(c) {
		t.Fatal("fresh flatness unavailable")
	}
	if alert.BeforeSend() {
		t.Fatal("queued naked-position mail survived a confirmed flat snapshot")
	}
	ti.notifyProtection(c, "stale retry", "detail", "unprotected_warning")
	ti.notifyProtection(c, "stale mark-price warning", "detail", "position_margin_mark_unavailable_0_periodic_safety_snapshot")
	if len(captured.Alerts) != 1 {
		t.Fatal("flat pending reconciliation sent another warning")
	}
	ti.notifyProtection(c, "settlement pending", "confirmed exit evidence unavailable", "accounting_unrecoverable")
	if len(captured.Alerts) != 2 {
		t.Fatal("flatness suppressed a legitimate accounting alert")
	}
}

func TestFlatReconcilingRetiresOnlyOldProtectiveOrder(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	if _, err := st.DB().Exec(`UPDATE copy_guard_cycles SET opened_at=datetime('now','-1 minute') WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	// Exercise the older lifecycle shape without an initial-intent binding too.
	if _, err := st.DB().Exec(`UPDATE copy_guard_cycles SET initial_intent_id=0 WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 2, Action: "reduce_long", Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: .5}); err != nil {
		t.Fatal(err)
	}
	ex := &positionMarginLifecycleExecutor{order: &trader.ProtectiveStopOrder{AlgoID: "old-stop", ClientID: "old-client", Symbol: "ETHUSDT", PositionSide: "long", MarginMode: "cross", State: "live", Quantity: 1, TriggerPrice: 90}}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: c.ID, TraderID: "t", AlgoID: "old-stop", AlgoClientID: "old-client", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 1, TriggerPrice: 90, Status: "live"}); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration("t", ex, st)
	ti.pollV4ProtectiveStops()
	current, _ := st.CopyTrade().GetCopyGuardCycle(c.ID)
	order, _ := st.CopyTrade().GetCopyGuardProtectiveOrder(c.ID)
	if current.ProtectionStatus != store.CopyGuardProtectionFlatReconciling || order.Status != "canceled" || ex.order.State != "canceled" || ex.closeCalls != 0 {
		t.Fatalf("flat protection not retired independently: %+v %+v", current, order)
	}
	// A later manual position cannot trigger the obsolete stop or a new close.
	ex.positions = []map[string]interface{}{exitPosition("cross", "manual-new", "long", 2)}
	ti.pollV4ProtectiveStops()
	if ex.closeCalls != 0 || len(ex.requests) != 0 {
		t.Fatal("old stop controlled new manual position")
	}
}
