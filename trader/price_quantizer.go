package trader

import (
	"math"
	"strconv"
	"strings"
)

// DefaultPriceTickSize is the legacy fallback used when a venue tick size is
// unavailable. It matches the historical OKX-only behaviour of copytrade's
// alignToTickSize so existing call sites keep their semantics.
const DefaultPriceTickSize = 0.0001

// maxPriceDecimals bounds the derived precision. No perpetual venue quotes
// finer than 1e-12, and an unbounded value would let a corrupt tick produce a
// meaningless format string.
const maxPriceDecimals = 12

// StepDecimals reports how many decimal places an exchange step (price tick or
// quantity lot) implies.
//
// The value is derived from the step's shortest round-trip decimal form rather
// than a fixed %f width: %f truncates at 6 places, which silently collapses the
// 1e-7/1e-8 ticks used by sub-cent altcoin perpetuals to zero.
func StepDecimals(step float64) int {
	if step <= 0 || math.IsNaN(step) || math.IsInf(step, 0) {
		step = DefaultPriceTickSize
	}
	s := strconv.FormatFloat(step, 'f', -1, 64)
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return 0
	}
	decimals := len(s) - dot - 1
	if decimals > maxPriceDecimals {
		decimals = maxPriceDecimals
	}
	return decimals
}

// roundToDecimals returns the float64 nearest to v rounded at the given decimal
// place. It is the step that removes the binary residue that a
// Floor/Ceil-then-multiply reintroduces.
func roundToDecimals(v float64, decimals int) float64 {
	if decimals <= 0 {
		return math.Round(v)
	}
	scale := math.Pow(10, float64(decimals))
	return math.Round(v*scale) / scale
}

// QuantizePrice snaps a price onto the venue tick grid.
//
// This is the only price rounding boundary; adapters serialize its result via
// FormatPrice and never invent their own precision.
//
// Two properties matter and neither is provided by a plain
// math.Floor(price/tick)*tick:
//
//   - The product is re-rounded to the tick's own decimal precision. Multiplying
//     an integer tick count back by a tick that has no exact binary form
//     reintroduces residue: 633826 * 0.1 evaluates to 63382.600000000006, which
//     OKX and Binance both reject (Binance as -1111 "Precision is over the
//     maximum defined for this asset"). Every retry then fails identically and
//     the position runs unprotected.
//
//   - A price already sitting on the grid is left alone. price/tick is not
//     exact in binary either, so an aligned price can evaluate to
//     633826.0000000001 and Ceil would push the protective stop one tick
//     further on every re-arm, producing endless amend churn.
//
// roundDown selects the direction for prices that are genuinely off-grid.
// Callers choose the direction that tightens protection: ceil for a long stop
// (which sits below entry), floor for a short stop (which sits above it).
func QuantizePrice(price, tick float64, roundDown bool) float64 {
	if tick <= 0 || math.IsNaN(tick) || math.IsInf(tick, 0) {
		tick = DefaultPriceTickSize
	}
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0
	}
	units := price / tick
	if nearest := math.Round(units); math.Abs(units-nearest) < 1e-9*math.Max(1, math.Abs(nearest)) {
		units = nearest
	} else if roundDown {
		units = math.Floor(units)
	} else {
		units = math.Ceil(units)
	}
	return roundToDecimals(units*tick, StepDecimals(tick))
}

// FormatPrice serializes a price with exactly the decimals its tick implies.
//
// strconv.FormatFloat(v, 'f', -1, 64) must never be used on an order price: it
// prints the shortest form that round-trips the float64, so any residual binary
// error is transmitted verbatim to the venue.
func FormatPrice(price, tick float64) string {
	if tick <= 0 || math.IsNaN(tick) || math.IsInf(tick, 0) {
		tick = DefaultPriceTickSize
	}
	return strconv.FormatFloat(price, 'f', StepDecimals(tick), 64)
}

// QuantizeAndFormatPrice snaps a price to the venue grid and serializes it.
// Adapters use this so a caller that computed its price against a stale or
// missing instrument spec still submits a grid-valid order.
func QuantizeAndFormatPrice(price, tick float64, roundDown bool) string {
	return FormatPrice(QuantizePrice(price, tick, roundDown), tick)
}

// protectiveStopRoundsDown picks the grid direction that can only tighten a
// protective stop. A long stop sits below entry and rounds up toward it; a
// short stop sits above entry and rounds down toward it. This mirrors the
// direction copytrade's stop calculator already uses, so the adapter's
// defensive re-quantization never widens a distance the risk policy bounded.
func protectiveStopRoundsDown(positionSide string) bool {
	return strings.EqualFold(strings.TrimSpace(positionSide), "short")
}
