package store

import (
	"path/filepath"
	"testing"
)

// newManualTestCycle 建一个处于观察态的周期，供人工重入信号测试挂靠
func newManualTestCycle(t *testing.T, cs *CopyTradeStore, traderID, posID string) *CopyGuardCycle {
	t.Helper()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: traderID, LeaderID: "leader", LeaderPosID: posID, Symbol: "BTCUSDT",
		Side: "long", MarginMode: "cross", Status: CopyGuardAttemptsExhausted, PolicySnapshot: "{}",
		LeaderEntryPrice: 100, FollowerEntryPrice: 101, FollowerNotional: 1000, AccountEquity: 5000, LastObservedPrice: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cycle
}

func sampleSignal(cycle *CopyGuardCycle, traderID string) *CopyGuardManualReentrySignal {
	return &CopyGuardManualReentrySignal{
		CycleID: cycle.ID, TraderID: traderID, LeaderPosID: cycle.LeaderPosID,
		Symbol: cycle.Symbol, Side: cycle.Side, MarginMode: cycle.MarginMode,
		TriggerPrice: 98.5, ATR: 1.2, DistanceATRRatio: 0.6, ReentryBoundary: 99.0,
		RecommendedNotional: 500, StopCount: 2, ReentryCount: 2, LeaderSize: 3.0,
		LeaderEntryPrice: 100, Protectable: true, Reason: "test signal",
	}
}

// TestManualReentrySignalLifecycle 覆盖信号完整生命周期：
// Save 去重 → List → Claim 幂等抢占 → MarkOutcome → Count
func TestManualReentrySignalLifecycle(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "manual-reentry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle := newManualTestCycle(t, cs, "trader-1", "pos-1")

	// 1) 首次落库 → PENDING
	sig, err := cs.SaveManualReentrySignal(sampleSignal(cycle, "trader-1"))
	if err != nil {
		t.Fatal(err)
	}
	if sig.Status != ManualReentryStatusPending {
		t.Fatalf("new signal status = %s, want PENDING", sig.Status)
	}
	if sig.ID == 0 || sig.CreatedAt.IsZero() {
		t.Fatalf("signal not persisted properly: %+v", sig)
	}

	// 2) 同周期再次 Save → 去重刷新（不新增行），保留 id/created_at
	refreshed := sampleSignal(cycle, "trader-1")
	refreshed.TriggerPrice = 97.0
	refreshed.RecommendedNotional = 600
	sig2, err := cs.SaveManualReentrySignal(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if sig2.ID != sig.ID {
		t.Fatalf("dedup failed: new id %d != %d", sig2.ID, sig.ID)
	}
	if sig2.TriggerPrice != 97.0 || sig2.RecommendedNotional != 600 {
		t.Fatalf("snapshot not refreshed: %+v", sig2)
	}
	if n, _ := cs.CountManualReentrySignalsForCycle(cycle.ID); n != 1 {
		t.Fatalf("dedup should keep 1 row, got %d", n)
	}

	// 3) List（PENDING 过滤）
	list, err := cs.ListManualReentrySignals([]string{"trader-1"}, []string{ManualReentryStatusPending}, 100)
	if err != nil || len(list) != 1 {
		t.Fatalf("list PENDING = %d rows, err=%v", len(list), err)
	}

	// 4) Claim 幂等抢占：第一次成功，第二次失败
	ok, err := cs.ClaimManualReentrySignal(sig.ID, "user-1", 96.5)
	if err != nil || !ok {
		t.Fatalf("first claim ok=%v err=%v", ok, err)
	}
	claimed, _ := cs.GetManualReentrySignal(sig.ID)
	if claimed.Status != ManualReentryStatusExecuting || claimed.Operator != "user-1" || claimed.ConfirmPrice != 96.5 {
		t.Fatalf("claim did not set executing fields: %+v", claimed)
	}
	if claimed.ConfirmedAt == nil {
		t.Fatal("confirmed_at not set on claim")
	}
	ok2, err := cs.ClaimManualReentrySignal(sig.ID, "user-2", 96.0)
	if err != nil || ok2 {
		t.Fatalf("second claim should fail (idempotent): ok=%v err=%v", ok2, err)
	}

	// 5) GetExecuting 定位 → MarkOutcome EXECUTED
	exec, err := cs.GetExecutingManualReentrySignalByCycle(cycle.ID)
	if err != nil || exec == nil || exec.ID != sig.ID {
		t.Fatalf("GetExecuting mismatch: %+v err=%v", exec, err)
	}
	if err := cs.MarkManualReentrySignalOutcome(sig.ID, ManualReentryStatusExecuted, ""); err != nil {
		t.Fatal(err)
	}
	done, _ := cs.GetManualReentrySignal(sig.ID)
	if done.Status != ManualReentryStatusExecuted || done.ExecutedAt == nil {
		t.Fatalf("outcome not marked executed: %+v", done)
	}
}

// TestManualReentrySignalDismissAndRelease 覆盖忽略与抢占回退
func TestManualReentrySignalDismissAndRelease(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "manual-dismiss.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle := newManualTestCycle(t, cs, "trader-1", "pos-1")

	sig, err := cs.SaveManualReentrySignal(sampleSignal(cycle, "trader-1"))
	if err != nil {
		t.Fatal(err)
	}

	// Dismiss PENDING → DISMISSED
	ok, err := cs.DismissManualReentrySignal(sig.ID, "user-1")
	if err != nil || !ok {
		t.Fatalf("dismiss ok=%v err=%v", ok, err)
	}
	// 已非 PENDING → 再次 dismiss 返回 false（幂等）
	ok2, _ := cs.DismissManualReentrySignal(sig.ID, "user-1")
	if ok2 {
		t.Fatal("dismiss on non-pending should return false")
	}
	// 已 DISMISSED → Claim 应失败
	claimed, _ := cs.ClaimManualReentrySignal(sig.ID, "user-1", 1)
	if claimed {
		t.Fatal("claim on dismissed signal must fail")
	}

	// Release 回退：新信号 Claim 后回退 PENDING，可再次确认
	sig2, err := cs.SaveManualReentrySignal(sampleSignal(cycle, "trader-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ClaimManualReentrySignal(sig2.ID, "user-1", 96); err != nil {
		t.Fatal(err)
	}
	if err := cs.ReleaseManualReentrySignal(sig2.ID, ManualReentryStatusPending, "channel busy"); err != nil {
		t.Fatal(err)
	}
	back, _ := cs.GetManualReentrySignal(sig2.ID)
	if back.Status != ManualReentryStatusPending || back.Error != "channel busy" {
		t.Fatalf("release to pending failed: %+v", back)
	}
	okAgain, err := cs.ClaimManualReentrySignal(sig2.ID, "user-1", 95)
	if err != nil || !okAgain {
		t.Fatalf("re-claim after release ok=%v err=%v", okAgain, err)
	}
}

// TestManualReentryAlertCooldownTracking 覆盖邮件冷却基准落库
func TestManualReentryAlertCooldownTracking(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "manual-alert.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle := newManualTestCycle(t, cs, "trader-1", "pos-1")

	sig, err := cs.SaveManualReentrySignal(sampleSignal(cycle, "trader-1"))
	if err != nil {
		t.Fatal(err)
	}
	if sig.LastAlertAt != nil {
		t.Fatal("new signal should have nil last_alert_at")
	}
	if err := cs.MarkManualReentrySignalAlerted(sig.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := cs.GetManualReentrySignal(sig.ID)
	if after.LastAlertAt == nil {
		t.Fatal("last_alert_at not set after MarkAlerted")
	}
	// 去重刷新不应重置 last_alert_at（冷却基于首次/上次提醒时间）
	if _, err := cs.SaveManualReentrySignal(sampleSignal(cycle, "trader-1")); err != nil {
		t.Fatal(err)
	}
	stillSet, _ := cs.GetManualReentrySignal(sig.ID)
	if stillSet.LastAlertAt == nil {
		t.Fatal("dedup refresh must not clear last_alert_at")
	}
}

// TestManualReentryInvalidationOnCycleClose 覆盖周期终结时 PENDING 信号根本性失效
func TestManualReentryInvalidationOnCycleClose(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "manual-invalidate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle := newManualTestCycle(t, cs, "trader-1", "pos-1")

	sig, err := cs.SaveManualReentrySignal(sampleSignal(cycle, "trader-1"))
	if err != nil {
		t.Fatal(err)
	}

	// 显式失效 API
	if err := cs.InvalidateManualReentrySignalsForCycle(cycle.ID, "leader reversed"); err != nil {
		t.Fatal(err)
	}
	inv, _ := cs.GetManualReentrySignal(sig.ID)
	if inv.Status != ManualReentryStatusInvalidated || inv.Error != "leader reversed" {
		t.Fatalf("explicit invalidate failed: %+v", inv)
	}

	// 周期闭合钩子：新 PENDING 信号在 CloseCopyGuardCycle 内被 INVALIDATED
	sig2, err := cs.SaveManualReentrySignal(sampleSignal(cycle, "trader-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.CloseCopyGuardCycle(cycle.ID, CopyGuardLeaderClosed, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	closed, _ := cs.GetManualReentrySignal(sig2.ID)
	if closed.Status != ManualReentryStatusInvalidated {
		t.Fatalf("cycle close should invalidate pending signal, got %s", closed.Status)
	}
}

// TestManualReentryQuotaGuard 覆盖单周期信号计数（配额护栏输入）
func TestManualReentryListScopedByTrader(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "manual-scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	c1 := newManualTestCycle(t, cs, "trader-1", "pos-1")
	c2 := newManualTestCycle(t, cs, "trader-2", "pos-2")
	if _, err := cs.SaveManualReentrySignal(sampleSignal(c1, "trader-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.SaveManualReentrySignal(sampleSignal(c2, "trader-2")); err != nil {
		t.Fatal(err)
	}
	// 只查 trader-1，不应看到 trader-2 的信号
	list, err := cs.ListManualReentrySignals([]string{"trader-1"}, nil, 100)
	if err != nil || len(list) != 1 || list[0].TraderID != "trader-1" {
		t.Fatalf("list scoping failed: %+v err=%v", list, err)
	}
	// 空 traderIDs → 空结果（不泄漏全表）
	empty, err := cs.ListManualReentrySignals(nil, nil, 100)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty trader ids should return empty, got %d err=%v", len(empty), err)
	}
}
