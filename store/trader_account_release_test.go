package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func flatAccountReleaseProof(t *testing.T, st *Store, id string) TraderAccountReleaseProof {
	t.Helper()
	check, err := st.Trader().PrepareAccountRelease("user-1", id)
	if err != nil {
		t.Fatal(err)
	}
	proof := TraderAccountReleaseProof{Check: check}
	for _, account := range check.Accounts {
		proof.Snapshots = append(proof.Snapshots, StoppedTraderFlatSnapshot{
			ExchangeID: account, TraderGeneration: check.Generation, ObservedAt: time.Now(),
			PositionsEmpty: true, OrdersEmpty: true,
		})
	}
	return proof
}

func TestArchivePermanentlyReleasesAccountWithoutRewritingEvidence(t *testing.T) {
	for _, state := range []string{TraderLifecycleStopped, TraderLifecycleArchived, TraderLifecycleStoppingReconcileRequired} {
		t.Run(state, func(t *testing.T) {
			st := resolutionStore(t)
			createLifecycleTestTrader(t, st, "old", state, 12)
			createLifecycleTestTrader(t, st, "new", TraderLifecycleStopped, 4)
			resolutionExec(t, st, `INSERT INTO copy_trade_account_claims(trader_id,exchange_id,generation,exclusive) VALUES('old','exchange-1',10,1)`)
			seedAccountProtection(t, st, "old", accountProtectionFixture{status: "mark_crossed_exit"})
			before, _ := st.CopyTrade().GetCopyGuardProtectiveOrder(1)
			start, err := st.Trader().BeginStart("user-1", "new")
			if err != nil {
				t.Fatal(err)
			}
			if err = st.Trader().CompleteCopyGuardStart("user-1", "new", start.Generation, "exchange-1"); err == nil {
				t.Fatal("incident was not reproduced")
			}
			if err = st.Trader().FailStart("user-1", "new", start.Generation, "blocked"); err != nil {
				t.Fatal(err)
			}
			if b, err := st.Trader().Archive("user-1", "old", flatAccountReleaseProof(t, st, "old")); err != nil || len(b) != 0 {
				t.Fatalf("archive: %v %v", b, err)
			}
			after, _ := st.CopyTrade().GetCopyGuardProtectiveOrder(1)
			if *before != *after {
				t.Fatal("abandoning history invented a venue outcome")
			}
			// Even new historical bookkeeping cannot reacquire account control.
			resolutionExec(t, st, `UPDATE copy_guard_protective_orders SET status='unknown',replacement_pending=1,previous_algo_id='late-history' WHERE cycle_id=1`)
			start, err = st.Trader().BeginStart("user-1", "new")
			if err != nil {
				t.Fatal(err)
			}
			if err = st.Trader().CompleteCopyGuardStart("user-1", "new", start.Generation, "exchange-1"); err != nil {
				t.Fatal(err)
			}
			if err = st.Trader().ClaimRunningExecutionAccount("new", start.Generation, "exchange-1", true); err != nil {
				t.Fatalf("runtime restoration re-blocked: %v", err)
			}
			if _, err = st.Trader().BeginStart("user-1", "old"); !errors.Is(err, ErrTraderArchived) {
				t.Fatal("archived runtime was allowed to restart", err)
			}
			if err = st.Trader().ClaimRunningExecutionAccount("old", 12, "exchange-1", true); !errors.Is(err, ErrTraderLifecycleConflict) {
				t.Fatal("old generation reclaimed the account", err)
			}
			// Idempotence must work even though a new owner is now running.
			if _, err = st.Trader().Archive("user-1", "old"); err != nil {
				t.Fatal(err)
			}
			ids, err := st.Trader().ListUnreleasedArchivedTraders("user-1", "exchange-1")
			if err != nil || len(ids) != 0 {
				t.Fatalf("release will be rechecked on each start: %v %v", ids, err)
			}
		})
	}
}

func TestArchiveProofRejectsRacesAndIncompleteEvidence(t *testing.T) {
	for _, change := range []string{"stale", "future", "position", "order", "wrong-account", "missing-account", "duplicate-account", "generation", "own-start", "other-start", "other-start-stop", "new-owner", "config-change", "claim-change", "credentials", "wrong-user"} {
		t.Run(change, func(t *testing.T) {
			st := resolutionStore(t)
			createLifecycleTestTrader(t, st, "old", TraderLifecycleStopped, 3)
			createLifecycleTestTrader(t, st, "other", TraderLifecycleStopped, 2)
			resolutionExec(t, st, `INSERT INTO exchanges(id,user_id,name,type,exchange_type,api_key) VALUES('exchange-1','user-1','test','cex','okx','before')`)
			resolutionExec(t, st, `INSERT INTO copy_trade_account_claims(trader_id,exchange_id,generation) VALUES('old','original-account',1)`)
			proof := flatAccountReleaseProof(t, st, "old")
			user := "user-1"
			switch change {
			case "stale":
				proof.Snapshots[0].ObservedAt = time.Now().Add(-2 * time.Minute)
			case "future":
				proof.Snapshots[0].ObservedAt = time.Now().Add(time.Minute)
			case "position":
				proof.Snapshots[0].PositionsEmpty = false
			case "order":
				proof.Snapshots[0].OrdersEmpty = false
			case "wrong-account":
				proof.Snapshots[0].ExchangeID = "foreign-account"
			case "missing-account":
				proof.Snapshots = proof.Snapshots[:1]
			case "duplicate-account":
				proof.Snapshots[1] = proof.Snapshots[0]
			case "generation":
				proof.Snapshots[0].TraderGeneration++
			case "own-start":
				_, _ = st.Trader().BeginStart(user, "old")
			case "other-start":
				_, _ = st.Trader().BeginStart(user, "other")
			case "other-start-stop":
				state, err := st.Trader().BeginStart(user, "other")
				if err != nil {
					t.Fatal(err)
				}
				if err = st.Trader().FailStart(user, "other", state.Generation, "ABA"); err != nil {
					t.Fatal(err)
				}
			case "new-owner":
				createLifecycleTestTrader(t, st, "new", TraderLifecycleStopped, 0)
			case "config-change":
				resolutionExec(t, st, `UPDATE traders SET exchange_id='another' WHERE id='old'`)
			case "claim-change":
				resolutionExec(t, st, `UPDATE copy_trade_account_claims SET generation=99 WHERE trader_id='old'`)
			case "credentials":
				resolutionExec(t, st, `UPDATE exchanges SET api_key='after' WHERE id='exchange-1'`)
			case "wrong-user":
				user = "outsider"
			}
			if _, err := st.Trader().Archive(user, "old", proof); err == nil {
				t.Fatal("invalid proof accepted")
			}
			var releases int
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM trader_account_releases`).Scan(&releases); err != nil || releases != 0 {
				t.Fatalf("failed archive left a release: %d %v", releases, err)
			}
		})
	}
}

func TestArchiveAndReleaseAreAtomicAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	createLifecycleTestTrader(t, st, "old", TraderLifecycleStopped, 3)
	resolutionExec(t, st, `INSERT INTO copy_trade_account_claims(trader_id,exchange_id,generation) VALUES('old','original',2)`)
	proof := flatAccountReleaseProof(t, st, "old")
	resolutionExec(t, st, `CREATE TRIGGER fail_release BEFORE INSERT ON trader_account_releases WHEN NEW.exchange_id='original' BEGIN SELECT RAISE(ABORT,'injected release failure'); END`)
	if _, err = st.Trader().Archive("user-1", "old", proof); err == nil {
		t.Fatal("injected failure ignored")
	}
	state, _ := st.Trader().GetLifecycle("old")
	if state.Status != TraderLifecycleStopped || state.Generation != 3 {
		t.Fatalf("partial archive: %+v", state)
	}
	var n int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM trader_account_releases`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial release %d: %v", n, err)
	}
	resolutionExec(t, st, `DROP TRIGGER fail_release`)
	if _, err = st.Trader().Archive("user-1", "old", proof); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := st.Trader().AccountReleaseComplete("user-1", "old")
	if err != nil || !complete {
		t.Fatalf("release lost on restart: %v %v", complete, err)
	}
	// A release for another generation is not an exemption.
	resolutionExec(t, st, `UPDATE traders SET lifecycle_generation=lifecycle_generation+1 WHERE id='old'`)
	complete, err = st.Trader().AccountReleaseComplete("user-1", "old")
	if err != nil || complete {
		t.Fatalf("generation mismatch accepted: %v %v", complete, err)
	}
}

func TestArchiveCancelsEntryAuthorizationWithoutRewritingSubmittedOrders(t *testing.T) {
	st := resolutionStore(t)
	createLifecycleTestTrader(t, st, "old", TraderLifecycleStopped, 3)
	reserve := func(pos string) *CopyTradeExecutionIntent {
		t.Helper()
		i, _, err := st.CopyTrade().ReserveExecutionIntent(&CopyTradeExecutionIntent{
			TraderID: "old", LeaderPosID: pos, SourceRevision: 1, SourceKind: "LEADER_TRANSITION",
			CanonicalKey: pos, Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		})
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	unsent, sent := reserve("unsent"), reserve("sent")
	resolutionExec(t, st, `UPDATE copy_trade_execution_intents SET status='SUBMITTED',submitted_at=CURRENT_TIMESTAMP,exchange_order_id='unknown-order' WHERE id=?`, sent.ID)
	sent, _ = st.CopyTrade().GetExecutionIntentByID(sent.ID)
	resolutionExec(t, st, `INSERT INTO copy_trade_current_position_requests(request_id,trader_id,provider,leader_id,source_generation,exchange_id,saved_generation,status)
 VALUES('batch','old','okx','leader',1,'exchange-1',3,'SEALED')`)
	for _, i := range []*CopyTradeExecutionIntent{unsent, sent} {
		resolutionExec(t, st, `INSERT INTO copy_trade_current_position_tasks(request_id,trader_id,leader_pos_id,symbol,side,margin_mode,opened_ms,source_size,entry_price,reference_price,notional,leverage,status,intent_id)
 VALUES('batch','old',?,'ETHUSDT','long','cross',0,1,100,100,100,1,'READY',?)`, i.LeaderPosID, i.ID)
	}
	if _, err := st.Trader().Archive("user-1", "old", flatAccountReleaseProof(t, st, "old")); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := st.DB().QueryRow(`SELECT status FROM copy_trade_current_position_requests WHERE request_id='batch'`).Scan(&status); err != nil || status != "CANCELLED" {
		t.Fatalf("entry authorization survived archive: %s %v", status, err)
	}
	unsent, err := st.CopyTrade().GetExecutionIntentByID(unsent.ID)
	if err != nil || unsent.Status != ExecutionIntentSkipped {
		t.Fatalf("unsent order survived archive: %+v %v", unsent, err)
	}
	after, err := st.CopyTrade().GetExecutionIntentByID(sent.ID)
	if err != nil || !reflect.DeepEqual(after, sent) {
		t.Fatalf("submitted history rewritten: %+v %v", after, err)
	}
	if st.Trader().IsGenerationRunning("old", 3) {
		t.Fatal("deleted generation can execute")
	}
}
