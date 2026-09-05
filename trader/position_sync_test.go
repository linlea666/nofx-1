package trader

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

type positionSyncFakeTrader struct {
	Trader
	positions     []map[string]interface{}
	pendingOrders []PendingOrderSnapshot
	trades        []TradeRecord
	tradeErr      error
	marketPrice   float64
}

func (f *positionSyncFakeTrader) GetPositions() ([]map[string]interface{}, error) {
	return f.positions, nil
}

func (f *positionSyncFakeTrader) GetPositionsFresh() ([]map[string]interface{}, error) {
	return f.positions, nil
}

func (f *positionSyncFakeTrader) GetPendingOrdersFresh() ([]PendingOrderSnapshot, error) {
	return f.pendingOrders, nil
}

func (f *positionSyncFakeTrader) GetTradesForSymbol(string, time.Time, int) ([]TradeRecord, error) {
	return f.trades, f.tradeErr
}

func (f *positionSyncFakeTrader) GetMarketPrice(string) (float64, error) {
	return f.marketPrice, nil
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

func TestStoppedReconciliationUsesImmutableFillsAndAuditsResidual(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "stopped-reconcile.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertPositionSyncTrader(t, st, "stopped-trader", "stopped", "exchange-1", false)
	openedAt := time.Now().Add(-time.Hour)
	position := &store.TraderPosition{
		TraderID: "stopped-trader", ExchangeID: "exchange-1", ExchangeType: "okx",
		Symbol: "BTCUSDT", Side: "LONG", Quantity: 0.1, EntryPrice: 60000,
		EntryTime: openedAt, Leverage: 2,
	}
	if err = st.Position().Create(position); err != nil {
		t.Fatal(err)
	}
	fake := &positionSyncFakeTrader{
		marketPrice: 61000,
		trades: []TradeRecord{{
			TradeID: "fill-1", OrderID: "order-1", Symbol: "BTCUSDT",
			Side: "SELL", PositionSide: "LONG", Price: 60500, Quantity: 0.06,
			RealizedPnL: 30, Fee: 0.5, Time: openedAt.Add(30 * time.Minute),
		}},
	}
	manager := NewPositionSyncManager(st, time.Second)
	manager.traderCache["stopped-trader"] = fake
	blockers, err := manager.ReconcileStoppedTrader("stopped-trader")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("stopped reconciliation failed: blockers=%+v err=%v", blockers, err)
	}
	open, err := st.Position().GetOpenPositions("stopped-trader")
	if err != nil || len(open) != 0 {
		t.Fatalf("local residual remained open: %+v err=%v", open, err)
	}
	var fillCount, allocationCount, auditCount int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM position_close_fills WHERE exchange_trade_id='fill-1'`).Scan(&fillCount); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM position_close_allocations WHERE exchange_trade_id='fill-1'`).Scan(&allocationCount); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM position_accounting_audits WHERE reason_code='AUTHORITATIVE_EXCHANGE_FLAT'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if fillCount != 1 || allocationCount != 1 || auditCount != 1 {
		t.Fatalf("immutable allocation/audit mismatch: fills=%d allocations=%d audits=%d", fillCount, allocationCount, auditCount)
	}
	var unscorable int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM trader_positions
		WHERE trader_id='stopped-trader' AND status='CLOSED' AND accounting_quality='UNSCORABLE'`).Scan(&unscorable); err != nil {
		t.Fatal(err)
	}
	if unscorable != 1 {
		t.Fatalf("residual was not isolated as UNSCORABLE: %d", unscorable)
	}
}

func TestStoppedReconciliationDoesNotMutateWhileExchangeRiskExists(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "stopped-reconcile-blocked.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertPositionSyncTrader(t, st, "stopped-trader", "stopped", "exchange-1", false)
	position := &store.TraderPosition{
		TraderID: "stopped-trader", ExchangeID: "exchange-1", ExchangeType: "okx",
		Symbol: "ETHUSDT", Side: "LONG", Quantity: 1, EntryPrice: 2000,
		EntryTime: time.Now().Add(-time.Hour), Leverage: 2,
	}
	if err = st.Position().Create(position); err != nil {
		t.Fatal(err)
	}
	fake := &positionSyncFakeTrader{
		positions: []map[string]interface{}{{"symbol": "ETHUSDT", "side": "long", "positionAmt": 1.0}},
		pendingOrders: []PendingOrderSnapshot{{
			ID: "algo-1", Symbol: "ETHUSDT", Status: "live", Protective: true,
		}},
	}
	manager := NewPositionSyncManager(st, time.Second)
	manager.traderCache["stopped-trader"] = fake
	blockers, err := manager.ReconcileStoppedTrader("stopped-trader")
	if err != nil || len(blockers) != 2 {
		t.Fatalf("expected position and order blockers: %+v err=%v", blockers, err)
	}
	open, err := st.Position().GetOpenPositions("stopped-trader")
	if err != nil || len(open) != 1 {
		t.Fatalf("blocked reconciliation mutated local position: %+v err=%v", open, err)
	}
}

func TestStoppedReconciliationRetiresFlatCopyGuardLifecycleIdempotently(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "stopped-copyguard-retirement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertPositionSyncTrader(t, st, "stopped-trader", "stopped", "exchange-1", false)
	if err = st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: "stopped-trader", LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "SNDKUSDT", Side: "short", MarginMode: "cross",
		Status: store.MappingStatusStoppedByRisk, LastKnownSize: 10,
	}); err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "stopped-trader", LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "SNDKUSDT", Side: "short", MarginMode: "cross",
		Status: store.CopyGuardAIAbandoned, ProtectionStatus: store.CopyGuardProtectionTriggered,
		PolicySnapshot:     `{"version":4}`,
		FollowerEntryPrice: 1, FollowerNotional: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 1, 10, 10, .1); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_guard_attempts SET status='STOPPED',closed_at=CURRENT_TIMESTAMP WHERE cycle_id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{
		CycleID: cycle.ID, TraderID: "stopped-trader", AlgoID: "old-algo", AlgoClientID: "old-client",
		Symbol: "SNDKUSDT", Side: "short", MarginMode: "cross", Quantity: 10,
		TriggerPrice: 1.2, TriggerType: "mark", Status: "live",
	}); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "stopped-trader", LeaderPosID: "leader-pos", SourceRevision: 2,
		CanonicalKey: "leader|stopped-trader|leader-pos|2", Action: "open_short",
		Symbol: "SNDKUSDT", Side: "short", LeaderTargetSize: 10,
		RequestedNotional: 10, RequestedQuantity: 10, QuantizedQuantity: 10,
		ClientOrderID: "stopped-local-prepared",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve local prepared intent: claimed=%v err=%v", claimed, err)
	}
	if _, err = st.CopyTrade().PrepareExecutionOrderAttemptRecordWithKind(
		intent.ID, intent.ClientOrderID, "INITIAL_OPEN", 10, 10,
	); err != nil {
		t.Fatal(err)
	}
	manager := NewPositionSyncManager(st, time.Second)
	manager.traderCache["stopped-trader"] = &positionSyncFakeTrader{}
	for run := 0; run < 2; run++ {
		blockers, reconcileErr := manager.ReconcileStoppedTrader("stopped-trader")
		if reconcileErr != nil || len(blockers) != 0 {
			t.Fatalf("run %d blockers=%+v err=%v", run, blockers, reconcileErr)
		}
	}
	blockers, err := st.Trader().GetArchiveBlockers("stopped-trader")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("flat stopped trader still blocked: %+v err=%v", blockers, err)
	}
	cycle, err = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil || cycle.ClosedAt == nil || cycle.Status != store.CopyGuardDetached ||
		cycle.AccountingStatus != store.CopyGuardAccountingUnscorable || cycle.ProtectionStatus != store.CopyGuardProtectionCanceled {
		t.Fatalf("cycle was not safely retired: %+v err=%v", cycle, err)
	}
	mapping, err := st.CopyTrade().GetMapping("stopped-trader", "leader-pos")
	if err != nil || mapping.Status != store.MappingStatusDetached {
		t.Fatalf("mapping retirement mismatch: %+v err=%v", mapping, err)
	}
	protective, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil || protective.Status != "canceled" || protective.ReplacementPending {
		t.Fatalf("local protective order was not retired: %+v err=%v", protective, err)
	}
	var events int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type=?`, cycle.ID, store.StoppedTraderFlatRetirementReason).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("retirement event count=%d want=1", events)
	}
	intent, err = st.CopyTrade().GetExecutionIntentByID(intent.ID)
	if err != nil || intent.Status != store.ExecutionIntentFailed || intent.ReasonCode != store.ExecutionIntentCycleTerminatedBeforeSubmit || intent.TerminalAt == nil {
		t.Fatalf("local PREPARED intent was not safely retired: %+v err=%v", intent, err)
	}
	attempts, err := st.CopyTrade().ListExecutionOrderAttempts(intent.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != store.ExecutionOrderAttemptTerminalNoFill || attempts[0].SubmittedAt != nil {
		t.Fatalf("local PREPARED attempt retirement mismatch: %+v err=%v", attempts, err)
	}
}

func TestStoppedReconciliationDoesNotTreatFillQueryFailureAsNoFills(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "stopped-reconcile-history-error.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertPositionSyncTrader(t, st, "stopped-trader", "stopped", "exchange-1", false)
	position := &store.TraderPosition{
		TraderID: "stopped-trader", ExchangeID: "exchange-1", ExchangeType: "okx",
		Symbol: "BTCUSDT", Side: "LONG", Quantity: 0.1, EntryPrice: 60000,
		EntryTime: time.Now().Add(-time.Hour), Leverage: 2,
	}
	if err = st.Position().Create(position); err != nil {
		t.Fatal(err)
	}
	manager := NewPositionSyncManager(st, time.Second)
	manager.traderCache["stopped-trader"] = &positionSyncFakeTrader{
		tradeErr: errors.New("rate limited"), marketPrice: 61000,
	}
	if _, err = manager.ReconcileStoppedTrader("stopped-trader"); err == nil {
		t.Fatal("fill-history failure was treated as an authoritative empty result")
	}
	open, queryErr := st.Position().GetOpenPositions("stopped-trader")
	if queryErr != nil || len(open) != 1 {
		t.Fatalf("fill-history failure mutated local position: %+v err=%v", open, queryErr)
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
