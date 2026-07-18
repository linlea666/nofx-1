package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestInitializeSourceBaselineRollsBackOnLiveMappingConflict(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "baseline-atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.CopyTrade().SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "trader", LeaderID: "leader", LeaderPosID: "live",
		Symbol: "BTCUSDT", Side: "long", MarginMode: "cross",
		OpenedAt: time.Now(), LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	err = st.CopyTrade().InitializeSourceBaseline("trader", "leader", "smart_money", 2, []CopyTradeBaselinePosition{
		{LeaderPosID: "would-be-ignored", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Size: 2},
		{LeaderPosID: "live", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Size: 1},
	})
	if err == nil {
		t.Fatal("live mapping conflict must reject the entire baseline transaction")
	}
	if mapping, getErr := st.CopyTrade().GetMapping("trader", "would-be-ignored"); getErr != nil || mapping != nil {
		t.Fatalf("partial ignored mapping escaped rollback: mapping=%+v err=%v", mapping, getErr)
	}
	if complete, completeErr := st.CopyTrade().IsSourceBaselineComplete("trader", "leader", "smart_money", 2); completeErr != nil || complete {
		t.Fatalf("failed baseline must not write completion marker: complete=%v err=%v", complete, completeErr)
	}
}

func TestRebaselineSourceRecoveryAbsorbsOnlyRiskIncrease(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "baseline-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, id := range []string{"added", "reduced"} {
		if err := st.CopyTrade().SavePositionMapping(&CopyTradePositionMapping{
			TraderID: "trader", LeaderID: "leader", LeaderPosID: id,
			Symbol: id + "USDT", Side: "long", MarginMode: "cross",
			OpenedAt: time.Now(), LastKnownSize: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CopyTrade().RebaselineSourceRecovery("trader", "leader", []CopyTradeBaselinePosition{
		{LeaderPosID: "added", Symbol: "ADDEDUSDT", Side: "long", MarginMode: "cross", Size: 15},
		{LeaderPosID: "reduced", Symbol: "REDUCEDUSDT", Side: "long", MarginMode: "cross", Size: 6},
		{LeaderPosID: "new", Symbol: "NEWUSDT", Side: "long", MarginMode: "cross", Size: 4},
	}); err != nil {
		t.Fatal(err)
	}
	added, _ := st.CopyTrade().GetMapping("trader", "added")
	reduced, _ := st.CopyTrade().GetMapping("trader", "reduced")
	newMapping, _ := st.CopyTrade().GetMapping("trader", "new")
	if added.LastKnownSize != 15 || reduced.LastKnownSize != 10 ||
		newMapping == nil || newMapping.Status != MappingStatusIgnored || newMapping.LastKnownSize != 4 {
		t.Fatalf("recovery baseline changed risk semantics: added=%+v reduced=%+v new=%+v", added, reduced, newMapping)
	}
}
