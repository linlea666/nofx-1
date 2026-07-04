package copytrade

import (
	"fmt"
	"math"
	"strings"

	"nofx/logger"
	"nofx/store"
)

type RiskDistanceResult struct {
	Distance        float64
	ExpectedLossUSD float64
	ExpectedLossPct float64
	GovernedBy      string
	NoiseConflict   bool
}

// ComputeRiskDistanceV4 is pure and unit-explicit; exchange/network concerns stay outside it.
// leverage participates only in the margin-loss cap (RiskLeverageFallback / RiskLeverageMaxLoss):
// distance = entryPrice × maxLoss / leverage means "stop where the margin loss equals maxLoss".
// A cap tighter than the ATR distance intentionally trades noise protection for a bounded,
// user-visible margin loss (the OKX PnL% view), so NoiseConflict is reported.
func ComputeRiskDistanceV4(c *CopyConfig, entryPrice, positionNotional, accountEquity, atrDistance float64, leverage int) (RiskDistanceResult, error) {
	if c == nil || entryPrice <= 0 || positionNotional <= 0 || accountEquity <= 0 || atrDistance <= 0 {
		return RiskDistanceResult{}, fmt.Errorf("invalid v4 risk calculation input")
	}
	if leverage <= 0 {
		leverage = 1
	}
	r := RiskDistanceResult{Distance: atrDistance, GovernedBy: "atr"}
	accountDistance := accountEquity * c.RiskAccountPct / positionNotional * entryPrice
	if c.RiskStopMode == "account_hard_limit" && accountDistance < r.Distance {
		r.Distance = accountDistance
		r.GovernedBy = "account"
		r.NoiseConflict = true
	}
	if c.RiskLeverageFallback && c.RiskLeverageMaxLoss > 0 {
		marginCapDistance := entryPrice * c.RiskLeverageMaxLoss / float64(leverage)
		if marginCapDistance < r.Distance {
			r.Distance = marginCapDistance
			r.GovernedBy = "margin_cap"
			r.NoiseConflict = true
		}
	}
	r.ExpectedLossUSD = positionNotional*r.Distance/entryPrice + positionNotional*c.RiskSlippageBufferBPS/10000
	r.ExpectedLossPct = r.ExpectedLossUSD / accountEquity
	return r, nil
}

// ComputeOwnPathBaseline 计算"未被止损、每个 attempt 按自身实际名义持有到领航员
// 平仓价"的反事实基线盈亏（own-path 口径）。
//
// 背景：旧口径用领航员比例折算的影子名义（BaselineNotional）计算基线，领航员
// 加仓后影子名义可达跟随者实际仓位的数倍（WLD：608 vs 166.8 USD），而跟随者
// 受保证金约束根本开不出该仓位，导致"净保护效果"虚高（+13.99 实际约 +1）。
//
// 口径：baseline = BaselineRealizedPnL（领航员观察期减仓的已实现部分，按自身
// 名义记账）+ Σ attempt.Notional × move(attempt.EntryPrice → leaderClosePrice)。
// 每个 attempt 以自身入场价和自身名义为基准：被止损的 attempt 反事实持有到
// 领航员平仓价（衡量该次止损决策的得失），跟随领航员实际平仓的 attempt
// 反事实与实际路径几乎一致（贡献 ≈ 0，自然抵消）。
//
// ok=false 表示 attempt 数据不完整（无有效 entry/notional）或平仓价缺失，
// 调用方应回退到旧口径或跳过重算。
func ComputeOwnPathBaseline(cycle *store.CopyGuardCycle, attempts []*store.CopyGuardAttempt, leaderClosePrice float64) (baseline float64, ok bool) {
	if cycle == nil || leaderClosePrice <= 0 {
		return 0, false
	}
	baseline = cycle.BaselineRealizedPnL
	counted := 0
	for _, a := range attempts {
		if a == nil || a.EntryPrice <= 0 || a.Notional <= 0 {
			continue
		}
		move := (leaderClosePrice - a.EntryPrice) / a.EntryPrice
		if strings.EqualFold(cycle.Side, "short") {
			move = -move
		}
		baseline += a.Notional * move
		counted++
	}
	if counted == 0 {
		return 0, false
	}
	return baseline, true
}

// MigrateCopyGuardBaselinesV2 启动时一次性把历史 RECONCILED 且触发过止损的
// 周期基线从旧口径（领航员比例影子名义）重算为 own-path 口径，修正历史
// net_guard_effect 虚高（如 WLD 的 +13.99 → 约 +1）。
// 幂等：处理完把所有 v1 行标记为 v2；数据不完整的保留原值、不再重扫。
func MigrateCopyGuardBaselinesV2(st *store.Store) {
	cs := st.CopyTrade()
	cycles, err := cs.ListCopyGuardCyclesNeedingBaselineMigration()
	if err != nil {
		logger.Warnf("⚠️ Copy Guard 基线迁移扫描失败: %v", err)
		return
	}
	migrated := 0
	for _, cycle := range cycles {
		closePrice := recoverLeaderClosePrice(cycle)
		if closePrice <= 0 {
			continue
		}
		attempts, aerr := cs.ListCopyGuardAttempts(cycle.ID)
		if aerr != nil {
			continue
		}
		baseline, ok := ComputeOwnPathBaseline(cycle, attempts, closePrice)
		if !ok {
			continue
		}
		if err := cs.ApplyCopyGuardBaselineMigration(cycle.ID, baseline); err != nil {
			logger.Warnf("⚠️ Copy Guard 基线迁移失败 cycle=%d: %v", cycle.ID, err)
			continue
		}
		migrated++
	}
	if err := cs.FinishCopyGuardBaselineMigration(); err != nil {
		logger.Warnf("⚠️ Copy Guard 基线版本标记失败: %v", err)
	}
	if len(cycles) > 0 {
		logger.Infof("📐 Copy Guard 基线口径迁移完成：%d/%d 个历史周期已重算为 own-path 口径", migrated, len(cycles))
	}
}

// recoverLeaderClosePrice 从旧口径基线反解当时使用的领航员平仓价：
// oldBaseline = realized + shadowNotional × move，故
// move = (baseline_pnl − baseline_realized_pnl) / baseline_notional。
// 反解对 last_observed / leader_history 两种来源都精确成立；
// 影子名义缺失时退回最后观测价。
func recoverLeaderClosePrice(cycle *store.CopyGuardCycle) float64 {
	if cycle.BaselineNotional > 0 && cycle.LeaderEntryPrice > 0 {
		move := (cycle.BaselinePnL - cycle.BaselineRealizedPnL) / cycle.BaselineNotional
		if strings.EqualFold(cycle.Side, "short") {
			move = -move
		}
		if p := cycle.LeaderEntryPrice * (1 + move); p > 0 {
			return p
		}
	}
	return cycle.LastObservedPrice
}

func ValidateRiskPolicyV4(c *CopyConfig) error {
	if c == nil {
		return fmt.Errorf("nil copy guard config")
	}
	if c.RiskPolicyVersion < 4 {
		return nil
	}
	if c.ProviderType != ProviderOKX {
		return fmt.Errorf("copy guard v4 is only supported for OKX")
	}
	if c.RiskStopMode != "volatility_priority" && c.RiskStopMode != "account_hard_limit" {
		return fmt.Errorf("invalid risk_stop_mode %q", c.RiskStopMode)
	}
	if c.RiskATRPeriod < 5 || c.RiskATRPeriod > 100 {
		return fmt.Errorf("risk_atr_period must be 5..100")
	}
	if c.RiskATRCacheMaxAgeMinutes < 1 || c.RiskATRCacheMaxAgeMinutes > 1440 {
		return fmt.Errorf("risk_atr_cache_max_age_minutes must be 1..1440")
	}
	if invalidRange(c.RiskATRMultiplier, 0.5, 5) {
		return fmt.Errorf("risk_atr_multiplier must be 0.5..5")
	}
	if invalidRange(c.RiskATRFallbackPct, 0.001, 0.2) {
		return fmt.Errorf("risk_atr_fallback_pct must be 0.1%%..20%%")
	}
	if c.RiskTriggerPriceType != "mark" && c.RiskTriggerPriceType != "last" && c.RiskTriggerPriceType != "index" {
		return fmt.Errorf("invalid risk_trigger_price_type")
	}
	if invalidRange(c.RiskAccountPct, 0.001, 1) {
		return fmt.Errorf("risk_account_pct must be 0.1%%..100%%")
	}
	if invalidRange(c.RiskSlippageBufferBPS, 0, 1000) {
		return fmt.Errorf("risk_slippage_buffer_bps must be 0..1000")
	}
	if invalidRange(c.RiskLiquidationBufferATR, 0, 5) {
		return fmt.Errorf("risk_liquidation_buffer_atr must be 0..5")
	}
	if c.RiskMaxReentries < 0 || c.RiskMaxReentries > 10 {
		return fmt.Errorf("risk_max_reentries must be 0..10")
	}
	if invalidRange(c.RiskReentryRatio, 0.1, 1) {
		return fmt.Errorf("risk_reentry_ratio must be 0.1..1")
	}
	if invalidRange(c.RiskReentryBandATR, 0, 3) {
		return fmt.Errorf("risk_reentry_band_atr must be 0..3")
	}
	if c.RiskReentryCooldownSeconds < 0 || c.RiskReentryCooldownSeconds > 86400 {
		return fmt.Errorf("risk_reentry_cooldown_seconds must be 0..86400")
	}
	if invalidRange(c.RiskReentryMaxChaseATR, 0, 2) {
		return fmt.Errorf("risk_reentry_max_chase_atr must be 0..2")
	}
	if invalidRange(c.RiskReentryMaxATRExpansion, 1, 10) {
		return fmt.Errorf("risk_reentry_max_atr_expansion must be 1..10")
	}
	if c.RiskWatchTimeoutMinutes < 0 || c.RiskWatchTimeoutMinutes > 525600 {
		return fmt.Errorf("risk_watch_timeout_minutes out of range")
	}
	if c.RiskAddonBudgetPct != 0 && invalidRange(c.RiskAddonBudgetPct, 0.01, 1) {
		return fmt.Errorf("risk_addon_budget_pct must be 1%%..100%%")
	}
	return nil
}

func invalidRange(v, min, max float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0) || v < min || v > max
}

func ValidateStoredRiskPolicy(c *store.CopyTradeConfig) error {
	if c == nil || c.RiskPolicyVersion < 4 {
		return nil
	}
	c.FillRiskDefaults()
	return ValidateRiskPolicyV4(&CopyConfig{ProviderType: ProviderType(c.ProviderType), RiskPolicyVersion: c.RiskPolicyVersion, RiskStopMode: c.RiskStopMode, RiskATRPeriod: c.RiskATRPeriod, RiskATRCacheMaxAgeMinutes: c.RiskATRCacheMaxAgeMinutes, RiskATRMultiplier: c.RiskATRMultiplier, RiskATRFallbackPct: c.RiskATRFallbackPct, RiskTriggerPriceType: c.RiskTriggerPriceType, RiskAccountPct: c.RiskAccountPct, RiskSlippageBufferBPS: c.RiskSlippageBufferBPS, RiskLiquidationBufferATR: c.RiskLiquidationBufferATR, RiskMaxReentries: c.RiskMaxReentries, RiskReentryRatio: c.RiskReentryRatio, RiskReentryBandATR: c.RiskReentryBandATR, RiskReentryCooldownSeconds: c.RiskReentryCooldownSeconds, RiskReentryMaxChaseATR: c.RiskReentryMaxChaseATR, RiskReentryMaxATRExpansion: c.RiskReentryMaxATRExpansion, RiskWatchTimeoutMinutes: c.RiskWatchTimeoutMinutes, RiskAddonBudgetPct: c.RiskAddonBudgetPct})
}
