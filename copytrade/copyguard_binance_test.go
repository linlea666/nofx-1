package copytrade

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

// TestSupportsCopyGuardPredicate 锁定 Copy Guard 支持的领航员数据源集合：
// OKX 与 Binance 支持，Hyperliquid 不支持。放开 Binance 后其余门槛统一走
// 此谓词，避免散落的 `== ProviderOKX` 硬编码重新蔓延。
func TestSupportsCopyGuardPredicate(t *testing.T) {
	cases := map[ProviderType]bool{
		ProviderOKX:         true,
		ProviderBinance:     true,
		ProviderHyperliquid: false,
	}
	for provider, want := range cases {
		if got := SupportsCopyGuard(provider); got != want {
			t.Fatalf("SupportsCopyGuard(%s)=%v want %v", provider, got, want)
		}
	}
}

// TestValidateRiskPolicyV4AcceptsBinance 校验 v4 策略校验对 Binance 放行，
// 且仍拒绝 Hyperliquid。
func TestValidateRiskPolicyV4AcceptsBinance(t *testing.T) {
	base := func(p ProviderType) *CopyConfig {
		c := &CopyConfig{
			ProviderType:               p,
			RiskPolicyVersion:          4,
			RiskStopMode:               "volatility_priority",
			RiskATRPeriod:              14,
			RiskATRCacheMaxAgeMinutes:  120,
			RiskATRMultiplier:          2.0,
			RiskATRFallbackPct:         0.02,
			RiskTriggerPriceType:       "mark",
			RiskAccountPct:             0.10,
			RiskLiquidationBufferATR:   0.5,
			RiskReentryRatio:           0.5,
			RiskReentryMaxATRExpansion: 2,
		}
		return c
	}
	if err := ValidateRiskPolicyV4(base(ProviderBinance)); err != nil {
		t.Fatalf("binance v4 policy should validate, got %v", err)
	}
	if err := ValidateRiskPolicyV4(base(ProviderHyperliquid)); err == nil {
		t.Fatalf("hyperliquid v4 policy must be rejected")
	}
}

// TestShouldManageStopLossGating 校验 shouldManageStopLoss 的三重门槛：
//   - 数据源支持（binance 通过，hyperliquid 拒绝）
//   - v4+（version=0 的存量 binance 配置即使 SL 开关默认为 true 也不进入保护单路径）
//   - RiskStopLossEnabled
func TestShouldManageStopLossGating(t *testing.T) {
	dec := &decision.Decision{Symbol: "ETHUSDT", Action: "open_long"}

	newTI := func(p ProviderType, version int, enabled bool) *TraderIntegration {
		return &TraderIntegration{
			traderID: "t",
			engine: &Engine{config: &CopyConfig{
				ProviderType:        p,
				RiskPolicyVersion:   version,
				RiskStopLossEnabled: enabled,
			}},
		}
	}

	if newTI(ProviderBinance, 4, true).shouldManageStopLoss(dec) != true {
		t.Fatalf("binance v4 + SL enabled should manage stop loss")
	}
	// 存量 binance：SL 开关默认 true 但 version=0 → 不得进入保护单路径
	if newTI(ProviderBinance, 0, true).shouldManageStopLoss(dec) != false {
		t.Fatalf("legacy binance (version 0) must not manage stop loss")
	}
	if newTI(ProviderBinance, 4, false).shouldManageStopLoss(dec) != false {
		t.Fatalf("binance with SL disabled must not manage stop loss")
	}
	if newTI(ProviderHyperliquid, 4, true).shouldManageStopLoss(dec) != false {
		t.Fatalf("hyperliquid must never manage stop loss")
	}
	if newTI(ProviderOKX, 4, true).shouldManageStopLoss(dec) != true {
		t.Fatalf("okx v4 + SL enabled should manage stop loss (unchanged)")
	}
}

// TestBinanceStoppedByRiskDetectedForV4 校验 Binance 领航员数据源在 v4 配置下，
// 跟随者本地仓位消失但领航员仍持仓时能识别为账户保护止损触发。
// 这是 Copy Guard 的核心对账路径，放开 Binance 后必须与 OKX 行为一致。
func TestBinanceStoppedByRiskDetectedForV4(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.RiskPolicyVersion = 4
	e.config.RiskStopLossEnabled = true
	e.stopRiskSuspectCount = make(map[string]int)

	saveActiveMapping(t, st, posID, 0.02)
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: e.traderID, LeaderID: e.config.LeaderID, LeaderPosID: posID,
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot: "{}", LeaderEntryPrice: 2096.58, FollowerEntryPrice: 2096.58,
		FollowerNotional: 41.9316, AccountEquity: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 2096.58, 41.9316, .02, 20); err != nil {
		t.Fatal(err)
	}

	// 领航员仍持有该仓位
	e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.02)

	// 跟随者本地无任何仓位（模拟止损已被执行交易所触发平掉）
	e.getFollowerPositionsResult = func() (map[string]*Position, error) {
		return map[string]*Position{}, nil
	}

	// 连续确认阈值次数后应标记为 stopped_by_risk
	for i := 0; i < stopRiskSuspectThreshold; i++ {
		e.checkStoppedByRisk()
	}

	mapping, err := st.CopyTrade().GetActiveMapping(e.traderID, posID)
	if err == nil && mapping != nil {
		t.Fatalf("mapping should no longer be active after stop-by-risk, got %+v", mapping)
	}
	stopped, err := st.CopyTrade().ListStoppedByRiskMappings(e.traderID)
	if err != nil {
		t.Fatalf("list stopped mappings: %v", err)
	}
	if len(stopped) != 1 || stopped[0].LeaderPosID != posID {
		t.Fatalf("expected posId %s marked stopped_by_risk, got %+v", posID, stopped)
	}
	cycle, err = st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil || cycle.Status != store.CopyGuardStoppedWatching || cycle.StopCount != 1 {
		t.Fatalf("stop ledger was not committed before mapping transition: cycle=%+v err=%v", cycle, err)
	}
}

// TestBinancePosIDReuseCreatesFreshCycle 校验 Binance 合成 posId（userId_symbol_side
// 跨生命周期复用）在旧周期关闭后，同一 posId 再开仓会创建全新的 Copy Guard 周期，
// 而不是复用已关闭的旧周期。这是 Binance 与 OKX（每仓位生命周期 posId 唯一）的
// 关键语义差异，必须由 EnsureCopyGuardCycle 的 closed_at 判定正确处理。
func TestBinancePosIDReuseCreatesFreshCycle(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cs := st.CopyTrade()

	const posID = "1239518824_ETHUSDT_LONG"
	first, err := cs.EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "t", LeaderID: "leader", LeaderPosID: posID, Symbol: "ETHUSDT",
		Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot: "{}", LeaderEntryPrice: 2000, FollowerEntryPrice: 2000,
		FollowerNotional: 100, AccountEquity: 1000, LastObservedPrice: 2000,
	})
	if err != nil {
		t.Fatalf("ensure first cycle: %v", err)
	}

	// 关闭第一个周期（领航员平仓 → 跟随平仓）
	if err := cs.CloseCopyGuardCycle(first.ID, store.CopyGuardLeaderClosed, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("close first cycle: %v", err)
	}

	// 关闭后不得再把已关闭周期当作 open 复用
	if _, err := cs.GetOpenCopyGuardCycle("t", posID); err == nil {
		t.Fatalf("closed cycle must not be returned as open for reused posId")
	}

	// 同一 posId 再次开仓 → 必须是新周期。
	// UNIQUE(trader_id, leader_pos_id, opened_at) 用 opened_at 区分同 posId 的多个
	// 生命周期；生产中 Binance 快照轮询间隔 3s（秒级时间戳）保证重开的 opened_at
	// 不同。测试用 >1s 的等待模拟这一真实时序，避免同秒时间戳撞唯一键。
	time.Sleep(1100 * time.Millisecond)

	// 同一 posId 再次开仓 → 必须是新周期
	second, err := cs.EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "t", LeaderID: "leader", LeaderPosID: posID, Symbol: "ETHUSDT",
		Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot: "{}", LeaderEntryPrice: 2100, FollowerEntryPrice: 2100,
		FollowerNotional: 120, AccountEquity: 1000, LastObservedPrice: 2100,
	})
	if err != nil {
		t.Fatalf("ensure second cycle: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("reused closed cycle %d for reopened posId; expected a fresh cycle", first.ID)
	}
}
