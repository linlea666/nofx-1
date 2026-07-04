package copytrade

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

// ============================================================================
// own-path 基线：WLD 周期 27 实盘数字复现
//
// 旧口径用领航员比例折算的影子名义 608 USD 计算基线，得出"帮少亏 13.99"；
// 跟随者实际只开出 166.8 USD，own-path 口径下净保护效果约 +0.8。
// ============================================================================

func TestComputeOwnPathBaselineReproducesWLD(t *testing.T) {
	cycle := &store.CopyGuardCycle{
		Side:                "long",
		LeaderEntryPrice:    0.4388830735558965,
		BaselineNotional:    608.3734019857784, // 旧口径影子名义（不应被使用）
		BaselineRealizedPnL: 0,
		ActualPnL:           -5.392990400000038,
	}
	attempts := []*store.CopyGuardAttempt{
		{AttemptNo: 0, EntryPrice: 0.4389600000000001, Notional: 166.80480000000003},
	}
	leaderClose := 0.4226
	baseline, ok := ComputeOwnPathBaseline(cycle, attempts, leaderClose)
	if !ok {
		t.Fatal("baseline must be computable with complete attempt data")
	}
	// 166.8048 × (0.4226−0.43896)/0.43896 ≈ −6.217
	if math.Abs(baseline-(-6.217)) > 0.01 {
		t.Fatalf("own-path baseline = %.4f, want ≈ -6.217", baseline)
	}
	netEffect := cycle.ActualPnL - baseline
	if math.Abs(netEffect-0.824) > 0.05 {
		t.Fatalf("net guard effect = %.4f, want ≈ +0.82 (not the inflated +13.99)", netEffect)
	}

	// attempt 数据缺失 → 回退旧口径
	if _, ok := ComputeOwnPathBaseline(cycle, nil, leaderClose); ok {
		t.Fatal("missing attempts must report ok=false so callers fall back")
	}
	if _, ok := ComputeOwnPathBaseline(cycle, attempts, 0); ok {
		t.Fatal("missing close price must report ok=false")
	}
}

// ============================================================================
// 加仓账户风险预算（v4.1 仅告警不拦截）：超预算记录 ADDON_RISK_WARNING +
// 事件限频；预算内不产生事件
// ============================================================================

func newAddonBudgetEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &Engine{
		traderID: "trader-1",
		config: &CopyConfig{
			ProviderType:        ProviderOKX,
			LeaderID:            "leader",
			CopyRatio:           1,
			RiskPolicyVersion:   4,
			RiskStopLossEnabled: true,
			RiskStopMode:        "volatility_priority",
			RiskAddonBudgetPct:  0.15,
			// 不受支持的 timeframe 让 ATR 立即失败 → 走 RiskATRFallbackPct（2%），测试可确定
			RiskATRTimeframe:   "5m",
			RiskATRPeriod:      14,
			RiskATRFallbackPct: 0.02,
		},
		store:                st,
		stats:                &EngineStats{},
		getFollowerEquity:    func() float64 { return 100 },
		lastAddonBudgetEvent: make(map[string]time.Time),
	}
	return e, st
}

func TestAddonBudgetWarnsWithoutBlocking(t *testing.T) {
	e, st := newAddonBudgetEngine(t)
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-add", Symbol: "WLDUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 1, FollowerEntryPrice: 1, FollowerNotional: 100, AccountEquity: 100})
	if err != nil {
		t.Fatal(err)
	}
	signal := &TradeSignal{ProviderType: ProviderOKX, LeaderID: "leader", LeaderPosID: "pos-add", Fill: &Fill{Symbol: "WLDUSDT", Price: 1, PositionSide: "long", Action: ActionAdd}}

	countBudgetEvents := func() int {
		events, err := st.CopyTrade().ListCopyGuardEvents(cycle.ID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, ev := range events {
			if ev.Type == "ADDON_RISK_WARNING" {
				n++
			}
		}
		return n
	}

	// 现有名义 100 + 加仓 700 = 800，预期损失 800×2% = 16 USD = 16% > 预算 15% → 告警
	e.warnAddonRiskBudget(signal, "pos-add", 700)
	if countBudgetEvents() != 1 {
		t.Fatalf("expected exactly 1 ADDON_RISK_WARNING event, got %d", countBudgetEvents())
	}

	// 60 秒限频窗口内重复超预算：不重复写事件
	e.warnAddonRiskBudget(signal, "pos-add", 700)
	if countBudgetEvents() != 1 {
		t.Fatalf("rate-limited warning must not write a duplicate event, got %d", countBudgetEvents())
	}

	// 100 + 500 = 600，预期损失 12% ≤ 15% → 不告警
	e2, st2 := newAddonBudgetEngine(t)
	cycle2, err := st2.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-add", Symbol: "WLDUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 1, FollowerEntryPrice: 1, FollowerNotional: 100, AccountEquity: 100})
	if err != nil {
		t.Fatal(err)
	}
	e2.warnAddonRiskBudget(signal, "pos-add", 500)
	events2, _ := st2.CopyTrade().ListCopyGuardEvents(cycle2.ID)
	for _, ev := range events2 {
		if ev.Type == "ADDON_RISK_WARNING" {
			t.Fatal("an add within the budget must not produce a warning event")
		}
	}
	// 预算设 0（禁用）→ 完全不检查
	e2.config.RiskAddonBudgetPct = 0
	e2.warnAddonRiskBudget(signal, "pos-add", 10000)
	events2, _ = st2.CopyTrade().ListCopyGuardEvents(cycle2.ID)
	for _, ev := range events2 {
		if ev.Type == "ADDON_RISK_WARNING" {
			t.Fatal("budget=0 disables the check entirely")
		}
	}
}

// ============================================================================
// 历史基线一次性迁移：旧口径（影子名义）→ own-path 口径
// ============================================================================

func TestMigrateCopyGuardBaselinesV2RecomputesHistoricalCycle(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "migrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()

	// 自身仓位：entry 100、名义 100，98 被止损（pnl −2）
	cycle, err := cs.EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-mig", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 100, FollowerNotional: 100, AccountEquity: 500, LastObservedPrice: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.OpenCopyGuardAttempt(cycle.ID, 0, 100, 100, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := cs.RecordCopyGuardStop(cycle, 2, 98, -2, 0, 0, "algo-1", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if err := cs.ReconcileCopyGuardAttempt(cycle.ID, 0, -2, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	// 旧口径关闭：影子名义 600、领航员平仓 95 → 旧基线 600×(95−100)/100 = −30，
	// 旧 net_guard_effect = −2 − (−30) = +28（虚高）
	if _, err := st.DB().Exec(`UPDATE copy_guard_cycles SET baseline_notional=600, last_observed_price=95 WHERE id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	if err := cs.CloseCopyGuardCycle(cycle.ID, store.CopyGuardLeaderClosed, -2, -30, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	closed, _ := cs.GetCopyGuardCycle(cycle.ID)
	if closed.AccountingStatus != store.CopyGuardAccountingReconciled || math.Abs(closed.NetGuardEffect-28) > 1e-9 {
		t.Fatalf("precondition: reconciled cycle with inflated effect +28, got %+v", closed)
	}
	// 模拟旧库数据（EnsureCopyGuardCycle 新建即 v2，历史行是 v1）
	if _, err := st.DB().Exec(`UPDATE copy_guard_cycles SET baseline_version=1 WHERE id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}

	MigrateCopyGuardBaselinesV2(st)

	migrated, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	// own-path：反解平仓价 = 100×(1−30/600) = 95；基线 = 100×(95−100)/100 = −5；
	// net = −2 − (−5) = +3
	if math.Abs(migrated.BaselinePnL-(-5)) > 1e-9 {
		t.Fatalf("baseline must be recomputed to -5, got %.4f", migrated.BaselinePnL)
	}
	if math.Abs(migrated.NetGuardEffect-3) > 1e-9 {
		t.Fatalf("net guard effect must be recomputed to +3, got %.4f", migrated.NetGuardEffect)
	}

	// 幂等：重跑不再变化（版本已标 2，不会重扫）
	MigrateCopyGuardBaselinesV2(st)
	again, _ := cs.GetCopyGuardCycle(cycle.ID)
	if again.BaselinePnL != migrated.BaselinePnL || again.NetGuardEffect != migrated.NetGuardEffect {
		t.Fatal("migration must be idempotent")
	}
}
