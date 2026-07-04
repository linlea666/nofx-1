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

// TestV4AccountingIgnoresPriorAttemptRecord 复现实盘 cycle 15 的双计事故：
// OKX 净持仓模式下 posId 跨 attempt 复用，领航员平仓 1 秒后新平仓记录尚未生成，
// 对账若以周期起点做时间下限会误匹配 attempt 0 的止损平仓记录，导致同一笔亏损
// 计两次。修复后：只有晚于当前 attempt 开仓时间且贴近周期关闭时间的记录才可匹配。
func TestV4AccountingIgnoresPriorAttemptRecord(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "accounting2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "short", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}", FollowerEntryPrice: 1745.45, FollowerNotional: 143.13})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().UpdateCopyGuardFollowerPosition(cycle.ID, "follower-pos", 1745.45, 143.13); err != nil {
		t.Fatal(err)
	}
	// attempt 0 被止损（其平仓记录与最终平仓共用同一个 posId）
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 1745.45, 143.13, 0.082, 13.7); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().RecordCopyGuardStop(cycle, 14.9, 1765.88, -1.82, 0.14, 0.01, "algo-0", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	// 重入 attempt 1，随后领航员平仓
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err := st.CopyTrade().RecordCopyGuardReentryFilled(cycle, 1761.94, 1150.55, 0.653, 14.9, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	// 回拨时间线：周期 3 小时前开、attempt 0 也是 3 小时前开——旧记录（2 小时前）
	// 落在"周期起点之后、attempt 1 开仓之前"，正是实盘误匹配的窗口
	backdate := time.Now().Add(-3 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	if _, err := st.DB().Exec(`UPDATE copy_guard_cycles SET opened_at=? WHERE id=?`, backdate, cycle.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE copy_guard_attempts SET opened_at=? WHERE cycle_id=? AND attempt_no=0`, backdate, cycle.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().BeginCopyGuardAccounting(cycle.ID, store.CopyGuardLeaderClosed, "exit-order", -0.54); err != nil {
		t.Fatal(err)
	}
	// OKX 只回了 attempt 0 的旧止损记录（新记录还没生成）：不得据此对账
	oldStop := trader.ClosedPnLRecord{Symbol: "ETHUSDT", Side: "short", ExchangeID: "follower-pos", ExitPrice: 1765.88, RealizedPnL: -1.82, Fee: 0.14, ExitTime: time.Now().Add(-2 * time.Hour)}
	executor := &accountingV4Executor{status: "FILLED", records: []trader.ClosedPnLRecord{oldStop}}
	ti := NewTraderIntegration("trader-1", executor, st)
	pending, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	ti.reconcileV4CycleAccounting(pending)
	pending, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if pending.AccountingStatus == store.CopyGuardAccountingReconciled {
		t.Fatalf("stale prior-attempt record must not settle the accounting: %+v", pending)
	}
	// 新记录生成后正常对账，attempt 1 记的是新记录的盈亏
	executor.records = append(executor.records, trader.ClosedPnLRecord{Symbol: "ETHUSDT", Side: "short", ExchangeID: "follower-pos", ExitPrice: 1762.1, RealizedPnL: -0.5, Fee: 0.29, ExitTime: time.Now()})
	ti.reconcileV4CycleAccounting(pending)
	settled, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if settled.AccountingStatus != store.CopyGuardAccountingReconciled {
		t.Fatalf("fresh record must reconcile the accounting: %+v", settled)
	}
	attempts, err := st.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts: %v %v", attempts, err)
	}
	for _, a := range attempts {
		if a.AttemptNo == 1 && a.PnL != -0.5 {
			t.Fatalf("attempt 1 must settle with the fresh record's pnl (-0.5), got %+v", a)
		}
	}
}
