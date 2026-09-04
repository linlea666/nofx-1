package copytrade

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"nofx/store"
)

func TestLifecyclePolicyDecodesHistoricalRuntimeCopyConfig(t *testing.T) {
	runtime := &CopyConfig{
		ProviderType:                 ProviderBinance,
		LeaderID:                     "leader-private",
		CopyRatio:                    3,
		BinanceP20T:                  "cookie-secret",
		BinanceCSRFToken:             "csrf-secret",
		RiskPolicyVersion:            4,
		RiskProtectionMode:           store.RiskProtectionModePositionMarginPct,
		RiskPositionMarginStopPct:    0.8,
		RiskTriggerPriceType:         "mark",
		RiskLiquidationBufferATR:     0.75,
		RiskReentryEnabled:           false,
		RiskReentryDecisionMode:      "disabled",
		RiskMaxReentries:             0,
		RiskReentryCooldownSeconds:   777,
		RiskReentryMaxATRExpansion:   1.8,
		RiskUnprotectableDisposition: "warn",
	}
	raw, err := json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	cycle := &store.CopyGuardCycle{PolicySnapshot: string(raw)}

	mode, pct := copyGuardProtectionSettings(cycle, nil)
	if mode != store.RiskProtectionModePositionMarginPct || pct != 0.8 {
		t.Fatalf("historical runtime snapshot decoded incorrectly: mode=%q pct=%v", mode, pct)
	}
	got := copyGuardLifecycleConfig(cycle, &CopyConfig{})
	if got.RiskPolicyVersion != 4 || got.RiskLiquidationBufferATR != 0.75 ||
		got.RiskReentryCooldownSeconds != 777 || got.RiskReentryMaxATRExpansion != 1.8 {
		t.Fatalf("historical runtime snapshot lost risk fields: %+v", got)
	}
	canonical, err := store.CanonicalizeCopyGuardPolicySnapshot(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(canonical, "cookie-secret") || strings.Contains(canonical, "csrf-secret") ||
		strings.Contains(canonical, "leader-private") || strings.Contains(canonical, "copy_ratio") {
		t.Fatalf("canonical snapshot leaked ordinary or credential fields: %s", canonical)
	}
}

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

// TestComputeRiskDistanceV4ExplicitMarginCapOffResistsNoise 复现截图 ETH 场景：
// 用户显式关闭 margin_cap（RiskLeverageFallback=false）后，高杠杆止损不再被
// entry×maxLoss/lev 压进噪音区，改由 min(k×ATR, account_cap) 决定；account_cap
// 始终参与 min 作单笔硬兜底。
func TestComputeRiskDistanceV4ExplicitMarginCapOffResistsNoise(t *testing.T) {
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
		t.Fatalf("显式关闭 margin_cap 时应由 account_cap(≈%.2f) 主导，got %+v", accountCap, r)
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

func TestValidateRiskPolicyAICostWarningsHaveNoHardUpperBound(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 7}
	c.FillRiskDefaults()
	c.RiskAIDailyCallLimit = 12
	c.RiskAILifecycleCallLimit = 5
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatalf("independent soft warning thresholds must remain compatible: %v", err)
	}
	c.RiskAIDailyCallLimit = 100000
	c.RiskAILifecycleCallLimit = 200000
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatalf("soft warning thresholds must not retain a hard upper bound: %v", err)
	}
	// Zero keeps the long-standing "use default" compatibility semantics.
	// A negative value is the explicit invalid case.
	c.RiskAIDailyCallLimit = -1
	if err := ValidateRiskPolicyV4(c); err == nil {
		t.Fatal("non-positive compatibility warning threshold must be rejected")
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

func TestBuildAIProtectionPlanFromStopContract(t *testing.T) {
	cfg := &CopyConfig{RiskSlippageBufferBPS: 5, RiskRoundTripFeeBPS: 10, RiskLeverageFallback: true, RiskLeverageMaxLoss: 0.50, RiskLiquidationBufferATR: 0.5, RiskMinStopATRRatio: 0.5}
	base := AIProtectionPlanInput{Side: SideLong, EntryPrice: 100, CurrentPrice: 100, AIStopPrice: 98.03, ATR: 2, Equity: 1000, AvailableRiskUSD: 50, PlannedNotional: 100, PriceTickSize: 0.1, Leverage: 5}
	plan, err := BuildAIProtectionPlanFromStop(cfg, base)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(plan.StopPrice-98.1) > 1e-9 || plan.GovernedBy != "ai_stop" || plan.ExpectedLossUSD <= 0 {
		t.Fatalf("unexpected AI plan: %+v", plan)
	}

	for name, mutate := range map[string]func(*AIProtectionPlanInput){
		"wrong side":           func(in *AIProtectionPlanInput) { in.AIStopPrice = 101 },
		"already crossed":      func(in *AIProtectionPlanInput) { in.CurrentPrice = 98 },
		"too narrow":           func(in *AIProtectionPlanInput) { in.AIStopPrice = 99.5 },
		"too wide":             func(in *AIProtectionPlanInput) { in.AIStopPrice = 90 },
		"risk cap":             func(in *AIProtectionPlanInput) { in.AvailableRiskUSD = 1 },
		"liquidation conflict": func(in *AIProtectionPlanInput) { in.LiquidationPrice = 98 },
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			mutate(&in)
			if _, err := BuildAIProtectionPlanFromStop(cfg, in); err == nil {
				t.Fatal("unsafe AI stop must be rejected")
			}
		})
	}

	short := base
	short.Side, short.AIStopPrice = SideShort, 101.97
	plan, err = BuildAIProtectionPlanFromStop(cfg, short)
	if err != nil || math.Abs(plan.StopPrice-101.9) > 1e-9 {
		t.Fatalf("short stop must floor toward safety: plan=%+v err=%v", plan, err)
	}
}

// The AI stop floor must be the same configured structural floor ordinary
// copying uses. While it was hardcoded at 0.5 ATR, an AI stop at 0.75 ATR was
// armed on a position whose identical ordinary-copy stop was declared
// unprotectable, so the two paths disagreed about the same instrument.
func TestAIProtectionPlanFloorFollowsConfiguredRatio(t *testing.T) {
	cfg := func(minRatio float64) *CopyConfig {
		return &CopyConfig{RiskSlippageBufferBPS: 5, RiskRoundTripFeeBPS: 10, RiskLiquidationBufferATR: 0.5, RiskMinStopATRRatio: minRatio}
	}
	// 0.75 ATR: below the production 1.0 floor, above the retired 0.5 constant.
	in := AIProtectionPlanInput{Side: SideLong, EntryPrice: 100, CurrentPrice: 100, AIStopPrice: 98.5,
		ATR: 2, Equity: 1000, AvailableRiskUSD: 50, PlannedNotional: 100, PriceTickSize: 0.1, Leverage: 5}

	if _, err := BuildAIProtectionPlanFromStop(cfg(1.0), in); err == nil {
		t.Fatal("a 0.75 ATR stop must be rejected under the 1.0 ATR floor")
	}
	if _, err := BuildAIProtectionPlanFromStop(cfg(0.5), in); err != nil {
		t.Fatalf("the same stop must be accepted under a 0.5 floor: %v", err)
	}
	// Zero disables the floor on both paths alike.
	tight := in
	tight.AIStopPrice = 99.7
	if _, err := BuildAIProtectionPlanFromStop(cfg(0), tight); err != nil {
		t.Fatalf("floor 0 must accept any distance the other checks allow: %v", err)
	}
}

// The position margin ceiling is inversely proportional to leverage, so as a
// veto it made every structurally sound stop impossible above roughly 67x —
// and it fired after the entry had already filled, leaving the position
// unprotected. It must now only be reported.
func TestAIProtectionPlanReportsMarginCapWithoutRejecting(t *testing.T) {
	cfg := &CopyConfig{RiskSlippageBufferBPS: 5, RiskRoundTripFeeBPS: 10, RiskLiquidationBufferATR: 0.5,
		RiskMinStopATRRatio: 1.0, RiskLeverageFallback: true, RiskLeverageMaxLoss: 0.50}
	// 100x, 2 ATR stop: 2% adverse move is 200% of the initial margin, far past
	// the 50% ceiling, yet well inside the account risk budget.
	in := AIProtectionPlanInput{Side: SideLong, EntryPrice: 100, CurrentPrice: 100, AIStopPrice: 98,
		ATR: 1, Equity: 1000, AvailableRiskUSD: 50, PlannedNotional: 100, PriceTickSize: 0.1, Leverage: 100}

	plan, err := BuildAIProtectionPlanFromStop(cfg, in)
	if err != nil {
		t.Fatalf("margin ceiling must not veto a structurally sound stop: %v", err)
	}
	if !plan.MarginCapExceeded {
		t.Fatalf("the breach must still be recorded for alerting: %+v", plan)
	}
	if plan.ExpectedMarginLossPct <= cfg.RiskLeverageMaxLoss {
		t.Fatalf("test no longer exercises a breach: %+v", plan)
	}

	// Real budgets must still veto: the same stop against a 1 USD budget.
	starved := in
	starved.AvailableRiskUSD = 1
	if _, err := BuildAIProtectionPlanFromStop(cfg, starved); err == nil {
		t.Fatal("account/cycle/portfolio risk budget must remain a hard veto")
	}
}

func TestBuildPositionMarginStopAnchorFormulaAndSafeTickRounding(t *testing.T) {
	long, err := BuildPositionMarginStopAnchor(SideLong, 100, 2, 3, 0.80, 0.01)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(long.RawStopPrice-73.33333333333333) > 1e-9 || math.Abs(long.StopPrice-73.34) > 1e-9 {
		t.Fatalf("long stop must use entry*(1-pct/leverage) and ceil toward safety: %+v", long)
	}
	if math.Abs(long.InitialMargin-66.66666666666667) > 1e-9 || math.Abs(long.ConfiguredRiskUSD-53.333333333333336) > 1e-9 {
		t.Fatalf("long margin/risk audit is wrong: %+v", long)
	}
	short, err := BuildPositionMarginStopAnchor(SideShort, 100, 2, 3, 0.80, 0.01)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(short.RawStopPrice-126.66666666666667) > 1e-9 || math.Abs(short.StopPrice-126.66) > 1e-9 {
		t.Fatalf("short stop must use entry*(1+pct/leverage) and floor toward safety: %+v", short)
	}
	for _, invalid := range []struct {
		entry, quantity, leverage, pct, tick float64
	}{
		{0, 1, 10, .8, .1}, {100, 0, 10, .8, .1}, {100, 1, 0, .8, .1},
		{100, 1, 10, 0, .1}, {100, 1, 10, 1, .1}, {100, 1, 10, .8, 0},
		{math.NaN(), 1, 10, .8, .1}, {100, math.Inf(1), 10, .8, .1},
	} {
		if _, err := BuildPositionMarginStopAnchor(SideLong, invalid.entry, invalid.quantity, invalid.leverage, invalid.pct, invalid.tick); err == nil {
			t.Fatalf("invalid anchor accepted: %+v", invalid)
		}
	}
}

func TestPositionMarginStopScreenshotRegression(t *testing.T) {
	tests := []struct {
		name                                string
		side                                SideType
		currentEntry, stop, leverage        float64
		wantAnchorEntry, wantEffectiveRatio float64
	}{
		{"BTC short", SideShort, 78859.30, 80951.40, 30, 78848.76623376623, 0.7959},
		{"ETH short", SideShort, 2494.43, 2585.36, 20, 2485.923076923077, 0.7291},
		{"MU long", SideLong, 935.28, 911.89, 20, 949.8854166666667, 0.5002},
		{"SNDK long", SideLong, 1496.28, 1421.46, 10, 1545.0652173913043, 0.5000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			anchor, err := InferPositionMarginStopAnchorEntry(tc.side, tc.stop, tc.leverage, .80)
			if err != nil || math.Abs(anchor-tc.wantAnchorEntry) > 1e-6 {
				t.Fatalf("inverse anchor=%v want=%v err=%v", anchor, tc.wantAnchorEntry, err)
			}
			effective, err := PositionMarginEffectiveLossPct(tc.side, tc.currentEntry, tc.stop, tc.leverage)
			if err != nil || math.Abs(effective-tc.wantEffectiveRatio) > 0.00015 {
				t.Fatalf("effective ratio=%v want≈%v err=%v", effective, tc.wantEffectiveRatio, err)
			}
		})
	}
}

func TestEvaluatePositionMarginStopKeepsAnchorAndOnlyTightens(t *testing.T) {
	base := PositionMarginStopEvaluationInput{
		Side: SideLong, CurrentEntryPrice: 100, CurrentQuantity: 2,
		CurrentLeverage: 10, AnchorStopPrice: 92, MarkPrice: 100,
		PriceTickSize: .1, BaseQuantityStep: .01, FollowerEquity: 1000,
	}
	result, err := EvaluatePositionMarginStop(base)
	if err != nil || result.StopPrice != 92 || math.Abs(result.CurrentRiskUSD-16) > 1e-9 || math.Abs(result.CurrentEffectiveMarginLossPct-.8) > 1e-9 {
		t.Fatalf("base evaluation=%+v err=%v", result, err)
	}
	// Add/average/leverage changes affect audit values, never the anchor price.
	changed := base
	changed.CurrentEntryPrice, changed.CurrentQuantity, changed.CurrentLeverage = 95, 4, 20
	result, err = EvaluatePositionMarginStop(changed)
	if err != nil || result.StopPrice != 92 || math.Abs(result.CurrentRiskUSD-12) > 1e-9 {
		t.Fatalf("position change moved the fixed stop: %+v err=%v", result, err)
	}
	// Liquidation safety can tighten once; an existing tighter stop is retained
	// after the liquidation line later moves back.
	clamped := base
	clamped.LiquidationPrice = 95
	result, err = EvaluatePositionMarginStop(clamped)
	if err != nil || !result.Clamped || math.Abs(result.StopPrice-95.2) > 1e-9 {
		t.Fatalf("liquidation clamp=%+v err=%v", result, err)
	}
	recovered := base
	recovered.ExistingStopPrice, recovered.LiquidationPrice = result.StopPrice, 80
	result, err = EvaluatePositionMarginStop(recovered)
	if err != nil || result.StopPrice != 95.2 || result.GovernedBy != "position_margin_existing_tighter" {
		t.Fatalf("safety recovery widened the stop: %+v err=%v", result, err)
	}
	recovered.MarkPrice = 95.1
	result, err = EvaluatePositionMarginStop(recovered)
	if err != nil || !result.AlreadyCrossed {
		t.Fatalf("crossed fixed stop was not detected: %+v err=%v", result, err)
	}
}

func TestFixedPositionMarginPolicyRequiresIsolation(t *testing.T) {
	cfg := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4}
	cfg.FillRiskDefaults()
	cfg.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	cfg.RiskPositionMarginStopPct = .80
	cfg.RiskTriggerPriceType = "mark"
	cfg.RiskReentryEnabled = false
	cfg.RiskManualReentryEnabled = false
	cfg.RiskReentryDecisionMode = "disabled"
	cfg.RiskMaxReentries = 0
	if err := ValidateRiskPolicyV4(cfg); err != nil {
		t.Fatalf("valid fixed policy rejected: %v", err)
	}
	for name, mutate := range map[string]func(*CopyConfig){
		"zero percent":    func(c *CopyConfig) { c.RiskPositionMarginStopPct = 0 },
		"hundred percent": func(c *CopyConfig) { c.RiskPositionMarginStopPct = 1 },
		"last trigger":    func(c *CopyConfig) { c.RiskTriggerPriceType = "last" },
		"AI reentry":      func(c *CopyConfig) { c.RiskReentryEnabled = true; c.RiskReentryDecisionMode = "ai_guarded" },
		"manual":          func(c *CopyConfig) { c.RiskManualReentryEnabled = true },
		"second entry":    func(c *CopyConfig) { c.RiskMaxReentries = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := *cfg
			mutate(&candidate)
			if err := ValidateRiskPolicyV4(&candidate); err == nil {
				t.Fatal("contradictory fixed policy was accepted")
			}
		})
	}
}

func TestPositionMarginLifecycleSettingsUseImmutableSnapshot(t *testing.T) {
	cycle := &store.CopyGuardCycle{PolicySnapshot: `{
		"risk_protection_mode":"position_margin_pct",
		"risk_position_margin_stop_pct":0.8,
		"risk_liquidation_buffer_atr":0.25
	}`}
	fallback := &CopyConfig{
		RiskProtectionMode:        store.RiskProtectionModeATRStructure,
		RiskPositionMarginStopPct: 0.4,
		RiskLiquidationBufferATR:  1.5,
	}
	mode, pct := copyGuardProtectionSettings(cycle, fallback)
	if mode != store.RiskProtectionModePositionMarginPct || pct != .8 {
		t.Fatalf("active lifecycle was changed by current config: mode=%s pct=%v", mode, pct)
	}
	if got := copyGuardLiquidationBufferATR(cycle, fallback); got != .25 {
		t.Fatalf("active lifecycle liquidation buffer=%v want=.25", got)
	}

	// A legacy snapshot without the new mode remains ATR and uses the historic
	// safety default instead of inheriting a later trader configuration.
	legacy := &store.CopyGuardCycle{PolicySnapshot: `{}`}
	mode, pct = copyGuardProtectionSettings(legacy, fallback)
	if mode != store.RiskProtectionModeATRStructure || pct != store.DefaultRiskPositionMarginStopPct {
		t.Fatalf("legacy lifecycle compatibility changed: mode=%s pct=%v", mode, pct)
	}
	if got := copyGuardLiquidationBufferATR(legacy, fallback); got != .5 {
		t.Fatalf("legacy lifecycle liquidation buffer=%v want=.5", got)
	}

	// Switching the trader template to fixed mode also turns off the current
	// reentry fields. An ATR lifecycle must continue with the settings that were
	// frozen when it opened instead of inheriting that later isolation change.
	currentFixed := *fallback
	currentFixed.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	currentFixed.RiskTriggerPriceType = "mark"
	currentFixed.RiskReentryEnabled = false
	currentFixed.RiskReentryDecisionMode = "disabled"
	currentFixed.RiskMaxReentries = 0
	atrSnapshot := &store.CopyGuardCycle{PolicySnapshot: `{
		"version":4,
		"risk_protection_mode":"atr_structure",
		"trigger_price_type":"last",
		"reentry_enabled":true,
		"reentry_ratio":0.4,
		"max_reentries":2,
		"reentry_decision_mode":"ai_guarded",
		"reentry_band_atr":0.75,
		"reentry_cooldown_seconds":600
	}`}
	active := copyGuardLifecycleConfig(atrSnapshot, &currentFixed)
	if active == nil || active.RiskProtectionMode != store.RiskProtectionModeATRStructure ||
		!active.RiskReentryEnabled || active.RiskReentryDecisionMode != "ai_guarded" ||
		active.RiskMaxReentries != 2 || active.RiskReentryRatio != .4 ||
		active.RiskTriggerPriceType != "last" || active.RiskReentryCooldownSeconds != 600 {
		t.Fatalf("ATR lifecycle inherited the later fixed template: %+v", active)
	}

	currentATR := currentFixed
	currentATR.RiskProtectionMode = store.RiskProtectionModeATRStructure
	currentATR.RiskReentryEnabled = true
	currentATR.RiskReentryDecisionMode = "ai_guarded"
	currentATR.RiskMaxReentries = 2
	fixedSnapshot := &store.CopyGuardCycle{PolicySnapshot: `{
		"risk_protection_mode":"position_margin_pct",
		"risk_position_margin_stop_pct":0.8,
		"reentry_enabled":true,
		"max_reentries":2,
		"reentry_decision_mode":"ai_guarded"
	}`}
	isolated := copyGuardLifecycleConfig(fixedSnapshot, &currentATR)
	if isolated == nil || isolated.RiskReentryEnabled || isolated.RiskReentryDecisionMode != "disabled" ||
		isolated.RiskMaxReentries != 0 || isolated.RiskTriggerPriceType != "mark" {
		t.Fatalf("fixed lifecycle isolation was not enforced from its snapshot: %+v", isolated)
	}
}
