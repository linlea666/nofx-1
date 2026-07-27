package copytrade

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

type ownershipIntegrationFixture struct {
	store           *store.Store
	engine          *Engine
	integration     *TraderIntegration
	cycle           *store.CopyGuardCycle
	entryIntentID   int64
	duplicateIntent int64
	traderID        string
	leaderPosID     string
}

type failingFreshPositionsExecutor struct {
	livePositionExecutor
	err error
}

type ownershipProtectiveExecutor struct {
	*livePositionExecutor
	*mockStopMgr
}

func TestOwnershipAmbiguityNotifyKeysAreStableAndHourly(t *testing.T) {
	now := time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC)
	rate1, dedup1 := ownershipAmbiguityNotifyKeys("trader", 42, "FOLLOWER_POSITION_UNAVAILABLE", now)
	rate2, dedup2 := ownershipAmbiguityNotifyKeys("trader", 42, "FOLLOWER_POSITION_UNAVAILABLE", now.Add(30*time.Minute))
	if rate1 != rate2 || dedup1 != dedup2 {
		t.Fatalf("same reason within an hour changed notification identity: %q/%q vs %q/%q", rate1, dedup1, rate2, dedup2)
	}
	_, nextHour := ownershipAmbiguityNotifyKeys("trader", 42, "FOLLOWER_POSITION_UNAVAILABLE", now.Add(time.Hour))
	if nextHour == dedup1 {
		t.Fatalf("persistent ownership ambiguity cannot notify again in next hour: %q", nextHour)
	}
	differentRate, _ := ownershipAmbiguityNotifyKeys("trader", 42, "SOURCE_SNAPSHOT_UNAVAILABLE", now)
	if differentRate == rate1 {
		t.Fatalf("reason transition reused notification rate key: %q", differentRate)
	}
}

func (e *failingFreshPositionsExecutor) GetPositionsFresh() ([]map[string]interface{}, error) {
	return nil, e.err
}

func newOwnershipIntegrationFixture(t *testing.T, positions []map[string]interface{}, leaderPositions map[string]*Position) ownershipIntegrationFixture {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "ownership-integration.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const traderID = "ownership-integration-trader"
	const leaderPosID = "1239518824_BTCUSDT_LONG"
	const entryOrderID = "3771678372297924608"
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: traderID, LeaderPosID: leaderPosID, LeaderID: "leader",
		Symbol: "BTCUSDT", SourceSymbol: "BTCUSDT", ExecutionSymbol: "BTCUSDT",
		SourceQuoteAsset: "USDT", ExecutionSettleAsset: "USDT",
		Side: "long", MarginMode: "cross", OpenedAt: time.Now().Add(-time.Hour),
		OpenPrice: 64000, OpenSizeUSD: 205, LastKnownSize: 0.001,
	}); err != nil {
		t.Fatal(err)
	}
	if err = cs.CloseMapping(traderID, leaderPosID, 64200); err != nil {
		t.Fatal(err)
	}
	entry, claimed, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: traderID, LeaderPosID: leaderPosID, SourceRevision: 1,
		CanonicalKey: "legacy-entry", Action: "open_long", Symbol: "BTCUSDT",
		Side: "long", MarginMode: "cross", LeaderTargetSize: 0.001,
		RequestedNotional: 205, ClientOrderID: "entry-client",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve entry claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(entry.ID, store.ExecutionIntentSubmitted, "", "", entryOrderID, 0.0032, 0.0032, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(entry.ID, store.ExecutionIntentFilled, "", "", entryOrderID, 0.0032, 0.0032, 0.0032); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(entry.ID, store.ExecutionIntentProtected, "", "", entryOrderID, 0.0032, 0.0032, 0.0032); err != nil {
		t.Fatal(err)
	}
	cycle, err := cs.EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: traderID, LeaderID: "leader", LeaderPosID: leaderPosID,
		Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot: "{}", LeaderEntryPrice: 64523.9, FollowerEntryPrice: 64130.1,
		FollowerNotional: 205.21632, BaselineLeaderSize: 0.001, AccountEquity: 140,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateCopyGuardExecutionOrder(cycle.ID, entryOrderID, ""); err != nil {
		t.Fatal(err)
	}
	if err = st.Position().Create(&store.TraderPosition{
		TraderID: traderID, ExchangeID: "okx-account", ExchangeType: "okx",
		Symbol: "BTCUSDT", Side: "LONG", Quantity: 0.0032, EntryPrice: 64130.1,
		EntryOrderID: entryOrderID, EntryTime: time.Now().Add(-time.Hour), Leverage: 50,
	}); err != nil {
		t.Fatal(err)
	}
	duplicate, claimed, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: traderID, LeaderPosID: leaderPosID, SourceRevision: 3,
		CanonicalKey: "leader|ownership-integration-trader|1239518824_BTCUSDT_LONG|3",
		Action:       "open_long", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 0.003, RequestedNotional: 206, ClientOrderID: "duplicate-client",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve duplicate claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(duplicate.ID, store.ExecutionIntentSubmitted, "", "", "", 0.0032, 0.0032, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(duplicate.ID, store.ExecutionIntentReconciling, "ORDER_LOOKUP_FAILED", "failed to get order status: OKX API error: code=51603, msg=Order does not exist", "", 0.0032, 0.0032, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.LogCopyEvent(&store.CopyTradeEvent{
		TraderID: traderID, LeaderID: "leader", ProviderType: "binance",
		Category: store.CopyEventCategoryAction, EventType: store.CopyEventTypeOpen,
		Severity: store.CopyEventSeverityWarn, Symbol: "BTCUSDT", Side: "long",
		MarginMode: "cross", LeaderPosID: leaderPosID, Status: "reconciling",
		Detail: map[string]interface{}{
			"intent_id": duplicate.ID, "source_revision": 3,
			"reason_code": "POSITION_EXISTS_LOOKUP_PENDING",
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{
		traderID: traderID,
		config: &CopyConfig{
			ProviderType: ProviderBinance, LeaderID: "leader", CopyRatio: 1,
			RiskPolicyVersion: 4, RiskStopLossEnabled: true,
		},
		store: st, seenFills: make(map[string]time.Time), seenTTL: time.Hour,
		pollNowCh: make(chan struct{}, 1), stateSyncInterval: 20 * time.Second,
		leaderState:   &AccountState{TotalEquity: 1000, Positions: leaderPositions},
		lastStateSync: time.Now(), decisionCh: make(chan *decision.FullDecision, 10), stats: &EngineStats{},
		getFollowerBalance: func() float64 { return 140 },
		getFollowerEquity:  func() float64 { return 140 },
	}
	executor := &livePositionExecutor{positions: positions}
	ti := NewTraderIntegration(traderID, executor, st)
	ti.engine = engine
	return ownershipIntegrationFixture{
		store: st, engine: engine, integration: ti, cycle: cycle,
		entryIntentID: entry.ID, duplicateIntent: duplicate.ID,
		traderID: traderID, leaderPosID: leaderPosID,
	}
}

func TestOwnershipRecoveryReentersNormalRevisionedCloseChain(t *testing.T) {
	f := newOwnershipIntegrationFixture(t, []map[string]interface{}{{
		"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 0.0032, "posId": "okx-live",
	}}, map[string]*Position{})
	f.integration.reconcileMappingOwnershipGaps(true)
	mapping, err := f.store.CopyTrade().GetActiveMapping(f.traderID, f.leaderPosID)
	if err != nil || mapping == nil || mapping.SourceRevision != 3 {
		t.Fatalf("ownership mapping was not recovered: mapping=%+v err=%v", mapping, err)
	}
	fills := f.engine.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].Action != ActionClose {
		t.Fatalf("restored ownership did not expose authoritative close: %+v", fills)
	}
	signal := f.engine.buildSignal(&fills[0])
	match := f.engine.matchSignalWithMapping(signal)
	if !match.ShouldFollow || match.Action != ActionClose || match.PosID != f.leaderPosID {
		t.Fatalf("normal close matcher rejected restored mapping: %+v", match)
	}
	dec := f.engine.buildDecisionV2(signal, match, 0)
	if dec.Action != "close_long" || dec.CloseRatio != 0 || !f.engine.reserveExecutionIntent(&dec) {
		t.Fatalf("normal full-close decision was not reserved: %+v", dec)
	}
	if dec.SourceRevision != 4 || dec.ExecutionIntentID <= 0 {
		t.Fatalf("close did not use next canonical revision: %+v", dec)
	}
	intent, err := f.store.CopyTrade().GetExecutionIntentByID(dec.ExecutionIntentID)
	if err != nil || intent.Action != "close_long" || intent.SourceRevision != 4 {
		t.Fatalf("close intent is not auditable: intent=%+v err=%v", intent, err)
	}
}

func TestFollowerPositionForOwnershipGapUsesExecutionIdentityAndAggregates(t *testing.T) {
	gap := &store.CopyTradeOwnershipGap{
		Symbol: "BTCUSDT", ExecutionSymbol: "BTC-USDT-SWAP", Side: "short", MarginMode: "isolated",
	}
	got := followerPositionForOwnershipGap([]map[string]interface{}{
		{"symbol": "BTC-USDT-SWAP", "side": "short", "mgnMode": "isolated", "positionAmt": 0.2, "posId": "owned"},
		{"symbol": "BTCUSDT", "side": "SHORT", "marginMode": "isolated", "quantity": 0.3},
		{"symbol": "BTC-USDT-SWAP", "side": "long", "mgnMode": "isolated", "positionAmt": 9.0},
		{"symbol": "BTC-USDT-SWAP", "side": "short", "mgnMode": "cross", "positionAmt": 8.0},
	}, gap)
	if got.Quantity != 0.5 || got.FollowerPosID != "owned" {
		t.Fatalf("execution identity aggregation mismatch: %+v", got)
	}
}

func TestOwnershipRecoverySupportsConfiguredLeaderFamilies(t *testing.T) {
	for _, tt := range []struct {
		name       string
		provider   ProviderType
		sourceMode BinanceSourceMode
	}{
		{name: "binance_copy_management", provider: ProviderBinance, sourceMode: BinanceSourceCopyManagement},
		{name: "binance_smart_money", provider: ProviderBinance, sourceMode: BinanceSourceSmartMoney},
		{name: "okx", provider: ProviderOKX},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newOwnershipIntegrationFixture(t, []map[string]interface{}{{
				"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 0.0032,
			}}, map[string]*Position{})
			f.engine.config.ProviderType = tt.provider
			f.engine.config.BinanceSourceMode = tt.sourceMode
			f.integration.reconcileMappingOwnershipGaps(true)
			mapping, err := f.store.CopyTrade().GetActiveMapping(f.traderID, f.leaderPosID)
			if err != nil || mapping == nil || mapping.SourceRevision != 3 {
				t.Fatalf("family recovery failed: mapping=%+v err=%v", mapping, err)
			}
		})
	}
}

func TestOwnershipRecoveryWhileLeaderOpenAbsorbsMissedRiskIncrease(t *testing.T) {
	const posID = "1239518824_BTCUSDT_LONG"
	leader := &Position{PosID: posID, Symbol: "BTCUSDT", Side: SideLong, Size: 0.004, MarginMode: "cross", MarkPrice: 65000}
	f := newOwnershipIntegrationFixture(t, []map[string]interface{}{{
		"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 0.0032,
	}}, map[string]*Position{posID: leader})
	f.integration.reconcileMappingOwnershipGaps(true)
	mapping, err := f.store.CopyTrade().GetActiveMapping(f.traderID, f.leaderPosID)
	if err != nil || mapping == nil || mapping.LastKnownSize != leader.Size {
		t.Fatalf("leader target was not rebaselined without chasing: mapping=%+v err=%v", mapping, err)
	}
	if fills := f.engine.detectBinancePositionSnapshotFills(); len(fills) != 0 {
		t.Fatalf("recovery must not chase missed risk increase: %+v", fills)
	}
}

func TestOwnershipRecoveryPreservesMissedRiskReduction(t *testing.T) {
	const posID = "1239518824_BTCUSDT_LONG"
	leader := &Position{PosID: posID, Symbol: "BTCUSDT", Side: SideLong, Size: 0.002, MarginMode: "cross", MarkPrice: 65000}
	f := newOwnershipIntegrationFixture(t, []map[string]interface{}{{
		"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 0.0032,
	}}, map[string]*Position{posID: leader})
	f.integration.reconcileMappingOwnershipGaps(true)
	mapping, err := f.store.CopyTrade().GetActiveMapping(f.traderID, f.leaderPosID)
	if err != nil || mapping == nil || mapping.LastKnownSize != 0.003 || mapping.SourceRevision != 3 {
		t.Fatalf("missed source reduction was swallowed by recovery: mapping=%+v err=%v", mapping, err)
	}
	fills := f.engine.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].Action != ActionReduce {
		t.Fatalf("missed source reduction was not exposed to normal diff chain: %+v", fills)
	}
	signal := f.engine.buildSignal(&fills[0])
	match := f.engine.matchSignalWithMapping(signal)
	if !match.ShouldFollow || match.Action != ActionReduce {
		t.Fatalf("normal reduce matcher rejected recovered reduction: %+v", match)
	}
	dec := f.engine.buildDecisionV2(signal, match, 0)
	if dec.Action != "reduce_long" || !f.engine.reserveExecutionIntent(&dec) || dec.SourceRevision != 4 {
		t.Fatalf("recovered reduction did not reserve next canonical revision: %+v", dec)
	}
}

func TestOwnershipRecoveryPreservesLeaderReversalClose(t *testing.T) {
	const posID = "1239518824_BTCUSDT_LONG"
	leader := &Position{PosID: posID, Symbol: "BTCUSDT", Side: SideShort, Size: 0.002, MarginMode: "cross", MarkPrice: 65000}
	f := newOwnershipIntegrationFixture(t, []map[string]interface{}{{
		"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 0.0032,
	}}, map[string]*Position{posID: leader})
	f.integration.reconcileMappingOwnershipGaps(true)
	mapping, err := f.store.CopyTrade().GetActiveMapping(f.traderID, f.leaderPosID)
	if err != nil || mapping == nil || mapping.LastKnownSize != 0.003 {
		t.Fatalf("leader reversal was absorbed by ownership recovery: mapping=%+v err=%v", mapping, err)
	}
	fills := f.engine.detectBinancePositionSnapshotFills()
	var closeFill *Fill
	for i := range fills {
		if fills[i].Action == ActionClose {
			closeFill = &fills[i]
			break
		}
	}
	if closeFill == nil {
		t.Fatalf("leader reversal did not expose old-side close: %+v", fills)
	}
	if !sourceFillMarksLeaderReversal(closeFill.ID) {
		t.Fatalf("leader reversal source identity lost durable attribution: %s", closeFill.ID)
	}
	match := f.engine.matchSignalWithMapping(f.engine.buildSignal(closeFill))
	if !match.ShouldFollow || match.Action != ActionClose || match.PosID != f.leaderPosID {
		t.Fatalf("recovered reversal close was rejected: %+v", match)
	}
	dec := f.engine.buildDecisionV2(f.engine.buildSignal(closeFill), match, 0)
	if !dec.LeaderReversed || dec.Action != "close_long" {
		t.Fatalf("reversal attribution was lost before ordinary close execution: %+v", dec)
	}
	if !f.engine.reserveExecutionIntent(&dec) {
		t.Fatalf("reversal close intent was not reserved: %+v", dec)
	}
	intent, err := f.store.CopyTrade().GetExecutionIntentByID(dec.ExecutionIntentID)
	if err != nil || !sourceFillMarksLeaderReversal(intent.SourceFillID) {
		t.Fatalf("reversal attribution was not persisted for restart reconciliation: intent=%+v err=%v", intent, err)
	}
}

func TestOwnershipRecoveryFailsClosedWithoutFreshSourceOrFollowerState(t *testing.T) {
	f := newOwnershipIntegrationFixture(t, []map[string]interface{}{{
		"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 0.0032,
	}}, map[string]*Position{})
	f.engine.lastStateSync = time.Now().Add(-2 * time.Minute)
	f.integration.reconcileMappingOwnershipGaps(true)
	mapping, err := f.store.CopyTrade().GetMappingForReconciliation(f.traderID, f.leaderPosID)
	if err != nil || mapping.Status != store.MappingStatusClosed {
		t.Fatalf("stale source recovered ownership: mapping=%+v err=%v", mapping, err)
	}
	cycle, err := f.store.CopyTrade().GetCopyGuardCycle(f.cycle.ID)
	if err != nil || cycle.AccountingStatus != "UNSCORABLE" {
		t.Fatalf("stale source gap was not marked unscorable: cycle=%+v err=%v", cycle, err)
	}

	f.engine.lastStateSync = time.Now()
	f.integration.executor = &failingFreshPositionsExecutor{err: errors.New("fresh position lookup unavailable")}
	f.integration.lastOwnershipReconcile = time.Time{}
	f.integration.reconcileMappingOwnershipGaps(true)
	mapping, err = f.store.CopyTrade().GetMappingForReconciliation(f.traderID, f.leaderPosID)
	if err != nil || mapping.Status != store.MappingStatusClosed {
		t.Fatalf("unknown follower state recovered ownership: mapping=%+v err=%v", mapping, err)
	}
}

func TestOwnershipGapAlreadyFlatClosesLeaderLifecycleWithoutOrder(t *testing.T) {
	f := newOwnershipIntegrationFixture(t, nil, map[string]*Position{})
	if _, err := f.store.DB().Exec(`UPDATE trader_positions SET status='CLOSED' WHERE trader_id=? AND entry_order_id<>''`, f.traderID); err != nil {
		t.Fatal(err)
	}
	manual, err := f.store.CopyTrade().SaveManualReentrySignal(&store.CopyGuardManualReentrySignal{
		CycleID: f.cycle.ID, TraderID: f.traderID, LeaderPosID: f.leaderPosID,
		Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", TriggerPrice: 64000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ReentryAI().EnsureReentryCandidate(&store.CopyGuardReentryCandidate{
		CycleID: f.cycle.ID, TraderID: f.traderID, LeaderPosID: f.leaderPosID,
		Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: store.ReentryCandidateWatching,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.integration.reconcileMappingOwnershipGaps(true)
	cycle, err := f.store.CopyTrade().GetCopyGuardCycle(f.cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Status != store.CopyGuardLeaderClosed || cycle.ClosedAt == nil {
		t.Fatalf("flat leader-closed lifecycle was not finalized: %+v", cycle)
	}
	var closeIntents int
	if err = f.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents WHERE trader_id=? AND leader_pos_id=? AND action='close_long'`, f.traderID, f.leaderPosID).Scan(&closeIntents); err != nil {
		t.Fatal(err)
	}
	if closeIntents != 0 {
		t.Fatalf("flat ownership gap created a redundant close order intent: %d", closeIntents)
	}
	duplicate, err := f.store.CopyTrade().GetExecutionIntentByID(f.duplicateIntent)
	if err != nil || duplicate.Status != store.ExecutionIntentSkipped || duplicate.ReasonCode != store.OwnershipRecoveredReason {
		t.Fatalf("flat ownership gap did not terminalize duplicate open intent: intent=%+v err=%v", duplicate, err)
	}
	manual, err = f.store.CopyTrade().GetManualReentrySignal(manual.ID)
	if err != nil || manual.Status != store.ManualReentryStatusInvalidated {
		t.Fatalf("leader close did not invalidate manual reentry: signal=%+v err=%v", manual, err)
	}
	candidate, err := f.store.ReentryAI().GetReentryCandidateByCycle(f.cycle.ID)
	if err != nil || candidate.Status != store.ReentryCandidateInvalidated || candidate.ClosedAt == nil {
		t.Fatalf("leader close did not invalidate AI reentry: candidate=%+v err=%v", candidate, err)
	}
}

func TestOwnershipGapFollowerFlatLeaderOpenDetachesAndBlocksReopen(t *testing.T) {
	const posID = "1239518824_BTCUSDT_LONG"
	leader := &Position{PosID: posID, Symbol: "BTCUSDT", Side: SideLong, Size: 0.003, MarginMode: "cross"}
	f := newOwnershipIntegrationFixture(t, nil, map[string]*Position{posID: leader})
	f.integration.reconcileMappingOwnershipGaps(true)
	mapping, err := f.store.CopyTrade().GetMappingForReconciliation(f.traderID, f.leaderPosID)
	if err != nil || mapping.Status != store.MappingStatusDetached || mapping.SourceRevision != 3 || mapping.LastKnownSize != leader.Size {
		t.Fatalf("flat follower with live leader was not detached: mapping=%+v err=%v", mapping, err)
	}
	cycle, err := f.store.CopyTrade().GetCopyGuardCycle(f.cycle.ID)
	if err != nil || cycle.Status != store.CopyGuardDetached || cycle.ClosedAt == nil || cycle.AccountingStatus != store.CopyGuardAccountingUnscorable {
		t.Fatalf("detached flat lifecycle state mismatch: cycle=%+v err=%v", cycle, err)
	}
	duplicate, err := f.store.CopyTrade().GetExecutionIntentByID(f.duplicateIntent)
	if err != nil || duplicate.Status != store.ExecutionIntentSkipped || duplicate.ReasonCode != store.OwnershipRecoveredReason {
		t.Fatalf("detached ownership gap did not terminalize duplicate open: intent=%+v err=%v", duplicate, err)
	}
	if fills := f.engine.detectBinancePositionSnapshotFills(); len(fills) != 0 {
		t.Fatalf("detached lifecycle must not recreate follower exposure: %+v", fills)
	}
	leader.Size = 0.004
	if fills := f.engine.detectBinancePositionSnapshotFills(); len(fills) != 0 {
		t.Fatalf("detached lifecycle must block later source adds: %+v", fills)
	}
}

func TestOwnershipGapFollowerFlatLeaderReversalFinalizesAsReversed(t *testing.T) {
	const posID = "1239518824_BTCUSDT_LONG"
	leader := &Position{PosID: posID, Symbol: "BTCUSDT", Side: SideShort, Size: 0.002, MarginMode: "cross", MarkPrice: 65000}
	f := newOwnershipIntegrationFixture(t, nil, map[string]*Position{posID: leader})
	f.integration.reconcileMappingOwnershipGaps(true)
	cycle, err := f.store.CopyTrade().GetCopyGuardCycle(f.cycle.ID)
	if err != nil || cycle.Status != store.CopyGuardLeaderReversed || cycle.ClosedAt == nil {
		t.Fatalf("flat follower reversal lifecycle attribution mismatch: cycle=%+v err=%v", cycle, err)
	}
	var closeIntents int
	if err = f.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents WHERE trader_id=? AND leader_pos_id=? AND action='close_long'`, f.traderID, f.leaderPosID).Scan(&closeIntents); err != nil {
		t.Fatal(err)
	}
	if closeIntents != 0 {
		t.Fatalf("flat follower reversal created redundant close intent: %d", closeIntents)
	}
	var reversalEvents int
	if err = f.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='LEADER_REVERSED'`, f.cycle.ID).Scan(&reversalEvents); err != nil {
		t.Fatal(err)
	}
	if reversalEvents != 1 {
		t.Fatalf("flat follower reversal event missing: %d", reversalEvents)
	}
}

func TestDetachedOwnershipLifecycleClosesOnLeaderReversal(t *testing.T) {
	const posID = "1239518824_BTCUSDT_LONG"
	leader := &Position{PosID: posID, Symbol: "BTCUSDT", Side: SideLong, Size: 0.003, MarginMode: "cross"}
	f := newOwnershipIntegrationFixture(t, nil, map[string]*Position{posID: leader})
	f.integration.reconcileMappingOwnershipGaps(true)
	f.engine.leaderState.Positions[posID] = &Position{PosID: posID, Symbol: "BTCUSDT", Side: SideShort, Size: 0.002, MarginMode: "cross"}
	f.engine.checkIgnoredPositionsClosed()
	mapping, err := f.store.CopyTrade().GetMappingForReconciliation(f.traderID, f.leaderPosID)
	if err != nil || mapping.Status != store.MappingStatusClosed {
		t.Fatalf("leader reversal did not end detached lifecycle: mapping=%+v err=%v", mapping, err)
	}
	fills := f.engine.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].Action != ActionOpen || fills[0].PositionSide != SideShort {
		t.Fatalf("new reversed lifecycle was not exposed after detached close: %+v", fills)
	}
}

func TestOwnershipGapFlatKeepsCycleOpenUntilProtectionCancelIsConfirmed(t *testing.T) {
	const posID = "1239518824_BTCUSDT_LONG"
	leader := &Position{PosID: posID, Symbol: "BTCUSDT", Side: SideLong, Size: 0.003, MarginMode: "cross"}
	f := newOwnershipIntegrationFixture(t, nil, map[string]*Position{posID: leader})
	order := &store.CopyGuardProtectiveOrder{
		CycleID: f.cycle.ID, TraderID: f.traderID, AlgoID: "live-stop", Symbol: "BTCUSDT",
		Side: "long", MarginMode: "cross", Quantity: 0.0032, TriggerPrice: 63000, Status: "live",
	}
	if err := f.store.CopyTrade().UpsertCopyGuardProtectiveOrder(order); err != nil {
		t.Fatal(err)
	}
	f.integration.executor = &ownershipProtectiveExecutor{
		livePositionExecutor: &livePositionExecutor{},
		mockStopMgr:          &mockStopMgr{cancelErr: errors.New("exchange cancel state unavailable")},
	}
	f.integration.reconcileMappingOwnershipGaps(true)
	cycle, err := f.store.CopyTrade().GetCopyGuardCycle(f.cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cycle.ClosedAt != nil || cycle.AccountingStatus != "UNSCORABLE" {
		t.Fatalf("uncertain protective cancel prematurely closed lifecycle: %+v", cycle)
	}
	mapping, err := f.store.CopyTrade().GetMappingForReconciliation(f.traderID, f.leaderPosID)
	if err != nil || mapping.Status != store.MappingStatusClosed {
		t.Fatalf("uncertain protective cancel recovered or mutated mapping: mapping=%+v err=%v", mapping, err)
	}
}
