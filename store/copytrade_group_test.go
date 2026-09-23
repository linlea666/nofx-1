package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func groupStoreFixture(t *testing.T) (*Store, *FollowGroup, int64) {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "groups.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs := st.CopyTrade()
	var g *FollowGroup
	for _, id := range []string{"a", "b"} {
		if err = cs.SavePositionMapping(&CopyTradePositionMapping{TraderID: "t", LeaderID: "l", LeaderPosID: id, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 10, OpenedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		g, err = cs.EnsureFollowGroupMember("t", "okx", "l", "ETHUSDT", "long", id, 1000, MappingStatusActive)
		if err != nil {
			t.Fatal(err)
		}
	}
	policy, err := EncodeCopyGuardPolicySnapshot(NewCopyGuardDefaults())
	if err != nil {
		t.Fatal(err)
	}
	c, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "t", LeaderID: "l", LeaderPosID: "a", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: policy})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.BindFollowGroupGuard(c.ID); err != nil {
		t.Fatal(err)
	}
	return st, g, c.ID
}

func TestFollowGroupPauseResumeDoesNotClearRiskBan(t *testing.T) {
	st, g, cycle := groupStoreFixture(t)
	cs := st.CopyTrade()
	if err := cs.BlockFollowGroupRiskByCycle(cycle); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.MarkManualStopped("t", "a"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		m, err := cs.GetMappingForReconciliation("t", id)
		if err != nil || m.Status != MappingStatusManualStopped {
			t.Fatalf("group pause missed %s: %+v %v", id, m, err)
		}
	}
	g, err := cs.GetFollowGroup(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.ResumeFollowGroup("t", g.ID, g.Version, map[string]float64{"a": 7, "b": 15}); err != nil {
		t.Fatal(err)
	}
	g, err = cs.GetFollowGroup(g.ID)
	if err != nil || g.Paused || !g.RiskBlocked {
		t.Fatalf("resume erased risk gate: %+v %v", g, err)
	}
	for index, action := range []string{"open_long", "reduce_long"} {
		i, _, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "a", SourceRevision: int64(3 + index), Action: action, Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: 5})
		if err != nil {
			t.Fatal(err)
		}
		if err = cs.BindFollowGroupIntent(i.ID, g.ID); err != nil {
			t.Fatal(err)
		}
		err = cs.CheckFollowSubmission(i.ID)
		if action == "open_long" && !errors.Is(err, ErrFollowControlChanged) {
			t.Fatalf("risk blocked add accepted: %v", err)
		}
		if action == "reduce_long" && err != nil {
			t.Fatalf("risk block vetoed valid leader reduction: %v", err)
		}
	}
}

func TestFollowGroupPausedSourceEndRetainsProtection(t *testing.T) {
	st, g, cycle := groupStoreFixture(t)
	cs := st.CopyTrade()
	if _, err := cs.MarkManualStopped("t", "a"); err != nil {
		t.Fatal(err)
	}
	if err := cs.ResetFollowGroupObservations("t"); err != nil {
		t.Fatal(err)
	}
	if err := cs.EndInactiveFollowGroups("t"); err != nil {
		t.Fatal(err)
	}
	g, err := cs.GetFollowGroup(g.ID)
	if err != nil || !g.SourceEnded || !g.Paused {
		t.Fatalf("source terminal state missing: %+v %v", g, err)
	}
	if keep, err := cs.FollowGroupKeepsProtection(cycle); err != nil || !keep {
		t.Fatalf("paused source close canceled follower protection: %v %v", keep, err)
	}
	if err := cs.ResumeFollowGroup("t", g.ID, g.Version, map[string]float64{"a": 10}); !errors.Is(err, ErrFollowControlChanged) {
		t.Fatalf("ended source resumed: %v", err)
	}
}

func TestFollowGroupRiskBlockedNewChildKeepsExitSubscription(t *testing.T) {
	st, g, cycle := groupStoreFixture(t)
	cs := st.CopyTrade()
	if err := cs.BlockFollowGroupRiskByCycle(cycle); err != nil {
		t.Fatal(err)
	}
	newGroup, err := cs.EnsureFollowGroupMember("t", "okx", "l", "ETHUSDT", "long", "new", 2000, "")
	if err != nil || newGroup.ID != g.ID {
		t.Fatalf("new child escaped old risk group: %+v %v", newGroup, err)
	}
	if err = cs.ObserveFollowGroupMember(g.ID, "new", "isolated", 5); err != nil {
		t.Fatal(err)
	}
	m, err := cs.GetMappingForReconciliation("t", "new")
	if err != nil || m.Status != MappingStatusStoppedByRisk || m.LastKnownSize != 5 {
		t.Fatalf("risk child lost future reductions: %+v %v", m, err)
	}
}

func TestFollowGroupProtectionResolutionUsesMarginWithoutNewAnchor(t *testing.T) {
	st, _, cycle := groupStoreFixture(t)
	c, err := st.CopyTrade().GetFollowGroupProtectionCycle("t", "b", "cross")
	if err != nil || c.ID != cycle {
		t.Fatalf("shared scope anchor replaced: %+v %v", c, err)
	}
	if _, err = st.CopyTrade().GetFollowGroupProtectionCycle("t", "b", "isolated"); err == nil {
		t.Fatal("another margin scope borrowed original anchor")
	}
}

func TestFollowGroupSnapshotFailureCannotPublishFalseFlat(t *testing.T) {
	st, g, _ := groupStoreFixture(t)
	cs := st.CopyTrade()
	if _, err := cs.MarkManualStopped("t", "a"); err != nil {
		t.Fatal(err)
	}
	if err := cs.PublishFollowGroupSnapshot("t", []FollowGroupObservation{{g.ID, "a", "cross", 10}, {g.ID, "b", "cross", 10}}); err != nil {
		t.Fatal(err)
	}
	// A failure after the reset must preserve the preceding complete image.
	if err := cs.PublishFollowGroupSnapshot("t", []FollowGroupObservation{{g.ID, "a", "cross", 5}, {g.ID, "missing", "cross", 1}}); err == nil {
		t.Fatal("accepted invalid snapshot member")
	}
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_follow_group_members WHERE group_id=? AND observed_size=10`, g.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("partial image published: %d %v", count, err)
	}
	current, err := cs.GetFollowGroup(g.ID)
	if err != nil || current.SourceEnded {
		t.Fatalf("false flat source boundary: %+v %v", current, err)
	}
}

func TestFollowGroupResumeRequiresAllMembersAndPerOrderFillProof(t *testing.T) {
	st, g, _ := groupStoreFixture(t)
	cs := st.CopyTrade()
	if _, err := cs.MarkManualStopped("t", "a"); err != nil {
		t.Fatal(err)
	}
	g, _ = cs.GetFollowGroup(g.ID)
	if err := cs.ResumeFollowGroup("t", g.ID, g.Version, map[string]float64{"a": 5}); err == nil {
		t.Fatal("partial membership resumed")
	}
	i := resolutionIntent(t, st, "t", "a", "open_long", ExecutionIntentReconciling, 2, 20)
	resolutionExec(t, st, `UPDATE copy_trade_execution_intents SET submitted_at=CURRENT_TIMESTAMP,filled_quantity=2,quantized_quantity=2 WHERE id=?`, i.ID)
	resolutionExec(t, st, `UPDATE copy_trade_position_mappings SET source_revision=2 WHERE trader_id='t' AND leader_pos_id='a'`)
	for n, client := range []string{"one", "two"} {
		resolutionExec(t, st, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,?,?,'FILLED','FILLED',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID, n+1, client)
	}
	// Aggregate filled_quantity matches, but the second order has not been booked.
	resolutionExec(t, st, `INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional) VALUES(?,'one',1,2000)`, i.ID)
	if err := cs.ResumeFollowGroup("t", g.ID, g.Version, map[string]float64{"a": 5, "b": 0}); !errors.Is(err, ErrSourceTransitionUnresolved) {
		t.Fatalf("missing per-order proof resumed: %v", err)
	}
	m, _ := cs.GetMappingForReconciliation("t", "a")
	if m.Status != MappingStatusManualStopped || m.LastKnownSize != 10 {
		t.Fatalf("failed resume changed baseline: %+v", m)
	}
	resolutionExec(t, st, `INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional) VALUES(?,'two',1,2000)`, i.ID)
	if err := cs.ResumeFollowGroup("t", g.ID, g.Version, map[string]float64{"a": 5, "b": 0}); err != nil {
		t.Fatal(err)
	}
	result, _ := cs.GetExecutionIntentByID(i.ID)
	if result.Status != ExecutionIntentFilled {
		t.Fatalf("acknowledged trade not finalized: %+v", result)
	}
}

func TestFollowGroupMixedLegacyParticipationNeverDependsOnMappingOrder(t *testing.T) {
	for _, first := range []string{MappingStatusActive, MappingStatusIgnored} {
		t.Run(first, func(t *testing.T) {
			st := resolutionStore(t)
			cs := st.CopyTrade()
			second := MappingStatusActive
			if first == second {
				second = MappingStatusIgnored
			}
			var g *FollowGroup
			for n, status := range []string{first, second} {
				id := []string{"a", "b"}[n]
				resolutionMapping(t, st, "t", id, status, 1, 10)
				var err error
				g, err = cs.EnsureFollowGroupMember("t", "okx", "leader", "ETHUSDT", "long", id, 1000, status)
				if err != nil {
					t.Fatal(err)
				}
			}
			if g.ConflictReason != "GROUP_PARTICIPATION_CONFLICT" {
				t.Fatalf("silently chose participation: %+v", g)
			}
			intent := resolutionIntent(t, st, "t", "a", "reduce_long", ExecutionIntentReserved, 2, 5)
			if err := cs.BindFollowGroupIntent(intent.ID, g.ID); err != nil {
				t.Fatal(err)
			}
			if err := cs.CheckFollowSubmission(intent.ID); !errors.Is(err, ErrFollowControlChanged) {
				t.Fatalf("conflicted group submitted: %v", err)
			}
		})
	}
}

func TestFollowGroupRepeatedPauseIsIdempotent(t *testing.T) {
	st, g, _ := groupStoreFixture(t)
	cs := st.CopyTrade()
	if _, err := cs.MarkManualStopped("t", "a"); err != nil {
		t.Fatal(err)
	}
	once, _ := cs.GetFollowGroup(g.ID)
	var version int64
	if err := st.DB().QueryRow(`SELECT version FROM copy_trade_follow_controls WHERE trader_id='t' AND leader_pos_id='a'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.MarkManualStopped("t", "b"); err != nil {
		t.Fatal(err)
	}
	twice, _ := cs.GetFollowGroup(g.ID)
	var next int64
	if err := st.DB().QueryRow(`SELECT version FROM copy_trade_follow_controls WHERE trader_id='t' AND leader_pos_id='a'`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if once.Version != twice.Version || next != version {
		t.Fatalf("repeated pause changed controls: %d/%d %d/%d", once.Version, twice.Version, version, next)
	}
}

func TestFollowGroupLateIgnoredBaselineAppliesToNewSourceMembers(t *testing.T) {
	st := resolutionStore(t)
	cs := st.CopyTrade()
	g, err := cs.EnsureFollowGroupMember("t", "okx", "leader", "ETHUSDT", "long", "a", 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	resolutionMapping(t, st, "t", "a", MappingStatusIgnored, 1, 10)
	g, err = cs.EnsureFollowGroupMember("t", "okx", "leader", "ETHUSDT", "long", "a", 1000, MappingStatusIgnored)
	if err != nil || !g.Ignored {
		t.Fatalf("late ignored baseline not propagated: %+v %v", g, err)
	}
	child, err := cs.EnsureFollowGroupMember("t", "okx", "leader", "ETHUSDT", "long", "b", 1100, "")
	if err != nil || child.ID != g.ID || !child.Ignored {
		t.Fatalf("new child escaped whole-round skip: %+v %v", child, err)
	}
	if err = cs.PublishFollowGroupSnapshot("t", []FollowGroupObservation{{g.ID, "a", "cross", 10}, {g.ID, "b", "cross", 10}}); err != nil {
		t.Fatal(err)
	}
	m, err := cs.GetMappingForReconciliation("t", "b")
	if err != nil || m.Status != MappingStatusIgnored {
		t.Fatalf("new member is not baseline-only: %+v %v", m, err)
	}
}
