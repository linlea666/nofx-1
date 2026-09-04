package copyguardmetrics

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"nofx/store"
)

type ShadowPolicyGate struct {
	Policy                  string   `json:"policy"`
	IndependentCycles       int      `json:"independent_cycles"`
	EnterSamples            int      `json:"enter_samples"`
	MeanIncrementalEffect   float64  `json:"mean_incremental_effect"`
	MedianIncrementalEffect float64  `json:"median_incremental_effect"`
	BootstrapCI95Low        float64  `json:"bootstrap_ci95_low"`
	BootstrapCI95High       float64  `json:"bootstrap_ci95_high"`
	UnprotectedFilledCount  int      `json:"unprotected_filled_count"`
	EligibleForManualEnable bool     `json:"eligible_for_manual_enable"`
	BlockingReasons         []string `json:"blocking_reasons"`
	VerifiedMarkCoverage    float64  `json:"verified_mark_coverage"`
	VerifiedCrossings       int      `json:"verified_crossings"`
	TailLossCVaR95USD       float64  `json:"tail_loss_cvar_95_usd"`
	PostStopReversalRate    float64  `json:"post_stop_reversal_rate"`
	AverageSlippageBPS      float64  `json:"average_slippage_bps"`
	MinimumLeverage         float64  `json:"minimum_leverage"`
	MaximumLeverage         float64  `json:"maximum_leverage"`
	MinimumNotional         float64  `json:"minimum_notional"`
	MaximumNotional         float64  `json:"maximum_notional"`
}

type ShadowPromotionReport struct {
	Status                   string             `json:"status"`
	MinimumIndependentCycles int                `json:"minimum_independent_cycles"`
	MinimumEnterSamples      int                `json:"minimum_enter_samples"`
	RequiresPositiveMedian   bool               `json:"requires_positive_median"`
	RequiresNonNegativeCI95  bool               `json:"requires_non_negative_ci95"`
	RequiresZeroUnprotected  bool               `json:"requires_zero_unprotected"`
	RequiresMarkCoverage     float64            `json:"requires_mark_coverage"`
	Policies                 []ShadowPolicyGate `json:"policies"`
}

func shadowUnscorable(cycle *store.CopyGuardCycle, policy, reason string) *store.CopyGuardShadowEvaluation {
	return &store.CopyGuardShadowEvaluation{
		CycleID: cycle.ID, TraderID: cycle.TraderID, Policy: policy,
		EvaluationVersion: store.CopyGuardShadowEvaluationVersion,
		Status:            store.CopyGuardShadowUnscorable, DataQuality: store.CopyGuardShadowQualityUnscorable,
		Reason: reason,
	}
}

func observedCycleCosts(cycle *store.CopyGuardCycle) float64 {
	if cycle == nil {
		return 0
	}
	return math.Abs(cycle.Fees) + math.Abs(cycle.FundingFee) +
		math.Abs(cycle.LiquidationPenalty) + math.Abs(cycle.Slippage)
}

func currentCycleNetPnL(st *store.Store, cycle *store.CopyGuardCycle) (gross, cost, net float64, reason string, ok bool) {
	if st == nil || cycle == nil {
		return 0, 0, 0, "cycle or store is unavailable", false
	}
	var exchangeType string
	err := st.DB().QueryRow(`SELECT LOWER(COALESCE(e.exchange_type,e.id,''))
		FROM traders t LEFT JOIN exchanges e ON e.id=t.exchange_id
		WHERE t.id=?`, cycle.TraderID).Scan(&exchangeType)
	if err != nil {
		return 0, 0, 0, "follower exchange PnL basis is unavailable", false
	}
	switch exchangeType {
	case "okx":
		// OKX positions-history realizedPnl already includes fees. Funding and
		// liquidation adjustments belong to that authoritative realized value
		// as well, while the separate fields are retained for attribution.
		net = cycle.ActualPnL
		cost = observedCycleCosts(cycle)
		gross = net + cost
		return gross, cost, net, "OKX authoritative realizedPnL already includes exchange costs; separate costs are attribution only", true
	case "binance":
		// Binance user-trade realizedPnl excludes commission. Funding is signed
		// (credit or debit); penalty/slippage are stored as positive costs.
		cost = math.Abs(cycle.Fees) + math.Abs(cycle.LiquidationPenalty) + math.Abs(cycle.Slippage)
		gross = cycle.ActualPnL
		net = gross - cost + cycle.FundingFee
		return gross, cost, net, "Binance realizedPnl is gross of commission; observed signed funding and costs were applied", true
	default:
		return 0, 0, 0, "unsupported or unknown follower exchange PnL basis: " + exchangeType, false
	}
}

func directionalShadowPnL(side string, notional, entry, exit float64) (float64, bool) {
	if notional <= 0 || entry <= 0 || exit <= 0 {
		return 0, false
	}
	move := (exit - entry) / entry
	if strings.EqualFold(side, "short") {
		move = -move
	} else if !strings.EqualFold(side, "long") {
		return 0, false
	}
	return notional * move, true
}

// EvaluateCycleShadowPolicies evaluates report-only alternatives. It never
// changes policy, reserves risk, creates an order, or makes a model call.
func EvaluateCycleShadowPolicies(st *store.Store, cycleID int64) ([]*store.CopyGuardShadowEvaluation, error) {
	if st == nil || cycleID <= 0 {
		return nil, fmt.Errorf("invalid shadow evaluation context")
	}
	cycle, err := st.CopyTrade().GetCopyGuardCycle(cycleID)
	if err != nil {
		return nil, err
	}
	if cycle.ClosedAt == nil {
		return []*store.CopyGuardShadowEvaluation{}, nil
	}
	attempts, err := st.CopyTrade().ListCopyGuardAttempts(cycleID)
	if err != nil {
		return nil, err
	}
	samples, err := st.CopyTrade().ListCopyGuardWatchSamples(cycleID)
	if err != nil {
		return nil, err
	}
	var initial *store.CopyGuardAttempt
	for _, attempt := range attempts {
		if attempt != nil && attempt.AttemptNo == 0 {
			initial = attempt
			break
		}
	}
	results := []*store.CopyGuardShadowEvaluation{
		shadowUnscorable(cycle, store.CopyGuardShadowCurrentStop, "cycle accounting is not reconciled"),
		shadowUnscorable(cycle, store.CopyGuardShadowWideStopEqualRisk, "initial stop path is unavailable"),
		shadowUnscorable(cycle, store.CopyGuardShadowStagedReduction, "initial stop and no-stop baseline are unavailable"),
		shadowUnscorable(cycle, store.CopyGuardShadowProbeReentry25Pct, "post-stop path is unavailable"),
	}
	costs := observedCycleCosts(cycle)
	currentNet, currentNetAvailable := float64(0), false
	if cycle.AccountingStatus == store.CopyGuardAccountingReconciled {
		gross, attributedCost, net, basisReason, basisOK := currentCycleNetPnL(st, cycle)
		if basisOK {
			currentNet, currentNetAvailable = net, true
			results[0] = &store.CopyGuardShadowEvaluation{
				CycleID: cycle.ID, TraderID: cycle.TraderID,
				Policy:            store.CopyGuardShadowCurrentStop,
				EvaluationVersion: store.CopyGuardShadowEvaluationVersion,
				Status:            store.CopyGuardShadowScorable, DataQuality: store.CopyGuardShadowQualityVerified,
				GrossPnL: gross, EstimatedCost: attributedCost, NetPnL: net,
				SizeFactor: 1, EntryPrice: cycle.FollowerEntryPrice,
				ExitPrice: cycle.LastObservedPrice,
				Reason:    basisReason,
			}
		} else {
			results[0].Reason = basisReason
		}
	}
	baselineUsable := cycle.AccountingStatus == store.CopyGuardAccountingReconciled &&
		cycle.BaselineSource != "missing"
	if initial != nil && initial.Reconciled && baselineUsable {
		stagedGross := 0.5*initial.PnL + 0.5*cycle.BaselinePnL
		results[2] = &store.CopyGuardShadowEvaluation{
			CycleID: cycle.ID, TraderID: cycle.TraderID,
			Policy:            store.CopyGuardShadowStagedReduction,
			EvaluationVersion: store.CopyGuardShadowEvaluationVersion,
			Status:            store.CopyGuardShadowScorable, DataQuality: store.CopyGuardShadowQualityEstimated,
			GrossPnL: stagedGross, EstimatedCost: costs, NetPnL: stagedGross - costs,
			SizeFactor: 0.5, EntryPrice: initial.EntryPrice,
			ExitPrice: cycle.LastObservedPrice,
			Reason:    "50% exits on the observed stop path and 50% follows the reconciled no-stop baseline; observed cycle costs deducted conservatively",
		}
	}

	if initial != nil && initial.Reconciled && baselineUsable && len(samples) > 0 {
		entry := initial.EntryPrice
		if entry <= 0 {
			entry = cycle.FollowerEntryPrice
		}
		notional := initial.Notional
		if notional <= 0 {
			notional = cycle.BaselineNotional
		}
		atr := initial.ATR
		if atr <= 0 {
			atr = cycle.ATRAtStop
		}
		stop := initial.StopFillPrice
		if stop <= 0 {
			stop = initial.ExitPrice
		}
		if stop <= 0 {
			stop = initial.StopTriggerPrice
		}
		if entry > 0 && notional > 0 && atr > 0 && stop > 0 {
			wideStop := stop - atr
			if strings.EqualFold(cycle.Side, "short") {
				wideStop = stop + atr
			}
			exit := cycle.LastObservedPrice
			wideTriggered := false
			for _, sample := range samples {
				if sample == nil || sample.MarkPrice <= 0 {
					continue
				}
				if (strings.EqualFold(cycle.Side, "long") && sample.MarkPrice <= wideStop) ||
					(strings.EqualFold(cycle.Side, "short") && sample.MarkPrice >= wideStop) {
					exit, wideTriggered = sample.MarkPrice, true
					break
				}
			}
			gross := 0.5 * cycle.BaselinePnL
			if wideTriggered {
				if computed, ok := directionalShadowPnL(cycle.Side, notional*0.5, entry, exit); ok {
					gross = computed
				} else {
					exit = 0
				}
			}
			if exit > 0 {
				estimatedCost := costs * 0.5
				reason := "double-width stop was not crossed; equal-risk half size follows the reconciled leader-close baseline"
				if wideTriggered {
					reason = "double-width stop crossed in the recorded post-stop path; equal-risk half-size outcome uses the first observed crossing"
				}
				results[1] = &store.CopyGuardShadowEvaluation{
					CycleID: cycle.ID, TraderID: cycle.TraderID,
					Policy:            store.CopyGuardShadowWideStopEqualRisk,
					EvaluationVersion: store.CopyGuardShadowEvaluationVersion,
					Status:            store.CopyGuardShadowScorable, DataQuality: store.CopyGuardShadowQualityEstimated,
					GrossPnL: gross, EstimatedCost: estimatedCost, NetPnL: gross - estimatedCost,
					SizeFactor: 0.5, EntryPrice: entry, ExitPrice: exit, Reason: reason,
				}
			}
		}
	}

	if baselineUsable && len(samples) > 0 {
		var trigger *store.CopyGuardWatchSample
		for _, sample := range samples {
			if sample != nil && sample.MarkPrice > 0 && sample.Gate == "REENTRY_TRIGGERED" {
				trigger = sample
				break
			}
		}
		if trigger == nil {
			results[3] = &store.CopyGuardShadowEvaluation{
				CycleID: cycle.ID, TraderID: cycle.TraderID,
				Policy:            store.CopyGuardShadowProbeReentry25Pct,
				EvaluationVersion: store.CopyGuardShadowEvaluationVersion,
				Status:            store.CopyGuardShadowNoSignal, DataQuality: store.CopyGuardShadowQualityEstimated,
				Reason: "recorded path contained no deterministic reentry trigger; no probe trade simulated",
			}
		} else {
			exit := cycle.LastObservedPrice
			notional := cycle.BaselineNotional
			if notional <= 0 && initial != nil {
				notional = initial.Notional
			}
			gross, ok := directionalShadowPnL(cycle.Side, notional*0.25, trigger.MarkPrice, exit)
			if ok {
				// Ten basis points is a conservative round-trip fallback when
				// observed costs are too small to represent another entry/exit.
				estimatedCost := math.Max(costs*0.25, notional*0.25*0.001)
				results[3] = &store.CopyGuardShadowEvaluation{
					CycleID: cycle.ID, TraderID: cycle.TraderID,
					Policy:            store.CopyGuardShadowProbeReentry25Pct,
					EvaluationVersion: store.CopyGuardShadowEvaluationVersion,
					Status:            store.CopyGuardShadowScorable, DataQuality: store.CopyGuardShadowQualityEstimated,
					GrossPnL: gross, EstimatedCost: estimatedCost, NetPnL: gross - estimatedCost,
					SizeFactor: 0.25, EntryPrice: trigger.MarkPrice, ExitPrice: exit,
					Reason: "25% probe enters only on the recorded deterministic recovery trigger and exits at the reconciled leader-close mark; conservative round-trip cost deducted",
				}
			}
		}
	}

	for _, result := range results {
		if err = st.CopyTrade().SaveCopyGuardShadowEvaluation(result); err != nil {
			return nil, err
		}
	}
	if err = st.CopyTrade().ReconcileCopyGuardPositionMarginShadowV2(cycle, currentNet, currentNetAvailable); err != nil {
		return nil, err
	}
	if rows, listErr := st.CopyTrade().ListCopyGuardShadowEvaluations(cycleID); listErr == nil {
		for _, row := range rows {
			if row.Policy == store.CopyGuardShadowFirstEntryPositionMargin80 && row.EvaluationVersion == store.CopyGuardPositionMarginShadowEvaluationVersion {
				results = append(results, row)
				break
			}
		}
	} else {
		return nil, listErr
	}
	return results, nil
}

func shadowBootstrap(values []float64) (low, high float64, available bool) {
	if len(values) < 50 {
		return 0, 0, false
	}
	rng := rand.New(rand.NewSource(7))
	means := make([]float64, 5000)
	for i := range means {
		for range values {
			means[i] += values[rng.Intn(len(values))]
		}
		means[i] /= float64(len(values))
	}
	sort.Float64s(means)
	return means[124], means[4874], true
}

func shadowTailLossCVaR95(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := int(math.Ceil(float64(len(sorted)) * .05))
	if n < 1 {
		n = 1
	}
	total := float64(0)
	for _, value := range sorted[:n] {
		if value < 0 {
			total += -value
		}
	}
	return total / float64(n)
}

func minPositiveMetric(current, candidate float64) float64 {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

func BuildShadowPromotionReport(
	st *store.Store, traderIDs []string, from, to time.Time,
) (*ShadowPromotionReport, error) {
	rows, err := st.CopyTrade().ListCopyGuardShadowEvaluationsForTraders(traderIDs, from, to)
	if err != nil {
		return nil, err
	}
	unprotected, err := st.CopyTrade().CountUnprotectedFilledIntents(traderIDs, from, to)
	if err != nil {
		return nil, err
	}
	report := &ShadowPromotionReport{
		Status: "INSUFFICIENT_DATA", MinimumIndependentCycles: 50,
		MinimumEnterSamples: 10, RequiresPositiveMedian: true,
		RequiresNonNegativeCI95: true, RequiresZeroUnprotected: true,
		RequiresMarkCoverage: .95,
	}
	for _, policy := range []string{
		store.CopyGuardShadowFirstEntryPositionMargin80,
	} {
		var values []float64
		var netValues []float64
		verifiedCrossings, reversedCrossings := 0, 0
		coverageTotal, slippageTotal := float64(0), float64(0)
		cycles := make(map[int64]struct{})
		for _, row := range rows {
			if row == nil || row.Policy != policy || row.EvaluationVersion != store.CopyGuardPositionMarginShadowEvaluationVersion ||
				row.Status != store.CopyGuardShadowScorable || row.DataQuality != store.CopyGuardShadowQualityVerified {
				continue
			}
			cycles[row.CycleID] = struct{}{}
			values = append(values, row.IncrementalEffect)
			netValues = append(netValues, row.NetPnL)
			coverageTotal += row.MarkCoverage
			slippageTotal += row.SlippageBPS
			if row.StopCrossed && row.CrossingVerified {
				verifiedCrossings++
				if row.PostStopReversed {
					reversedCrossings++
				}
			}
		}
		gate := ShadowPolicyGate{
			Policy: policy, IndependentCycles: len(cycles), EnterSamples: verifiedCrossings,
			VerifiedCrossings:      verifiedCrossings,
			UnprotectedFilledCount: unprotected,
		}
		for _, row := range rows {
			if row == nil || row.Policy != policy || row.EvaluationVersion != store.CopyGuardPositionMarginShadowEvaluationVersion ||
				row.Status != store.CopyGuardShadowScorable || row.DataQuality != store.CopyGuardShadowQualityVerified {
				continue
			}
			gate.MinimumLeverage = minPositiveMetric(gate.MinimumLeverage, row.MinimumLeverage)
			gate.MaximumLeverage = math.Max(gate.MaximumLeverage, row.MaximumLeverage)
			gate.MinimumNotional = minPositiveMetric(gate.MinimumNotional, row.MinimumNotional)
			gate.MaximumNotional = math.Max(gate.MaximumNotional, row.MaximumNotional)
		}
		if len(values) > 0 {
			gate.VerifiedMarkCoverage = coverageTotal / float64(len(values))
			gate.AverageSlippageBPS = slippageTotal / float64(len(values))
		}
		if verifiedCrossings > 0 {
			gate.PostStopReversalRate = float64(reversedCrossings) / float64(verifiedCrossings)
		}
		gate.TailLossCVaR95USD = shadowTailLossCVaR95(netValues)
		if len(values) > 0 {
			for _, value := range values {
				gate.MeanIncrementalEffect += value
			}
			gate.MeanIncrementalEffect /= float64(len(values))
			sorted := append([]float64(nil), values...)
			sort.Float64s(sorted)
			middle := len(sorted) / 2
			gate.MedianIncrementalEffect = sorted[middle]
			if len(sorted)%2 == 0 {
				gate.MedianIncrementalEffect = (sorted[middle-1] + sorted[middle]) / 2
			}
		}
		ciAvailable := false
		gate.BootstrapCI95Low, gate.BootstrapCI95High, ciAvailable = shadowBootstrap(values)
		if gate.IndependentCycles < 50 {
			gate.BlockingReasons = append(gate.BlockingReasons, "NEED_50_INDEPENDENT_CLOSED_CYCLES")
		}
		if gate.EnterSamples < 10 {
			gate.BlockingReasons = append(gate.BlockingReasons, "NEED_10_VERIFIED_STOP_CROSSINGS")
		}
		if gate.MeanIncrementalEffect <= 0 {
			gate.BlockingReasons = append(gate.BlockingReasons, "NET_MEAN_NOT_POSITIVE_AFTER_COSTS")
		}
		if gate.MedianIncrementalEffect <= 0 {
			gate.BlockingReasons = append(gate.BlockingReasons, "MEDIAN_CYCLE_NOT_POSITIVE")
		}
		if !ciAvailable || gate.BootstrapCI95Low < 0 {
			gate.BlockingReasons = append(gate.BlockingReasons, "BOOTSTRAP_CI95_LOWER_BOUND_NEGATIVE_OR_UNAVAILABLE")
		}
		if unprotected > 0 {
			gate.BlockingReasons = append(gate.BlockingReasons, "UNPROTECTED_FILLED_EXECUTIONS_PRESENT")
		}
		if gate.VerifiedMarkCoverage < .95 {
			gate.BlockingReasons = append(gate.BlockingReasons, "VERIFIED_MARK_COVERAGE_BELOW_95_PERCENT")
		}
		gate.EligibleForManualEnable = len(gate.BlockingReasons) == 0
		report.Policies = append(report.Policies, gate)
	}
	for _, policy := range report.Policies {
		if policy.EligibleForManualEnable {
			report.Status = "MANUAL_ENABLE_ELIGIBLE"
			break
		}
	}
	return report, nil
}
