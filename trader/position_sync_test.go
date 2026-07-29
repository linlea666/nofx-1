package trader

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

type positionSyncFakeTrader struct {
	Trader
	positions []map[string]interface{}
}

func (f *positionSyncFakeTrader) GetPositions() ([]map[string]interface{}, error) {
	return f.positions, nil
}

func insertPositionSyncTrader(t *testing.T, st *store.Store, id, name, exchangeID string, running bool) {
	t.Helper()
	status := store.TraderLifecycleStopped
	generation := int64(0)
	if running {
		status = store.TraderLifecycleRunning
		generation = 1
	}
	if _, err := st.DB().Exec(`INSERT INTO traders
		(id,user_id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		id, "user-1", name, "model-1", exchangeID, 100, running, status, generation); err != nil {
		t.Fatal(err)
	}
}

func TestPositionShortfallRequiresStableVisibilityGrace(t *testing.T) {
	manager := NewPositionSyncManager(nil, time.Second)
	oldest := time.Now().Add(-time.Minute)
	if manager.positionShortfallConfirmed("trader-1", "PROSUSDT_SHORT", 0, 121, oldest) {
		t.Fatal("first missing snapshot must not close a position")
	}
	key := "trader-1|PROSUSDT_SHORT"
	manager.shortfallMutex.Lock()
	observation := manager.shortfallObserved[key]
	observation.observedAt = time.Now().Add(-31 * time.Second)
	manager.shortfallObserved[key] = observation
	manager.shortfallMutex.Unlock()
	if !manager.positionShortfallConfirmed("trader-1", "PROSUSDT_SHORT", 0, 121, oldest) {
		t.Fatal("stable shortfall beyond the grace window was not confirmed")
	}
	if manager.positionShortfallConfirmed("trader-1", "PROSUSDT_SHORT", 0, 201, oldest) {
		t.Fatal("changed local quantity must restart shortfall confirmation")
	}
}

func TestExternalPositionSyncDoesNotLetStoppedSharedTraderClaim(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "position-sync-owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertPositionSyncTrader(t, st, "stopped-trader", "old stopped", "exchange-1", false)
	insertPositionSyncTrader(t, st, "active-trader", "active owner", "exchange-1", true)
	manager := NewPositionSyncManager(st, time.Second)
	fake := &positionSyncFakeTrader{positions: []map[string]interface{}{{
		"symbol": "PROSUSDT", "side": "short", "positionAmt": 121.0,
		"entryPrice": 0.4451, "leverage": 20.0,
		"createdTime": float64(time.Now().UnixMilli()),
	}}}

	manager.syncExternalPositions("stopped-trader", "exchange-1", "okx", fake)
	var count int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM trader_positions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stopped trader claimed shared account position: %d", count)
	}

	manager.syncExternalPositions("active-trader", "exchange-1", "okx", fake)
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM trader_positions WHERE trader_id='active-trader' AND source='sync'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("sole running trader did not claim position: %d", count)
	}
	manager.syncExternalPositions("active-trader", "exchange-1", "okx", fake)
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM trader_positions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("idempotent sync created duplicate rows: %d", count)
	}
}

func TestPeriodicAccountingSyncSkipsStoppedTrader(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "stopped-accounting-sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertPositionSyncTrader(t, st, "stopped-trader", "stopped", "exchange-1", false)
	manager := NewPositionSyncManager(st, time.Second)
	manager.syncDueClosedAccounting()
	manager.lastHistorySyncMutex.RLock()
	_, synced := manager.lastHistorySync["stopped-trader"]
	manager.lastHistorySyncMutex.RUnlock()
	if synced {
		t.Fatal("stopped trader continued periodic exchange accounting reconciliation")
	}
}

func TestExternalPositionSyncFailsClosedWithMultipleRunningTraders(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "position-sync-ambiguous.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertPositionSyncTrader(t, st, "active-1", "active one", "exchange-1", true)
	insertPositionSyncTrader(t, st, "active-2", "active two", "exchange-1", true)
	manager := NewPositionSyncManager(st, time.Second)
	fake := &positionSyncFakeTrader{positions: []map[string]interface{}{{
		"symbol": "PROSUSDT", "side": "short", "positionAmt": 121.0,
		"entryPrice": 0.4451, "createdTime": float64(time.Now().UnixMilli()),
	}}}
	manager.syncExternalPositions("active-1", "exchange-1", "okx", fake)
	var count int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM trader_positions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("ambiguous shared account position was auto-claimed: %d", count)
	}
}

func TestExternalPositionSyncUsesExclusiveMappingEvidenceOnSharedAccount(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "position-sync-evidence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertPositionSyncTrader(t, st, "active-1", "mapped owner", "exchange-1", true)
	insertPositionSyncTrader(t, st, "active-2", "other trader", "exchange-1", true)
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_position_mappings
		(trader_id,leader_pos_id,leader_id,symbol,side,margin_mode,status)
		VALUES('active-1','leader-pos-1','leader-1','PROSUSDT','short','cross','active')`); err != nil {
		t.Fatal(err)
	}
	manager := NewPositionSyncManager(st, time.Second)
	fake := &positionSyncFakeTrader{positions: []map[string]interface{}{{
		"symbol": "PROSUSDT", "side": "short", "positionAmt": 121.0,
		"entryPrice": 0.4451, "createdTime": float64(time.Now().UnixMilli()),
	}}}
	manager.syncExternalPositions("active-2", "exchange-1", "okx", fake)
	manager.syncExternalPositions("active-1", "exchange-1", "okx", fake)
	var owner string
	if err = st.DB().QueryRow(`SELECT trader_id FROM trader_positions`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "active-1" {
		t.Fatalf("exclusive mapping evidence assigned position to %q", owner)
	}
}

func TestIsClosingTradeForPositionUsesDirectionNotPnL(t *testing.T) {
	tests := []struct {
		name         string
		trade        TradeRecord
		positionSide string
		want         bool
	}{
		{"hedge long close at break even", TradeRecord{Side: "SELL", PositionSide: "LONG", RealizedPnL: 0}, "long", true},
		{"hedge long add", TradeRecord{Side: "BUY", PositionSide: "LONG", RealizedPnL: 12}, "long", false},
		{"hedge short close", TradeRecord{Side: "BUY", PositionSide: "SHORT"}, "short", true},
		{"hedge short add", TradeRecord{Side: "SELL", PositionSide: "SHORT"}, "short", false},
		{"one way long close", TradeRecord{Side: "SELL", PositionSide: "BOTH"}, "long", true},
		{"one way short close", TradeRecord{Side: "BUY"}, "short", true},
		{"unknown position side fails closed", TradeRecord{Side: "SELL", PositionSide: "SIDEWAYS"}, "long", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClosingTradeForPosition(tt.trade, tt.positionSide); got != tt.want {
				t.Fatalf("isClosingTradeForPosition()=%v want=%v", got, tt.want)
			}
		})
	}
}
