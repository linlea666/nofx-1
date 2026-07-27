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
	if err = st.db.QueryRow(`SELECT COALESCE(SUM(realized_pnl),0) FROM position_close_fills WHERE trader_id='t1' AND data_quality='VERIFIED'`).Scan(&trustedPnL); err != nil {
		t.Fatal(err)
	}
	if trustedPnL != 100 {
		t.Fatalf("one exchange close was counted more than once: %.2f", trustedPnL)
	}
	var lotQuality string
	if err = st.db.QueryRow(`SELECT accounting_quality FROM trader_positions WHERE id=?`, first.ID).Scan(&lotQuality); err != nil {
		t.Fatal(err)
	}
	if lotQuality != "ALLOCATED_ESTIMATE" {
		t.Fatalf("FIFO local lot must be identified as an estimated allocation: %s", lotQuality)
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

func TestLateAuthoritativeFillRepairsClosedUnscorableLot(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-late-fill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps := st.Position()
	pos := &TraderPosition{
		TraderID: "t1", ExchangeID: "account-1", ExchangeType: "binance",
		Symbol: "ETHUSDT", Side: "SHORT", Quantity: 1,
		EntryPrice: 2000, EntryTime: time.Now().Add(-time.Hour),
	}
	if err = ps.Create(pos); err != nil {
		t.Fatal(err)
	}
	if err = ps.ClosePositionUnscorable(pos.ID, 1950, "history delayed"); err != nil {
		t.Fatal(err)
	}
	claimed, err := ps.ClosePositionWithAllocation(
		pos.ID, "account-1", "late-trade-1", "ETHUSDT", "SHORT",
		1, 1900, 100, 1, "manual",
	)
	if err != nil || !claimed {
		t.Fatalf("late fill did not reconcile: claimed=%v err=%v", claimed, err)
	}
	var quality string
	var pnl, fee, exitPrice float64
	if err = st.db.QueryRow(`SELECT accounting_quality,realized_pnl,fee,exit_price FROM trader_positions WHERE id=?`,
		pos.ID).Scan(&quality, &pnl, &fee, &exitPrice); err != nil {
		t.Fatal(err)
	}
	if quality != "ALLOCATED_ESTIMATE" || pnl != 100 || fee != 1 || exitPrice != 1900 {
		t.Fatalf("late fill did not advance accounting quality: quality=%s pnl=%v fee=%v exit=%v",
			quality, pnl, fee, exitPrice)
	}
}

func TestLegacyDuplicateHeuristicCreatesAuditWithoutMutatingRows(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-audit-only.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 2; i++ {
		if _, err = st.db.Exec(`INSERT INTO trader_positions
			(trader_id,exchange_id,symbol,side,quantity,entry_price,entry_time,exit_price,exit_order_id,exit_time,realized_pnl,status,accounting_quality)
			VALUES('t1','account-1','ETHUSDT','SHORT',1,2000,?,1900,'same-order',?,100,'CLOSED','VERIFIED')`,
			now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.Position().InitTables(); err != nil {
		t.Fatal(err)
	}
	var mutated, audits, trusted int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM trader_positions WHERE accounting_quality<>'VERIFIED'`).Scan(&mutated); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM position_accounting_audits WHERE reason_code='POSSIBLE_LEGACY_DUPLICATION'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM trusted_closed_positions`).Scan(&trusted); err != nil {
		t.Fatal(err)
	}
	if mutated != 0 || audits != 1 || trusted != 0 {
		t.Fatalf("legacy suspicion must be preserved but excluded from trusted metrics: mutated=%d audits=%d trusted=%d",
			mutated, audits, trusted)
	}
}

func TestPartialAuthoritativeCloseSplitsOpenRemainder(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-partial-close.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pos := &TraderPosition{
		TraderID: "t1", ExchangeID: "account-1", ExchangeType: "binance",
		Symbol: "BTCUSDT", Side: "LONG", Quantity: 1,
		EntryPrice: 60000, EntryTime: time.Now().Add(-time.Hour),
	}
	if err = st.Position().Create(pos); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.Position().ClosePositionWithAllocations(pos.ID, "account-1", []PositionCloseFill{{
		TradeID: "partial-1", Symbol: "BTCUSDT", Side: "LONG", Quantity: 0.4,
		ExitPrice: 61000, RealizedPnL: 400, Fee: 1,
		FillTime: time.Now(), DataQuality: "VERIFIED",
	}}, 61000, "manual")
	if err != nil || !claimed {
		t.Fatalf("partial close claimed=%v err=%v", claimed, err)
	}
	var closedQty, openQty float64
	var closedQuality string
	if err = st.db.QueryRow(`SELECT quantity,accounting_quality FROM trader_positions WHERE id=?`,
		pos.ID).Scan(&closedQty, &closedQuality); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT COALESCE(SUM(quantity),0) FROM trader_positions
		WHERE exchange_id='account-1' AND symbol='BTCUSDT' AND side='LONG' AND status='OPEN'`).Scan(&openQty); err != nil {
		t.Fatal(err)
	}
	if math.Abs(closedQty-0.4) > 1e-9 || math.Abs(openQty-0.6) > 1e-9 || closedQuality != "ALLOCATED_ESTIMATE" {
		t.Fatalf("partial lot split mismatch: closed=%.8f open=%.8f quality=%s",
			closedQty, openQty, closedQuality)
	}
}

func TestLegacyAllocationMigrationBackfillsAuthoritativeFillParent(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-allocation-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pos := &TraderPosition{
		TraderID: "t1", ExchangeID: "account-1", Symbol: "ETHUSDT", Side: "SHORT",
		Quantity: 1, EntryPrice: 2000, EntryTime: time.Now().Add(-time.Hour),
	}
	if err = st.Position().Create(pos); err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec(`INSERT INTO position_close_allocations
		(fill_id,exchange_id,exchange_trade_id,position_id,symbol,side,quantity,exit_price,realized_pnl,fee)
		VALUES(0,'account-1','legacy-fill-1',?,'ETHUSDT','SHORT',1,1900,100,1)`, pos.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.Position().InitTables(); err != nil {
		t.Fatal(err)
	}
	var fillID, parentID int64
	var quality string
	if err = st.db.QueryRow(`SELECT fill_id FROM position_close_allocations
		WHERE exchange_trade_id='legacy-fill-1'`).Scan(&fillID); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT id,data_quality FROM position_close_fills
		WHERE exchange_id='account-1' AND exchange_trade_id='legacy-fill-1'`).Scan(&parentID, &quality); err != nil {
		t.Fatal(err)
	}
	if fillID == 0 || fillID != parentID || quality != "MIGRATED_VERIFIED" {
		t.Fatalf("legacy allocation parent mismatch: fill_id=%d parent=%d quality=%s", fillID, parentID, quality)
	}
	// A second startup must not create another authoritative fill.
	if err = st.Position().InitTables(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM position_close_fills
		WHERE exchange_id='account-1' AND exchange_trade_id='legacy-fill-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migration is not restart-idempotent: fills=%d", count)
	}
}
