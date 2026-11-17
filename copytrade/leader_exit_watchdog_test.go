package copytrade

import (
	"math"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

type exitWatchExecutor struct {
	overweightExecutor
	positions []map[string]interface{}
}

func (x *exitWatchExecutor) GetPositions() ([]map[string]interface{}, error) { return x.positions, nil }

func exitWatchIntegration(t *testing.T) (*TraderIntegration, *exitWatchExecutor) {
	t.Helper()
	e := reduceTestEngine(t, ProviderHyperliquid, 1000)
	if _, err := e.store.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation) VALUES(?,?,'','e',1000,1,'RUNNING',1)`, e.traderID, e.traderID); err != nil {
		t.Fatal(err)
	}
	x := &exitWatchExecutor{positions: []map[string]interface{}{
		{"symbol": "SOLUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 8.5},
		{"symbol": "SOLUSDT", "side": "long", "mgnMode": "isolated", "positionAmt": 3.0}, // another scope
		{"symbol": "SOLUSDT", "side": "short", "mgnMode": "cross", "positionAmt": 2.0},
	}}
	ti := &TraderIntegration{traderID: e.traderID, store: e.store, engine: e, executor: x, lifecycleGeneration: 1}
	e.lastStateSync = time.Now()
	return ti, x
}

// checkAfter runs the watchdog as if the leader had been flat for absent.
func checkAfter(ti *TraderIntegration, absent time.Duration) {
	ti.enforceLeaderExits()
	for posID := range ti.leaderExitAbsentSince {
		ti.leaderExitAbsentSince[posID] = time.Now().Add(-absent)
	}
	ti.lastLeaderExitWatch = time.Time{}
	ti.engine.lastStateSync = time.Now()
	ti.enforceLeaderExits()
}

func TestLeaderExitWatchdogClosesFollowerAfterGrace(t *testing.T) {
	ti, x := exitWatchIntegration(t)
	checkAfter(ti, time.Hour) // leader still holds p
	if len(x.reduced) != 0 {
		t.Fatal("closed while the leader still holds the position")
	}

	ti.engine.leaderState.Positions = map[string]*Position{}
	ti.lastLeaderExitWatch = time.Time{}
	checkAfter(ti, leaderExitEnforceAfter/2)
	if len(x.reduced) != 0 {
		t.Fatal("closed inside the grace period")
	}
	ti.lastLeaderExitWatch = time.Time{}
	checkAfter(ti, leaderExitEnforceAfter+time.Second)
	if len(x.reduced) != 1 || math.Abs(x.reduced[0]-8.5) > 1e-9 || x.reduceTo != "SOLUSDT|long" {
		t.Fatalf("follower not closed in its own margin scope: %+v %s", x.reduced, x.reduceTo)
	}
	var events int
	if err := ti.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_events WHERE event_type='LEADER_EXIT_ENFORCED'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("enforced exit not logged: %d %v", events, err)
	}
}

func TestLeaderExitWatchdogGuards(t *testing.T) {
	// A stale leader snapshot never confirms an exit.
	ti, x := exitWatchIntegration(t)
	ti.engine.leaderState.Positions = map[string]*Position{}
	ti.enforceLeaderExits()
	ti.leaderExitAbsentSince["p"] = time.Now().Add(-time.Hour)
	ti.lastLeaderExitWatch = time.Time{}
	ti.engine.lastStateSync = time.Now().Add(-time.Hour)
	ti.enforceLeaderExits()
	if len(x.reduced) != 0 {
		t.Fatal("closed on a stale leader snapshot")
	}

	// Another leader position on the same symbol+side keeps the follower.
	ti, x = exitWatchIntegration(t)
	ti.engine.leaderState.Positions = map[string]*Position{
		"q": {PosID: "q", Symbol: "SOLUSDT", Side: SideLong, MarginMode: "cross", Size: 5, EntryPrice: 100, PositionValue: 500},
	}
	checkAfter(ti, time.Hour)
	if len(x.reduced) != 0 {
		t.Fatal("closed while the leader holds the symbol+side")
	}

	// A position the user took over is left alone.
	ti, x = exitWatchIntegration(t)
	if _, err := ti.store.DB().Exec(`INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,cycle_id,attempt_no,symbol,side,margin_mode,state,reason,opened_at) VALUES(?,?,1,0,0,'SOLUSDT','long','cross','RELEASED','manual',CURRENT_TIMESTAMP)`, ti.traderID, "p"); err != nil {
		t.Fatal(err)
	}
	ti.engine.leaderState.Positions = map[string]*Position{}
	checkAfter(ti, time.Hour)
	if len(x.reduced) != 0 {
		t.Fatal("closed a released (user-managed) position")
	}
}

// A catch-up whose bookkeeping cannot settle never blocks a close/reduce,
// while risk-increasing actions still stop on it.
func TestStaleCatchupBookkeepingNeverBlocksExit(t *testing.T) {
	ti, _ := exitWatchIntegration(t)
	cs := ti.store.CopyTrade()
	intent, claimed, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: ti.traderID, LeaderPosID: "orphan", SourceRevision: 3,
		CanonicalKey: "leader|" + ti.traderID + "|orphan|3", Action: "open_long",
		Symbol: "SOLUSDT", Side: "long", LeaderTargetSize: 10, ClientOrderID: "orphan-catchup",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "orphan-catchup", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, "orphan-catchup", store.ExecutionOrderAttemptTerminalNoFill, "", "CANCELED", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = ti.store.DB().Exec(`UPDATE copy_trade_execution_intents SET status='PARTIALLY_FILLED',reason_code='CATCHUP_PENDING' WHERE id=?`, intent.ID); err != nil {
		t.Fatal(err)
	}
	next := func(action string) *decision.Decision {
		return &decision.Decision{Action: action, Symbol: "SOLUSDT", LeaderPosID: "orphan", SourceRevision: 5, ExecutionIntentID: intent.ID + 100, IsCopyTrade: true}
	}
	if err = ti.supersedeOlderOrdinaryCatchup(next("open_long")); err == nil {
		t.Fatal("risk increase must still stop on an unsettled catch-up")
	}
	if err = ti.supersedeOlderOrdinaryCatchup(next("close_long")); err != nil {
		t.Fatalf("close blocked by catch-up bookkeeping: %v", err)
	}
	if err = ti.supersedeOlderOrdinaryCatchup(next("reduce_long")); err != nil {
		t.Fatalf("reduction blocked by catch-up bookkeeping: %v", err)
	}
}
