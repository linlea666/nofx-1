package trader

import (
	"errors"
	"math"
	"testing"

	"nofx/decision"
)

type quantityResolverMock struct{ Trader }

func (m *quantityResolverMock) ResolveExecutionInstrument(string) (*ExecutionInstrument, error) {
	return &ExecutionInstrument{BaseQuantityStep: 0.01, MinBaseQuantity: 0.01}, nil
}

type minimumQuantityResolverMock struct{ Trader }

func (m *minimumQuantityResolverMock) ResolveExecutionInstrument(string) (*ExecutionInstrument, error) {
	return &ExecutionInstrument{
		BaseQuantityStep: 0.01,
		MinBaseQuantity:  0.01,
		MinNotional:      10,
	}, nil
}

func TestQuantizeOrderIntentDirectionalPolicy(t *testing.T) {
	inst := &ExecutionInstrument{BaseQuantityStep: 0.01, MinBaseQuantity: 0.01}
	increase, err := QuantizeOrderIntent(inst, 0.366, QuantityRiskIncrease)
	if err != nil {
		t.Fatal(err)
	}
	if increase.Quantized != 0.37 || !increase.StepOverride {
		t.Fatalf("increase=%+v, want 0.37 step override", increase)
	}
	reduce, err := QuantizeOrderIntent(inst, 0.366, QuantityRiskReduce)
	if err != nil {
		t.Fatal(err)
	}
	if reduce.Quantized != 0.36 || reduce.StepOverride {
		t.Fatalf("reduce=%+v, want 0.36 without override", reduce)
	}
	protection, err := QuantizeOrderIntent(inst, 0.361, QuantityProtection)
	if err != nil {
		t.Fatal(err)
	}
	if protection.Quantized != 0.37 {
		t.Fatalf("protection=%+v, want 0.37", protection)
	}
}

func TestQuantizeOrderIntentSubLotAndMinimumLot(t *testing.T) {
	inst := &ExecutionInstrument{BaseQuantityStep: 0.01, MinBaseQuantity: 0.02}
	increase, err := QuantizeOrderIntent(inst, 0.004, QuantityRiskIncrease)
	if err != nil || increase.Quantized != 0.02 || !increase.UsedMinimum {
		t.Fatalf("increase=%+v err=%v", increase, err)
	}
	if _, err := QuantizeOrderIntent(inst, 0.019, QuantityRiskReduce); !errors.Is(err, ErrQuantitySubLot) {
		t.Fatalf("expected ErrQuantitySubLot, got %v", err)
	}
}

func TestQuantizeOrderIntentAtPriceUsesVenueMinimumByAction(t *testing.T) {
	inst := &ExecutionInstrument{
		BaseQuantityStep: 0.01,
		MinBaseQuantity:  0.01,
		MinNotional:      10,
	}
	open, err := QuantizeOrderIntentAtPrice(inst, 0.06, 100, QuantityInitialOpen)
	if err != nil {
		t.Fatal(err)
	}
	if open.Quantized != 0.10 || !open.UsedMinimum {
		t.Fatalf("initial open must promote exactly to exchange minimum: %+v", open)
	}
	if _, err := QuantizeOrderIntentAtPrice(inst, 0.06, 100, QuantityAdd); !errors.Is(err, ErrQuantityBelowMinimum) {
		t.Fatalf("small add must remain unsubmitted for later reconciliation, got %v", err)
	}
	if _, err := QuantizeOrderIntentAtPrice(inst, 0.099, 100, QuantityCatchup); !errors.Is(err, ErrQuantityBelowMinimum) {
		t.Fatalf("catch-up must never be promoted to the venue minimum and overbuy, got %v", err)
	}
	closeQty, err := QuantizeOrderIntentAtPrice(inst, 0.06, 100, QuantityFinalClose)
	if err != nil || closeQty.Quantized != 0.06 {
		t.Fatalf("final close must ignore opening min notional: %+v err=%v", closeQty, err)
	}
}

func TestMinimumExecutableQuantityCeilsNotionalToStep(t *testing.T) {
	inst := &ExecutionInstrument{
		BaseQuantityStep: 0.03,
		MinBaseQuantity:  0.03,
		MinNotional:      11,
	}
	got, err := MinimumExecutableQuantity(inst, 100)
	if err != nil || math.Abs(got-0.12) > 1e-12 {
		t.Fatalf("minimum=%v err=%v, want 0.12", got, err)
	}
}

func TestAutoTraderQuantizesConcreteAttemptWithoutRestoringReservedTarget(t *testing.T) {
	at := &AutoTrader{trader: &quantityResolverMock{}}
	dec := &decision.Decision{Symbol: "BTCUSDT", RequestedQuantity: 0.366, QuantizedQuantity: 0.37, QuantityStepOverride: true}
	got, err := at.quantizeCopyOpenQuantity(dec, 100, 0.369, true)
	if err != nil || got != 0.37 || !dec.QuantityStepOverride {
		t.Fatalf("concrete executable attempt was not quantized independently: got=%v decision=%+v err=%v", got, dec, err)
	}
	retry, err := at.quantizeCopyOpenQuantity(dec, 100, got*0.5, false)
	if err != nil || retry != 0.19 {
		t.Fatalf("explicit retry must quantize its new request: got=%v err=%v", retry, err)
	}
}

func TestMarginReducedInitialAttemptNeverPromotesAboveAffordableQuantity(t *testing.T) {
	at := &AutoTrader{trader: &minimumQuantityResolverMock{}}
	dec := &decision.Decision{
		Symbol: "BTCUSDT", CopyTradeAction: "open",
		TargetPositionSizeUSD: 10, PositionSizeUSD: 6,
	}
	if _, err := at.quantizeCopyOpenQuantity(dec, 100, .06, true); !errors.Is(err, ErrQuantityBelowMinimum) {
		t.Fatalf("margin-reduced initial attempt must wait below venue minimum instead of overbuying: %v", err)
	}
}
