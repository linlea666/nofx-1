package copytrade

import (
	"path/filepath"
	"testing"

	"nofx/decision"
	"nofx/store"
)

// The executors below declare their venue: on OKX, cross and isolated
// positions of one symbol/side are separate venue positions.
type okxEntryExecutor struct{ *continuityExecutor }

func (*okxEntryExecutor) GetExchange() string { return "okx" }

type binanceEntryExecutor struct{ *continuityExecutor }

func (*binanceEntryExecutor) GetExchange() string { return "binance" }

type okxExitExecutor struct{ *leaderExitExecutor }

func (*okxExitExecutor) GetExchange() string { return "okx" }

func manualPositionExecutor(mode string) *continuityExecutor {
	ex := &continuityExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}}
	ex.setPosition(100, 1, 10, 100, 80)
	ex.positions[0]["marginMode"] = mode
	return ex
}

func copyOpenDecision(mode string) *decision.Decision {
	return &decision.Decision{IsCopyTrade: true, LeaderPosID: "hl-eth-long", Symbol: "ETHUSDT", Action: "open_long", MarginMode: mode, Leverage: 10}
}

// Production case: an OKX account holds a manual isolated ETH long and the
// leader opens a long that is copied in cross. OKX keeps them as two venue
// positions, so the copy must not be skipped as an independent conflict.
func TestCopyEntryIgnoresManualPositionInOtherMarginModeOnOKX(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "margin-scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cases := []struct {
		name     string
		executor DecisionExecutor
		copyMode string
		conflict bool
	}{
		{"okx isolated manual, cross copy", &okxEntryExecutor{manualPositionExecutor("isolated")}, "cross", false},
		{"okx cross manual, isolated copy", &okxEntryExecutor{manualPositionExecutor("cross")}, "isolated", false},
		{"okx same margin mode", &okxEntryExecutor{manualPositionExecutor("cross")}, "cross", true},
		{"okx copy mode unknown", &okxEntryExecutor{manualPositionExecutor("isolated")}, "", true},
		{"binance one margin type per symbol", &binanceEntryExecutor{manualPositionExecutor("isolated")}, "cross", true},
		{"venue unknown", manualPositionExecutor("isolated"), "cross", true},
	}
	for _, tc := range cases {
		ti := controlIntegration(st, tc.executor)
		err := ti.preflightCopyPositionOwnership(copyOpenDecision(tc.copyMode))
		if got := ReasonCodeOf(err) == "INDEPENDENT_POSITION_CONFLICT"; got != tc.conflict {
			t.Fatalf("%s: conflict=%v want %v (err=%v)", tc.name, got, tc.conflict, err)
		}
	}
}

// A leader exit on OKX closes the copied margin mode only; an independent
// position in the other mode is neither closed nor counted as a residual.
func TestLeaderExitOnOKXLeavesOtherMarginModeManualPosition(t *testing.T) {
	ti, ex, d := leaderExitFixture(t, 1)
	okx := NewTraderIntegration(ti.traderID, &okxExitExecutor{ex}, ti.store)
	okx.engine = ti.engine
	d.MarginMode = "cross"
	ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 3), exitPosition("isolated", "i", "long", 2)}
	if err := okx.executeAccountLeaderExit(d, true); err != nil {
		t.Fatalf("full exit must complete without treating the manual isolated position as residual: %v", err)
	}
	if len(ex.requests) != 1 || ex.requests[0].MarginMode != "cross" {
		t.Fatalf("exit must close only the copied margin mode: %+v", ex.requests)
	}
	if getFloatField(ex.positions[1], "positionAmt") != 2 {
		t.Fatal("manual isolated position was touched by the leader exit")
	}
}

// Leader exits take priority: if the copied mode recorded on the decision has
// no visible position, the exit keeps the whole symbol/side instead of
// skipping the close.
func TestLeaderExitOnOKXFallsBackWhenCopiedModeIsNotVisible(t *testing.T) {
	ti, ex, d := leaderExitFixture(t, 1)
	okx := NewTraderIntegration(ti.traderID, &okxExitExecutor{ex}, ti.store)
	okx.engine = ti.engine
	d.MarginMode = "isolated"
	ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 3)}
	if err := okx.executeAccountLeaderExit(d, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.requests) != 1 || ex.requests[0].MarginMode != "cross" {
		t.Fatalf("stale margin-mode record must not skip the leader exit: %+v", ex.requests)
	}
}
