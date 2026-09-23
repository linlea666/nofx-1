package copytrade

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

func accountLeaderExit(dec *decision.Decision) bool {
	return dec != nil && dec.IsCopyTrade && dec.LeaderExitScope == leaderAccountExitScope && dec.ExecutionIntentID > 0
}

func (ti *TraderIntegration) leaderExitPositions(symbol, side string) (map[string]store.LeaderExitTarget, error) {
	positions, err := ti.getFreshPositions()
	if err != nil {
		return nil, err
	}
	result := make(map[string]store.LeaderExitTarget)
	for _, p := range positions {
		if getStringField(p, "symbol") != symbol || !strings.EqualFold(getStringField(p, "side"), side) {
			continue
		}
		qty := math.Abs(getFloatField(p, "positionAmt", "quantity"))
		if qty == 0 {
			continue
		}
		mode := getStringField(p, "mgnMode", "marginMode")
		id := getStringField(p, "posId", "positionId")
		if (mode != "cross" && mode != "isolated") || math.IsNaN(qty) || math.IsInf(qty, 0) {
			return nil, fmt.Errorf("invalid current position scope")
		}
		key := trader.CopyPositionKey(symbol, side, mode, id)
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate exchange position scope")
		}
		result[key] = store.LeaderExitTarget{Key: key, MarginMode: mode, PositionID: id, Quantity: qty}
	}
	return result, nil
}

func (ti *TraderIntegration) prepareLeaderExit(dec *decision.Decision) (*store.LeaderExitPlan, error) {
	if plan, err := ti.store.CopyTrade().GetLeaderExitPlan(dec.ExecutionIntentID); err == nil {
		return plan, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := ti.store.CopyTrade().CheckFollowSubmission(dec.ExecutionIntentID); err != nil {
		return nil, err
	}
	side := "long"
	if strings.HasSuffix(dec.Action, "short") {
		side = "short"
	}
	positions, err := ti.leaderExitPositions(dec.Symbol, side)
	if err != nil {
		return nil, err
	}
	plan := store.LeaderExitPlan{IntentID: dec.ExecutionIntentID, TraderID: ti.traderID, LeaderPosID: dec.LeaderPosID, Symbol: dec.Symbol, Side: side, Ratio: dec.CloseRatio, SourceClosed: dec.CopyTradeAction == "close"}
	if c, cycleErr := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID); cycleErr == nil {
		plan.CycleID = c.ID
	} else if !errors.Is(cycleErr, sql.ErrNoRows) {
		return nil, cycleErr
	}
	if len(positions) == 0 {
		return ti.store.CopyTrade().PrepareLeaderExit(plan)
	}
	resolver, ok := ti.executor.(trader.ExecutionInstrumentResolver)
	if !ok {
		return nil, fmt.Errorf("leader exit quantity resolver unavailable")
	}
	inst, err := resolver.ResolveExecutionInstrument(dec.Symbol)
	if err != nil {
		return nil, err
	}
	for _, target := range positions {
		kind := trader.QuantityPartialReduce
		if plan.Ratio == 1 {
			kind = trader.QuantityFinalClose
		}
		q, err := trader.QuantizeOrderIntent(inst, target.Quantity*plan.Ratio, kind)
		if errors.Is(err, trader.ErrQuantitySubLot) && plan.Ratio < 1 {
			continue
		}
		if err != nil {
			return nil, err
		}
		if plan.Ratio < 1 && q.Quantized >= target.Quantity {
			return nil, fmt.Errorf("partial reduction would flatten position after quantization")
		}
		target.Quantity = q.Quantized
		plan.Targets = append(plan.Targets, target)
	}
	sort.Slice(plan.Targets, func(i, j int) bool { return plan.Targets[i].Key < plan.Targets[j].Key })
	return ti.store.CopyTrade().PrepareLeaderExit(plan)
}

func (ti *TraderIntegration) reconcileLeaderExitAttempt(intentID int64, a *store.CopyTradeExecutionOrderAttempt, symbol string) error {
	if a.SubmittedAt == nil || a.TerminalAt != nil {
		return nil
	}
	lookup, ok := ti.executor.(ClientOrderStatusProvider)
	if !ok {
		return fmt.Errorf("leader exit order lookup unavailable")
	}
	order, err := lookup.GetOrderStatusByClientID(symbol, a.ClientOrderID)
	if err != nil {
		return fmt.Errorf("leader exit acknowledgement pending: %w", err)
	}
	ti.observeExecutionFillTime(intentID, order)
	state := strings.ToUpper(getStringField(order, "status", "state"))
	filled := getFloatField(order, "executedQty", "filled_quantity")
	if !isTerminalExchangeOrderState(state) || (state == "FILLED" && filled <= 0) {
		return fmt.Errorf("leader exit order acknowledgement pending (%s)", state)
	}
	status := store.ExecutionOrderAttemptTerminalNoFill
	if filled > 0 {
		status = store.ExecutionOrderAttemptFilled
	}
	return ti.store.CopyTrade().CompleteExecutionOrderAttempt(intentID, a.ClientOrderID, status, getStringField(order, "orderId", "ordId"), state, "", filled)
}

// runLeaderExitOrders uses a separate execution lock shared with real risk
// exits. It never holds protectionMu or waits for ATR/AI/history to submit.
func (ti *TraderIntegration) runLeaderExitOrders(dec *decision.Decision, plan *store.LeaderExitPlan, submit bool) error {
	ti.exitExecutionMu.Lock()
	defer ti.exitExecutionMu.Unlock()
	closer, ok := ti.executor.(trader.ScopedPositionCloser)
	if !ok {
		return fmt.Errorf("scoped leader exits unsupported")
	}
	cs := ti.store.CopyTrade()
	fullDrain := plan.Ratio == 1
	if batch, batchErr := cs.GetFollowGroupExitBatch(plan.IntentID); batchErr == nil {
		fullDrain = batch.FullGroupExit
	} else if !errors.Is(batchErr, sql.ErrNoRows) {
		return batchErr
	}
	attempts, err := cs.ListExecutionOrderAttempts(plan.IntentID)
	if err != nil {
		return err
	}
	for _, a := range attempts {
		if err = ti.reconcileLeaderExitAttempt(plan.IntentID, a, plan.Symbol); err != nil {
			return err
		}
	}
	if !submit {
		return nil
	}
	if err = cs.ResumeLeaderExitSubmission(plan.IntentID); err != nil {
		return err
	}
	positions, err := ti.leaderExitPositions(plan.Symbol, plan.Side)
	if err != nil {
		return err
	}
	// A full exit also drains newly visible scopes from in-flight entries. A
	// partial exit never expands its frozen target set after preparation.
	if fullDrain {
		known := make(map[string]bool)
		for _, t := range plan.Targets {
			known[t.Key] = true
		}
		changed := false
		for key, t := range positions {
			if !known[key] {
				plan.Targets = append(plan.Targets, t)
				changed = true
			}
		}
		if changed {
			if err = cs.ExtendFullLeaderExit(*plan); err != nil {
				return err
			}
		}
	}
	attempts, err = cs.ListExecutionOrderAttempts(plan.IntentID)
	if err != nil {
		return err
	}
	for _, target := range plan.Targets {
		live, exists := positions[target.Key]
		if !exists {
			continue
		}
		kind := "LEADER_EXIT:" + strings.ToUpper(stableClientOrderID(ti.traderID, target.Key, "scope"))
		filled := 0.0
		sequence := 1
		clientID := ""
		for _, a := range attempts {
			if a.QuantityKind == kind {
				filled += a.FilledQuantity
				sequence++
				if a.SubmittedAt == nil && a.TerminalAt == nil {
					clientID = a.ClientOrderID
				}
			}
		}
		quantity := math.Min(live.Quantity, math.Max(0, target.Quantity-filled))
		if fullDrain {
			quantity = live.Quantity
		}
		if quantity <= 1e-12 {
			continue
		}
		if clientID == "" {
			clientID = stableClientOrderID(ti.traderID, fmt.Sprintf("%d|%s|%d", plan.IntentID, target.Key, sequence), "leader_exit")
		}
		if _, err = cs.PrepareExecutionOrderAttemptRecordWithKind(plan.IntentID, clientID, kind, quantity, quantity); err != nil {
			return err
		}
		submitted := false
		order, submitErr := closer.CloseScopedPosition(trader.ScopedPositionCloseRequest{ExecutionIntentID: plan.IntentID, Symbol: plan.Symbol, Side: plan.Side, MarginMode: target.MarginMode, PositionID: target.PositionID, Quantity: quantity, ClientOrderID: clientID, BeforeSubmit: func() error {
			// Pause/resume is checked at the actual HTTP boundary. Risk custody
			// release is intentionally not a veto for a leader reduction.
			if !ti.lifecycleRunning() {
				return fmt.Errorf("trader execution generation is no longer running")
			}
			if err := cs.CheckFollowSubmission(plan.IntentID); err != nil {
				return err
			}
			if _, err := cs.MarkExecutionOrderAttemptSubmitted(plan.IntentID, clientID); err != nil {
				return err
			}
			submitted = true
			return nil
		}})
		ti.observeExecutionFillTime(plan.IntentID, order)
		state := strings.ToUpper(getStringField(order, "status", "state"))
		fill := getFloatField(order, "executedQty", "filled_quantity")
		orderID := getStringField(order, "orderId", "ordId")
		attemptState := store.ExecutionOrderAttemptSubmitted
		message := ""
		if submitErr != nil {
			message = submitErr.Error()
			attemptState = store.ExecutionOrderAttemptUnknown
			if !submitted {
				attemptState = store.ExecutionOrderAttemptTerminalNoFill
			}
		} else if isTerminalExchangeOrderState(state) && (state != "FILLED" || fill > 0) {
			attemptState = store.ExecutionOrderAttemptTerminalNoFill
			if fill > 0 {
				attemptState = store.ExecutionOrderAttemptFilled
			}
		}
		if err = cs.CompleteExecutionOrderAttempt(plan.IntentID, clientID, attemptState, orderID, state, message, fill); err != nil {
			return err
		}
		dec.ExchangeOrderID = orderID
		if submitErr != nil {
			return submitErr
		}
		latest, err := cs.ListExecutionOrderAttempts(plan.IntentID)
		if err != nil {
			return err
		}
		for _, a := range latest {
			if a.ClientOrderID == clientID {
				if err = ti.reconcileLeaderExitAttempt(plan.IntentID, a, plan.Symbol); err != nil {
					return err
				}
			}
		}
	}
	positions, err = ti.leaderExitPositions(plan.Symbol, plan.Side)
	if err != nil {
		return err
	}
	if fullDrain {
		pending, err := cs.HasUnsettledScopeEntries(ti.traderID, plan.Symbol, plan.Side)
		if err != nil {
			return err
		}
		if pending {
			return fmt.Errorf("leader exit waiting for in-flight entry reconciliation")
		}
	}
	if fullDrain && len(positions) > 0 {
		return fmt.Errorf("leader full exit residual remains")
	}
	attempts, err = cs.ListExecutionOrderAttempts(plan.IntentID)
	if err != nil {
		return err
	}
	for _, target := range plan.Targets {
		if _, exists := positions[target.Key]; !exists {
			continue
		}
		kind := "LEADER_EXIT:" + strings.ToUpper(stableClientOrderID(ti.traderID, target.Key, "scope"))
		filled := 0.0
		for _, a := range attempts {
			if a.QuantityKind == kind {
				if a.SubmittedAt != nil && a.TerminalAt == nil {
					return fmt.Errorf("leader exit order remains unsettled")
				}
				filled += a.FilledQuantity
			}
		}
		if !fullDrain && filled+math.Max(1e-12, target.Quantity*1e-8) < target.Quantity {
			return fmt.Errorf("partial leader exit residual remains")
		}
	}
	return nil
}

func (ti *TraderIntegration) executeAccountLeaderExit(dec *decision.Decision, submit bool) error {
	if existing, err := ti.store.CopyTrade().GetLeaderExitPlan(dec.ExecutionIntentID); err == nil && existing.Completed {
		if submit {
			return ti.finishLeaderExit(dec, existing)
		}
		return nil
	}
	if errors.Is(ti.store.CopyTrade().CheckFollowSubmission(dec.ExecutionIntentID), store.ErrFollowControlChanged) {
		attempts, err := ti.store.CopyTrade().ListExecutionOrderAttempts(dec.ExecutionIntentID)
		if err != nil {
			return err
		}
		for _, a := range attempts {
			if err = ti.reconcileLeaderExitAttempt(dec.ExecutionIntentID, a, dec.Symbol); err != nil {
				return err
			}
		}
		return ti.store.CopyTrade().CancelLeaderExitAfterControlChange(dec.ExecutionIntentID)
	}
	plan, err := ti.prepareLeaderExit(dec)
	if err != nil {
		return err
	}

	if submit {
		if err = ti.supersedeOlderOrdinaryCatchup(dec); err != nil {
			return err
		}
	}
	if err = ti.runLeaderExitOrders(dec, plan, submit); err != nil {
		return err
	}
	if !submit {
		return nil
	}
	// Mark source/intent complete before slow accounting. Old protective orders
	// remain separately visible and must be retired before a new source entry.
	if err = ti.store.CopyTrade().CompleteLeaderExit(plan.IntentID); err != nil {
		return err
	}
	dec.ExecutionStatus = store.ExecutionIntentFilled
	dec.ExchangeFillConfirmed = true
	return ti.finishLeaderExit(dec, plan)
}

func (ti *TraderIntegration) recordAccountLeaderExit(dec *decision.Decision) (store.DecisionAction, string) {
	start := time.Now()
	err := ti.executeAccountLeaderExit(dec, true)
	action := store.DecisionAction{Action: dec.Action, Symbol: dec.Symbol, Price: dec.EntryPrice, Timestamp: start, Reasoning: dec.Reasoning, Success: err == nil}
	if err != nil {
		action.Error = err.Error()
		ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "LEADER_EXIT_PENDING", err.Error())
		ti.saveSignalLog(dec, "reconciling", err.Error())
		return action, fmt.Sprintf("⏳ %s %s 同向总仓退出待对账: %v", dec.Action, dec.Symbol, err)
	}
	if intent, readErr := ti.store.CopyTrade().GetExecutionIntentByID(dec.ExecutionIntentID); readErr == nil {
		dec.ExecutionStatus = intent.Status
		dec.FilledQuantity = intent.FilledQuantity
		if intent.Status == store.ExecutionIntentSkipped {
			ti.saveSignalLog(dec, "skipped", intent.ReasonCode)
			return action, fmt.Sprintf("⏸ %s %s 已取消：%s", dec.Action, dec.Symbol, intent.ReasonCode)
		}
	}
	ti.saveSignalLog(dec, "executed", "")
	return action, fmt.Sprintf("✅ %s %s 同向总仓退出确认 (%dms)", dec.Action, dec.Symbol, time.Since(start).Milliseconds())
}

func (ti *TraderIntegration) finishLeaderExit(dec *decision.Decision, plan *store.LeaderExitPlan) error {
	if plan.Cancelled {
		return nil
	}
	// Exchange/source completion is final. A failed protective cleanup cannot
	// turn it back into a business order waiting to execute.
	ti.transitionExecutionIntent(dec, store.ExecutionIntentFilled, "LEADER_EXIT_CONFIRMED", "")
	// Protective callbacks carry no business intent authority.
	protectionDec := *dec
	protectionDec.ExecutionIntentID = 0
	if err := ti.finishLeaderExitProtection(&protectionDec, plan); err != nil {
		_, _ = ti.store.CopyTrade().RecordRuntimeIssue(store.CopyRuntimeIssue{TraderID: ti.traderID, Area: "protection", ResourceID: fmt.Sprintf("leader_exit:%d", plan.IntentID), LeaderPosID: dec.LeaderPosID, Symbol: dec.Symbol, Code: "EXIT_PROTECTION_PENDING", Detail: err.Error()})
	} else {
		_ = ti.store.CopyTrade().ResolveRuntimeIssue(ti.traderID, "protection", fmt.Sprintf("leader_exit:%d", plan.IntentID))
	}
	return nil
}

func (ti *TraderIntegration) finishLeaderExitProtection(dec *decision.Decision, plan *store.LeaderExitPlan) error {
	if plan.Cancelled {
		return nil
	}
	if batch, err := ti.store.CopyTrade().GetFollowGroupExitBatch(plan.IntentID); err == nil {
		if !batch.FullGroupExit {
			return ti.queueFollowGroupProtections(batch.GroupID)
		}
		cycles, err := ti.store.CopyTrade().FollowGroupGuardCycles(batch.GroupID)
		if err != nil {
			return err
		}
		for _, c := range cycles {
			if c.ClosedAt != nil {
				continue
			}
			copyDec := *dec
			copyDec.LeaderPosID = c.LeaderPosID
			copyDec.MarginMode = c.MarginMode
			copyDec.Symbol = c.Symbol
			if _, err = ti.finalizeCopyGuardCycleState(&copyDec, false); err != nil {
				return err
			}
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if plan.SourceClosed && plan.CycleID > 0 {
		c, err := ti.store.CopyTrade().GetCopyGuardCycle(plan.CycleID)
		if err != nil {
			return err
		}
		if c.ClosedAt == nil {
			if current, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID); err != nil || current.ID != plan.CycleID {
				return fmt.Errorf("original exit cycle changed")
			}
			if _, err = ti.finalizeCopyGuardCycleState(dec, false); err != nil {
				return err
			}
		}
	} else if !plan.SourceClosed {
		ti.queueProtectionRefresh(dec)
	}
	return nil
}

func oppositePositionSideOnReversal(side string, reversed bool) string {
	if !reversed {
		return ""
	}
	if side == "long" {
		return "short"
	}
	return "long"
}
