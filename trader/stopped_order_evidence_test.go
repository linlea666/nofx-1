package trader

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"nofx/store"
)

type stoppedEvidenceTrader struct {
	positionSyncFakeTrader
	queries        int
	fail           bool
	wrongIdentity  bool
	beforeSnapshot func()
}

func (f *stoppedEvidenceTrader) GetOrderStatusByClientID(_, client string) (map[string]interface{}, error) {
	f.queries++
	if f.fail {
		return nil, fmt.Errorf("query timeout")
	}
	id := "order-" + client
	if f.wrongIdentity {
		id = "another-order"
	}
	qty := .158
	if client == "4" {
		qty = .027
	}
	return map[string]interface{}{"status": "FILLED", "orderId": id, "executedQty": qty, "fillTime": float64(1790168949471)}, nil
}
func (f *stoppedEvidenceTrader) GetPositionsFresh() ([]map[string]interface{}, error) {
	if f.beforeSnapshot != nil {
		f.beforeSnapshot()
	}
	return f.positions, nil
}

func stoppedEvidenceIncident(t *testing.T) (*store.Store, *PositionSyncManager, int64, *stoppedEvidenceTrader) {
	t.Helper()
	st, m, old := legacyStoppedReconciliationFixture(t)
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := st.DB().Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE traders SET exchange_id='new-account' WHERE id='old-trader'`)
	exec(`INSERT INTO copy_trade_account_claims(trader_id,exchange_id,generation,exclusive) VALUES('old-trader','shared-account',0,1)`)
	exec(`UPDATE copy_trade_execution_intents SET action='reduce_short',reason_code='LEADER_EXIT_CONFIRMED',terminal_at=CURRENT_TIMESTAMP WHERE id=?`, old.ID)
	exec(`INSERT INTO copy_trade_leader_exits(intent_id,trader_id,leader_pos_id,plan_json,completed) VALUES(?,'old-trader','old-source','{}',1)`, old.ID)
	exec(`UPDATE copy_trade_position_mappings SET status='detached',last_known_size=0 WHERE trader_id='old-trader'`)
	exec(`INSERT INTO copy_trade_position_custody(trader_id,leader_pos_id,initial_intent_id,symbol,side,state) VALUES('old-trader','old-source',1,'ARXUSDT','short','MANAGED')`)
	for n := 1; n <= 4; n++ {
		exec(`INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,exchange_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,?,?,?,'SUBMITTED','FILLED',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, old.ID, n, fmt.Sprint(n), fmt.Sprintf("order-%d", n))
	}
	f := &stoppedEvidenceTrader{}
	m.queryTraderFactory = func(_, _, account string) (Trader, error) {
		if account == "shared-account" {
			return f, nil
		}
		if account == "new-account" {
			return &positionSyncFakeTrader{}, nil
		}
		return nil, fmt.Errorf("unexpected account %s", account)
	}
	return st, m, old.ID, f
}

func TestStoppedEvidenceRepairUsesOriginalAccountAndReleasesCustody(t *testing.T) {
	st, m, id, f := stoppedEvidenceIncident(t)
	config, err := st.Trader().GetByID("old-trader")
	if err != nil {
		t.Fatal(err)
	}
	config.ExchangeID = "third-account"
	if err = st.Trader().Update(config); err == nil {
		t.Fatal("saved account change with unresolved original custody")
	}
	unchanged, _ := st.Trader().GetByID("old-trader")
	if unchanged.ExchangeID != "new-account" {
		t.Fatal("failed account change partially saved")
	}
	insertPositionSyncTrader(t, st, "new-trader", "new follower", "shared-account", false)
	for n := 0; n < 2; n++ {
		b, err := m.ReconcileStoppedTrader("old-trader")
		if err != nil || len(b) > 0 {
			t.Fatalf("blockers=%v err=%v", b, err)
		}
	}
	if f.queries != 4 {
		t.Fatalf("replayed queries: %d", f.queries)
	}
	var qty float64
	var audit, booked int
	if err := st.DB().QueryRow(`SELECT filled_quantity FROM copy_trade_execution_intents WHERE id=?`, id).Scan(&qty); err != nil {
		t.Fatal(err)
	}
	if math.Abs(qty-.501) > 1e-10 {
		t.Fatalf("fill=%v", qty)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*),COUNT(booked_at) FROM copy_trade_order_evidence_repairs WHERE intent_id=?`, id).Scan(&audit, &booked); err != nil {
		t.Fatal(err)
	}
	if audit != 4 || booked != 4 {
		t.Fatalf("audits=%d booked=%d", audit, booked)
	}
	c, err := st.CopyTrade().GetPositionCustody("old-trader", "old-source")
	if err != nil || c.State != "RELEASED" {
		t.Fatalf("custody=%+v err=%v", c, err)
	}
	b, err := st.Trader().Archive("user-1", "old-trader")
	if err != nil || len(b) > 0 {
		t.Fatalf("archive=%v %v", b, err)
	}
	start, err := st.Trader().BeginStart("user-1", "new-trader")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().CompleteCopyGuardStart("user-1", "new-trader", start.Generation, "shared-account"); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedEvidenceFailureCannotReleaseResponsibility(t *testing.T) {
	for _, name := range []string{"timeout", "identity", "position", "pending", "generation"} {
		t.Run(name, func(t *testing.T) {
			st, m, _, f := stoppedEvidenceIncident(t)
			switch name {
			case "timeout":
				f.fail = true
			case "identity":
				f.wrongIdentity = true
			case "position":
				f.positions = []map[string]interface{}{{"symbol": "BTCUSDT", "positionAmt": 1.0}}
			case "pending":
				f.pendingOrders = []PendingOrderSnapshot{{ID: "live", Symbol: "BTCUSDT"}}
			case "generation":
				f.beforeSnapshot = func() {
					_, err := st.Trader().BeginStart("user-1", "old-trader")
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			b, err := m.ReconcileStoppedTrader("old-trader")
			if err == nil && len(b) == 0 {
				t.Fatal("unsafe responsibility release")
			}
			c, err := st.CopyTrade().GetPositionCustody("old-trader", "old-source")
			if err != nil || c.State != "MANAGED" {
				t.Fatalf("custody=%+v err=%v", c, err)
			}
		})
	}
}

func TestAccountQueryAdapterNeverInitializesTradingMode(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" {
			t.Errorf("query adapter performed %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":"0","data":[]}`)
	}))
	defer srv.Close()
	previous := okxBaseURL
	okxBaseURL = srv.URL
	defer func() { okxBaseURL = previous }()
	adapter, err := NewAccountQueryTrader(&store.Exchange{ID: "account", UserID: "owner", ExchangeType: "okx", APIKey: "fixture-key", SecretKey: "fixture-secret", Passphrase: "fixture-pass"}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("query construction contacted venue")
	}
	if _, err = adapter.(FreshPositionProvider).GetPositionsFresh(); err != nil {
		t.Fatal(err)
	}
	if _, err = adapter.(PendingOrderProvider).GetPendingOrdersFresh(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 3 {
		t.Fatal("did not query all account/order snapshots")
	}
}
