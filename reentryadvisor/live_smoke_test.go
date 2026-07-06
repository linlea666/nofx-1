package reentryadvisor

import (
	"encoding/json"
	"os"
	"testing"
)

// TestLiveMarketSectionSmoke 真实请求 Binance 公共接口的冒烟验证（默认跳过）。
// 运行：REENTRY_LIVE_SMOKE=1 go test ./reentryadvisor -run LiveMarketSection -v
func TestLiveMarketSectionSmoke(t *testing.T) {
	if os.Getenv("REENTRY_LIVE_SMOKE") == "" {
		t.Skip("set REENTRY_LIVE_SMOKE=1 to run live smoke test")
	}
	bn := newBinanceClient()

	// 主流币：市场层应完整
	meta := &MetaSection{}
	m := buildMarketSection(bn, "BTCUSDT", 500, meta)
	if m == nil {
		t.Fatal("BTCUSDT market section is nil")
	}
	if !meta.FuturesAvailable {
		t.Fatal("BTCUSDT futures should be available")
	}
	if m.CurrentPrice <= 0 {
		t.Fatalf("current price = %v", m.CurrentPrice)
	}
	if len(m.Klines) < 5 {
		t.Fatalf("klines timeframes = %d", len(m.Klines))
	}
	if len(m.ContractCVD) == 0 {
		t.Fatal("contract cvd empty")
	}
	if m.OpenInterest == nil || m.Funding == nil {
		t.Fatalf("oi=%v funding=%v", m.OpenInterest, m.Funding)
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	t.Logf("BTCUSDT market section (%d bytes), missing=%v, spot=%v", len(out), meta.MissingFields, meta.SpotAvailable)
	t.Logf("funding=%+v basis=%+v ls=%+v", m.Funding, m.Basis, m.LongShort)
	t.Logf("sr(1h)=%+v", m.SupportResistance["1h"])

	// Binance 不存在的币种：应整体降级为 nil
	meta2 := &MetaSection{}
	if m2 := buildMarketSection(bn, "NOSUCHCOINUSDT", 1, meta2); m2 != nil {
		t.Fatal("nonexistent symbol should return nil market section")
	}
	if meta2.FuturesAvailable {
		t.Fatal("nonexistent symbol should mark futures unavailable")
	}
}
