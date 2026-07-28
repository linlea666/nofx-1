package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCreateOpenPositionIfAbsentReportsActualInsert(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-create.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	position := &TraderPosition{
		TraderID: "trader-1", ExchangeID: "exchange-1", ExchangeType: "okx",
		ExchangePositionID: "PROSUSDT_SHORT_reconciled_1",
		Symbol:             "PROSUSDT", Side: "SHORT", Quantity: 121,
		EntryPrice: 0.4451, EntryTime: time.Now(), Leverage: 20, Source: "sync",
	}
	created, err := st.Position().CreateOpenPositionIfAbsent(position)
	if err != nil || !created {
		t.Fatalf("first insert created=%v err=%v", created, err)
	}
	duplicate := *position
	duplicate.ID = 0
	created, err = st.Position().CreateOpenPositionIfAbsent(&duplicate)
	if err != nil || created {
		t.Fatalf("duplicate insert created=%v err=%v", created, err)
	}
	var count int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM trader_positions WHERE exchange_id=? AND exchange_position_id=?`,
		position.ExchangeID, position.ExchangePositionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unexpected position row count: %d", count)
	}
}
