package copytrade

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"nofx/decision"
	"nofx/logger"
	"nofx/store"
)

func (ti *TraderIntegration) protectionCycleForDecision(dec *decision.Decision) (*store.CopyGuardCycle, error) {
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if !errors.Is(err, sql.ErrNoRows) {
		return cycle, err
	}
	return ti.store.CopyTrade().GetFollowGroupProtectionCycle(ti.traderID, dec.LeaderPosID, dec.MarginMode)
}

func (ti *TraderIntegration) recordProtectionFollowup(dec *decision.Decision, code string, cause error) {
	if ti == nil || ti.store == nil || dec == nil {
		return
	}
	key := dec.LeaderPosID
	if errors.Is(cause, errCopyPositionEnded) {
		_ = ti.store.CopyTrade().ResolveRuntimeIssue(ti.traderID, "protection", key)
		return
	}
	detail := "protection follow-up pending"
	if cause != nil {
		detail = cause.Error()
	}
	side := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(dec.Action, "open_"), "reduce_"), "close_")
	changed, err := ti.store.CopyTrade().RecordRuntimeIssue(store.CopyRuntimeIssue{TraderID: ti.traderID, Area: "protection", ResourceID: key, LeaderPosID: dec.LeaderPosID, Symbol: dec.Symbol, Side: side, Code: code, Detail: detail})
	if err != nil {
		logger.Warnf("[%s] persist protection follow-up: %v", ti.traderID, err)
	} else if changed {
		logger.Warnf("[%s] protection follow-up %s %s: %s", ti.traderID, dec.Symbol, code, detail)
	}
}

// Reconciliation proves business completion from per-order committed fills.
// Protection recovery is independent and is never allowed to adopt a later
// manual position or roll the mapping back to this old intent's revision.
func (ti *TraderIntegration) recoverAcknowledgedProtection(intent *store.CopyTradeExecutionIntent) {
	if intent == nil || ti.engine == nil {
		return
	}
	m, err := ti.store.CopyTrade().GetMappingForReconciliation(ti.traderID, intent.LeaderPosID)
	if err != nil || m == nil || m.SourceRevision != intent.SourceRevision {
		return
	}
	dec := &decision.Decision{IsCopyTrade: true, Symbol: m.Symbol, Action: intent.Action, LeaderPosID: m.LeaderPosID, MarginMode: m.MarginMode, ExecutionIntentID: intent.ID, SourceRevision: intent.SourceRevision, ExecutionStatus: store.ExecutionIntentFilled, Reasoning: "Copy Guard protection retry"}
	if strings.HasPrefix(intent.Action, "close_") {
		if m.Status == store.MappingStatusClosed {
			// Protection/accounting retries may never reopen this completed
			// business intent. The monitor independently retries this hook.
			dec.ExecutionIntentID = 0
			dec.ExchangeOrderID = intent.ExchangeOrderID
			if _, err = ti.finalizeCopyGuardCycleState(dec, false); err != nil {
				ti.recordProtectionFollowup(dec, "COMPLETED_EXIT_PROTECTION_PENDING", err)
			} else {
				_ = ti.store.CopyTrade().ResolveRuntimeIssue(ti.traderID, "protection", dec.LeaderPosID)
			}
		}
		return
	}
	if m.Status != store.MappingStatusActive || !usesV4CopyGuardRisk(ti.engine.config) {
		return
	}
	if _, _, err = ti.ensureV4CycleForMapping(m, nil, "acknowledged business fill protection follow-up"); err != nil {
		ti.recordProtectionFollowup(dec, "COPY_GUARD_CYCLE_RECOVERY_PENDING", err)
		return
	}
	_ = ti.store.CopyTrade().ResolveRuntimeIssue(ti.traderID, "protection", dec.LeaderPosID)
	ti.queueProtectionRefresh(dec)
}

func (ti *TraderIntegration) retryAcknowledgedCloseProtections() {
	intents, err := ti.store.CopyTrade().ListFilledExecutionIntentsWithLifecycleGap(ti.traderID)
	if err != nil {
		return
	}
	for _, intent := range intents {
		if !strings.HasPrefix(intent.Action, "close_") {
			continue
		}
		if resolved, err := ti.store.CopyTrade().ResolveAcknowledgedLeaderIntent(intent.ID); err == nil && resolved {
			ti.recoverAcknowledgedProtection(intent)
		}
	}
}

func (ti *TraderIntegration) finishNoFillSource(dec *decision.Decision, disposition, reason string, cause error) error {
	if dec == nil || dec.ExecutionIntentID <= 0 {
		return fmt.Errorf("source transition identity unavailable")
	}
	facts, err := ti.store.CopyTrade().InspectLeaderTransition(dec.ExecutionIntentID)
	if err != nil {
		return err
	}
	leaderID := ""
	if ti.engine != nil && ti.engine.config != nil {
		leaderID = ti.engine.config.LeaderID
	}
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	err = ti.store.CopyTrade().FinishLeaderSourceTransition(store.FinishLeaderSourceTransitionRequest{IntentID: dec.ExecutionIntentID, TraderID: ti.traderID, LeaderID: leaderID, ExpectedFingerprint: facts.Fingerprint, Disposition: disposition, Reason: reason, Evidence: detail})
	if err != nil {
		return err
	}
	dec.ExecutionStatus, dec.ExecutionReasonCode = store.ExecutionIntentSkipped, reason
	return nil
}

// Never replace an uncertain exchange result with a local skipped state.
func (ti *TraderIntegration) finishGatedSource(dec *decision.Decision, disposition, reason string, cause error) {
	if err := ti.finishNoFillSource(dec, disposition, reason, cause); err != nil {
		ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "SOURCE_FINISH_PENDING", err.Error())
	}
}

func (ti *TraderIntegration) acknowledgeTerminalZeroFill(intent *store.CopyTradeExecutionIntent) {
	if intent == nil || intent.SourceKind != "LEADER_TRANSITION" || !strings.HasPrefix(intent.Action, "open_") {
		return
	}
	dec := &decision.Decision{ExecutionIntentID: intent.ID, LeaderPosID: intent.LeaderPosID, SourceRevision: intent.SourceRevision}
	if err := ti.finishNoFillSource(dec, store.SourceConsumeNoFill, "EXCHANGE_TERMINAL_NO_FILL", nil); err != nil && !errors.Is(err, sql.ErrNoRows) {
		ti.recordExecutionReconciliationFailure(intent, "SOURCE_FINISH_PENDING", err.Error())
	}
}

// Display failure is not evidence of an exchange outcome. Only proven
// unsubmitted/zero-fill risk increases may be consumed; reductions remain due
// until the venue or the dedicated exit reconciler proves their completion.
func (ti *TraderIntegration) finishFailedLeaderSource(dec *decision.Decision, cause error) bool {
	if dec == nil || dec.ExecutionIntentID <= 0 || ti.store == nil {
		return false
	}
	facts, err := ti.store.CopyTrade().InspectLeaderTransition(dec.ExecutionIntentID)
	if err != nil {
		ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "SOURCE_FINISH_PENDING", err.Error())
		return true
	}
	if facts.SourceKind != "LEADER_TRANSITION" {
		return false
	}
	if facts.Effect != store.SourceEffectUnsubmitted && facts.Effect != store.SourceEffectZeroFill {
		ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "EXCHANGE_OUTCOME_PENDING", cause.Error())
		return true
	}
	if !strings.HasPrefix(dec.Action, "open_") || store.IsLeaderSourceRevalidationReason(classifyExecutionFailure(cause)) || isRetryableExecutionError(cause) {
		ti.deferSourceTransitionRevalidation(dec, classifyExecutionFailure(cause), cause)
		return true
	}
	ti.finishGatedSource(dec, store.SourceConsumeNoFill, classifyExecutionFailure(cause), cause)
	return true
}
