package trader

import (
	"errors"
	"testing"
)

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
