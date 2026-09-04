package store

import (
	"path/filepath"
	"testing"
)

func TestCopyGuardRiskExitTransactionsAreIdempotentAndFlatGated(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "risk-exit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "risk-trader", LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", OpenPrice: 100,
		OpenSizeUSD: 100, LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	policy := NewCopyGuardDefaults()
	policy.TraderID, policy.ProviderType = "risk-trader", "okx"
	policySnapshot, err := EncodeCopyGuardPolicySnapshot(policy)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "risk-trader", LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing,
		PolicySnapshot: policySnapshot, FollowerEntryPrice: 100, FollowerNotional: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.OpenCopyGuardAttempt(cycle.ID, 0, 100, 100, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpsertCopyGuardProtectiveOrder(&CopyGuardProtectiveOrder{
		CycleID: cycle.ID, TraderID: "risk-trader", AlgoID: "stop-1", Symbol: "ETHUSDT",
		Side: "long", MarginMode: "cross", Quantity: 1, QuantityStep: .01,
		TriggerPrice: 92, TriggerType: "mark", Status: "live",
	}); err != nil {
		t.Fatal(err)
	}
	begin := CopyGuardRiskExitBegin{
		CycleID: cycle.ID, TraderID: "risk-trader", LeaderPosID: "leader-pos", AttemptNo: 0,
		TriggerPrice: 92, Quantity: 1, AlgoID: "stop-1", TriggerSource: "exchange_hosted",
		LeaderPnL: -8, LeaderSize: 1, AddCount: 2,
	}
	created, err := cs.BeginCopyGuardRiskExit(begin)
	if err != nil || !created {
		t.Fatalf("begin risk exit: created=%v err=%v", created, err)
	}
	if created, err = cs.BeginCopyGuardRiskExit(begin); err != nil || created {
		t.Fatalf("idempotent begin: created=%v err=%v", created, err)
	}
	mapping, err := cs.GetMapping("risk-trader", "leader-pos")
	if err != nil || mapping.Status != MappingStatusStoppedByRisk || mapping.LeaderPnLAtStop != -8 {
		t.Fatalf("mapping was not gated atomically: mapping=%+v err=%v", mapping, err)
	}
	cycle, err = cs.GetCopyGuardCycle(cycle.ID)
	if err != nil || cycle.Status != CopyGuardStopPendingFlat || cycle.ProtectionStatus != CopyGuardProtectionTriggered {
		t.Fatalf("cycle did not enter pending-flat atomically: cycle=%+v err=%v", cycle, err)
	}
	attempts, err := cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != "OPEN" {
		t.Fatalf("begin must not finalize attempt before flat: attempts=%+v err=%v", attempts, err)
	}
	changed, err := cs.MarkCopyGuardRiskExitPartial(cycle.ID, 0, .2, 91.8, nil)
	if err != nil || !changed {
		t.Fatalf("mark partial: changed=%v err=%v", changed, err)
	}
	if changed, err = cs.MarkCopyGuardRiskExitPartial(cycle.ID, 0, .2, 91.8, nil); err != nil || changed {
		t.Fatalf("idempotent partial: changed=%v err=%v", changed, err)
	}
	final := CopyGuardRiskExitFinalize{
		CopyGuardRiskExitBegin: begin, ATR: 2, ExitPrice: 91.7, PnL: -8.3, Fee: .1,
		Slippage: .2, ActualOrderID: "fill-1", VenueState: "filled",
	}
	finalized, err := cs.FinalizeCopyGuardRiskExit(final)
	if err != nil || !finalized {
		t.Fatalf("finalize risk exit: finalized=%v err=%v", finalized, err)
	}
	if finalized, err = cs.FinalizeCopyGuardRiskExit(final); err != nil || finalized {
		t.Fatalf("idempotent finalize: finalized=%v err=%v", finalized, err)
	}
	cycle, err = cs.GetCopyGuardCycle(cycle.ID)
	if err != nil || cycle.Status != CopyGuardStoppedWatching || cycle.StopCount != 1 || cycle.ActualPnL != -8.3 {
		t.Fatalf("risk exit final state mismatch: cycle=%+v err=%v", cycle, err)
	}
	attempts, err = cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != "STOPPED" || attempts[0].ExitOrderID != "fill-1" {
		t.Fatalf("final attempt mismatch: attempts=%+v err=%v", attempts, err)
	}
	var pendingEvents, stopEvents, flatEvents int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='STOP_PENDING_FLAT'`, cycle.ID).Scan(&pendingEvents); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='STOP_TRIGGERED'`, cycle.ID).Scan(&stopEvents); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='STOP_FLAT_CONFIRMED'`, cycle.ID).Scan(&flatEvents); err != nil {
		t.Fatal(err)
	}
	if pendingEvents != 1 || stopEvents != 1 || flatEvents != 1 {
		t.Fatalf("risk exit events duplicated: pending=%d stop=%d flat=%d", pendingEvents, stopEvents, flatEvents)
	}

	// Older builds could persist STOP_TRIGGERED before proving flat. Adopting
	// that row must move it into the same pending/partial state machine rather
	// than leaving two competing trigger states alive.
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "risk-trader", LeaderID: "leader", LeaderPosID: "legacy-triggered",
		Symbol: "BTCUSDT", Side: "short", MarginMode: "cross", OpenPrice: 100,
		OpenSizeUSD: 100, LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	legacy, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "risk-trader", LeaderID: "leader", LeaderPosID: "legacy-triggered",
		Symbol: "BTCUSDT", Side: "short", MarginMode: "cross", Status: CopyGuardFollowing,
		PolicySnapshot: policySnapshot, FollowerEntryPrice: 100, FollowerNotional: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.OpenCopyGuardAttempt(legacy.ID, 0, 100, 100, 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec(`UPDATE copy_guard_cycles SET status=? WHERE id=?`, CopyGuardStopTriggered, legacy.ID); err != nil {
		t.Fatal(err)
	}
	legacyBegin := CopyGuardRiskExitBegin{
		CycleID: legacy.ID, TraderID: "risk-trader", LeaderPosID: "legacy-triggered",
		AttemptNo: 0, TriggerPrice: 108, Quantity: 1, TriggerSource: "legacy_trigger_adoption",
	}
	if created, err = cs.BeginCopyGuardRiskExit(legacyBegin); err != nil || !created {
		t.Fatalf("adopt legacy triggered risk exit: created=%v err=%v", created, err)
	}
	legacy, err = cs.GetCopyGuardCycle(legacy.ID)
	if err != nil || legacy.Status != CopyGuardStopPendingFlat {
		t.Fatalf("legacy triggered state was not normalized: cycle=%+v err=%v", legacy, err)
	}
	if changed, err = cs.MarkCopyGuardRiskExitPartial(legacy.ID, 0, .001, 108.1,
		map[string]interface{}{"classification": "STOP_DUST_RESIDUAL"}); err != nil || !changed {
		t.Fatalf("mark dust residual: changed=%v err=%v", changed, err)
	}
	var dustEvents int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='STOP_DUST_RESIDUAL'`, legacy.ID).Scan(&dustEvents); err != nil {
		t.Fatal(err)
	}
	if dustEvents != 1 {
		t.Fatalf("dust residual classification was lost: events=%d", dustEvents)
	}
}
