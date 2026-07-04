package copytrade

import (
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
	positions []map[string]interface{}
}

func (e *stopMgrExecutor) GetPositions() ([]map[string]interface{}, error) { return e.positions, nil }

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
	if err := st.CopyTrade().RecordCopyGuardStopObserved(cycle.ID, "trader-1", 0, 7.59, 1700, 1, -1.1, map[string]interface{}{"confirmation": "position_absent_fallback"}); err != nil {
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
// 重入判定：首轮观察不判穿越（B7）；穿越后正常触发；失败不消耗次数
// ============================================================================

func TestReentryFirstTickGuardAndCrossing(t *testing.T) {
	e, st := newReentryTestEngine(t)
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	// RecordCopyGuardStop 未写 last_observed_price → 首轮为 0
	if cycle.LastObservedPrice != 0 {
		t.Fatalf("precondition: LastObservedPrice must start at 0, got %v", cycle.LastObservedPrice)
	}
	// ATR fallback = 1700*0.02 = 34；band=0.5 → boundary = 1700-17 = 1683
	// 标记价 1690 已在带内：首轮（last=0）必须只记录观测、不触发
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

	// 价格先跌出带外（1660 < 1683），下一轮记录带外观测
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1660
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
		t.Fatal("below-boundary tick must not trigger reentry")
	default:
	}

	// 再穿回带内（1660 → 1690 穿越 1683）→ 触发重入决策 + REENTRY_PENDING
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1690
	e.checkReentryConditions()
	select {
	case full := <-e.decisionCh:
		if len(full.Decisions) != 1 || full.Decisions[0].Symbol != "ETHUSDT" {
			t.Fatalf("unexpected reentry decision: %+v", full.Decisions)
		}
	default:
		t.Fatal("band crossing must emit a reentry decision")
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
		t.Fatal("band crossing must emit a reentry decision")
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

	// 1690 穿越了旧带宽边界 1683，但未达到恢复下限 1700 → 不得重入
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
		t.Fatal("crossing the band without min recovery must not trigger reentry")
	default:
	}

	// 恢复到 1705（≥1700 且 ≤ 追价上限 1700+68）→ 触发
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1705
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
	default:
		t.Fatal("crossing the recovery-raised boundary must trigger reentry")
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

	// 按摊低后的实时均价算边界是 1650−17=1633（1620→1640 会穿越）；
	// 保守锚点必须用快照 1700 → 边界 1683，1640 不得触发
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
		t.Fatal("averaged-down live entry must not loosen the reentry boundary")
	default:
	}

	// 真实恢复到 1690（穿越 1683，≤ 锚点 1700+68）→ 触发
	e.leaderState.Positions["ETHUSDT_long"].MarkPrice = 1690
	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
	default:
		t.Fatal("crossing the snapshot-anchored boundary must trigger reentry")
	}
}

// ============================================================================
// v4.1 周期累计亏损熔断：已实现亏损触达权益比例后不再重入
// ============================================================================

func TestCycleLossBreakerBlocksReentry(t *testing.T) {
	e, st := newReentryTestEngine(t)
	e.config.RiskCycleMaxLossPct = 0.10 // 权益 100 → 熔断线 −10
	cycle := seedStoppedCycle(t, st, "trader-1", "long", 1700)
	if _, err := st.DB().Exec(`UPDATE copy_guard_cycles SET actual_pnl=-12 WHERE id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	_ = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, 1700, 1660, 34)
	e.leaderState.Positions["ETHUSDT_long"] = &Position{Symbol: "ETHUSDT", Side: SideLong, Size: 1, EntryPrice: 1700, MarkPrice: 1690, MarginMode: "cross", PosID: "leader-pos"}

	e.checkReentryConditions()
	select {
	case <-e.decisionCh:
		t.Fatal("cycle loss breaker must block reentry even when the price condition holds")
	default:
	}
	got, _ := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if got.Status != store.CopyGuardCycleLossCapped {
		t.Fatalf("cycle must be flagged CYCLE_LOSS_CAPPED: %+v", got)
	}
	if got.ClosedAt != nil {
		t.Fatalf("capped cycle stays open until the leader closes: %+v", got)
	}
	events, err := st.CopyTrade().ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == "CYCLE_LOSS_BREAKER" {
			found = true
		}
	}
	if !found {
		t.Fatal("CYCLE_LOSS_BREAKER event must be recorded")
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
	ti.upsertV4Protection(dec, "long", 0.128, 1717.33, &StopLossCalcResult{SLPrice: 1711.63, TickSize: 0.01})

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
