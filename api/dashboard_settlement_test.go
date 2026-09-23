package api

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

func TestDashboardUsesUniqueNetSettlementAndMarksUnverifiedCosts(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// One liquidation fill can be shared by many local lots. The account
	// summary must read its single settlement, not sum the cumulative fee per lot.
	for _, row := range []struct {
		id       string
		pnl, fee float64
	}{{"liquidation", -205.8198, 10.5916132}, {"pending", 10, .2}} {
		if _, err = st.DB().Exec(`INSERT INTO position_close_fills(trader_id,exchange_id,exchange_trade_id,symbol,side,quantity,exit_price,realized_pnl,fee,fill_time) VALUES('t','e',?,'ZECUSDT','LONG',1,1462.93,?,?,?)`, row.id, row.pnl, row.fee, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.DB().Exec(`INSERT INTO position_fill_settlements(fill_id,settlement_id,fee,funding_fee,liquidation_penalty,net_pnl) SELECT id,1,21.2861363,0,211.832264,-438.9382003 FROM position_close_fills WHERE exchange_trade_id='liquidation'`); err != nil {
		t.Fatal(err)
	}
	summary, err := (&Server{store: st}).getDashboardSummary()
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range map[string][2]float64{
		"net plus pending gross": {summary.TotalPnL, -428.9382003},
		"verified net":           {summary.VerifiedNetPnL, -438.9382003},
		"gross":                  {summary.GrossPnL, -195.8198},
		"fees":                   {summary.TotalFees, 21.4861363},
		"penalty":                {summary.LiquidationPenalty, 211.832264},
		"today":                  {summary.TodayPnL, -428.9382003},
		"week":                   {summary.WeekPnL, -428.9382003},
		"month":                  {summary.MonthPnL, -428.9382003},
	} {
		if math.Abs(values[0]-values[1]) > 1e-8 {
			t.Errorf("%s got %.9f want %.9f", name, values[0], values[1])
		}
	}
	if summary.SettlementPending != 1 {
		t.Fatalf("incomplete costs were presented as verified: %+v", summary)
	}
}
