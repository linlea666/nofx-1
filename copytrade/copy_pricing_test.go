package copytrade

import (
	"math"
	"strings"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

func TestCopyPriceToleranceBySymbol(t *testing.T) {
	for symbol, want := range map[string]float64{"BTCUSDT": 0.2, "ETHUSDT": 0.2, "ETHFIUSDT": 0.5, "SOLUSDT": 0.5, "ZAMAUSDT": 0.5} {
		if got := copyPriceTolerancePct(symbol); got != want {
			t.Fatalf("%s tolerance=%v want %v", symbol, got, want)
		}
	}
	cases := []struct {
		buying          bool
		price, ref, tol float64
		within          bool
	}{
		{true, 99, 100, 0.2, true},     // buying below the leader
		{true, 100.15, 100, 0.2, true}, // slightly worse, inside tolerance
		{true, 100.3, 100, 0.2, false}, // worse than tolerance
		{false, 101, 100, 0.2, true},   // selling above the leader
		{false, 99.85, 100, 0.2, true},
		{false, 99.7, 100, 0.2, false},
	}
	for _, c := range cases {
		if got := copyPriceWithinTolerance(c.buying, c.price, c.ref, c.tol); got != c.within {
			t.Fatalf("%+v within=%v", c, got)
		}
	}
}

// HL reports only the average entry; the add price follows from its move.
func TestLeaderAddReferencePriceDerivesAndAccumulates(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderHyperliquid)
	pos := &Position{Size: 10, EntryPrice: 100}
	match := &SignalMatchResult{Action: ActionOpen, PosID: "p", LeaderPosition: pos}
	e.observeLeaderPosition(match)

	// 10 @ 100 → 15 @ 102: the 5 added cost (15*102 - 10*100)/5 = 106.
	add1 := &SignalMatchResult{Action: ActionAdd, PosID: "p", LeaderPosition: &Position{Size: 15, EntryPrice: 102}}
	if ref := e.leaderAddReferencePrice(add1, &Fill{Price: 105}, 10, 1); math.Abs(ref-106) > 1e-9 {
		t.Fatalf("derived add price=%v want 106", ref)
	}
	e.observeLeaderPosition(add1)
	// A replay of the same revision does not count the add twice.
	if ref := e.leaderAddReferencePrice(add1, &Fill{Price: 105}, 10, 1); math.Abs(ref-106) > 1e-9 {
		t.Fatalf("replay changed reference: %v", ref)
	}
	// A second unbought add: 15 @ 102 → 20 @ 104 → 5 added at 110; VWAP 108.
	add2 := &SignalMatchResult{Action: ActionAdd, PosID: "p", LeaderPosition: &Position{Size: 20, EntryPrice: 104}}
	if ref := e.leaderAddReferencePrice(add2, &Fill{Price: 109}, 15, 1); math.Abs(ref-108) > 1e-9 {
		t.Fatalf("VWAP of unbought adds=%v want 108", ref)
	}
	e.observeLeaderPosition(add2)
	// Once the follower grew, the earlier adds were bought: start over.
	add3 := &SignalMatchResult{Action: ActionAdd, PosID: "p", LeaderPosition: &Position{Size: 25, EntryPrice: 105}}
	if ref := e.leaderAddReferencePrice(add3, &Fill{Price: 108}, 20, 2); math.Abs(ref-109) > 1e-9 {
		t.Fatalf("reference after follower fill=%v want 109", ref)
	}
	// A reduction clears the accumulation; an unknown entry falls back to the snapshot price.
	e.observeLeaderPosition(&SignalMatchResult{Action: ActionReduce, PosID: "p", LeaderPosition: &Position{Size: 12, EntryPrice: 105}})
	if acc := e.leaderAdds["p"]; acc != nil {
		t.Fatalf("reduce kept stale adds: %+v", acc)
	}
	e.observeLeaderPosition(&SignalMatchResult{Action: ActionClose, PosID: "p"})
	add4 := &SignalMatchResult{Action: ActionAdd, PosID: "p", LeaderPosition: &Position{Size: 14, EntryPrice: 106}}
	if ref := e.leaderAddReferencePrice(add4, &Fill{Price: 107}, 12, 2); ref != 107 {
		t.Fatalf("missing entry history must use the snapshot price: %v", ref)
	}
}

func priceGateEngine(t *testing.T, followerQty, followerEntry float64) (*Engine, *store.Store) {
	t.Helper()
	e, st := newTestCopyTradeEngine(t, ProviderHyperliquid)
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderID: "leader", LeaderPosID: "p", Symbol: "SOLUSDT", Side: "long", MarginMode: "cross", LastKnownSize: 10, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE copy_trade_position_mappings SET status='active' WHERE trader_id=? AND leader_pos_id='p'`, e.traderID); err != nil {
		t.Fatal(err)
	}
	e.getFollowerPositionsResult = func() (map[string]*Position, error) {
		if followerQty <= 0 {
			return map[string]*Position{}, nil
		}
		return map[string]*Position{"f": {Symbol: "SOLUSDT", Side: SideLong, MarginMode: "cross", Size: followerQty, EntryPrice: followerEntry, PositionValue: followerQty * followerEntry}}, nil
	}
	return e, st
}

func TestRiskIncreasePriceGate(t *testing.T) {
	price := 100.0
	e, st := priceGateEngine(t, 1, 100)
	e.getFollowerMarketPrice = func(string) (float64, error) { return price, nil }
	e.observeLeaderPosition(&SignalMatchResult{Action: ActionOpen, PosID: "p", LeaderPosition: &Position{Size: 10, EntryPrice: 100}})
	add := &SignalMatchResult{Action: ActionAdd, PosID: "p", LeaderPosition: &Position{Size: 20, EntryPrice: 100}} // added at 100
	dec := &decision.Decision{Action: "open_long", Symbol: "SOLUSDT"}

	price = 100.4 // 0.4% worse, inside the 0.5% alt tolerance
	if wait, note := e.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, add, dec, 50); wait || !strings.Contains(note, "补仓") {
		t.Fatalf("inside tolerance must execute: wait=%v note=%q", wait, note)
	}
	price = 101 // 1% worse
	if wait, _ := e.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, add, dec, 50); !wait {
		t.Fatal("price beyond tolerance must wait at the leader price")
	}
	price = 99 // better than the leader
	if wait, _ := e.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, add, dec, 50); wait {
		t.Fatal("a better price must execute at once")
	}

	// Cost credit: the follower bought at 95 vs the leader's 100 (5% better),
	// so an alt add may be up to min(0.5+5, 1.5)% worse than the leader.
	e2, _ := priceGateEngine(t, 1, 95)
	addPrice := 101.0
	e2.getFollowerMarketPrice = func(string) (float64, error) { return addPrice, nil }
	e2.observeLeaderPosition(&SignalMatchResult{Action: ActionOpen, PosID: "p", LeaderPosition: &Position{Size: 10, EntryPrice: 100}})
	if wait, note := e2.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, add, dec, 50); wait || !strings.Contains(note, "成本优势") {
		t.Fatalf("cost credit not applied: wait=%v note=%q", wait, note)
	}
	addPrice = 101.6 // beyond the 1.5% cap
	if wait, _ := e2.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, add, dec, 50); !wait {
		t.Fatal("cost credit must stay capped at 3x the tolerance")
	}

	// Initial open: wait at most two minutes, then follow at market.
	open := &SignalMatchResult{Action: ActionOpen, PosID: "new", LeaderPosition: &Position{Size: 5, EntryPrice: 100}}
	intent, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: e.traderID, LeaderPosID: "new", SourceRevision: 1, SourceKind: "LEADER_TRANSITION", CanonicalKey: "k-open", Action: "open_long", Symbol: "SOLUSDT", Side: "long", LeaderTargetSize: 5, ClientOrderID: "c-open"})
	if err != nil {
		t.Fatal(err)
	}
	openDec := &decision.Decision{Action: "open_long", Symbol: "SOLUSDT", ExecutionIntentID: intent.ID}
	price = 101
	if wait, _ := e.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, open, openDec, 50); !wait {
		t.Fatal("fresh initial open beyond tolerance must wait")
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET created_at=? WHERE id=?`, time.Now().Add(-3*time.Minute).UTC().Format("2006-01-02 15:04:05"), intent.ID); err != nil {
		t.Fatal(err)
	}
	if wait, note := e.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, open, openDec, 50); wait || !strings.Contains(note, "按市价跟上") {
		t.Fatalf("initial open must follow after the wait: wait=%v note=%q", wait, note)
	}

	// A leader reversal opens the new side at once, like a stop.
	reversed := &SignalMatchResult{Action: ActionOpen, PosID: "rev", LeaderReversed: true, LeaderPosition: &Position{Size: 5, EntryPrice: 100}}
	price = 105
	if wait, _ := e.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, reversed, &decision.Decision{Action: "open_long", Symbol: "SOLUSDT"}, 50); wait {
		t.Fatal("a reversal must not wait for price")
	}

	// Short adds mirror the direction; other providers are untouched.
	e3, _ := priceGateEngine(t, 0, 0)
	e3.getFollowerMarketPrice = func(string) (float64, error) { return price, nil }
	e3.observeLeaderPosition(&SignalMatchResult{Action: ActionOpen, PosID: "p", LeaderPosition: &Position{Size: 10, EntryPrice: 100}})
	shortDec := &decision.Decision{Action: "open_short", Symbol: "SOLUSDT"}
	price = 99
	if wait, _ := e3.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, add, shortDec, 50); !wait {
		t.Fatal("selling 1% below the leader must wait")
	}
	price = 100.6
	if wait, _ := e3.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, add, shortDec, 50); wait {
		t.Fatal("selling above the leader must execute")
	}
	e3.config.ProviderType = ProviderBinance
	price = 99
	if wait, _ := e3.riskIncreasePriceGate(&Fill{Symbol: "SOLUSDT", Price: 100}, add, shortDec, 50); wait {
		t.Fatal("Binance must not be gated")
	}
}

// End to end: a new leader open beyond tolerance produces no decision and
// keeps its source revision replayable; the next poll at a fair price copies it.
func TestInitialOpenWaitsThenFollowsThroughSignalFlow(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
	e.config.LeaderID = "57"
	e.config.MinTradeWarn = 1
	pos := fcTestPosition("ETHUSDT", SideShort, 1, 2000, 1794694989778)
	e.provider = &fcPollTestProvider{state: &AccountState{TotalEquity: 1000, Timestamp: time.Now(), Positions: map[string]*Position{pos.PosID: pos}}}
	price := 1990.0 // a short sold 0.5% below the leader's 2000; ETH allows 0.2%
	e.getFollowerMarketPrice = func(string) (float64, error) { return price, nil }

	e.poll()
	select {
	case full := <-e.decisionCh:
		t.Fatalf("open beyond tolerance was sent: %+v", full.Decisions[0])
	default:
	}
	var status, reason, lastError string
	if err := st.DB().QueryRow(`SELECT status,reason_code,last_error FROM copy_trade_execution_intents WHERE trader_id=? AND leader_pos_id=?`, e.traderID, pos.PosID).Scan(&status, &reason, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "RECONCILING" || reason != "SOURCE_REVALIDATION_REQUIRED" || !strings.Contains(lastError, "PRICE_WAIT") {
		t.Fatalf("waiting open not deferred for replay: %s %s %s", status, reason, lastError)
	}

	price = 2001 // selling above the leader's entry
	e.poll()
	select {
	case full := <-e.decisionCh:
		d := full.Decisions[0]
		if d.Action != "open_short" || d.LeaderPosID != pos.PosID || !strings.Contains(d.Reasoning, "首仓") {
			t.Fatalf("replayed open not copied: %+v", d)
		}
	default:
		t.Fatal("open not copied once the price came back")
	}
}
