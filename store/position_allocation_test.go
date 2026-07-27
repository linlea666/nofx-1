package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestExchangeCloseAllocationCannotDuplicatePnLAcrossLocalPositions(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-allocation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps := st.Position()
	first := &TraderPosition{TraderID: "t1", ExchangeID: "account-1", ExchangeType: "binance", Symbol: "ETHUSDT", Side: "SHORT", Quantity: 1, EntryPrice: 2000, EntryTime: time.Now().Add(-time.Hour)}
	second := *first
	second.EntryTime = first.EntryTime.Add(time.Minute)
	if err = ps.Create(first); err != nil {
		t.Fatal(err)
	}
	if err = ps.Create(&second); err != nil {
		t.Fatal(err)
	}
	claimed, err := ps.ClosePositionWithAllocation(first.ID, "account-1", "trade-77", "ETHUSDT", "SHORT", 1, 1900, 100, 1, "manual")
	if err != nil || !claimed {
		t.Fatalf("first allocation claimed=%v err=%v", claimed, err)
	}
	claimed, err = ps.ClosePositionWithAllocation(second.ID, "account-1", "trade-77", "ETHUSDT", "SHORT", 1, 1900, 100, 1, "manual")
	if err != nil || claimed {
		t.Fatalf("duplicate allocation claimed=%v err=%v", claimed, err)
	}
	var trustedPnL float64
	if err = st.db.QueryRow(`SELECT COALESCE(SUM(realized_pnl),0) FROM trader_positions WHERE trader_id='t1' AND accounting_quality='VERIFIED'`).Scan(&trustedPnL); err != nil {
		t.Fatal(err)
	}
	if trustedPnL != 100 {
		t.Fatalf("one exchange close was counted more than once: %.2f", trustedPnL)
	}
}

func TestExchangeCloseAllocationSplitsAggregateFillAcrossLocalLots(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-allocation-split.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps := st.Position()
	first := &TraderPosition{TraderID: "t1", ExchangeID: "account-1", ExchangeType: "binance", Symbol: "ETHUSDT", Side: "SHORT", Quantity: 0.4, EntryPrice: 2000, EntryTime: time.Now().Add(-time.Hour)}
	second := *first
	second.Quantity = 0.6
	second.EntryTime = first.EntryTime.Add(time.Minute)
	if err = ps.Create(first); err != nil {
		t.Fatal(err)
	}
	if err = ps.Create(&second); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		claimed, closeErr := ps.ClosePositionWithAllocation(id, "account-1", "trade-split", "ETHUSDT", "SHORT", 1, 1900, 100, 2, "manual")
		if closeErr != nil || !claimed {
			t.Fatalf("allocation position=%d claimed=%v err=%v", id, claimed, closeErr)
		}
	}
	var quantity, pnl, fee float64
	if err = st.db.QueryRow(`SELECT COALESCE(SUM(quantity),0),COALESCE(SUM(realized_pnl),0),COALESCE(SUM(fee),0) FROM position_close_allocations WHERE exchange_trade_id='trade-split'`).Scan(&quantity, &pnl, &fee); err != nil {
		t.Fatal(err)
	}
	if math.Abs(quantity-1) > 1e-9 || math.Abs(pnl-100) > 1e-9 || math.Abs(fee-2) > 1e-9 {
		t.Fatalf("aggregate close allocation mismatch: quantity=%.8f pnl=%.8f fee=%.8f", quantity, pnl, fee)
	}
}

func TestOverlappingExchangeHistoryAllocatesEachFillOnlyOnce(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-allocation-overlap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps := st.Position()
	first := &TraderPosition{TraderID: "t1", ExchangeID: "account-1", Symbol: "ETHUSDT", Side: "SHORT", Quantity: 0.5, EntryPrice: 2000, EntryTime: time.Now().Add(-time.Hour)}
	second := *first
	second.EntryTime = first.EntryTime.Add(time.Minute)
	if err = ps.Create(first); err != nil {
		t.Fatal(err)
	}
	if err = ps.Create(&second); err != nil {
		t.Fatal(err)
	}
	fills := []PositionCloseFill{
		{TradeID: "fill-1", Symbol: "ETHUSDT", Side: "SHORT", Quantity: 0.5, ExitPrice: 1900, RealizedPnL: 50, Fee: 0.5},
		{TradeID: "fill-2", Symbol: "ETHUSDT", Side: "SHORT", Quantity: 0.5, ExitPrice: 1890, RealizedPnL: 55, Fee: 0.5},
	}
	for _, id := range []int64{first.ID, second.ID} {
		claimed, closeErr := ps.ClosePositionWithAllocations(id, "account-1", fills, 1900, "manual")
		if closeErr != nil || !claimed {
			t.Fatalf("overlap allocation position=%d claimed=%v err=%v", id, claimed, closeErr)
		}
	}
	var allocations int
	var quantity, pnl float64
	if err = st.db.QueryRow(`SELECT COUNT(*),SUM(quantity),SUM(realized_pnl) FROM position_close_allocations WHERE exchange_id='account-1'`).Scan(&allocations, &quantity, &pnl); err != nil {
		t.Fatal(err)
	}
	if allocations != 2 || math.Abs(quantity-1) > 1e-9 || math.Abs(pnl-105) > 1e-9 {
		t.Fatalf("overlapping fill history duplicated accounting: allocations=%d quantity=%.8f pnl=%.8f", allocations, quantity, pnl)
	}
}
