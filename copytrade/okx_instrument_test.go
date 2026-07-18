package copytrade

import "testing"

func TestOKXInstrumentRowMatchesRealSwapShape(t *testing.T) {
	row := okxInstrumentRow{
		InstID:     "BTC-USDT-SWAP",
		InstType:   "SWAP",
		State:      "live",
		SettleCcy:  "USDT",
		CtValCcy:   "BTC",
		Uly:        "BTC-USDT",
		InstFamily: "BTC-USDT",
	}
	if !okxInstrumentRowMatchesSwap(row, "BTC-USDT-SWAP", "BTC", "USDT") {
		t.Fatal("real SWAP shape with empty SPOT-only baseCcy/quoteCcy must be accepted")
	}

	row.SettleCcy = "USDC"
	if okxInstrumentRowMatchesSwap(row, "BTC-USDT-SWAP", "BTC", "USDT") {
		t.Fatal("settlement mismatch must fail closed")
	}
}
