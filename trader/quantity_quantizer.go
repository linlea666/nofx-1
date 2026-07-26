package trader

import (
	"errors"
	"fmt"
	"math"
)

var ErrQuantitySubLot = errors.New("quantity is below one executable lot")
var ErrQuantityBelowMinimum = errors.New("quantity is below exchange minimum")

type QuantityIntentKind string

const (
	QuantityInitialOpen   QuantityIntentKind = "initial_open"
	QuantityAdd           QuantityIntentKind = "add"
	QuantityPartialReduce QuantityIntentKind = "partial_reduce"
	QuantityFinalClose    QuantityIntentKind = "final_close"
	QuantityAIReentry     QuantityIntentKind = "ai_reentry"
	QuantityProtection    QuantityIntentKind = "protection"

	// Deprecated compatibility aliases. New copy-trade call sites must select
	// an action-specific intent so a small add can never inherit open promotion.
	QuantityRiskIncrease QuantityIntentKind = "risk_increase"
	QuantityRiskReduce   QuantityIntentKind = "risk_reduce"
)

type QuantityQuantization struct {
	Requested    float64
	Quantized    float64
	Step         float64
	Minimum      float64
	UsedMinimum  bool
	StepOverride bool
}

// MinimumExecutableQuantity resolves the venue's complete minimum in the base
// quantity unit used by the execution boundary. The notional component is
// rounded upward to a valid quantity step; callers must never substitute an
// operational warning threshold such as MinTradeWarn.
func MinimumExecutableQuantity(inst *ExecutionInstrument, price float64) (float64, error) {
	if inst == nil || inst.BaseQuantityStep <= 0 || price <= 0 {
		return 0, fmt.Errorf("invalid minimum executable quantity input")
	}
	step := inst.BaseQuantityStep
	minimum := math.Max(inst.MinBaseQuantity, step)
	if inst.MinNotional > 0 {
		notionalQuantity := inst.MinNotional / price
		if notionalQuantity > minimum {
			minimum = notionalQuantity
		}
	}
	const epsilon = 1e-12
	minimum = math.Ceil(minimum/step-epsilon) * step
	return math.Round(minimum/step) * step, nil
}

// QuantizeOrderIntent is the only business-level quantity rounding boundary.
// Adapters serialize its result; they do not choose a different rounding mode.
func QuantizeOrderIntent(inst *ExecutionInstrument, requested float64, kind QuantityIntentKind) (QuantityQuantization, error) {
	if inst == nil || inst.BaseQuantityStep <= 0 || requested <= 0 {
		return QuantityQuantization{}, fmt.Errorf("invalid quantity quantization input")
	}
	step := inst.BaseQuantityStep
	minimum := inst.MinBaseQuantity
	if minimum < step {
		minimum = step
	}
	const epsilon = 1e-12
	var quantized float64
	switch kind {
	case QuantityInitialOpen, QuantityRiskIncrease:
		quantized = math.Floor(requested/step+0.5+epsilon) * step
		if quantized < minimum-epsilon {
			quantized = minimum
		}
	case QuantityAdd, QuantityAIReentry:
		quantized = math.Floor(requested/step+epsilon) * step
		if quantized < minimum-epsilon {
			return QuantityQuantization{Requested: requested, Step: step, Minimum: minimum}, ErrQuantityBelowMinimum
		}
	case QuantityPartialReduce, QuantityRiskReduce:
		quantized = math.Floor(requested/step+epsilon) * step
		if quantized < minimum-epsilon {
			return QuantityQuantization{Requested: requested, Step: step, Minimum: minimum}, ErrQuantitySubLot
		}
	case QuantityFinalClose:
		// The quantity comes from the venue's real live position. Final close
		// ignores opening minimums but still stays on the native step grid.
		quantized = math.Floor(requested/step+epsilon) * step
		if quantized <= epsilon {
			return QuantityQuantization{Requested: requested, Step: step, Minimum: step}, ErrQuantitySubLot
		}
	case QuantityProtection:
		quantized = math.Ceil(requested/step-epsilon) * step
		if quantized < minimum {
			quantized = minimum
		}
	default:
		return QuantityQuantization{}, fmt.Errorf("unknown quantity intent kind %q", kind)
	}
	// Normalize floating noise back onto the integer step grid.
	units := math.Round(quantized / step)
	quantized = units * step
	return QuantityQuantization{
		Requested: requested, Quantized: quantized, Step: step, Minimum: minimum,
		UsedMinimum:  quantized == minimum && requested < minimum-epsilon,
		StepOverride: quantized > requested+epsilon,
	}, nil
}

// QuantizeOrderIntentAtPrice applies the exchange minimum-notional boundary in
// addition to quantity precision. Only an initial source open is allowed to be
// promoted to that minimum. Adds and AI reentries are rounded down and skipped
// when they cannot independently satisfy the venue.
func QuantizeOrderIntentAtPrice(inst *ExecutionInstrument, requested, price float64, kind QuantityIntentKind) (QuantityQuantization, error) {
	if requested <= 0 || price <= 0 {
		return QuantityQuantization{}, fmt.Errorf("invalid priced quantity quantization input")
	}
	minimum, err := MinimumExecutableQuantity(inst, price)
	if err != nil {
		return QuantityQuantization{}, err
	}
	q, err := QuantizeOrderIntent(inst, requested, kind)
	if err != nil {
		return q, err
	}
	q.Minimum = minimum
	const epsilon = 1e-12
	switch kind {
	case QuantityInitialOpen, QuantityRiskIncrease:
		if q.Quantized < minimum-epsilon {
			q.Quantized = minimum
			q.UsedMinimum = true
			q.StepOverride = q.Quantized > requested+epsilon
		}
	case QuantityAdd, QuantityAIReentry:
		if q.Quantized < minimum-epsilon {
			return QuantityQuantization{Requested: requested, Step: inst.BaseQuantityStep, Minimum: minimum}, ErrQuantityBelowMinimum
		}
	case QuantityPartialReduce, QuantityRiskReduce, QuantityFinalClose, QuantityProtection:
		// Reduce-only orders may be exempt from the venue opening min-notional
		// rule (Binance is one such venue). Their real constraints are the live
		// position, min quantity and native step, handled above.
	default:
		return QuantityQuantization{}, fmt.Errorf("unknown priced quantity intent kind %q", kind)
	}
	q.UsedMinimum = q.Quantized == minimum && requested < minimum-epsilon
	return q, nil
}
