package copytrade

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

type incompleteV4Executor struct{}

func (incompleteV4Executor) ExecuteDecision(*decision.Decision) error { return nil }
func (incompleteV4Executor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
func (incompleteV4Executor) GetPositions() ([]map[string]interface{}, error) { return nil, nil }

type accountingV4Executor struct {
	status  string
	records []trader.ClosedPnLRecord
}

func (e *accountingV4Executor) ExecuteDecision(*decision.Decision) error { return nil }
func (e *accountingV4Executor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
func (e *accountingV4Executor) GetPositions() ([]map[string]interface{}, error) { return nil, nil }
func (e *accountingV4Executor) GetOrderStatus(string, string) (map[string]interface{}, error) {
	return map[string]interface{}{"status": e.status, "avgPrice": 1746.73}, nil
}
func (e *accountingV4Executor) GetClosedPnL(time.Time, int) ([]trader.ClosedPnLRecord, error) {
	return e.records, nil
}

func TestProtectionCoverageAndRetrySchedule(t *testing.T) {
	if got := protectionCoverage(8, 10); got != 0.8 {
		t.Fatalf("under coverage = %v", got)
	}
	if got := protectionRatio(12, 10); got != 1.2 {
		t.Fatalf("over coverage ratio = %v", got)
	}
	if got := protectionCoverage(12, 10); got != 1 {
		t.Fatalf("display coverage must cap at 100%%, got %v", got)
	}
	want := []time.Duration{0, time.Second, 3 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute}
	for i, expected := range want {
		if got := protectionRetryDelay(i); got != expected {
			t.Fatalf("retry %d = %v, want %v", i, got, expected)
		}
	}
}

func TestValidateV4ExecutorCapabilitiesRejectsSilentDowngrade(t *testing.T) {
	if err := validateV4ExecutorCapabilities(incompleteV4Executor{}); err == nil {
		t.Fatal("v4 must reject an executor that hides protective stop and accounting capabilities")
	}
}

func TestV4AccountingWaitsForTerminalFillThenReconciles(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "accounting.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}", FollowerEntryPrice: 1735.19, FollowerNotional: 286.99})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 1735.19, 286.99, 0.165, 8); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().UpdateCopyGuardFollowerPosition(cycle.ID, "follower-pos", 1735.19, 286.99); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().BeginCopyGuardAccounting(cycle.ID, store.CopyGuardLeaderClosed, "exit-order", 1.986091); err != nil {
		t.Fatal(err)
	}
	executor := &accountingV4Executor{status: "NEW"}
	ti := NewTraderIntegration("trader-1", executor, st)
	pending, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	ti.reconcileV4CycleAccounting(pending)
	pending, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if pending.AccountingStatus != store.CopyGuardAccountingPending || pending.ActualPnL != 0 {
		t.Fatalf("non-terminal acknowledgement was settled: %+v", pending)
	}
	executor.status = "FILLED"
	executor.records = []trader.ClosedPnLRecord{{Symbol: "ETHUSDT", Side: "long", ExchangeID: "follower-pos", ExitPrice: 1746.73, RealizedPnL: 1.61, Fee: 0.29, ExitTime: time.Now()}}
	ti.reconcileV4CycleAccounting(pending)
	settled, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if settled.AccountingStatus != store.CopyGuardAccountingReconciled || settled.ActualPnL != 1.61 || settled.NetGuardEffect != 0 {
		t.Fatalf("terminal fill was not reconciled: %+v", settled)
	}
}
