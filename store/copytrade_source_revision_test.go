package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSourceSnapshotRevisionPersistsAcrossAcknowledgedLifecycle(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "source-revision.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	copyStore := st.CopyTrade()

	revision, err := copyStore.GetSourceSnapshotRevision("trader", "position")
	if err != nil || revision != 0 {
		t.Fatalf("missing position revision: got=%d err=%v", revision, err)
	}
	mapping := &CopyTradePositionMapping{
		TraderID: "trader", LeaderPosID: "position", LeaderID: "leader",
		Symbol: "BTCUSDC", Side: "long", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 1,
	}
	if err := copyStore.SavePositionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	assertSourceRevision(t, copyStore, "trader", "position", 1)
	loaded, err := copyStore.GetActiveMapping("trader", "position")
	if err != nil || loaded == nil || loaded.SourceRevision != 1 {
		t.Fatalf("mapping scan lost source revision: mapping=%+v err=%v", loaded, err)
	}

	if err := copyStore.UpdateLastKnownSize("trader", "position", 2); err != nil {
		t.Fatal(err)
	}
	assertSourceRevision(t, copyStore, "trader", "position", 2)
	if err := copyStore.CloseMapping("trader", "position", 100); err != nil {
		t.Fatal(err)
	}
	assertSourceRevision(t, copyStore, "trader", "position", 3)
	if hidden, err := copyStore.GetMapping("trader", "position"); err != nil || hidden != nil {
		t.Fatalf("closed mapping behavior changed: mapping=%+v err=%v", hidden, err)
	}

	mapping.LastKnownSize = 1
	if err := copyStore.SavePositionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	assertSourceRevision(t, copyStore, "trader", "position", 4)
}

func assertSourceRevision(t *testing.T, copyStore *CopyTradeStore, traderID, leaderPosID string, want int64) {
	t.Helper()
	got, err := copyStore.GetSourceSnapshotRevision(traderID, leaderPosID)
	if err != nil || got != want {
		t.Fatalf("source revision: got=%d want=%d err=%v", got, want, err)
	}
}
