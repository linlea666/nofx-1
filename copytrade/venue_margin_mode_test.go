package copytrade

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"nofx/decision"
)

type marginModeExecutor struct {
	positions []map[string]interface{}
	err       error
}

func (x *marginModeExecutor) ExecuteDecision(*decision.Decision) error { return nil }
func (x *marginModeExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{"total_equity": 100.0}, nil
}
func (x *marginModeExecutor) GetPositions() ([]map[string]interface{}, error) {
	return x.positions, x.err
}

func venuePosition(mode string) map[string]interface{} {
	return map[string]interface{}{"symbol": "AIOTUSDT", "side": "short", "marginMode": mode, "positionAmt": -441.0}
}

// Production: the leader opened AIOT isolated; the Binance Multi-Assets
// account opened it cross. Recording "isolated" hid the copy from its exits.
func TestAdoptVenueMarginModeRecordsWhatTheVenueUsed(t *testing.T) {
	cases := []struct {
		name      string
		positions []map[string]interface{}
		err       error
		want      string
	}{
		{"forced cross is adopted", []map[string]interface{}{venuePosition("cross")}, nil, "cross"},
		{"requested scope exists", []map[string]interface{}{venuePosition("isolated")}, nil, "isolated"},
		{"both scopes stay ambiguous", []map[string]interface{}{venuePosition("cross"), venuePosition("isolated")}, nil, "isolated"},
		{"two foreign scopes stay ambiguous", []map[string]interface{}{venuePosition("cross"), venuePosition("cross")}, nil, "isolated"},
		{"unreadable venue changes nothing", nil, errors.New("timeout"), "isolated"},
		{"other symbols are ignored", []map[string]interface{}{{"symbol": "BTCUSDT", "side": "short", "marginMode": "cross", "positionAmt": -1.0}}, nil, "isolated"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
			ti := &TraderIntegration{traderID: e.traderID, store: st, engine: e, executor: &marginModeExecutor{positions: c.positions, err: c.err}}
			dec := &decision.Decision{Action: "open_short", Symbol: "AIOTUSDT", MarginMode: "isolated", IsCopyTrade: true, LeaderPosID: "fc_57_AIOT-USDT-SWAP_short"}
			ti.adoptVenueMarginMode(dec)
			if dec.MarginMode != c.want {
				t.Fatalf("margin mode=%s want %s", dec.MarginMode, c.want)
			}
		})
	}
	// Reductions and AI reentries are never rewritten.
	e, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
	ti := &TraderIntegration{traderID: e.traderID, store: st, engine: e, executor: &marginModeExecutor{positions: []map[string]interface{}{venuePosition("cross")}}}
	for _, dec := range []*decision.Decision{
		{Action: "close_short", Symbol: "AIOTUSDT", MarginMode: "isolated"},
		{Action: "open_short", Symbol: "AIOTUSDT", MarginMode: "isolated", CopyTradeAction: "ai_reentry", Reasoning: "Copy trading: AI reentry"},
	} {
		ti.adoptVenueMarginMode(dec)
		if dec.MarginMode != "isolated" {
			t.Fatalf("%s rewritten to %s", dec.Action, dec.MarginMode)
		}
	}
}

// The follower may hold the copy in another margin mode than the leader;
// the target still counts the followed leader position, and an empty
// aggregate never turns a partial leader reduce into a full close.
func TestProportionalTargetFollowsMappedFollowerScope(t *testing.T) {
	e := reduceTestEngine(t, ProviderFollowCoin, 1000)
	e.leaderState.Positions["p"].MarginMode = "isolated" // leader isolated, mapping/follower cross
	if _, err := e.store.DB().Exec(`UPDATE copy_trade_position_mappings SET margin_mode='cross' WHERE leader_pos_id='p'`); err != nil {
		t.Fatal(err)
	}
	m, _ := e.store.CopyTrade().GetActiveMapping(e.traderID, "p")
	if total, known := e.followedLeaderGroupNotional(m, "p"); !known || total != 8000 {
		t.Fatalf("leader position excluded by its own margin mode: %v %v", total, known)
	}
	reduce := &SignalMatchResult{Action: ActionReduce, PosID: "p", LeaderPosition: &Position{Size: 80}}
	e.planTargetReduce(reduce)
	if reduce.ReduceRatioOverride != 0.2 {
		t.Fatalf("cross-scope copy not reduced to target: %+v", reduce)
	}

	delete(e.leaderState.Positions, "p")
	reduce = &SignalMatchResult{Action: ActionReduce, PosID: "p", LeaderPosition: &Position{Size: 80}}
	e.planTargetReduce(reduce)
	if reduce.ReduceRatioOverride != 0 || reduce.ReduceSkip {
		t.Fatalf("empty aggregate must keep the leader ratio, not close all: %+v", reduce)
	}
}

func TestPendingDecisionRecordIsKeptOncePerInterval(t *testing.T) {
	ti := &TraderIntegration{}
	if ti.pendingRecordedRecently(7) {
		t.Fatal("first blocked replay must be recorded")
	}
	for i := 0; i < 5; i++ {
		if !ti.pendingRecordedRecently(7) {
			t.Fatal("replays inside the interval must not be recorded again")
		}
	}
	if ti.pendingRecordedRecently(8) {
		t.Fatal("another intent is recorded independently")
	}
	ti.pendingRecordAt[7] = time.Now().Add(-pendingRecordInterval - time.Second)
	if ti.pendingRecordedRecently(7) {
		t.Fatal("a still-blocked intent is recorded again after the interval")
	}
}

func TestRepeatedEquityWarningsAreThrottledAndBounded(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderOKX)
	for i := 0; i < maxRetainedWarnings*3; i++ {
		e.logWarning(Warning{Type: WarnFollowerEquityUnavailable, Symbol: "ETHUSDT"})
		e.logWarning(Warning{Type: "high_value", Symbol: fmt.Sprintf("S%d", i)})
	}
	if len(e.warnings) != maxRetainedWarnings {
		t.Fatalf("warnings not bounded: %d", len(e.warnings))
	}
	if e.stats.WarningsCount != maxRetainedWarnings*6 {
		t.Fatalf("warning statistics lost: %v", e.stats.WarningsCount)
	}
	if last := e.warnings[len(e.warnings)-1]; !strings.HasPrefix(last.Symbol, "S") {
		t.Fatalf("most recent warning not retained: %+v", last)
	}
	if len(e.warningLoggedAt) != 1 {
		t.Fatalf("only equity warnings are throttled: %v", e.warningLoggedAt)
	}
}
