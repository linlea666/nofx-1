package manager

import (
	"path/filepath"
	"testing"

	"nofx/store"
)

func TestRejectedAccountRestoreCannotAdvertiseRunningOrEraseVenueObligations(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status,lifecycle_generation) VALUES('a','owner','a','','account',1000,1,'RUNNING',1),('b','owner','b','','account',1000,1,'RUNNING',1)`); err != nil {
		t.Fatal(err)
	}
	i, _, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{TraderID: "b", LeaderPosID: "p", SourceRevision: 1, SourceKind: "LEADER_TRANSITION", CanonicalKey: "restore-b", Action: "open_long", Symbol: "ETHUSDT", Side: "long"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents SET status='RECONCILING',submitted_at=CURRENT_TIMESTAMP,exchange_order_id='unknown' WHERE id=?`, i.ID); err != nil {
		t.Fatal(err)
	}
	cfg, err := st.Trader().GetByID("b")
	if err != nil {
		t.Fatal(err)
	}
	if err = claimRestoredExecutionAccount(st, cfg, true); err == nil {
		t.Fatal("conflicting account was restored")
	}
	got, err := st.Trader().GetByID("b")
	if err != nil {
		t.Fatal(err)
	}
	if got.IsRunning || got.LifecycleStatus != store.TraderLifecycleStoppingReconcileRequired {
		t.Fatalf("false RUNNING or lost obligations: %+v", got)
	}
	intent, err := st.CopyTrade().GetExecutionIntentByID(i.ID)
	if err != nil || intent.Status != store.ExecutionIntentReconciling {
		t.Fatalf("venue uncertainty erased: %+v %v", intent, err)
	}
	issues, err := st.CopyTrade().ListRuntimeIssues("b")
	if err != nil || len(issues) != 1 || issues[0].Code != "EXECUTION_ACCOUNT_RESTORE_BLOCKED" {
		t.Fatalf("missing rejection diagnostic: %+v %v", issues, err)
	}
}
