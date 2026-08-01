package copytrade

import (
	"errors"
	"math"
	"testing"

	"nofx/store"
	"nofx/trader"
)

func TestPlanAIReentryNotionalPromotionAndCeiling(t *testing.T) {
	inst := &trader.ExecutionInstrument{BaseQuantityStep: 0.01, MinBaseQuantity: 0.01, MinNotional: 10}
	plan, err := PlanAIReentryNotional(100, 0.5, 0.2, 12, 100, inst)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Promoted || math.Abs(plan.ExecutionNotional-12) > 1e-9 || plan.ExchangeMinimum != 10 || plan.EffectiveMinimum != 12 {
		t.Fatalf("unexpected promotion: %+v", plan)
	}

	plan, err = PlanAIReentryNotional(100, 0.5, 0.1, 0, 100, inst)
	if err != nil || !plan.Promoted || plan.ExecutionNotional != 11 {
		t.Fatalf("zero config must use exchange floor: plan=%+v err=%v", plan, err)
	}

	_, err = PlanAIReentryNotional(9, 0.5, 1, 12, 100, inst)
	if err == nil || ReasonCodeOf(err) != "INELIGIBLE_PROMOTION_CEILING" {
		t.Fatalf("ceiling must terminally reject: %v", err)
	}
}

func TestValidateAIStopAgainstEntireEntryZone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		side    SideType
		stop    float64
		wantErr bool
	}{
		{name: "long valid", side: SideLong, stop: 99},
		{name: "long inside zone", side: SideLong, stop: 100, wantErr: true},
		{name: "short valid", side: SideShort, stop: 103},
		{name: "short inside zone", side: SideShort, stop: 102, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAIStopAgainstEntryZone(tc.side, tc.stop, 100, 102)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestPlannedAIReentryLeverageMatchesExecutionPolicy(t *testing.T) {
	leader := &Position{Leverage: 3}
	if got := plannedAIReentryLeverage(&CopyConfig{SyncLeverage: false}, leader); got != 10 {
		t.Fatalf("unsynced leverage=%d want=10", got)
	}
	if got := plannedAIReentryLeverage(&CopyConfig{SyncLeverage: true}, leader); got != 3 {
		t.Fatalf("synced leverage=%d want=3", got)
	}
	if got := plannedAIReentryLeverage(&CopyConfig{SyncLeverage: true}, &Position{}); got != 10 {
		t.Fatalf("missing leader leverage=%d want=10", got)
	}
}

func TestStoppedAttemptNotionalUsesExactStoppedGeneration(t *testing.T) {
	attempts := []*store.CopyGuardAttempt{{AttemptNo: 0, Status: "STOPPED", Notional: 80}, {AttemptNo: 1, Status: "OPEN", Notional: 30}}
	if got := stoppedAttemptNotional(attempts, 0, 10); got != 80 {
		t.Fatalf("got %v", got)
	}
	if got := stoppedAttemptNotional(attempts, 2, 10); got != 10 {
		t.Fatalf("fallback got %v", got)
	}
	var coded *ReasonCodedError
	if _, err := PlanAIReentryNotional(0, .5, 1, 0, 100, &trader.ExecutionInstrument{BaseQuantityStep: .01}); err == nil || errors.As(err, &coded) {
		t.Fatal("invalid input is not a lifecycle reason-code rejection")
	}
}
