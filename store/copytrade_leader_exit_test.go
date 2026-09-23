package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReleasedCloseRepairAndNoChaseBaselineAreIdempotent(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "repair.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	policy, err := EncodeCopyGuardPolicySnapshot(NewCopyGuardDefaults())
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "t", LeaderID: "l", LeaderPosID: "ZECUSDT", Symbol: "ZECUSDT", Side: "long", MarginMode: "isolated", Status: CopyGuardFollowing, PolicySnapshot: policy, ProtectionStatus: CopyGuardProtectionVerified})
	if err != nil {
		t.Fatal(err)
	}
	for _, symbol := range []string{"ZECUSDT", "DOGEUSDT"} {
		if err = cs.SavePositionMapping(&CopyTradePositionMapping{TraderID: "t", LeaderPosID: symbol, LeaderID: "l", Symbol: symbol, Side: "long", MarginMode: "isolated", LastKnownSize: 2991, OpenedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		i, _, e := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: symbol, SourceRevision: 2, SourceKind: "LEADER_TRANSITION", Action: "close_long", Symbol: symbol, Side: "long", SourceFillID: "old-" + symbol})
		if e != nil {
			t.Fatal(e)
		}
		if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET status='SKIPPED',reason_code='POSITION_CUSTODY_RELEASED' WHERE id=?`, i.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = st.DB().Exec(`INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,symbol,side,state) VALUES('t',?,1,?,'long','RELEASED')`, symbol, symbol); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_position_custody SET cycle_id=? WHERE leader_pos_id='ZECUSDT'`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	if n, e := cs.RepairReleasedCloseBlockers("t"); e != nil || n != 2 {
		t.Fatalf("repair: %d %v", n, e)
	}
	if n, e := cs.RepairReleasedCloseBlockers("t"); e != nil || n != 0 {
		t.Fatalf("non-idempotent repair: %d %v", n, e)
	}
	closed, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil || closed.ClosedAt == nil || closed.Status != CopyGuardDetached || closed.ProtectionStatus != CopyGuardProtectionVerified || closed.PolicySnapshot != cycle.PolicySnapshot {
		t.Fatalf("released cycle was reused, or protection cancellation was fabricated: %+v %v", closed, err)
	}
	if err = cs.BaselineRepairedSource("t", map[string]*CopyTradePositionMapping{"ZECUSDT": {Side: "short", MarginMode: "isolated", LastKnownSize: 710}}, map[string]int64{"ZECUSDT": 2000}, true); err != nil {
		t.Fatal(err)
	}
	m, _ := cs.GetMapping("t", "ZECUSDT")
	if m == nil || m.Status != MappingStatusIgnored || m.Side != "short" || m.LastKnownSize != 710 || m.SourceRevision != 3 {
		t.Fatalf("missed live source was not ignored: %+v", m)
	}
	if err = cs.BaselineRepairedSource("t", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	m, _ = cs.GetMapping("t", "ZECUSDT")
	if m.Status != MappingStatusIgnored {
		t.Fatal("repeat baseline changed mapping")
	}
	if m, _ = cs.GetMapping("t", "DOGEUSDT"); m != nil {
		t.Fatal("absent DOGE source not retired")
	}
}

func TestExitRolloutSkipsUntrackedSourceOnceWithoutChasing(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "baseline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	current := map[string]*CopyTradePositionMapping{"new-id": {LeaderID: "leader", Symbol: "ZECUSDT", Side: "short", MarginMode: "isolated", LastKnownSize: 710}}
	if err = st.CopyTrade().BaselineRepairedSource("t", current, map[string]int64{"new-id": 2000}, true); err != nil {
		t.Fatal(err)
	}
	m, err := st.CopyTrade().GetMapping("t", "new-id")
	if err != nil || m == nil || m.Status != MappingStatusIgnored || m.LastFailureReason != "ROLLOUT_NO_CHASE" {
		t.Fatalf("rollout chased missing source: %+v %v", m, err)
	}
	current["later-id"] = &CopyTradePositionMapping{LeaderID: "leader", Symbol: "DOGEUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 100}
	if err = st.CopyTrade().BaselineRepairedSource("t", current, nil, true); err != nil {
		t.Fatal(err)
	}
	m, err = st.CopyTrade().GetMapping("t", "later-id")
	if err != nil || m != nil {
		t.Fatalf("normal new cycle was repeatedly baselined: %+v %v", m, err)
	}
}
