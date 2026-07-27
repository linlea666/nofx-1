package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type ownershipRecoveryFixture struct {
	store           *Store
	cycle           *CopyGuardCycle
	entryIntent     *CopyTradeExecutionIntent
	duplicateIntent *CopyTradeExecutionIntent
	traderID        string
	leaderPosID     string
	entryOrderID    string
}

func newOwnershipRecoveryFixture(t *testing.T, withPositionConflictEvent bool) ownershipRecoveryFixture {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "ownership-gap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const traderID = "ownership-trader"
	const leaderPosID = "1239518824_BTCUSDT_LONG"
	const entryOrderID = "3771678372297924608"
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: traderID, LeaderPosID: leaderPosID, LeaderID: "leader",
		Symbol: "BTCUSDT", SourceSymbol: "BTCUSDT", ExecutionSymbol: "BTCUSDT",
		SourceQuoteAsset: "USDT", ExecutionSettleAsset: "USDT",
		Side: "long", MarginMode: "cross", OpenedAt: time.Now().Add(-7 * 24 * time.Hour),
		OpenPrice: 64000, OpenSizeUSD: 200, LastKnownSize: 0.001,
	}); err != nil {
		t.Fatal(err)
	}
	if err = cs.CloseMapping(traderID, leaderPosID, 64200); err != nil {
		t.Fatal(err)
	}
	entryIntent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: traderID, LeaderPosID: leaderPosID, SourceRevision: 1,
		CanonicalKey: "legacy-entry|1", Action: "open_long", Symbol: "BTCUSDT",
		Side: "long", MarginMode: "cross", LeaderTargetSize: 0.001,
		RequestedNotional: 205.59, ClientOrderID: "entry-client",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve entry claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(entryIntent.ID, ExecutionIntentSubmitted, "", "", entryOrderID, 0.0032, 0.0032, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(entryIntent.ID, ExecutionIntentFilled, "", "", entryOrderID, 0.0032, 0.0032, 0.0032); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(entryIntent.ID, ExecutionIntentProtected, "", "", entryOrderID, 0.0032, 0.0032, 0.0032); err != nil {
		t.Fatal(err)
	}
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: traderID, LeaderID: "leader", LeaderPosID: leaderPosID,
		Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing,
		PolicySnapshot: "{}", LeaderEntryPrice: 64523.9, FollowerEntryPrice: 64130.1,
		FollowerNotional: 205.21632, BaselineLeaderSize: 0.001, AccountEquity: 140,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateCopyGuardExecutionOrder(cycle.ID, entryOrderID, ""); err != nil {
		t.Fatal(err)
	}
	if err = st.Position().Create(&TraderPosition{
		TraderID: traderID, ExchangeID: "okx-account", ExchangeType: "okx",
		Symbol: "BTCUSDT", Side: "LONG", Quantity: 0.0032, EntryPrice: 64130.1,
		EntryOrderID: entryOrderID, EntryTime: time.Now().Add(-3 * 24 * time.Hour), Leverage: 50,
	}); err != nil {
		t.Fatal(err)
	}
	duplicateIntent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: traderID, LeaderPosID: leaderPosID, SourceRevision: 3,
		CanonicalKey: "leader|ownership-trader|1239518824_BTCUSDT_LONG|3",
		Action:       "open_long", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 0.003, RequestedNotional: 206, ClientOrderID: "duplicate-client",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve duplicate claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(duplicateIntent.ID, ExecutionIntentSubmitted, "", "", "", 0.0032, 0.0032, 0); err != nil {
		t.Fatal(err)
	}
	lookupErr := "failed to get order status: OKX API error: code=51603, msg=Order does not exist"
	if err = cs.UpdateExecutionIntent(duplicateIntent.ID, ExecutionIntentReconciling, "ORDER_LOOKUP_FAILED", lookupErr, "", 0.0032, 0.0032, 0); err != nil {
		t.Fatal(err)
	}
	if withPositionConflictEvent {
		if err = cs.LogCopyEvent(&CopyTradeEvent{
			TraderID: traderID, LeaderID: "leader", ProviderType: "binance",
			Category: CopyEventCategoryAction, EventType: CopyEventTypeOpen,
			Severity: CopyEventSeverityWarn, Symbol: "BTCUSDT", Side: "long",
			MarginMode: "cross", LeaderPosID: leaderPosID, Status: "reconciling",
			Detail: map[string]interface{}{
				"intent_id": duplicateIntent.ID, "source_revision": 3,
				"reason_code": "POSITION_EXISTS_LOOKUP_PENDING",
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return ownershipRecoveryFixture{
		store: st, cycle: cycle, entryIntent: entryIntent, duplicateIntent: duplicateIntent,
		traderID: traderID, leaderPosID: leaderPosID, entryOrderID: entryOrderID,
	}
}

func TestRestoreConfirmedMappingOwnershipRepairsLegacyRevisionGap(t *testing.T) {
	f := newOwnershipRecoveryFixture(t, true)
	cs := f.store.CopyTrade()
	gaps, err := cs.ListOpenCycleOwnershipGaps(f.traderID)
	if err != nil || len(gaps) != 1 {
		t.Fatalf("gaps=%+v err=%v", gaps, err)
	}
	gap := gaps[0]
	if !gap.Recoverable || gap.ReasonCode != OwnershipGapStrongEvidence || gap.EntryIntentID != f.entryIntent.ID {
		t.Fatalf("strong ownership evidence was not classified: %+v", gap)
	}
	result, err := cs.RestoreConfirmedMappingOwnership(RestoreMappingOwnershipRequest{
		TraderID: f.traderID, LeaderPosID: f.leaderPosID, CycleID: f.cycle.ID,
		EntryIntentID: f.entryIntent.ID, FollowerQuantity: 0.0032, FollowerPosID: "okx-live-pos",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MappingRevision != 3 || len(result.SupersededIntentIDs) != 1 || result.SupersededIntentIDs[0] != f.duplicateIntent.ID || !result.PendingRiskReduction {
		t.Fatalf("unexpected recovery result: %+v", result)
	}
	mapping, err := cs.GetActiveMapping(f.traderID, f.leaderPosID)
	if err != nil || mapping == nil {
		t.Fatalf("mapping not active: mapping=%+v err=%v", mapping, err)
	}
	if mapping.SourceRevision != 3 || mapping.LastKnownSize != 0.003 || mapping.ClosedAt != nil || mapping.ExecutionSymbol != "BTCUSDT" {
		t.Fatalf("mapping recovery lost lifecycle identity: %+v", mapping)
	}
	stored, err := cs.GetExecutionIntentByID(f.duplicateIntent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != ExecutionIntentSkipped || stored.ReasonCode != OwnershipRecoveredReason || !strings.Contains(stored.LastError, "51603") {
		t.Fatalf("duplicate intent was not auditably superseded: %+v", stored)
	}
	var supersededEvents, recoveredEvents int
	if err = f.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type=?`, f.cycle.ID, OwnershipRecoveredReason).Scan(&supersededEvents); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='MAPPING_OWNERSHIP_RECOVERED'`, f.cycle.ID).Scan(&recoveredEvents); err != nil {
		t.Fatal(err)
	}
	if supersededEvents != 1 || recoveredEvents != 1 {
		t.Fatalf("typed recovery audit events missing: superseded=%d recovered=%d", supersededEvents, recoveredEvents)
	}
	next, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: f.traderID, LeaderPosID: f.leaderPosID, SourceRevision: 4,
		CanonicalKey: "leader|ownership-trader|1239518824_BTCUSDT_LONG|4",
		Action:       "close_long", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross",
	})
	if err != nil || !claimed || next.SourceRevision != 4 {
		t.Fatalf("next close revision was blocked: intent=%+v claimed=%v err=%v", next, claimed, err)
	}
	if gaps, err = cs.ListOpenCycleOwnershipGaps(f.traderID); err != nil || len(gaps) != 0 {
		t.Fatalf("recovery was not idempotently removed from gap set: gaps=%+v err=%v", gaps, err)
	}
}

func TestRestoreConfirmedMappingOwnershipRejectsUncertainSubmittedIntent(t *testing.T) {
	f := newOwnershipRecoveryFixture(t, false)
	_, err := f.store.CopyTrade().RestoreConfirmedMappingOwnership(RestoreMappingOwnershipRequest{
		TraderID: f.traderID, LeaderPosID: f.leaderPosID, CycleID: f.cycle.ID,
		EntryIntentID: f.entryIntent.ID, FollowerQuantity: 0.0032,
	})
	if err == nil || !strings.Contains(err.Error(), OwnershipGapUnsafeRevision) {
		t.Fatalf("uncertain submitted intent was not rejected: %v", err)
	}
	mapping, getErr := f.store.CopyTrade().GetMappingForReconciliation(f.traderID, f.leaderPosID)
	if getErr != nil || mapping.Status != MappingStatusClosed || mapping.SourceRevision != 2 {
		t.Fatalf("failed recovery mutated mapping: mapping=%+v err=%v", mapping, getErr)
	}
	intent, getErr := f.store.CopyTrade().GetExecutionIntentByID(f.duplicateIntent.ID)
	if getErr != nil || intent.Status != ExecutionIntentReconciling {
		t.Fatalf("failed recovery mutated intent: intent=%+v err=%v", intent, getErr)
	}
}

func TestRestoreConfirmedMappingOwnershipNeverReplaysDurableAttempt(t *testing.T) {
	f := newOwnershipRecoveryFixture(t, true)
	if _, err := f.store.DB().Exec(`INSERT INTO copy_trade_execution_order_attempts(
		intent_id,attempt_no,client_order_id,quantity_kind,requested_quantity,quantized_quantity,status
	) VALUES(?,?,?,?,?,?,?)`, f.duplicateIntent.ID, 1, "durable-duplicate", "INITIAL_OPEN", 0.0032, 0.0032, "PREPARED"); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.CopyTrade().RestoreConfirmedMappingOwnership(RestoreMappingOwnershipRequest{
		TraderID: f.traderID, LeaderPosID: f.leaderPosID, CycleID: f.cycle.ID,
		EntryIntentID: f.entryIntent.ID, FollowerQuantity: 0.0032,
	})
	if err == nil || !strings.Contains(err.Error(), OwnershipGapUnsafeRevision) {
		t.Fatalf("durably prepared exchange attempt was replayed or superseded: %v", err)
	}
	intent, getErr := f.store.CopyTrade().GetExecutionIntentByID(f.duplicateIntent.ID)
	if getErr != nil || intent.Status != ExecutionIntentReconciling {
		t.Fatalf("rejected durable attempt was mutated: intent=%+v err=%v", intent, getErr)
	}
}

func TestOwnershipGapWithoutConfirmedEntryIntentStaysAmbiguous(t *testing.T) {
	f := newOwnershipRecoveryFixture(t, true)
	if _, err := f.store.DB().Exec(`UPDATE copy_trade_execution_intents SET exchange_order_id='different-order' WHERE id=?`, f.entryIntent.ID); err != nil {
		t.Fatal(err)
	}
	gaps, err := f.store.CopyTrade().ListOpenCycleOwnershipGaps(f.traderID)
	if err != nil || len(gaps) != 1 {
		t.Fatalf("gaps=%+v err=%v", gaps, err)
	}
	if gaps[0].Recoverable || gaps[0].ReasonCode != OwnershipGapEntryIntentMissing {
		t.Fatalf("manual/ambiguous ownership was adopted: %+v", gaps[0])
	}
}

func TestRestoreConfirmedMappingOwnershipSupportsShortIsolatedLifecycle(t *testing.T) {
	f := newOwnershipRecoveryFixture(t, true)
	for _, stmt := range []string{
		`UPDATE copy_trade_position_mappings SET side='short',margin_mode='isolated'`,
		`UPDATE copy_guard_cycles SET side='short',margin_mode='isolated'`,
		`UPDATE copy_trade_execution_intents SET action='open_short',side='short',margin_mode='isolated'`,
		`UPDATE trader_positions SET side='SHORT'`,
	} {
		if _, err := f.store.DB().Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	gaps, err := f.store.CopyTrade().ListOpenCycleOwnershipGaps(f.traderID)
	if err != nil || len(gaps) != 1 || !gaps[0].Recoverable {
		t.Fatalf("short isolated ownership was not recognized: gaps=%+v err=%v", gaps, err)
	}
	result, err := f.store.CopyTrade().RestoreConfirmedMappingOwnership(RestoreMappingOwnershipRequest{
		TraderID: f.traderID, LeaderPosID: f.leaderPosID, CycleID: f.cycle.ID,
		EntryIntentID: f.entryIntent.ID, FollowerQuantity: 0.0032,
	})
	if err != nil || result.MappingRevision != 3 {
		t.Fatalf("short isolated recovery failed: result=%+v err=%v", result, err)
	}
	mapping, err := f.store.CopyTrade().GetActiveMapping(f.traderID, f.leaderPosID)
	if err != nil || mapping.Side != "short" || mapping.MarginMode != "isolated" {
		t.Fatalf("short isolated identity changed: mapping=%+v err=%v", mapping, err)
	}
}

func TestOwnershipAmbiguityDeduplicatesDynamicDetailByStableReason(t *testing.T) {
	f := newOwnershipRecoveryFixture(t, true)
	cs := f.store.CopyTrade()
	changed, err := cs.MarkCopyGuardOwnershipAmbiguous(f.cycle.ID, f.traderID, "FOLLOWER_POSITION_UNAVAILABLE", "request_id=one")
	if err != nil || !changed {
		t.Fatalf("first ambiguity reason was not persisted: changed=%v err=%v", changed, err)
	}
	changed, err = cs.MarkCopyGuardOwnershipAmbiguous(f.cycle.ID, f.traderID, "FOLLOWER_POSITION_UNAVAILABLE", "request_id=two")
	if err != nil || changed {
		t.Fatalf("dynamic detail created a duplicate state transition: changed=%v err=%v", changed, err)
	}
	cycle, err := cs.GetCopyGuardCycle(f.cycle.ID)
	if err != nil || !strings.Contains(cycle.AccountingError, "request_id=two") {
		t.Fatalf("latest ambiguity detail was not retained: cycle=%+v err=%v", cycle, err)
	}
	changed, err = cs.MarkCopyGuardOwnershipAmbiguous(f.cycle.ID, f.traderID, "SOURCE_SNAPSHOT_UNAVAILABLE", "")
	if err != nil || !changed {
		t.Fatalf("reason-code transition was not recorded: changed=%v err=%v", changed, err)
	}
	var events int
	if err = f.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='OWNERSHIP_AMBIGUOUS'`, f.cycle.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("stable ambiguity reasons should produce two transitions, got %d", events)
	}
}

func TestFlatOwnershipResolutionIsIdempotentBeforeLeaderAccounting(t *testing.T) {
	f := newOwnershipRecoveryFixture(t, true)
	req := ResolveFlatOwnershipGapRequest{
		TraderID: f.traderID, LeaderPosID: f.leaderPosID, CycleID: f.cycle.ID,
		EntryIntentID: f.entryIntent.ID, LeaderStillOpen: false,
		ReasonCode: OwnershipGapFollowerAlreadyFlat,
	}
	first, err := f.store.CopyTrade().ResolveConfirmedOwnershipGapFlat(req)
	if err != nil || first.MappingRevision != 3 || len(first.SupersededIntentIDs) != 1 {
		t.Fatalf("first flat resolution failed: result=%+v err=%v", first, err)
	}
	second, err := f.store.CopyTrade().ResolveConfirmedOwnershipGapFlat(req)
	if err != nil || second.MappingRevision != 3 || len(second.SupersededIntentIDs) != 0 {
		t.Fatalf("flat resolution was not restart-idempotent: result=%+v err=%v", second, err)
	}
	var flatEvents, supersededEvents int
	if err = f.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='OWNERSHIP_GAP_FLAT_RETIRED'`, f.cycle.ID).Scan(&flatEvents); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type=?`, f.cycle.ID, OwnershipRecoveredReason).Scan(&supersededEvents); err != nil {
		t.Fatal(err)
	}
	if flatEvents != 1 || supersededEvents != 1 {
		t.Fatalf("idempotent flat resolution duplicated audit events: flat=%d superseded=%d", flatEvents, supersededEvents)
	}
}
