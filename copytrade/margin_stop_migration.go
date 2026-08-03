package copytrade

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"nofx/decision"
	"nofx/logger"
	"nofx/store"
)

const migrationMarginStopRevisionBase int64 = 8_000_000_000_000_000

var migrationMarginStopRetrySchedule = [...]time.Duration{
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
	120 * time.Second,
	300 * time.Second,
}

// reconcileStartupPositionMarginStops applies the migrated position-margin
// cap to fresh follower positions before risk increases are allowed. It never
// needs leader-source HTTP and therefore cannot compete with Binance source
// polling. true means at least one position or exchange result is still
// unknown and open/add must remain frozen.
func (ti *TraderIntegration) reconcileStartupPositionMarginStops() (bool, error) {
	if ti == nil || ti.store == nil || ti.engine == nil || ti.engine.config == nil {
		return false, nil
	}
	cfg := ti.engine.config
	if !SupportsCopyGuard(cfg.ProviderType) || cfg.RiskPolicyVersion < 4 ||
		!cfg.RiskStopLossEnabled || !cfg.RiskLeverageFallback || cfg.RiskLeverageMaxLoss <= 0 ||
		cfg.RiskMarginStopMigrationVersion <= 0 {
		return false, nil
	}
	mappings, err := ti.store.CopyTrade().ListActiveMappings(ti.traderID)
	if err != nil {
		return true, fmt.Errorf("list active mappings for migrated margin stop: %w", err)
	}
	if len(mappings) == 0 {
		pending, pendingErr := ti.store.CopyTrade().HasUnfinishedMigrationMarginStopIntents(ti.traderID)
		return pending, pendingErr
	}
	positions, err := ti.getFreshPositions()
	if err != nil {
		ti.recordMigrationMarginStopReconciliation("fresh follower positions unavailable", err, nil, nil, 0)
		return true, nil
	}

	pending := false
	for _, mapping := range mappings {
		position := migratedFollowerPosition(positions, mapping.Symbol, mapping.Side, mapping.MarginMode)
		if position == nil {
			// Absence without an existing migration intent is not automatically a
			// margin stop; the normal authoritative leader/stop reconciliation
			// decides whether this was a leader close or a protective fill.
			continue
		}
		if getFloatField(position, "markPrice", "mark_price", "lastPrice", "price") <= 0 {
			if priceProvider, ok := ti.executor.(StopLossManager); ok {
				if price, priceErr := priceProvider.GetMarketPrice(mapping.Symbol); priceErr == nil && price > 0 {
					cloned := make(map[string]interface{}, len(position)+1)
					for key, value := range position {
						cloned[key] = value
					}
					cloned["markPrice"] = price
					position = cloned
				}
			}
		}
		ratio, quantity, notional, calcErr := migratedPositionLossRatio(cfg, position)
		if calcErr != nil {
			ti.recordMigrationMarginStopReconciliation("position loss ratio unavailable", calcErr, mapping, nil, 0)
			pending = true
			continue
		}
		if ratio+1e-12 < cfg.RiskLeverageMaxLoss {
			continue
		}
		cycle, _, cycleErr := ti.ensureV4CycleForMapping(mapping, positions, "position margin stop migration")
		if cycleErr != nil {
			ti.recordMigrationMarginStopReconciliation("copy guard cycle unavailable", cycleErr, mapping, nil, ratio)
			pending = true
			continue
		}
		if cycle == nil {
			pending = true
			continue
		}
		// This path exists to stop pre-v4 positions from running naked after the
		// upgrade — not to enforce a margin percentage on positions Copy Guard is
		// already managing. Since v8 the stop distance is chosen by ATR, so a
		// protected position at 50x legitimately sits well past RiskLeverageMaxLoss
		// on its way to a 2 ATR stop. Force-closing it here would reinstate exactly
		// the fixed-margin exit the v8 rework removed, and only on the restarts
		// where this reconciler happens to be running.
		if cycleFullyProtected(cycle) || cycleProtectionVerdictReached(cycle) {
			continue
		}
		unfinished, unfinishedErr := ti.hasUnfinishedMigrationIntent(cycle.ID)
		if unfinishedErr != nil {
			return true, unfinishedErr
		}
		if unfinished {
			pending = true
			continue
		}
		intent, reserveErr := ti.reserveMigrationMarginStopIntent(mapping, cycle, quantity, notional, ratio)
		if reserveErr != nil {
			ti.recordMigrationMarginStopReconciliation("persist migration exit intent", reserveErr, mapping, cycle, ratio)
			pending = true
			continue
		}
		if intent != nil && (intent.Status == store.ExecutionIntentReserved || intent.Status == store.ExecutionIntentReconciling) {
			ti.reconcileMigrationMarginStopIntent(intent)
		}
		pending = true
	}
	unfinished, unfinishedErr := ti.store.CopyTrade().HasUnfinishedMigrationMarginStopIntents(ti.traderID)
	if unfinishedErr != nil {
		return true, unfinishedErr
	}
	return pending || unfinished, nil
}

func migratedFollowerPosition(positions []map[string]interface{}, symbol, side, marginMode string) map[string]interface{} {
	for _, position := range positions {
		if !strings.EqualFold(getStringField(position, "symbol"), symbol) ||
			!strings.EqualFold(getStringField(position, "side"), side) ||
			absFloat(getFloatField(position, "positionAmt", "quantity")) <= 0 {
			continue
		}
		mode := getStringField(position, "marginMode", "mgnMode")
		if marginMode != "" && mode != "" && !strings.EqualFold(mode, marginMode) {
			continue
		}
		return position
	}
	return nil
}

func migratedPositionLossRatio(cfg *CopyConfig, position map[string]interface{}) (ratio, quantity, notional float64, err error) {
	if cfg == nil || position == nil {
		return 0, 0, 0, fmt.Errorf("position loss input unavailable")
	}
	entry := getFloatField(position, "entryPrice", "entry_price", "avgPrice")
	mark := getFloatField(position, "markPrice", "mark_price", "lastPrice", "price")
	quantity = absFloat(getFloatField(position, "positionAmt", "quantity"))
	leverage := getIntOrFloatField(position, "leverage")
	if entry <= 0 || mark <= 0 || quantity <= 0 || leverage <= 0 {
		return 0, quantity, 0, fmt.Errorf("fresh entry/mark/quantity/leverage is incomplete")
	}
	notional = entry * quantity
	adverseMove := float64(0)
	if strings.EqualFold(getStringField(position, "side"), "short") {
		adverseMove = math.Max(mark-entry, 0)
	} else {
		adverseMove = math.Max(entry-mark, 0)
	}
	frictionRate := math.Max((cfg.RiskSlippageBufferBPS+cfg.RiskRoundTripFeeBPS)/10000, 0)
	estimatedTotalLoss := adverseMove*quantity + notional*frictionRate
	initialMargin := notional / float64(leverage)
	if initialMargin <= 0 {
		return 0, quantity, notional, fmt.Errorf("initial margin is unavailable")
	}
	return estimatedTotalLoss / initialMargin, quantity, notional, nil
}

func (ti *TraderIntegration) hasUnfinishedMigrationIntent(cycleID int64) (bool, error) {
	intents, err := ti.store.CopyTrade().ListExecutionIntentsByCycle(cycleID)
	if err != nil {
		return false, err
	}
	var latestRetryableTerminal *store.CopyTradeExecutionIntent
	migrationIntentCount := 0
	for _, intent := range intents {
		if intent == nil || intent.SourceKind != store.ExecutionIntentSourceMigrationMarginStop {
			continue
		}
		migrationIntentCount++
		switch intent.Status {
		case store.ExecutionIntentReserved, store.ExecutionIntentSubmitted, store.ExecutionIntentReconciling, store.ExecutionIntentPartiallyFilled:
			return true, nil
		case store.ExecutionIntentFailed:
			if intent.ReasonCode == "MIGRATION_EXIT_REJECTED" || intent.ReasonCode == "MIGRATION_EXIT_TERMINAL_NO_FILL" {
				latestRetryableTerminal = intent
			}
		}
	}
	// A deterministic exchange rejection or terminal no-fill remains retryable,
	// but the three-second reconciliation monitor must not hammer the venue. A
	// completed partial is deliberately excluded: fresh residual size should be
	// reduced again immediately.
	if latestRetryableTerminal != nil {
		backoff := migrationMarginStopRetryBackoff(migrationIntentCount)
		if time.Since(latestRetryableTerminal.UpdatedAt) < backoff {
			return true, nil
		}
	}
	return false, nil
}

func migrationMarginStopRetryBackoff(attemptCount int) time.Duration {
	if attemptCount <= 0 {
		attemptCount = 1
	}
	index := attemptCount - 1
	if index >= len(migrationMarginStopRetrySchedule) {
		index = len(migrationMarginStopRetrySchedule) - 1
	}
	return migrationMarginStopRetrySchedule[index]
}

func (ti *TraderIntegration) reserveMigrationMarginStopIntent(mapping *store.CopyTradePositionMapping, cycle *store.CopyGuardCycle, quantity, notional, lossRatio float64) (*store.CopyTradeExecutionIntent, error) {
	sequence, err := ti.store.CopyTrade().CountMigrationMarginStopIntents(cycle.ID)
	if err != nil {
		return nil, err
	}
	sequence++
	revision := migrationMarginStopRevisionBase + cycle.ID*1_000_000 + int64(sequence)
	action := "close_long"
	if strings.EqualFold(mapping.Side, "short") {
		action = "close_short"
	}
	canonical := fmt.Sprintf("migration_margin_stop|%s|%d|%d", ti.traderID, cycle.ID, sequence)
	clientID := fmt.Sprintf("cgm%dr%d", cycle.ID, sequence)
	intent, claimed, err := ti.store.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: ti.traderID, LeaderPosID: mapping.LeaderPosID, SourceRevision: revision,
		SourceKind: store.ExecutionIntentSourceMigrationMarginStop, CanonicalKey: canonical,
		CycleID: cycle.ID, AttemptNo: cycle.ReentryCount, Action: action,
		Symbol: mapping.Symbol, Side: mapping.Side, MarginMode: mapping.MarginMode,
		LeaderTargetSize: mapping.LastKnownSize, RequestedNotional: notional,
		RequestedQuantity: quantity, TargetQuantity: quantity, ClientOrderID: clientID,
	})
	if err != nil {
		return nil, err
	}
	if !claimed {
		return intent, nil
	}
	if err = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
		CycleID: cycle.ID, TraderID: ti.traderID, Type: "MIGRATION_MARGIN_STOP_EXIT",
		Quantity: quantity, Notional: notional,
		Metadata: map[string]interface{}{
			"intent_id": intent.ID, "loss_ratio": lossRatio,
			"threshold": ti.engine.config.RiskLeverageMaxLoss,
			"source":    "position_margin_stop_migration",
		},
	}); err != nil {
		_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentReconciling,
			"MIGRATION_RECONCILING", err.Error(), "", quantity, 0, 0)
		return nil, err
	}
	return intent, nil
}

func (ti *TraderIntegration) reconcileMigrationMarginStopIntent(intent *store.CopyTradeExecutionIntent) {
	if intent == nil || intent.SourceKind != store.ExecutionIntentSourceMigrationMarginStop {
		return
	}
	positions, err := ti.getFreshPositions()
	if err != nil {
		_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentReconciling,
			"MIGRATION_RECONCILING", err.Error(), intent.ExchangeOrderID, intent.RequestedQuantity, 0, intent.FilledQuantity)
		return
	}
	position := migratedFollowerPosition(positions, intent.Symbol, intent.Side, intent.MarginMode)
	if position == nil {
		if err = ti.finalizeMigrationMarginStopIntent(intent); err != nil {
			_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentReconciling,
				"MIGRATION_LOCAL_COMMIT_PENDING", err.Error(), intent.ExchangeOrderID, intent.RequestedQuantity, 0, intent.FilledQuantity)
		}
		return
	}

	if intent.Status != store.ExecutionIntentReserved {
		provider, ok := ti.executor.(ClientOrderStatusProvider)
		if !ok || intent.ClientOrderID == "" {
			_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentReconciling,
				"MIGRATION_ORDER_LOOKUP_UNAVAILABLE", "migration close order cannot be reconciled by client id", intent.ExchangeOrderID, intent.RequestedQuantity, 0, intent.FilledQuantity)
			return
		}
		order, actualClientID, lookupErr := ti.lookupExecutionIntentOrder(provider, intent)
		if lookupErr != nil {
			_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentReconciling,
				"MIGRATION_ORDER_LOOKUP_FAILED", lookupErr.Error(), intent.ExchangeOrderID, intent.RequestedQuantity, 0, intent.FilledQuantity)
			return
		}
		state := strings.ToUpper(getStringField(order, "status", "state"))
		filled := getFloatField(order, "executedQty", "filled_quantity", "filledQty", "accFillSz")
		orderID := getStringField(order, "orderId", "ordId", "exchange_order_id")
		_ = ti.store.CopyTrade().UpdateExecutionIntentExchangeState(intent.ID, state)
		if !isTerminalExchangeOrderState(state) {
			_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentReconciling,
				"MIGRATION_EXIT_SUBMITTED", state, orderID, intent.RequestedQuantity, 0, filled)
			return
		}
		attemptStatus := store.ExecutionOrderAttemptTerminalNoFill
		intentStatus := store.ExecutionIntentFailed
		reason := "MIGRATION_EXIT_TERMINAL_NO_FILL"
		if filled > 0 {
			attemptStatus = store.ExecutionOrderAttemptPartiallyFilled
			intentStatus = store.ExecutionIntentCompletedPartial
			reason = "MIGRATION_EXIT_PARTIAL_RETRY_REQUIRED"
		}
		_ = ti.store.CopyTrade().CompleteExecutionOrderAttempt(intent.ID, actualClientID, attemptStatus, orderID, state, reason, filled)
		_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, intentStatus, reason, reason, orderID, intent.RequestedQuantity, 0, filled)
		return
	}

	ti.executionMu.Lock()
	defer ti.executionMu.Unlock()
	dec := &decision.Decision{
		Symbol: intent.Symbol, Action: intent.Action, IsCopyTrade: true,
		CopyTradeAction: "migration_margin_stop", LeaderPosID: intent.LeaderPosID,
		MarginMode: intent.MarginMode, SourceRevision: intent.SourceRevision,
		ExecutionIntentID: intent.ID, ClientOrderID: intent.ClientOrderID,
		RequestedQuantity: intent.RequestedQuantity,
		Reasoning:         "Copy Guard migrated position margin stop reduce-only exit",
	}
	ti.bindExecutionAttemptRecorder(dec)
	execErr := ti.executeDecisionWithRetry(dec)
	if execErr != nil {
		status := store.ExecutionIntentReconciling
		reason := "MIGRATION_EXIT_SUBMISSION_UNKNOWN"
		if !isRetryableExecutionError(execErr) {
			status = store.ExecutionIntentFailed
			reason = "MIGRATION_EXIT_REJECTED"
		}
		ti.transitionExecutionIntent(dec, status, reason, execErr.Error())
		return
	}
	// ACK is provisional. A fresh flat position is the only immediate success
	// signal; otherwise the durable client order is reconciled on the next pass.
	positions, err = ti.getFreshPositions()
	if err == nil && migratedFollowerPosition(positions, intent.Symbol, intent.Side, intent.MarginMode) == nil {
		intent.ExchangeOrderID = dec.ExchangeOrderID
		intent.ExchangeState = dec.ExchangeOrderState
		intent.FilledQuantity = math.Max(dec.FilledQuantity, intent.RequestedQuantity)
		if err = ti.finalizeMigrationMarginStopIntent(intent); err != nil {
			ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "MIGRATION_LOCAL_COMMIT_PENDING", err.Error())
		}
		return
	}
	ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "MIGRATION_EXIT_SUBMITTED", "reduce-only exit submitted; awaiting fresh flat position")
}

func (ti *TraderIntegration) finalizeMigrationMarginStopIntent(intent *store.CopyTradeExecutionIntent) error {
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, intent.LeaderPosID)
	if err != nil {
		return err
	}
	exitPrice := cycle.FollowerEntryPrice
	if priceProvider, ok := ti.executor.(StopLossManager); ok {
		if price, priceErr := priceProvider.GetMarketPrice(intent.Symbol); priceErr == nil && price > 0 {
			exitPrice = price
		}
	}
	metadata := map[string]interface{}{
		"confirmation": "migration_reduce_only_flat", "intent_id": intent.ID,
		"actual_order_id": intent.ExchangeOrderID, "quantity": intent.RequestedQuantity,
		"migration_version": ti.engine.config.RiskMarginStopMigrationVersion,
	}
	if err = ti.store.CopyTrade().RecordCopyGuardStopObserved(cycle.ID, ti.traderID, cycle.ReentryCount, 0, exitPrice, intent.RequestedQuantity, metadata); err != nil {
		return err
	}
	mapping, mapErr := ti.store.CopyTrade().GetMapping(ti.traderID, intent.LeaderPosID)
	if mapErr != nil && !errors.Is(mapErr, sql.ErrNoRows) {
		return mapErr
	}
	leaderSize, addCount := float64(0), 0
	if mapping != nil {
		leaderSize, addCount = mapping.LastKnownSize, mapping.AddCount
	}
	if err = ti.store.CopyTrade().MarkStoppedByRisk(ti.traderID, intent.LeaderPosID, 0, leaderSize, addCount); err != nil {
		return err
	}
	_ = ti.store.CopyTrade().SnapshotCopyGuardLeaderEntryAtStop(cycle.ID, cycle.LeaderEntryPrice)
	attempts, attemptsErr := ti.store.CopyTrade().ListExecutionOrderAttempts(intent.ID)
	if attemptsErr == nil && len(attempts) > 0 {
		latest := attempts[len(attempts)-1]
		_ = ti.store.CopyTrade().CompleteExecutionOrderAttempt(intent.ID, latest.ClientOrderID,
			store.ExecutionOrderAttemptFilled, intent.ExchangeOrderID, "FILLED", "", math.Max(intent.FilledQuantity, intent.RequestedQuantity))
	}
	if err = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentFilled,
		"MIGRATION_MARGIN_STOP_EXIT_CONFIRMED", "", intent.ExchangeOrderID,
		intent.RequestedQuantity, 0, math.Max(intent.FilledQuantity, intent.RequestedQuantity)); err != nil {
		return err
	}
	ti.cancelV4Protection(&decision.Decision{Symbol: intent.Symbol, Action: intent.Action, LeaderPosID: intent.LeaderPosID, MarginMode: intent.MarginMode})
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
		CycleID: cycle.ID, TraderID: ti.traderID, Type: "MIGRATION_MARGIN_STOP_EXIT_CONFIRMED",
		Price: exitPrice, Quantity: intent.RequestedQuantity,
		Metadata: metadata,
	})
	logger.Warnf("🛑 [%s] 历史仓位 50%% 止损迁移退出已确认 | cycle=%d %s %s", ti.traderID, cycle.ID, cycle.Symbol, cycle.Side)
	return nil
}

func (ti *TraderIntegration) recordMigrationMarginStopReconciliation(summary string, cause error, mapping *store.CopyTradePositionMapping, cycle *store.CopyGuardCycle, lossRatio float64) {
	if ti == nil || ti.store == nil {
		return
	}
	detail := map[string]interface{}{"loss_ratio": lossRatio}
	if cause != nil {
		detail["error"] = cause.Error()
	}
	event := &store.CopyTradeEvent{
		TraderID: ti.traderID, Category: store.CopyEventCategoryReconcile,
		EventType: "MIGRATION_RECONCILING", Severity: store.CopyEventSeverityWarn,
		Status: "reconciling", Summary: summary, Detail: detail,
		DedupKey: fmt.Sprintf("migration_reconciling|%s|%s|%d", ti.traderID, summary, time.Now().Unix()/300),
	}
	if ti.engine != nil && ti.engine.config != nil {
		event.LeaderID = ti.engine.config.LeaderID
		event.ProviderType = string(ti.engine.config.ProviderType)
	}
	if mapping != nil {
		event.Symbol, event.Side, event.MarginMode, event.LeaderPosID = mapping.Symbol, mapping.Side, mapping.MarginMode, mapping.LeaderPosID
	}
	if cycle != nil {
		event.CycleID = cycle.ID
	}
	_ = ti.store.CopyTrade().LogCopyEvent(event)
}

func (ti *TraderIntegration) monitorMigrationMarginStopReconciliation() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ti.ctx.Done():
			return
		case <-ticker.C:
			if !ti.lifecycleRunning() {
				return
			}
			pending, err := ti.reconcileStartupPositionMarginStops()
			if err != nil {
				logger.Warnf("⚠️ [%s] 历史仓位止损迁移对账失败: %v", ti.traderID, err)
				continue
			}
			if pending {
				continue
			}
			ti.engine.setRiskIncreaseBlock("")
			_ = ti.store.CopyTrade().LogCopyEvent(&store.CopyTradeEvent{
				TraderID: ti.traderID, LeaderID: ti.engine.config.LeaderID,
				ProviderType: string(ti.engine.config.ProviderType),
				Category:     store.CopyEventCategoryReconcile, EventType: "MIGRATION_RECONCILED",
				Severity: store.CopyEventSeverityInfo, Status: "success",
				Summary:  "历史仓位止损迁移已完成，风险增加已恢复",
				DedupKey: fmt.Sprintf("migration_reconciled|%s|v%d", ti.traderID, ti.engine.config.RiskMarginStopMigrationVersion),
			})
			return
		}
	}
}
