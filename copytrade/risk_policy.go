package copytrade

import (
	"fmt"
	"math"
	"strings"

	"nofx/logger"
	"nofx/store"
)

// copyGuardProtectionSettings reads the immutable lifecycle snapshot. A
// legacy snapshot without the new fields is deliberately ATR mode even if the
// trader configuration is changed while that position remains open.
func copyGuardProtectionSettings(cycle *store.CopyGuardCycle, fallback *CopyConfig) (string, float64) {
	mode := store.RiskProtectionModeATRStructure
	pct := store.DefaultRiskPositionMarginStopPct
	if cycle != nil {
		if snapshot, err := store.DecodeCopyGuardPolicySnapshot(cycle.PolicySnapshot); err == nil {
			if snapshot.ProtectionMode == store.RiskProtectionModeATRStructure || snapshot.ProtectionMode == store.RiskProtectionModePositionMarginPct {
				mode = snapshot.ProtectionMode
			}
			if snapshot.PositionMarginStopPct >= 0.01 && snapshot.PositionMarginStopPct <= 0.99 {
				pct = snapshot.PositionMarginStopPct
			}
		}
		return mode, pct
	}
	if fallback != nil {
		if fallback.RiskProtectionMode == store.RiskProtectionModePositionMarginPct {
			mode = fallback.RiskProtectionMode
		}
		if fallback.RiskPositionMarginStopPct >= 0.01 && fallback.RiskPositionMarginStopPct <= 0.99 {
			pct = fallback.RiskPositionMarginStopPct
		}
	}
	return mode, pct
}

// copyGuardLifecycleConfig reconstructs the risk settings frozen when a
// lifecycle was opened. Trader settings may be edited while the engine keeps
// running; those edits are the template for later positions and must not
// silently change an existing ATR lifecycle into fixed mode (or disable its
// already-snapshotted reentry contract). Newly-added pointer fields preserve a
// safe fallback for snapshots written by older binaries.
func copyGuardLifecycleConfig(cycle *store.CopyGuardCycle, fallback *CopyConfig) *CopyConfig {
	if fallback == nil {
		return nil
	}
	cfg := *fallback
	if cycle == nil {
		return &cfg
	}
	p, err := store.DecodeCopyGuardPolicySnapshot(cycle.PolicySnapshot)
	if err != nil {
		logger.Errorf("Copy Guard cycle %d has invalid policy snapshot: %v", cycle.ID, err)
		return nil
	}
	hasPolicy := p.Version >= 4 || p.DefaultsVersion > 0 ||
		p.ProtectionMode == store.RiskProtectionModeATRStructure ||
		p.ProtectionMode == store.RiskProtectionModePositionMarginPct
	if !hasPolicy {
		return nil
	}
	// Missing legacy fields use the versioned compatibility defaults, never
	// today's mutable trader template. Current snapshots persist these fields.
	cfg.RiskAccountPct, cfg.RiskATRMultiplier, cfg.RiskATRTimeframe = .02, 2, "1h"
	cfg.RiskLeverageFallback, cfg.RiskLeverageMaxLoss, cfg.RiskReentryRatio = true, .5, .5
	cfg.RiskTriggerPriceType, cfg.RiskStopMode, cfg.RiskStopPriority = "mark", "volatility_priority", "volatility_first"
	cfg.RiskATRPeriod, cfg.RiskATRCacheMaxAgeMinutes, cfg.RiskATRFallbackPct = 14, 120, .02
	cfg.RiskReentryEnabled, cfg.RiskManualReentryEnabled, cfg.RiskReentryDecisionMode = false, false, "disabled"
	cfg.RiskPolicyVersion, cfg.RiskNotificationLevel = 4, "important"
	cfg.RiskUnprotectableDisposition, cfg.RiskUnprotectableAction = "warn", "follow"
	// A lifecycle can only have been created while protection was enabled. The
	// current trader master switch is a template for future positions and must
	// not disable the immutable contract of this already-open cycle.
	cfg.RiskStopLossEnabled = true

	if p.Version > 0 {
		cfg.RiskPolicyVersion = p.Version
	}
	mode, pct := copyGuardProtectionSettings(cycle, fallback)
	cfg.RiskProtectionMode = mode
	cfg.RiskPositionMarginStopPct = pct
	if p.AccountPct != nil {
		cfg.RiskAccountPct = *p.AccountPct
	}
	if p.ATRMultiplier != nil {
		cfg.RiskATRMultiplier = *p.ATRMultiplier
	}
	if p.ATRTimeframe != nil {
		cfg.RiskATRTimeframe = *p.ATRTimeframe
	}
	if p.LeverageFallback != nil {
		cfg.RiskLeverageFallback = *p.LeverageFallback
	}
	if p.LeverageMaxLoss != nil {
		cfg.RiskLeverageMaxLoss = *p.LeverageMaxLoss
	}
	if p.StopMode != "" {
		cfg.RiskStopMode = p.StopMode
	}
	if p.StopPriority != "" {
		cfg.RiskStopPriority = p.StopPriority
	}
	cfg.RiskStopMaxAccountLossPct = p.StopMaxAccountLossPct
	if p.ATRPeriod > 0 {
		cfg.RiskATRPeriod = p.ATRPeriod
	}
	if p.ATRCacheMaxAgeMinutes > 0 {
		cfg.RiskATRCacheMaxAgeMinutes = p.ATRCacheMaxAgeMinutes
	}
	if p.ATRFallbackPct > 0 {
		cfg.RiskATRFallbackPct = p.ATRFallbackPct
	}
	if p.TriggerPriceType != "" {
		cfg.RiskTriggerPriceType = p.TriggerPriceType
	}
	cfg.RiskSlippageBufferBPS = p.SlippageBufferBPS
	cfg.RiskRoundTripFeeBPS = p.RoundTripFeeBPS
	cfg.RiskLiquidationBufferATR = p.LiquidationBufferATR
	if p.ReentryEnabled != nil {
		cfg.RiskReentryEnabled = *p.ReentryEnabled
	} else if p.ReentryDecisionMode != "" {
		// Older policy snapshots did not persist the boolean. Their decision
		// mode and attempt count are durable evidence of the intended state.
		cfg.RiskReentryEnabled = p.ReentryDecisionMode != "disabled" && p.MaxReentries > 0
	}
	if p.ManualReentryEnabled != nil {
		cfg.RiskManualReentryEnabled = *p.ManualReentryEnabled
	} else if p.ATRProfile != nil && mode == store.RiskProtectionModeATRStructure {
		cfg.RiskManualReentryEnabled = p.ATRProfile.ManualReentryEnabled
	}
	if p.ReentryRatio != nil {
		cfg.RiskReentryRatio = *p.ReentryRatio
	}
	cfg.RiskMaxReentries = p.MaxReentries
	cfg.RiskReentryBandATR = p.ReentryBandATR
	cfg.RiskReentryCooldownSeconds = p.ReentryCooldownSec
	cfg.RiskReentryMaxChaseATR = p.ReentryMaxChaseATR
	cfg.RiskReentryMaxATRExpansion = p.MaxATRExpansion
	cfg.RiskWatchTimeoutMinutes = p.WatchTimeoutMinutes
	cfg.RiskMigrationConfirmed = p.MigrationConfirmed
	cfg.RiskAddonBudgetPct = p.AddonBudgetPct
	cfg.RiskCycleLossBudgetPct = p.CycleLossBudgetPct
	cfg.RiskPortfolioLossBudgetPct = p.PortfolioLossBudgetPct
	if p.ReentryDecisionMode != "" {
		cfg.RiskReentryDecisionMode = p.ReentryDecisionMode
	}
	cfg.RiskReentryMinNotional = p.ReentryMinNotional
	cfg.RiskAIConfidenceThreshold = p.AIConfidenceThreshold
	cfg.RiskAIMinReviewSeconds = p.AIMinReviewSeconds
	cfg.RiskAIDailyCallLimit = p.AIDailyCallLimit
	cfg.RiskAILifecycleCallLimit = p.AILifecycleCallLimit
	if p.NotificationLevel != "" {
		cfg.RiskNotificationLevel = p.NotificationLevel
	}
	cfg.RiskReentryMinRecoveryATR = p.ReentryMinRecoveryATR
	cfg.RiskReentryMinRecoveryATRExplicit = p.ReentryMinRecoveryATRExplicit
	cfg.RiskReentryCooldownEscalation = p.ReentryCooldownEscalation
	cfg.RiskReentryRecoveryEscalation = p.ReentryRecoveryEscalation
	if p.UnprotectableDisposition != "" {
		cfg.RiskUnprotectableDisposition = p.UnprotectableDisposition
	}
	if p.UnprotectableAction != "" {
		cfg.RiskUnprotectableAction = p.UnprotectableAction
	}
	cfg.RiskReentryNoiseOverride = p.ReentryNoiseOverride
	cfg.RiskMinStopATRRatio = p.MinStopATRRatio
	cfg.RiskMinStopATRRatioExplicit = p.MinStopATRRatioExplicit

	if mode == store.RiskProtectionModePositionMarginPct {
		cfg.RiskTriggerPriceType = "mark"
		cfg.RiskReentryEnabled = false
		cfg.RiskManualReentryEnabled = false
		cfg.RiskReentryDecisionMode = "disabled"
		cfg.RiskMaxReentries = 0
	}
	return &cfg
}

// copyGuardLiquidationBufferATR follows the same lifecycle-snapshot rule as
// the fixed stop mode. The safety clamp is part of the stop contract: changing
// trader configuration must not silently move an already-open lifecycle.
func copyGuardLiquidationBufferATR(cycle *store.CopyGuardCycle, fallback *CopyConfig) float64 {
	const defaultBuffer = 0.5
	if cycle != nil {
		if snapshot, err := store.DecodeCopyGuardPolicySnapshot(cycle.PolicySnapshot); err == nil &&
			(snapshot.SnapshotSchemaVersion > 0 || snapshot.Version > 0 || snapshot.DefaultsVersion > 0) &&
			!invalidRange(snapshot.LiquidationBufferATR, 0, 5) {
			return snapshot.LiquidationBufferATR
		}
		return defaultBuffer
	}
	if fallback != nil && !invalidRange(fallback.RiskLiquidationBufferATR, 0, 5) {
		return fallback.RiskLiquidationBufferATR
	}
	return defaultBuffer
}

type RiskDistanceResult struct {
	Distance        float64
	ExpectedLossUSD float64
	// ExpectedLossPct: 权益口径——预期止损损失 / 账户权益
	ExpectedLossPct float64
	// ExpectedMarginLossPct: 保证金口径——预期止损损失 / 该仓位保证金
	// （= distance/entry × leverage），用户直觉中的"仓位止损 20%"
	ExpectedMarginLossPct float64
	GovernedBy            string
	// DistanceATRRatio: 止损距离 / 原始 ATR。< 0.5 表示止损距离小于正常噪音，
	// 预计高频止损（UI 易扫损提示 + 重入自适应加严的输入）。
	// ATR 走 fallback 时该比值为等效近似；原始 ATR 由 atrDistance/multiplier 反推。
	DistanceATRRatio float64
	// NoiseConflict: 最终生效的硬 cap 比 ATR 基线更紧（止损落在噪音区内）
	NoiseConflict                bool
	AccountRiskThresholdExceeded bool
	// MarginCapDistance 仓位保证金上限换算出的价格距离（0 = 未启用）。
	// MarginCapExceeded 表示实际止损距离已超出该上限：普通跟单路径下这是
	// 告警信号而非收紧依据——收紧它等于把止损压进噪音区。
	MarginCapDistance float64
	MarginCapExceeded bool
}

// PositionMarginStopAnchorPlan is the immutable first-fill calculation used
// by the fixed position-margin percentage mode. Fees, funding and slippage are
// intentionally excluded from the trigger formula; they remain execution
// attribution and may make the realized loss larger than ConfiguredRiskUSD.
type PositionMarginStopAnchorPlan struct {
	EntryPrice              float64
	Leverage                float64
	Quantity                float64
	ConfiguredMarginLossPct float64
	RawStopPrice            float64
	StopPrice               float64
	InitialMargin           float64
	ConfiguredRiskUSD       float64
	AlignedPriceRiskUSD     float64
	EquivalentPriceMovePct  float64
}

type PositionMarginStopEvaluationInput struct {
	Side                 SideType
	CurrentEntryPrice    float64
	CurrentQuantity      float64
	CurrentLeverage      float64
	AnchorStopPrice      float64
	ExistingStopPrice    float64
	MarkPrice            float64
	LiquidationPrice     float64
	PriceTickSize        float64
	BaseQuantityStep     float64
	ATRValue             float64
	LiquidationBufferATR float64
	FollowerEquity       float64
}

type PositionMarginStopEvaluation struct {
	StopPrice                     float64
	CurrentRiskUSD                float64
	CurrentMargin                 float64
	CurrentEffectiveMarginLossPct float64
	CurrentAccountLossPct         float64
	EquivalentPriceMovePct        float64
	DistanceATRRatio              float64
	LiquidationPriceIgnored       bool
	Clamped                       bool
	AlreadyCrossed                bool
	GovernedBy                    string
}

// PositionMarginRawStopPrice is the single source of truth for the fixed
// initial-margin-loss formula. It deliberately excludes price-tick alignment,
// fees, funding and slippage; callers that place an exchange order must apply
// the venue's tick rule afterwards.
func PositionMarginRawStopPrice(side SideType, entryPrice, leverage, configuredPct float64) (float64, error) {
	if side != SideLong && side != SideShort {
		return 0, fmt.Errorf("invalid position-margin stop side %s", side)
	}
	if invalidPositive(entryPrice) || invalidPositive(leverage) || invalidRange(configuredPct, 0.01, 0.99) {
		return 0, fmt.Errorf("invalid position-margin stop formula input")
	}
	move := configuredPct / leverage
	raw := entryPrice * (1 - move)
	if side == SideShort {
		raw = entryPrice * (1 + move)
	}
	if invalidPositive(raw) {
		return 0, fmt.Errorf("position-margin stop formula overflows")
	}
	return raw, nil
}

// BuildPositionMarginStopAnchor calculates and aligns a first-fill fixed stop.
// Long stops ceil and short stops floor so tick quantization can only tighten
// the configured loss ceiling.
func BuildPositionMarginStopAnchor(side SideType, entryPrice, quantity, leverage, configuredPct, tickSize float64) (*PositionMarginStopAnchorPlan, error) {
	if side != SideLong && side != SideShort {
		return nil, fmt.Errorf("invalid position-margin stop side %s", side)
	}
	if invalidPositive(entryPrice) || invalidPositive(quantity) || invalidPositive(leverage) || invalidPositive(tickSize) ||
		invalidRange(configuredPct, 0.01, 0.99) {
		return nil, fmt.Errorf("invalid position-margin stop anchor input")
	}
	move := configuredPct / leverage
	raw, err := PositionMarginRawStopPrice(side, entryPrice, leverage, configuredPct)
	if err != nil {
		return nil, err
	}
	roundDown := false
	if side == SideShort {
		roundDown = true
	}
	stop := alignToTickSize(raw, tickSize, roundDown)
	valid := stop > 0 && ((side == SideLong && stop < entryPrice) || (side == SideShort && stop > entryPrice))
	if !valid || invalidPositive(raw) || invalidPositive(stop) {
		return nil, fmt.Errorf("position-margin stop cannot be represented at tick size")
	}
	margin := entryPrice * quantity / leverage
	risk := margin * configuredPct
	alignedRisk := math.Abs(entryPrice-stop) * quantity
	if invalidPositive(margin) || invalidPositive(risk) || invalidPositive(alignedRisk) {
		return nil, fmt.Errorf("position-margin stop anchor overflows")
	}
	return &PositionMarginStopAnchorPlan{
		EntryPrice: entryPrice, Leverage: leverage, Quantity: quantity,
		ConfiguredMarginLossPct: configuredPct, RawStopPrice: raw, StopPrice: stop,
		InitialMargin: margin, ConfiguredRiskUSD: risk,
		AlignedPriceRiskUSD:    alignedRisk,
		EquivalentPriceMovePct: move,
	}, nil
}

// EvaluatePositionMarginStop keeps the immutable anchor price through every
// add/reduce/average/leverage change. The only permitted price change is a
// one-way tightening above/below the current liquidation safety boundary; a
// previously tighter live stop is therefore always retained.
func EvaluatePositionMarginStop(in PositionMarginStopEvaluationInput) (*PositionMarginStopEvaluation, error) {
	if in.Side != SideLong && in.Side != SideShort {
		return nil, fmt.Errorf("invalid position-margin stop side %s", in.Side)
	}
	if invalidPositive(in.CurrentEntryPrice) || invalidPositive(in.CurrentQuantity) || invalidPositive(in.CurrentLeverage) ||
		invalidPositive(in.AnchorStopPrice) || invalidPositive(in.PriceTickSize) || invalidPositive(in.BaseQuantityStep) ||
		invalidNonNegative(in.ExistingStopPrice) || invalidNonNegative(in.MarkPrice) || invalidNonNegative(in.LiquidationPrice) ||
		invalidNonNegative(in.ATRValue) || invalidNonNegative(in.LiquidationBufferATR) || invalidNonNegative(in.FollowerEquity) {
		return nil, fmt.Errorf("invalid position-margin stop evaluation input")
	}
	result := &PositionMarginStopEvaluation{StopPrice: in.AnchorStopPrice, GovernedBy: "position_margin_anchor"}
	if in.ExistingStopPrice > 0 {
		keepExisting := (in.Side == SideLong && in.ExistingStopPrice > result.StopPrice) ||
			(in.Side == SideShort && in.ExistingStopPrice < result.StopPrice)
		if keepExisting {
			result.StopPrice = in.ExistingStopPrice
			result.GovernedBy = "position_margin_existing_tighter"
		}
	}
	if in.LiquidationPrice > 0 {
		if in.MarkPrice <= 0 || (in.Side == SideLong && in.LiquidationPrice > in.MarkPrice) || (in.Side == SideShort && in.LiquidationPrice < in.MarkPrice) {
			result.LiquidationPriceIgnored = true
		} else {
			// Fixed-mode safety does not depend on dormant ATR configuration.
			buffer := liquidationSafetyBuffer(in.PriceTickSize, 0, in.CurrentEntryPrice, 0)
			if in.Side == SideLong && result.StopPrice <= in.LiquidationPrice+buffer {
				result.StopPrice = alignToTickSize(in.LiquidationPrice+buffer, in.PriceTickSize, false)
				result.Clamped = true
				result.GovernedBy = "position_margin_liquidation_clamp"
			}
			if in.Side == SideShort && result.StopPrice >= in.LiquidationPrice-buffer {
				result.StopPrice = alignToTickSize(in.LiquidationPrice-buffer, in.PriceTickSize, true)
				result.Clamped = true
				result.GovernedBy = "position_margin_liquidation_clamp"
			}
		}
	}
	if invalidPositive(result.StopPrice) {
		return nil, fmt.Errorf("position-margin liquidation clamp is not representable")
	}
	if err := populatePositionMarginRisk(in, result.StopPrice, result); err != nil {
		return nil, err
	}
	if in.MarkPrice > 0 {
		result.AlreadyCrossed = (in.Side == SideLong && in.MarkPrice <= result.StopPrice) ||
			(in.Side == SideShort && in.MarkPrice >= result.StopPrice)
	}
	return result, nil
}

// PositionMarginRiskAtStop computes theoretical current-position risk at an already
// chosen price. It never changes quantity, leverage or a strategy stop.
func PositionMarginRiskAtStop(in PositionMarginStopEvaluationInput, stop float64) (*PositionMarginStopEvaluation, error) {
	result := &PositionMarginStopEvaluation{StopPrice: stop}
	if err := populatePositionMarginRisk(in, stop, result); err != nil {
		return nil, err
	}
	return result, nil
}

func populatePositionMarginRisk(in PositionMarginStopEvaluationInput, stop float64, result *PositionMarginStopEvaluation) error {
	if (in.Side != SideLong && in.Side != SideShort) || invalidPositive(in.CurrentEntryPrice) || invalidPositive(in.CurrentQuantity) || invalidPositive(in.CurrentLeverage) || invalidPositive(stop) || invalidNonNegative(in.FollowerEquity) || invalidNonNegative(in.ATRValue) {
		return fmt.Errorf("invalid position-margin risk inputs")
	}
	adverseDistance := float64(0)
	if in.Side == SideLong && stop < in.CurrentEntryPrice {
		adverseDistance = in.CurrentEntryPrice - stop
	}
	if in.Side == SideShort && stop > in.CurrentEntryPrice {
		adverseDistance = stop - in.CurrentEntryPrice
	}
	result.CurrentRiskUSD = adverseDistance * in.CurrentQuantity
	result.CurrentMargin = in.CurrentEntryPrice * in.CurrentQuantity / in.CurrentLeverage
	if result.CurrentMargin > 0 {
		result.CurrentEffectiveMarginLossPct = result.CurrentRiskUSD / result.CurrentMargin
	}
	if in.FollowerEquity > 0 {
		result.CurrentAccountLossPct = result.CurrentRiskUSD / in.FollowerEquity
	}
	if invalidNonNegative(result.CurrentRiskUSD) || invalidPositive(result.CurrentMargin) ||
		invalidNonNegative(result.CurrentEffectiveMarginLossPct) || invalidNonNegative(result.CurrentAccountLossPct) {
		return fmt.Errorf("position-margin stop evaluation overflows")
	}
	result.EquivalentPriceMovePct = adverseDistance / in.CurrentEntryPrice
	if in.ATRValue > 0 {
		result.DistanceATRRatio = adverseDistance / in.ATRValue
	}
	return nil
}
func InferPositionMarginStopAnchorEntry(side SideType, stopPrice, leverage, configuredPct float64) (float64, error) {
	if invalidPositive(stopPrice) || invalidPositive(leverage) || invalidRange(configuredPct, 0.01, 0.99) {
		return 0, fmt.Errorf("invalid position-margin inverse input")
	}
	denominator := 1 - configuredPct/leverage
	if side == SideShort {
		denominator = 1 + configuredPct/leverage
	} else if side != SideLong {
		return 0, fmt.Errorf("invalid position-margin inverse side %s", side)
	}
	if denominator <= 0 {
		return 0, fmt.Errorf("invalid position-margin inverse denominator")
	}
	entry := stopPrice / denominator
	if invalidPositive(entry) {
		return 0, fmt.Errorf("position-margin inverse overflows")
	}
	return entry, nil
}

func PositionMarginEffectiveLossPct(side SideType, currentEntry, stopPrice, leverage float64) (float64, error) {
	if invalidPositive(currentEntry) || invalidPositive(stopPrice) || invalidPositive(leverage) {
		return 0, fmt.Errorf("invalid position-margin effective loss input")
	}
	move := float64(0)
	if side == SideLong {
		move = (currentEntry - stopPrice) / currentEntry
	} else if side == SideShort {
		move = (stopPrice - currentEntry) / currentEntry
	} else {
		return 0, fmt.Errorf("invalid position-margin effective loss side %s", side)
	}
	if move < 0 {
		move = 0
	}
	result := move * leverage
	if invalidNonNegative(result) {
		return 0, fmt.Errorf("position-margin effective loss overflows")
	}
	return result, nil
}

type ProtectionPlan struct {
	EntryPrice            float64 `json:"entry_price"`
	StopPrice             float64 `json:"stop_price"`
	StopDistance          float64 `json:"stop_distance"`
	ATR                   float64 `json:"atr"`
	StructureInvalidation float64 `json:"structure_invalidation"`
	SlippageBPS           float64 `json:"slippage_bps"`
	RoundTripFeeBPS       float64 `json:"round_trip_fee_bps"`
	MaxRiskUSD            float64 `json:"max_risk_usd"`
	MaxNotional           float64 `json:"max_notional"`
	PlannedNotional       float64 `json:"planned_notional"`
	ExpectedLossUSD       float64 `json:"expected_loss_usd"`
	ExpectedLossPct       float64 `json:"expected_loss_pct"`
	ExpectedMarginLossPct float64 `json:"expected_margin_loss_pct"`
	GovernedBy            string  `json:"governed_by"`
	// MarginCapExceeded mirrors RiskDistanceResult: the position margin ceiling
	// is a reporting threshold, not a veto. See BuildAIProtectionPlanFromStop.
	MarginCapExceeded bool `json:"margin_cap_exceeded"`
}

type AIProtectionPlanInput struct {
	Side             SideType
	EntryPrice       float64
	CurrentPrice     float64
	AIStopPrice      float64
	ATR              float64
	Equity           float64
	AvailableRiskUSD float64
	PlannedNotional  float64
	PriceTickSize    float64
	LiquidationPrice float64
	Leverage         int
}

// BuildAIProtectionPlanFromStop validates the model's absolute stop as an
// immutable trading thesis. Deterministic risk management may align it only in
// the safer direction or reject the entry; it must never replace it with an
// ATR/structure-derived stop.
func BuildAIProtectionPlanFromStop(c *CopyConfig, in AIProtectionPlanInput) (*ProtectionPlan, error) {
	if c == nil || in.EntryPrice <= 0 || in.CurrentPrice <= 0 || in.AIStopPrice <= 0 || in.ATR <= 0 || in.Equity <= 0 || in.AvailableRiskUSD <= 0 || in.PlannedNotional <= 0 || in.PriceTickSize <= 0 {
		return nil, fmt.Errorf("invalid AI protection input")
	}
	if in.Side != SideLong && in.Side != SideShort {
		return nil, fmt.Errorf("invalid AI protection side %s", in.Side)
	}
	stop := alignToTickSize(in.AIStopPrice, in.PriceTickSize, in.Side == SideShort)
	if in.Side == SideLong {
		if stop >= in.EntryPrice {
			return nil, fmt.Errorf("long AI stop %.8f must be below entry %.8f", stop, in.EntryPrice)
		}
		if in.CurrentPrice <= stop {
			return nil, fmt.Errorf("long AI stop %.8f is already crossed by current price %.8f", stop, in.CurrentPrice)
		}
	} else {
		if stop <= in.EntryPrice {
			return nil, fmt.Errorf("short AI stop %.8f must be above entry %.8f", stop, in.EntryPrice)
		}
		if in.CurrentPrice >= stop {
			return nil, fmt.Errorf("short AI stop %.8f is already crossed by current price %.8f", stop, in.CurrentPrice)
		}
	}
	distance := math.Abs(in.EntryPrice - stop)
	atrRatio := distance / in.ATR
	// The lower bound is the same structural floor ordinary copying uses: below
	// it a stop is inside the noise band and is a loss generator rather than a
	// protection. Keeping a separate hardcoded 0.5 here meant an AI stop at 0.7
	// ATR was armed while an identical ordinary-copy stop was declared
	// unprotectable. Zero disables the floor, matching finalizeStopLossPrice.
	minATRRatio := c.RiskMinStopATRRatio
	if minATRRatio < 0 {
		minATRRatio = 0
	}
	if atrRatio < minATRRatio-1e-9 || atrRatio > 4+1e-9 {
		return nil, fmt.Errorf("AI stop distance %.4f ATR is outside [%.2f,4]", atrRatio, minATRRatio)
	}
	if in.LiquidationPrice > 0 && isLiquidationPriceDirectionValid(in.Side, in.EntryPrice, in.LiquidationPrice) {
		buffer := liquidationSafetyBuffer(in.PriceTickSize, in.ATR, in.EntryPrice, c.RiskLiquidationBufferATR)
		unsafe := (in.Side == SideLong && stop <= in.LiquidationPrice+buffer) ||
			(in.Side == SideShort && stop >= in.LiquidationPrice-buffer)
		if unsafe {
			return nil, fmt.Errorf("AI stop %.8f conflicts with liquidation safety boundary", stop)
		}
	}
	frictionRate := (c.RiskSlippageBufferBPS + c.RiskRoundTripFeeBPS) / 10000
	if frictionRate < 0 {
		frictionRate = 0
	}
	expected := in.PlannedNotional * (distance/in.EntryPrice + frictionRate)
	if expected > in.AvailableRiskUSD+1e-9 {
		return nil, fmt.Errorf("AI stop expected loss %.8f exceeds available risk %.8f", expected, in.AvailableRiskUSD)
	}
	marginLossPct := 0.0
	if in.Leverage <= 0 {
		in.Leverage = 1
	}
	if initialMargin := in.PlannedNotional / float64(in.Leverage); initialMargin > 0 {
		marginLossPct = expected / initialMargin
	}
	// The position margin ceiling is recorded, never enforced — the same
	// demotion ComputeAccountProtectionDistance applies to ordinary copying.
	//
	// As a veto it was worse than as a tightener. Its price-space width is
	// 0.5/leverage, so at a 0.75% ATR it admits 1.33 ATR at 50x and 0.67 ATR at
	// 100x, while the structural floor above demands at least 1.0 ATR: beyond
	// roughly 67x the admissible band is empty and every AI stop was rejected.
	// Rejection here happens after the entry is already filled, so the outcome
	// was an unprotected position heading for forced exit rather than a safer
	// one. Real budgets — account, cycle, portfolio (AvailableRiskUSD above),
	// the liquidation buffer and the crossed-price check — still veto.
	marginCapExceeded := c.RiskLeverageFallback && c.RiskLeverageMaxLoss > 0 &&
		marginLossPct > c.RiskLeverageMaxLoss+1e-9
	return &ProtectionPlan{
		EntryPrice: in.EntryPrice, StopPrice: stop, StopDistance: distance, ATR: in.ATR,
		SlippageBPS: c.RiskSlippageBufferBPS, RoundTripFeeBPS: c.RiskRoundTripFeeBPS,
		MaxRiskUSD: in.AvailableRiskUSD, MaxNotional: in.PlannedNotional, PlannedNotional: in.PlannedNotional,
		ExpectedLossUSD: expected, ExpectedLossPct: expected / in.Equity, ExpectedMarginLossPct: marginLossPct,
		GovernedBy: "ai_stop", MarginCapExceeded: marginCapExceeded,
	}, nil
}

// ComputeAccountProtectionDistance protects the real ordinary-copy position
// without resizing it. Market structure/ATR choose the preferred distant stop;
// volatility_first treats the account percentage as an observability threshold,
// while the backward-compatible account_cap mode may tighten that distance.
// Liquidation safety is applied later by finalizeStopLossPrice.
func ComputeAccountProtectionDistance(c *CopyConfig, side SideType, entryPrice, positionNotional, accountEquity, atr, atrDistance, structureInvalidation, maxAccountLossPct float64, leverage int) (RiskDistanceResult, error) {
	if c == nil || entryPrice <= 0 || positionNotional <= 0 || accountEquity <= 0 || atrDistance <= 0 {
		return RiskDistanceResult{}, fmt.Errorf("invalid account protection input")
	}
	if side != SideLong && side != SideShort {
		return RiskDistanceResult{}, fmt.Errorf("invalid account protection side %s", side)
	}
	if maxAccountLossPct < 0.001 || maxAccountLossPct > 0.30 {
		return RiskDistanceResult{}, fmt.Errorf("max account loss pct %.6f must be between 0.001 and 0.30", maxAccountLossPct)
	}
	if leverage <= 0 {
		leverage = 1
	}

	distance := atrDistance
	governedBy := "atr"
	structureDistance := 0.0
	if side == SideLong && structureInvalidation > 0 && structureInvalidation < entryPrice {
		structureDistance = entryPrice - structureInvalidation + 0.25*atr
	}
	if side == SideShort && structureInvalidation > entryPrice {
		structureDistance = structureInvalidation - entryPrice + 0.25*atr
	}
	if structureDistance > distance {
		distance = structureDistance
		governedBy = "structure"
	}
	minimumExecutionDistance := entryPrice * math.Max(c.RiskSlippageBufferBPS, 1) / 10000
	if distance < minimumExecutionDistance {
		distance = minimumExecutionDistance
		governedBy = "execution_minimum"
	}

	frictionRate := (c.RiskSlippageBufferBPS + c.RiskRoundTripFeeBPS) / 10000
	if frictionRate < 0 {
		frictionRate = 0
	}
	// The position margin ceiling is a reporting threshold here, never a
	// distance tightener.
	//
	// Its price-space form is entry × (RiskLeverageMaxLoss/leverage − friction),
	// i.e. inversely proportional to leverage, so a single percentage produces
	// wildly different noise tolerance: at a 0.75% hourly ATR it leaves 6.4 ATR
	// of room at 10x but only 0.37 ATR at 100x. Letting it win the min() pushed
	// the stop inside the noise band on every high-leverage position, and the
	// production ledger shows exactly that — 137 stopped cycles realized
	// -968.69 against a -843.77 hold-to-leader-close baseline, with the sub-0.3
	// ATR bucket alone averaging -15.28 per stop.
	//
	// Auto-deriving the percentage per leverage does not help either: solving
	// RiskLeverageMaxLoss = leverage × (k×ATR% + friction) collapses the cap
	// distance back to exactly k×ATR, so a volatility-first distance already is
	// the correctly leverage-tiered answer. The ceiling can therefore only ever
	// bind on the harmful side, and the physical limit that must still be
	// respected — liquidation — is enforced by finalizeStopLossPrice.
	marginCapExceeded := false
	marginCapDistance := 0.0
	if c.RiskLeverageFallback && c.RiskLeverageMaxLoss > 0 {
		initialMargin := positionNotional / float64(leverage)
		priceRiskBudget := initialMargin*c.RiskLeverageMaxLoss - positionNotional*frictionRate
		if priceRiskBudget <= 0 {
			return RiskDistanceResult{}, fmt.Errorf("trading friction exhausts position margin loss cap")
		}
		marginCapDistance = priceRiskBudget / positionNotional * entryPrice
		marginCapExceeded = distance > marginCapDistance
	}
	noiseConflict := false
	if c.RiskStopPriority == "account_cap" {
		priceRiskBudget := accountEquity*maxAccountLossPct - positionNotional*frictionRate
		if priceRiskBudget <= 0 {
			return RiskDistanceResult{}, fmt.Errorf("trading friction exhausts account position loss cap")
		}
		accountCapDistance := priceRiskBudget / positionNotional * entryPrice
		if accountCapDistance < distance {
			distance = accountCapDistance
			governedBy = "account_cap"
			noiseConflict = true
		}
	}
	if distance <= 0 || distance >= entryPrice {
		return RiskDistanceResult{}, fmt.Errorf("account protection distance %.8f is invalid", distance)
	}

	expected := positionNotional * (distance/entryPrice + frictionRate)
	result := RiskDistanceResult{
		Distance:                     distance,
		ExpectedLossUSD:              expected,
		ExpectedLossPct:              expected / accountEquity,
		GovernedBy:                   governedBy,
		NoiseConflict:                noiseConflict,
		AccountRiskThresholdExceeded: expected/accountEquity > maxAccountLossPct+1e-12,
		MarginCapDistance:            marginCapDistance,
		MarginCapExceeded:            marginCapExceeded,
	}
	if initialMargin := positionNotional / float64(leverage); initialMargin > 0 {
		result.ExpectedMarginLossPct = expected / initialMargin
	}
	if atr > 0 {
		result.DistanceATRRatio = distance / atr
	}
	return result, nil
}

// AvailableCopyGuardRiskUSD applies the same three-layer minimum used by the
// atomic reservation. Usage must exclude the attempt being resized.
func AvailableCopyGuardRiskUSD(c *CopyConfig, equity float64, usage store.CopyGuardRiskUsage) (float64, error) {
	if c == nil || equity <= 0 {
		return 0, fmt.Errorf("invalid risk capacity input")
	}
	available := equity * c.RiskAccountPct
	cycleRemaining := equity*c.RiskCycleLossBudgetPct - usage.CycleUsedUSD
	portfolioRemaining := equity*c.RiskPortfolioLossBudgetPct - usage.PortfolioUsedUSD
	if cycleRemaining < available {
		available = cycleRemaining
	}
	if portfolioRemaining < available {
		available = portfolioRemaining
	}
	if available <= 1e-9 {
		return 0, fmt.Errorf("copy guard risk budget exhausted")
	}
	return available, nil
}

// BuildProtectionPlan keeps market structure and risk sizing in one contract
// shared by initial entries, add-ons and AI reentries. A wide valid stop always
// shrinks notional; it is never squeezed merely to fit the account budget.
func BuildProtectionPlan(c *CopyConfig, side SideType, entryPrice, atr, structureInvalidation, equity, budgetPct, nominalCap float64) (*ProtectionPlan, error) {
	if c == nil || entryPrice <= 0 || atr <= 0 || equity <= 0 || budgetPct <= 0 || nominalCap <= 0 {
		return nil, fmt.Errorf("invalid protection plan input")
	}
	distance := atr * c.RiskATRMultiplier
	structureDistance := float64(0)
	if side == SideLong && structureInvalidation > 0 && structureInvalidation < entryPrice {
		structureDistance = entryPrice - structureInvalidation + 0.25*atr
	}
	if side == SideShort && structureInvalidation > entryPrice {
		structureDistance = structureInvalidation - entryPrice + 0.25*atr
	}
	if structureDistance > distance {
		distance = structureDistance
	}
	minimumExecutionDistance := entryPrice * math.Max(c.RiskSlippageBufferBPS, 1) / 10000
	if minimumExecutionDistance > distance {
		distance = minimumExecutionDistance
	}
	if distance > 4*atr {
		return nil, fmt.Errorf("protection distance %.8f exceeds 4 ATR %.8f", distance, 4*atr)
	}
	maxNotional, err := MaxNotionalForRiskDistance(c, equity, entryPrice, distance, budgetPct, nominalCap)
	if err != nil {
		return nil, fmt.Errorf("risk sizing failed: %w", err)
	}
	if maxNotional <= 0 {
		return nil, fmt.Errorf("risk sizing produced zero notional")
	}
	stopPrice := entryPrice - distance
	if side == SideShort {
		stopPrice = entryPrice + distance
	}
	if stopPrice <= 0 {
		return nil, fmt.Errorf("computed stop price is invalid")
	}
	frictionRate := (c.RiskSlippageBufferBPS + c.RiskRoundTripFeeBPS) / 10000
	expected := maxNotional * (distance/entryPrice + frictionRate)
	return &ProtectionPlan{EntryPrice: entryPrice, StopPrice: stopPrice, StopDistance: distance, ATR: atr, StructureInvalidation: structureInvalidation, SlippageBPS: c.RiskSlippageBufferBPS, RoundTripFeeBPS: c.RiskRoundTripFeeBPS, MaxRiskUSD: equity * budgetPct, MaxNotional: maxNotional, PlannedNotional: maxNotional, ExpectedLossUSD: expected, ExpectedLossPct: expected / equity}, nil
}

// ComputeRiskDistanceV4 is pure and unit-explicit; exchange/network concerns stay outside it.
//
// Copy Guard v5.2 止损：各项纯取严（min），任何机制不得放宽——
//
//	distance = min( ATR 基线      = atrDistance（默认 2.0×ATR，抗噪主力线）,
//	                [可选] 保证金 = entry × RiskLeverageMaxLoss / leverage,
//	                单次风险预算 = equity × RiskAccountPct / notional × entry（v7 默认 2%） )
//
// 仓位保证金止损由配置显式控制；新配置和已迁移的全局止损配置默认开启 50%。
// 高杠杆下该上限可能比 ATR 基线更早生效，这是用户选择的仓位亏损硬上限；
// AI 重入遇到止损结构与硬上限冲突时拒绝成交，不会暗中替换 AI 止损。
//
// v4.1 的噪音下限（noise_floor）放宽机制已删除：它在高杠杆下覆盖保证金 cap，
// 是实盘 -40% 止损（周期 64）的直接根因。ATR 不再放宽硬止损，但保留基线、
// 重入边界/追价/波动扩张与易扫损提示等全部其他职能。
// RiskStopMode 不再影响计算，字段仅作兼容保留。普通跟单是否启用账户
// 硬 cap 由 RiskStopPriority 决定；本函数属于 AI 独立风险预算链。
func ComputeRiskDistanceV4(c *CopyConfig, entryPrice, positionNotional, accountEquity, atrDistance float64, leverage int) (RiskDistanceResult, error) {
	if c == nil || entryPrice <= 0 || positionNotional <= 0 || accountEquity <= 0 || atrDistance <= 0 {
		return RiskDistanceResult{}, fmt.Errorf("invalid v4 risk calculation input")
	}
	if leverage <= 0 {
		leverage = 1
	}
	r := RiskDistanceResult{Distance: atrDistance, GovernedBy: "atr"}
	if c.RiskLeverageFallback && c.RiskLeverageMaxLoss > 0 {
		marginCapDistance := entryPrice * c.RiskLeverageMaxLoss / float64(leverage)
		if marginCapDistance < r.Distance {
			r.Distance = marginCapDistance
			r.GovernedBy = "margin_cap"
			r.NoiseConflict = true
		}
	}
	frictionRate := (c.RiskSlippageBufferBPS + c.RiskRoundTripFeeBPS) / 10000
	if frictionRate < 0 {
		frictionRate = 0
	}
	if c.RiskAccountPct > 0 {
		// ai_guarded 的止损距离由 ATR/结构失效决定，账户预算只能缩仓，
		// 不能把止损压进噪音区。legacy_rule 保留原兼容计算。
		budgetUSD := accountEquity * c.RiskAccountPct
		if c.RiskReentryDecisionMode != "ai_guarded" {
			priceRiskBudget := budgetUSD - positionNotional*frictionRate
			if priceRiskBudget <= 0 {
				return RiskDistanceResult{}, fmt.Errorf("trading friction exhausts attempt risk budget")
			}
			accountDistance := priceRiskBudget / positionNotional * entryPrice
			if accountDistance < r.Distance {
				r.Distance = accountDistance
				r.GovernedBy = "account_cap"
				r.NoiseConflict = true
			}
		}
	}
	if c.RiskATRMultiplier > 0 {
		if rawATR := atrDistance / c.RiskATRMultiplier; rawATR > 0 {
			r.DistanceATRRatio = r.Distance / rawATR
		}
	}
	r.ExpectedLossUSD = positionNotional*r.Distance/entryPrice + positionNotional*frictionRate
	r.ExpectedLossPct = r.ExpectedLossUSD / accountEquity
	if c.RiskReentryDecisionMode == "ai_guarded" && c.RiskCycleLossBudgetPct > 0 && r.ExpectedLossPct > c.RiskCycleLossBudgetPct+1e-9 {
		return RiskDistanceResult{}, fmt.Errorf("ATR/structure stop risk %.4f exceeds cycle hard budget %.4f", r.ExpectedLossPct, c.RiskCycleLossBudgetPct)
	}
	if margin := positionNotional / float64(leverage); margin > 0 {
		r.ExpectedMarginLossPct = r.ExpectedLossUSD / margin
	}
	return r, nil
}

// MaxNotionalForRiskDistance sizes the position around a market-valid stop.
// Fees and slippage are inside the budget; AI may only multiply the result by
// a factor <=1 and therefore can never increase deterministic risk.
func MaxNotionalForRiskDistance(c *CopyConfig, equity, entryPrice, stopDistance, budgetPct, nominalCap float64) (float64, error) {
	if c == nil || equity <= 0 || entryPrice <= 0 || stopDistance <= 0 || budgetPct <= 0 {
		return 0, fmt.Errorf("invalid risk sizing input")
	}
	frictionRate := (c.RiskSlippageBufferBPS + c.RiskRoundTripFeeBPS) / 10000
	if frictionRate < 0 {
		frictionRate = 0
	}
	riskRate := stopDistance/entryPrice + frictionRate
	if riskRate <= 0 {
		return 0, fmt.Errorf("invalid per-unit risk")
	}
	maxNotional := equity * budgetPct / riskRate
	if nominalCap > 0 && maxNotional > nominalCap {
		maxNotional = nominalCap
	}
	return maxNotional, nil
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
	// In ATR/legacy configs zero means "field absent" and is defaulted below.
	// Once fixed mode is explicitly selected it is a real user value: accepting
	// zero here would silently turn an invalid 0% request into the 80% default.
	if c.RiskProtectionMode == store.RiskProtectionModePositionMarginPct &&
		invalidRange(c.RiskPositionMarginStopPct, 0.01, 0.99) {
		return fmt.Errorf("risk_position_margin_stop_pct must be 1%%..99%%")
	}
	// API/存储路径在校验前会补默认值，纯函数调用与旧测试也必须得到
	// 同一契约；复制后补值避免校验函数意外修改调用方配置。
	normalized := *c
	normalized.FillRiskDefaults()
	c = &normalized
	if !SupportsCopyGuard(c.ProviderType) {
		return fmt.Errorf("copy guard v4 is only supported for OKX or Binance leader sources")
	}
	if c.RiskProtectionMode != store.RiskProtectionModeATRStructure &&
		c.RiskProtectionMode != store.RiskProtectionModePositionMarginPct {
		return fmt.Errorf("risk_protection_mode must be atr_structure or position_margin_pct")
	}
	if invalidRange(c.RiskPositionMarginStopPct, 0.01, 0.99) {
		return fmt.Errorf("risk_position_margin_stop_pct must be 1%%..99%%")
	}
	if c.RiskProtectionMode == store.RiskProtectionModePositionMarginPct {
		if c.RiskTriggerPriceType != "mark" {
			return fmt.Errorf("position_margin_pct requires mark trigger price")
		}
		if c.RiskReentryEnabled || c.RiskManualReentryEnabled ||
			c.RiskReentryDecisionMode != "disabled" || c.RiskMaxReentries != 0 {
			return fmt.Errorf("position_margin_pct forbids AI, automatic and manual reentry")
		}
	}
	if c.RiskStopMode != "volatility_priority" && c.RiskStopMode != "account_hard_limit" {
		return fmt.Errorf("invalid risk_stop_mode %q", c.RiskStopMode)
	}
	if c.RiskStopPriority != "volatility_first" && c.RiskStopPriority != "account_cap" {
		return fmt.Errorf("risk_stop_priority must be volatility_first or account_cap")
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
	if invalidRange(c.RiskCycleLossBudgetPct, c.RiskAccountPct, 0.20) {
		return fmt.Errorf("risk_cycle_loss_budget_pct must be >= risk_account_pct and <= 20%%")
	}
	if invalidRange(c.RiskPortfolioLossBudgetPct, c.RiskCycleLossBudgetPct, 0.20) {
		return fmt.Errorf("risk_portfolio_loss_budget_pct must be >= cycle budget and <= 20%%")
	}
	if invalidRange(c.RiskRoundTripFeeBPS, 0, 1000) {
		return fmt.Errorf("risk_round_trip_fee_bps must be 0..1000")
	}
	if invalidRange(c.RiskSlippageBufferBPS, 0, 1000) {
		return fmt.Errorf("risk_slippage_buffer_bps must be 0..1000")
	}
	if c.RiskLeverageFallback && invalidRange(c.RiskLeverageMaxLoss, 0.01, 1) {
		return fmt.Errorf("risk_leverage_max_loss must be 1%%..100%% when position stop is enabled")
	}
	if invalidRange(c.RiskLiquidationBufferATR, 0, 5) {
		return fmt.Errorf("risk_liquidation_buffer_atr must be 0..5")
	}
	// 上界 3 与 RiskATRMultiplier 的 0.5..5 呼应：下限设得比止损距离基线还高
	// 会让绝大多数仓位判为不可保护，等于变相关闭止损。
	if c.RiskMinStopATRRatio < 0 || c.RiskMinStopATRRatio > 3 {
		return fmt.Errorf("risk_min_stop_atr_ratio must be 0..3")
	}
	maxReentries := 10
	if c.RiskReentryDecisionMode == "ai_guarded" {
		maxReentries = 2
	}
	if c.RiskMaxReentries < 0 || c.RiskMaxReentries > maxReentries {
		return fmt.Errorf("risk_max_reentries must be 0..%d", maxReentries)
	}
	maxRatio := 1.0
	if c.RiskReentryDecisionMode == "ai_guarded" {
		maxRatio = 0.5
	}
	if invalidRange(c.RiskReentryRatio, 0.1, maxRatio) {
		return fmt.Errorf("risk_reentry_ratio must be 0.1..%.1f", maxRatio)
	}
	if c.RiskReentryDecisionMode != "legacy_rule" && c.RiskReentryDecisionMode != "ai_guarded" && c.RiskReentryDecisionMode != "disabled" {
		return fmt.Errorf("risk_reentry_decision_mode must be legacy_rule, ai_guarded or disabled")
	}
	if invalidRange(c.RiskAIConfidenceThreshold, 0.70, 0.95) {
		return fmt.Errorf("risk_ai_confidence_threshold must be 0.70..0.95")
	}
	if c.RiskAIMinReviewSeconds < 300 || c.RiskAIMinReviewSeconds > 21600 {
		return fmt.Errorf("risk_ai_min_review_seconds must be 300..21600")
	}
	// Legacy call-limit fields are cost-warning thresholds only. They remain
	// positive for backwards-compatible UI/reporting but never cap eligibility,
	// and deliberately have no upper bound.
	if c.RiskAIDailyCallLimit < 1 || c.RiskAILifecycleCallLimit < 1 {
		return fmt.Errorf("AI cost warning thresholds must be positive")
	}
	if c.RiskNotificationLevel != "critical" && c.RiskNotificationLevel != "important" && c.RiskNotificationLevel != "verbose" {
		return fmt.Errorf("risk_notification_level must be critical, important or verbose")
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
	if math.IsNaN(c.RiskReentryMinNotional) || math.IsInf(c.RiskReentryMinNotional, 0) || c.RiskReentryMinNotional < 0 {
		return fmt.Errorf("risk_reentry_min_notional must be >= 0")
	}
	if c.RiskAddonBudgetPct != 0 && invalidRange(c.RiskAddonBudgetPct, 0.01, 1) {
		return fmt.Errorf("risk_addon_budget_pct must be 1%%..100%%")
	}
	if invalidRange(c.RiskReentryMinRecoveryATR, 0, 5) {
		return fmt.Errorf("risk_reentry_min_recovery_atr must be 0..5")
	}
	if c.RiskReentryCooldownEscalation != 0 && invalidRange(c.RiskReentryCooldownEscalation, 1, 10) {
		return fmt.Errorf("risk_reentry_cooldown_escalation must be 1..10")
	}
	if c.RiskReentryRecoveryEscalation != 0 && invalidRange(c.RiskReentryRecoveryEscalation, 1, 10) {
		return fmt.Errorf("risk_reentry_recovery_escalation must be 1..10")
	}
	if c.RiskUnprotectableAction != "" && c.RiskUnprotectableAction != "close" && c.RiskUnprotectableAction != "follow" {
		return fmt.Errorf("risk_unprotectable_action must be close or follow")
	}
	if c.RiskUnprotectableDisposition != "" && c.RiskUnprotectableDisposition != "warn" && c.RiskUnprotectableDisposition != "close" {
		return fmt.Errorf("risk_unprotectable_disposition must be warn or close")
	}
	return nil
}

func invalidRange(v, min, max float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0) || v < min || v > max
}

func invalidPositive(v float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0) || v <= 0
}

func invalidNonNegative(v float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0) || v < 0
}

func ValidateStoredRiskPolicy(c *store.CopyTradeConfig) error {
	if c == nil {
		return nil
	}
	if c.RiskPolicyVersion >= 4 && c.RiskProtectionMode == store.RiskProtectionModePositionMarginPct &&
		invalidRange(c.RiskPositionMarginStopPct, 0.01, 0.99) {
		return fmt.Errorf("risk_position_margin_stop_pct must be 1%%..99%%")
	}
	c.FillRiskDefaults()
	if c.CopyCatchupWindowSeconds < 1 || c.CopyCatchupWindowSeconds > 600 {
		return fmt.Errorf("copy_catchup_window_seconds must be 1..600")
	}
	if invalidRange(c.CopyCatchupMaxAdverseBPS, 0.1, 500) {
		return fmt.Errorf("copy_catchup_max_adverse_bps must be 0.1..500")
	}
	if c.RiskPolicyVersion < 4 {
		return nil
	}
	return ValidateRiskPolicyV4(&CopyConfig{ProviderType: ProviderType(c.ProviderType), RiskPolicyVersion: c.RiskPolicyVersion, RiskProtectionMode: c.RiskProtectionMode, RiskPositionMarginStopPct: c.RiskPositionMarginStopPct, RiskStopMode: c.RiskStopMode, RiskStopPriority: c.RiskStopPriority, RiskATRPeriod: c.RiskATRPeriod, RiskATRCacheMaxAgeMinutes: c.RiskATRCacheMaxAgeMinutes, RiskATRMultiplier: c.RiskATRMultiplier, RiskATRFallbackPct: c.RiskATRFallbackPct, RiskTriggerPriceType: c.RiskTriggerPriceType, RiskAccountPct: c.RiskAccountPct, RiskLeverageFallback: c.RiskLeverageFallback, RiskLeverageMaxLoss: c.RiskLeverageMaxLoss, RiskCycleLossBudgetPct: c.RiskCycleLossBudgetPct, RiskPortfolioLossBudgetPct: c.RiskPortfolioLossBudgetPct, RiskRoundTripFeeBPS: c.RiskRoundTripFeeBPS, RiskSlippageBufferBPS: c.RiskSlippageBufferBPS, RiskLiquidationBufferATR: c.RiskLiquidationBufferATR, RiskMaxReentries: c.RiskMaxReentries, RiskReentryEnabled: c.RiskReentryEnabled, RiskManualReentryEnabled: c.RiskManualReentryEnabled, RiskReentryRatio: c.RiskReentryRatio, RiskReentryDecisionMode: c.RiskReentryDecisionMode, RiskReentryMinNotional: c.RiskReentryMinNotional, RiskAIConfidenceThreshold: c.RiskAIConfidenceThreshold, RiskAIMinReviewSeconds: c.RiskAIMinReviewSeconds, RiskAIDailyCallLimit: c.RiskAIDailyCallLimit, RiskAILifecycleCallLimit: c.RiskAILifecycleCallLimit, RiskNotificationLevel: c.RiskNotificationLevel, RiskReentryBandATR: c.RiskReentryBandATR, RiskReentryCooldownSeconds: c.RiskReentryCooldownSeconds, RiskReentryMaxChaseATR: c.RiskReentryMaxChaseATR, RiskReentryMaxATRExpansion: c.RiskReentryMaxATRExpansion, RiskWatchTimeoutMinutes: c.RiskWatchTimeoutMinutes, RiskAddonBudgetPct: c.RiskAddonBudgetPct, RiskMinStopATRRatio: c.RiskMinStopATRRatio, RiskMinStopATRRatioExplicit: c.RiskMinStopATRRatioExplicit, RiskReentryMinRecoveryATR: c.RiskReentryMinRecoveryATR, RiskReentryCooldownEscalation: c.RiskReentryCooldownEscalation, RiskReentryRecoveryEscalation: c.RiskReentryRecoveryEscalation, RiskUnprotectableDisposition: c.RiskUnprotectableDisposition, RiskUnprotectableAction: c.RiskUnprotectableAction})
}
