package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUnsupportedExecutionInstrumentHealthTracksOnlyCurrentMappings(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "unsupported.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err := cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "trader", LeaderPosID: "pos", LeaderID: "leader", Symbol: "BTCUSDC",
		Side: "long", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cs.MarkUnsupportedExecutionInstrument("trader", "pos", "BTCUSDC", "okx", "exact settle contract missing"); err != nil {
		t.Fatal(err)
	}
	items, err := cs.ListUnsupportedExecutionInstruments("trader")
	if err != nil || len(items) != 1 || items[0].SourceSymbol != "BTCUSDC" {
		t.Fatalf("current unsupported instrument missing: items=%+v err=%v", items, err)
	}
	if err := cs.CloseMapping("trader", "pos", 100); err != nil {
		t.Fatal(err)
	}
	items, err = cs.ListUnsupportedExecutionInstruments("trader")
	if err != nil || len(items) != 0 {
		t.Fatalf("closed source position must leave current health view: items=%+v err=%v", items, err)
	}
}
