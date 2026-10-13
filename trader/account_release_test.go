package trader

import (
	"errors"
	"reflect"
	"testing"

	"nofx/store"
)

func TestDeleteAbandonsHistoricalFillGapsButVerifiesBothAccounts(t *testing.T) {
	st, m, intentID, oldAccount := stoppedEvidenceIncident(t)
	oldAccount.fail = true // Historical order lookup would fail if invoked.
	before, err := st.CopyTrade().GetExecutionIntentByID(intentID)
	if err != nil {
		t.Fatal(err)
	}
	var queried []string
	m.queryTraderFactory = func(_, user, account string) (Trader, error) {
		if user != "user-1" {
			t.Fatal("wrong account owner")
		}
		queried = append(queried, account)
		if account == "shared-account" {
			return oldAccount, nil
		}
		return &positionSyncFakeTrader{}, nil
	}
	blockers, err := m.ArchiveTrader("user-1", "old-trader")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("delete: %v %v", blockers, err)
	}
	if !reflect.DeepEqual(queried, []string{"new-account", "shared-account"}) || oldAccount.queries != 0 {
		t.Fatalf("wrong query scope: %v history lookups=%d", queried, oldAccount.queries)
	}
	after, err := st.CopyTrade().GetExecutionIntentByID(intentID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("historical receipt rewritten: %+v %v", after, err)
	}
	var zeroFills, repairs int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts WHERE intent_id=? AND filled_quantity=0`, intentID).Scan(&zeroFills); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_order_evidence_repairs WHERE intent_id=?`, intentID).Scan(&repairs); err != nil {
		t.Fatal(err)
	}
	if zeroFills != 4 || repairs != 0 {
		t.Fatalf("invented fills: gaps=%d repairs=%d", zeroFills, repairs)
	}
	insertPositionSyncTrader(t, st, "new", "new follower", "shared-account", false)
	start, err := st.Trader().BeginStart("user-1", "new")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().CompleteCopyGuardStart("user-1", "new", start.Generation, "shared-account"); err != nil {
		t.Fatal(err)
	}
	// Once released, a new trader's positions must not block repeated deletion.
	oldAccount.positions = []map[string]interface{}{{"symbol": "ETHUSDT", "positionAmt": 1.0}}
	if b, err := m.ArchiveTrader("user-1", "old-trader"); err != nil || len(b) > 0 {
		t.Fatalf("repeat deletion: %v %v", b, err)
	}
	if len(queried) != 2 {
		t.Fatal("idempotent deletion queried the new owner's account")
	}
}

func TestLegacyJiuchuanArchiveReleasesOnNextStart(t *testing.T) {
	st, m, _ := legacyStoppedReconciliationFixture(t)
	if _, err := st.DB().Exec(`UPDATE traders SET lifecycle_status='ARCHIVED',lifecycle_generation=12 WHERE id='old-trader'`); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{
		CycleID: 806, TraderID: "old-trader", AlgoID: "4003355246718783488", AlgoClientID: "cg806a0", Symbol: "SNDKUSDT", Side: "long", MarginMode: "cross", Quantity: 1.3, Status: "mark_crossed_exit",
	}); err != nil {
		t.Fatal(err)
	}
	insertPositionSyncTrader(t, st, "new", "new follower", "shared-account", false)
	fake := &legacyRetirementSnapshotTrader{}
	drained := 0
	m.queryTraderFactory = func(_, _, _ string) (Trader, error) {
		if drained != 1 {
			t.Fatal("snapshot read before runtime drained")
		}
		return fake, nil
	}
	drain := func(id string) error {
		if id != "old-trader" {
			t.Fatal(id)
		}
		drained++
		return nil
	}
	if err := m.ReleaseArchivedAccounts("user-1", "shared-account", drain); err != nil {
		t.Fatal(err)
	}
	start, err := st.Trader().BeginStart("user-1", "new")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().CompleteCopyGuardStart("user-1", "new", start.Generation, "shared-account"); err != nil {
		t.Fatal(err)
	}
	if err = m.ReleaseArchivedAccounts("user-1", "shared-account", drain); err != nil {
		t.Fatal(err)
	}
	if drained != 1 || fake.positionReads != 1 || fake.orderReads != 1 {
		t.Fatalf("legacy release repeated: drains=%d reads=%d/%d", drained, fake.positionReads, fake.orderReads)
	}
}

func TestSettledArchivesDoNotAddNewStartupRequirements(t *testing.T) {
	st, m, _ := legacyStoppedReconciliationFixture(t)
	if _, err := st.DB().Exec(`UPDATE traders SET lifecycle_status='ARCHIVED' WHERE id='old-trader';
 UPDATE copy_trade_execution_intents SET terminal_at=CURRENT_TIMESTAMP,exchange_state='FILLED' WHERE trader_id='old-trader'`); err != nil {
		t.Fatal(err)
	}
	m.queryTraderFactory = func(_, _, _ string) (Trader, error) {
		t.Fatal("settled archive triggered unnecessary venue checks")
		return nil, nil
	}
	if err := m.ReleaseArchivedAccounts("user-1", "shared-account", func(string) error { t.Fatal("settled archive was revisited"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestAccountDeletionFailsClosedOnCurrentRiskAndIdentityChanges(t *testing.T) {
	for _, scenario := range []string{"position", "ordinary-order", "conditional-order", "position-timeout", "order-timeout", "credentials-missing", "wrong-user", "account-change", "concurrent-start", "drain-failed"} {
		t.Run(scenario, func(t *testing.T) {
			st, m, _ := legacyStoppedReconciliationFixture(t)
			fake := &legacyRetirementSnapshotTrader{}
			m.queryTraderFactory = func(_, _, _ string) (Trader, error) { return fake, nil }
			user := "user-1"
			switch scenario {
			case "position":
				fake.positions = []map[string]interface{}{{"symbol": "SNDKUSDT"}}
			case "ordinary-order":
				fake.pendingOrders = []PendingOrderSnapshot{{ID: "order", Symbol: "SNDKUSDT", Status: "live"}}
			case "conditional-order":
				fake.pendingOrders = []PendingOrderSnapshot{{ID: "algo", Symbol: "SNDKUSDT", Status: "live", Protective: true}}
			case "position-timeout":
				fake.positionErr = errors.New("position timeout")
			case "order-timeout":
				fake.orderErr = errors.New("order timeout")
			case "credentials-missing":
				m.queryTraderFactory = nil
			case "wrong-user":
				user = "outsider"
			case "account-change", "concurrent-start":
				m.queryTraderFactory = func(_, _, _ string) (Trader, error) {
					if scenario == "account-change" {
						_, _ = st.DB().Exec(`UPDATE traders SET exchange_id='other-account' WHERE id='old-trader'`)
					} else {
						_, _ = st.Trader().BeginStart(user, "old-trader")
					}
					return fake, nil
				}
			case "drain-failed":
				_, _ = st.DB().Exec(`UPDATE traders SET lifecycle_status='ARCHIVED' WHERE id='old-trader'`)
				if err := m.ReleaseArchivedAccounts(user, "shared-account", func(string) error { return errors.New("runtime still starting") }); err == nil {
					t.Fatal("drain failure ignored")
				}
				if fake.positionReads != 0 {
					t.Fatal("queried while runtime starting")
				}
				return
			}
			b, err := m.ArchiveTrader(user, "old-trader")
			if err == nil && len(b) == 0 {
				t.Fatal("unverified account released")
			}
			var n int
			if err = st.DB().QueryRow(`SELECT COUNT(*) FROM trader_account_releases`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("partial release %d %v", n, err)
			}
		})
	}
}
