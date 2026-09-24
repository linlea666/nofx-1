package copytrade

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

func TestLeaderExitFilledACKWithoutQuantityQueriesOriginalOrder(t *testing.T) {
	for _, reply := range []string{"available", "timeout", "live", "missing quantity", "wrong identity"} {
		t.Run(reply, func(t *testing.T) {
			ti, ex, d := leaderExitFixture(t, .5)
			ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 10)}
			ex.ackOnly = true
			switch reply {
			case "timeout":
				ex.lookupErr = errors.New("timeout")
			case "live":
				ex.lookupReply = map[string]interface{}{"orderId": "exit-1", "status": "NEW", "executedQty": 0.0}
			case "missing quantity":
				ex.lookupReply = map[string]interface{}{"orderId": "exit-1", "status": "FILLED"}
			case "wrong identity":
				ex.lookupReply = map[string]interface{}{"orderId": "other", "status": "FILLED", "executedQty": 5.0}
			}
			err := ti.executeAccountLeaderExit(d, true)
			if reply == "available" && err != nil {
				t.Fatal(err)
			}
			if reply != "available" {
				if err == nil {
					t.Fatal("unknown acknowledgement completed")
				}
				restarted := NewTraderIntegration(ti.traderID, ex, ti.store)
				restarted.engine = ti.engine
				for i := 0; i < 3; i++ {
					_ = restarted.executeAccountLeaderExit(d, true)
				}
				if len(ex.requests) != 1 {
					t.Fatalf("unknown acknowledgement resubmitted: %d", len(ex.requests))
				}
				ex.lookupErr = nil
				ex.lookupReply = nil
				if err = restarted.executeAccountLeaderExit(d, true); err != nil {
					t.Fatal(err)
				}
			}
			if len(ex.requests) != 1 || getFloatField(ex.positions[0], "positionAmt") != 5 {
				t.Fatal("reduced more than the frozen target")
			}
			intent, _ := ti.store.CopyTrade().GetExecutionIntentByID(d.ExecutionIntentID)
			if intent.FilledQuantity != 5 {
				t.Fatalf("missing actual fill: %+v", intent)
			}
		})
	}
}

func seedLegacyExitReceipt(t *testing.T, ti *TraderIntegration, ex *leaderExitExecutor, d *decision.Decision, qty float64, index int, kind string) {
	t.Helper()
	id, order := fmt.Sprintf("legacy-client-%d", index), fmt.Sprintf("legacy-order-%d", index)
	cs := ti.store.CopyTrade()
	if _, err := cs.PrepareExecutionOrderAttemptRecordWithKind(d.ExecutionIntentID, id, kind, qty, qty); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.MarkExecutionOrderAttemptSubmitted(d.ExecutionIntentID, id); err != nil {
		t.Fatal(err)
	}
	if _, err := ti.store.DB().Exec(`UPDATE copy_trade_execution_order_attempts SET status='SUBMITTED',exchange_state='FILLED',exchange_order_id=?,filled_quantity=0,terminal_at=CURRENT_TIMESTAMP WHERE intent_id=? AND client_order_id=?`, order, d.ExecutionIntentID, id); err != nil {
		t.Fatal(err)
	}
	if ex.orders == nil {
		ex.orders = map[string]map[string]interface{}{}
	}
	ex.orders[id] = map[string]interface{}{"orderId": order, "status": "FILLED", "executedQty": qty, "fillTime": float64(time.Now().Add(-time.Hour).UnixMilli())}
}

func TestLegacyRepeatedExitRepairDoesNotReplayAndPreservesDeferredSource(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			ti, ex, d := leaderExitFixture(t, .1)
			ex.positions = []map[string]interface{}{exitPosition("cross", "c", "long", 1.368)}
			plan, err := ti.prepareLeaderExit(d)
			if err != nil {
				t.Fatal(err)
			}
			kind := "LEADER_EXIT:" + strings.ToUpper(stableClientOrderID(ti.traderID, plan.Targets[0].Key, "scope"))
			for n := 1; n <= 13; n++ {
				qty := .109
				if n == 13 {
					qty = .06
				}
				seedLegacyExitReceipt(t, ti, ex, d, qty, n, kind)
			}
			ex.positions = nil
			if _, err = ti.store.DB().Exec(`INSERT INTO copy_trade_source_transitions(trader_id,leader_pos_id,source_fill_id,source_revision,action,leader_target_size,intent_id,status) VALUES(?,'p','deferred-close',2,'close_long',0,?,'SOURCE_REPLAY_PENDING')`, ti.traderID, d.ExecutionIntentID); err != nil {
				t.Fatal(err)
			}
			if completed {
				if _, err = ti.store.DB().Exec(`UPDATE copy_trade_leader_exits SET completed=1 WHERE intent_id=?`, d.ExecutionIntentID); err != nil {
					t.Fatal(err)
				}
				if _, err = ti.store.DB().Exec(`UPDATE copy_trade_execution_intents SET status='FILLED',terminal_at=CURRENT_TIMESTAMP WHERE id=?`, d.ExecutionIntentID); err != nil {
					t.Fatal(err)
				}
			} else if err = ti.store.CopyTrade().CompleteLeaderExit(d.ExecutionIntentID); err == nil {
				t.Fatal("legacy false terminal was accepted")
			}
			ti.recoverExitOrderEvidence()
			if !completed {
				if err = ti.executeAccountLeaderExit(d, true); err != nil {
					t.Fatal(err)
				}
			}
			// Restart/re-run repair never repeats a business action.
			ti.lastExitEvidenceID = 0
			ti.recoverExitOrderEvidence()
			var count int
			if err = ti.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_order_evidence_repairs`).Scan(&count); err != nil || count != 13 {
				t.Fatalf("audit count=%d err=%v", count, err)
			}
			intent, _ := ti.store.CopyTrade().GetExecutionIntentByID(d.ExecutionIntentID)
			if math.Abs(intent.FilledQuantity-1.368) > 1e-9 || len(ex.requests) != 0 {
				t.Fatalf("historical correction replayed/lost fills: %+v requests=%d", intent, len(ex.requests))
			}
			var status string
			if err = ti.store.DB().QueryRow(`SELECT status FROM copy_trade_source_transitions WHERE source_fill_id='deferred-close'`).Scan(&status); err != nil || status != "SOURCE_REPLAY_PENDING" {
				t.Fatalf("later close swallowed: %s %v", status, err)
			}
			m, _ := ti.store.CopyTrade().GetMapping(ti.traderID, "p")
			if completed && m.SourceRevision != 1 {
				t.Fatal("historical repair changed source progress")
			}
			if !completed {
				next, _, err := ti.store.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: ti.traderID, LeaderPosID: "p", SourceRevision: 3, SourceKind: "LEADER_TRANSITION", Action: "close_long", Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: 0, LeaderExitScope: leaderAccountExitScope, LeaderExitRatio: 1, SourceFillID: "deferred-close", ClientOrderID: "final-close"})
				if err != nil {
					t.Fatal(err)
				}
				close := *d
				close.ExecutionIntentID = next.ID
				close.SourceRevision = 3
				close.LeaderPosSize = 0
				close.Action = "close_long"
				close.CopyTradeAction = "close"
				close.CloseRatio = 1
				if err = ti.executeAccountLeaderExit(&close, true); err != nil {
					t.Fatal(err)
				}
				m, _ = ti.store.CopyTrade().GetMapping(ti.traderID, "p")
				if m != nil && m.Status != store.MappingStatusClosed {
					t.Fatalf("final close stuck: %+v", m)
				}
			}
		})
	}
}

func TestFlatFollowerDuringExitReconciliationDoesNotRearmOrSettle(t *testing.T) {
	st, c, _ := seedHardeningFill(t)
	c.OpenedAt = time.Now().Add(-time.Minute)
	ex := &positionMarginLifecycleExecutor{}
	ti := NewTraderIntegration("t", ex, st)
	intent, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "p", SourceRevision: 2, SourceKind: "LEADER_TRANSITION", Action: "reduce_long", Symbol: "ETHUSDT", Side: "long", LeaderTargetSize: .5})
	if err != nil {
		t.Fatal(err)
	}
	if !ti.confirmCopyGuardFollowerAbsent(c) {
		t.Fatal("confirmed flat exit kept re-arming")
	}
	current, _ := st.CopyTrade().GetCopyGuardCycle(c.ID)
	m, _ := st.CopyTrade().GetMapping("t", "p")
	i, _ := st.CopyTrade().GetExecutionIntentByID(intent.ID)
	if current.ProtectionStatus != store.CopyGuardProtectionFlatReconciling || current.ClosedAt != nil || m.Status != store.MappingStatusActive || m.SourceRevision != 1 || i.Status != store.ExecutionIntentReserved {
		t.Fatalf("flatness settled business state: %+v %+v %+v", current, m, i)
	}
	ti.markProtectionIssue(c, store.CopyGuardProtectionDegraded, "PROTECTION_DEGRADED", errors.New("missing old position"), 0, false)
	current, _ = st.CopyTrade().GetCopyGuardCycle(c.ID)
	if current.ProtectionStatus != store.CopyGuardProtectionFlatReconciling {
		t.Fatal("stale protection task undid flatness")
	}
	// A late possible opening order invalidates the flatness decision.
	if _, _, err = st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "t", LeaderPosID: "other", SourceRevision: 1, SourceKind: "LEADER_TRANSITION", Action: "open_long", Symbol: "ETHUSDT", Side: "long"}); err != nil {
		t.Fatal(err)
	}
	if ti.confirmCopyGuardFollowerAbsent(c) {
		t.Fatal("flatness ignored an in-flight opening")
	}
	ex.freshErr = errors.New("position API unavailable")
	if ti.confirmCopyGuardFollowerAbsent(c) {
		t.Fatal("API error was interpreted as flat")
	}
}
