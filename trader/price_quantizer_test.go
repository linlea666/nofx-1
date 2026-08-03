package trader

import (
	"math"
	"strconv"
	"testing"
)

// TestQuantizePriceRemovesBinaryResidue pins the exact production failure.
//
// OKX rejected an amend whose payload contained newSlTriggerPx
// "63382.600000000006" for BTC-USDT-SWAP (tick 0.1), and Binance rejected every
// XNYUSDT protective stop with -1111 "Precision is over the maximum defined for
// this asset", leaving the position unprotected across 17 retries. Both strings
// come from Floor(price/tick)*tick followed by FormatFloat(-1).
func TestQuantizePriceRemovesBinaryResidue(t *testing.T) {
	// The naive form is what shipped; assert it really does produce the
	// residue so this test fails loudly if Go's arithmetic ever changes.
	naive := math.Floor(63382.65/0.1) * 0.1
	if got := strconv.FormatFloat(naive, 'f', -1, 64); got != "63382.600000000006" {
		t.Fatalf("premise changed: naive alignment produced %s, expected 63382.600000000006", got)
	}

	if got := QuantizeAndFormatPrice(63382.65, 0.1, true); got != "63382.6" {
		t.Fatalf("BTC-USDT-SWAP trigger = %s, want 63382.6", got)
	}
	if got := QuantizeAndFormatPrice(0.00659523, 1e-7, false); got != "0.0065953" {
		t.Fatalf("XNYUSDT trigger = %s, want 0.0065953", got)
	}
}

// TestQuantizePriceLeavesAlignedPriceAlone guards the re-arm churn path: an
// already-valid trigger must survive recomputation unchanged, otherwise every
// protective poll amends the order one tick further and never converges.
func TestQuantizePriceLeavesAlignedPriceAlone(t *testing.T) {
	cases := []struct {
		price, tick float64
		roundDown   bool
	}{
		{63382.6, 0.1, false},
		{63382.6, 0.1, true},
		{1911.25, 0.01, false},
		{0.0065952, 1e-7, true},
		{2.674, 0.001, false},
	}
	for _, c := range cases {
		got := QuantizePrice(c.price, c.tick, c.roundDown)
		if math.Abs(got-c.price) > c.tick/2 {
			t.Fatalf("aligned price %v (tick %v, roundDown=%v) moved to %v", c.price, c.tick, c.roundDown, got)
		}
		if FormatPrice(got, c.tick) != FormatPrice(c.price, c.tick) {
			t.Fatalf("aligned price %v serialized as %s, want %s", c.price, FormatPrice(got, c.tick), FormatPrice(c.price, c.tick))
		}
	}
}

// TestQuantizePriceDirectionOnlyTightens verifies an off-grid stop is never
// pushed away from entry: a long stop rounds up toward entry, a short stop
// rounds down toward it.
func TestQuantizePriceDirectionOnlyTightens(t *testing.T) {
	const tick = 0.1
	// Long stop below a 100.0 entry rounds up, toward entry.
	if got := QuantizePrice(98.34, tick, false); got < 98.34 {
		t.Fatalf("long stop %v rounded away from entry, want >= 98.34", got)
	}
	// Short stop above a 100.0 entry rounds down, toward entry.
	if got := QuantizePrice(101.66, tick, true); got > 101.66 {
		t.Fatalf("short stop %v rounded away from entry, want <= 101.66", got)
	}
	if got := protectiveStopRoundsDown("short"); !got {
		t.Fatal("short protective stop must round down")
	}
	if got := protectiveStopRoundsDown("long"); got {
		t.Fatal("long protective stop must round up")
	}
}

// TestStepDecimalsHandlesSubMicroTicks covers the sub-cent altcoin perpetuals
// that broke the old %f-based precision derivation, which truncated at six
// decimals and collapsed a 1e-8 tick to zero.
func TestStepDecimalsHandlesSubMicroTicks(t *testing.T) {
	cases := map[float64]int{
		1: 0, 10: 0, 0.5: 1, 0.1: 1, 0.01: 2, 0.0001: 4,
		1e-7: 7, 1e-8: 8, 0.00000025: 8,
	}
	for step, want := range cases {
		if got := StepDecimals(step); got != want {
			t.Fatalf("StepDecimals(%v) = %d, want %d", step, got, want)
		}
	}
	if got := StepDecimals(0); got != StepDecimals(DefaultPriceTickSize) {
		t.Fatalf("zero tick must fall back to the default tick precision, got %d", got)
	}
}

// TestQuantizePriceRejectsInvalidInput keeps a corrupt price from being
// serialized as a plausible order.
func TestQuantizePriceRejectsInvalidInput(t *testing.T) {
	for _, p := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if got := QuantizePrice(p, 0.1, false); got != 0 {
			t.Fatalf("QuantizePrice(%v) = %v, want 0", p, got)
		}
	}
}
