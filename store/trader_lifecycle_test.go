package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func createLifecycleTestTrader(t *testing.T, st *Store, id, status string, generation int64) {
	t.Helper()
	running := status == TraderLifecycleRunning
	if _, err := st.DB().Exec(`INSERT INTO traders
		(id,user_id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		id, "user-1", id, "model-1", "exchange-1", 1000, running, status, generation); err != nil {
		t.Fatal(err)
	}
}

func TestTraderLifecycleMigrationProjectsLegacyRunningState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-traders.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`CREATE TABLE traders (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL DEFAULT 'default',
		name TEXT NOT NULL,
		ai_model_id TEXT NOT NULL,
		exchange_id TEXT NOT NULL,
		initial_balance REAL NOT NULL,
		scan_interval_minutes INTEGER DEFAULT 3,
		is_running BOOLEAN DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,is_running)
	VALUES('running','user-1','running','model','exchange',1000,1),
	      ('stopped','user-1','stopped','model','exchange',1000,0)`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	running, err := st.Trader().GetLifecycle("running")
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != TraderLifecycleRunning || !running.IsRunning ||
		!st.Trader().IsGenerationRunning("running", running.Generation) {
		t.Fatalf("legacy running projection was lost: %+v", running)
	}
	stopped, err := st.Trader().GetLifecycle("stopped")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != TraderLifecycleStopped || stopped.IsRunning {
		t.Fatalf("legacy stopped projection was changed: %+v", stopped)
	}
}

func TestLifecycleOwnershipLookupSurvivesMissingRuntimeDependencies(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "missing-dependencies.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleStopped, 4)

	if _, err = st.Trader().GetFullConfig("user-1", "trader-1"); err == nil {
		t.Fatal("incomplete AI/exchange dependencies unexpectedly produced a full runtime config")
	}
	lifecycle, err := st.Trader().GetLifecycleForUser("user-1", "trader-1")
	if err != nil || lifecycle.Status != TraderLifecycleStopped || lifecycle.Generation != 4 {
		t.Fatalf("safe stop/archive lookup was lost with runtime dependencies: %+v err=%v", lifecycle, err)
	}
	if _, err = st.Trader().GetLifecycleForUser("other-user", "trader-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lifecycle ownership boundary was not enforced: %v", err)
	}
}

func TestBeginStopInvalidatesGenerationAndPreservesSubmittedWork(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "stop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleRunning, 7)

	candidate, err := st.ReentryAI().EnsureReentryCandidate(&CopyGuardReentryCandidate{
		CycleID: 101, TraderID: "trader-1", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "long", FeatureHash: "stop",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_guard_reentry_candidates SET status='ENTRY_PENDING' WHERE id=?`,
		candidate.ID); err != nil {
		t.Fatal(err)
	}
	preSubmit, reserved, err := st.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-pos", SourceRevision: 1,
		SourceKind: "AI_REENTRY", CanonicalKey: "stop|pre-submit",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long",
	})
	if err != nil || !reserved {
		t.Fatalf("reserve pre-submit: reserved=%v err=%v", reserved, err)
	}
	submitted, reserved, err := st.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-pos", SourceRevision: 2,
		SourceKind: "AI_REENTRY", CanonicalKey: "stop|submitted",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long",
	})
	if err != nil || !reserved {
		t.Fatalf("reserve submitted: reserved=%v err=%v", reserved, err)
	}
	if _, err = st.CopyTrade().PrepareExecutionOrderAttempt(submitted.ID, "submitted-client-id", 0.1); err != nil {
		t.Fatal(err)
	}

	lifecycle, err := st.Trader().BeginStop("user-1", "trader-1")
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.Status != TraderLifecycleStopping || lifecycle.Generation != 8 {
		t.Fatalf("unexpected stop transition: %+v", lifecycle)
	}
	if st.Trader().IsGenerationRunning("trader-1", 7) ||
		st.Trader().IsGenerationRunning("trader-1", lifecycle.Generation) {
		t.Fatal("stopping trader still passed the runtime generation gate")
	}
	candidate, err = st.ReentryAI().GetReentryCandidate(candidate.ID)
	if err != nil || candidate.Status != ReentryCandidatePausedByTrader {
		t.Fatalf("pre-submit entry candidate was not paused by stop: %+v err=%v", candidate, err)
	}
	preSubmit, err = st.CopyTrade().GetExecutionIntentByID(preSubmit.ID)
	if err != nil || preSubmit.Status != ExecutionIntentSkipped ||
		preSubmit.ReasonCode != "TRADER_STOPPED_PRE_SUBMIT" {
		t.Fatalf("safe pre-submit work was not terminalized: %+v err=%v", preSubmit, err)
	}
	submitted, err = st.CopyTrade().GetExecutionIntentByID(submitted.ID)
	if err != nil || submitted.Status != ExecutionIntentSubmitted || submitted.TerminalAt != nil {
		t.Fatalf("submitted work was fabricated as terminal: %+v err=%v", submitted, err)
	}
	blockers, err := st.Trader().GetStopBlockers("trader-1")
	if err != nil || len(blockers) != 1 || blockers[0].ResourceID != "2" {
		t.Fatalf("submitted blocker not reported: %+v err=%v", blockers, err)
	}
	if err = st.Trader().MarkStopReconcileRequired(
		"user-1", "trader-1", lifecycle.Generation, FormatLifecycleBlockers(blockers)); err != nil {
		t.Fatal(err)
	}
	current, err := st.Trader().GetLifecycle("trader-1")
	if err != nil || current.Status != TraderLifecycleStoppingReconcileRequired {
		t.Fatalf("uncertain stop was reported as complete: %+v err=%v", current, err)
	}
}

func TestStopBlockersDistinguishHistoricalTerminalWorkFromActiveRisk(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "stop-blocker-semantics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleStoppingReconcileRequired, 2)

	if _, err = st.DB().Exec(`INSERT INTO copy_trade_position_mappings
		(trader_id,leader_pos_id,leader_id,symbol,side,margin_mode,status,closed_at)
		VALUES('trader-1','closed-leader','leader','BTCUSDT','long','cross','closed',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO copy_trade_execution_intents
			(trader_id,leader_pos_id,source_revision,action,symbol,side,status,submitted_at,filled_at,protected_at,terminal_at,exchange_order_id,exchange_state,filled_quantity)
		 VALUES('trader-1','protected-leader',1,'open_long','BTCUSDT','long','PROTECTED',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,NULL,'protected-order','FILLED',0.1)`,
		`INSERT INTO copy_trade_execution_intents
			(trader_id,leader_pos_id,source_revision,action,symbol,side,status,submitted_at,filled_at,terminal_at,exchange_order_id,exchange_state,filled_quantity)
		 VALUES('trader-1','closed-leader',1,'open_long','BTCUSDT','long','FILLED',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,NULL,'closed-order','FILLED',0.1)`,
		`INSERT INTO copy_trade_execution_intents
			(trader_id,leader_pos_id,source_revision,action,symbol,side,status,submitted_at,filled_at,terminal_at,exchange_order_id,exchange_state,filled_quantity)
		 VALUES('trader-1','active-leader',1,'open_long','ETHUSDT','long','FILLED',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'active-order','FILLED',1)`,
	} {
		if _, err = st.DB().Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	blockers, err := st.Trader().GetStopBlockers("trader-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 1 || blockers[0].Symbol != "ETHUSDT" || blockers[0].Status != ExecutionIntentFilled {
		t.Fatalf("historical terminal work polluted stop blockers: %+v", blockers)
	}
}

func TestStopBlockersIgnorePreparedAttemptUntilSubmissionBoundary(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "stop-prepared-boundary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleStoppingReconcileRequired, 2)
	intent, claimed, err := st.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-pos", SourceRevision: 1,
		CanonicalKey: "stop|prepared", Action: "open_long", Symbol: "SYNUSDT", Side: "long",
		RequestedQuantity: 56, QuantizedQuantity: 56, ClientOrderID: "prepared-only",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve intent: claimed=%v err=%v", claimed, err)
	}
	if _, err = st.CopyTrade().PrepareExecutionOrderAttemptRecordWithKind(
		intent.ID, intent.ClientOrderID, "INITIAL_OPEN", 56, 56,
	); err != nil {
		t.Fatal(err)
	}
	blockers, err := st.Trader().GetStopBlockers("trader-1")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("local PREPARED attempt became exchange-risk blocker: %+v err=%v", blockers, err)
	}
	if _, err = st.CopyTrade().MarkExecutionOrderAttemptSubmitted(intent.ID, intent.ClientOrderID); err != nil {
		t.Fatal(err)
	}
	blockers, err = st.Trader().GetStopBlockers("trader-1")
	if err != nil || len(blockers) != 1 || blockers[0].ResourceID != fmt.Sprint(intent.ID) {
		t.Fatalf("submitted attempt was not reported as blocker: %+v err=%v", blockers, err)
	}
}

func TestExecutionIntentTerminalMigrationBackfillsProtectedAndClosedFilled(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "execution-terminal-migration.db")
	st, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleStopped, 1)
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_position_mappings
		(trader_id,leader_pos_id,leader_id,symbol,side,margin_mode,status,closed_at)
		VALUES('trader-1','closed-leader','leader','BTCUSDT','long','cross','closed',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_execution_intents
		(trader_id,leader_pos_id,source_revision,action,symbol,side,status,submitted_at,filled_at,terminal_at,exchange_state)
		VALUES
		('trader-1','protected-leader',1,'open_long','BTCUSDT','long','PROTECTED',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,NULL,'FILLED'),
		('trader-1','closed-leader',1,'open_long','BTCUSDT','long','FILLED',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,NULL,'FILLED')`); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var terminalCount, protectedCount int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents WHERE terminal_at IS NOT NULL`).Scan(&terminalCount); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents WHERE status='PROTECTED' AND protected_at IS NOT NULL`).Scan(&protectedCount); err != nil {
		t.Fatal(err)
	}
	if terminalCount != 2 || protectedCount != 1 {
		t.Fatalf("terminal migration incomplete: terminal=%d protected=%d", terminalCount, protectedCount)
	}
}

func TestResumeTraderCandidatesRequiresAuthoritativeStoppedByRiskCycle(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "resume-candidates.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleStarting, 8)

	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "stopped-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		Status: CopyGuardStoppedWatching, PolicySnapshot: "{}",
		LeaderEntryPrice: 100, FollowerEntryPrice: 100, FollowerNotional: 100,
		AccountEquity: 1000, LastObservedPrice: 95,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().SavePositionMapping(&CopyTradePositionMapping{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "stopped-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		OpenedAt: time.Now(), OpenPrice: 100, OpenSizeUSD: 100, LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().MarkStoppedByRisk("trader-1", "stopped-pos", -5, 1, 0); err != nil {
		t.Fatal(err)
	}
	candidate, err := st.ReentryAI().EnsureReentryCandidate(&CopyGuardReentryCandidate{
		CycleID: cycle.ID, TraderID: "trader-1", LeaderPosID: "stopped-pos",
		Symbol: "ETHUSDT", Side: "long", FeatureHash: "restart",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_guard_reentry_candidates
		SET status=? WHERE id=?`, ReentryCandidatePausedByTrader, candidate.ID); err != nil {
		t.Fatal(err)
	}

	if err = st.ReentryAI().ResumeTraderCandidatesForStart("trader-1"); err != nil {
		t.Fatal(err)
	}
	candidate, err = st.ReentryAI().GetReentryCandidate(candidate.ID)
	if err != nil || candidate.Status != ReentryCandidateWaiting ||
		candidate.PendingTrigger != "TRADER_RESTARTED" {
		t.Fatalf("authoritative stopped-by-risk candidate was not resumed: %+v err=%v", candidate, err)
	}
}

func TestArchiveBlocksRiskThenSoftDeletesAndPreservesHistory(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleStopped, 3)
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_signal_logs
		(trader_id,leader_id,provider_type,signal_id,symbol,action,position_side,status)
		VALUES('trader-1','leader','okx','audit-1','BTCUSDT','open','long','executed')`); err != nil {
		t.Fatal(err)
	}
	position := &TraderPosition{
		TraderID: "trader-1", ExchangeID: "exchange-1", ExchangeType: "okx",
		Symbol: "BTCUSDT", Side: "LONG", Quantity: 0.1, EntryPrice: 60000,
		EntryTime: time.Now(), Leverage: 2,
	}
	if err = st.Position().Create(position); err != nil {
		t.Fatal(err)
	}
	blockers, err := st.Trader().Archive("user-1", "trader-1")
	if err != nil || len(blockers) != 1 || blockers[0].Code != "OPEN_POSITION" {
		t.Fatalf("unsafe archive was not blocked: %+v err=%v", blockers, err)
	}
	if err = st.Position().ClosePosition(position.ID, 60100, "close-1", 10, 0.1, "exchange_confirmed"); err != nil {
		t.Fatal(err)
	}
	blockers, err = st.Trader().Archive("user-1", "trader-1")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("safe archive failed: %+v err=%v", blockers, err)
	}
	lifecycle, err := st.Trader().GetLifecycle("trader-1")
	if err != nil || lifecycle.Status != TraderLifecycleArchived {
		t.Fatalf("trader was not archived: %+v err=%v", lifecycle, err)
	}
	visible, err := st.Trader().List("user-1")
	if err != nil || len(visible) != 0 {
		t.Fatalf("archived trader remains visible: %+v err=%v", visible, err)
	}
	var traderRows, signalRows, positionRows int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM traders WHERE id='trader-1'`).Scan(&traderRows); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_signal_logs WHERE trader_id='trader-1'`).Scan(&signalRows); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM trader_positions WHERE trader_id='trader-1'`).Scan(&positionRows); err != nil {
		t.Fatal(err)
	}
	if traderRows != 1 || signalRows != 1 || positionRows != 1 {
		t.Fatalf("soft archive lost audit history: trader=%d signal=%d position=%d",
			traderRows, signalRows, positionRows)
	}
}

func TestCompatibilityStatusAndDeleteCannotBypassLifecycleSafety(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "compat-lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleRunning, 4)
	intent, reserved, err := st.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-pos", SourceRevision: 1,
		SourceKind: "AI_REENTRY", CanonicalKey: "compat|submitted",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long",
	})
	if err != nil || !reserved {
		t.Fatalf("reserve submitted intent: reserved=%v err=%v", reserved, err)
	}
	if _, err = st.CopyTrade().PrepareExecutionOrderAttempt(intent.ID, "compat-client-id", 0.1); err != nil {
		t.Fatal(err)
	}

	if err = st.Trader().UpdateStatus("user-1", "trader-1", false); err != nil {
		t.Fatal(err)
	}
	state, err := st.Trader().GetLifecycle("trader-1")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != TraderLifecycleStoppingReconcileRequired || state.IsRunning {
		t.Fatalf("compat status setter fabricated STOPPED despite submitted work: %+v", state)
	}
	if err = st.Trader().Delete("user-1", "trader-1"); !errors.Is(err, ErrTraderLifecycleConflict) {
		t.Fatalf("compat delete reported false success while blocked: %v", err)
	}
}

func TestGetByIDPreservesCompetitionVisibilityConfiguration(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "trader-get-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.Trader().Create(&Trader{
		ID: "trader-1", UserID: "user-1", Name: "trader",
		AIModelID: "model", ExchangeID: "exchange", InitialBalance: 1000,
		ShowInCompetition: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Trader().GetByID("trader-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ShowInCompetition {
		t.Fatal("GetByID silently dropped show_in_competition")
	}
}

func TestOrphanTombstoneInvalidatesOnlySafeActiveWork(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "tombstone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO copy_guard_reentry_candidates
		(cycle_id,trader_id,leader_pos_id,symbol,side,status,feature_hash,next_review_at)
		VALUES(901,'deleted-trader','leader-pos','ETHUSDT','long','WAITING','orphan',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().ReconcileOrphanTombstones(); err != nil {
		t.Fatal(err)
	}
	var status, tombstone string
	if err = st.DB().QueryRow(`SELECT status FROM copy_guard_reentry_candidates WHERE cycle_id=901`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT lifecycle_status FROM trader_tombstones WHERE trader_id='deleted-trader'`).Scan(&tombstone); err != nil {
		t.Fatal(err)
	}
	if status != ReentryCandidateInvalidatedTraderArchived || tombstone != TraderLifecycleArchived {
		t.Fatalf("orphan was not safely tombstoned: candidate=%s tombstone=%s", status, tombstone)
	}
}
