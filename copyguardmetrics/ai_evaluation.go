package copyguardmetrics

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"nofx/store"
)

const (
	evaluationHorizonNextDecision = "NEXT_DECISION"
	evaluationHorizonAttemptClose = "ATTEMPT_CLOSE"
	evaluationHorizonTerminal     = "TERMINAL"
)

type AIEffectSummary struct {
	CycleID               int64          `json:"cycle_id"`
	EvaluationVersion     int            `json:"evaluation_version"`
	TotalDecisions        int            `json:"total_decisions"`
	ScorableDecisions     int            `json:"scorable_decisions"`
	UnscorableDecisions   int            `json:"unscorable_decisions"`
	DecisionCounts        map[string]int `json:"decision_counts"`
	DecisionOutcomeCounts map[string]int `json:"decision_outcome_counts"`
	MarketOutcomeCounts   map[string]int `json:"market_outcome_counts"`
	MissedReversals       int            `json:"missed_reversals"`
	CorrectAbandons       int            `json:"correct_abandons"`
	RiskGateSavedLosses   int            `json:"risk_gate_saved_losses"`
	ActualReentryPnL      float64        `json:"actual_reentry_pnl"`
	FinalDecision         string         `json:"final_decision"`
	FinalDecisionOutcome  string         `json:"final_decision_outcome"`
}

type evaluationDatapack struct {
	CopyGuard struct {
		GateATR             float64 `json:"gate_atr_okx"`
		RecommendedNotional float64 `json:"recommended_notional_usdt"`
		Protectable         bool    `json:"new_stop_protectable_precheck"`
		Leader              struct {
			StillHolding bool `json:"still_holding_same_side"`
		} `json:"leader"`
	} `json:"copy_guard"`
}

type pathResult struct {
	marketOutcome   string
	mfeATR          float64
	maeATR          float64
	firstReversalAt *time.Time
	sampleCount     int
	coverage        float64
	maxGapSeconds   float64
	reason          string
}

// EvaluateCycleAIDecisions materializes deterministic post-decision outcomes
// for one closed Copy Guard cycle. It is safe to call from both the background
// backfiller and the final email path: the unique evaluation key and INSERT
// OR IGNORE make it idempotent across restarts and concurrent callers.
func EvaluateCycleAIDecisions(st *store.Store, cycleID int64) (*AIEffectSummary, error) {
	if st == nil || cycleID <= 0 {
		return nil, fmt.Errorf("invalid AI evaluation context")
	}
	cycle, err := st.CopyTrade().GetCopyGuardCycle(cycleID)
	if err != nil {
		return nil, err
	}
	if cycle.ClosedAt == nil {
		return nil, fmt.Errorf("cycle %d is not closed", cycleID)
	}
	analyses, err := st.ReentryAI().ListReentryAnalysesByCycle(cycleID, 500)
	if err != nil {
		return nil, err
	}
	filtered := analyses[:0]
	for _, analysis := range analyses {
		if analysis != nil && analysis.CandidateID > 0 && analysis.CallStatus == "COMPLETED" && analysis.Verdict != "" {
			filtered = append(filtered, analysis)
		}
	}
	analyses = filtered
	sort.Slice(analyses, func(i, j int) bool { return analyses[i].ID < analyses[j].ID })
	attempts, err := st.CopyTrade().ListCopyGuardAttempts(cycleID)
	if err != nil {
		return nil, err
	}
	events, err := st.CopyTrade().ListCopyGuardEvents(cycleID)
	if err != nil {
		return nil, err
	}
	samples, err := st.CopyTrade().ListCopyGuardWatchSamples(cycleID)
	if err != nil {
		return nil, err
	}
	traderName := st.Trader().ResolveDisplayName(cycle.TraderID)
	expectedInterval := 60 * time.Second
	if trader, traderErr := st.Trader().GetByID(cycle.TraderID); traderErr == nil && trader != nil && trader.ScanIntervalMinutes > 1 {
		expectedInterval = time.Duration(trader.ScanIntervalMinutes) * time.Minute
	}

	insertedTerminal := false
	for i, analysis := range analyses {
		end := *cycle.ClosedAt
		horizon := evaluationHorizonTerminal
		var next *store.ReentryAIAnalysis
		if i+1 < len(analyses) && analyses[i+1].CandidateID == analysis.CandidateID {
			next = analyses[i+1]
			end = next.SnapshotAt
			horizon = evaluationHorizonNextDecision
		}
		attempt, requested := requestedAttempt(events, attempts, analysis)
		actualExecuted := requested && attempt != nil && attempt.ClosedAt != nil && attempt.Reconciled
		if actualExecuted {
			end = *attempt.ClosedAt
			horizon = evaluationHorizonAttemptClose
		}
		if end.Before(analysis.SnapshotAt) {
			end = analysis.SnapshotAt
		}
		pack := evaluationDatapack{}
		_ = json.Unmarshal([]byte(analysis.DatapackJSON), &pack)
		atr := pack.CopyGuard.GateATR
		if atr <= 0 {
			atr = cycle.ATRAtStop
		}
		actionability := "NOT_ACTIONABLE"
		if pack.CopyGuard.Protectable && pack.CopyGuard.RecommendedNotional > 0 && pack.CopyGuard.Leader.StillHolding {
			actionability = "ACTIONABLE_SNAPSHOT"
		}
		preflightRejected := hasEventForAnalysis(events, "REENTRY_PREFLIGHT_REJECTED", analysis.ID)
		if preflightRejected {
			actionability = "PREFLIGHT_REJECTED"
		}
		path := evaluatePath(samples, analysis.AttemptNo-1, cycle.Side, analysis.SnapshotPrice, atr, analysis.SnapshotAt, end, expectedInterval)
		decisionOutcome := classifyDecisionOutcome(analysis, next, path.marketOutcome, actionability, actualExecuted, attempt, hasLaterExecutedAttempt(events, attempts, analysis))
		var actualPnL *float64
		if actualExecuted {
			value := attempt.PnL
			actualPnL = &value
		}
		evaluation := &store.ReentryAIDecisionEvaluation{
			AnalysisID: analysis.ID, CandidateID: analysis.CandidateID, TraderID: analysis.TraderID,
			TraderNameSnapshot: traderName, CycleID: analysis.CycleID, AttemptNo: analysis.AttemptNo,
			DecisionGeneration: analysis.DecisionGeneration, Decision: analysis.Verdict, Horizon: horizon,
			EvaluationVersion: store.ReentryDecisionEvaluationVersion, EvaluationStatus: "FINAL",
			MarketOutcome: path.marketOutcome, DecisionOutcome: decisionOutcome, Actionability: actionability,
			Reason: path.reason, ReferencePrice: analysis.SnapshotPrice, ReferenceATR: atr,
			MFEATR: path.mfeATR, MAEATR: path.maeATR, FirstReversalAt: path.firstReversalAt,
			WindowStartAt: analysis.SnapshotAt, WindowEndAt: end, SampleCount: path.sampleCount,
			CoverageRatio: path.coverage, MaxGapSeconds: path.maxGapSeconds,
			ActualExecuted: actualExecuted, ActualPnL: actualPnL, EvaluationLatency: end.Sub(analysis.SnapshotAt).Seconds(),
		}
		saved, inserted, saveErr := st.ReentryAI().SaveReentryDecisionEvaluation(evaluation)
		if saveErr != nil {
			return nil, saveErr
		}
		if inserted {
			if i == len(analyses)-1 {
				insertedTerminal = true
			}
			_ = st.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
				CycleID: cycleID, TraderID: cycle.TraderID, Type: "AI_DECISION_OUTCOME_FINALIZED",
				Price: analysis.SnapshotPrice, PnL: valueOrZero(actualPnL),
				Metadata: map[string]interface{}{
					"evaluation_id": saved.ID, "evaluation_version": saved.EvaluationVersion,
					"analysis_id": analysis.ID, "candidate_id": analysis.CandidateID,
					"attempt_no": analysis.AttemptNo, "decision_generation": analysis.DecisionGeneration,
					"decision": analysis.Verdict, "market_outcome": saved.MarketOutcome,
					"decision_outcome": saved.DecisionOutcome, "actionability": saved.Actionability,
					"mfe_atr": saved.MFEATR, "mae_atr": saved.MAEATR,
					"coverage_ratio": saved.CoverageRatio, "trader_name_snapshot": traderName,
				},
			})
		}
	}

	evaluations, err := st.ReentryAI().ListReentryDecisionEvaluationsByCycle(cycleID)
	if err != nil {
		return nil, err
	}
	summary := summarizeCycle(cycle, attempts, evaluations)
	if len(evaluations) > 0 && insertedTerminal && !hasEventType(events, "AI_CANDIDATE_OUTCOME_FINALIZED") {
		_ = st.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
			CycleID: cycleID, TraderID: cycle.TraderID, Type: "AI_CANDIDATE_OUTCOME_FINALIZED",
			PnL: summary.ActualReentryPnL,
			Metadata: map[string]interface{}{
				"evaluation_version": summary.EvaluationVersion, "total_decisions": summary.TotalDecisions,
				"scorable_decisions": summary.ScorableDecisions, "unscorable_decisions": summary.UnscorableDecisions,
				"missed_reversals": summary.MissedReversals, "correct_abandons": summary.CorrectAbandons,
				"risk_gate_saved_losses": summary.RiskGateSavedLosses, "actual_reentry_pnl": summary.ActualReentryPnL,
				"final_decision": summary.FinalDecision, "final_decision_outcome": summary.FinalDecisionOutcome,
				"trader_name_snapshot": traderName,
			},
		})
	}
	return summary, nil
}

func evaluatePath(samples []*store.CopyGuardWatchSample, watchAttempt int, side string, referencePrice, atr float64, start, end time.Time, expectedInterval time.Duration) pathResult {
	result := pathResult{marketOutcome: store.ReentryMarketInsufficient, reason: "insufficient point-in-time watch data"}
	if referencePrice <= 0 || atr <= 0 || !end.After(start) {
		return result
	}
	var selected []*store.CopyGuardWatchSample
	for _, sample := range samples {
		if sample == nil || sample.AttemptNo != watchAttempt || sample.MarkPrice <= 0 || sample.CreatedAt.Before(start) || sample.CreatedAt.After(end) {
			continue
		}
		selected = append(selected, sample)
	}
	result.sampleCount = len(selected)
	duration := end.Sub(start)
	if expectedInterval < time.Minute {
		expectedInterval = time.Minute
	}
	expected := math.Max(1, math.Ceil(duration.Seconds()/expectedInterval.Seconds()))
	result.coverage = math.Min(1, float64(len(selected))/expected)
	if len(selected) < 3 {
		return result
	}
	maxGap := selected[0].CreatedAt.Sub(start)
	for i := 1; i < len(selected); i++ {
		if gap := selected[i].CreatedAt.Sub(selected[i-1].CreatedAt); gap > maxGap {
			maxGap = gap
		}
	}
	if gap := end.Sub(selected[len(selected)-1].CreatedAt); gap > maxGap {
		maxGap = gap
	}
	result.maxGapSeconds = math.Max(0, maxGap.Seconds())
	allowedGap := math.Max((10 * time.Minute).Seconds(), 3*expectedInterval.Seconds())
	if result.coverage < 0.8 || result.maxGapSeconds > allowedGap {
		result.reason = fmt.Sprintf("watch data incomplete: coverage=%.2f max_gap=%.0fs", result.coverage, result.maxGapSeconds)
		return result
	}
	long := strings.EqualFold(side, "long")
	type bucketClose struct {
		bucket int64
		at     time.Time
		diff   float64
	}
	var buckets []bucketClose
	var adverseAt, oneATRAt *time.Time
	for _, sample := range selected {
		diff := sample.MarkPrice - referencePrice
		if !long {
			diff = -diff
		}
		ratio := diff / atr
		if ratio > result.mfeATR {
			result.mfeATR = ratio
		}
		if -ratio > result.maeATR {
			result.maeATR = -ratio
		}
		if ratio >= 1 && oneATRAt == nil {
			t := sample.CreatedAt
			oneATRAt = &t
		}
		if ratio <= -1 && adverseAt == nil {
			t := sample.CreatedAt
			adverseAt = &t
		}
		bucket := sample.CreatedAt.UTC().Unix() / 300
		if len(buckets) == 0 || buckets[len(buckets)-1].bucket != bucket {
			buckets = append(buckets, bucketClose{bucket: bucket, at: sample.CreatedAt, diff: ratio})
		} else {
			buckets[len(buckets)-1] = bucketClose{bucket: bucket, at: sample.CreatedAt, diff: ratio}
		}
	}
	var twoBucketAt *time.Time
	for i := 1; i < len(buckets); i++ {
		if buckets[i-1].diff >= 0.5 && buckets[i].diff >= 0.5 && buckets[i].bucket == buckets[i-1].bucket+1 {
			t := buckets[i].at
			twoBucketAt = &t
			break
		}
	}
	if oneATRAt != nil && twoBucketAt != nil {
		confirmed := *oneATRAt
		if twoBucketAt.After(confirmed) {
			confirmed = *twoBucketAt
		}
		if adverseAt == nil || confirmed.Before(*adverseAt) {
			result.marketOutcome = store.ReentryMarketReversal
			result.firstReversalAt = &confirmed
			result.reason = "direction recovered 1 ATR and held above 0.5 ATR across two consecutive 5m observation buckets before a 1 ATR adverse move"
			return result
		}
	}
	if adverseAt != nil {
		result.marketOutcome = store.ReentryMarketAgainst
		result.reason = "price moved 1 ATR against the original direction before any confirmed recovery"
		return result
	}
	result.marketOutcome = store.ReentryMarketChop
	result.reason = "neither a confirmed 1 ATR recovery nor a 1 ATR adverse continuation occurred in the decision window"
	return result
}

func classifyDecisionOutcome(analysis, next *store.ReentryAIAnalysis, marketOutcome, actionability string, actualExecuted bool, attempt *store.CopyGuardAttempt, laterAttempt *store.CopyGuardAttempt) string {
	if actualExecuted && attempt != nil {
		switch {
		case attempt.PnL > 0:
			return "ENTER_CAPTURED_PROFIT"
		case attempt.PnL < 0:
			return "ENTER_EXECUTED_LOSS"
		default:
			return "ENTER_EXECUTED_FLAT"
		}
	}
	if marketOutcome == store.ReentryMarketInsufficient {
		return "UNSCORABLE"
	}
	switch analysis.Verdict {
	case store.ReentryVerdictEnter:
		if actionability == "PREFLIGHT_REJECTED" {
			switch marketOutcome {
			case store.ReentryMarketReversal:
				return "RISK_GATE_MISSED_REVERSAL"
			case store.ReentryMarketAgainst:
				return "RISK_GATE_SAVED_LOSS"
			default:
				return "RISK_GATE_INCONCLUSIVE"
			}
		}
		if marketOutcome == store.ReentryMarketReversal {
			return "ENTER_NOT_EXECUTED_MISSED"
		}
		return "ENTER_NOT_EXECUTED"
	case store.ReentryVerdictWait:
		if laterAttempt != nil && laterAttempt.ClosedAt != nil && laterAttempt.Reconciled {
			if laterAttempt.PnL > 0 {
				return "WAIT_LATER_CAPTURED"
			}
			return "WAIT_LATER_ENTERED_LOSS"
		}
		if marketOutcome == store.ReentryMarketReversal {
			if actionability == "ACTIONABLE_SNAPSHOT" {
				return "WAIT_DELAYED_REVERSAL"
			}
			return "REVERSAL_NOT_ACTIONABLE"
		}
		return "WAIT_APPROPRIATE"
	case store.ReentryVerdictAbandon:
		if next != nil {
			return "ABANDON_PROVISIONAL"
		}
		if marketOutcome == store.ReentryMarketReversal {
			if actionability == "ACTIONABLE_SNAPSHOT" {
				return "ABANDON_MISSED_REVERSAL"
			}
			return "REVERSAL_NOT_ACTIONABLE"
		}
		if marketOutcome == store.ReentryMarketAgainst {
			return "ABANDON_CORRECT"
		}
		return "ABANDON_INCONCLUSIVE"
	default:
		return "UNSCORABLE"
	}
}

func requestedAttempt(events []*store.CopyGuardEvent, attempts []*store.CopyGuardAttempt, analysis *store.ReentryAIAnalysis) (*store.CopyGuardAttempt, bool) {
	if !hasEventForAnalysis(events, "REENTRY_REQUESTED", analysis.ID) {
		return nil, false
	}
	for _, attempt := range attempts {
		if attempt != nil && attempt.AttemptNo == analysis.AttemptNo {
			return attempt, true
		}
	}
	return nil, true
}

func hasLaterExecutedAttempt(events []*store.CopyGuardEvent, attempts []*store.CopyGuardAttempt, analysis *store.ReentryAIAnalysis) *store.CopyGuardAttempt {
	for _, event := range events {
		if event == nil || event.Type != "REENTRY_REQUESTED" || event.CreatedAt.Before(analysis.SnapshotAt) {
			continue
		}
		attemptNo, ok := metadataInt(event.Metadata, "attempt_no")
		if !ok || attemptNo != analysis.AttemptNo {
			continue
		}
		for _, attempt := range attempts {
			if attempt != nil && attempt.AttemptNo == attemptNo {
				return attempt
			}
		}
	}
	return nil
}

func hasEventForAnalysis(events []*store.CopyGuardEvent, eventType string, analysisID int64) bool {
	for _, event := range events {
		if event == nil || event.Type != eventType {
			continue
		}
		if id, ok := metadataInt64(event.Metadata, "analysis_id"); ok && id == analysisID {
			return true
		}
	}
	return false
}

func metadataInt64(metadata map[string]interface{}, key string) (int64, bool) {
	if metadata == nil {
		return 0, false
	}
	switch value := metadata[key].(type) {
	case float64:
		return int64(value), true
	case int:
		return int64(value), true
	case int64:
		return value, true
	case json.Number:
		v, err := value.Int64()
		return v, err == nil
	default:
		return 0, false
	}
}

func metadataInt(metadata map[string]interface{}, key string) (int, bool) {
	v, ok := metadataInt64(metadata, key)
	return int(v), ok
}

func hasEventType(events []*store.CopyGuardEvent, eventType string) bool {
	for _, event := range events {
		if event != nil && event.Type == eventType {
			return true
		}
	}
	return false
}

func summarizeCycle(cycle *store.CopyGuardCycle, attempts []*store.CopyGuardAttempt, evaluations []*store.ReentryAIDecisionEvaluation) *AIEffectSummary {
	summary := &AIEffectSummary{
		CycleID: cycle.ID, EvaluationVersion: store.ReentryDecisionEvaluationVersion,
		DecisionCounts: map[string]int{}, DecisionOutcomeCounts: map[string]int{}, MarketOutcomeCounts: map[string]int{},
	}
	for _, attempt := range attempts {
		if attempt != nil && attempt.AttemptNo > 0 && attempt.Reconciled {
			summary.ActualReentryPnL += attempt.PnL
		}
	}
	for _, evaluation := range evaluations {
		if evaluation == nil || evaluation.EvaluationVersion != store.ReentryDecisionEvaluationVersion {
			continue
		}
		summary.TotalDecisions++
		summary.DecisionCounts[evaluation.Decision]++
		summary.DecisionOutcomeCounts[evaluation.DecisionOutcome]++
		summary.MarketOutcomeCounts[evaluation.MarketOutcome]++
		if evaluation.DecisionOutcome == "UNSCORABLE" {
			summary.UnscorableDecisions++
		} else {
			summary.ScorableDecisions++
		}
		switch evaluation.DecisionOutcome {
		case "ABANDON_MISSED_REVERSAL", "WAIT_DELAYED_REVERSAL", "RISK_GATE_MISSED_REVERSAL", "ENTER_NOT_EXECUTED_MISSED":
			summary.MissedReversals++
		case "ABANDON_CORRECT":
			summary.CorrectAbandons++
		case "RISK_GATE_SAVED_LOSS":
			summary.RiskGateSavedLosses++
		}
		summary.FinalDecision = evaluation.Decision
		summary.FinalDecisionOutcome = evaluation.DecisionOutcome
	}
	return summary
}

// SummarizeCycleAIEffects reuses the same candidate/cycle aggregation in API
// documents and CSV exports without re-running evaluation or duplicating label
// semantics outside this package.
func SummarizeCycleAIEffects(cycle *store.CopyGuardCycle, attempts []*store.CopyGuardAttempt, evaluations []*store.ReentryAIDecisionEvaluation) *AIEffectSummary {
	if cycle == nil {
		return &AIEffectSummary{EvaluationVersion: store.ReentryDecisionEvaluationVersion, DecisionCounts: map[string]int{}, DecisionOutcomeCounts: map[string]int{}, MarketOutcomeCounts: map[string]int{}}
	}
	return summarizeCycle(cycle, attempts, evaluations)
}

func valueOrZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
