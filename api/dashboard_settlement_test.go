package api

import (
	"fmt"
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

// Regression: at 00:30 in a UTC+8 server the old unzoned SQL parameter
// represented 08:00 local time, hiding all current-day fills. The negative
// offset case over-counted the previous day. Exercise each calendar boundary
// against mixed SQLite UTC and RFC3339 exchange timestamps, including 1 ms
// before the cutoff so datetime() truncation cannot hide a boundary mistake.
func TestDashboardCalendarBoundariesNormalizeStoredTimestamps(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("UTC+8", 8*3600), time.FixedZone("UTC-5", -5*3600), newYork} {
		t.Run(zone.String(), func(t *testing.T) {
			now := time.Date(2026, time.November, 1, 3, 30, 0, 0, zone)
			for _, period := range []struct {
				name  string
				month time.Month
				day   int
			}{
				{"today", time.November, 1},
				{"week", time.October, 26},
				{"month", time.November, 1},
			} {
				t.Run(period.name, func(t *testing.T) {
					st, err := store.New(filepath.Join(t.TempDir(), "boundary.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer st.Close()
					boundary := time.Date(2026, period.month, period.day, 0, 0, 0, 0, zone)
					for i, row := range []struct {
						at  string
						pnl float64
					}{
						{boundary.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano), 100},
						{boundary.Format(time.RFC3339Nano), 2},
						{boundary.Add(time.Second).UTC().Format("2006-01-02 15:04:05"), 3},
					} {
						if _, err = st.DB().Exec(`INSERT INTO position_close_fills(trader_id,exchange_id,exchange_trade_id,symbol,side,quantity,exit_price,realized_pnl,fill_time) VALUES('t','e',?,'ETHUSDT','LONG',1,100,?,?)`, fmt.Sprint(i), row.pnl, row.at); err != nil {
							t.Fatal(err)
						}
						if _, err = st.DB().Exec(`INSERT INTO trader_positions(trader_id,exchange_id,symbol,side,quantity,entry_price,entry_time,exit_time,realized_pnl,status,accounting_quality) VALUES('t','e','ETHUSDT','LONG',1,100,'2026-01-01',?,?,'CLOSED','ALLOCATED_ESTIMATE')`, row.at, row.pnl); err != nil {
							t.Fatal(err)
						}
					}
					s := &Server{store: st}
					summary, err := s.getDashboardSummaryAt(now)
					if err != nil {
						t.Fatal(err)
					}
					stats, err := s.getTraderDashboardStatsAt("t", now)
					if err != nil {
						t.Fatal(err)
					}
					account, local, trades := summary.TodayPnL, stats.TodayPnL, stats.TodayTrades
					switch period.name {
					case "week":
						account, local, trades = summary.WeekPnL, stats.WeekPnL, stats.WeekTrades
					case "month":
						account, local, trades = summary.MonthPnL, stats.MonthPnL, stats.MonthTrades
					}
					if account != 5 || local != 5 || trades != 2 {
						t.Fatalf("boundary %s account=%v local=%v trades=%d; want 5,5,2", boundary.Format(time.RFC3339), account, local, trades)
					}
				})
			}
		})
	}
}
