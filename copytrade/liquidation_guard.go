package copytrade

import (
	"fmt"
	"math"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

func liquidationOnly(cfg *CopyConfig) bool {
	return cfg != nil && !cfg.RiskStopLossEnabled && cfg.RiskLiquidationGuardEnabled
}

// The liquidation-only planner deliberately uses the same clamp and order
// lifecycle as a strategy stop. Missing venue facts never imply a crossing.
func (ti *TraderIntegration) refreshLiquidationOnly(dec *decision.Decision, cycle *store.CopyGuardCycle, pos map[string]interface{}, manualPrice float64) {
	side := SideType(cycle.Side)
	entry, qty := getFloatField(pos, "entryPrice", "entry_price"), math.Abs(getFloatField(pos, "positionAmt", "quantity"))
	liq := getFloatField(pos, "liquidationPrice", "liquidation_price")
	mark := ti.fixedStopMarkObservation(dec.Symbol, getFloatField(pos, "markPrice", "mark_price"))
	pending := func(err error) {
		ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "LIQUIDATION_SAFETY_UNVERIFIED", err, cycle.ProtectionCoverage, false)
	}
	resolver, ok := ti.executor.(trader.ExecutionInstrumentResolver)
	if !ok {
		pending(fmt.Errorf("execution precision unavailable"))
		return
	}
	inst, err := resolver.ResolveExecutionInstrument(dec.Symbol)
	if err != nil || inst == nil {
		pending(fmt.Errorf("execution precision unavailable: %v", err))
		return
	}
	if invalidPositive(liq) || invalidPositive(mark.Price) || (side == SideLong && liq > mark.Price) || (side == SideShort && liq < mark.Price) {
		pending(fmt.Errorf("valid liquidation and mark prices are required"))
		return
	}
	anchor := liq
	if manualPrice > 0 {
		anchor = manualPrice
	}
	existing, _ := ti.store.CopyTrade().GetCopyGuardAttemptFinalStop(cycle.ID, cycle.ReentryCount)
	leverage := getIntOrFloatField(pos, "leverage")
	if leverage <= 0 {
		pending(fmt.Errorf("position leverage unavailable"))
		return
	}
	evaluation, err := EvaluatePositionMarginStop(PositionMarginStopEvaluationInput{
		Side: side, CurrentEntryPrice: entry, CurrentQuantity: qty, CurrentLeverage: float64(leverage),
		AnchorStopPrice: anchor, ExistingStopPrice: existing, MarkPrice: mark.Price, LiquidationPrice: liq,
		PriceTickSize: inst.PriceTickSize, BaseQuantityStep: inst.BaseQuantityStep,
	})
	if err != nil {
		pending(err)
		return
	}
	evaluation.GovernedBy = "liquidation_guard"
	if manualPrice > 0 && !evaluation.Clamped {
		evaluation.GovernedBy = "manual_exchange"
	}
	// Persist the stop before an emergency exit, including the first protection
	// attempt, so restart recovery uses the exact verified mark crossing.
	if err = ti.store.CopyTrade().UpdateCopyGuardAttemptRiskAudit(cycle.ID, cycle.ReentryCount, float64(leverage), evaluation.CurrentMargin, 0, 0, "", 0, evaluation.StopPrice, evaluation.CurrentEffectiveMarginLossPct, evaluation.GovernedBy); err != nil {
		pending(err)
		return
	}
	if evaluation.AlreadyCrossed {
		ti.handlePositionMarginStopCrossed(dec, cycle, cycle.Side, qty, mark.Price, evaluation)
		return
	}
	dec.Leverage = leverage
	ti.upsertV4ProtectionWithFollowerPosition(dec, cycle.Side, qty, entry, &StopLossCalcResult{
		SLPrice: evaluation.StopPrice, SLDistance: math.Abs(entry - evaluation.StopPrice), GovernedBy: evaluation.GovernedBy,
		TickSize: inst.PriceTickSize, QuantityStep: inst.BaseQuantityStep, Clamped: evaluation.Clamped,
		ExpectedLossUSD: evaluation.CurrentRiskUSD, ExpectedMarginLossPct: evaluation.CurrentEffectiveMarginLossPct,
	}, getStringField(pos, "posId", "positionId"), true)
}

// Never let an old close-all stop survive into a new source lifecycle. This
// runs only at the entry boundary; ordinary leader reductions do not wait for
// protection health or retirement.
func (ti *TraderIntegration) retireOldProtectionBeforeEntry(dec *decision.Decision) error {
	if ti.store == nil || dec == nil || (dec.Action != "open_long" && dec.Action != "open_short") {
		return nil
	}
	orders, err := ti.store.CopyTrade().ListActiveCopyGuardProtectiveOrders(ti.traderID)
	if err != nil {
		return err
	}
	for _, order := range orders {
		if order.Symbol != dec.Symbol || "open_"+order.Side != dec.Action {
			continue
		}
		cycle, err := ti.store.CopyTrade().GetCopyGuardCycle(order.CycleID)
		if err != nil {
			return err
		}
		if ti.copyGuardOwnsPosition(cycle) {
			continue
		}
		mgr, ok := ti.executor.(ProtectiveStopManagerV4)
		if !ok {
			return fmt.Errorf("old protective order retirement unavailable")
		}
		ti.protectionMu.Lock()
		err = ti.cancelProtectiveOrderForCycle(mgr, cycle, order)
		ti.protectionMu.Unlock()
		if err != nil {
			return fmt.Errorf("old protective order retirement pending: %w", err)
		}
	}
	return nil
}
