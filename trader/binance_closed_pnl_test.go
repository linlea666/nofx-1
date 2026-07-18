package trader

import (
	"math"
	"testing"
	"time"
)

func TestBuildBinanceClosedPnLAggregatesOrderFills(t *testing.T) {
	now := time.Now().UTC()
	records := buildBinanceClosedPnLRecords([]TradeRecord{
		{TradeID: "1", OrderID: "close-1", Symbol: "BTCUSDC", Side: "SELL", PositionSide: "LONG", Price: 105, Quantity: 0.4, RealizedPnL: 2, Fee: 0.04, Time: now},
		{TradeID: "2", OrderID: "close-1", Symbol: "BTCUSDC", Side: "SELL", PositionSide: "LONG", Price: 106, Quantity: 0.6, RealizedPnL: 3, Fee: 0.06, Time: now.Add(time.Second)},
		{TradeID: "3", OrderID: "open-short", Symbol: "BTCUSDC", Side: "SELL", PositionSide: "SHORT", Price: 106, Quantity: 1, RealizedPnL: 0, Time: now},
	}, 100)
	if len(records) != 1 {
		t.Fatalf("records=%+v, want one aggregated close", records)
	}
	got := records[0]
	if got.Symbol != "BTCUSDC" || got.Side != "long" || got.OrderID != "close-1" {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if math.Abs(got.Quantity-1) > 1e-9 || math.Abs(got.QuantityCoins-1) > 1e-9 || math.Abs(got.RealizedPnL-5) > 1e-9 || math.Abs(got.Fee-0.1) > 1e-9 {
		t.Fatalf("aggregate mismatch: %+v", got)
	}
	if math.Abs(got.ExitPrice-105.6) > 1e-9 || math.Abs(got.EntryPrice-100.6) > 1e-9 {
		t.Fatalf("price reconstruction mismatch: %+v", got)
	}
}

func TestBuildBinanceClosedPnLIncludesBreakEvenHedgeClose(t *testing.T) {
	records := buildBinanceClosedPnLRecords([]TradeRecord{{
		TradeID: "1", OrderID: "break-even", Symbol: "ETHUSD1", Side: "BUY", PositionSide: "SHORT",
		Price: 100, Quantity: 2, RealizedPnL: 0, Fee: 0.02, Time: time.Now().UTC(),
	}}, 10)
	if len(records) != 1 || records[0].Side != "short" || records[0].Quantity != 2 {
		t.Fatalf("explicit hedge-mode break-even close was lost: %+v", records)
	}
}
