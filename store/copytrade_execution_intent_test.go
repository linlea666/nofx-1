package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecutionIntentReservationIsCanonicalAndKeepsClientID(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "intent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, SourceFillID: "f1", Action: "open_short", Symbol: "BTCUSDT", ClientOrderID: "stable-id"}
	first, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed || first.ClientOrderID != "stable-id" {
		t.Fatalf("first=%+v claimed=%v err=%v", first, claimed, err)
	}
	duplicate, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, Action: "open_short", ClientOrderID: "different-id"})
	if err != nil || claimed || duplicate.ID != first.ID {
		t.Fatalf("duplicate=%+v claimed=%v err=%v", duplicate, claimed, err)
	}
	if err := cs.UpdateExecutionIntent(first.ID, ExecutionIntentFailed, "PRE_SUBMIT", "safe retry", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	reclaimed, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, SourceFillID: "f2", Action: "open_short", Symbol: "BTCUSDT", ClientOrderID: "new-id"})
	if err != nil || !claimed {
		t.Fatalf("reclaimed=%+v claimed=%v err=%v", reclaimed, claimed, err)
	}
	if reclaimed.ClientOrderID != "stable-id" {
		t.Fatalf("client id changed across retry: %q", reclaimed.ClientOrderID)
	}
	if err := cs.UpdateExecutionIntent(reclaimed.ID, ExecutionIntentFailed, "REAL_FAILURE", "exchange rejected", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	counted, err := cs.MarkExecutionIntentFailureCounted(reclaimed.ID)
	if err != nil || !counted {
		t.Fatalf("first failure count claim=%v err=%v", counted, err)
	}
	counted, err = cs.MarkExecutionIntentFailureCounted(reclaimed.ID)
	if err != nil || counted {
		t.Fatalf("duplicate failure count claim=%v err=%v", counted, err)
	}
}

func TestRiskSkippedAddReclaimsOnlyForNewSourceFill(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "risk-skip-reclaim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 2, SourceFillID: "size-2",
		Action: "open_long", Symbol: "ETHUSDT", LeaderTargetSize: 2, ClientOrderID: "stable",
	}
	first, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed {
		t.Fatalf("first reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(first.ID, ExecutionIntentSkipped, "RISK_CAP", "no capacity", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err = cs.ReserveExecutionIntent(base); err != nil || claimed {
		t.Fatalf("unchanged source fill retried: claimed=%v err=%v", claimed, err)
	}
	next := *base
	next.SourceFillID = "size-3"
	next.LeaderTargetSize = 3
	reclaimed, claimed, err := cs.ReserveExecutionIntent(&next)
	if err != nil || !claimed || reclaimed.ID != first.ID || reclaimed.LeaderTargetSize != 3 {
		t.Fatalf("new cumulative source transition was not reclaimed: intent=%+v claimed=%v err=%v", reclaimed, claimed, err)
	}
}

func TestPrecheckInfrastructureFailureReplaysSameSourceTransition(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "precheck-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		SourceFillID: "snapshot-1", Action: "open_long", Symbol: "ETHUSDT",
		LeaderTargetSize: 1, ClientOrderID: "stable",
	}
	intent, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed {
		t.Fatalf("reserve: claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentFailed, "PRECHECK_PRICE_UNAVAILABLE", "temporary price failure", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	replayed, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed || replayed.ID != intent.ID || replayed.ClientOrderID != "stable" {
		t.Fatalf("same authoritative transition must retry after precheck recovery: intent=%+v claimed=%v err=%v", replayed, claimed, err)
	}
}

func TestExecutionIntentAdditiveMigrationIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-intent.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE copy_trade_execution_intents (
		id INTEGER PRIMARY KEY AUTOINCREMENT,trader_id TEXT NOT NULL,leader_pos_id TEXT NOT NULL,source_revision INTEGER NOT NULL,
		source_fill_id TEXT DEFAULT '',action TEXT NOT NULL,symbol TEXT DEFAULT '',side TEXT DEFAULT '',margin_mode TEXT DEFAULT '',
		leader_target_size REAL DEFAULT 0,requested_notional REAL DEFAULT 0,requested_quantity REAL DEFAULT 0,quantized_quantity REAL DEFAULT 0,
		filled_quantity REAL DEFAULT 0,client_order_id TEXT DEFAULT '',exchange_order_id TEXT DEFAULT '',status TEXT NOT NULL DEFAULT 'RESERVED',
		reason_code TEXT DEFAULT '',last_error TEXT DEFAULT '',failure_counted BOOLEAN NOT NULL DEFAULT 0,created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,UNIQUE(trader_id,leader_pos_id,source_revision,action))`)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for pass := 0; pass < 2; pass++ {
		st, openErr := New(dbPath)
		if openErr != nil {
			t.Fatalf("migration pass %d: %v", pass, openErr)
		}
		if pass == 0 {
			intent, claimed, reserveErr := st.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, SourceKind: "AI_REENTRY", CanonicalKey: "ai|t1|1|1", CycleID: 1, CandidateID: 2, AnalysisID: 3, AttemptNo: 1, DecisionGeneration: 4, Action: "open_long"})
			if reserveErr != nil || !claimed || intent.SourceKind != "AI_REENTRY" || intent.CandidateID != 2 {
				t.Fatalf("migrated columns unusable: intent=%+v claimed=%v err=%v", intent, claimed, reserveErr)
			}
			if updateErr := st.CopyTrade().UpdateExecutionIntentQuantityConstraints(intent.ID, 0.006, 0.01, 0.001, 0.001, 10, 0.01); updateErr != nil {
				t.Fatalf("migrated quantity constraint columns unusable: %v", updateErr)
			}
		} else {
			intents, getErr := st.CopyTrade().ListExecutionIntentsByCycle(1)
			if getErr != nil || len(intents) != 1 {
				t.Fatalf("load migrated intent: count=%d err=%v", len(intents), getErr)
			}
			intent := intents[0]
			if intent.RequestedQuantity != 0.006 || intent.QuantizedQuantity != 0.01 ||
				intent.QuantityStep != 0.001 || intent.ExchangeMinQuantity != 0.001 ||
				intent.ExchangeMinNotional != 10 || intent.MinimumExecutableQuantity != 0.01 {
				t.Fatalf("quantity constraints were not preserved across restart: %+v", intent)
			}
		}
		if closeErr := st.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
}

func TestSubmittedExecutionIntentCannotBeReclaimed(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "submitted-intent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, CanonicalKey: "leader|t1|p1|1", Action: "open_long", Symbol: "ETHUSDT", ClientOrderID: "stable"}
	first, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(first.ID, ExecutionIntentSubmitted, "", "", "order-1", 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(first.ID, ExecutionIntentFailed, "REAL_FAILURE", "rejected", "order-1", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || claimed || got.ID != first.ID || got.Status != ExecutionIntentFailed {
		t.Fatalf("submitted failure was unsafely reclaimed: got=%+v claimed=%v err=%v", got, claimed, err)
	}
}

func TestTerminalUnknownAttemptNormalizesOnStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terminal-unknown.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	cs := st.CopyTrade()
	cases := []struct {
		exchangeState  string
		filledQuantity float64
		wantStatus     string
	}{
		{exchangeState: "REJECTED", wantStatus: ExecutionOrderAttemptTerminalNoFill},
		{exchangeState: "FAILED", wantStatus: ExecutionOrderAttemptTerminalNoFill},
		{exchangeState: "FILLED", wantStatus: ExecutionOrderAttemptUnknown},
		{exchangeState: "FILLED", filledQuantity: 0.4, wantStatus: ExecutionOrderAttemptFilled},
		{exchangeState: "CANCELED", filledQuantity: 0.4, wantStatus: ExecutionOrderAttemptPartiallyFilled},
		{exchangeState: "EXPIRED", filledQuantity: 0.4, wantStatus: ExecutionOrderAttemptPartiallyFilled},
	}
	intentIDs := make([]int64, 0, len(cases))
	for index, testCase := range cases {
		clientOrderID := fmt.Sprintf("client-%d", index+1)
		intent, claimed, reserveErr := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
			TraderID: "t1", LeaderPosID: fmt.Sprintf("p%d", index+1), SourceRevision: 1,
			CanonicalKey: fmt.Sprintf("leader|t1|p%d|1", index+1), Action: "open_long", ClientOrderID: clientOrderID,
		})
		if reserveErr != nil || !claimed {
			t.Fatalf("case %d reserve claimed=%v err=%v", index, claimed, reserveErr)
		}
		if _, err = cs.PrepareExecutionOrderAttemptWithQuantities(intent.ID, clientOrderID, 1, 1); err != nil {
			t.Fatal(err)
		}
		if err = cs.CompleteExecutionOrderAttempt(intent.ID, clientOrderID, ExecutionOrderAttemptUnknown, "", testCase.exchangeState, "legacy ambiguous status", testCase.filledQuantity); err != nil {
			t.Fatal(err)
		}
		intentIDs = append(intentIDs, intent.ID)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for index, testCase := range cases {
		attempts, listErr := st.CopyTrade().ListExecutionOrderAttempts(intentIDs[index])
		if listErr != nil || len(attempts) != 1 || attempts[0].Status != testCase.wantStatus {
			t.Fatalf("case %d normalization failed: want=%s attempts=%+v err=%v", index, testCase.wantStatus, attempts, listErr)
		}
	}
}

func TestCommitLeaderExecutionFillAtomicallyAdvancesMappingAndIntent(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "atomic-fill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, CanonicalKey: "leader|t1|p1|1", Action: "open_short", Symbol: "BTCUSDT", Side: "short", MarginMode: "cross", LeaderTargetSize: 3, ClientOrderID: "stable"})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentSubmitted, "", "", "order-1", 0.37, 0.37, 0); err != nil {
		t.Fatal(err)
	}
	commit := LeaderExecutionCommit{IntentID: intent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1", SourceRevision: 1, Action: "open_short", Symbol: "BTCUSDT", SourceSymbol: "BTCUSDT", ExecutionSymbol: "BTCUSDT", Side: "short", MarginMode: "cross", LeaderTargetSize: 3, FillPrice: 66125.9, FilledQuantity: 0.37, FilledNotional: 244.66583, ExchangeOrderID: "order-1", ExchangeState: "FILLED"}
	if err = cs.CommitLeaderExecutionFill(commit); err != nil {
		t.Fatal(err)
	}
	mapping, err := cs.GetMapping("t1", "p1")
	if err != nil || mapping == nil || mapping.SourceRevision != 1 || mapping.Status != MappingStatusActive || mapping.LastKnownSize != 3 {
		t.Fatalf("mapping was not atomically committed: mapping=%+v err=%v", mapping, err)
	}
	stored, _, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, CanonicalKey: "leader|t1|p1|1", Action: "open_short"})
	if err != nil || stored.Status != ExecutionIntentFilled || stored.FilledQuantity != 0.37 || stored.ExchangeOrderID != "order-1" {
		t.Fatalf("intent terminal state mismatch: intent=%+v err=%v", stored, err)
	}
	if err = cs.CommitLeaderExecutionFill(commit); err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	mapping, err = cs.GetMapping("t1", "p1")
	if err != nil || mapping.OpenSizeUSD != commit.FilledNotional {
		t.Fatalf("idempotent replay duplicated mapping notional: mapping=%+v err=%v", mapping, err)
	}
}

func TestCommitLeaderExecutionFillAtomicallyCreatesCopyGuardLifecycle(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "atomic-copyguard-fill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	policy := NewCopyGuardDefaults()
	policy.TraderID = "t1"
	policy.ProviderType = "okx"
	policy.RiskProtectionMode = RiskProtectionModePositionMarginPct
	policy.RiskPositionMarginStopPct = .80
	policy.RiskReentryEnabled = false
	policy.RiskReentryDecisionMode = "disabled"
	policy.RiskManualReentryEnabled = false
	policy.RiskMaxReentries = 0
	policySnapshot, err := EncodeCopyGuardPolicySnapshot(policy)
	if err != nil {
		t.Fatal(err)
	}

	reserveInitial := func(traderID, leaderPosID, clientID string) *CopyTradeExecutionIntent {
		t.Helper()
		intent, claimed, reserveErr := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
			TraderID: traderID, LeaderPosID: leaderPosID,
			SourceRevision: 1, SourceKind: "LEADER_TRANSITION",
			CanonicalKey: "leader|" + traderID + "|" + leaderPosID + "|1",
			Action:       "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
			LeaderTargetSize: 1, RequestedQuantity: 1, QuantizedQuantity: 1, ClientOrderID: clientID,
		})
		if reserveErr != nil || !claimed {
			t.Fatalf("reserve %s/%s: claimed=%v err=%v", traderID, leaderPosID, claimed, reserveErr)
		}
		if updateErr := cs.UpdateExecutionIntent(intent.ID, ExecutionIntentSubmitted, "", "", clientID+"-venue", 1, 1, 0); updateErr != nil {
			t.Fatal(updateErr)
		}
		return intent
	}
	commitFor := func(intent *CopyTradeExecutionIntent, traderID, leaderPosID, clientID, snapshot string) LeaderExecutionCommit {
		return LeaderExecutionCommit{
			IntentID: intent.ID, TraderID: traderID, LeaderID: "leader", LeaderPosID: leaderPosID,
			SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", SourceSymbol: "ETHUSDT",
			ExecutionSymbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 1,
			FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: clientID,
			ExchangeOrderID: clientID + "-venue", ExchangeState: "FILLED", OrderTerminal: true,
			InitialCopyGuard: &InitialCopyGuardLifecycle{
				PolicySnapshot: snapshot, LeaderEntryPrice: 101, AccountEquity: 1000, ATRAtEntry: 2,
			},
		}
	}

	intent := reserveInitial("t1", "p1", "initial")
	commit := commitFor(intent, "t1", "p1", "initial", policySnapshot)
	if err = cs.CommitLeaderExecutionFill(commit); err != nil {
		t.Fatal(err)
	}
	cycle, err := cs.GetOpenCopyGuardCycle("t1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if cycle.InitialIntentID != intent.ID || cycle.EntryOrderID != "initial-venue" {
		t.Fatalf("cycle did not retain initial fill identity: %+v", cycle)
	}
	attempts, err := cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 1 || attempts[0].AttemptNo != 0 || attempts[0].Status != "OPEN" {
		t.Fatalf("attempt zero was not atomically created: attempts=%+v err=%v", attempts, err)
	}
	storedIntent, err := cs.GetExecutionIntentByID(intent.ID)
	if err != nil || storedIntent.CycleID != cycle.ID || storedIntent.Status != ExecutionIntentFilled {
		t.Fatalf("intent was not bound to initial cycle: intent=%+v err=%v", storedIntent, err)
	}
	if err = cs.CommitLeaderExecutionFill(commit); err != nil {
		t.Fatalf("idempotent initial fill replay failed: %v", err)
	}
	var cycles, attemptCount, initialEvents int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_cycles WHERE trader_id='t1' AND leader_pos_id='p1'`).Scan(&cycles); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_attempts WHERE cycle_id=?`, cycle.ID).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='INITIAL_ENTRY_FILLED'`, cycle.ID).Scan(&initialEvents); err != nil {
		t.Fatal(err)
	}
	if cycles != 1 || attemptCount != 1 || initialEvents != 1 {
		t.Fatalf("initial lifecycle replay duplicated rows: cycles=%d attempts=%d events=%d", cycles, attemptCount, initialEvents)
	}

	// Later leader fills bind to the same lifecycle inside the mapping commit.
	// This signed ledger is the only trustworthy baseline for detecting manual
	// or second-program quantity changes on the execution account.
	addIntent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 2, SourceKind: "LEADER_TRANSITION",
		CanonicalKey: "leader|t1|p1|2", Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 1.5, RequestedQuantity: .5, QuantizedQuantity: .5, ClientOrderID: "add",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve add: claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(addIntent.ID, ExecutionIntentSubmitted, "", "", "add-venue", .5, .5, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.CommitLeaderExecutionFill(LeaderExecutionCommit{
		IntentID: addIntent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		SourceRevision: 2, Action: "open_long", IsAdd: true, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 1.5, FillPrice: 102, FilledQuantity: .5, FilledNotional: 51,
		ClientOrderID: "add", ExchangeOrderID: "add-venue", ExchangeState: "FILLED", OrderTerminal: true,
	}); err != nil {
		t.Fatal(err)
	}
	reduceIntent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 3, SourceKind: "LEADER_TRANSITION",
		CanonicalKey: "leader|t1|p1|3", Action: "reduce_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 1.3, RequestedQuantity: .2, QuantizedQuantity: .2, ClientOrderID: "reduce",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve reduce: claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(reduceIntent.ID, ExecutionIntentSubmitted, "", "", "reduce-venue", .2, .2, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.CommitLeaderExecutionFill(LeaderExecutionCommit{
		IntentID: reduceIntent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		SourceRevision: 3, Action: "reduce_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 1.3, FillPrice: 103, FilledQuantity: .2, FilledNotional: 20.6,
		ClientOrderID: "reduce", ExchangeOrderID: "reduce-venue", ExchangeState: "FILLED", OrderTerminal: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{addIntent.ID, reduceIntent.ID} {
		bound, getErr := cs.GetExecutionIntentByID(id)
		if getErr != nil || bound.CycleID != cycle.ID {
			t.Fatalf("later intent %d was not atomically cycle-bound: %+v err=%v", id, bound, getErr)
		}
	}
	expected, err := cs.GetCopyGuardPositionOwnershipExpectation(cycle.ID)
	if err != nil || !expected.Reliable || expected.InFlight || math.Abs(expected.ExpectedQuantity-1.3) > 1e-12 || len(expected.ContributingIntents) != 3 {
		t.Fatalf("signed ownership ledger is wrong: %+v err=%v", expected, err)
	}
	if _, _, err = cs.InitializeCopyGuardPositionMarginShadowV2(&CopyGuardPositionMarginShadowV2{
		CycleID: cycle.ID, TraderID: cycle.TraderID, Side: "long",
		AnchorEntryPrice: 100, AnchorLeverage: 10, AnchorInitialMargin: 10,
		AnchorStopPrice: 92, ConfiguredMarginLossPct: .8,
		PriceTickSize: .1, QuantityStep: .001, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1.3, CurrentLeverage: 10,
		EffectiveStopPrice: 92, LastLeaderSize: 1.3,
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := cs.MarkCopyGuardPositionOwnershipAnomaly(cycle.ID, "t1", expected.ExpectedQuantity, 1.7, .001, expected.ContributingIntents)
	if err != nil || !changed {
		t.Fatalf("position ownership anomaly was not recorded: changed=%v err=%v", changed, err)
	}
	if changed, err = cs.MarkCopyGuardPositionOwnershipAnomaly(cycle.ID, "t1", expected.ExpectedQuantity, 1.8, .001, expected.ContributingIntents); err != nil || changed {
		t.Fatalf("same anomaly reason was not idempotent: changed=%v err=%v", changed, err)
	}
	cycle, err = cs.GetCopyGuardCycle(cycle.ID)
	if err != nil || cycle.AccountingStatus != CopyGuardAccountingUnscorable || !strings.HasPrefix(cycle.AccountingError, "POSITION_OWNERSHIP_ANOMALY:") {
		t.Fatalf("ownership anomaly remained scoreable: cycle=%+v err=%v", cycle, err)
	}
	shadowV2, err := cs.GetCopyGuardPositionMarginShadowV2(cycle.ID)
	if err != nil || shadowV2.DataQuality != CopyGuardShadowQualityUnscorable ||
		!strings.HasPrefix(shadowV2.UnscorableReason, "POSITION_OWNERSHIP_ANOMALY:") {
		t.Fatalf("ownership anomaly did not exclude the v2 shadow: shadow=%+v err=%v", shadowV2, err)
	}
	var ownershipEvents int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='POSITION_OWNERSHIP_ANOMALY'`, cycle.ID).Scan(&ownershipEvents); err != nil || ownershipEvents != 1 {
		t.Fatalf("ownership anomaly event count=%d err=%v", ownershipEvents, err)
	}
	if err = cs.CloseCopyGuardCycle(cycle.ID, CopyGuardLeaderClosed, 5, 7, .1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteCopyGuardAccounting(cycle.ID, 0, 105, 5, .1, 0, 0); err != nil {
		t.Fatal(err)
	}
	cycle, err = cs.GetCopyGuardCycle(cycle.ID)
	if err != nil || cycle.AccountingStatus != CopyGuardAccountingUnscorable || cycle.TrackingDifference != 0 || cycle.NetGuardEffect != 0 || !strings.HasPrefix(cycle.AccountingError, "POSITION_OWNERSHIP_ANOMALY:") {
		t.Fatalf("settlement made ownership anomaly scoreable: cycle=%+v err=%v", cycle, err)
	}

	badIntent := reserveInitial("t2", "p2", "bad-initial")
	badCommit := commitFor(badIntent, "t2", "p2", "bad-initial", `{not-json`)
	if err = cs.CommitLeaderExecutionFill(badCommit); err == nil {
		t.Fatal("invalid lifecycle policy unexpectedly committed a partial mapping")
	}
	var mappings, badCycles, fillCommits int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_position_mappings WHERE trader_id='t2' AND leader_pos_id='p2'`).Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_cycles WHERE trader_id='t2' AND leader_pos_id='p2'`).Scan(&badCycles); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_fill_commits WHERE intent_id=?`, badIntent.ID).Scan(&fillCommits); err != nil {
		t.Fatal(err)
	}
	badStored, err := cs.GetExecutionIntentByID(badIntent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mappings != 0 || badCycles != 0 || fillCommits != 0 || badStored.Status != ExecutionIntentSubmitted {
		t.Fatalf("failed lifecycle left partial state: mappings=%d cycles=%d fills=%d intent=%+v", mappings, badCycles, fillCommits, badStored)
	}
}

func TestLeaderExecutionPartialFillKeepsDurableGapAndAccumulatesCatchup(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "partial-fill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, SourceFillID: "s1",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 1, RequestedNotional: 1000, RequestedQuantity: 10,
		QuantizedQuantity: 10, ClientOrderID: "initial",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	first := LeaderExecutionCommit{
		IntentID: intent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", Side: "long",
		MarginMode: "cross", LeaderTargetSize: 1, FillPrice: 100,
		FilledQuantity: 6, FilledNotional: 600, ExchangeOrderID: "o1", ExchangeState: "FILLED",
	}
	if err = cs.CommitLeaderExecutionFill(first); err != nil {
		t.Fatal(err)
	}
	partial, _, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, SourceFillID: "s1",
		Action: "open_long", Symbol: "ETHUSDT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if partial.Status != ExecutionIntentPartiallyFilled || partial.FilledQuantity != 6 {
		t.Fatalf("partial gap was not persisted: %+v", partial)
	}
	if _, err = cs.PrepareExecutionOrderAttemptWithQuantities(intent.ID, "catchup", 4, 4); err != nil {
		t.Fatal(err)
	}
	second := first
	second.FilledQuantity, second.FilledNotional, second.ExchangeOrderID = 4, 400, "o2"
	if err = cs.CommitLeaderExecutionFill(second); err != nil {
		t.Fatal(err)
	}
	if err = cs.CommitLeaderExecutionFill(second); err != nil {
		t.Fatalf("catch-up replay was not idempotent: %v", err)
	}
	done, _, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, SourceFillID: "s1",
		Action: "open_long", Symbol: "ETHUSDT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != ExecutionIntentFilled || done.FilledQuantity != 10 {
		t.Fatalf("catch-up was not accumulated exactly once: %+v", done)
	}
}

func TestLeaderExecutionCumulativeOrderSnapshotAppliesOnlyDelta(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "cumulative-order-fill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|t1|p1|1", Action: "open_long",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 1, RequestedQuantity: 10, QuantizedQuantity: 10,
		ClientOrderID: "same-order",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntentQuantityConstraints(intent.ID, 10, 10, .1, .1, 5, .1); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntentQuantityConstraints(intent.ID, 6, 6, .1, .1, 5, .1); err != nil {
		t.Fatal(err)
	}
	immutableTarget, err := cs.GetExecutionIntentByID(intent.ID)
	if err != nil || immutableTarget.TargetQuantity != 10 {
		t.Fatalf("executable retry quantity overwrote immutable target: %+v err=%v", immutableTarget, err)
	}
	if _, err = cs.PrepareExecutionOrderAttemptWithKind(intent.ID, "same-order", "initial_open", 10, 10); err != nil {
		t.Fatal(err)
	}
	first := LeaderExecutionCommit{
		IntentID: intent.ID, TraderID: "t1", LeaderID: "leader",
		LeaderPosID: "p1", SourceRevision: 1, Action: "open_long",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 1, FillPrice: 100, FilledQuantity: 4,
		FilledNotional: 400, ClientOrderID: "same-order",
		ExchangeState:   "PARTIALLY_FILLED",
		AttemptQuantity: 10,
	}
	if err = cs.CommitLeaderExecutionFill(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.FilledQuantity = 7
	second.FilledNotional = 700
	second.ExchangeOrderID = "venue-order"
	if err = cs.CommitLeaderExecutionFill(second); err != nil {
		t.Fatal(err)
	}
	if err = cs.CommitLeaderExecutionFill(second); err != nil {
		t.Fatalf("duplicate cumulative snapshot was not a no-op: %v", err)
	}
	stored, err := cs.GetExecutionIntentByID(intent.ID)
	if err != nil || stored.Status != ExecutionIntentPartiallyFilled ||
		math.Abs(stored.FilledQuantity-7) > 1e-9 || math.Abs(stored.FilledNotional-700) > 1e-9 {
		t.Fatalf("cumulative intent snapshot was not applied once: %+v err=%v", stored, err)
	}
	mapping, err := cs.GetMapping("t1", "p1")
	if err != nil || math.Abs(mapping.OpenSizeUSD-700) > 1e-9 {
		t.Fatalf("mapping did not receive the later exchange delta once: %+v err=%v", mapping, err)
	}
	attempts, err := cs.ListExecutionOrderAttempts(intent.ID)
	if err != nil || len(attempts) != 1 ||
		attempts[0].Status != ExecutionOrderAttemptPartiallyFilled ||
		math.Abs(attempts[0].FilledQuantity-7) > 1e-9 {
		t.Fatalf("order attempt cumulative state mismatch: %+v err=%v", attempts, err)
	}
}

func TestLeaderAddFillUpdatesMappingNotionalOnFirstAndLaterSnapshots(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "leader-add-fill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		Status: MappingStatusActive, SourceRevision: 1,
		OpenSizeUSD: 100, LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 2,
		Action: "open_long", Symbol: "ETHUSDT", Side: "long",
		LeaderTargetSize: 2, TargetQuantity: 1, ClientOrderID: "add-client",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve add claimed=%v err=%v", claimed, err)
	}
	first := LeaderExecutionCommit{
		IntentID: intent.ID, TraderID: "t1", LeaderID: "leader",
		LeaderPosID: "p1", SourceRevision: 2, Action: "open_long", IsAdd: true,
		Symbol: "ETHUSDT", Side: "long", FillPrice: 100,
		LeaderTargetSize: 2, FilledQuantity: .4, FilledNotional: 40,
		ClientOrderID: "add-client", ExchangeState: "PARTIALLY_FILLED",
		AttemptQuantity: 1,
	}
	if err = cs.CommitLeaderExecutionFill(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ExchangeOrderID = "add-venue"
	second.FilledQuantity, second.FilledNotional = .7, 70
	if err = cs.CommitLeaderExecutionFill(second); err != nil {
		t.Fatal(err)
	}
	mapping, err := cs.GetMapping("t1", "p1")
	if err != nil || math.Abs(mapping.OpenSizeUSD-170) > 1e-9 || mapping.SourceRevision != 2 {
		t.Fatalf("add fill notional was not accumulated exactly once: mapping=%+v err=%v", mapping, err)
	}
}

func TestFilledExecutionLifecycleGapIsRecoverableWithoutReplayingOrder(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "filled-lifecycle-gap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	openIntent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|t1|p1|1", SourceKind: "LEADER_TRANSITION",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 8, ClientOrderID: "open-stable",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve open claimed=%v err=%v", claimed, err)
	}
	if err = cs.CommitLeaderExecutionFill(LeaderExecutionCommit{
		IntentID: openIntent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", Side: "long",
		MarginMode: "cross", LeaderTargetSize: 8, FillPrice: 100, FilledQuantity: 1,
		FilledNotional: 100, ExchangeOrderID: "open-order", ExchangeState: "FILLED",
	}); err != nil {
		t.Fatal(err)
	}
	gaps, err := cs.ListFilledExecutionIntentsWithLifecycleGap("t1")
	if err != nil || len(gaps) != 1 || gaps[0].ID != openIntent.ID {
		t.Fatalf("confirmed open without cycle must be recoverable: gaps=%+v err=%v", gaps, err)
	}
	if _, err = cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1", Symbol: "ETHUSDT",
		Side: "long", MarginMode: "cross", Status: CopyGuardFollowing,
		PolicySnapshot: `{"version":4}`, LeaderEntryPrice: 100, FollowerEntryPrice: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if gaps, err = cs.ListFilledExecutionIntentsWithLifecycleGap("t1"); err != nil || len(gaps) != 0 {
		t.Fatalf("open with lifecycle must not be replayed: gaps=%+v err=%v", gaps, err)
	}
	closeIntent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 2,
		CanonicalKey: "leader|t1|p1|2", SourceKind: "LEADER_TRANSITION",
		Action: "close_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 0, ClientOrderID: "close-stable",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve close claimed=%v err=%v", claimed, err)
	}
	if err = cs.CommitLeaderExecutionFill(LeaderExecutionCommit{
		IntentID: closeIntent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		SourceRevision: 2, Action: "close_long", Symbol: "ETHUSDT", Side: "long",
		MarginMode: "cross", LeaderTargetSize: 0, FillPrice: 105, FilledQuantity: 1,
		FilledNotional: 105, ExchangeOrderID: "close-order", ExchangeState: "FILLED",
	}); err != nil {
		t.Fatal(err)
	}
	closed, err := cs.GetMappingForReconciliation("t1", "p1")
	if err != nil || closed == nil || closed.Status != MappingStatusClosed || closed.SourceRevision != 2 {
		t.Fatalf("closed mapping must remain visible to reconciliation: mapping=%+v err=%v", closed, err)
	}
	gaps, err = cs.ListFilledExecutionIntentsWithLifecycleGap("t1")
	if err != nil || len(gaps) != 1 || gaps[0].ID != closeIntent.ID {
		t.Fatalf("confirmed close with open cycle must be recoverable: gaps=%+v err=%v", gaps, err)
	}
}

func TestCommitSkippedSubLotAdvancesRevisionWithoutFakeReduction(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "sub-lot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{TraderID: "t1", LeaderPosID: "p1", LeaderID: "leader", Symbol: "BTCUSDT", Side: "short", MarginMode: "cross", OpenedAt: time.Now(), OpenPrice: 100, OpenSizeUSD: 10, LastKnownSize: 1}); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 2, CanonicalKey: "leader|t1|p1|2", Action: "reduce_short", Symbol: "BTCUSDT", Side: "short", LeaderTargetSize: 0.999, ClientOrderID: "reduce-stable"})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.CommitSkippedSubLot(intent.ID, "t1", "p1", 2, 0.999); err != nil {
		t.Fatal(err)
	}
	mapping, err := cs.GetMapping("t1", "p1")
	if err != nil || mapping.SourceRevision != 2 || mapping.LastKnownSize != 0.999 || mapping.ReduceCount != 0 || mapping.AccumulatedReduceRatio != 0 {
		t.Fatalf("sub-lot skip mutated reduction accounting: mapping=%+v err=%v", mapping, err)
	}
	stored, _, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 2, CanonicalKey: "leader|t1|p1|2", Action: "reduce_short"})
	if err != nil || stored.Status != ExecutionIntentSkipped || stored.ReasonCode != "SKIPPED_SUBLOT" {
		t.Fatalf("sub-lot intent mismatch: intent=%+v err=%v", stored, err)
	}
}

func TestSkippedTransitionRejectsNewerMappingRevisionAsEvidence(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "stale-skip.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "t1", LeaderPosID: "p1", LeaderID: "leader", Symbol: "BTCUSDT",
		Side: "short", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var oldIntent *CopyTradeExecutionIntent
	for revision, target := range []float64{0.999, 0.998} {
		intent, claimed, reserveErr := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
			TraderID: "t1", LeaderPosID: "p1", SourceRevision: int64(revision + 2),
			CanonicalKey: fmt.Sprintf("skip-revision-%d", revision+2),
			SourceFillID: fmt.Sprintf("fill-%d", revision+2),
			Action:       "reduce_short", Symbol: "BTCUSDT", Side: "short",
			LeaderTargetSize: target,
		})
		if reserveErr != nil || !claimed {
			t.Fatalf("revision %d reserve claimed=%v err=%v", revision+2, claimed, reserveErr)
		}
		if oldIntent == nil {
			oldIntent = intent
		}
		if commitErr := cs.CommitSkippedLeaderTransition(intent.ID, "t1", "p1", int64(revision+2), target, "SKIPPED_SUBLOT"); commitErr != nil {
			t.Fatalf("revision %d commit: %v", revision+2, commitErr)
		}
	}
	if err = cs.CommitSkippedLeaderTransition(oldIntent.ID, "t1", "p1", 2, 0.999, "SKIPPED_SUBLOT"); err == nil {
		t.Fatal("newer mapping revision incorrectly proved an older skipped intent")
	}
}

func TestCommitIgnoredLeaderTransitionAtomicallyCreatesBaseline(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "ignored-open.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|t1|p1|1", Action: "open_short", Symbol: "BTCUSDT",
		Side: "short", MarginMode: "cross", LeaderTargetSize: 4,
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	commit := IgnoredLeaderTransition{
		IntentID: intent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		SourceRevision: 1, Symbol: "BTCUSDT", Side: "short", MarginMode: "cross",
		LeaderTargetSize: 4, ReasonCode: "RISK_CAP",
	}
	if err = cs.CommitIgnoredLeaderTransition(commit); err != nil {
		t.Fatal(err)
	}
	mapping, err := cs.GetMapping("t1", "p1")
	if err != nil || mapping.Status != MappingStatusIgnored || mapping.SourceRevision != 1 || mapping.LastKnownSize != 4 {
		t.Fatalf("ignored mapping mismatch: mapping=%+v err=%v", mapping, err)
	}
	stored, _, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, Action: "open_short"})
	if err != nil || stored.Status != ExecutionIntentSkipped || stored.ReasonCode != "RISK_CAP" {
		t.Fatalf("ignored intent mismatch: intent=%+v err=%v", stored, err)
	}
	if err = cs.CommitIgnoredLeaderTransition(commit); err != nil {
		t.Fatalf("idempotent commit failed: %v", err)
	}
}

func TestSettleOrdinaryCatchupAdvancesZeroFillRevision(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "settle-catchup-zero.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "trader-1", LeaderID: "leader-1", LeaderPosID: "pros-pos",
		Symbol: "PROSUSDT", Side: "short", MarginMode: "cross",
		Status: MappingStatusActive, SourceRevision: 4, LastKnownSize: 13344,
		OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_position_mappings SET source_revision=4 WHERE trader_id='trader-1' AND leader_pos_id='pros-pos'`); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "pros-pos", SourceRevision: 5,
		SourceFillID: "pros-add-5", CanonicalKey: "leader|trader-1|pros-pos|5",
		Action: "open_short", Symbol: "PROSUSDT", Side: "short",
		MarginMode: "cross", LeaderTargetSize: 13389,
		ClientOrderID: "ct1234567890abcdef",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "ct1234567890abcdef", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, "ct1234567890abcdef",
		ExecutionOrderAttemptTerminalNoFill, "", "REJECTED", "OKX 51000", 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentFailed,
		"CATCHUP_SUPERSEDED", "authoritative leader position changed", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	recoverable, err := cs.ListRecoverableOrdinaryCatchupIntents("trader-1")
	if err != nil || len(recoverable) != 1 || recoverable[0].ID != intent.ID {
		t.Fatalf("current revision blocker was not discoverable at startup: intents=%+v err=%v", recoverable, err)
	}

	status, err := cs.SettleOrdinaryCatchupTransition(OrdinaryCatchupSettlement{
		IntentID: intent.ID, TraderID: "trader-1", LeaderID: "leader-1",
		ReasonCode: "CATCHUP_SKIPPED_NO_FILL", Detail: "leader closed",
	})
	if err != nil || status != ExecutionIntentSkipped {
		t.Fatalf("settle status=%s err=%v", status, err)
	}
	mapping, err := cs.GetMapping("trader-1", "pros-pos")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.SourceRevision != 5 || mapping.LastKnownSize != 13389 {
		t.Fatalf("mapping revision was not advanced: %+v", mapping)
	}
	stored, err := cs.GetExecutionIntentByID(intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != ExecutionIntentSkipped || stored.ReasonCode != "CATCHUP_SKIPPED_NO_FILL" {
		t.Fatalf("unexpected settled intent: %+v", stored)
	}
}

func TestSettleOrdinaryCatchupRejectsUnknownAttempt(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "settle-catchup-unknown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "trader-1", LeaderID: "leader-1", LeaderPosID: "pros-pos",
		Symbol: "PROSUSDT", Side: "short", MarginMode: "cross",
		Status: MappingStatusActive, SourceRevision: 4, LastKnownSize: 13344,
		OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_position_mappings SET source_revision=4 WHERE trader_id='trader-1' AND leader_pos_id='pros-pos'`); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "pros-pos", SourceRevision: 5,
		CanonicalKey: "leader|trader-1|pros-pos|5", Action: "open_short",
		Symbol: "PROSUSDT", Side: "short", LeaderTargetSize: 13389,
		ClientOrderID: "ct1234567890abcdef",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "ct1234567890abcdef", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, "ct1234567890abcdef",
		ExecutionOrderAttemptUnknown, "", "", "transport timeout", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.SettleOrdinaryCatchupTransition(OrdinaryCatchupSettlement{
		IntentID: intent.ID, TraderID: "trader-1", LeaderID: "leader-1",
	}); !errors.Is(err, ErrOrdinaryCatchupUnsettled) {
		t.Fatalf("unknown attempt must block source settlement: %v", err)
	}
	mapping, err := cs.GetMapping("trader-1", "pros-pos")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.SourceRevision != 4 {
		t.Fatalf("ambiguous attempt advanced mapping: %+v", mapping)
	}
}

func TestSettleOrdinaryCatchupPreservesPartialFill(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "settle-catchup-partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "trader-1", LeaderID: "leader-1", LeaderPosID: "sndk-pos",
		Symbol: "SNDKUSDT", Side: "long", MarginMode: "cross",
		Status: MappingStatusActive, SourceRevision: 8, LastKnownSize: 59.697,
		OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_position_mappings SET source_revision=8 WHERE trader_id='trader-1' AND leader_pos_id='sndk-pos'`); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "sndk-pos", SourceRevision: 8,
		CanonicalKey: "leader|trader-1|sndk-pos|8", Action: "open_long",
		Symbol: "SNDKUSDT", Side: "long", LeaderTargetSize: 59.697,
		TargetQuantity: 0.201, ClientOrderID: "ctabcdef1234567890",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "ctabcdef1234567890", 0.2); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, "ctabcdef1234567890",
		ExecutionOrderAttemptFilled, "order-1", "FILLED", "", 0.2); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentPartiallyFilled,
		"CATCHUP_PENDING", "", "order-1", 0.201, 0.201, 0.2); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentFailed,
		"CATCHUP_SUPERSEDED", "revision advanced", "order-1", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	status, err := cs.SettleOrdinaryCatchupTransition(OrdinaryCatchupSettlement{
		IntentID: intent.ID, TraderID: "trader-1", LeaderID: "leader-1",
		ReasonCode: "CATCHUP_RESIDUAL_SUPERSEDED",
	})
	if err != nil || status != ExecutionIntentCompletedPartial {
		t.Fatalf("settle status=%s err=%v", status, err)
	}
	stored, err := cs.GetExecutionIntentByID(intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != ExecutionIntentCompletedPartial || stored.FilledQuantity != 0.2 {
		t.Fatalf("partial fill was not preserved: %+v", stored)
	}
	baseline, available, err := cs.GetConfirmedInitialLeaderSize("trader-1", "sndk-pos")
	if err != nil || !available || baseline != 59.697 {
		t.Fatalf("completed partial fill disappeared from confirmed baseline: size=%v available=%v err=%v",
			baseline, available, err)
	}
}

func TestSettleOrdinaryCatchupDoesNotReopenClosedMapping(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "settle-closed-mapping.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "trader-1", LeaderID: "leader-1", LeaderPosID: "closed-pos",
		Symbol: "SNDKUSDT", Side: "long", MarginMode: "cross",
		Status: MappingStatusActive, SourceRevision: 4, LastKnownSize: 20,
		OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "closed-pos", SourceRevision: 5,
		CanonicalKey: "leader|trader-1|closed-pos|5", Action: "open_long",
		Symbol: "SNDKUSDT", Side: "long", LeaderTargetSize: 25,
		ClientOrderID: "closed-mapping-catchup",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "closed-mapping-catchup", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, "closed-mapping-catchup",
		ExecutionOrderAttemptTerminalNoFill, "", "CANCELED", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET status='FAILED',reason_code='CATCHUP_SUPERSEDED' WHERE id=?`, intent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_position_mappings SET status='closed',source_revision=6,closed_at=CURRENT_TIMESTAMP WHERE trader_id='trader-1' AND leader_pos_id='closed-pos'`); err != nil {
		t.Fatal(err)
	}
	status, err := cs.SettleOrdinaryCatchupTransition(OrdinaryCatchupSettlement{
		IntentID: intent.ID, TraderID: "trader-1", LeaderID: "leader-1",
		ReasonCode: "CATCHUP_SKIPPED_NO_FILL", Detail: "mapping already closed by later source transition",
	})
	if err != nil || status != ExecutionIntentSkipped {
		t.Fatalf("settle closed mapping status=%s err=%v", status, err)
	}
	var mappingStatus string
	var sourceRevision int64
	err = st.DB().QueryRow(`SELECT status,source_revision FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`,
		"trader-1", "closed-pos").Scan(&mappingStatus, &sourceRevision)
	if err != nil || mappingStatus != MappingStatusClosed || sourceRevision != 6 {
		t.Fatalf("closed mapping was mutated: status=%s revision=%d err=%v", mappingStatus, sourceRevision, err)
	}
}

func TestSettleOrdinaryCatchupAdvancesClosedMappingSequenceWithoutReopening(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "settle-closed-sequence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "trader-1", LeaderID: "leader-1", LeaderPosID: "closed-pos",
		Symbol: "SNDKUSDT", Side: "long", MarginMode: "cross",
		Status: MappingStatusActive, SourceRevision: 4, LastKnownSize: 20,
		OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "closed-pos", SourceRevision: 5,
		CanonicalKey: "leader|trader-1|closed-pos|5", Action: "open_long",
		Symbol: "SNDKUSDT", Side: "long", LeaderTargetSize: 25,
		ClientOrderID: "closed-sequence-catchup",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "closed-sequence-catchup", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, "closed-sequence-catchup",
		ExecutionOrderAttemptTerminalNoFill, "", "CANCELED", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents
		SET status='FAILED',reason_code='CATCHUP_SUPERSEDED' WHERE id=?`, intent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_position_mappings
		SET status='closed',source_revision=4,closed_at=CURRENT_TIMESTAMP
		WHERE trader_id='trader-1' AND leader_pos_id='closed-pos'`); err != nil {
		t.Fatal(err)
	}
	recoverable, err := cs.ListRecoverableOrdinaryCatchupIntents("trader-1")
	if err != nil || len(recoverable) != 1 || recoverable[0].ID != intent.ID {
		t.Fatalf("closed zero-fill blocker was not discoverable: intents=%+v err=%v", recoverable, err)
	}

	status, err := cs.SettleOrdinaryCatchupTransition(OrdinaryCatchupSettlement{
		IntentID: intent.ID, TraderID: "trader-1", LeaderID: "leader-1",
		ReasonCode: "CATCHUP_SKIPPED_NO_FILL", Detail: "closed mapping sequence acknowledgement",
	})
	if err != nil || status != ExecutionIntentSkipped {
		t.Fatalf("settle closed sequence status=%s err=%v", status, err)
	}
	var mappingStatus string
	var sourceRevision int64
	err = st.DB().QueryRow(`SELECT status,source_revision FROM copy_trade_position_mappings
		WHERE trader_id=? AND leader_pos_id=?`, "trader-1", "closed-pos").
		Scan(&mappingStatus, &sourceRevision)
	if err != nil || mappingStatus != MappingStatusClosed || sourceRevision != 5 {
		t.Fatalf("closed mapping sequence mismatch: status=%s revision=%d err=%v",
			mappingStatus, sourceRevision, err)
	}
}

func TestReclaimUnsubmittedExecutionIntentRequiresNoExchangeEvidence(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "reclaim-unsubmitted.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|trader-1|p1|1", Action: "close_long",
		LeaderTargetSize: 0, ClientOrderID: "close-unsubmitted",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents
		SET status='FAILED',reason_code='MANUAL_REVIEW_REQUIRED',terminal_at=CURRENT_TIMESTAMP
		WHERE id=?`, intent.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := cs.ReclaimUnsubmittedExecutionIntent(intent.ID, "trader-1", "close_long", 0)
	if err != nil || !reclaimed {
		t.Fatalf("safe reclaim=%v err=%v", reclaimed, err)
	}
	stored, err := cs.GetExecutionIntentByID(intent.ID)
	if err != nil || stored.Status != ExecutionIntentReserved || stored.ReasonCode != "" {
		t.Fatalf("unexpected reclaimed intent: %+v err=%v", stored, err)
	}

	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "close-unsubmitted", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents
		SET status='FAILED',reason_code='MANUAL_REVIEW_REQUIRED',terminal_at=CURRENT_TIMESTAMP
		WHERE id=?`, intent.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err = cs.ReclaimUnsubmittedExecutionIntent(intent.ID, "trader-1", "close_long", 0)
	if err != nil || reclaimed {
		t.Fatalf("attempt evidence must block reclaim: reclaimed=%v err=%v", reclaimed, err)
	}
}

func TestPreSubmitTerminalAttemptCanReplaySameSourceRevision(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "pre-submit-attempt-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	request := &CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "p1", SourceRevision: 1,
		SourceFillID: "fill-1", CanonicalKey: "leader|trader-1|p1|1",
		Action: "open_long", Symbol: "SYNUSDT", Side: "long", LeaderTargetSize: 56,
		RequestedNotional: 5, RequestedQuantity: 56, QuantizedQuantity: 56,
		ClientOrderID: "copy-syn-open",
	}
	intent, claimed, err := cs.ReserveExecutionIntent(request)
	if err != nil || !claimed {
		t.Fatalf("initial reserve claimed=%v err=%v", claimed, err)
	}
	attempt, err := cs.PrepareExecutionOrderAttemptRecordWithKind(intent.ID, request.ClientOrderID, "INITIAL_OPEN", 56, 56)
	if err != nil || attempt.Status != ExecutionOrderAttemptPrepared {
		t.Fatalf("prepare local attempt=%+v err=%v", attempt, err)
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, request.ClientOrderID,
		ExecutionOrderAttemptTerminalNoFill, "", "", "minimum notional changed before submit", 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentFailed, "PRE_SUBMIT", "minimum notional changed before submit", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	replayed, claimed, err := cs.ReserveExecutionIntent(request)
	if err != nil || !claimed || replayed.ID != intent.ID || replayed.Status != ExecutionIntentReserved {
		t.Fatalf("safe pre-submit replay intent=%+v claimed=%v err=%v", replayed, claimed, err)
	}
	attempt, err = cs.PrepareExecutionOrderAttemptRecordWithKind(intent.ID, request.ClientOrderID, "INITIAL_OPEN", 57, 57)
	if err != nil || attempt.Status != ExecutionOrderAttemptPrepared || attempt.TerminalAt != nil || attempt.SubmittedAt != nil {
		t.Fatalf("safe attempt was not reopened locally: %+v err=%v", attempt, err)
	}
	if _, err = cs.MarkExecutionOrderAttemptSubmitted(intent.ID, request.ClientOrderID); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentFailed, "PRE_SUBMIT", "should remain blocked", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if replayed, claimed, err = cs.ReserveExecutionIntent(request); err != nil || claimed {
		t.Fatalf("submitted attempt was replayed: intent=%+v claimed=%v err=%v", replayed, claimed, err)
	}
}

func TestClosedMappingReopenUsesNextRevisionAndReactivatesMapping(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "reopen-fill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "t1", LeaderPosID: "p1", LeaderID: "leader", Symbol: "BTCUSDT",
		Side: "short", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err = cs.CloseMapping("t1", "p1", 99); err != nil {
		t.Fatal(err)
	}
	revision, err := cs.GetSourceSnapshotRevision("t1", "p1")
	if err != nil || revision != 2 {
		t.Fatalf("closed revision=%d err=%v", revision, err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: revision + 1,
		CanonicalKey: "leader|t1|p1|3", Action: "open_short", Symbol: "BTCUSDT",
		Side: "short", MarginMode: "cross", ClientOrderID: "reopen-stable",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentSubmitted, "", "", "order-3", 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	err = cs.CommitLeaderExecutionFill(LeaderExecutionCommit{
		IntentID: intent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		SourceRevision: 3, Action: "open_short", Symbol: "BTCUSDT", Side: "short",
		MarginMode: "cross", LeaderTargetSize: 2, FillPrice: 101, FilledQuantity: 1,
		FilledNotional: 101, ExchangeOrderID: "order-3", ExchangeState: "FILLED",
	})
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := cs.GetActiveMapping("t1", "p1")
	if err != nil || mapping == nil || mapping.SourceRevision != 3 || mapping.LastKnownSize != 2 {
		t.Fatalf("mapping not reopened: mapping=%+v err=%v", mapping, err)
	}
}

func TestStaleIntentCannotClaimNewerClosedMappingAcknowledgement(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "stale-fill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "t1", LeaderPosID: "p1", LeaderID: "leader", Symbol: "BTCUSDT",
		Side: "short", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err = cs.CloseMapping("t1", "p1", 99); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "legacy|t1|p1|1", Action: "open_short", Symbol: "BTCUSDT",
		Side: "short", MarginMode: "cross", ClientOrderID: "legacy-stable",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentSubmitted, "", "", "old-order", 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	err = cs.CommitLeaderExecutionFill(LeaderExecutionCommit{
		IntentID: intent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		SourceRevision: 1, Action: "open_short", Symbol: "BTCUSDT", Side: "short",
		MarginMode: "cross", LeaderTargetSize: 2, FillPrice: 101, FilledQuantity: 1,
		FilledNotional: 101, ExchangeOrderID: "old-order", ExchangeState: "FILLED",
	})
	if err == nil {
		t.Fatal("stale intent unexpectedly committed")
	}
	stored, _, reserveErr := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "legacy|t1|p1|1", Action: "open_short",
	})
	if reserveErr != nil || stored.Status != ExecutionIntentSubmitted {
		t.Fatalf("stale intent state changed: intent=%+v err=%v", stored, reserveErr)
	}
}

func TestSubmittedAndReconcilingIntentMaySettleAsSkipped(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "skip-settlement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	for index, start := range []string{ExecutionIntentSubmitted, ExecutionIntentReconciling} {
		intent, claimed, reserveErr := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
			TraderID: "t1", LeaderPosID: "p" + start, SourceRevision: 1,
			CanonicalKey: "skip|" + start, Action: "close_long",
		})
		if reserveErr != nil || !claimed {
			t.Fatalf("case %d reserve claimed=%v err=%v", index, claimed, reserveErr)
		}
		if updateErr := cs.UpdateExecutionIntent(intent.ID, start, "", "", "", 0, 0, 0); updateErr != nil {
			t.Fatal(updateErr)
		}
		if updateErr := cs.UpdateExecutionIntent(intent.ID, ExecutionIntentSkipped, "POSITION_ALREADY_FLAT", "", "", 0, 0, 0); updateErr != nil {
			t.Fatalf("case %d skip transition: %v", index, updateErr)
		}
	}
}

func TestTerminalIntentDoesNotAbsorbLaterSourceTransition(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "terminal-source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	first, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 4,
		CanonicalKey: "leader|t1|p1|4", SourceFillID: "fill-5600",
		SourceFillIDs: []string{"fill-5600"}, Action: "reduce_short",
		LeaderTargetSize: 5600,
	})
	if err != nil || !claimed {
		t.Fatalf("first reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(first.ID, ExecutionIntentSkipped, "SKIPPED_SUBLOT", "", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 4,
		CanonicalKey: "leader|t1|p1|4", SourceFillID: "fill-4000",
		SourceFillIDs: []string{"fill-4000"}, Action: "reduce_short",
		LeaderTargetSize: 4000,
	})
	if err != nil || claimed || got.ID != first.ID {
		t.Fatalf("later transition should wait for next revision: got=%+v claimed=%v err=%v", got, claimed, err)
	}
	var attached, transitions int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intent_sources WHERE intent_id=? AND source_fill_id='fill-4000'`, first.ID).Scan(&attached); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_source_transitions WHERE intent_id=? AND source_fill_id='fill-4000' AND status='SOURCE_REPLAY_PENDING'`, first.ID).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if attached != 0 || transitions != 1 {
		t.Fatalf("terminal intent must not absorb later source but must audit it for replay: sources=%d replay_transitions=%d", attached, transitions)
	}
	replayed, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 5,
		CanonicalKey: "leader|t1|p1|5", SourceFillID: "fill-4000",
		SourceFillIDs: []string{"fill-4000"}, Action: "reduce_short",
		LeaderTargetSize: 4000,
	})
	if err != nil || !claimed || replayed.ID == first.ID {
		t.Fatalf("replayed source must bind to the next canonical intent: got=%+v claimed=%v err=%v", replayed, claimed, err)
	}
	var rebound int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_source_transitions WHERE intent_id=? AND source_fill_id='fill-4000' AND source_revision=5 AND status='RESERVED'`, replayed.ID).Scan(&rebound); err != nil {
		t.Fatal(err)
	}
	if rebound != 1 {
		t.Fatalf("replayed source audit was not rebound to the next intent: %d", rebound)
	}
}

func TestMissingFollowerDetachesWithoutCreatingStopEvidence(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "detached.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "t1", LeaderPosID: "p1", LeaderID: "leader", Symbol: "BTCUSDT",
		Side: "short", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 11000,
	}); err != nil {
		t.Fatal(err)
	}
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1", Symbol: "BTCUSDT",
		Side: "short", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: `{"version":4}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 2,
		CanonicalKey: "leader|t1|p1|2", SourceFillID: "reduce-5600",
		Action: "reduce_short", LeaderTargetSize: 5600,
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.CommitDetachedLeaderTransition(intent.ID, "t1", "p1", 2, 5600, "FOLLOWER_POSITION_MISSING"); err != nil {
		t.Fatal(err)
	}
	mapping, err := cs.GetMapping("t1", "p1")
	if err != nil || mapping == nil || mapping.Status != MappingStatusDetached || mapping.SourceRevision != 2 || mapping.LastKnownSize != 5600 {
		t.Fatalf("detached mapping mismatch: mapping=%+v err=%v", mapping, err)
	}
	closedCycle, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil || closedCycle.Status != CopyGuardDetached || closedCycle.ClosedAt == nil || closedCycle.StopCount != 0 || closedCycle.AccountingStatus != "UNSCORABLE" {
		t.Fatalf("detached cycle invented stop evidence: cycle=%+v err=%v", closedCycle, err)
	}
}

func TestExecutionReconciliationBecomesManualReviewAfterBound(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "reconcile-bound.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|t1|p1|1", Action: "open_long",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if terminal, err := cs.RecordExecutionReconciliationFailure(intent.ID, "ORDER_LOOKUP_FAILED", "temporary", 2, time.Hour); err != nil || terminal {
		t.Fatalf("first reconciliation result terminal=%v err=%v", terminal, err)
	}
	if terminal, err := cs.RecordExecutionReconciliationFailure(intent.ID, "ORDER_LOOKUP_FAILED", "again", 2, time.Hour); err != nil || !terminal {
		t.Fatalf("second reconciliation result terminal=%v err=%v", terminal, err)
	}
	var status, reason string
	if err = st.DB().QueryRow(`SELECT status,reason_code FROM copy_trade_execution_intents WHERE id=?`, intent.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != ExecutionIntentFailed || reason != "MANUAL_REVIEW_REQUIRED" {
		t.Fatalf("unexpected reconciliation terminal: %s/%s", status, reason)
	}
}

func TestSubmittedExecutionReconciliationNeverStopsAutomaticLookup(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "submitted-reconcile-bound.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|t1|p1|1", Action: "open_long",
		ClientOrderID: "stable-client-id",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "stable-client-id", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentSubmitted, "", "", "", 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		terminal, recordErr := cs.RecordExecutionReconciliationFailure(
			intent.ID, "ORDER_LOOKUP_FAILED", "exchange temporarily unavailable", 1, time.Nanosecond,
		)
		if recordErr != nil {
			t.Fatal(recordErr)
		}
		if terminal {
			t.Fatalf("submitted intent became terminal on reconciliation attempt %d", attempt+1)
		}
	}
	var status, reason string
	if err = st.DB().QueryRow(`SELECT status,reason_code FROM copy_trade_execution_intents WHERE id=?`, intent.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != ExecutionIntentReconciling || reason != "ORDER_LOOKUP_FAILED" {
		t.Fatalf("submitted intent must keep automatic reconciliation: status=%s reason=%s", status, reason)
	}
}

func TestSourceRevalidationIntentCanOnlyBeReclaimedWithoutAttempts(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "source-revalidation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|t1|p1|1", Action: "open_long", Symbol: "ETHUSDT",
		SourceFillID: "f1", LeaderTargetSize: 1, ClientOrderID: "stable",
	}
	intent, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if pending, pendingErr := cs.HasUnfinishedLeaderExecutionIntent("t1", "p1"); pendingErr != nil || !pending {
		t.Fatalf("reserved leader intent must block startup ignored baseline: pending=%v err=%v", pending, pendingErr)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentReconciling, "SOURCE_REVALIDATION_REQUIRED", "", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	replayed := *base
	replayed.SourceFillID = "f2"
	replayed.SourceFillIDs = []string{"f1", "f2"}
	replayed.LeaderTargetSize = 2
	got, claimed, err := cs.ReserveExecutionIntent(&replayed)
	if err != nil || !claimed || got.ID != intent.ID || got.Status != ExecutionIntentReserved {
		t.Fatalf("healthy replay must reclaim pre-submit intent: got=%+v claimed=%v err=%v", got, claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "stable", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentReconciling, "SOURCE_REVALIDATION_REQUIRED", "", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err = cs.ReserveExecutionIntent(&replayed); err != nil || claimed {
		t.Fatalf("an intent with a durable attempt must never be reclaimed: claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentFailed, "EXCHANGE_TERMINAL_NO_FILL", "", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if pending, pendingErr := cs.HasUnfinishedLeaderExecutionIntent("t1", "p1"); pendingErr != nil || pending {
		t.Fatalf("terminal leader intent must not block startup baseline: pending=%v err=%v", pending, pendingErr)
	}
}

func TestExecutionOrderAttemptCompletionRequiresDurablePreparation(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "attempt-completion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|t1|p1|1", Action: "open_long",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, "missing-client-id", ExecutionOrderAttemptFilled, "order-1", "FILLED", "", 1); err == nil {
		t.Fatal("missing durable order attempt was silently accepted")
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "stable-client-id", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(intent.ID, "stable-client-id", ExecutionOrderAttemptFilled, "order-1", "FILLED", "", 1); err != nil {
		t.Fatalf("prepared order attempt completion failed: %v", err)
	}
}

func TestExecutionOrderAttemptAllowsZeroOnlyForCloseAllAndSubmitsAtomically(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "close-all-attempt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()

	closeIntent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "close-pos", SourceRevision: 2,
		SourceFillID: "close-snapshot", CanonicalKey: "leader|t1|close-pos|2",
		Action: "close_short", Symbol: "LIGHTUSDT", Side: "short",
		ClientOrderID: "close-all-client",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve close intent claimed=%v err=%v", claimed, err)
	}
	attempt, err := cs.PrepareExecutionOrderAttempt(closeIntent.ID, "close-all-client", 0)
	if err != nil {
		t.Fatalf("close-all zero quantity must be durable: %v", err)
	}
	if attempt.QuantizedQuantity != 0 || attempt.Status != ExecutionOrderAttemptPrepared {
		t.Fatalf("unexpected close-all attempt: %+v", attempt)
	}
	var closeStatus string
	var closeSubmitted sql.NullString
	if err = st.DB().QueryRow(`SELECT status,submitted_at FROM copy_trade_execution_intents WHERE id=?`, closeIntent.ID).
		Scan(&closeStatus, &closeSubmitted); err != nil {
		t.Fatal(err)
	}
	if closeStatus != ExecutionIntentSubmitted || !closeSubmitted.Valid {
		t.Fatalf("attempt and parent submission must commit atomically: status=%s submitted=%v", closeStatus, closeSubmitted)
	}

	openIntent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "open-pos", SourceRevision: 1,
		CanonicalKey: "leader|t1|open-pos|1", Action: "open_long",
		Symbol: "ETHUSDT", Side: "long", ClientOrderID: "open-zero-client",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve open intent claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(openIntent.ID, "open-zero-client", 0); err == nil {
		t.Fatal("zero quantity open must be rejected")
	}
	var openStatus string
	var openSubmitted sql.NullString
	var attemptCount int
	if err = st.DB().QueryRow(`SELECT status,submitted_at FROM copy_trade_execution_intents WHERE id=?`, openIntent.ID).
		Scan(&openStatus, &openSubmitted); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts WHERE intent_id=?`, openIntent.ID).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if openStatus != ExecutionIntentReserved || openSubmitted.Valid || attemptCount != 0 {
		t.Fatalf("rejected open mutated durable state: status=%s submitted=%v attempts=%d", openStatus, openSubmitted, attemptCount)
	}
	openAttempt, err := cs.PrepareExecutionOrderAttemptWithQuantities(openIntent.ID, "open-zero-client", 1.234, 1.2)
	if err != nil {
		t.Fatalf("valid quantized open attempt failed: %v", err)
	}
	if openAttempt.RequestedQuantity != 1.234 || openAttempt.QuantizedQuantity != 1.2 {
		t.Fatalf("raw and quantized quantities were conflated: %+v", openAttempt)
	}
	var storedRequested, storedQuantized float64
	if err = st.DB().QueryRow(`SELECT requested_quantity,quantized_quantity FROM copy_trade_execution_intents WHERE id=?`, openIntent.ID).
		Scan(&storedRequested, &storedQuantized); err != nil {
		t.Fatal(err)
	}
	if storedRequested != 1.234 || storedQuantized != 1.2 {
		t.Fatalf("parent intent lost quantity audit fields: requested=%v quantized=%v", storedRequested, storedQuantized)
	}
}

func TestLegacyZeroQuantityCloseFailureIsNarrowlyReclaimable(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "legacy-close-reclaim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "legacy-close", SourceRevision: 3,
		SourceFillID: "snapshot-close-1", CanonicalKey: "leader|t1|legacy-close|3",
		Action: "close_short", Symbol: "LIGHTUSDT", Side: "short",
		ClientOrderID: "stable-close-client",
	}
	intent, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	// Reproduce the old ordering: the parent was marked SUBMITTED before
	// attempt preparation rejected quantity=0, so no exchange call or durable
	// attempt could have occurred.
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentSubmitted, "", "", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentFailed, "EXECUTION_FAILED",
		"persist close short attempt: invalid execution order attempt", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	replayed := *base
	replayed.SourceFillID = "snapshot-close-2"
	reclaimed, claimed, err := cs.ReserveExecutionIntent(&replayed)
	if err != nil || !claimed || reclaimed.ID != intent.ID || reclaimed.Status != ExecutionIntentReserved {
		t.Fatalf("proven pre-adapter legacy failure was not reclaimed: intent=%+v claimed=%v err=%v", reclaimed, claimed, err)
	}
	var submitted sql.NullString
	if err = st.DB().QueryRow(`SELECT submitted_at FROM copy_trade_execution_intents WHERE id=?`, intent.ID).Scan(&submitted); err != nil {
		t.Fatal(err)
	}
	if submitted.Valid {
		t.Fatalf("legacy false submission evidence was not cleared: %v", submitted)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "stable-close-client", 0); err != nil {
		t.Fatalf("reclaimed close-all intent must reach durable submission: %v", err)
	}
}

func TestApplyAIReentryFillSnapshotCommitsOnlyCumulativeDelta(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "ai-partial-fill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "t1", LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		Status: MappingStatusStoppedByRisk, LastKnownSize: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_position_mappings SET status='stopped_by_risk' WHERE trader_id='t1' AND leader_pos_id='leader-pos'`); err != nil {
		t.Fatal(err)
	}
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{PolicySnapshot: `{"version":4}`,
		TraderID: "t1", LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		Status: CopyGuardReentryPending, LeaderEntryPrice: 100,
		FollowerEntryPrice: 95, FollowerNotional: 0, AccountEquity: 1000,
		ProtectionStatus: CopyGuardProtectionCanceled,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "leader-pos", SourceRevision: 1_000_001,
		SourceKind: "AI_REENTRY", CanonicalKey: "ai|t1|cycle|1",
		CycleID: cycle.ID, CandidateID: 11, AnalysisID: 12, AttemptNo: 1, DecisionGeneration: 2,
		Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 5, RequestedNotional: 100, ClientOrderID: "ai-client",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve intent claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttemptWithKind(intent.ID, "ai-client", "ai_reentry", 1, 1); err != nil {
		t.Fatal(err)
	}
	first, err := cs.ApplyAIReentryFillSnapshot(AIReentryFillSnapshot{
		IntentID: intent.ID, TraderID: "t1", ClientOrderID: "ai-client",
		ExchangeState:      "PARTIALLY_FILLED",
		CumulativeQuantity: .4, CumulativeNotional: 40, AveragePrice: 100,
		AttemptQuantity: 1, LeaderTargetSize: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.FirstFill || first.DeltaQuantity != .4 || first.IntentStatus != ExecutionIntentPartiallyFilled {
		t.Fatalf("unexpected first fill: %+v", first)
	}
	duplicate, err := cs.ApplyAIReentryFillSnapshot(AIReentryFillSnapshot{
		IntentID: intent.ID, TraderID: "t1", ClientOrderID: "ai-client",
		ExchangeOrderID: "venue-order", ExchangeState: "PARTIALLY_FILLED",
		CumulativeQuantity: .4, CumulativeNotional: 40, AveragePrice: 100,
		AttemptQuantity: 1, LeaderTargetSize: 5,
	})
	if err != nil || duplicate.DeltaQuantity != 0 {
		t.Fatalf("duplicate cumulative snapshot was not a no-op: %+v err=%v", duplicate, err)
	}
	final, err := cs.ApplyAIReentryFillSnapshot(AIReentryFillSnapshot{
		IntentID: intent.ID, TraderID: "t1", ClientOrderID: "ai-client",
		ExchangeOrderID: "venue-order", ExchangeState: "CANCELED",
		CumulativeQuantity: .7, CumulativeNotional: 70, AveragePrice: 100,
		AttemptQuantity: 1, LeaderTargetSize: 5, OrderTerminal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if final.FirstFill || math.Abs(final.DeltaQuantity-.3) > 1e-9 || final.IntentStatus != ExecutionIntentFilled {
		t.Fatalf("unexpected terminal partial result: %+v", final)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentProtected, "", "", "", 0, 0, .7); err != nil {
		t.Fatal(err)
	}
	protectedDuplicate, err := cs.ApplyAIReentryFillSnapshot(AIReentryFillSnapshot{
		IntentID: intent.ID, TraderID: "t1", ClientOrderID: "ai-client",
		ExchangeOrderID: "venue-order", ExchangeState: "CANCELED",
		CumulativeQuantity: .7, CumulativeNotional: 70, AveragePrice: 100,
		AttemptQuantity: 1, LeaderTargetSize: 5, OrderTerminal: true,
	})
	if err != nil || protectedDuplicate.DeltaQuantity != 0 ||
		protectedDuplicate.IntentStatus != ExecutionIntentProtected {
		t.Fatalf("duplicate terminal snapshot regressed protected intent: %+v err=%v", protectedDuplicate, err)
	}
	storedIntent, err := cs.GetExecutionIntentByID(intent.ID)
	if err != nil || math.Abs(storedIntent.FilledQuantity-.7) > 1e-9 ||
		math.Abs(storedIntent.FilledNotional-70) > 1e-9 || storedIntent.Status != ExecutionIntentProtected ||
		storedIntent.ProtectedAt == nil || storedIntent.TerminalAt == nil {
		t.Fatalf("intent cumulative fill is wrong: %+v err=%v", storedIntent, err)
	}
	mapping, err := cs.GetMapping("t1", "leader-pos")
	if err != nil || mapping.Status != MappingStatusActive || math.Abs(mapping.OpenSizeUSD-70) > 1e-9 {
		t.Fatalf("mapping did not receive exact fill deltas: %+v err=%v", mapping, err)
	}
	cycle, err = cs.GetCopyGuardCycle(cycle.ID)
	if err != nil || cycle.ReentryCount != 1 || cycle.Status != CopyGuardFollowingReentry ||
		math.Abs(cycle.FollowerNotional-70) > 1e-9 {
		t.Fatalf("cycle did not receive exact fill deltas: %+v err=%v", cycle, err)
	}
	var attemptQuantity, attemptNotional float64
	if err = st.DB().QueryRow(`SELECT quantity,notional FROM copy_guard_attempts WHERE cycle_id=? AND attempt_no=1`,
		cycle.ID).Scan(&attemptQuantity, &attemptNotional); err != nil {
		t.Fatal(err)
	}
	if math.Abs(attemptQuantity-.7) > 1e-9 || math.Abs(attemptNotional-70) > 1e-9 {
		t.Fatalf("copy guard attempt double-counted fill: quantity=%v notional=%v", attemptQuantity, attemptNotional)
	}
	attempts, err := cs.ListExecutionOrderAttempts(intent.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != ExecutionOrderAttemptPartiallyFilled ||
		attempts[0].TerminalAt == nil || math.Abs(attempts[0].FilledQuantity-.7) > 1e-9 {
		t.Fatalf("durable order attempt state is wrong: %+v err=%v", attempts, err)
	}
}

func TestLegacyCloseFailureWithAmbiguousEvidenceIsNotReclaimed(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "legacy-close-ambiguous.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "ambiguous-close", SourceRevision: 3,
		CanonicalKey: "leader|t1|ambiguous-close|3", Action: "close_short",
		Symbol: "LIGHTUSDT", Side: "short", ClientOrderID: "ambiguous-client",
	}
	intent, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentSubmitted, "", "", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentFailed, "EXECUTION_FAILED",
		"exchange response was lost", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err = cs.ReserveExecutionIntent(base); err != nil || claimed {
		t.Fatalf("ambiguous submitted close must remain terminal: claimed=%v err=%v", claimed, err)
	}
}

func TestSourceRevalidationIntentRebindsToAuthoritativeAction(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "source-revalidation-action.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|t1|p1|1", Action: "open_long", Symbol: "ETHUSDT",
		Side: "long", SourceFillID: "f1", LeaderTargetSize: 1, ClientOrderID: "open-order",
	}
	intent, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentReconciling, "SOURCE_REVALIDATION_REQUIRED", "", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	revalidated := *base
	revalidated.Action = "close_long"
	revalidated.SourceFillID = "f2"
	revalidated.SourceFillIDs = []string{"f2"}
	revalidated.LeaderTargetSize = 0
	revalidated.ClientOrderID = "close-order"
	got, claimed, err := cs.ReserveExecutionIntent(&revalidated)
	if err != nil || !claimed {
		t.Fatalf("authoritative action rebind claimed=%v err=%v", claimed, err)
	}
	if got.ID != intent.ID || got.Action != "close_long" || got.LeaderTargetSize != 0 ||
		got.ClientOrderID != "close-order" || got.Status != ExecutionIntentReserved {
		t.Fatalf("unexpected rebound intent: %+v", got)
	}
}

func TestSupersedeUnsubmittedExecutionIntentAllowsLaterReplay(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "source-superseded.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := &CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1,
		CanonicalKey: "leader|t1|p1|1", Action: "open_long", Symbol: "ETHUSDT",
		Side: "long", SourceFillID: "f1", LeaderTargetSize: 1, ClientOrderID: "open-order",
	}
	intent, claimed, err := cs.ReserveExecutionIntent(base)
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentReconciling, "SOURCE_REVALIDATION_REQUIRED", "", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.SupersedeUnsubmittedExecutionIntent(intent.ID, "t1", "p1"); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	if err = st.DB().QueryRow(`SELECT status,reason_code FROM copy_trade_execution_intents WHERE id=?`, intent.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != ExecutionIntentSkipped || reason != "SOURCE_SUPERSEDED" {
		t.Fatalf("unexpected superseded terminal: %s/%s", status, reason)
	}
	replayed := *base
	replayed.SourceFillID = "f2"
	replayed.SourceFillIDs = []string{"f2"}
	got, claimed, err := cs.ReserveExecutionIntent(&replayed)
	if err != nil || !claimed || got.ID != intent.ID || got.Status != ExecutionIntentReserved {
		t.Fatalf("later source replay must safely reclaim zero-side-effect intent: got=%+v claimed=%v err=%v", got, claimed, err)
	}
}

func TestCopyGuardBackfillBaselineUsesConfirmedInitialOpenOnly(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "baseline-evidence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if size, available, err := cs.GetConfirmedInitialLeaderSize("t1", "p1"); err != nil || available || size != 0 {
		t.Fatalf("missing evidence must remain unavailable: size=%v available=%v err=%v", size, available, err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: 1, CanonicalKey: "baseline",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 8, ClientOrderID: "baseline-order",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if err = cs.CommitLeaderExecutionFill(LeaderExecutionCommit{
		IntentID: intent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
		SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", Side: "long",
		MarginMode: "cross", LeaderTargetSize: 8, FillPrice: 100, FilledQuantity: 1,
		FilledNotional: 100, ExchangeOrderID: "order", ExchangeState: "FILLED",
	}); err != nil {
		t.Fatal(err)
	}
	size, available, err := cs.GetConfirmedInitialLeaderSize("t1", "p1")
	if err != nil || !available || size != 8 {
		t.Fatalf("confirmed initial evidence not recovered: size=%v available=%v err=%v", size, available, err)
	}
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{PolicySnapshot: `{"version":4}`,
		TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1", Symbol: "ETHUSDT",
		Side: "long", Status: CopyGuardFollowing, BaselineLeaderSize: size, ShadowLeaderSize: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cycle.BaselineAvailable || cycle.BaselineLeaderSize != 8 || cycle.ShadowLeaderSize != 6 {
		t.Fatalf("immutable baseline and current shadow were conflated: %+v", cycle)
	}
}

func TestCopyGuardBackfillBaselineUsesCurrentReopenedLifecycle(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "baseline-reopen.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	commit := func(revision int64, action string, target float64, isAdd bool) {
		t.Helper()
		intent, claimed, reserveErr := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
			TraderID: "t1", LeaderPosID: "p1", SourceRevision: revision,
			CanonicalKey: fmt.Sprintf("baseline-reopen-%d", revision),
			Action:       action, Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
			LeaderTargetSize: target, ClientOrderID: fmt.Sprintf("baseline-order-%d", revision),
		})
		if reserveErr != nil || !claimed {
			t.Fatalf("reserve revision %d claimed=%v err=%v", revision, claimed, reserveErr)
		}
		if commitErr := cs.CommitLeaderExecutionFill(LeaderExecutionCommit{
			IntentID: intent.ID, TraderID: "t1", LeaderID: "leader", LeaderPosID: "p1",
			SourceRevision: revision, Action: action, IsAdd: isAdd, Symbol: "ETHUSDT", Side: "long",
			MarginMode: "cross", LeaderTargetSize: target, FillPrice: 100, FilledQuantity: 1,
			FilledNotional: 100, ExchangeOrderID: fmt.Sprintf("order-%d", revision), ExchangeState: "FILLED",
		}); commitErr != nil {
			t.Fatalf("commit revision %d: %v", revision, commitErr)
		}
	}

	commit(1, "open_long", 8, false)
	commit(2, "open_long", 10, true)
	commit(3, "close_long", 0, false)
	commit(4, "open_long", 5, false)
	commit(5, "open_long", 7, true)

	size, available, err := cs.GetConfirmedInitialLeaderSize("t1", "p1")
	if err != nil || !available || size != 5 {
		t.Fatalf("current reopened lifecycle baseline must use revision 4 open, not old open/add: size=%v available=%v err=%v", size, available, err)
	}
}
