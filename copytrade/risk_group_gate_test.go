package copytrade

import (
	"errors"
	"testing"

	"nofx/decision"
	"nofx/store"
)

func TestRiskGroupGateBlocksPeerDuringPersistenceFailureButPermitsNewRound(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	e.config.FollowExitPolicyVersion = 2
	cycle := seedRiskExitGateLifecycle(t, st, e.traderID)
	cs := st.CopyTrade()
	group, err := cs.EnsureFollowGroupMember(e.traderID, "okx", "leader", "ETHUSDT", "long", "leader-pos", 1, store.MappingStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cs.EnsureFollowGroupMember(e.traderID, "okx", "leader", "ETHUSDT", "long", "peer", 2, store.MappingStatusActive); err != nil {
		t.Fatal(err)
	}
	if err = cs.BindFollowGroupGuard(cycle.ID); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration(e.traderID, &flatExecutor{}, st)
	ti.engine = e
	// Inject a failure AFTER the trusted trigger, specifically in the group
	// transaction. The pre-existing in-memory fence must cover the sibling.
	if _, err = st.DB().Exec(`CREATE TRIGGER fail_group_risk BEFORE UPDATE OF risk_blocked ON copy_trade_follow_groups BEGIN SELECT RAISE(ABORT,'group storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err = ti.establishRiskExitGate(store.CopyGuardRiskExitBegin{CycleID: cycle.ID, TraderID: e.traderID, LeaderPosID: "leader-pos", TriggerSource: "test", Quantity: 1}); err == nil {
		t.Fatal("fault injection did not reach group risk write")
	}
	add := &decision.Decision{LeaderPosID: "peer", Symbol: "ETHUSDT", Action: "open_long", CopyTradeAction: "add"}
	if !ti.riskExitGateActive(add) {
		t.Fatal("sibling escaped in-memory risk fence")
	}
	if err = ti.executeDecisionUnderRiskExitGate(add, add); !errors.Is(err, errCopyGuardRiskExitGate) {
		t.Fatalf("peer reached executor: %v", err)
	}
	if ti.riskExitGateActive(&decision.Decision{LeaderPosID: "peer", Symbol: "ETHUSDT", Action: "close_long"}) {
		t.Fatal("risk stop blocked valid leader exit")
	}
	if _, err = st.DB().Exec(`DROP TRIGGER fail_group_risk`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_follow_groups SET source_ended=1 WHERE id=?`, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.EnsureFollowGroupMember(e.traderID, "okx", "leader", "ETHUSDT", "long", "peer", 3, ""); err != nil {
		t.Fatal(err)
	}
	if ti.riskExitGateActive(add) {
		t.Fatal("old group gate blocked genuinely new round with reused source ID")
	}
}
