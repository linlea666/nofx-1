package copytrade

import (
	"math"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

func reduceTestEngine(t *testing.T, provider ProviderType, followerValue float64) *Engine {
	t.Helper()
	e, st := newTestCopyTradeEngine(t, provider)
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderID: "leader", LeaderPosID: "p", Symbol: "SOLUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 100, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE copy_trade_position_mappings SET status='active' WHERE trader_id=? AND leader_pos_id='p'`, e.traderID); err != nil {
		t.Fatal(err)
	}
	// Leader: 1000 equity, long 80 SOL @ entry 100 after reducing from 100.
	e.leaderState = &AccountState{TotalEquity: 1000, Positions: map[string]*Position{
		"p": {PosID: "p", Symbol: "SOLUSDT", Side: SideLong, MarginMode: "cross", Size: 80, EntryPrice: 100, PositionValue: 8000},
	}}
	e.getFollowerEquity = func() float64 { return 100 }
	e.getFollowerPositionsResult = func() (map[string]*Position, error) {
		if followerValue <= 0 {
			return map[string]*Position{}, nil
		}
		return map[string]*Position{"f": {Symbol: "SOLUSDT", Side: SideLong, MarginMode: "cross", Size: followerValue / 100, EntryPrice: 100, PositionValue: followerValue}}, nil
	}
	return e
}

func TestReducePriceGateReadsLeaderIntent(t *testing.T) {
	e := reduceTestEngine(t, ProviderHyperliquid, 1000)
	price := 110.0
	e.getFollowerMarketPrice = func(string) (float64, error) { return price, nil }
	leader := &Position{Side: SideLong, Size: 80, EntryPrice: 100}
	reduce := &SignalMatchResult{Action: ActionReduce, PosID: "p", LeaderPosition: leader}

	// Below the leader's entry: a stop, executed at once whatever our price.
	price = 80
	if wait, note := e.reducePriceGate(&Fill{Symbol: "SOLUSDT", Price: 95}, reduce); wait || !strings.Contains(note, "止损减仓") {
		t.Fatalf("stop reduce must not wait: wait=%v note=%q", wait, note)
	}

	// Above the entry: a take-profit at the leader's 110.
	price = 109.6 // 0.36% worse, inside the alt 0.5%
	if wait, note := e.reducePriceGate(&Fill{Symbol: "SOLUSDT", Price: 110}, reduce); wait || !strings.Contains(note, "止盈减仓") {
		t.Fatalf("take-profit inside tolerance must execute: wait=%v note=%q", wait, note)
	}
	price = 108 // 1.8% worse
	if wait, _ := e.reducePriceGate(&Fill{Symbol: "SOLUSDT", Price: 110}, reduce); !wait {
		t.Fatal("take-profit beyond tolerance must wait")
	}
	// The reference stays the leader's first reduce price while the market moves.
	price = 109.6
	if wait, note := e.reducePriceGate(&Fill{Symbol: "SOLUSDT", Price: 108}, reduce); wait || !strings.Contains(note, "领航员减仓价=110") {
		t.Fatalf("reference moved with the market: wait=%v note=%q", wait, note)
	}

	// After the take-profit wait the reduction follows at market.
	price = 108
	if wait, _ := e.reducePriceGate(&Fill{Symbol: "SOLUSDT", Price: 110}, reduce); !wait {
		t.Fatal("new take-profit beyond tolerance must wait")
	}
	e.reduceWaits["p"].since = time.Now().Add(-6 * time.Minute)
	if wait, note := e.reducePriceGate(&Fill{Symbol: "SOLUSDT", Price: 110}, reduce); wait || !strings.Contains(note, "按市价执行") {
		t.Fatalf("take-profit must execute after the wait: wait=%v note=%q", wait, note)
	}

	// A configured wait and tolerance are honoured; a close clears the state.
	wait30, tol := 30, 2.0
	e.config.TakeProfitMaxWaitSeconds, e.config.PriceToleranceAltPct = wait30, &tol
	if wait, _ := e.reducePriceGate(&Fill{Symbol: "SOLUSDT", Price: 110}, reduce); wait {
		t.Fatal("configured 2% tolerance must accept 1.8%")
	}
	e.observeLeaderPosition(&SignalMatchResult{Action: ActionClose, PosID: "p"})
	if e.reduceWaits["p"] != nil {
		t.Fatal("close must clear the reduce wait")
	}

	// A short take-profit is below its entry and buys back.
	short := &SignalMatchResult{Action: ActionReduce, PosID: "s", LeaderPosition: &Position{Side: SideShort, Size: 5, EntryPrice: 100}}
	e.config.PriceToleranceAltPct = nil
	price = 92
	if wait, _ := e.reducePriceGate(&Fill{Symbol: "SOLUSDT", Price: 90}, short); !wait {
		t.Fatal("short take-profit 2.2% above the leader must wait")
	}
}

func TestTargetReduceSizesAgainstProportionalTarget(t *testing.T) {
	// Target after the leader reduce: 8000/1000*100 = 800.
	e := reduceTestEngine(t, ProviderHyperliquid, 1000)
	m := &SignalMatchResult{Action: ActionReduce, PosID: "p", LeaderPosition: &Position{Size: 80}}
	e.planTargetReduce(m)
	if m.ReduceSkip || math.Abs(m.ReduceRatioOverride-0.2) > 1e-9 {
		t.Fatalf("follower at 1000 must reduce 20%% to 800: %+v", m)
	}

	// An earlier skipped reduction is part of the excess.
	e = reduceTestEngine(t, ProviderHyperliquid, 1250)
	m = &SignalMatchResult{Action: ActionReduce, PosID: "p", LeaderPosition: &Position{Size: 80}}
	e.planTargetReduce(m)
	if math.Abs(m.ReduceRatioOverride-(450.0/1250)) > 1e-9 {
		t.Fatalf("accumulated excess not reduced: %+v", m)
	}

	// Already at or below target: nothing to reduce.
	e = reduceTestEngine(t, ProviderHyperliquid, 700)
	m = &SignalMatchResult{Action: ActionReduce, PosID: "p", LeaderPosition: &Position{Size: 80}}
	e.planTargetReduce(m)
	if !m.ReduceSkip {
		t.Fatalf("under-target follower must skip: %+v", m)
	}

	// A remainder below the venue minimum closes everything.
	e = reduceTestEngine(t, ProviderHyperliquid, 1000)
	e.getFollowerMinNotional = func(string) (float64, error) { return 900, nil }
	m = &SignalMatchResult{Action: ActionReduce, PosID: "p", LeaderPosition: &Position{Size: 80}}
	e.planTargetReduce(m)
	if m.ReduceRatioOverride != 1 || !strings.Contains(m.ReduceNote, "直接全平") {
		t.Fatalf("dust remainder must close all: %+v", m)
	}

	// Copy Guard sources keep their own group exit ratio.
	e = reduceTestEngine(t, ProviderOKX, 1000)
	m = &SignalMatchResult{Action: ActionReduce, PosID: "p", LeaderPosition: &Position{Size: 80}}
	e.planTargetReduce(m)
	if m.ReduceRatioOverride != 0 || m.ReduceSkip {
		t.Fatalf("Copy Guard source must not be re-planned: %+v", m)
	}
}

func TestAddIsCappedByAccountLeverage(t *testing.T) {
	e := reduceTestEngine(t, ProviderHyperliquid, 0)
	// Leader account leverage 8x → cap max(12x, 3x) on 100 equity = 1200.
	if size, w := e.capByAccountLeverage("SOLUSDT", 1000, 100, 1000, 500); w != nil || math.Abs(size-200) > 1e-9 {
		t.Fatalf("add not trimmed to the leverage cap: %v %+v", size, w)
	}
	if size, w := e.capByAccountLeverage("SOLUSDT", 1000, 100, 1300, 50); size != 0 || w == nil {
		t.Fatalf("account over the cap must not add: %v %+v", size, w)
	}
}

// End to end: a FollowCoin leader takes profit on a short. While buying back
// is far worse than the leader's price no decision is made; once the price is
// fair the follower reduces to its proportional target. A stop reduces at once.
func TestTakeProfitReduceWaitsThenReducesToTarget(t *testing.T) {
	for _, stop := range []bool{false, true} {
		e, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
		e.config.LeaderID = "57"
		mark := 1900.0 // below the 2000 entry: a short take-profit
		if stop {
			mark = 2100
		}
		eth := fcTestPosition("ETHUSDT", SideShort, 2, mark, 1794694989778)
		eth.EntryPrice = 2000
		if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
			TraderID: e.traderID, LeaderPosID: eth.PosID, LeaderID: "57", Symbol: "ETHUSDT",
			Side: string(SideShort), MarginMode: "cross", OpenedAt: time.Now(), OpenPrice: 2000, LastKnownSize: 3,
		}); err != nil {
			t.Fatal(err)
		}
		e.provider = &fcPollTestProvider{state: &AccountState{TotalEquity: 1000, Timestamp: time.Now(), Positions: map[string]*Position{eth.PosID: eth}}}
		e.getFollowerEquity = func() float64 { return 100 }
		// Target after the reduce: 2*mark/1000*100; the follower holds 1.5x that.
		target := 2 * mark / 1000 * 100
		e.getFollowerPositionsResult = func() (map[string]*Position, error) {
			return map[string]*Position{"f": {Symbol: "ETHUSDT", Side: SideShort, MarginMode: "cross", Size: 0.3, EntryPrice: 2000, PositionValue: target * 1.5}}, nil
		}
		price := mark * 1.03 // buying back 3% above the leader
		e.getFollowerMarketPrice = func(string) (float64, error) { return price, nil }

		e.poll()
		if !stop {
			select {
			case full := <-e.decisionCh:
				t.Fatalf("take-profit far from the leader was executed: %+v", full.Decisions[0])
			default:
			}
			price = mark
			e.poll()
		}
		select {
		case full := <-e.decisionCh:
			d := full.Decisions[0]
			if d.Action != "reduce_short" || math.Abs(d.CloseRatio-1.0/3) > 1e-9 {
				t.Fatalf("stop=%v: want reduce_short to target (1/3), got %s %.6f", stop, d.Action, d.CloseRatio)
			}
			want := "止盈减仓"
			if stop {
				want = "止损减仓"
			}
			if !strings.Contains(d.Reasoning, want) || !strings.Contains(d.Reasoning, "按比例目标减仓") {
				t.Fatalf("stop=%v: reasoning=%q", stop, d.Reasoning)
			}
		default:
			t.Fatalf("stop=%v: reduce not executed", stop)
		}
	}
}
