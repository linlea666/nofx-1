package copytrade

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader"
)

type legacyCustodyHistoryExecutor struct {
	*stopMgrExecutor
	fills      []trader.TradeRecord
	historyErr error
}

func (e *legacyCustodyHistoryExecutor) GetTradesForSymbol(string, time.Time, int) ([]trader.TradeRecord, error) {
	return e.fills, e.historyErr
}
func (e *legacyCustodyHistoryExecutor) GetTradesForPosition(symbol, mode string, _ time.Time) ([]trader.TradeRecord, error) {
	if symbol != "ETHUSDT" || mode != "cross" {
		return nil, errors.New("wrong position scope")
	}
	return e.fills, e.historyErr
}

func TestLegacyCustodyBackfillRequiresInitialFillAndUnbrokenScope(t *testing.T) {
	for _, scenario := range []string{"continuous", "duplicate_page", "initial_catchup", "manual_reopen", "missing_initial_fill", "partial_initial_fill", "wrong_scope", "history_error", "quantity_mismatch", "conflicting_identity", "same_millisecond_ambiguous"} {
		t.Run(scenario, func(t *testing.T) {
			st, err := store.New(filepath.Join(t.TempDir(), "legacy-custody.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ex := &legacyCustodyHistoryExecutor{stopMgrExecutor: &stopMgrExecutor{mockStopMgr: &mockStopMgr{}, positions: []map[string]interface{}{
				{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "entryPrice": 100.0, "positionAmt": 1.3, "posId": "reused"},
			}}}
			ti := NewTraderIntegration("trader-1", ex, st)
			ti.engine = &Engine{config: &CopyConfig{ProviderType: ProviderOKX, LeaderID: "leader", RiskPolicyVersion: 4, RiskStopLossEnabled: true}}
			policy := store.NewCopyGuardDefaults()
			policy.TraderID, policy.ProviderType, policy.LeaderID = "trader-1", string(ProviderOKX), "leader"
			ti.policySnapshot, err = store.EncodeCopyGuardPolicySnapshot(policy)
			if err != nil {
				t.Fatal(err)
			}
			mapping := &store.CopyTradePositionMapping{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "legacy", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", OpenedAt: time.Now().Add(-time.Hour), OpenPrice: 100, LastKnownSize: 10, SourceRevision: 1}
			if err = st.CopyTrade().SavePositionMapping(mapping); err != nil {
				t.Fatal(err)
			}
			i, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "trader-1", LeaderPosID: "legacy", SourceRevision: 1, SourceKind: "LEADER_TRANSITION", CanonicalKey: "leader|legacy|1", Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", ClientOrderID: "first-client", LeaderTargetSize: 10})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET status='FILLED',filled_quantity=1,filled_notional=100,exchange_order_id='initial-order',submitted_at=CURRENT_TIMESTAMP WHERE id=?`, i.ID); err != nil {
				t.Fatal(err)
			}
			at := i.CreatedAt
			ex.fills = []trader.TradeRecord{
				{TradeID: "1", OrderID: "initial-order", Symbol: "ETHUSDT", PositionSide: "LONG", Side: "BUY", MarginMode: "cross", Quantity: 1, Price: 100, Time: at},
				{TradeID: "2", OrderID: "manual-add", Symbol: "ETHUSDT", PositionSide: "LONG", Side: "BUY", MarginMode: "cross", Quantity: .3, Price: 100, Time: at.Add(time.Second)},
			}
			switch scenario {
			case "duplicate_page":
				ex.fills = append(ex.fills, ex.fills[0])
			case "initial_catchup":
				ex.fills[0].Quantity = .9
				fill := ex.fills[0]
				fill.TradeID, fill.OrderID, fill.Quantity, fill.Time = "catchup", "catchup-order", .1, at.Add(500*time.Millisecond)
				ex.fills = append(ex.fills, fill)
				if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET exchange_order_id='catchup-order' WHERE id=?`, i.ID); err != nil {
					t.Fatal(err)
				}
				for n, order := range []struct {
					id       string
					quantity float64
				}{{"initial-order", .9}, {"catchup-order", .1}} {
					if _, err = st.DB().Exec(`INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,exchange_order_id,status,exchange_state,filled_quantity,submitted_at,terminal_at) VALUES(?,?,?,?,'FILLED','FILLED',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, i.ID, n+1, order.id, order.id, order.quantity); err != nil {
						t.Fatal(err)
					}
				}
			case "manual_reopen":
				close := ex.fills[0]
				close.TradeID, close.OrderID, close.Side, close.Time = "close", "manual-close", "SELL", at.Add(time.Millisecond)
				ex.fills = append(ex.fills, close)
				ex.positions[0]["positionAmt"] = .3
			case "missing_initial_fill":
				ex.fills = ex.fills[1:]
			case "partial_initial_fill":
				ex.fills[0].Quantity = .5
				ex.positions[0]["positionAmt"] = .8
			case "wrong_scope":
				ex.fills[0].MarginMode = "isolated"
			case "history_error":
				ex.historyErr = errors.New("incomplete pagination")
			case "quantity_mismatch":
				ex.positions[0]["positionAmt"] = 2.0
			case "conflicting_identity":
				dup := ex.fills[0]
				dup.Quantity = 2
				ex.fills = append(ex.fills, dup)
			case "same_millisecond_ambiguous":
				ex.fills[1].Time, ex.fills[1].Side = at, "SELL"
				ex.positions[0]["positionAmt"] = .7
			}
			cycle, created, err := ti.ensureV4CycleForMapping(mapping, nil, "legacy recovery")
			allowed := scenario == "continuous" || scenario == "duplicate_page" || scenario == "initial_catchup"
			if allowed {
				if err != nil || !created || cycle == nil || cycle.InitialIntentID != i.ID || cycle.EntryOrderID != "initial-order" {
					t.Fatalf("continuous confirmed entry must recover: %+v %v %v", cycle, created, err)
				}
				again, recreated, retryErr := ti.ensureV4CycleForMapping(mapping, nil, "retry")
				if retryErr != nil || recreated || again.ID != cycle.ID {
					t.Fatalf("recovery not idempotent: %+v %v %v", again, recreated, retryErr)
				}
			} else {
				if err == nil || created || cycle != nil {
					t.Fatalf("uncertain/new manual scope was adopted: %+v %v %v", cycle, created, err)
				}
				if scenario == "manual_reopen" && !errors.Is(err, errCopyPositionEnded) {
					t.Fatalf("flat boundary not recognized: %v", err)
				}
				if scenario == "partial_initial_fill" && !strings.Contains(err.Error(), "incomplete") {
					t.Fatalf("initial quantity mismatch not identified: %v", err)
				}
				var count int
				if e := st.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_cycles`).Scan(&count); e != nil || count != 0 {
					t.Fatalf("failed proof created a lifecycle: %d %v", count, e)
				}
			}
		})
	}
}
