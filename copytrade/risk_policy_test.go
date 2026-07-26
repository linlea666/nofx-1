package copytrade

import (
	"math"
	"testing"

	"nofx/store"
)

func TestComputeRiskDistanceV4ATRGovernsWhenCapsAreWider(t *testing.T) {
	// 账户线 0.2 → 账户距离 = 1000×0.2/1000×100 = 20，远宽于 ATR 3 → ATR 主导。
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.2, RiskSlippageBufferBPS: 10, RiskATRCacheMaxAgeMinutes: 120}
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Distance != 3 || r.GovernedBy != "atr" {
		t.Fatalf("unexpected result: %+v", r)
	}
	// 3% of 1000 plus 10bps = 31 USD. Leverage must not enter this formula.
	if math.Abs(r.ExpectedLossUSD-31) > 1e-9 {
		t.Fatalf("expected 31, got %f", r.ExpectedLossUSD)
	}
}

func TestComputeRiskDistanceV4AccountCapReportsNoiseConflict(t *testing.T) {
	// 账户线 1%：距离 = 1000×0.01/2000×100 = 0.5 < ATR 3 → 账户 cap 主导且
	// 落在噪音区内。v5 起账户线在任何 stop mode 下都是硬 cap。
	c := &CopyConfig{RiskPolicyVersion: 4, RiskStopMode: "account_hard_limit", RiskAccountPct: 0.01}
	r, err := ComputeRiskDistanceV4(c, 100, 2000, 1000, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account_cap" || !r.NoiseConflict {
		t.Fatalf("unexpected result: %+v", r)
	}
}

// TestComputeRiskDistanceV4MarginCapBoundsStopDistance reproduces the WLD
// incident: 20x leverage with an ATR distance of ~3% of price meant the stop
// only fired around -60% margin loss. With the margin-loss cap the distance is
// bounded by entry × maxLoss / leverage regardless of how wide the ATR is.
func TestComputeRiskDistanceV4MarginCapBoundsStopDistance(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.2, RiskLeverageFallback: true, RiskLeverageMaxLoss: 0.3}
	// entry=100, leverage=20, maxLoss=30% → cap distance = 100×0.3/20 = 1.5 < ATR 3.
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 20)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-1.5) > 1e-9 || r.GovernedBy != "margin_cap" || !r.NoiseConflict {
		t.Fatalf("expected margin_cap distance 1.5 with noise conflict, got %+v", r)
	}
	// Cap wider than ATR must not loosen the stop.
	r, err = ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 5) // cap = 100×0.3/5 = 6 > 3
	if err != nil {
		t.Fatal(err)
	}
	if r.Distance != 3 || r.GovernedBy != "atr" {
		t.Fatalf("expected atr-governed distance 3, got %+v", r)
	}
	// Disabled fallback keeps pure ATR behaviour.
	c.RiskLeverageFallback = false
	r, err = ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 20)
	if err != nil {
		t.Fatal(err)
	}
	if r.Distance != 3 || r.GovernedBy != "atr" {
		t.Fatalf("expected atr-governed distance with fallback disabled, got %+v", r)
	}
}

// TestComputeRiskDistanceV4NoLoosening：v5 移除噪音下限——高杠杆下保证金 cap
// 塌缩到噪音区内（100x → 0.3% 价距）时，任何机制都不得把它放宽（v4.1 的
// noise_floor 曾放宽到 2×ATR，是实盘 -40% 止损的直接根因）。宽度问题由重入
// 噪音档（distance/ATR < 0.3 默认禁入）与 UI 易扫损提示处理。
func TestComputeRiskDistanceV4NoLoosening(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.2, RiskLeverageFallback: true, RiskLeverageMaxLoss: 0.3, RiskATRMultiplier: 1.5}
	// entry=100, leverage=100 → margin cap distance = 100×0.3/100 = 0.3
	// atrDistance=3（raw ATR 2 × multiplier 1.5）
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.3) > 1e-9 || r.GovernedBy != "margin_cap" || !r.NoiseConflict {
		t.Fatalf("tight margin cap must never be loosened, got %+v", r)
	}
	// distance/ATR = 0.3/2 = 0.15 < 0.3 → 易扫损配置，重入噪音档的输入
	if math.Abs(r.DistanceATRRatio-0.15) > 1e-9 {
		t.Fatalf("DistanceATRRatio must expose the noise tier input, got %+v", r)
	}
	// 保证金口径：损失 = 0.3% 名义 = 3 USD，保证金 = 1000/100 = 10 → 30%
	if math.Abs(r.ExpectedMarginLossPct-0.3) > 1e-9 {
		t.Fatalf("ExpectedMarginLossPct must be loss/margin, got %+v", r)
	}
}

// TestComputeRiskDistanceV4AccountHardBackstopCapsEveryMode verifies that
// RiskAccountPct is a catastrophe backstop in every stop mode.
func TestComputeRiskDistanceV4AccountHardBackstopCapsEveryMode(t *testing.T) {
	// volatility_priority, notional 2000 vs equity 1000, account pct 1% →
	// account distance = 1000×0.01/2000×100 = 0.5 < ATR 3.
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.01}
	r, err := ComputeRiskDistanceV4(c, 100, 2000, 1000, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account_cap" || !r.NoiseConflict {
		t.Fatalf("account backstop must cap the distance in volatility_priority, got %+v", r)
	}
	// 账户线也压过保证金 cap：100x margin cap 0.3 < 0.5 → margin_cap 主导；
	// 若保证金 cap 更宽（10x → 3），账户线 0.5 接管。
	c.RiskLeverageFallback, c.RiskLeverageMaxLoss = true, 0.3
	r, err = ComputeRiskDistanceV4(c, 100, 2000, 1000, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account_cap" {
		t.Fatalf("account backstop must outrank a wider margin cap, got %+v", r)
	}
}

// TestComputeRiskDistanceV4DefaultMarginCapOffResistsNoise 复现截图 ETH 场景：
// v5.2 默认关闭 margin_cap（RiskLeverageFallback=false）后，高杠杆止损不再被
// entry×maxLoss/lev 压进噪音区，改由 min(k×ATR, account_cap) 决定；account_cap
// 始终参与 min 作单笔硬兜底。
func TestComputeRiskDistanceV4DefaultMarginCapOffResistsNoise(t *testing.T) {
	// entry=1766, 100x, notional=859, equity=90, ATR raw≈10.6, k=2.0
	// atrDistance = 10.6×2.0 = 21.2；account_cap = 90×0.10/859×1766 ≈ 18.51
	// margin_cap（若开启）= 1766×0.2/100 = 3.532（噪音区）
	const entry, notional, equity = 1766.0, 859.0, 90.0
	const atrDistance = 21.2
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.10, RiskLeverageMaxLoss: 0.2, RiskATRMultiplier: 2.0, RiskLeverageFallback: false}

	r, err := ComputeRiskDistanceV4(c, entry, notional, equity, atrDistance, 100)
	if err != nil {
		t.Fatal(err)
	}
	accountCap := equity * 0.10 / notional * entry // ≈ 18.51
	if math.Abs(r.Distance-accountCap) > 1e-6 || r.GovernedBy != "account_cap" {
		t.Fatalf("默认关 margin_cap 时应由 account_cap(≈%.2f) 主导，got %+v", accountCap, r)
	}
	// 抗噪目标：止损距离远大于坍缩的 margin_cap 3.53 点
	if r.Distance <= 3.532 {
		t.Fatalf("止损距离必须显著大于坍缩的 margin_cap 3.53，got %.4f", r.Distance)
	}

	// 对照组：显式开启 margin_cap → 恢复旧的坍缩行为（0.2% ≈ 3.53 点）
	c.RiskLeverageFallback = true
	r2, err := ComputeRiskDistanceV4(c, entry, notional, equity, atrDistance, 100)
	if err != nil {
		t.Fatal(err)
	}
	marginCap := entry * 0.2 / 100 // ≈ 3.532
	if math.Abs(r2.Distance-marginCap) > 1e-6 || r2.GovernedBy != "margin_cap" {
		t.Fatalf("显式开启 margin_cap 应恢复坍缩距离(≈%.2f)，got %+v", marginCap, r2)
	}
}

// TestComputeRiskDistanceV4Cycle41GoldNoiseResistance 用 cycle-41（XAU）真实参数离线核验：
// 实盘 k=1.5 时 SL 距离 7.59 点，被 ~8 点摆动扫了 3 次。k=2.0 后止损距离应 >9.4 点，
// 且不被 account_cap / margin_cap 压回噪音区（两者对该小仓都极宽）。
func TestComputeRiskDistanceV4Cycle41GoldNoiseResistance(t *testing.T) {
	// 日志实测：atr_value=5.0609367，entry=4180.4，notional=20.902，equity=102.596，leverage=50
	const rawATR, entry, notional, equity, leverage = 5.060936717414582, 4180.4, 20.902, 102.59603407481568, 50
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.10, RiskLeverageMaxLoss: 0.2, RiskATRMultiplier: 2.0, RiskLeverageFallback: false}

	r, err := ComputeRiskDistanceV4(c, entry, notional, equity, rawATR*2.0, leverage)
	if err != nil {
		t.Fatal(err)
	}
	if r.GovernedBy != "atr" {
		t.Fatalf("小仓下 account_cap/margin_cap 都极宽，应由 ATR 主导，got %+v", r)
	}
	// k=2.0 → 10.12 点 > 实测 ~9.4 点摆动，不再被扫
	if r.Distance <= 9.4 {
		t.Fatalf("k=2.0 止损距离必须 >9.4 点摆动，got %.4f", r.Distance)
	}
	// 对照旧 k=1.5：7.59 点（会被扫）
	if r.Distance <= 7.591405076121873 {
		t.Fatalf("k=2.0 距离必须显著大于旧 1.5×ATR 的 7.59 点，got %.4f", r.Distance)
	}
}

func TestValidateRiskPolicyV4(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskATRPeriod: 14, RiskATRCacheMaxAgeMinutes: 120, RiskATRMultiplier: 1.5, RiskATRFallbackPct: 0.02, RiskTriggerPriceType: "mark", RiskAccountPct: 0.02, RiskLiquidationBufferATR: 0.5, RiskMaxReentries: 2, RiskReentryRatio: 0.5, RiskReentryBandATR: 0.5, RiskReentryCooldownSeconds: 60, RiskReentryMaxATRExpansion: 2}
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatal(err)
	}
	// v5：不可保护处置模式仅接受 close/follow
	c.RiskUnprotectableAction = "panic"
	if err := ValidateRiskPolicyV4(c); err == nil {
		t.Fatal("invalid risk_unprotectable_action must be rejected")
	}
	c.RiskUnprotectableAction = "follow"
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatal(err)
	}
	c.RiskReentryMinNotional = -1
	if err := ValidateRiskPolicyV4(c); err == nil {
		t.Fatal("negative explicit reentry minimum must be rejected")
	}
}

func TestValidateRiskPolicyAIReviewLimitsAreIndependent(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 7}
	c.FillRiskDefaults()
	c.RiskAIDailyCallLimit = 12
	c.RiskAILifecycleCallLimit = 5
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatalf("lifecycle limit may be lower than the rolling daily ceiling: %v", err)
	}
	c.RiskAIDailyCallLimit = 13
	if err := ValidateRiskPolicyV4(c); err == nil {
		t.Fatal("daily limit above 12 must be rejected")
	}
	c.RiskAIDailyCallLimit = 12
	c.RiskAILifecycleCallLimit = 31
	if err := ValidateRiskPolicyV4(c); err == nil {
		t.Fatal("lifecycle limit above 30 must be rejected")
	}
}

func TestBuildProtectionPlanKeepsFeesAndSlippageInsideBudget(t *testing.T) {
	c := &CopyConfig{RiskATRMultiplier: 2, RiskSlippageBufferBPS: 10, RiskRoundTripFeeBPS: 12}
	plan, err := BuildProtectionPlan(c, SideLong, 100, 2, 97, 1000, 0.02, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.StopDistance != 4 || plan.StopPrice != 96 {
		t.Fatalf("ATR/structure stop changed unexpectedly: %+v", plan)
	}
	if plan.ExpectedLossUSD > 20+1e-9 || plan.MaxNotional <= 0 {
		t.Fatalf("fees and slippage must remain inside 2%% budget: %+v", plan)
	}
	// A narrower account budget must resize notional, never squeeze the 2x ATR stop.
	smaller, err := BuildProtectionPlan(c, SideLong, 100, 2, 97, 1000, 0.01, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if smaller.StopDistance != plan.StopDistance || smaller.MaxNotional >= plan.MaxNotional {
		t.Fatalf("budget must shrink quantity without moving stop: old=%+v new=%+v", plan, smaller)
	}
}

func TestBuildProtectionPlanRejectsStructureBeyondFourATR(t *testing.T) {
	c := &CopyConfig{RiskATRMultiplier: 2, RiskSlippageBufferBPS: 10, RiskRoundTripFeeBPS: 12}
	if _, err := BuildProtectionPlan(c, SideLong, 100, 2, 90, 1000, 0.02, 1000); err == nil {
		t.Fatal("an invalidation beyond 4 ATR must reject entry instead of squeezing the stop")
	}
}

func TestMaxNotionalRiskFrictionBoundary(t *testing.T) {
	c := &CopyConfig{RiskSlippageBufferBPS: 10, RiskRoundTripFeeBPS: 12}
	notional, err := MaxNotionalForRiskDistance(c, 500, 2000, 40, 0.02, 100000)
	if err != nil {
		t.Fatal(err)
	}
	loss := notional * (40.0/2000.0 + 22.0/10000.0)
	if math.Abs(loss-10) > 1e-9 {
		t.Fatalf("planned loss %.12f must exactly respect $10 budget", loss)
	}
}

func TestAvailableCopyGuardRiskUsesSmallestRemainingBudget(t *testing.T) {
	c := &CopyConfig{RiskAccountPct: .02, RiskCycleLossBudgetPct: .05, RiskPortfolioLossBudgetPct: .08}
	available, err := AvailableCopyGuardRiskUSD(c, 1000, store.CopyGuardRiskUsage{CycleUsedUSD: 34, PortfolioUsedUSD: 65})
	if err != nil {
		t.Fatal(err)
	}
	// attempt=20, cycle remaining=16, portfolio remaining=15.
	if math.Abs(available-15) > 1e-9 {
		t.Fatalf("available risk = %.8f, want 15", available)
	}
	if _, err := AvailableCopyGuardRiskUSD(c, 1000, store.CopyGuardRiskUsage{PortfolioUsedUSD: 80}); err == nil {
		t.Fatal("exhausted portfolio budget must reject sizing")
	}
}
