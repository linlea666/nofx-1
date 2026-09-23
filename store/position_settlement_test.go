package store

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestZECLiquidationSettlementAllocatesCostsOnceAndPreservesFillTime(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "zec.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps := st.Position()
	opened := time.Date(2026, 9, 21, 20, 24, 29, 0, time.UTC)
	closed := time.Date(2026, 9, 21, 20, 34, 6, 317000000, time.UTC)
	var lots []*TraderPosition
	for i, qty := range []float64{13.72, .72, .04} {
		p := &TraderPosition{TraderID: "t", ExchangeID: "okx-account", ExchangeType: "okx", Symbol: "ZECUSDT", Side: "LONG", Quantity: qty, EntryPrice: 1477.1441, EntryOrderID: fmt.Sprintf("entry-%d", i), EntryTime: opened.Add(time.Duration(i) * time.Second)}
		if err = ps.Create(p); err != nil {
			t.Fatal(err)
		}
		lots = append(lots, p)
	}
	fill := PositionCloseFill{TradeID: "trade:ZEC-USDT-SWAP:-3943331824554373122", Symbol: "ZECUSDT", Side: "LONG", Quantity: 14.48, ExitPrice: 1462.93, RealizedPnL: -205.8198, Fee: 10.5916132, FillTime: closed}
	if _, err = ps.ClosePositionWithAllocations(lots[0].ID, "okx-account", []PositionCloseFill{fill}, 1462.93, "unknown"); err != nil {
		t.Fatal(err)
	}
	settlement := PositionSettlement{ExchangeID: "okx-account", PositionID: "3939444648729202689", Symbol: "ZECUSDT", Side: "long", MarginMode: "isolated", OpenedAt: opened, ClosedAt: closed, Quantity: 14.48, GrossPnL: -205.8198, Fee: 21.2861363, LiquidationPenalty: 211.832264, NetPnL: -438.9382003, CloseType: "liquidation", TradeIDs: []string{fill.TradeID}, EntryOrderIDs: []string{"entry-0", "entry-1", "entry-2"}}
	for i := 0; i < 3; i++ {
		if err = ps.ApplyPositionSettlement(settlement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = ps.ClosePositionWithAllocations(lots[1].ID, "okx-account", []PositionCloseFill{fill}, 1462.93, "unknown"); err != nil {
		t.Fatal(err)
	}
	var pnl, fees, penalty float64
	var count int
	if err = st.DB().QueryRow(`SELECT SUM(realized_pnl),SUM(fee),SUM(liquidation_penalty),SUM(settlement_verified) FROM trader_positions`).Scan(&pnl, &fees, &penalty, &count); err != nil {
		t.Fatal(err)
	}
	if math.Abs(pnl-settlement.NetPnL) > 1e-8 || math.Abs(fees-settlement.Fee) > 1e-8 || math.Abs(penalty-settlement.LiquidationPenalty) > 1e-8 || count != 3 {
		t.Fatalf("duplicated/missing costs pnl=%v fees=%v penalty=%v lots=%d", pnl, fees, penalty, count)
	}
	positions, err := ps.GetClosedPositions("t", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range positions {
		if p.NetPnL == nil || p.GrossPnL == nil || !p.SettlementVerified || p.CloseReason != "liquidation" || p.ExitTime == nil || !p.ExitTime.Equal(closed) {
			t.Fatalf("settlement display incomplete: %+v", p)
		}
	}
	// Reusing the same venue posId for a later cycle cannot consume these fills.
	wrong := settlement
	wrong.OpenedAt = opened.Add(time.Hour)
	wrong.ClosedAt = closed.Add(time.Hour)
	if err = ps.ApplyPositionSettlement(wrong); err == nil {
		t.Fatal("reused posId accepted old fills")
	}
	wrong = settlement
	wrong.NetPnL = -100
	if err = ps.ApplyPositionSettlement(wrong); err == nil {
		t.Fatal("inconsistent costs accepted")
	}
}
