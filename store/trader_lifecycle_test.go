package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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

func TestCopyGuardStartEnforcesExecutionAccountExclusivity(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-account-exclusive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "guard-a", TraderLifecycleRunning, 1)
	createLifecycleTestTrader(t, st, "guard-b", TraderLifecycleStarting, 2)
	for _, traderID := range []string{"guard-a", "guard-b"} {
		cfg := NewCopyGuardDefaults()
		cfg.TraderID = traderID
		cfg.ProviderType = "okx"
		cfg.LeaderID = "leader-" + traderID
		cfg.Enabled = true
		cfg.RiskPolicyVersion = 4
		if err = st.CopyTrade().Upsert(cfg); err != nil {
			t.Fatal(err)
		}
	}

	conflict, err := st.Trader().FindRunningCopyGuardConflict("guard-b", "exchange-1")
	if err != nil || conflict != "guard-a" {
		t.Fatalf("running account owner not found: conflict=%q err=%v", conflict, err)
	}
	if err = st.Trader().CompleteCopyGuardStart("user-1", "guard-b", 2, "exchange-1"); err == nil {
		t.Fatal("second Copy Guard program started on the same execution account")
	}
	lifecycle, err := st.Trader().GetLifecycle("guard-b")
	if err != nil || lifecycle.Status != TraderLifecycleStarting || lifecycle.IsRunning {
		t.Fatalf("conflicted start mutated lifecycle: %+v err=%v", lifecycle, err)
	}
	if _, err = st.DB().Exec("UPDATE traders SET lifecycle_status=?,is_running=0 WHERE id='guard-a'", TraderLifecycleStopped); err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().CompleteCopyGuardStart("user-1", "guard-b", 2, "exchange-1"); err != nil {
		t.Fatalf("account was not released after the first program stopped: %v", err)
	}
	lifecycle, err = st.Trader().GetLifecycle("guard-b")
	if err != nil || lifecycle.Status != TraderLifecycleRunning || !lifecycle.IsRunning {
		t.Fatalf("exclusive start did not commit: %+v err=%v", lifecycle, err)
	}

	// Account ownership is bidirectional: while guard-b owns the account, a
	// plain AI program cannot start beside it.
	createLifecycleTestTrader(t, st, "plain-c", TraderLifecycleStarting, 3)
	if err = st.Trader().CompleteStart("user-1", "plain-c", 3); err == nil {
		t.Fatal("plain trader started beside an active Copy Guard owner")
	}
	if _, err = st.DB().Exec("UPDATE traders SET lifecycle_status=?,is_running=0 WHERE id='guard-b'", TraderLifecycleStopped); err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().CompleteStart("user-1", "plain-c", 3); err != nil {
		t.Fatalf("plain trader did not start after Copy Guard released account: %v", err)
	}

	// Conversely, a Copy Guard candidate cannot claim an account already used
	// by a plain running program.
	createLifecycleTestTrader(t, st, "guard-d", TraderLifecycleStarting, 4)
	guardD := NewCopyGuardDefaults()
	guardD.TraderID, guardD.ProviderType, guardD.LeaderID = "guard-d", "binance", "leader-d"
	guardD.Enabled = true
	if err = st.CopyTrade().Upsert(guardD); err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().CompleteCopyGuardStart("user-1", "guard-d", 4, "exchange-1"); err == nil {
		t.Fatal("Copy Guard claimed an account with a plain running trader")
	}

	// Ordinary programs retain their prior behavior when no Copy Guard owns the
	// account; this feature does not globally serialize all traders.
	createLifecycleTestTrader(t, st, "plain-e", TraderLifecycleStarting, 5)
	if err = st.Trader().CompleteStart("user-1", "plain-e", 5); err != nil {
		t.Fatalf("plain traders were made globally exclusive: %v", err)
	}
}

func TestConcurrentCopyGuardStartsCommitOnlyOneAccountOwner(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-concurrent-owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, traderID := range []string{"guard-a", "guard-b"} {
		createLifecycleTestTrader(t, st, traderID, TraderLifecycleStarting, int64(i+1))
		cfg := NewCopyGuardDefaults()
		cfg.TraderID, cfg.ProviderType, cfg.LeaderID = traderID, "okx", "leader-"+traderID
		cfg.Enabled = true
		if err = st.CopyTrade().Upsert(cfg); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, traderID := range []string{"guard-a", "guard-b"} {
		wg.Add(1)
		go func(id string, generation int64) {
			defer wg.Done()
			<-start
			results <- st.Trader().CompleteCopyGuardStart("user-1", id, generation, "exchange-1")
		}(traderID, int64(i+1))
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded := 0
	for callErr := range results {
		if callErr == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent Copy Guard starts committed %d owners, want exactly one", succeeded)
	}
	count, err := st.Trader().CountRunningByExchange("exchange-1")
	if err != nil || count != 1 {
		t.Fatalf("running account owners=%d err=%v, want 1", count, err)
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
	// All three intents are settled: their exchange outcome is known, so none of
	// them is unfinished reconciliation work that could hold up the stop.
	blockers, err := st.Trader().GetStopBlockers("trader-1")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("settled fills polluted stop blockers: %+v err=%v", blockers, err)
	}
	// The unprotected one is still archive risk, and the same discrimination
	// applies: PROTECTED bookkeeping and a closed mapping are both exemptions.
	unprotected := filterLifecycleBlockers(t, st, "trader-1", "UNPROTECTED_FILL_UNRESOLVED")
	if len(unprotected) != 1 || unprotected[0].Symbol != "ETHUSDT" ||
		unprotected[0].Status != ExecutionIntentFilled {
		t.Fatalf("historical terminal work polluted archive blockers: %+v", unprotected)
	}
}

// filterLifecycleBlockers returns the archive blockers carrying the given code.
// GetArchiveBlockers intentionally aggregates several independent risk sources,
// so assertions must name the one under test instead of counting the total.
func filterLifecycleBlockers(t *testing.T, st *Store, traderID, code string) []TraderLifecycleBlocker {
	t.Helper()
	all, err := st.Trader().GetArchiveBlockers(traderID)
	if err != nil {
		t.Fatal(err)
	}
	var matched []TraderLifecycleBlocker
	for _, blocker := range all {
		if blocker.Code == code {
			matched = append(matched, blocker)
		}
	}
	return matched
}

// TestArchiveBlockersTrustVerifiedCycleProtectionOverIntentBookkeeping covers the
// deadlock found in production: protected_at is only written by the
// FILLED->PROTECTED transition, so protection established on a later retry leaves
// it NULL, and once the trader stops nothing can backfill it. The position was
// fully protected (VERIFIED, coverage 1.0) yet counted as naked risk. The check
// now gates archive rather than the stop, but must keep the same discrimination.
func TestArchiveBlockersTrustVerifiedCycleProtectionOverIntentBookkeeping(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "stop-blocker-verified-protection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleStoppingReconcileRequired, 2)

	if _, err = st.DB().Exec(`INSERT INTO copy_guard_cycles
		(id,trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot,
		 protection_status,protection_coverage)
		VALUES(501,'trader-1','leader','active-leader','ETHUSDT','long','cross','FOLLOWING','{}',
		       'VERIFIED',1.0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_execution_intents
		(trader_id,leader_pos_id,source_revision,cycle_id,action,symbol,side,status,
		 submitted_at,filled_at,protected_at,terminal_at,exchange_order_id,exchange_state,filled_quantity)
		VALUES('trader-1','active-leader',1,501,'open_long','ETHUSDT','long','FILLED',
		       CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,NULL,CURRENT_TIMESTAMP,'active-order','FILLED',1)`); err != nil {
		t.Fatal(err)
	}

	if blockers := filterLifecycleBlockers(t, st, "trader-1", "UNPROTECTED_FILL_UNRESOLVED"); len(blockers) != 0 {
		t.Fatalf("verified protection still reported as naked risk: %+v", blockers)
	}

	// The check must not be weakened: partial coverage is real naked risk again.
	if _, err = st.DB().Exec(`UPDATE copy_guard_cycles SET protection_coverage=0.5 WHERE id=501`); err != nil {
		t.Fatal(err)
	}
	blockers := filterLifecycleBlockers(t, st, "trader-1", "UNPROTECTED_FILL_UNRESOLVED")
	if len(blockers) != 1 || blockers[0].Symbol != "ETHUSDT" {
		t.Fatalf("partially covered position must still block the archive: %+v", blockers)
	}

	if _, err = st.DB().Exec(`UPDATE copy_guard_cycles
		SET protection_coverage=1.0,protection_status='DEGRADED' WHERE id=501`); err != nil {
		t.Fatal(err)
	}
	if blockers = filterLifecycleBlockers(t, st, "trader-1", "UNPROTECTED_FILL_UNRESOLVED"); len(blockers) != 1 {
		t.Fatalf("degraded protection must still block the archive: %+v", blockers)
	}
	// Whatever the protection verdict, a settled fill never holds up the stop.
	if stopBlockers, stopErr := st.Trader().GetStopBlockers("trader-1"); stopErr != nil || len(stopBlockers) != 0 {
		t.Fatalf("unprotected fill blocked the stop: %+v err=%v", stopBlockers, stopErr)
	}
}

// TestUnprotectedFillWithoutCopyGuardCycleNeverFreezesLifecycle reproduces the
// production freeze on trader zhu-exz00. Copy Guard was never active for it, so
// refreshStopLossAfterExecute returned early: no cycle was created (cycle_id=0)
// and protected_at was never written. The old FILLED-unprotected stop blocker
// then had no reachable exemption — both cycle exemptions join
// copy_guard_cycles.id=i.cycle_id and no row can have id 0, while the mapping
// exemption needs the stopped engine to close the mapping — so stop, start and
// archive all died at once on a trader that never opted into Copy Guard.
func TestUnprotectedFillWithoutCopyGuardCycleNeverFreezesLifecycle(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "unprotected-without-cycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleStoppingReconcileRequired, 1)

	if _, err = st.DB().Exec(`INSERT INTO copy_trade_position_mappings
		(trader_id,leader_pos_id,leader_id,symbol,side,margin_mode,status)
		VALUES('trader-1','active-leader','leader','BTCUSDT','long','cross','active')`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_execution_intents
		(trader_id,leader_pos_id,source_revision,cycle_id,action,symbol,side,status,
		 submitted_at,filled_at,protected_at,terminal_at,exchange_order_id,exchange_state,filled_quantity)
		VALUES('trader-1','active-leader',1,0,'open_long','BTCUSDT','long','FILLED',
		       CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,NULL,CURRENT_TIMESTAMP,'real-order','FILLED',0.0867)`); err != nil {
		t.Fatal(err)
	}

	blockers, err := st.Trader().GetStopBlockers("trader-1")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("settled fill without a Copy Guard cycle froze the stop: %+v err=%v", blockers, err)
	}
	if err = st.Trader().CompleteStop("user-1", "trader-1", 1); err != nil {
		t.Fatalf("stop could not be completed: %v", err)
	}
	current, err := st.Trader().GetLifecycle("trader-1")
	if err != nil || current.Status != TraderLifecycleStopped {
		t.Fatalf("trader did not reach STOPPED: %+v err=%v", current, err)
	}
	// The naked position is not forgotten: archive still refuses it, and the
	// active ownership mapping is reported alongside as a second, independent
	// reason so a data gap in one source cannot silently open the archive gate.
	unprotected := filterLifecycleBlockers(t, st, "trader-1", "UNPROTECTED_FILL_UNRESOLVED")
	if len(unprotected) != 1 || unprotected[0].Symbol != "BTCUSDT" {
		t.Fatalf("unprotected fill was lost from archive blockers: %+v", unprotected)
	}
	if mappings := filterLifecycleBlockers(t, st, "trader-1", "OWNERSHIP_MAPPING_ACTIVE"); len(mappings) != 1 {
		t.Fatalf("active ownership mapping was lost from archive blockers: %+v", mappings)
	}
}

// TestBeginStartEscapesStoppingStates pins the second half of that freeze: with
// start gated on STOPPED, a single unclearable stop blocker left the operator no
// action at all. Startup recovery is what resolves unsettled work, so the
// stopping states must be startable; archived ones still must not be.
func TestBeginStartEscapesStoppingStates(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "start-escape.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, from := range []string{
		TraderLifecycleStopped,
		TraderLifecycleStopping,
		TraderLifecycleStoppingReconcileRequired,
	} {
		traderID := "trader-" + from
		createLifecycleTestTrader(t, st, traderID, from, 3)
		lifecycle, startErr := st.Trader().BeginStart("user-1", traderID)
		if startErr != nil {
			t.Fatalf("start from %s was rejected: %v", from, startErr)
		}
		if lifecycle.Status != TraderLifecycleStarting || lifecycle.Generation != 4 {
			t.Fatalf("start from %s did not fence the previous generation: %+v", from, lifecycle)
		}
	}
	for _, from := range []string{TraderLifecycleArchived, TraderLifecycleArchiving} {
		traderID := "trader-" + from
		createLifecycleTestTrader(t, st, traderID, from, 1)
		if _, startErr := st.Trader().BeginStart("user-1", traderID); !errors.Is(startErr, ErrTraderArchived) {
			t.Fatalf("archived trader became startable from %s: %v", from, startErr)
		}
	}
}

// TestStopBlockersIgnoreReconcilingIntentThatNeverReachedExchange pins the
// evidence requirement for RECONCILING. An intent can be moved to RECONCILING
// before it is ever sent to the exchange; treating that as unsettled risk left
// traders unable to ever finish stopping.
func TestStopBlockersIgnoreReconcilingIntentThatNeverReachedExchange(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "stop-blocker-reconciling-evidence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createLifecycleTestTrader(t, st, "trader-1", TraderLifecycleStoppingReconcileRequired, 2)

	if _, err = st.DB().Exec(`INSERT INTO copy_trade_execution_intents
		(trader_id,leader_pos_id,source_revision,action,symbol,side,status,filled_quantity)
		VALUES('trader-1','never-sent',1,'open_long','SOLUSDT','long','RECONCILING',0)`); err != nil {
		t.Fatal(err)
	}
	blockers, err := st.Trader().GetStopBlockers("trader-1")
	if err != nil || len(blockers) != 0 {
		t.Fatalf("intent that never reached the exchange became a blocker: %+v err=%v", blockers, err)
	}

	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents
		SET exchange_order_id='real-order' WHERE trader_id='trader-1'`); err != nil {
		t.Fatal(err)
	}
	blockers, err = st.Trader().GetStopBlockers("trader-1")
	if err != nil || len(blockers) != 1 || blockers[0].Symbol != "SOLUSDT" {
		t.Fatalf("reconciling intent with exchange evidence must block: %+v err=%v", blockers, err)
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
