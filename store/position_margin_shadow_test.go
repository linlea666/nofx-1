package store

import (
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCopyGuardStopAnchorIsAtomicImmutableAndRestartSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixed-stop-anchor.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	cs := st.CopyTrade()
	policy := NewCopyGuardDefaults()
	policy.TraderID = "anchor-trader"
	policy.ProviderType = "okx"
	policy.RiskPolicyVersion = 4
	policy.RiskProtectionMode = RiskProtectionModePositionMarginPct
	policy.RiskPositionMarginStopPct = .80
	policy.FillRiskDefaults()
	policySnapshot, err := EncodeCopyGuardPolicySnapshot(policy)
	if err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&CopyTradeExecutionIntent{
		TraderID: "anchor-trader", LeaderPosID: "position",
		SourceRevision: 1, SourceKind: "LEADER_TRANSITION", CanonicalKey: "leader|anchor-trader|position|1",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		LeaderTargetSize: 1, RequestedQuantity: 1, QuantizedQuantity: 1, ClientOrderID: "anchor-open",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve initial fill intent: claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(intent.ID, ExecutionIntentSubmitted, "", "", "anchor-order", 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.CommitLeaderExecutionFill(LeaderExecutionCommit{
		IntentID: intent.ID, TraderID: "anchor-trader", LeaderID: "leader", LeaderPosID: "position",
		SourceRevision: 1, Action: "open_long", Symbol: "ETHUSDT", SourceSymbol: "ETHUSDT",
		ExecutionSymbol: "ETHUSDT", Side: "long", MarginMode: "cross", LeaderTargetSize: 1,
		FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: "anchor-open",
		ExchangeOrderID: "anchor-order", ExchangeState: "FILLED", OrderTerminal: true,
		InitialCopyGuard: &InitialCopyGuardLifecycle{
			PolicySnapshot: policySnapshot, LeaderEntryPrice: 100, AccountEquity: 1000, ATRAtEntry: 2,
		},
	}); err != nil {
		t.Fatal(err)
	}
	cycle, err := cs.GetOpenCopyGuardCycle("anchor-trader", "position")
	if err != nil {
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
				Source: CopyGuardAnchorSourceInitialFill, SourceIntentID: intent.ID,
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
		ConfiguredMarginLossPct: .80, Source: CopyGuardAnchorSourceInitialFill, SourceIntentID: intent.ID,
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
		Symbol: "ETHUSDT", Side: "long", Status: CopyGuardFollowing, PolicySnapshot: `{"version":4}`,
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
	if stop, callErr := cs.TightenCopyGuardPositionMarginStopAuditWithMark(
		cycle.ID, 0, "long", 10, 10, 100, 99, 94, "position_margin_anchor",
	); callErr != nil || stop != 97 {
		t.Fatalf("long wider update changed durable stop=%v err=%v", stop, callErr)
	}
	attempts, err := cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("read long attempt: attempts=%+v err=%v", attempts, err)
	}
	if math.Abs(attempts[0].ExpectedPositionLossPct-.30) > 1e-12 ||
		attempts[0].GovernedBy != "position_margin_liquidation_clamp" ||
		attempts[0].CurrentMarkPrice != 99 || attempts[0].CurrentMarkAt == nil {
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

func TestPositionMarginShadowV2AccountsForAddsReductionsCrossingCostsAndCoverage(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-margin-v2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "shadow-v2-trader", LeaderID: "leader", LeaderPosID: "v2-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing,
		PolicySnapshot: `{"version":4}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, created, err := cs.InitializeCopyGuardPositionMarginShadowV2(&CopyGuardPositionMarginShadowV2{
		CycleID: cycle.ID, TraderID: cycle.TraderID, Side: "long",
		AnchorEntryPrice: 100, AnchorLeverage: 10, AnchorInitialMargin: 10,
		AnchorStopPrice: 92, ConfiguredMarginLossPct: .8,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 10,
		EffectiveStopPrice: 92, LastLeaderSize: 1, ConfiguredCostBPS: 10,
	})
	if err != nil || !created {
		t.Fatalf("initialize v2 created=%v err=%v", created, err)
	}
	if changed, callErr := cs.SyncCopyGuardPositionMarginShadowV2(cycle.ID, 105, 2, 20, 2, 110); callErr != nil || !changed {
		t.Fatalf("v2 add changed=%v err=%v", changed, callErr)
	}
	if changed, callErr := cs.SyncCopyGuardPositionMarginShadowV2(cycle.ID, 105, 1.5, 20, 1.5, 108); callErr != nil || !changed {
		t.Fatalf("v2 reduction changed=%v err=%v", changed, callErr)
	}
	base := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Minute)
	checkpoint := func(key string, at time.Time, low, high, last float64) bool {
		t.Helper()
		crossed, checkpointErr := cs.CheckpointCopyGuardPositionMarginShadowV2(cycle.ID, CopyGuardShadowMarkCheckpoint{
			Key: key, BucketAt: at, MinimumMark: low, MaximumMark: high, LastMark: last,
			ObservationCount: 20, CoveredSeconds: 60, ObservedAt: at.Add(time.Minute), Source: "LIVE_MARK",
		}, 92)
		if checkpointErr != nil {
			t.Fatal(checkpointErr)
		}
		return crossed
	}
	if checkpoint("minute-1", base, 99, 110, 100) {
		t.Fatal("v2 crossed above long stop")
	}
	if !checkpoint("minute-2", base.Add(time.Minute), 91, 95, 91) {
		t.Fatal("v2 did not cross at authoritative mark")
	}
	if checkpoint("minute-2", base.Add(time.Minute), 91, 95, 91) {
		t.Fatal("duplicate checkpoint crossed twice")
	}
	if checkpoint("minute-3", base.Add(2*time.Minute), 90, 101, 101) {
		t.Fatal("terminal v2 shadow crossed twice")
	}
	// Once the fixed shadow has crossed, leader close is irrelevant and a
	// missing close price must not leave the shadow lifecycle active forever.
	if err = cs.FinalizeCopyGuardPositionMarginShadow(cycle.ID, 0); err != nil {
		t.Fatal(err)
	}
	shadow, err := cs.GetCopyGuardPositionMarginShadowV2(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.Status != CopyGuardPositionMarginShadowFinalized || math.Abs(shadow.RealizedGrossPnL-(-19.5)) > 1e-9 ||
		math.Abs(shadow.EntryTurnover-210) > 1e-9 || math.Abs(shadow.ExitTurnover-190.5) > 1e-9 {
		t.Fatalf("v2 virtual ledger is wrong: %+v", shadow)
	}
	evaluations, err := cs.ListCopyGuardShadowEvaluations(cycle.ID)
	if err != nil || len(evaluations) != 1 {
		t.Fatalf("v2 evaluation rows=%+v err=%v", evaluations, err)
	}
	evaluation := evaluations[0]
	if evaluation.EvaluationVersion != CopyGuardPositionMarginShadowEvaluationVersion || !evaluation.StopCrossed ||
		!evaluation.CrossingVerified || !evaluation.PostStopReversed || evaluation.MarkCoverage != 1 ||
		math.Abs(evaluation.GrossPnL-(-19.5)) > 1e-9 || math.Abs(evaluation.EstimatedCost-.20025) > 1e-9 {
		t.Fatalf("v2 provisional evaluation is wrong: %+v", evaluation)
	}
	if _, err = st.DB().Exec(`INSERT INTO copy_trade_execution_intents
		(trader_id,leader_pos_id,source_revision,canonical_key,cycle_id,action,status,filled_quantity,filled_notional)
		VALUES(?,?,?,?,?,'open_long','FILLED',2,210),(?,?,?,?,?,'reduce_long','FILLED',1.5,190.5)`,
		cycle.TraderID, cycle.LeaderPosID, 1, "v2-open", cycle.ID,
		cycle.TraderID, cycle.LeaderPosID, 2, "v2-reduce", cycle.ID); err != nil {
		t.Fatal(err)
	}
	cycle.AccountingStatus = CopyGuardAccountingReconciled
	cycle.Fees, cycle.FundingFee, cycle.Slippage = 2, .5, .4
	if err = cs.ReconcileCopyGuardPositionMarginShadowV2(cycle, -10, true); err != nil {
		t.Fatal(err)
	}
	evaluations, err = cs.ListCopyGuardShadowEvaluations(cycle.ID)
	if err != nil || len(evaluations) != 1 {
		t.Fatalf("reconciled v2 rows=%+v err=%v", evaluations, err)
	}
	evaluation = evaluations[0]
	if evaluation.DataQuality != CopyGuardShadowQualityVerified || evaluation.CostSource != "OBSERVED_PRORATED" ||
		math.Abs(evaluation.NetPnL-(-21.4)) > 1e-9 || math.Abs(evaluation.IncrementalEffect-(-11.4)) > 1e-9 {
		t.Fatalf("v2 observed-cost comparison is wrong: %+v", evaluation)
	}
}

func TestPositionMarginShadowV2NoCrossClosesAtLeaderExit(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-margin-v2-no-cross.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "shadow-v2-no-cross", LeaderID: "leader", LeaderPosID: "no-cross",
		Symbol: "ETHUSDT", Side: "short", MarginMode: "cross", Status: CopyGuardFollowing,
		PolicySnapshot: `{"version":4}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cs.InitializeCopyGuardPositionMarginShadowV2(&CopyGuardPositionMarginShadowV2{
		CycleID: cycle.ID, TraderID: cycle.TraderID, Side: "short",
		AnchorEntryPrice: 100, AnchorLeverage: 20, AnchorInitialMargin: 5,
		AnchorStopPrice: 104, ConfiguredMarginLossPct: .8,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 2,
		CurrentEntryPrice: 100, CurrentQuantity: 2, CurrentLeverage: 20,
		EffectiveStopPrice: 104, LastLeaderSize: 2, ConfiguredCostBPS: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Minute)
	for index, mark := range []float64{101, 99} {
		if crossed, checkpointErr := cs.CheckpointCopyGuardPositionMarginShadowV2(cycle.ID, CopyGuardShadowMarkCheckpoint{
			Key: "no-cross-" + string(rune('a'+index)), BucketAt: base.Add(time.Duration(index) * time.Minute),
			MinimumMark: mark - .5, MaximumMark: mark + .5, LastMark: mark,
			ObservationCount: 20, CoveredSeconds: 60, ObservedAt: base.Add(time.Duration(index+1) * time.Minute), Source: "LIVE_MARK",
		}, 104); checkpointErr != nil || crossed {
			t.Fatalf("no-cross checkpoint %d crossed=%v err=%v", index, crossed, checkpointErr)
		}
	}
	if err = cs.FinalizeCopyGuardPositionMarginShadow(cycle.ID, 98); err != nil {
		t.Fatal(err)
	}
	evaluations, err := cs.ListCopyGuardShadowEvaluations(cycle.ID)
	if err != nil || len(evaluations) != 1 {
		t.Fatalf("no-cross v2 evaluation rows=%+v err=%v", evaluations, err)
	}
	evaluation := evaluations[0]
	if evaluation.EvaluationVersion != 2 || evaluation.StopCrossed || evaluation.ExitPrice != 98 ||
		evaluation.MarkCoverage != 1 || math.Abs(evaluation.GrossPnL-4) > 1e-9 ||
		math.Abs(evaluation.EstimatedCost-.396) > 1e-9 {
		t.Fatalf("no-cross v2 leader-close result is wrong: %+v", evaluation)
	}
}

func TestPositionMarginShadowV2IncompleteMarkPathIsUnscorable(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-margin-v2-gap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "shadow-v2-gap", LeaderID: "leader", LeaderPosID: "gap",
		Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing,
		PolicySnapshot: `{"version":4}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cs.InitializeCopyGuardPositionMarginShadowV2(&CopyGuardPositionMarginShadowV2{
		CycleID: cycle.ID, TraderID: cycle.TraderID, Side: "long",
		AnchorEntryPrice: 100, AnchorLeverage: 10, AnchorInitialMargin: 10,
		AnchorStopPrice: 92, ConfiguredMarginLossPct: .8,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 10,
		EffectiveStopPrice: 92, LastLeaderSize: 1, ConfiguredCostBPS: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Minute)
	crossed, err := cs.CheckpointCopyGuardPositionMarginShadowV2(cycle.ID, CopyGuardShadowMarkCheckpoint{
		Key: "gap-cross", BucketAt: base, MinimumMark: 91, MaximumMark: 101, LastMark: 91,
		ObservationCount: 1, CoveredSeconds: 60, GapSeconds: 180,
		ObservedAt: base.Add(4 * time.Minute), Source: "HISTORY_MARK_1M",
	}, 92)
	if err != nil || !crossed {
		t.Fatalf("gap crossing crossed=%v err=%v", crossed, err)
	}
	if err = cs.FinalizeCopyGuardPositionMarginShadow(cycle.ID, 110); err != nil {
		t.Fatal(err)
	}
	evaluations, err := cs.ListCopyGuardShadowEvaluations(cycle.ID)
	if err != nil || len(evaluations) != 1 {
		t.Fatalf("gap v2 evaluation rows=%+v err=%v", evaluations, err)
	}
	evaluation := evaluations[0]
	if evaluation.Status != CopyGuardShadowUnscorable || evaluation.DataQuality != CopyGuardShadowQualityUnscorable ||
		math.Abs(evaluation.MarkCoverage-.25) > 1e-9 || evaluation.PostStopReversed ||
		!strings.Contains(evaluation.Reason, "below 95%") {
		t.Fatalf("incomplete mark path remained scoreable: %+v", evaluation)
	}
}

func TestPositionMarginShadowV2MissingLeaderCloseFinalizesUnscorable(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "position-margin-v2-missing-close.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "shadow-v2-missing-close", LeaderID: "leader", LeaderPosID: "missing-close",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing,
		PolicySnapshot: `{"version":4}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cs.InitializeCopyGuardPositionMarginShadowV2(&CopyGuardPositionMarginShadowV2{
		CycleID: cycle.ID, TraderID: cycle.TraderID, Side: "long",
		AnchorEntryPrice: 100, AnchorLeverage: 10, AnchorInitialMargin: 10,
		AnchorStopPrice: 92, ConfiguredMarginLossPct: .8,
		PriceTickSize: .1, QuantityStep: .01, InitialQuantity: 1,
		CurrentEntryPrice: 100, CurrentQuantity: 1, CurrentLeverage: 10,
		EffectiveStopPrice: 92, LastLeaderSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)
	if _, err = cs.CheckpointCopyGuardPositionMarginShadowV2(cycle.ID, CopyGuardShadowMarkCheckpoint{
		Key: "missing-close-mark", BucketAt: base, MinimumMark: 99, MaximumMark: 101, LastMark: 100,
		ObservationCount: 20, CoveredSeconds: 60, ObservedAt: base.Add(time.Minute), Source: "LIVE_MARK",
	}, 92); err != nil {
		t.Fatal(err)
	}
	if err = cs.FinalizeCopyGuardPositionMarginShadow(cycle.ID, 0); err != nil {
		t.Fatal(err)
	}
	shadow, err := cs.GetCopyGuardPositionMarginShadowV2(cycle.ID)
	if err != nil || shadow.Status != CopyGuardPositionMarginShadowFinalized {
		t.Fatalf("missing-close shadow did not finalize: shadow=%+v err=%v", shadow, err)
	}
	evaluations, err := cs.ListCopyGuardShadowEvaluations(cycle.ID)
	if err != nil || len(evaluations) != 1 || evaluations[0].Status != CopyGuardShadowUnscorable ||
		!strings.Contains(evaluations[0].Reason, "close price is unavailable") {
		t.Fatalf("missing-close shadow was not excluded: evaluations=%+v err=%v", evaluations, err)
	}
}
