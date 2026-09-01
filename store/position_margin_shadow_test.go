package store

import (
	"math"
	"path/filepath"
	"sync"
	"testing"
)

func TestCopyGuardStopAnchorIsAtomicImmutableAndRestartSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixed-stop-anchor.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "anchor-trader", LeaderID: "leader", LeaderPosID: "position",
		Symbol: "ETHUSDT", Side: "long", Status: CopyGuardFollowing, PolicySnapshot: "{}",
		FollowerEntryPrice: 100, FollowerNotional: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.OpenCopyGuardAttempt(cycle.ID, 0, 100, 100, 1, 0); err != nil {
		t.Fatal(err)
	}

	const callers = 8
	type result struct {
		anchor  *CopyGuardStopAnchor
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			entry := 100 + float64(i)
			anchor, created, callErr := cs.InitializeCopyGuardStopAnchor(cycle.ID, 0, CopyGuardStopAnchor{
				EntryPrice: entry, Leverage: 10, InitialMargin: entry / 10,
				Price: entry * .92, ConfiguredMarginLossPct: .80,
			})
			results <- result{anchor: anchor, created: created, err: callErr}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	createdCount := 0
	var winner *CopyGuardStopAnchor
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent anchor initialization failed: %v", got.err)
		}
		if got.created {
			createdCount++
		}
		if winner == nil {
			winner = got.anchor
		} else if *winner != *got.anchor {
			t.Fatalf("concurrent callers observed different anchors: winner=%+v got=%+v", winner, got.anchor)
		}
	}
	if createdCount != 1 || winner == nil {
		t.Fatalf("anchor winners=%d anchor=%+v", createdCount, winner)
	}
	loaded, err := cs.GetCopyGuardStopAnchor(cycle.ID, 0)
	if err != nil || *loaded != *winner {
		t.Fatalf("immutable anchor read=%+v err=%v winner=%+v", loaded, err, winner)
	}
	if err = cs.UpdateCopyGuardAttemptRiskAudit(cycle.ID, 0, 10, 10, 0, 0, "", 0, winner.Price+1, .7, "position_margin_liquidation_clamp"); err != nil {
		t.Fatal(err)
	}
	if stop, stopErr := cs.GetCopyGuardAttemptFinalStop(cycle.ID, 0); stopErr != nil || stop != winner.Price+1 {
		t.Fatalf("durable one-way stop=%v err=%v", stop, stopErr)
	}

	if err = cs.UpdateCopyGuardAttemptPosition(cycle.ID, 0, 130, 260, 2, 3); err != nil {
		t.Fatal(err)
	}
	again, created, err := cs.InitializeCopyGuardStopAnchor(cycle.ID, 0, CopyGuardStopAnchor{
		EntryPrice: 130, Leverage: 20, InitialMargin: 13, Price: 124.8,
		ConfiguredMarginLossPct: .80,
	})
	if err != nil || created || *again != *winner {
		t.Fatalf("weighted-average update overwrote immutable anchor: again=%+v created=%v err=%v winner=%+v", again, created, err, winner)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	loaded, err = st.CopyTrade().GetCopyGuardStopAnchor(cycle.ID, 0)
	if err != nil || *loaded != *winner {
		t.Fatalf("restart anchor read=%+v err=%v winner=%+v", loaded, err, winner)
	}
	if stop, stopErr := st.CopyTrade().GetCopyGuardAttemptFinalStop(cycle.ID, 0); stopErr != nil || stop != winner.Price+1 {
		t.Fatalf("restart durable one-way stop=%v err=%v", stop, stopErr)
	}
	attempts, err := st.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("restart attempt read: attempts=%+v err=%v", attempts, err)
	}
	persisted := attempts[0]
	if persisted.StopAnchorEntryPrice != winner.EntryPrice ||
		persisted.StopAnchorLeverage != winner.Leverage ||
		persisted.StopAnchorInitialMargin != winner.InitialMargin ||
		persisted.StopAnchorPrice != winner.Price ||
		persisted.StopConfiguredMarginLossPct != winner.ConfiguredMarginLossPct {
		t.Fatalf("restart changed anchor: attempt=%+v winner=%+v", persisted, winner)
	}
}

func TestCopyGuardPositionMarginStopAuditIsAtomicallyOneWay(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "fixed-stop-one-way.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "one-way-trader", LeaderID: "leader", LeaderPosID: "position",
		Symbol: "ETHUSDT", Side: "long", Status: CopyGuardFollowing, PolicySnapshot: "{}",
		FollowerEntryPrice: 100, FollowerNotional: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.OpenCopyGuardAttempt(cycle.ID, 0, 100, 100, 1, 0); err != nil {
		t.Fatal(err)
	}

	candidates := []float64{91, 95, 93, 97, 92}
	start := make(chan struct{})
	errs := make(chan error, len(candidates))
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		wg.Add(1)
		go func(stop float64) {
			defer wg.Done()
			<-start
			_, callErr := cs.TightenCopyGuardPositionMarginStopAudit(
				cycle.ID, 0, "long", 10, 10, 100, stop,
				"position_margin_liquidation_clamp",
			)
			errs <- callErr
		}(candidate)
	}
	close(start)
	wg.Wait()
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Fatalf("concurrent one-way update failed: %v", callErr)
		}
	}
	if stop, stopErr := cs.GetCopyGuardAttemptFinalStop(cycle.ID, 0); stopErr != nil || stop != 97 {
		t.Fatalf("long concurrent tightening chose stop=%v err=%v, want 97", stop, stopErr)
	}
	if stop, callErr := cs.TightenCopyGuardPositionMarginStopAudit(
		cycle.ID, 0, "long", 10, 10, 100, 94, "position_margin_anchor",
	); callErr != nil || stop != 97 {
		t.Fatalf("long wider update changed durable stop=%v err=%v", stop, callErr)
	}
	attempts, err := cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("read long attempt: attempts=%+v err=%v", attempts, err)
	}
	if math.Abs(attempts[0].ExpectedPositionLossPct-.30) > 1e-12 ||
		attempts[0].GovernedBy != "position_margin_liquidation_clamp" {
		t.Fatalf("long durable audit was widened or relabeled: %+v", attempts[0])
	}

	if err = cs.OpenCopyGuardAttempt(cycle.ID, 1, 100, 100, 1, 0); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []float64{108, 104, 106} {
		if _, err = cs.TightenCopyGuardPositionMarginStopAudit(
			cycle.ID, 1, "short", 10, 10, 100, candidate,
			"position_margin_liquidation_clamp",
		); err != nil {
			t.Fatal(err)
		}
	}
	if stop, stopErr := cs.GetCopyGuardAttemptFinalStop(cycle.ID, 1); stopErr != nil || stop != 104 {
		t.Fatalf("short tightening chose stop=%v err=%v, want 104", stop, stopErr)
	}
	if _, err = cs.TightenCopyGuardPositionMarginStopAudit(
		cycle.ID, 1, "short", 10, 10, 100, math.NaN(), "position_margin_anchor",
	); err == nil {
		t.Fatal("non-finite fixed stop entered the durable audit")
	}

	// The first real safety clamp must replace the anchor attribution. A later
	// wider proposal keeps both the tighter price and the clamp attribution.
	if err = cs.OpenCopyGuardAttempt(cycle.ID, 2, 100, 100, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.TightenCopyGuardPositionMarginStopAudit(
		cycle.ID, 2, "long", 10, 10, 100, 92, "position_margin_anchor",
	); err != nil {
		t.Fatal(err)
	}
	if stop, callErr := cs.TightenCopyGuardPositionMarginStopAudit(
		cycle.ID, 2, "long", 10, 10, 100, 95, "position_margin_liquidation_clamp",
	); callErr != nil || stop != 95 {
		t.Fatalf("first safety clamp was not persisted: stop=%v err=%v", stop, callErr)
	}
	if stop, callErr := cs.TightenCopyGuardPositionMarginStopAudit(
		cycle.ID, 2, "long", 10, 10, 100, 94, "position_margin_anchor",
	); callErr != nil || stop != 95 {
		t.Fatalf("wider proposal changed safety clamp: stop=%v err=%v", stop, callErr)
	}
	attempts, err = cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	var clamped *CopyGuardAttempt
	for _, attempt := range attempts {
		if attempt.AttemptNo == 2 {
			clamped = attempt
			break
		}
	}
	if clamped == nil || clamped.FinalStopPrice != 95 ||
		clamped.GovernedBy != "position_margin_liquidation_clamp" {
		t.Fatalf("safety clamp attribution was lost: %+v", clamped)
	}
}

func TestPositionMarginShadowTracksChangesCrossesOnceAndFinalizes(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-margin-shadow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	shadow, created, err := cs.InitializeCopyGuardPositionMarginShadow(&CopyGuardPositionMarginShadow{
		CycleID: 41, TraderID: "shadow-trader", Side: "long",
		AnchorEntryPrice: 100, AnchorLeverage: 10, AnchorInitialMargin: 10,
		AnchorStopPrice: 92, ConfiguredMarginLossPct: .80,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 10,
		EffectiveStopPrice: 92, LastLeaderSize: 1,
	})
	if err != nil || !created || shadow.AnchorStopPrice != 92 {
		t.Fatalf("initialize shadow=%+v created=%v err=%v", shadow, created, err)
	}
	if changed, err := cs.UpdateCopyGuardPositionMarginShadowPosition(41, 100, 1.5, 10, 1.5); err != nil || !changed {
		t.Fatalf("actual add was not recorded: changed=%v err=%v", changed, err)
	}
	if changed, err := cs.AdvanceCopyGuardPositionMarginShadowByLeader(41, 3, 110); err != nil || !changed {
		t.Fatalf("post-stop virtual add was not recorded: changed=%v err=%v", changed, err)
	}
	advanced, err := cs.GetCopyGuardPositionMarginShadow(41)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.AnchorEntryPrice != 100 || advanced.AnchorStopPrice != 92 ||
		math.Abs(advanced.CurrentQuantity-3) > 1e-9 || math.Abs(advanced.CurrentEntryPrice-105) > 1e-9 {
		t.Fatalf("virtual add changed anchor or weighted average is wrong: %+v", advanced)
	}

	crossed, tightened, err := cs.ObserveCopyGuardPositionMarginShadow(41, 95, 100, 105, 3, 10)
	if err != nil || crossed || !tightened {
		t.Fatalf("one-way tightening failed: crossed=%v tightened=%v err=%v", crossed, tightened, err)
	}
	// A later wider candidate is ignored and must not write.
	crossed, tightened, err = cs.ObserveCopyGuardPositionMarginShadow(41, 90, 100, 105, 3, 10)
	if err != nil || crossed || tightened {
		t.Fatalf("shadow stop widened: crossed=%v tightened=%v err=%v", crossed, tightened, err)
	}
	crossed, tightened, err = cs.ObserveCopyGuardPositionMarginShadow(41, 95, 94, 105, 3, 10)
	if err != nil || !crossed || tightened {
		t.Fatalf("first crossing was not committed exactly once: crossed=%v tightened=%v err=%v", crossed, tightened, err)
	}
	crossedAgain, _, err := cs.ObserveCopyGuardPositionMarginShadow(41, 96, 90, 105, 3, 10)
	if err != nil || crossedAgain {
		t.Fatalf("terminal shadow crossing replay was not idempotent: crossed=%v err=%v", crossedAgain, err)
	}
	if err = cs.FinalizeCopyGuardPositionMarginShadow(41, 120); err != nil {
		t.Fatal(err)
	}
	if err = cs.FinalizeCopyGuardPositionMarginShadow(41, 121); err != nil {
		t.Fatalf("shadow finalization is not idempotent: %v", err)
	}
	evaluations, err := cs.ListCopyGuardShadowEvaluations(41)
	if err != nil || len(evaluations) != 1 {
		t.Fatalf("shadow evaluation count=%d err=%v", len(evaluations), err)
	}
	evaluation := evaluations[0]
	if evaluation.Policy != CopyGuardShadowFirstEntryPositionMargin80 ||
		evaluation.Status != CopyGuardShadowScorable ||
		evaluation.DataQuality != CopyGuardShadowQualityEstimated ||
		math.Abs(evaluation.GrossPnL-(-33)) > 1e-9 || evaluation.EstimatedCost != 0 {
		t.Fatalf("unexpected shadow evaluation: %+v", evaluation)
	}
	finalized, err := cs.GetCopyGuardPositionMarginShadow(41)
	if err != nil || finalized.Status != CopyGuardPositionMarginShadowFinalized {
		t.Fatalf("shadow ledger not finalized: %+v err=%v", finalized, err)
	}
}

func TestPositionMarginShadowNoCrossIsNoSignal(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-margin-no-signal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	_, _, err = cs.InitializeCopyGuardPositionMarginShadow(&CopyGuardPositionMarginShadow{
		CycleID: 42, TraderID: "shadow-trader", Side: "short",
		AnchorEntryPrice: 100, AnchorLeverage: 20, AnchorInitialMargin: 5,
		AnchorStopPrice: 104, ConfiguredMarginLossPct: .80,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 20,
		EffectiveStopPrice: 104, LastLeaderSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.FinalizeCopyGuardPositionMarginShadow(42, 98); err != nil {
		t.Fatal(err)
	}
	evaluations, err := cs.ListCopyGuardShadowEvaluations(42)
	if err != nil || len(evaluations) != 1 || evaluations[0].Status != CopyGuardShadowNoSignal {
		t.Fatalf("no-cross shadow should be NO_SIGNAL: %+v err=%v", evaluations, err)
	}
}
