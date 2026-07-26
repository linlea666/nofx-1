package store

import (
	"database/sql"
	"path/filepath"
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
