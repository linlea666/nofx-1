package copytrade

import (
	"fmt"
	"math"

	"nofx/store"
	"nofx/trader"
)

type AIReentryNotionalPlan struct {
	StoppedNotional     float64
	BaseTargetNotional  float64
	ConfiguredMinimum   float64
	ExchangeMinimum     float64
	EffectiveMinimum    float64
	ExecutionNotional   float64
	ExecutionQuantity   float64
	PromotionMultiplier float64
	Promoted            bool
	PromotionReason     string
}

// ValidateAIStopAgainstEntryZone enforces the model contract before any risk
// reservation or order intent is created. Checking the entire entry zone (and
// not only the current/expected fill price) prevents an internally inconsistent
// thesis from becoming executable when price moves inside the approved zone.
func ValidateAIStopAgainstEntryZone(side SideType, stopPrice, entryLow, entryHigh float64) error {
	if stopPrice <= 0 || entryLow <= 0 || entryHigh < entryLow {
		return fmt.Errorf("invalid AI stop or entry zone")
	}
	switch side {
	case SideLong:
		if stopPrice >= entryLow {
			return fmt.Errorf("long AI stop %.8f must be below the entire entry zone [%.8f, %.8f]", stopPrice, entryLow, entryHigh)
		}
	case SideShort:
		if stopPrice <= entryHigh {
			return fmt.Errorf("short AI stop %.8f must be above the entire entry zone [%.8f, %.8f]", stopPrice, entryLow, entryHigh)
		}
	default:
		return fmt.Errorf("invalid AI protection side %s", side)
	}
	return nil
}

func plannedAIReentryLeverage(config *CopyConfig, leader *Position) int {
	if config == nil || !config.SyncLeverage {
		return 10
	}
	if leader != nil && leader.Leverage > 0 {
		return leader.Leverage
	}
	return 10
}

// PlanAIReentryNotional applies the single shared minimum contract. The venue
// and configured floors are both lifted to an executable quantity step, while
// the real stopped-attempt notional is an immutable promotion ceiling.
func PlanAIReentryNotional(stoppedNotional, reentryRatio, sizeFactor, configuredMinimum, price float64, inst *trader.ExecutionInstrument) (*AIReentryNotionalPlan, error) {
	if stoppedNotional <= 0 || reentryRatio <= 0 || sizeFactor <= 0 || sizeFactor > 1 || configuredMinimum < 0 || price <= 0 || inst == nil || inst.BaseQuantityStep <= 0 {
		return nil, fmt.Errorf("invalid AI reentry sizing input")
	}
	exchangeMinQty, err := trader.MinimumExecutableQuantity(inst, price)
	if err != nil {
		return nil, fmt.Errorf("resolve exchange minimum: %w", err)
	}
	venueMinQty, err := trader.MinimumExecutableOpenQuantity(inst, price)
	if err != nil {
		return nil, fmt.Errorf("resolve safe exchange minimum: %w", err)
	}
	step := inst.BaseQuantityStep
	configuredMinQty := 0.0
	if configuredMinimum > 0 {
		configuredMinQty = math.Ceil(configuredMinimum/price/step-1e-12) * step
	}
	effectiveMinQty := math.Max(venueMinQty, configuredMinQty)
	exchangeMinimum := exchangeMinQty * price
	effectiveMinimum := effectiveMinQty * price
	if effectiveMinimum > stoppedNotional+math.Max(0.01, stoppedNotional*1e-9) {
		return nil, reasonError("INELIGIBLE_PROMOTION_CEILING", "有效最低金额 %.8f 超过原止损仓位 %.8f", effectiveMinimum, stoppedNotional)
	}
	baseTarget := stoppedNotional * reentryRatio * sizeFactor
	target := math.Max(baseTarget, effectiveMinimum)
	quantity := math.Floor(target/price/step+1e-12) * step
	if quantity < effectiveMinQty-1e-12 {
		quantity = effectiveMinQty
	}
	maxQuantity := math.Floor(stoppedNotional/price/step+1e-12) * step
	if quantity > maxQuantity {
		quantity = maxQuantity
	}
	if quantity < effectiveMinQty-1e-12 || quantity <= 0 {
		return nil, reasonError("INELIGIBLE_PROMOTION_CEILING", "原止损仓位无法容纳最低可执行数量")
	}
	execution := quantity * price
	plan := &AIReentryNotionalPlan{
		StoppedNotional: stoppedNotional, BaseTargetNotional: baseTarget,
		ConfiguredMinimum: configuredMinimum, ExchangeMinimum: exchangeMinimum,
		EffectiveMinimum: effectiveMinimum, ExecutionNotional: execution, ExecutionQuantity: quantity,
	}
	if execution > baseTarget+math.Max(0.01, baseTarget*1e-9) {
		plan.Promoted = true
		plan.PromotionReason = "AI_REENTRY_PROMOTED_TO_MINIMUM"
		if baseTarget > 0 {
			plan.PromotionMultiplier = execution / baseTarget
		}
	}
	return plan, nil
}

func stoppedAttemptNotional(attempts []*store.CopyGuardAttempt, attemptNo int, fallback float64) float64 {
	for _, attempt := range attempts {
		if attempt != nil && attempt.AttemptNo == attemptNo && attempt.Status == "STOPPED" && attempt.Notional > 0 {
			return attempt.Notional
		}
	}
	return fallback
}
