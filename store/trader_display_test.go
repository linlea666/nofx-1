package store

import (
	"path/filepath"
	"testing"
)

func TestResolveTraderDisplayNameFallsBackToStableID(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "trader-name.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Trader().Create(&Trader{ID: "trader-1", UserID: "user-1", Name: "  量化主账户  ", AIModelID: "ai", ExchangeID: "ex", InitialBalance: 1000}); err != nil {
		t.Fatal(err)
	}
	if got := st.Trader().ResolveDisplayName("trader-1"); got != "量化主账户" {
		t.Fatalf("display name=%q", got)
	}
	if got := st.Trader().ResolveDisplayName("deleted-trader"); got != "deleted-trader" {
		t.Fatalf("fallback=%q", got)
	}
}
