package api

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

func TestPrepareCopyTradeSourceNormalizesAndGenerates(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "source-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &store.CopyTradeConfig{TraderID: "trader", ProviderType: "binance", LeaderID: "https://www.binance.com/zh-CN/smart-money/profile/5082050984257986817/", BinanceSourceMode: "smart_money"}
	if err := prepareCopyTradeSource(st, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.LeaderID != "5082050984257986817" || cfg.BinanceTopTraderID != cfg.LeaderID || cfg.SourceGeneration != 1 {
		t.Fatalf("unexpected normalized config: %+v", cfg)
	}
	existing := *cfg
	same := existing
	if err := prepareCopyTradeSource(st, &same, &existing); err != nil {
		t.Fatal(err)
	}
	if same.SourceGeneration != 1 {
		t.Fatalf("unchanged identity incremented generation: %+v", same)
	}
	changed := existing
	changed.BinanceTopTraderID = "5082050984257986818"
	changed.LeaderID = changed.BinanceTopTraderID
	if err := prepareCopyTradeSource(st, &changed, &existing); err != nil {
		t.Fatal(err)
	}
	if changed.SourceGeneration != 2 {
		t.Fatalf("identity change generation=%d", changed.SourceGeneration)
	}
}

func TestPrepareCopyTradeSourceRejectsSwitchWithLiveMapping(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "source-live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	existing := &store.CopyTradeConfig{TraderID: "trader", ProviderType: "binance", LeaderID: "5082050984257986817", BinanceSourceMode: "smart_money", BinanceTopTraderID: "5082050984257986817", SourceGeneration: 1}
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: "trader", LeaderID: existing.LeaderID, LeaderPosID: "pos", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 1}); err != nil {
		t.Fatal(err)
	}
	changed := *existing
	changed.LeaderID = "5082050984257986818"
	changed.BinanceTopTraderID = changed.LeaderID
	if err := prepareCopyTradeSource(st, &changed, existing); err == nil {
		t.Fatal("source switch with active mapping must be rejected")
	}
}

func TestPrepareCopyTradeSourceRejectsSwitchWhileRuntimeIsActive(t *testing.T) {
	existing := &store.CopyTradeConfig{
		TraderID: "runtime-trader", ProviderType: "binance",
		LeaderID: "5082050984257986817", BinanceSourceMode: "smart_money",
		BinanceTopTraderID: "5082050984257986817", SourceGeneration: 1,
	}
	changed := *existing
	changed.LeaderID = "5082050984257986818"
	changed.BinanceTopTraderID = changed.LeaderID
	if err := prepareCopyTradeSourceWithRuntime(nil, &changed, existing, true); err == nil {
		t.Fatal("running source switch must be rejected even when no active mapping exists")
	}
}

func TestPrepareCopyTradeSourceDoesNotReusePortfolioIDAsTopTraderID(t *testing.T) {
	existing := &store.CopyTradeConfig{
		TraderID: "trader", ProviderType: "binance",
		LeaderID: "5008318166959365632", BinanceSourceMode: "copy_management", SourceGeneration: 1,
	}
	next := *existing
	next.BinanceSourceMode = "smart_money"
	next.BinanceTopTraderID = ""
	if err := prepareCopyTradeSourceWithRuntime(nil, &next, existing, false); err == nil {
		t.Fatal("mode switch without an explicit topTraderId must not reinterpret portfolioId")
	}
}

func TestPrepareCopyTradeSourceDoesNotReuseTopTraderIDAsPortfolioID(t *testing.T) {
	existing := &store.CopyTradeConfig{
		TraderID: "trader", ProviderType: "binance",
		LeaderID: "5082050984257986817", BinanceSourceMode: "smart_money",
		BinanceTopTraderID: "5082050984257986817", SourceGeneration: 1,
	}
	next := *existing
	next.BinanceSourceMode = "copy_management"
	if err := prepareCopyTradeSourceWithRuntime(nil, &next, existing, false); err == nil {
		t.Fatal("mode switch without a new portfolioId must not reinterpret topTraderId")
	}
	next.LeaderID = "5008318166959365632"
	if err := prepareCopyTradeSourceWithRuntime(nil, &next, existing, false); err != nil {
		t.Fatalf("explicit new portfolioId should allow the mode switch: %v", err)
	}
}

func TestPrepareCopyTradeSourceRejectsSpoofedOrAmbiguousSmartMoneyURL(t *testing.T) {
	for _, value := range []string{
		"https://binance.com.evil.example/smart-money/profile/5082050984257986817",
		"https://www.binance.com/smart-money/profile/5082050984257986817/999",
		"https://www.binance.com/copy-trading/profile/5082050984257986817",
	} {
		cfg := &store.CopyTradeConfig{TraderID: "trader", ProviderType: "binance", LeaderID: value, BinanceSourceMode: "smart_money"}
		if err := prepareCopyTradeSource(nil, cfg, nil); err == nil {
			t.Fatalf("ambiguous Smart Money URL accepted: %s", value)
		}
	}
}
