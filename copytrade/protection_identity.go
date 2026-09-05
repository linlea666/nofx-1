package copytrade

import (
	"math"
	"nofx/trader"
	"strings"
)

// A full tick is a different order, not a floating-point tolerance.
func sameProtectivePrice(actual, requested, tick float64) bool {
	if actual <= 0 || requested <= 0 || math.IsNaN(actual) || math.IsNaN(requested) {
		return false
	}
	if tick > 0 {
		return math.Abs(actual-requested) < tick*1e-6 && math.Round(actual/tick) == math.Round(requested/tick)
	}
	return math.Abs(actual-requested) <= 1e-8
}

func protectiveTriggerMatches(actual *trader.ProtectiveStopOrder, symbol, triggerType string) bool {
	return actual != nil && strings.EqualFold(actual.Symbol, symbol) && strings.EqualFold(actual.TriggerType, triggerType)
}

func protectiveMarginScopeMatches(actual *trader.ProtectiveStopOrder, requested string) bool {
	if actual == nil {
		return false
	}
	if requested == "" {
		return true
	} // Only legacy callers lack a requested scope.
	if strings.EqualFold(actual.MarginMode, requested) {
		return true
	}
	// Binance Hedge CLOSE_ALL belongs to symbol + positionSide, regardless of
	// that side's margin setting. Its query response has no marginMode field.
	// OKX exact-quantity orders must return the explicit requested tdMode.
	return actual.MarginMode == "" && actual.CoverageMode == trader.ProtectiveStopCoverageCloseAll
}
