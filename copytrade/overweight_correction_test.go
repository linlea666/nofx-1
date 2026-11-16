package copytrade

import (
	"math"
	"testing"
	"time"

	"nofx/decision"
	"nofx/trader"
)

type overweightExecutor struct {
	reduced  []float64
	minimum  float64
	reduceTo string
}

func (x *overweightExecutor) ExecuteDecision(*decision.Decision) error { return nil }
func (x *overweightExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{"total_equity": 100.0}, nil
}
func (x *overweightExecutor) GetPositions() ([]map[string]interface{}, error) { return nil, nil }
func (x *overweightExecutor) ReducePositionMarket(symbol, side string, quantity float64) (string, error) {
	x.reduceTo = symbol + "|" + side
	x.reduced = append(x.reduced, quantity)
	return "order-1", nil
}
func (x *overweightExecutor) ResolveExecutionInstrument(symbol string) (*trader.ExecutionInstrument, error) {
	return &trader.ExecutionInstrument{SourceSymbol: symbol, NativeSymbol: symbol, BaseQuantityStep: 0.01, MinBaseQuantity: x.minimum}, nil
}

// The live incident: target ≈ 800 (8x leader leverage on 100 equity), held 8000.
func overweightIntegration(t *testing.T, provider ProviderType, followerValue float64) (*TraderIntegration, *overweightExecutor) {
	t.Helper()
	e := reduceTestEngine(t, provider, followerValue)
	x := &overweightExecutor{minimum: 0.01}
	ti := &TraderIntegration{traderID: e.traderID, store: e.store, engine: e, executor: x}
	return ti, x
}

func TestOverweightIsReducedToTargetAfterTwoChecks(t *testing.T) {
	ti, x := overweightIntegration(t, ProviderHyperliquid, 8000)
	ti.correctProportionalOverweight()
	if len(x.reduced) != 0 {
		t.Fatal("a single check must not reduce")
	}
	ti.correctProportionalOverweight() // throttled: still one strike
	if len(x.reduced) != 0 {
		t.Fatal("checks inside the interval must not count twice")
	}
	ti.lastOverweightCheck = time.Now().Add(-overweightCheckInterval)
	ti.correctProportionalOverweight()
	// 80 SOL held, target 8 SOL → reduce 72.
	if len(x.reduced) != 1 || math.Abs(x.reduced[0]-72) > 1e-9 || x.reduceTo != "SOLUSDT|long" {
		t.Fatalf("overweight not reduced to target: %+v %s", x.reduced, x.reduceTo)
	}
	var events int
	if err := ti.store.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_events WHERE event_type='PROPORTIONAL_OVERWEIGHT_CORRECTED'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("correction not logged: %d %v", events, err)
	}
}

func TestOverweightCorrectionGuards(t *testing.T) {
	recheck := func(ti *TraderIntegration) {
		ti.correctProportionalOverweight()
		ti.lastOverweightCheck = time.Now().Add(-overweightCheckInterval)
		ti.correctProportionalOverweight()
	}
	// Within 1.5x of the target: left alone.
	ti, x := overweightIntegration(t, ProviderHyperliquid, 1100)
	recheck(ti)
	if len(x.reduced) != 0 {
		t.Fatalf("mild overweight reduced: %+v", x.reduced)
	}
	// Disabled by the trader.
	ti, x = overweightIntegration(t, ProviderHyperliquid, 8000)
	ti.engine.config.DisableAutoCorrect = true
	recheck(ti)
	if len(x.reduced) != 0 {
		t.Fatal("disabled correction reduced")
	}
	// Copy Guard sources own their follower quantity.
	ti, x = overweightIntegration(t, ProviderOKX, 8000)
	recheck(ti)
	if len(x.reduced) != 0 {
		t.Fatal("Copy Guard source reduced")
	}
	// A target below the venue minimum keeps a minimum-sized position.
	ti, x = overweightIntegration(t, ProviderHyperliquid, 8000)
	x.minimum = 10
	recheck(ti)
	if len(x.reduced) != 0 {
		t.Fatal("minimum-promoted position reduced")
	}
	// An implausible leader denominator never triggers a reduction.
	ti, x = overweightIntegration(t, ProviderHyperliquid, 8000)
	ti.engine.leaderState.TotalEquity = 100 // 80x implied leader leverage
	recheck(ti)
	if len(x.reduced) != 0 {
		t.Fatal("reduced against an implausible denominator")
	}
}
