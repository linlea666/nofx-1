package copytrade

import (
	"errors"
	"testing"

	"nofx/decision"
)

func TestExecFailureDedupKeyStableForSameFailureState(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}
	err := errors.New("short position not found")
	dec := &decision.Decision{
		Action:        "reduce_short",
		Symbol:        "ETHUSDT",
		CloseRatio:    0.5154639175,
		MarginMode:    "cross",
		LeaderPosID:   "1243719130_ETHUSDT_SHORT",
		LeaderPosSize: 0.047,
	}

	first := ti.execFailureDedupKey(dec, err)
	second := ti.execFailureDedupKey(dec, err)

	if first != second {
		t.Fatalf("same failure state produced different keys:\n%s\n%s", first, second)
	}
}

func TestExecFailureDedupKeyChangesWhenLeaderOperationChanges(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}
	err := errors.New("short position not found")
	base := &decision.Decision{
		Action:        "reduce_short",
		Symbol:        "ETHUSDT",
		CloseRatio:    0.5154639175,
		MarginMode:    "cross",
		LeaderPosID:   "1243719130_ETHUSDT_SHORT",
		LeaderPosSize: 0.047,
	}
	next := *base
	next.LeaderPosSize = 0.02
	next.CloseRatio = 0.5744680851

	if ti.execFailureDedupKey(base, err) == ti.execFailureDedupKey(&next, err) {
		t.Fatalf("different leader operation should produce a different dedupe key")
	}
}
