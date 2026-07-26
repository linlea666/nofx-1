package copytrade

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

// ============================================================================
// 测试脚手架
// ============================================================================

// stopMgrExecutor: 决策执行器 + 保护单管理器 + 可配置持仓（poll/backfill/upsert 用）
type stopMgrExecutor struct {
	flatExecutor
	*mockStopMgr
	positions  []map[string]interface{}
	closeCalls int
}

func (e *stopMgrExecutor) GetPositions() ([]map[string]interface{}, error) { return e.positions, nil }
func (e *stopMgrExecutor) ClosePositionMarket(string, string) (string, error) {
	e.closeCalls++
	return "forced-close", nil
}

func newReentryTestEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "reentry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &Engine{
		traderID: "trader-1",
		config: &CopyConfig{
			ProviderType:       ProviderOKX,
			LeaderID:           "leader",
			CopyRatio:          1,
			RiskPolicyVersion:  4,
			RiskReentryEnabled: true,
			RiskReentryRatio:   0.5,
			RiskMaxReentries:   2,
			// 不受支持的 timeframe 让 ATR 立即失败 → 走 RiskATRFallbackPct，测试可确定
			RiskATRTimeframe:           "5m",
			RiskATRPeriod:              14,
			RiskATRFallbackPct:         0.02,
			RiskReentryBandATR:         0.5,
			RiskReentryMaxChaseATR:     2,
			RiskReentryMaxATRExpansion: 2,
			RiskReentryCooldownSeconds: 0,
			RiskWatchTimeoutMinutes:    0,
			MinTradeWarn:               10,
		},
		store:              st,
		seenFills:          make(map[string]time.Time),
		seenTTL:            time.Hour,
		leaderState:        &AccountState{TotalEquity: 1000, Positions: map[string]*Position{}},
		decisionCh:         make(chan *decision.FullDecision, 10),
		stats:              &EngineStats{},
		getFollowerBalance: func() float64 { return 100 },
		getFollowerEquity:  func() float64 { return 100 },
		getFollowerPositions: func() map[string]*Position {
			return map[string]*Position{}
		},
		stopRiskSuspectCount: make(map[string]int),
	}
	if _, err := st.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance)
		VALUES(?,?,?,?,?)`, e.traderID, "reentry-test", "", "exchange-1", 100); err != nil {
		t.Fatal(err)
	}
	persisted := store.NewCopyGuardDefaults()
	persisted.TraderID = e.traderID
	persisted.ProviderType = string(e.config.ProviderType)
	persisted.LeaderID = e.config.LeaderID
	persisted.Enabled = true
	persisted.RiskPolicyVersion = e.config.RiskPolicyVersion
	persisted.RiskStopLossEnabled = true
	persisted.RiskReentryEnabled = true
	persisted.RiskReentryDecisionMode = "legacy_rule"
	if err := st.CopyTrade().Upsert(persisted); err != nil {
		t.Fatal(err)
	}
	return e, st
}

// seedStoppedCycle: 建 mapping(stopped_by_risk) + open cycle(STOPPED_WATCHING)
func seedStoppedCycle(t *testing.T, st *store.Store, traderID string, side string, leaderEntry float64) *store.CopyGuardCycle {
	t.Helper()
	mapping := &store.CopyTradePositionMapping{TraderID: traderID, LeaderPosID: "leader-pos", LeaderID: "leader", Symbol: "ETHUSDT", Side: side, MarginMode: "cross", OpenedAt: time.Now(), OpenPrice: leaderEntry, OpenSizeUSD: 100, LastKnownSize: 1}
	if err := st.CopyTrade().SavePositionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().MarkStoppedByRisk(traderID, "leader-pos", -5, 1, 0); err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: traderID, LeaderID: "leader", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: side, MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: leaderEntry, FollowerEntryPrice: leaderEntry, FollowerNotional: 100, AccountEquity: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, leaderEntry, 100, 100/leaderEntry, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().RecordCopyGuardStop(cycle, 34, leaderEntry*0.98, -2, 0.1, 0, "algo-0", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

// ============================================================================
// 兜底止损路径必须关闭 attempt（B6）
// ============================================================================

func TestStopObservedClosesAttempt(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "observed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 1717.33, 110, 0.064, 7.59); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().RecordCopyGuardStopObserved(cycle.ID, "trader-1", 0, 7.59, 1700, 1, map[string]interface{}{"confirmation": "position_absent_fallback", "leader_pnl": -1.1}); err != nil {
		t.Fatal(err)
	}
	got, err := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil || got.Status != store.CopyGuardStoppedWatching {
		t.Fatalf("cycle must enter STOPPED_WATCHING: %+v %v", got, err)
	}
	attempts, err := st.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts: %v %v", attempts, err)
	}
	if attempts[0].Status != "STOPPED" || attempts[0].ExitPrice != 1700 {
		t.Fatalf("observed stop must close the open attempt with the observed price: %+v", attempts[0])
	}
}

// ============================================================================
// 两轮 stop → reentry 生命周期闭合（attempt 编号 / 次数 / 状态）
// ============================================================================

func TestDoubleStopReentryLifecycle(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "double.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 1717.33, 110, 0.064, 7.59); err != nil {
		t.Fatal(err)
	}

	// 第 1 次止损：关闭 attempt 0
	if err := st.CopyTrade().RecordCopyGuardStop(cycle, 8, 1700, -1.1, 0.05, 0.02, "algo-0", map[string]interface{}{"quantity": 0.064}); err != nil {
		t.Fatal(err)
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if cycle.Status != store.CopyGuardStoppedWatching || cycle.StopCount != 1 || cycle.ReentryCount != 0 {
		t.Fatalf("after first stop: %+v", cycle)
	}

	// 第 1 次重入：attempt 1 开启，reentry_count=1
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardReentryPending, cycle.LeaderEntryPrice, cycle.LastObservedPrice, 0)
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err := st.CopyTrade().RecordCopyGuardReentryFilled(cycle, 1705, 55, 0.032, 8, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if cycle.Status != store.CopyGuardFollowingReentry || cycle.ReentryCount != 1 {
		t.Fatalf("after first reentry: %+v", cycle)
	}

	// 第 2 次止损：关闭 attempt 1（attempt_no = ReentryCount）
	if err := st.CopyTrade().RecordCopyGuardStop(cycle, 9, 1690, -0.8, 0.03, 0.01, "algo-1", map[string]interface{}{"quantity": 0.032}); err != nil {
		t.Fatal(err)
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if cycle.Status != store.CopyGuardStoppedWatching || cycle.StopCount != 2 || cycle.ReentryCount != 1 {
		t.Fatalf("after second stop: %+v", cycle)
	}

	// 第 2 次重入：attempt 2 开启，reentry_count=2，UNIQUE(cycle_id, attempt_no) 无冲突
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardReentryPending, cycle.LeaderEntryPrice, cycle.LastObservedPrice, 0)
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err := st.CopyTrade().RecordCopyGuardReentryFilled(cycle, 1702, 27, 0.016, 9, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if cycle.ReentryCount != 2 {
		t.Fatalf("after second reentry: %+v", cycle)
	}
	attempts, err := st.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 3 {
		t.Fatalf("attempts: %d %v", len(attempts), err)
	}
	wantStatus := map[int]string{0: "STOPPED", 1: "STOPPED", 2: "OPEN"}
	for _, a := range attempts {
		if a.Status != wantStatus[a.AttemptNo] {
			t.Fatalf("attempt %d status=%s want=%s", a.AttemptNo, a.Status, wantStatus[a.AttemptNo])
		}
	}
}

// ============================================================================
// 重入判定（v5 确认式）：首轮观察不计确认（B7）；连续 N tick 满足才触发；
// 中途破坏清零重来
// ============================================================================

func TestReentryFirstTickGuardAndConfirmation(t *testing.T) {
	e, st := newReentryTestEngine(t)
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	// RecordCopyGuardStop 未写 last_observed_price → 首轮为 0
	if cycle.LastObservedPrice != 0 {
		t.Fatalf("precondition: LastObservedPrice must start at 0, got %v", cycle.LastObservedPrice)
	}
	// ATR fallback = 1700*0.02 = 34；band=0.5 → boundary = 1700-17 = 1683
	// 标记价 1690 已在带内：首轮（last=0）必须只记录观测、不计确认
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}
	e.checkReentryConditions()
	select {
	case dec := <-e.decisionCh:
		t.Fatalf("first observation tick must not trigger reentry, got %+v", dec.Decisions)
	default:
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if cycle.Status != store.CopyGuardStoppedWatching || cycle.LastObservedPrice != 1690 {
		t.Fatalf("first tick must persist observation only: %+v", cycle)
	}

	// 带内 2 个 tick：确认累计但未达阈值（3），不触发
	e.checkReentryConditions()
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
		t.Fatal("in-band ticks below the confirmation threshold must not trigger reentry")
	default:
	}

	// 跌出带外（1660 < 1683）→ 确认计数必须清零
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1660
	e.checkReentryConditions()
	if e.reentryCandidateTicks["leader-pos"] != 0 {
		t.Fatalf("out-of-band tick must reset the confirmation counter, got %d", e.reentryCandidateTicks["leader-pos"])
	}

	// 重新回带内并连续 3 tick 确认 → 触发重入决策 + REENTRY_PENDING
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1690
	e.checkReentryConditions()
	e.checkReentryConditions()
	e.checkReentryConditions()
	select {
	case full := <-e.decisionCh:
		if len(full.Decisions) != 1 || full.Decisions[0].Symbol != "ETHUSDT" {
			t.Fatalf("unexpected reentry decision: %+v", full.Decisions)
		}
	default:
		t.Fatal("three confirmed in-band ticks must emit a reentry decision")
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if cycle.Status != store.CopyGuardReentryPending {
		t.Fatalf("cycle must be REENTRY_PENDING after emitting: %+v", cycle)
	}
	if cycle.ReentryCount != 0 {
		t.Fatalf("reentry count must only increase after the fill, got %d", cycle.ReentryCount)
	}
}

// ============================================================================
// 重入定 size：以被止损 attempt 的名义价值为基准，与领航员当前仓位规模解耦
// （实盘事故：领航员跟随期间加仓后，按领航员占比折算的重入是首仓的 8 倍、
//  57 倍杠杆，止损价落入强平区导致保护单永远挂不上）
// ============================================================================

func TestReentrySizeUsesStoppedAttemptNotional(t *testing.T) {
	e, st := newReentryTestEngine(t)
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700) // attempt 0 notional=100 被止损
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1660, 34)
	// 领航员当前持仓极大（加仓后）：旧公式会算出 1×0.5×(2500×1690/1000)×100 ≈ 21 万
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 2500, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}
	// v5 确认式：连续 3 tick 满足才触发
	e.checkReentryConditions()
	e.checkReentryConditions()
	e.checkReentryConditions()
	select {
	case full := <-e.decisionCh:
		if len(full.Decisions) != 1 {
			t.Fatalf("unexpected decisions: %+v", full.Decisions)
		}
		got := full.Decisions[0].PositionSizeUSD
		if got != 50 { // 被止损名义 100 × 重入系数 0.5
			t.Fatalf("reentry size must be stopped-attempt notional × ratio (want 50), got %v", got)
		}
	default:
		t.Fatal("confirmed in-band ticks must emit a reentry decision")
	}
}

// ============================================================================
// 重入拦截条件：冷却期 / 次数耗尽
// ============================================================================

func TestReentryBlockedByCooldownAndExhaustion(t *testing.T) {
	// 冷却期内即使满足穿越条件也不触发
	e, st := newReentryTestEngine(t)
	e.config.RiskReentryCooldownSeconds = 3600
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1660, 34)
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
		t.Fatal("cooldown must block reentry")
	default:
	}
	got, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if got.Status != store.CopyGuardStoppedWatching {
		t.Fatalf("cooldown keeps the cycle watching: %+v", got)
	}

	// 次数耗尽：周期转 ATTEMPTS_EXHAUSTED（保持 open 等领航员平仓），不触发决策
	e2, st2 := newReentryTestEngine(t)
	e2.config.RiskMaxReentries = 0
	cycle2 := seedStoppedCycle(t, st2, "trader-1", "long", 1700)
	_ = st2.CopyTrade().UpdateCopyGuardObservation(cycle2.ID, store.CopyGuardStoppedWatching, 1700, 1660, 34)
	e2.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}
	e2.checkReentryConditions()
	select {
	case <-e2.decisionCh:
		t.Fatal("exhausted attempts must block reentry")
	default:
	}
	got2, _ := st2.CopyTrade().GetCopyGuardCycle(cycle2.ID)
	if got2.Status != store.CopyGuardAttemptsExhausted {
		t.Fatalf("cycle must be flagged ATTEMPTS_EXHAUSTED: %+v", got2)
	}
	if got2.ClosedAt != nil {
		t.Fatalf("exhausted cycle stays open until the leader closes (accounting chain): %+v", got2)
	}
}

// ============================================================================
// v4.1 重入最小恢复幅度：穿越带宽但未从止损价恢复足够幅度时不得重入
// ============================================================================

func TestReentryMinRecoveryRaisesBoundary(t *testing.T) {
	e, st := newReentryTestEngine(t)
	e.config.RiskReentryMinRecoveryATR = 1.0 // 要求从止损价恢复 1×ATR
	// seedStoppedCycle: entry=1700, 止损成交价 = 1700×0.98 = 1666
	// ATR fallback = 1700×0.02 = 34 → 带宽边界 1700−17=1683，
	// 恢复下限 1666+34=1700 → 有效边界抬高到 1700
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1660, 34)
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}

	// 1690 在旧带宽边界 1683 之上，但未达到恢复下限 1700 → 不得重入
	e.checkReentryConditions()
	e.checkReentryConditions()
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
		t.Fatal("in-band price without min recovery must not trigger reentry")
	default:
	}

	// 恢复到 1705（≥1700 且 ≤ 追价上限 1700+68）→ 连续确认后触发
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1705
	e.checkReentryConditions()
	e.checkReentryConditions()
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
	default:
		t.Fatal("confirmed ticks above the recovery-raised boundary must trigger reentry")
	}
}

// ============================================================================
// v4.1 保守锚点：领航员止损后摊均价（实时均价变低）不得放松重入门槛
// ============================================================================

func TestReentryConservativeAnchorIgnoresAveragedDownEntry(t *testing.T) {
	e, st := newReentryTestEngine(t)
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	// 止损时快照领航员均价 1700；随后领航员加仓摊均价到 1650
	if err := st.CopyTrade().SnapshotCopyGuardLeaderEntryAtStop(cycle.ID, 1700); err != nil {
		t.Fatal(err)
	}
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1620, 34)
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 2, EntryPrice: 1650, MarkPrice: 1640, MarginMode: "cross", PosID: "leader-pos"}

	// 按摊低后的实时均价算边界是 1650−17=1633（1640 在带内）；
	// 保守锚点必须用快照 1700 → 边界 1683，1640 不得触发
	e.checkReentryConditions()
	e.checkReentryConditions()
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
		t.Fatal("averaged-down live entry must not loosen the reentry boundary")
	default:
	}

	// 真实恢复到 1690（≥1683，≤ 锚点 1700+68）→ 连续确认后触发
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1690
	e.checkReentryConditions()
	e.checkReentryConditions()
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
	default:
		t.Fatal("confirmed ticks above the snapshot-anchored boundary must trigger reentry")
	}
}

// ============================================================================
// v5 噪音档：止损时 distance/ATR < 0.3 → 自动重入默认禁用；override 放行但
// 走谨慎档（确认 tick 数翻倍）
// ============================================================================

func TestReentryNoiseTierDisablesAndOverrides(t *testing.T) {
	e, st := newReentryTestEngine(t)
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	// 把止损快照改成极窄距离：entry 1700 → exit 1693.2（distance=6.8），
	// ATRAtStop=34 → ratio=0.2 < 0.3 → 噪音档禁入
	if _, err := st.DB().Exec(`UPDATE copy_guard_attempts SET exit_price=1693.2, stop_fill_price=1693.2 WHERE cycle_id=? AND attempt_no=0`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1660, 34)
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}

	for i := 0; i < 6; i++ {
		e.checkReentryConditions()
	}
	select {
	case <-e.decisionCh:
		t.Fatal("distance/ATR < 0.3 must disable automatic reentry by default")
	default:
	}

	// override 后放行，但走谨慎档：确认 tick 数翻倍（6）
	e.config.RiskReentryNoiseOverride = true
	for i := 0; i < 5; i++ {
		e.checkReentryConditions()
	}
	select {
	case <-e.decisionCh:
		t.Fatal("cautious tier must require doubled confirmation ticks")
	default:
	}
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
	default:
		t.Fatal("override + doubled confirmation must eventually trigger reentry")
	}
}

// ============================================================================
// v4.1 冷却递增上限：高倍率 × 高重入次数不得溢出为负冷却（等于绕过冷却）
// ============================================================================

func TestReentryCooldownEscalationClampsInsteadOfOverflow(t *testing.T) {
	e, st := newReentryTestEngine(t)
	e.config.RiskMaxReentries = 10
	e.config.RiskReentryCooldownSeconds = 86400
	e.config.RiskReentryCooldownEscalation = 10
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700) // stopped_at = 刚刚
	// 86400s × 10^9 转 time.Duration 会越过 int64 纳秒上限；未钳制时溢出为
	// 负值 → coolingDown 恒 false → 冷却被绕过
	if _, err := st.DB().Exec(`UPDATE copy_guard_cycles SET reentry_count=9 WHERE id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1660, 34)
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}

	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
		t.Fatal("escalated cooldown must clamp to a large positive value, not overflow and bypass")
	default:
	}
	got, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if got.Status != store.CopyGuardStoppedWatching {
		t.Fatalf("cooling cycle keeps watching: %+v", got)
	}
}

// ============================================================================
// 观察期领航员反手 → 周期以 LEADER_REVERSED 闭合（L3）
// ============================================================================

func TestWatchReversalClosesCycle(t *testing.T) {
	e, st := newReentryTestEngine(t)
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	// 领航员同 posId 反手为空单
	e.leaderState.Positions["ETHUSDT_short"] = &Position{Symbol: "ETHUSDT", Side: SideShort, Size: 1, EntryPrice: 1700, MarkPrice: 1695, MarginMode: "cross", PosID: "leader-pos"}
	e.checkReentryConditions()
	got, err := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.CopyGuardLeaderReversed || got.ClosedAt == nil {
		t.Fatalf("watch-phase reversal must close the cycle as LEADER_REVERSED: %+v", got)
	}
	// GetMapping 忽略 closed 状态 → 返回 nil 即代表映射已闭合、posId 可复用
	mapping, err := st.CopyTrade().GetMapping("trader-1", "leader-pos")
	if err != nil || mapping != nil {
		t.Fatalf("mapping must leave active/stopped states after reversal: %+v %v", mapping, err)
	}
	stopped, err := st.CopyTrade().ListStoppedByRiskMappings("trader-1")
	if err != nil || len(stopped) != 0 {
		t.Fatalf("no stopped_by_risk mapping may remain after reversal: %v %v", stopped, err)
	}
}

// ============================================================================
// 存量持仓回填生命周期（L1）
// ============================================================================

func TestBackfillV4Cycles(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "backfill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	executor := &stopMgrExecutor{mockStopMgr: &mockStopMgr{}, positions: []map[string]interface{}{
		{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "entryPrice": 1717.33, "positionAmt": 0.064, "posId": "follower-pos"},
	}}
	ti := NewTraderIntegration("trader-1", executor, st)
	ti.engine = &Engine{config: &CopyConfig{ProviderType: ProviderOKX, LeaderID: "leader", RiskPolicyVersion: 4, RiskStopLossEnabled: true}}

	// v4 前已跟随的存量仓位：active mapping 且无 open cycle
	mapping := &store.CopyTradePositionMapping{TraderID: "trader-1", LeaderPosID: "legacy-pos", LeaderID: "leader", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", OpenedAt: time.Now(), OpenPrice: 1720, OpenSizeUSD: 110, LastKnownSize: 1}
	if err := st.CopyTrade().SavePositionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	// 另一条 mapping 没有对应真实持仓 → 不应回填
	ghost := &store.CopyTradePositionMapping{TraderID: "trader-1", LeaderPosID: "ghost-pos", LeaderID: "leader", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", OpenedAt: time.Now(), OpenPrice: 60000, OpenSizeUSD: 50, LastKnownSize: 1}
	if err := st.CopyTrade().SavePositionMapping(ghost); err != nil {
		t.Fatal(err)
	}

	ti.backfillV4Cycles()
	cycle, err := st.CopyTrade().GetOpenCopyGuardCycle("trader-1", "legacy-pos")
	if err != nil {
		t.Fatalf("legacy position must be backfilled: %v", err)
	}
	if cycle.ProtectionStatus != store.CopyGuardProtectionPending {
		t.Fatalf("backfilled cycle must start PENDING so the retry channel attaches protection: %+v", cycle)
	}
	if cycle.FollowerEntryPrice != 1717.33 {
		t.Fatalf("backfill must use the real position entry price: %+v", cycle)
	}
	attempts, err := st.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != "OPEN" {
		t.Fatalf("backfill must open attempt 0: %v %v", attempts, err)
	}
	if _, err := st.CopyTrade().GetOpenCopyGuardCycle("trader-1", "ghost-pos"); err == nil {
		t.Fatal("a mapping without a real follower position must not be backfilled")
	}

	// 幂等：再跑一轮不得重复建周期
	ti.lastV4Backfill = time.Time{}
	ti.backfillV4Cycles()
	again, err := st.CopyTrade().GetOpenCopyGuardCycle("trader-1", "legacy-pos")
	if err != nil || again.ID != cycle.ID {
		t.Fatalf("backfill must be idempotent: %+v %v", again, err)
	}
}

// ============================================================================
// 观察期采样：gate 时间线 + 变化沿事件 + 同 gate 间隔内降噪（v4.1）
// ============================================================================

func TestWatchSamplesRecordGateTimeline(t *testing.T) {
	e, st := newReentryTestEngine(t)
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)

	// tick 1：首轮观察（last=0）不判穿越 → PRICE_NOT_RETURNED，且必须记录边界/追价上限
	// ATR fallback = 1700×0.02 = 34；boundary = 1700 − 17 = 1683；chase = 1700 + 68 = 1768
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}
	e.checkReentryConditions()
	// tick 2：跌出带外，gate 不变且距上一条采样 < 固定间隔 → 必须降噪跳过
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1660
	e.checkReentryConditions()

	samples, err := st.CopyTrade().ListCopyGuardWatchSamples(cycle.ID)
	if err != nil || len(samples) != 1 {
		t.Fatalf("same-gate ticks within the interval must dedupe to one sample: %d %v", len(samples), err)
	}
	if samples[0].Gate != watchGatePriceNotReturned || samples[0].ReentryBoundary != 1683 || samples[0].ChaseLimit != 1768 {
		t.Fatalf("first sample must record gate/boundary/chase: %+v", samples[0])
	}

	// tick 3-5：回到带内，v5 确认式——先 REENTRY_CANDIDATE 采样，连续 3 tick
	// 后 REENTRY_TRIGGERED 采样 + 决策
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1690
	e.checkReentryConditions()
	samples, _ = st.CopyTrade().ListCopyGuardWatchSamples(cycle.ID)
	if len(samples) != 2 || samples[1].Gate != watchGateReentryCandidate {
		t.Fatalf("first in-band tick must record REENTRY_CANDIDATE: %+v", samples)
	}
	e.checkReentryConditions()
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
	default:
		t.Fatal("confirmed ticks must emit a reentry decision")
	}
	samples, _ = st.CopyTrade().ListCopyGuardWatchSamples(cycle.ID)
	if len(samples) != 3 || samples[2].Gate != watchGateReentryTriggered {
		t.Fatalf("trigger must append a REENTRY_TRIGGERED sample: %+v", samples)
	}
	events, _ := st.CopyTrade().ListCopyGuardEvents(cycle.ID)
	gateChanges := 0
	for _, ev := range events {
		if ev.Type == "REENTRY_GATE_CHANGED" {
			gateChanges++
		}
	}
	// PRICE_NOT_RETURNED → REENTRY_CANDIDATE → REENTRY_TRIGGERED 两次变化
	if gateChanges != 2 {
		t.Fatalf("exactly two REENTRY_GATE_CHANGED expected, got %d", gateChanges)
	}
}

// ============================================================================
// 观察期收尾统计：价格口径挽回/错过 + 已回归但被门控挡住的采样（v4.1）
// ============================================================================

func TestWatchSummaryPriceSavedAndBlockedGates(t *testing.T) {
	_, st := newReentryTestEngine(t)
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700) // 止损成交价 1700×0.98=1666，qty=100/1700

	// 观察期采样：一条价格已回归边界但被冷却挡住；一条未回归
	if err := st.CopyTrade().SaveCopyGuardWatchSample(&store.CopyGuardWatchSample{CycleID: cycle.ID, TraderID: "trader-1", MarkPrice: 1690, ATR: 34, ReentryBoundary: 1683, ChaseLimit: 1768, Gate: watchGateCooldown}); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().SaveCopyGuardWatchSample(&store.CopyGuardWatchSample{CycleID: cycle.ID, TraderID: "trader-1", MarkPrice: 1660, ATR: 34, ReentryBoundary: 1683, ChaseLimit: 1768, Gate: watchGatePriceNotReturned}); err != nil {
		t.Fatal(err)
	}

	// 领航员最终 1600 平仓：多单止损在 1666 离场帮忙少亏 (1666−1600)×qty > 0
	emitWatchSummary(st.CopyTrade(), "trader-1", cycle, 1600)

	events, err := st.CopyTrade().ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	var summary *store.CopyGuardEvent
	for _, ev := range events {
		if ev.Type == "WATCH_SUMMARY" {
			summary = ev
		}
	}
	if summary == nil {
		t.Fatal("WATCH_SUMMARY event must be recorded for stopped cycles")
	}
	wantSaved := (1700*0.98 - 1600) * (100.0 / 1700.0)
	if diff := summary.PnL - wantSaved; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("price_saved must be (stopPrice-closePrice)×qty=%.4f, got %.4f", wantSaved, summary.PnL)
	}
	blocked, ok := summary.Metadata["blocked_when_recovered"].(map[string]interface{})
	if !ok || blocked[watchGateCooldown] == nil {
		t.Fatalf("recovered-but-blocked gates must be reported: %+v", summary.Metadata)
	}
	if summary.Metadata["first_recovery_seconds"].(float64) < 0 {
		t.Fatalf("price recovered in samples, first_recovery_seconds must be >= 0: %+v", summary.Metadata)
	}

	// 未触发过止损的周期不产生 WATCH_SUMMARY
	fresh, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-2", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 60000, FollowerEntryPrice: 60000, FollowerNotional: 100, AccountEquity: 100})
	if err != nil {
		t.Fatal(err)
	}
	emitWatchSummary(st.CopyTrade(), "trader-1", fresh, 61000)
	events2, _ := st.CopyTrade().ListCopyGuardEvents(fresh.ID)
	for _, ev := range events2 {
		if ev.Type == "WATCH_SUMMARY" {
			t.Fatal("cycles without stops must not emit WATCH_SUMMARY")
		}
	}
}

// ============================================================================
// 已关闭周期的孤儿保护单：poll 撤销且不再告警（B4/B5）
// ============================================================================

func TestPollCancelsOrphanProtectiveOrder(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "orphan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	executor := &stopMgrExecutor{mockStopMgr: &mockStopMgr{}}
	ti := NewTraderIntegration("trader-1", executor, st)

	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: cycle.ID, TraderID: "trader-1", AlgoID: "orphan-algo", AlgoClientID: "cg1a0", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 0.064, TriggerPrice: 1711.63, TriggerType: "mark", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	// 周期关闭但保护单仍 live → 孤儿
	if err := st.CopyTrade().CloseCopyGuardCycle(cycle.ID, store.CopyGuardLeaderClosed, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	ti.pollV4ProtectiveStops()
	if len(executor.mockStopMgr.canceled) != 1 || executor.mockStopMgr.canceled[0] != "orphan-algo" {
		t.Fatalf("orphan protective order must be canceled on OKX: %v", executor.mockStopMgr.canceled)
	}
	stored, _ := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if stored.Status != "canceled" {
		t.Fatalf("orphan order must be marked canceled locally: %+v", stored)
	}
	closed, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if closed.ProtectionStatus == store.CopyGuardProtectionDegraded || closed.ProtectionStatus == store.CopyGuardProtectionUnknown {
		t.Fatalf("a closed cycle must never be flagged by poll: %+v", closed)
	}
}

func TestProtectiveStopMustConfirmFlatBeforeWatchingOrReentry(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "partial-stop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mock := &mockStopMgr{byID: map[string]*trader.ProtectiveStopOrder{
		"stop-algo": {AlgoID: "stop-algo", PositionSide: "long", MarginMode: "cross", Quantity: .05, TriggerPrice: 98, State: "effective"},
	}}
	executor := &stopMgrExecutor{mockStopMgr: mock, positions: []map[string]interface{}{{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "positionAmt": .01, "entryPrice": 100}}}
	ti := NewTraderIntegration("trader-1", executor, st)
	ti.engine = &Engine{traderID: "trader-1", config: &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskATRTimeframe: "unsupported", RiskATRPeriod: 14, RiskATRFallbackPct: .02}, store: st, leaderState: &AccountState{Positions: map[string]*Position{}}}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 100, FollowerNotional: 5, AccountEquity: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 100, 5, .05, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: cycle.ID, TraderID: "trader-1", AlgoID: "stop-algo", AlgoClientID: "cg", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: .05, TriggerPrice: 98, TriggerType: "mark", Status: "live"}); err != nil {
		t.Fatal(err)
	}

	ti.pollV4ProtectiveStops()
	partial, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if partial.Status != store.CopyGuardStopPartial || partial.StopCount != 0 || executor.closeCalls != 1 {
		t.Fatalf("partial stop must stay blocked and issue one controlled exit: cycle=%+v closes=%d", partial, executor.closeCalls)
	}
	if candidates, _ := st.ReentryAI().ListReentryCandidatesByCycle(cycle.ID); len(candidates) != 0 {
		t.Fatalf("residual position must never create reentry candidate: %+v", candidates)
	}

	// Only a fresh exchange-flat observation may advance into stopped watching.
	executor.positions = nil
	ti.pollV4ProtectiveStops()
	flat, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if flat.Status != store.CopyGuardStoppedWatching || flat.StopCount != 1 {
		t.Fatalf("confirmed flat must close the attempt exactly once: %+v", flat)
	}
}

// ============================================================================
// upsert 验证失败时 DB 必须记录 OKX 实单数量而非请求数量（B1）
// ============================================================================

func TestUpsertRecordsVerifiedQuantityOnMismatch(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "verify.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// OKX 实单只有一半数量（加仓后 amend 失败的实况）
	mock := &mockStopMgr{byID: map[string]*trader.ProtectiveStopOrder{}}
	executor := &stopMgrExecutor{mockStopMgr: mock, positions: []map[string]interface{}{
		{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "entryPrice": 1717.33, "positionAmt": 0.128, "posId": "follower-pos"},
	}}
	ti := NewTraderIntegration("trader-1", executor, st)
	ti.engine = &Engine{config: &CopyConfig{ProviderType: ProviderOKX, LeaderID: "leader", RiskPolicyVersion: 4, RiskStopLossEnabled: true, RiskTriggerPriceType: "mark"}}
	cycle := newTestCopyGuardCycle(t, st, "trader-1")
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 1717.33, 110, 0.064, 7.59); err != nil {
		t.Fatal(err)
	}
	// Place 成功但 OKX 实单是半量（模拟数量被交易所裁剪/旧单未 amend 成功）
	mock.byID["new-algo"] = &trader.ProtectiveStopOrder{AlgoID: "new-algo", ClientID: "cg", PositionSide: "long", MarginMode: "cross", Quantity: 0.064, TriggerPrice: 1711.63, State: "live"}

	dec := &decision.Decision{Symbol: "ETHUSDT", Action: "open_long", LeaderPosID: "leader-pos", MarginMode: "cross", EntryPrice: 1717.33}
	ti.upsertV4Protection(dec, "long", 0.128, 1717.33, &StopLossCalcResult{SLPrice: 1711.63, TickSize: 0.01, QuantityStep: 0.001})

	stored, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Quantity != 0.064 {
		t.Fatalf("DB must record the verified exchange quantity (0.064), not the requested one: %+v", stored)
	}
	got, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if got.ProtectionStatus != store.CopyGuardProtectionDegraded {
		t.Fatalf("half coverage must be flagged DEGRADED: %+v", got)
	}
}

// ============================================================================
// A. 重入窗口塌缩死锁修复（cycle 71）：恢复下界越过追价上限=空集 →
//    自动重入实质用尽 → 持久化 ATTEMPTS_EXHAUSTED 交 v5.1 人工确认接管
// ============================================================================

// makeWindowInfeasibleConfig 让首个止损后的重入窗口即为空集：恢复门槛 3×ATR
// 远大于 0.5×ATR 的追价带宽，reentryBoundary 必然越过 chaseLimit（复刻 cycle
// 71 高恢复门槛 + 窄追价上限导致的空集，无需真的连续两次止损即可确定性触发）。
func makeWindowInfeasibleConfig(e *Engine) {
	e.config.RiskReentryMinRecoveryATR = 3.0
	e.config.RiskReentryRecoveryEscalation = 1.5
	e.config.RiskReentryMaxChaseATR = 0.5
	e.config.RiskReentryBandATR = 0.5
}

func countGuardEvents(t *testing.T, st *store.Store, cycleID int64, typ string) int {
	t.Helper()
	events, err := st.CopyTrade().ListCopyGuardEvents(cycleID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// long 窗口空集 → ATTEMPTS_EXHAUSTED + 一次性 collapsed 事件 + manual 关闭时不发信号
func TestReentryWindowInfeasibleLongExhausts(t *testing.T) {
	e, st := newReentryTestEngine(t)
	makeWindowInfeasibleConfig(e) // 默认 RiskManualReentryEnabled=false
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1690, 34)
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}

	// 连续两个 tick：第一 tick 判空集转终态并写一次 collapsed；第二 tick 已是终态
	// 且 manual 关闭 → 走终态观察分支，不得重复刷 collapsed 事件。
	e.checkReentryConditions()
	e.checkReentryConditions()

	select {
	case <-e.decisionCh:
		t.Fatal("infeasible window must not emit an auto reentry decision")
	default:
	}
	got, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if got.Status != store.CopyGuardAttemptsExhausted {
		t.Fatalf("infeasible window must flag ATTEMPTS_EXHAUSTED, got %s", got.Status)
	}
	if got.ClosedAt != nil {
		t.Fatal("exhausted cycle must stay open for manual takeover / accounting chain")
	}
	if n := countGuardEvents(t, st, cycle.ID, "REENTRY_WINDOW_COLLAPSED"); n != 1 {
		t.Fatalf("exactly one REENTRY_WINDOW_COLLAPSED audit event expected (idempotent), got %d", n)
	}
	if n, _ := st.CopyTrade().CountManualReentrySignalsForCycle(cycle.ID); n != 0 {
		t.Fatalf("manual reentry disabled: no manual signal must be generated, got %d", n)
	}
}

// short 窗口空集：镜像逻辑（下界 < 上界）同样判定为用尽
func TestReentryWindowInfeasibleShortMirror(t *testing.T) {
	e, st := newReentryTestEngine(t)
	makeWindowInfeasibleConfig(e)
	cycle := seedStoppedCycle(t, st, "trader-1", "short", 1700)
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1710, 34)
	e.leaderState.Positions["ETHUSDT_short"] = &Position{Symbol: "ETHUSDT", Side: SideShort, Size: 1, EntryPrice: 1700, MarkPrice: 1710, MarginMode: "cross", PosID: "leader-pos"}

	e.checkReentryConditions()

	select {
	case <-e.decisionCh:
		t.Fatal("short infeasible window must not emit an auto reentry decision")
	default:
	}
	got, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if got.Status != store.CopyGuardAttemptsExhausted {
		t.Fatalf("short infeasible window must flag ATTEMPTS_EXHAUSTED, got %s", got.Status)
	}
}

// 窗口非空：自动重入旧行为不变（不误判用尽、正常触发决策）
func TestReentryWindowFeasibleKeepsAutoReentry(t *testing.T) {
	e, st := newReentryTestEngine(t)
	e.config.RiskReentryMinRecoveryATR = 0.5
	e.config.RiskReentryMaxChaseATR = 0.5 // 窗口 [1683,1717] 非空
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1690, 34)
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}

	e.checkReentryConditions()
	e.checkReentryConditions()
	e.checkReentryConditions()

	got, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if got.Status == store.CopyGuardAttemptsExhausted {
		t.Fatal("feasible window must not be flagged exhausted")
	}
	if n := countGuardEvents(t, st, cycle.ID, "REENTRY_WINDOW_COLLAPSED"); n != 0 {
		t.Fatalf("feasible window must not emit collapsed event, got %d", n)
	}
	select {
	case <-e.decisionCh:
	default:
		t.Fatal("feasible in-band confirmed ticks must emit a reentry decision")
	}
}

// 冷却期 chaseLimit=0：提前 continue，绝不进入窗口空集判定（避免误转终态）
func TestReentryWindowNotJudgedDuringCooldown(t *testing.T) {
	e, st := newReentryTestEngine(t)
	makeWindowInfeasibleConfig(e)
	e.config.RiskReentryCooldownSeconds = 3600
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1690, 34)
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}

	e.checkReentryConditions()

	got, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if got.Status != store.CopyGuardStoppedWatching {
		t.Fatalf("during cooldown window must not be judged infeasible; want STOPPED_WATCHING got %s", got.Status)
	}
	if n := countGuardEvents(t, st, cycle.ID, "REENTRY_WINDOW_COLLAPSED"); n != 0 {
		t.Fatalf("no collapsed event during cooldown, got %d", n)
	}
}

// v7 退役：即便开关为 true、价格越过追价上限且已回归下界（退役前会生成
// PENDING 人工信号的场景），也永不生成信号、不 emit 决策
func TestRetiredManualModeDoesNotSignalOnRecovery(t *testing.T) {
	e, st := newReentryTestEngine(t)
	e.config.RiskManualReentryEnabled = true
	e.config.RiskMaxReentries = 0 // ReentryCount(0)>=0 → 直接 ATTEMPTS_EXHAUSTED（终态短路）
	e.config.RiskReentryMinRecoveryATR = 0.5
	e.config.RiskReentryMaxChaseATR = 0.5 // 窗口 [1683,1717]
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardAttemptsExhausted, 1700, 1690, 34)
	// markPrice 1750：越过 chase 上限 1717，但已回归下界 1683
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1750, MarginMode: "cross", PosID: "leader-pos"}

	e.checkReentryConditions()
	e.checkReentryConditions()
	e.checkReentryConditions()

	if n, _ := st.CopyTrade().CountManualReentrySignalsForCycle(cycle.ID); n != 0 {
		t.Fatalf("v7 must never create a manual execution signal, got %d", n)
	}
	select {
	case <-e.decisionCh:
		t.Fatal("manualMode must not emit an auto reentry decision (only a human signal)")
	default:
	}
}

// manualMode + 价格未回归下界：即便忽略 chase，仍不得生成人工信号
func TestManualModeNoSignalWhenPriceNotReturned(t *testing.T) {
	e, st := newReentryTestEngine(t)
	e.config.RiskManualReentryEnabled = true
	e.config.RiskMaxReentries = 0
	e.config.RiskReentryMinRecoveryATR = 0.5
	e.config.RiskReentryMaxChaseATR = 0.5
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardAttemptsExhausted, 1700, 1670, 34)
	// markPrice 1670 < 下界 1683 → 未回归
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1670, MarginMode: "cross", PosID: "leader-pos"}

	e.checkReentryConditions()
	e.checkReentryConditions()
	e.checkReentryConditions()

	if n, _ := st.CopyTrade().CountManualReentrySignalsForCycle(cycle.ID); n != 0 {
		t.Fatalf("manualMode must not signal before price returns to boundary, got %d", n)
	}
}

// ============================================================================
// B. 人工重入已于 v7 退役：以下用例复用退役前的场景脚手架（cycle-41 等），
//    断言 RiskManualReentryEnabled=true 也永远不生成人工信号 / 不 emit 决策。
// ============================================================================

// 复刻 cycle-41 场景：3 次止损、名义衰减 100→50→8，自动重入次数用尽。
// v7 退役后即便开关为 true 也不得复活人工信号路径。
func TestRetiredManualModeDoesNotReviveAfterMultipleStops(t *testing.T) {
	e, st := newReentryTestEngine(t)
	e.config.RiskManualReentryEnabled = true // RiskMaxReentries=2（脚手架默认）

	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700) // attempt 0 名义 100 已止损
	// 重入 #1（名义 50）→ 止损；重入 #2（名义 8）→ 止损 → reentry_count=2 用尽
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardReentryPending, cycle.LeaderEntryPrice, cycle.LastObservedPrice, 0)
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err := st.CopyTrade().RecordCopyGuardReentryFilled(cycle, 1705, 50, 50.0/1705, 34, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err := st.CopyTrade().RecordCopyGuardStop(cycle, 34, 1683, -1, 0.05, 0, "algo-1", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardReentryPending, cycle.LeaderEntryPrice, cycle.LastObservedPrice, 0)
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err := st.CopyTrade().RecordCopyGuardReentryFilled(cycle, 1702, 8, 8.0/1702, 34, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err := st.CopyTrade().RecordCopyGuardStop(cycle, 34, 1680, -0.2, 0.01, 0, "algo-2", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	cycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if cycle.ReentryCount != 2 || cycle.StopCount != 3 {
		t.Fatalf("precondition: want reentry_count=2 stop_count=3, got %+v", cycle)
	}

	// ATR fallback 34：边界 1700−17=1683，mark 1690 已回归且带内
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}
	for i := 0; i < 6; i++ {
		e.checkReentryConditions()
	}

	sigs, err := st.CopyTrade().ListManualReentrySignals([]string{"trader-1"}, []string{store.ManualReentryStatusPending}, 10)
	if err != nil || len(sigs) != 0 {
		t.Fatalf("retired manual path must not create signals: %v %v", sigs, err)
	}
	select {
	case <-e.decisionCh:
		t.Fatal("manualMode must not emit an auto reentry decision")
	default:
	}
}

// v7 退役：首仓名义 < 最小下单额（退役前会抬到 门槛×1.2 并发信号的场景），
// 也永不生成兜底金额的人工信号
func TestRetiredManualModeDoesNotCreateMinimumSizedSignal(t *testing.T) {
	e, st := newReentryTestEngine(t)
	e.config.RiskManualReentryEnabled = true
	e.config.RiskMaxReentries = 0 // reentry_count(0)>=0 → 直接用尽（终态短路）
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	// 首仓名义压到 5（< 门槛 10）
	if _, err := st.DB().Exec(`UPDATE copy_guard_attempts SET notional=5 WHERE cycle_id=? AND attempt_no=0`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}
	for i := 0; i < 6; i++ {
		e.checkReentryConditions()
	}

	sigs, err := st.CopyTrade().ListManualReentrySignals([]string{"trader-1"}, []string{store.ManualReentryStatusPending}, 10)
	if err != nil || len(sigs) != 0 {
		t.Fatalf("retired manual path must not create a floored signal: %v %v", sigs, err)
	}
}

func TestConfirmManualReentryIsRetiredWithoutSideEffects(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "override.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ti := NewTraderIntegration("trader-1", flatExecutor{}, st)
	ti.engine = &Engine{config: &CopyConfig{ProviderType: ProviderOKX, LeaderID: "leader", MinTradeWarn: 10}}

	sig, err := st.CopyTrade().SaveManualReentrySignal(&store.CopyGuardManualReentrySignal{
		CycleID: 999, TraderID: "trader-1", LeaderPosID: "leader-pos", Symbol: "ETHUSDT",
		Side: "long", MarginMode: "cross", TriggerPrice: 1690, RecommendedNotional: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := ti.ConfirmManualReentry(sig.ID, "op", 150); !errors.Is(err, ErrManualReentryRetired) {
		t.Fatalf("manual confirmation must return the v7 retirement error, got %v", err)
	}
	got, _ := st.CopyTrade().GetManualReentrySignal(sig.ID)
	if got.Status != store.ManualReentryStatusPending {
		t.Fatalf("retired confirmation must not mutate the signal, got %s", got.Status)
	}
}

func TestAIReentryExecutionFailureReleasesRiskAndReturnsToWaiting(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "ai-execution-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardReentryPending, PolicySnapshot: "{}", AccountEquity: 100, LeaderEntryPrice: 1700, LastObservedPrice: 1690})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := st.ReentryAI().EnsureReentryCandidate(&store.CopyGuardReentryCandidate{CycleID: cycle.ID, TraderID: "trader-1", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", FeatureHash: "enter"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE copy_guard_reentry_candidates SET status=?,last_analysis_id=17,decision_generation=3 WHERE id=?`, store.ReentryCandidateEntryPending, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.ReentryAI().ReserveCopyGuardRisk("trader-1", cycle.ID, 1, 2, 100, .02, .05, .08); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration("trader-1", flatExecutor{}, st)
	ti.engine = &Engine{config: &CopyConfig{RiskPolicyVersion: 7, RiskReentryDecisionMode: "ai_guarded", RiskAIMinReviewSeconds: 300}}
	ti.handleReentryExecutionFailure(cycle, errors.New("exchange rejected order"))

	usage, err := st.ReentryAI().GetCopyGuardRiskUsage("trader-1", cycle.ID)
	if err != nil || usage.CycleUsedUSD != 0 || usage.PortfolioUsedUSD != 0 {
		t.Fatalf("failed order leaked risk reservation: usage=%+v err=%v", usage, err)
	}
	freshCandidate, _ := st.ReentryAI().GetReentryCandidate(candidate.ID)
	freshCycle, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if freshCandidate.Status != store.ReentryCandidateWaiting || freshCandidate.FailureCount != 0 || freshCycle.Status != store.CopyGuardAIWaiting {
		t.Fatalf("failed execution did not return to AI waiting: candidate=%+v cycle=%+v", freshCandidate, freshCycle)
	}
}

func TestAIReentryAmbiguousErrorsNeverReleaseForRetry(t *testing.T) {
	if !isAmbiguousReentryExecutionError(errors.New("post order request timeout")) {
		t.Fatal("post-submit timeout must be treated as an uncertain order")
	}
	if isAmbiguousReentryExecutionError(fmt.Errorf("%w: price drift", errAIReentryOrderPreflight)) {
		t.Fatal("deterministic final preflight rejection is known to be pre-order")
	}
	if isAmbiguousReentryExecutionError(errors.New("failed to get positions: network timeout")) {
		t.Fatal("pre-order position read failure is safe to retry")
	}
}

func TestAIReentryFinalPreflightUsesPersistedDisableBarrier(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "ai-disable-barrier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance)
		VALUES('trader-1','test','','exchange-1',100)`); err != nil {
		t.Fatal(err)
	}
	cfg := store.NewCopyGuardDefaults()
	cfg.TraderID = "trader-1"
	cfg.ProviderType = string(ProviderOKX)
	cfg.LeaderID = "leader"
	cfg.Enabled = true
	cfg.RiskPolicyVersion = 7
	cfg.RiskStopLossEnabled = false
	cfg.RiskReentryEnabled = true
	cfg.RiskReentryDecisionMode = "ai_guarded"
	if err := st.CopyTrade().Upsert(cfg); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration("trader-1", flatExecutor{}, st)
	ti.engine = &Engine{config: &CopyConfig{
		RiskPolicyVersion:       7,
		RiskStopLossEnabled:     true, // stale pre-save runtime snapshot
		RiskReentryEnabled:      true,
		RiskReentryDecisionMode: "ai_guarded",
	}}
	err = ti.validateAIReentryImmediatelyBeforeOrder(&decision.Decision{})
	if !errors.Is(err, errAIReentryOrderPreflight) || ReasonCodeOf(err) != "EXECUTION_DISABLED" {
		t.Fatalf("persisted stop disable must block an in-flight AI order, got code=%s err=%v", ReasonCodeOf(err), err)
	}
}

func TestAIReentryAmbiguousFailureKeepsLeaseAndReservation(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "ai-ambiguous-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardReentryPending, PolicySnapshot: "{}", AccountEquity: 100, LeaderEntryPrice: 1700, LastObservedPrice: 1690})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.CopyTrade().UpdateCopyGuardPendingOrder(cycle.ID, "cgr-uncertain")
	candidate, err := st.ReentryAI().EnsureReentryCandidate(&store.CopyGuardReentryCandidate{CycleID: cycle.ID, TraderID: "trader-1", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", FeatureHash: "enter"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE copy_guard_reentry_candidates SET status=? WHERE id=?`, store.ReentryCandidateEntryPending, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.ReentryAI().ReserveCopyGuardRisk("trader-1", cycle.ID, 1, 2, 100, .02, .05, .08); err != nil {
		t.Fatal(err)
	}
	ti := NewTraderIntegration("trader-1", flatExecutor{}, st)
	ti.engine = &Engine{config: &CopyConfig{RiskPolicyVersion: 7, RiskReentryDecisionMode: "ai_guarded", RiskAIMinReviewSeconds: 300}}
	freshCycle, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	ti.handleReentryExecutionFailure(freshCycle, errors.New("exchange response timeout after submit"))

	usage, _ := st.ReentryAI().GetCopyGuardRiskUsage("trader-1", cycle.ID)
	freshCandidate, _ := st.ReentryAI().GetReentryCandidate(candidate.ID)
	freshCycle, _ = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if usage.PortfolioUsedUSD != 2 || freshCandidate.Status != store.ReentryCandidateEntryPending || freshCycle.Status != store.CopyGuardReentryPending {
		t.Fatalf("uncertain order became retryable: usage=%+v candidate=%s cycle=%s", usage, freshCandidate.Status, freshCycle.Status)
	}
	if countGuardEvents(t, st, cycle.ID, "REENTRY_RECOVERY_PENDING") != 1 {
		t.Fatal("uncertain order must emit one recovery-pending audit event")
	}
}

func TestCopyGuardCandidateStatusMapsToCycleState(t *testing.T) {
	cases := map[string]string{
		store.ReentryCandidateWatching:        store.CopyGuardAIWatching,
		store.ReentryCandidateReviewing:       store.CopyGuardAIReviewing,
		store.ReentryCandidateWaiting:         store.CopyGuardAIWaiting,
		store.ReentryCandidatePaused:          store.CopyGuardAIWaiting,
		store.ReentryCandidateEntryPending:    store.CopyGuardReentryPending,
		store.ReentryCandidateAbandoned:       store.CopyGuardAIAbandoned,
		store.ReentryCandidateInvalidated:     store.CopyGuardAIAbandoned,
		store.ReentryCandidateExpired:         store.CopyGuardWatchTimeout,
		store.ReentryCandidateBudgetSuspended: store.CopyGuardBudgetSuspended,
	}
	for candidate, want := range cases {
		if got := copyGuardCycleStatusForCandidate(candidate); got != want {
			t.Fatalf("candidate %s mapped to %s, want %s", candidate, got, want)
		}
	}
}

// 纯函数：窗口可行性判定（long/short 镜像 + chaseLimit<=0 不判定 + epsilon 容差）
func TestReentryWindowInfeasibleHelper(t *testing.T) {
	cases := []struct {
		name            string
		side            string
		boundary, chase float64
		anchor          float64
		want            bool
	}{
		{"long empty set", "long", 1790, 1784, 1779, true},
		{"long feasible", "long", 1683, 1717, 1700, false},
		{"short empty set", "short", 1564, 1683, 1700, true},
		{"short feasible", "short", 1717, 1564, 1700, false},
		{"cooldown no chase", "long", 1790, 0, 1700, false},
		{"within epsilon not infeasible", "long", 1700.0000001, 1700, 1700, false},
	}
	for _, c := range cases {
		if got := reentryWindowInfeasible(c.side, c.boundary, c.chase, c.anchor); got != c.want {
			t.Fatalf("%s: reentryWindowInfeasible=%v want %v", c.name, got, c.want)
		}
	}
}
