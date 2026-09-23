package trader

import (
	"testing"
	"time"
)

func TestSettlementRequiresOneCompleteScopedLifecycle(t *testing.T) {
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	end := start.Add(time.Minute)
	record := ClosedPnLRecord{Symbol: "ZECUSDT", Side: "long", MarginMode: "isolated", QuantityCoins: 2, GrossPnL: -20, RealizedPnL: -33, Fee: 3, LiquidationPenalty: 10, EntryTime: start, ExitTime: end, ExchangeID: "reusable-pos", CloseType: "liquidation"}
	trades := []TradeRecord{{TradeID: "entry", OrderID: "entry-order", Symbol: "ZECUSDT", PositionSide: "LONG", Side: "BUY", MarginMode: "isolated", Quantity: 2, Time: start}, {TradeID: "exit", OrderID: "liquidation", Symbol: "ZECUSDT", PositionSide: "LONG", Side: "SELL", MarginMode: "isolated", Quantity: 2, RealizedPnL: -20, Time: end}}
	got, err := BuildPositionSettlement("account", record, trades)
	if err != nil || got.NetPnL != -33 || len(got.TradeIDs) != 1 || got.FirstEntryOrderID != "entry-order" {
		t.Fatalf("valid settlement rejected %+v %v", got, err)
	}
	if _, err = BuildPositionSettlement("account", record, trades[1:]); err == nil {
		t.Fatal("missing opening fills accepted")
	}
	wrong := record
	wrong.MarginMode = "cross"
	if _, err = BuildPositionSettlement("account", wrong, trades); err == nil {
		t.Fatal("other margin mode accepted")
	}
	wrong = record
	wrong.QuantityCoins = 3
	if _, err = BuildPositionSettlement("account", wrong, trades); err == nil {
		t.Fatal("incomplete quantity accepted")
	}
	// Same-ID same-side reopening inside a cumulative history row is ambiguous.
	reopened := append(append([]TradeRecord{}, trades...), TradeRecord{TradeID: "next", OrderID: "next-order", Symbol: "ZECUSDT", PositionSide: "LONG", Side: "BUY", MarginMode: "isolated", Quantity: 1, Time: end.Add(time.Second)})
	wrong = record
	wrong.ExitTime = end.Add(time.Second)
	if _, err = BuildPositionSettlement("account", wrong, reopened); err == nil {
		t.Fatal("two source lifecycles merged")
	}
}
