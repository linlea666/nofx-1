package trader

import (
	"errors"
	"fmt"
	"math"
)

var ErrQuantitySubLot = errors.New("quantity is below one executable lot")

type QuantityIntentKind string

const (
	QuantityRiskIncrease QuantityIntentKind = "risk_increase"
	QuantityRiskReduce   QuantityIntentKind = "risk_reduce"
	QuantityProtection   QuantityIntentKind = "protection"
)

type QuantityQuantization struct {
	Requested    float64
	Quantized    float64
	Step         float64
	Minimum      float64
	UsedMinimum  bool
	StepOverride bool
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
	case QuantityRiskIncrease:
		quantized = math.Floor(requested/step+0.5+epsilon) * step
		if quantized < minimum-epsilon {
			quantized = minimum
		}
	case QuantityRiskReduce:
		quantized = math.Floor(requested/step+epsilon) * step
		if quantized < minimum-epsilon {
			return QuantityQuantization{Requested: requested, Step: step, Minimum: minimum}, ErrQuantitySubLot
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
