package copytrade

import (
	"math"
	"testing"
	"time"

	"nofx/store"
)

// Production numbers (trader 滚仓王-466, HL leader 0x1f25…f925): BTC short
// 500, notional 32,266,500; real equity 6,554,442; follower equity 294.25.
const (
	liveLeaderNotional = 32266500.0
	liveLeaderEquity   = 6554442.30
	liveFollowerEquity = 294.25
)

func proportionalTestEngine(t *testing.T, followerValue float64) *Engine {
	t.Helper()
	e, st := newTestCopyTradeEngine(t, ProviderHyperliquid)
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderID: "leader", LeaderPosID: "BTCUSDT_short", Symbol: "BTCUSDT", Side: "short", MarginMode: "cross", LastKnownSize: 500, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE copy_trade_position_mappings SET status='active' WHERE trader_id=? AND leader_pos_id='BTCUSDT_short'`, e.traderID); err != nil {
		t.Fatal(err)
	}
	e.leaderState = &AccountState{TotalEquity: liveLeaderEquity, Positions: map[string]*Position{
		"BTCUSDT_short": {PosID: "BTCUSDT_short", Symbol: "BTCUSDT", Side: SideShort, MarginMode: "cross", Size: 500, PositionValue: liveLeaderNotional},
		// An unrelated, unfollowed leader position never counts toward the target.
		"ETHUSDT_long": {PosID: "ETHUSDT_long", Symbol: "ETHUSDT", Side: SideLong, MarginMode: "cross", Size: 10, PositionValue: 20000},
	}}
	e.getFollowerPositionsResult = func() (map[string]*Position, error) {
		if followerValue <= 0 {
			return map[string]*Position{}, nil
		}
		return map[string]*Position{"x": {Symbol: "BTCUSDT", Side: SideShort, MarginMode: "cross", Size: followerValue / 64600, PositionValue: followerValue}}, nil
	}
	return e
}

func TestAddIsSizedToProportionalTargetOfWholePosition(t *testing.T) {
	target := liveLeaderNotional / liveLeaderEquity * liveFollowerEquity // ≈ 1448.6
	add := &SignalMatchResult{Action: ActionAdd, PosID: "BTCUSDT_short"}

	// The live follower (≈ 11,466 notional, 8x target) must not add at all.
	e := proportionalTestEngine(t, 11466.5)
	size, w := e.sizeAddByProportionalTarget(add, "BTCUSDT", liveLeaderEquity, liveFollowerEquity, 2246.37)
	if size != 0 || w == nil || w.Type != WarnProportionalTargetReached {
		t.Fatalf("overweight follower kept adding: size=%v warning=%+v", size, w)
	}

	// Below target the add is trimmed to the remaining headroom only.
	e = proportionalTestEngine(t, 1000)
	size, w = e.sizeAddByProportionalTarget(add, "BTCUSDT", liveLeaderEquity, liveFollowerEquity, 2246.37)
	if w != nil || math.Abs(size-(target-1000)) > 1e-6 {
		t.Fatalf("add not trimmed to headroom %.4f: size=%v warning=%+v", target-1000, size, w)
	}

	// A small add tops the follower up to the whole deficit, so earlier
	// below-minimum adds are not lost.
	size, w = e.sizeAddByProportionalTarget(add, "BTCUSDT", liveLeaderEquity, liveFollowerEquity, 5)
	if w != nil || math.Abs(size-(target-1000)) > 1e-6 {
		t.Fatalf("deficit not carried: size=%v warning=%+v", size, w)
	}

	// Without a live follower position an add never rebuilds the position.
	e = proportionalTestEngine(t, 0)
	size, w = e.sizeAddByProportionalTarget(add, "BTCUSDT", liveLeaderEquity, liveFollowerEquity, 5)
	if w != nil || size != 5 {
		t.Fatalf("flat follower add must keep the single-change size: size=%v warning=%+v", size, w)
	}
	e = proportionalTestEngine(t, 1000)

	// Opens, other providers and unknown values are left to existing rules.
	if size, w = e.sizeAddByProportionalTarget(&SignalMatchResult{Action: ActionOpen, PosID: "BTCUSDT_short"}, "BTCUSDT", liveLeaderEquity, liveFollowerEquity, 9999); size != 9999 || w != nil {
		t.Fatalf("initial open must not be capped: %v %+v", size, w)
	}
	e.config.ProviderType = ProviderBinance
	if size, w = e.sizeAddByProportionalTarget(add, "BTCUSDT", liveLeaderEquity, liveFollowerEquity, 2246.37); size != 2246.37 || w != nil {
		t.Fatalf("Binance mirrored scale must be excluded: %v %+v", size, w)
	}
}

func TestAddWaitsWhenFollowerPositionsUnavailable(t *testing.T) {
	e := proportionalTestEngine(t, 0)
	e.getFollowerPositionsResult = func() (map[string]*Position, error) { return nil, errTestFollowerDown }
	size, w := e.sizeAddByProportionalTarget(&SignalMatchResult{Action: ActionAdd, PosID: "BTCUSDT_short"}, "BTCUSDT", liveLeaderEquity, liveFollowerEquity, 100)
	if size != 0 || w == nil || w.Type != WarnFollowerEquityUnavailable {
		t.Fatalf("unknown follower position must defer the add: %v %+v", size, w)
	}
}

var errTestFollowerDown = &HLSourceHTTPError{Status: 503, Message: "follower down"}

func TestLeaderEquityPlausibility(t *testing.T) {
	e := proportionalTestEngine(t, 0)
	// The perp sub-balance seen while the leader built the position: 50.1x.
	if w := e.checkLeaderEquityPlausible("BTCUSDT", 643870); w == nil || w.Type != WarnLeaderEquityUnavailable {
		t.Fatalf("implausible 50x denominator accepted: %+v", w)
	}
	e.lastSizingLeaderEquity = 0
	if w := e.checkLeaderEquityPlausible("BTCUSDT", liveLeaderEquity); w != nil {
		t.Fatalf("real equity rejected: %+v", w)
	}
	// A >50% jump is confirmed on the next poll, then accepted.
	if w := e.checkLeaderEquityPlausible("BTCUSDT", 3305850); w == nil {
		t.Fatal("unconfirmed equity jump accepted")
	}
	if w := e.checkLeaderEquityPlausible("BTCUSDT", 3305850); w != nil {
		t.Fatalf("confirmed equity rejected: %+v", w)
	}
	e.config.ProviderType = ProviderBinance
	if w := e.checkLeaderEquityPlausible("BTCUSDT", 1); w != nil {
		t.Fatalf("Binance anchor must be excluded: %+v", w)
	}
}
