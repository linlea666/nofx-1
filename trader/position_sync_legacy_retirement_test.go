package trader

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"nofx/store"
)

// The incident predates per-order execution evidence: mapping acknowledgement
// marked a close FILLED although no venue outcome was recorded. Retirement may
// release its current account risk, but must not invent that missing outcome.
func legacyStoppedReconciliationFixture(t *testing.T) (*store.Store, *PositionSyncManager, *store.CopyTradeExecutionIntent) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "legacy-stopped-retirement.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	insertPositionSyncTrader(t, st, "old-trader", "historical stopped trader", "shared-account", false)
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_position_mappings
		(trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,source_revision,last_known_size)
		VALUES('old-trader','leader','old-source','ARXUSDT','short','cross','closed',7,0)`); err != nil {
		t.Fatal(err)
	}
	result, err := st.DB().Exec(`INSERT INTO copy_trade_execution_intents
		(trader_id,leader_pos_id,source_revision,source_kind,canonical_key,action,symbol,side,margin_mode,
		 leader_target_size,requested_quantity,quantized_quantity,status,reason_code,submitted_at,filled_at,
		 created_at,updated_at)
		VALUES('old-trader','old-source',7,'LEADER_TRANSITION','historical-close',
		 'close_short','ARXUSDT','short','cross',0,12,12,'FILLED','MAPPING_ALREADY_ACKNOWLEDGED',
		 '2026-07-01 10:00:00','2026-07-01 10:00:03','2026-07-01 09:59:59','2026-07-01 10:00:03')`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	intent, err := st.CopyTrade().GetExecutionIntentByID(id)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewPositionSyncManager(st, time.Second)
	return st, manager, intent
}

type legacyRetirementSnapshotTrader struct {
	positionSyncFakeTrader
	positionErr   error
	orderErr      error
	positionReads int
	orderReads    int
}

func (f *legacyRetirementSnapshotTrader) GetPositionsFresh() ([]map[string]interface{}, error) {
	f.positionReads++
	return f.positions, f.positionErr
}

func (f *legacyRetirementSnapshotTrader) GetPendingOrdersFresh() ([]PendingOrderSnapshot, error) {
	f.orderReads++
	return f.pendingOrders, f.orderErr
}

func assertLegacyRetirementHistoryUnchanged(t *testing.T, st *store.Store, before *store.CopyTradeExecutionIntent, auditCount int) {
	t.Helper()
	after, err := st.CopyTrade().GetExecutionIntentByID(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("retirement rewrote historical execution instead of recording independent risk evidence:\nbefore=%+v\nafter=%+v", before, after)
	}
	var actualCount int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_venue_retirements WHERE intent_id=?`, before.ID).Scan(&actualCount); err != nil {
		t.Fatal(err)
	}
	if actualCount != auditCount {
		t.Fatalf("retirement audit count=%d, want=%d", actualCount, auditCount)
	}
}

func TestStoppedLegacyRetirementArchivesWithoutRewritingHistoricalExecution(t *testing.T) {
	st, manager, original := legacyStoppedReconciliationFixture(t)
	fake := &legacyRetirementSnapshotTrader{}
	manager.traderCache["old-trader"] = fake
	insertPositionSyncTrader(t, st, "new-trader", "new follower", "shared-account", false)

	// Pin the actual user-visible failure before the explicit reconciliation.
	start, err := st.Trader().BeginStart("user-1", "new-trader")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().CompleteCopyGuardStart("user-1", "new-trader", start.Generation, "shared-account"); err == nil {
		t.Fatal("historical unresolved close did not initially block the new account owner")
	}
	if err = st.Trader().FailStart("user-1", "new-trader", start.Generation, "expected old account obligation"); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		blockers, reconcileErr := manager.ReconcileStoppedTrader("old-trader")
		if reconcileErr != nil || len(blockers) != 0 {
			t.Fatalf("reconcile run %d: blockers=%+v err=%v", run, blockers, reconcileErr)
		}
		assertLegacyRetirementHistoryUnchanged(t, st, original, 1)
	}
	if fake.positionReads != 2 || fake.orderReads != 2 {
		t.Fatalf("each reconciliation must refresh the full venue snapshot: positions=%d orders=%d", fake.positionReads, fake.orderReads)
	}
	blockers, err := st.Trader().Archive("user-1", "old-trader")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("explicit flat retirement did not unblock archive: blockers=%+v err=%v", blockers, err)
	}
	oldLifecycle, err := st.Trader().GetLifecycle("old-trader")
	if err != nil || oldLifecycle.Status != store.TraderLifecycleArchived {
		t.Fatalf("old trader was not archived: %+v err=%v", oldLifecycle, err)
	}
	start, err = st.Trader().BeginStart("user-1", "new-trader")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().CompleteCopyGuardStart("user-1", "new-trader", start.Generation, "shared-account"); err != nil {
		t.Fatalf("retired historical account risk still blocked the new follower: %v", err)
	}
	current, err := st.Trader().GetLifecycle("new-trader")
	if err != nil || current.Status != store.TraderLifecycleRunning || !current.IsRunning {
		t.Fatalf("new follower did not become RUNNING: %+v err=%v", current, err)
	}
	assertLegacyRetirementHistoryUnchanged(t, st, original, 1)
}

func TestStoppedLegacyRetirementRequiresFreshRiskFreeVenueSnapshot(t *testing.T) {
	cases := []struct {
		name        string
		fake        legacyRetirementSnapshotTrader
		blockerCode string
		wantError   bool
	}{
		{
			name: "position on another contract",
			fake: legacyRetirementSnapshotTrader{positionSyncFakeTrader: positionSyncFakeTrader{
				positions: []map[string]interface{}{{"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.1}},
			}},
			blockerCode: "EXCHANGE_POSITION_PRESENT",
		},
		{
			name: "ordinary pending order",
			fake: legacyRetirementSnapshotTrader{positionSyncFakeTrader: positionSyncFakeTrader{
				pendingOrders: []PendingOrderSnapshot{{ID: "entry-order", Symbol: "ETHUSDT", Status: "live"}},
			}},
			blockerCode: "EXCHANGE_ORDER_PENDING",
		},
		{
			name: "protective pending order",
			fake: legacyRetirementSnapshotTrader{positionSyncFakeTrader: positionSyncFakeTrader{
				pendingOrders: []PendingOrderSnapshot{{ID: "algo-order", Symbol: "ARXUSDT", Status: "live", Protective: true}},
			}},
			blockerCode: "EXCHANGE_PROTECTIVE_ORDER_PENDING",
		},
		{name: "position API failed", fake: legacyRetirementSnapshotTrader{positionErr: errors.New("position snapshot unavailable")}, wantError: true},
		{name: "pending order API failed", fake: legacyRetirementSnapshotTrader{orderErr: errors.New("algo snapshot unavailable")}, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, manager, original := legacyStoppedReconciliationFixture(t)
			manager.traderCache["old-trader"] = &tc.fake
			blockers, err := manager.ReconcileStoppedTrader("old-trader")
			if tc.wantError {
				if err == nil {
					t.Fatal("failed venue query was treated as an authoritative empty snapshot")
				}
			} else if err != nil || len(blockers) != 1 || blockers[0].Code != tc.blockerCode {
				t.Fatalf("risk was not preserved as the expected blocker: %+v err=%v", blockers, err)
			}
			assertLegacyRetirementHistoryUnchanged(t, st, original, 0)
			current, err := st.Trader().GetLifecycle("old-trader")
			if err != nil || current.Status != store.TraderLifecycleStopped {
				t.Fatalf("read-only failure path changed the old trader lifecycle: %+v err=%v", current, err)
			}
		})
	}
}

func TestStoppedLegacyRetirementDoesNotClearUnresolvedSubmission(t *testing.T) {
	for _, variant := range []string{"submitted outcome unknown", "per-order attempt exists"} {
		t.Run(variant, func(t *testing.T) {
			st, manager, original := legacyStoppedReconciliationFixture(t)
			if variant == "submitted outcome unknown" {
				if _, err := st.DB().Exec(`UPDATE copy_trade_execution_intents SET status='SUBMITTED',reason_code='' WHERE id=?`, original.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := st.DB().Exec(`INSERT INTO copy_trade_execution_order_attempts
					(intent_id,attempt_no,client_order_id,requested_quantity,quantized_quantity,status,submitted_at)
					VALUES(?,1,'known-submission',12,12,'UNKNOWN','2026-07-01 10:00:00')`, original.ID); err != nil {
					t.Fatal(err)
				}
			}
			original, err := st.CopyTrade().GetExecutionIntentByID(original.ID)
			if err != nil {
				t.Fatal(err)
			}
			manager.traderCache["old-trader"] = &legacyRetirementSnapshotTrader{}
			blockers, err := manager.ReconcileStoppedTrader("old-trader")
			if err == nil && len(blockers) == 0 {
				t.Fatal("a currently flat account erased a genuine unresolved submission")
			}
			assertLegacyRetirementHistoryUnchanged(t, st, original, 0)
			insertPositionSyncTrader(t, st, "new-trader", "new follower", "shared-account", false)
			start, err := st.Trader().BeginStart("user-1", "new-trader")
			if err != nil {
				t.Fatal(err)
			}
			if err = st.Trader().CompleteCopyGuardStart("user-1", "new-trader", start.Generation, "shared-account"); err == nil {
				t.Fatal("unresolved submission no longer fenced a conflicting new follower")
			}
		})
	}
}
