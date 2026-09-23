package copytrade

import (
	"math"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

func decisionForGroupExit(id int64, ratio float64, closed bool) *decision.Decision {
	action, kind, target := "reduce_long", "reduce", 5.0
	if closed {
		action, kind, target = "close_long", "close", 0
	}
	return &decision.Decision{IsCopyTrade: true, CopyTradeAction: kind, Action: action, Symbol: "ETHUSDT", LeaderPosID: "p", SourceRevision: 2, LeaderPosSize: target, CloseRatio: ratio, ExecutionIntentID: id, LeaderExitScope: leaderAccountExitScope}
}

func groupedExitFixture(t *testing.T, pSize, qSize float64) (*TraderIntegration, *leaderExitExecutor, *store.FollowGroupExitBatch) {
	t.Helper()
	ti, ex, _ := leaderExitFixture(t, .5)
	ti.engine.config.FollowExitPolicyVersion = 2
	if err := ti.store.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: ti.traderID, LeaderID: "leader", LeaderPosID: "q", Symbol: "ETHUSDT", Side: "long", MarginMode: "isolated", LastKnownSize: 10, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ti.engine.leaderState.Positions = map[string]*Position{}
	if pSize > 0 {
		ti.engine.leaderState.Positions["p"] = &Position{PosID: "p", Symbol: "ETHUSDT", Side: SideLong, Size: pSize, MarginMode: "cross"}
	}
	if qSize > 0 {
		ti.engine.leaderState.Positions["q"] = &Position{PosID: "q", Symbol: "ETHUSDT", Side: SideLong, Size: qSize, MarginMode: "isolated"}
	}
	if err := ti.engine.synchronizeFollowGroups(ti.engine.leaderState); err != nil {
		t.Fatal(err)
	}
	b, err := ti.engine.followGroupReduction("p")
	if err != nil || b == nil {
		t.Fatalf("batch missing: %+v %v", b, err)
	}
	return ti, ex, b
}

func TestFollowGroupSnapshotReductionsFreezeOneCombinedQuantity(t *testing.T) {
	ti, ex, b := groupedExitFixture(t, 5, 5)
	if b.Ratio != .5 || b.FullGroupExit || len(b.Members) != 2 {
		t.Fatalf("wrong snapshot budget: %+v", b)
	}
	intents, err := ti.store.CopyTrade().ListUnfinishedExecutionIntents(ti.traderID)
	if err != nil || len(intents) != 1 {
		t.Fatal(err)
	}
	id := intents[0].ID
	if err = ti.store.CopyTrade().BindFollowGroupIntent(id, b.GroupID); err != nil {
		t.Fatal(err)
	}
	if err = ti.store.CopyTrade().BindFollowGroupExitBatch(id, *b); err != nil {
		t.Fatal(err)
	}
	ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 10)}
	d := decisionForGroupExit(id, .5, false)
	if err = ti.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 1 || ex.requests[0].Quantity != 5 {
		t.Fatalf("repeated percentage instead of batch: %+v", ex.requests)
	}
	for _, id := range []string{"p", "q"} {
		m, err := ti.store.CopyTrade().GetMappingForReconciliation(ti.traderID, id)
		if err != nil || m.SourceRevision != 2 || m.LastKnownSize != 5 {
			t.Fatalf("member not atomically acknowledged: %+v %v", m, err)
		}
	}
	if err = ti.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 1 {
		t.Fatal("completed batch repeated")
	}
}

func TestFollowGroupMemberCloseRetainsRemainingPositionProtection(t *testing.T) {
	ti, ex, b := groupedExitFixture(t, 0, 10)
	if b.FullGroupExit || b.Ratio != .5 || !b.Members[0].SourceClosed {
		t.Fatalf("member close confused with direction close: %+v", b)
	}
	cs := ti.store.CopyTrade()
	intents, _ := cs.ListUnfinishedExecutionIntents(ti.traderID)
	id := intents[0].ID
	if _, err := ti.store.DB().Exec(`UPDATE copy_trade_execution_intents SET action='close_long',leader_target_size=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.EncodeCopyGuardPolicySnapshot(store.NewCopyGuardDefaults())
	if err != nil {
		t.Fatal(err)
	}
	c, err := cs.EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: ti.traderID, LeaderID: "leader", LeaderPosID: "p", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.BindFollowGroupGuard(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ti.store.DB().Exec(`INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,cycle_id,symbol,side,margin_mode,state) VALUES(?,'p',1,?,'ETHUSDT','long','cross','MANAGED')`, ti.traderID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err = cs.BindFollowGroupIntent(id, b.GroupID); err != nil {
		t.Fatal(err)
	}
	if err = cs.BindFollowGroupExitBatch(id, *b); err != nil {
		t.Fatal(err)
	}
	ti.protectionAsync.Store(true)
	ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 10)}
	if err = ti.executeAccountLeaderExit(decisionForGroupExit(id, .5, true), true); err != nil {
		t.Fatal(err)
	}
	p, err := cs.GetPositionCustody(ti.traderID, "p")
	if err != nil || p.State != "MANAGED" {
		t.Fatalf("member released shared custody: %+v %v", p, err)
	}
	c, err = cs.GetCopyGuardCycle(c.ID)
	if err != nil || c.ClosedAt != nil {
		t.Fatalf("member closed actual protection: %+v %v", c, err)
	}
	jobs, err := cs.ListCopyGuardProtectionJobs(ti.traderID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("remaining protection not refreshed: %+v %v", jobs, err)
	}
}

func TestFollowGroupNewMemberDoesNotDiluteSnapshotReduction(t *testing.T) {
	ti, _, b := groupedExitFixture(t, 5, 10)
	ti.engine.leaderState.Positions["new"] = &Position{PosID: "new", Symbol: "ETHUSDT", Side: SideLong, Size: 100, MarginMode: "cross"}
	if err := ti.engine.synchronizeFollowGroups(ti.engine.leaderState); err != nil {
		t.Fatal(err)
	}
	b, err := ti.engine.followGroupReduction("p")
	if err != nil || math.Abs(b.Ratio-.25) > 1e-12 {
		t.Fatalf("new leg was included in old denominator: %+v %v", b, err)
	}
}

func TestFollowGroupSameSnapshotReplacementDoesNotCreateIntermediateEmptyGroup(t *testing.T) {
	ti, _, b := groupedExitFixture(t, 0, 0)
	ti.engine.leaderState.Positions["new"] = &Position{PosID: "new", Symbol: "ETHUSDT", Side: SideLong, Size: 20, MarginMode: "cross"}
	if err := ti.engine.synchronizeFollowGroups(ti.engine.leaderState); err != nil {
		t.Fatal(err)
	}
	b, err := ti.engine.followGroupReduction("p")
	if err != nil || b.FullGroupExit || b.Ratio != 1 {
		t.Fatalf("invented zero between same-frame members: %+v %v", b, err)
	}
}

func TestFollowGroupPartialBatchAndDatabaseFailureReplayOnlyRemainder(t *testing.T) {
	ti, ex, b := groupedExitFixture(t, 5, 5)
	cs := ti.store.CopyTrade()
	intents, _ := cs.ListUnfinishedExecutionIntents(ti.traderID)
	id := intents[0].ID
	if err := cs.BindFollowGroupIntent(id, b.GroupID); err != nil {
		t.Fatal(err)
	}
	if err := cs.BindFollowGroupExitBatch(id, *b); err != nil {
		t.Fatal(err)
	}
	ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 10)}
	ex.firstFill = 2
	d := decisionForGroupExit(id, .5, false)
	if err := ti.executeAccountLeaderExit(d, true); err == nil {
		t.Fatal("partial exchange fill prematurely acknowledged")
	}
	ex.positions[0]["positionAmt"] = 10.0
	if _, err := ti.store.DB().Exec(`CREATE TRIGGER reject_group_member BEFORE UPDATE ON copy_trade_position_mappings WHEN OLD.leader_pos_id='q' BEGIN SELECT RAISE(ABORT,'test group acknowledgement failure'); END`); err != nil {
		t.Fatal(err)
	}
	restarted := NewTraderIntegration(ti.traderID, ex, ti.store)
	restarted.engine = ti.engine
	if err := restarted.executeAccountLeaderExit(d, true); err == nil {
		t.Fatal("failed member commit ignored")
	}
	for _, pos := range []string{"p", "q"} {
		m, _ := cs.GetMappingForReconciliation(ti.traderID, pos)
		if m.LastKnownSize != 10 || m.SourceRevision != 1 {
			t.Fatalf("partial source acknowledgement: %+v", m)
		}
	}
	if _, err := ti.store.DB().Exec(`DROP TRIGGER reject_group_member`); err != nil {
		t.Fatal(err)
	}
	if err := restarted.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 2 || ex.requests[1].Quantity != 3 || getFloatField(ex.positions[0], "positionAmt") != 7 {
		t.Fatalf("fixed budget repeated on restart/commit retry: %+v", ex.requests)
	}
}

func TestFollowGroupReplacementBoundaryBlocksOnlyNewRiskUntilSourceFlat(t *testing.T) {
	ti, _, _ := groupedExitFixture(t, 0, 0)
	cs := ti.store.CopyTrade()
	ti.engine.leaderState.Positions["new"] = &Position{PosID: "new", Symbol: "ETHUSDT", Side: SideLong, Size: 20}
	if err := ti.engine.synchronizeFollowGroups(ti.engine.leaderState); err != nil {
		t.Fatal(err)
	}
	g, err := cs.GetFollowGroupForPosition(ti.traderID, "new")
	if err != nil || g.EntryBlockReason != "SOURCE_GROUP_BOUNDARY_UNCONFIRMED" {
		t.Fatalf("uncertain boundary admitted: %+v %v", g, err)
	}
	b, err := ti.engine.followGroupReduction("p")
	if err != nil || b == nil || b.FullGroupExit {
		t.Fatalf("entry uncertainty blocked proven old reduction: %+v %v", b, err)
	}
	i, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: ti.traderID, LeaderPosID: "new", SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.BindFollowGroupIntent(i.ID, g.ID); err != nil {
		t.Fatal(err)
	}
	if err = cs.CheckFollowSubmission(i.ID); err == nil {
		t.Fatal("uncertain new leg submitted")
	}
	ti.engine.leaderState.Positions = map[string]*Position{}
	if err = ti.engine.synchronizeFollowGroups(ti.engine.leaderState); err != nil {
		t.Fatal(err)
	}
	pending, _ := cs.GetFollowGroup(g.ID)
	if pending.SourceEnded {
		t.Fatal("new source disappearance canceled unacknowledged old exit")
	}
	// After the old batch is acknowledged, an authoritative flat image can
	// finish the boundary hold without submitting any new-source trade.
	if _, err = ti.store.DB().Exec(`UPDATE copy_trade_position_mappings SET status='closed',last_known_size=0 WHERE trader_id=?`, ti.traderID); err != nil {
		t.Fatal(err)
	}
	if err = ti.engine.synchronizeFollowGroups(ti.engine.leaderState); err != nil {
		t.Fatal(err)
	}
	g, err = cs.GetFollowGroup(g.ID)
	if err != nil || !g.SourceEnded {
		t.Fatalf("verified flat failed to end unknown boundary: %+v %v", g, err)
	}
}

func TestFollowGroupOlderSourceIncarnationCannotReduceNewCycle(t *testing.T) {
	ti, _, _ := groupedExitFixture(t, 5, 10)
	cs := ti.store.CopyTrade()
	i, _, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: ti.traderID, LeaderPosID: "p", SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.ConfirmSourceLifecycleForIntent(i.ID, 2000); err != nil {
		t.Fatal(err)
	}
	// A legacy lazy binding must not overwrite trustworthy initial intent proof.
	if err = cs.BindSourceLifecycle(ti.traderID, "p", 1000); err != nil {
		t.Fatal(err)
	}
	ti.engine.leaderState.Positions["p"].OpenedMS = 1000
	if _, err = ti.engine.followGroupReduction("p"); err == nil {
		t.Fatal("older incarnation reduced new position")
	}
	ti.engine.leaderState.Positions["p"].OpenedMS = 2000
	b, err := ti.engine.followGroupReduction("p")
	if err != nil || b == nil || b.Ratio != .25 {
		t.Fatalf("fresh current incarnation did not recover: %+v %v", b, err)
	}
}
