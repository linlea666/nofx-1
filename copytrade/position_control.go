package copytrade

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"nofx/decision"
	"nofx/logger"
	"nofx/store"
	"nofx/trader"
)

var errCopyPositionEnded = errors.New("immutable fills prove the original position ended")

// Protect independent manual positions before a new copy can merge into the
// same venue position, and verify lifetime before mutating an existing copy.
// This does not inspect protection health and never waits on protectionMu.
func (ti *TraderIntegration) preflightCopyPositionOwnership(dec *decision.Decision) error {
	if dec == nil || !dec.IsCopyTrade || isAIReentryDecision(dec) || dec.LeaderPosID == "" {
		return nil
	}
	if capability, ok := ti.executor.(interface{ SupportsPositionContinuity() bool }); ok && !capability.SupportsPositionContinuity() {
		return nil // Preserve non-Copy-Guard exchanges' existing execution path.
	}
	if _, ok := ti.executor.(trader.SymbolTradeHistoryProvider); !ok {
		return nil
	}
	m, err := ti.store.CopyTrade().GetMapping(ti.traderID, dec.LeaderPosID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return reasonError("CUSTODY_CHECK_PENDING", "read position ownership: %w", err)
	}
	if m != nil && m.Status == store.MappingStatusActive {
		p, readErr := ti.store.CopyTrade().GetPositionCustody(ti.traderID, dec.LeaderPosID)
		if readErr == nil && p.State == "RELEASED" {
			return reasonError("POSITION_CUSTODY_RELEASED", "原跟单仓已结束，当前新仓由用户管理")
		}
		if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
			return reasonError("CUSTODY_CHECK_PENDING", "read position custody: %w", readErr)
		}
		if c, e := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID); e == nil {
			if e = ti.verifyCopyGuardContinuity(c); e != nil {
				if errors.Is(e, errCopyPositionEnded) || !ti.copyGuardOwnsPosition(c) {
					return reasonError("POSITION_CUSTODY_RELEASED", "原跟单仓已结束，忽略旧周期指令")
				}
				return reasonError("CUSTODY_CHECK_PENDING", "原仓成交归属待核实: %w", e)
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return reasonError("CUSTODY_CHECK_PENDING", "read position cycle: %w", e)
		} else if p != nil {
			if _, e = ti.verifyFollowingPosition(m, p); e != nil {
				if errors.Is(e, errCopyPositionEnded) {
					return reasonError("POSITION_CUSTODY_RELEASED", "原跟单仓已结束")
				}
				return reasonError("CUSTODY_CHECK_PENDING", "原仓成交归属待核实: %w", e)
			}
		}
		return nil
	}
	if dec.Action != "open_long" && dec.Action != "open_short" {
		return nil
	}
	positions, err := ti.getFreshPositions()
	if err != nil {
		return reasonError("CUSTODY_CHECK_PENDING", "read independent positions: %w", err)
	}
	side := strings.TrimPrefix(dec.Action, "open_")
	managedPeer, err := ti.store.CopyTrade().FollowGroupHasManagedPeer(ti.traderID, dec.LeaderPosID, dec.Symbol, side)
	if err != nil {
		return reasonError("CUSTODY_CHECK_PENDING", "read follow group ownership: %w", err)
	}
	if managedPeer {
		managedPeer, err = ti.verifyManagedFollowGroupPeer(dec)
		if err != nil {
			return reasonError("CUSTODY_CHECK_PENDING", "verify follow group continuity: %w", err)
		}
	}
	for _, p := range positions {
		if getStringField(p, "symbol") == dec.Symbol && strings.EqualFold(getStringField(p, "side"), side) && absFloat(getFloatField(p, "positionAmt", "quantity")) > 0 {
			if managedPeer {
				return nil
			}
			if err := ti.store.CopyTrade().IgnoreFollowGroup(ti.traderID, dec.LeaderPosID); err != nil {
				return reasonError("CUSTODY_CHECK_PENDING", "record independent group baseline: %w", err)
			}
			return reasonError("INDEPENDENT_POSITION_CONFLICT", "该交易所持仓范围已有独立仓位，本轮不接管")
		}
	}
	return nil
}

// A released original position is flat for this lifecycle, even if the venue
// has reused its position id for a new manual entry.
func (ti *TraderIntegration) copyGuardFollowerQuantity(c *store.CopyGuardCycle, fresh bool) (float64, bool) {
	if p, err := ti.store.CopyTrade().GetPositionCustody(ti.traderID, c.LeaderPosID); err == nil {
		if p.CycleID != c.ID || p.AttemptNo != c.ReentryCount {
			return 0, true
		}
		if p.State == "RELEASED" {
			// Trigger observation may have advanced the durable lifecycle since
			// the caller loaded c. A stale FOLLOWING value cannot settle a late fill.
			current, readErr := ti.store.CopyTrade().GetCopyGuardCycle(c.ID)
			if readErr != nil {
				return 0, false
			}
			if current.ClosedAt == nil && current.ReentryCount == c.ReentryCount && (store.CopyGuardRiskExitPending(current.Status) || current.ProtectionStatus == store.CopyGuardProtectionForcedExitPending) {
				return ti.releasedCopyGuardResidual(current, fresh)
			}
			return 0, true
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	return ti.followerPositionQuantity(c.Symbol, c.Side, c.MarginMode, c.FollowerPosID, fresh)
}

func (ti *TraderIntegration) copyGuardOwnsPosition(c *store.CopyGuardCycle) bool {
	if c == nil || c.ClosedAt != nil {
		return false
	}
	allowed, err := ti.store.CopyTrade().CopyGuardHasPositionAuthority(c.ID, c.ReentryCount)
	return err == nil && allowed
}

// Immutable fills distinguish a new manual position from an old position whose
// venue posId has been reused. A snapshot by itself cannot prove continuity.
func (ti *TraderIntegration) verifyCopyGuardContinuity(c *store.CopyGuardCycle) error {
	if !ti.copyGuardOwnsPosition(c) {
		return fmt.Errorf("original position ownership released")
	}
	_, ok := ti.executor.(trader.SymbolTradeHistoryProvider)
	if !ok {
		return nil
	} // legacy/in-process executors have no history capability
	fills, entryID, err := ti.loadCopyGuardContinuity(c)
	if err != nil {
		return err
	}
	ended, err := positionEndedInFills(fills, entryID, c.Symbol, c.Side)
	if err != nil {
		return err
	}
	if !ended {
		return nil
	}
	if err = ti.store.CopyTrade().ReleaseCopyGuardCustody(c.ID, c.ReentryCount, "CONFIRMED_FLAT_IN_FILLS"); err != nil {
		return err
	}
	// Preserve authoritative stop evidence before classifying an external close.
	triggerKnown := false
	if mgr, ok := ti.executor.(ProtectiveStopManagerV4); ok {
		if order, readErr := ti.store.CopyTrade().GetCopyGuardProtectiveOrder(c.ID); readErr == nil {
			live, queryErr := ti.resolveProtectiveOrder(mgr, order.AlgoID, order.AlgoClientID, c.Symbol)
			if queryErr != nil {
				return errCopyPositionEnded
			}
			if live != nil && isProtectiveStopFired(live.State) {
				ti.observeTrustedProtectiveTrigger(c, order, live)
				triggerKnown = true
			}
		}
	}
	if !triggerKnown {
		// This operation only classifies FOLLOWING cycles. Pending risk exits
		// retain their own trigger and settlement state.
		_ = ti.store.CopyTrade().MarkCopyGuardFollowerAbsent(c.ID, ti.traderID, c.LeaderPosID)
	}
	return errCopyPositionEnded
}

func (ti *TraderIntegration) loadCopyGuardContinuity(c *store.CopyGuardCycle) ([]trader.TradeRecord, string, error) {
	history, ok := ti.executor.(trader.SymbolTradeHistoryProvider)
	if !ok {
		return nil, "", fmt.Errorf("immutable fill history unavailable")
	}
	entryID := c.EntryOrderID
	start := c.OpenedAt
	if c.ReentryCount == 0 {
		if c.InitialIntentID > 0 {
			intent, readErr := ti.store.CopyTrade().GetExecutionIntentByID(c.InitialIntentID)
			if readErr != nil {
				return nil, "", readErr
			}
			if intent.CreatedAt.Before(start) {
				start = intent.CreatedAt
			}
		}
		if evidence, err := ti.store.CopyTrade().GetCopyGuardInitialFillEvidence(c.ID); err == nil && evidence.ExchangeOrderID != "" {
			entryID = evidence.ExchangeOrderID
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, "", err
		} else {
			// Mutable cycle.entry_order_id may already point to a later add.
			entryID = ""
			orders, orderErr := ti.store.CopyTrade().ListExecutionOrderAttempts(c.InitialIntentID)
			if orderErr != nil {
				return nil, "", orderErr
			}
			for _, o := range orders {
				if o.FilledQuantity > 0 && o.ExchangeOrderID != "" {
					entryID = o.ExchangeOrderID
					break
				}
			}
		}
	}
	if c.ReentryCount > 0 {
		attempts, err := ti.store.CopyTrade().ListCopyGuardAttempts(c.ID)
		if err != nil {
			return nil, "", err
		}
		for _, a := range attempts {
			if a.AttemptNo == c.ReentryCount {
				entryID = a.EntryOrderID
				start = a.OpenedAt
			}
		}
		intents, readErr := ti.store.CopyTrade().ListExecutionIntentsByCycle(c.ID)
		if readErr != nil {
			return nil, "", readErr
		}
		for _, i := range intents {
			if i.SourceKind == "AI_REENTRY" && i.AttemptNo == c.ReentryCount && i.CreatedAt.Before(start) {
				start = i.CreatedAt
			}
		}
	}
	if entryID == "" {
		return nil, "", fmt.Errorf("confirmed entry order identity unavailable")
	}
	queryStart, err := ti.store.CopyTrade().CopyGuardContinuityStart(c.ID, c.ReentryCount, entryID, start)
	if err != nil {
		return nil, "", err
	}
	var fills []trader.TradeRecord
	checkedAt := time.Now()
	if scoped, ok := ti.executor.(trader.ScopedTradeHistoryProvider); ok {
		fills, err = scoped.GetTradesForPosition(c.Symbol, c.MarginMode, queryStart)
	} else {
		fills, err = history.GetTradesForSymbol(c.Symbol, queryStart, 100)
	}
	if err != nil {
		return nil, "", err
	}
	var proof []store.CopyGuardContinuityFill
	for _, f := range fills {
		if strings.EqualFold(f.Symbol, c.Symbol) && strings.EqualFold(f.PositionSide, c.Side) {
			proof = append(proof, store.CopyGuardContinuityFill{TradeID: f.TradeID, OrderID: f.OrderID, Side: f.Side, PositionSide: f.PositionSide, Quantity: f.Quantity, TimeMS: f.Time.UnixMilli()})
		}
	}
	proof, err = ti.store.CopyTrade().MergeCopyGuardContinuityFills(c.ID, c.ReentryCount, checkedAt, proof)
	if err != nil {
		return nil, "", err
	}
	fills = nil
	for _, f := range proof {
		fills = append(fills, trader.TradeRecord{TradeID: f.TradeID, OrderID: f.OrderID, Symbol: c.Symbol, Side: f.Side, PositionSide: f.PositionSide, Quantity: f.Quantity, Time: time.UnixMilli(f.TimeMS)})
	}
	return fills, entryID, nil
}

func orderedContinuityFills(fills []trader.TradeRecord) []trader.TradeRecord {
	fills = append([]trader.TradeRecord(nil), fills...)
	sort.SliceStable(fills, func(i, j int) bool {
		if !fills[i].Time.Equal(fills[j].Time) {
			return fills[i].Time.Before(fills[j].Time)
		}
		a, b := fills[i].TradeID, fills[j].TradeID
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	return fills
}

func positionEndedInFills(fills []trader.TradeRecord, entryID, symbol, side string) (bool, error) {
	fills = orderedContinuityFills(fills)
	started := false
	quantity := 0.0
	seen := map[string]bool{}
	for _, f := range fills {
		if !strings.EqualFold(f.Symbol, symbol) || (!strings.EqualFold(f.PositionSide, side) && !strings.EqualFold(f.PositionSide, "BOTH")) {
			continue
		}
		if f.TradeID == "" || f.Quantity <= 0 || math.IsNaN(f.Quantity) || math.IsInf(f.Quantity, 0) {
			return false, fmt.Errorf("invalid immutable fill")
		}
		if !strings.EqualFold(f.Side, "BUY") && !strings.EqualFold(f.Side, "SELL") {
			return false, fmt.Errorf("invalid immutable fill direction")
		}
		if seen[f.TradeID] {
			continue
		}
		seen[f.TradeID] = true
		if !started {
			if f.OrderID != entryID {
				continue
			}
			started = true
		}
		increase := strings.EqualFold(f.Side, "BUY") == strings.EqualFold(side, "long")
		if increase {
			quantity += f.Quantity
		} else {
			quantity -= f.Quantity
		}
		if quantity <= 1e-10 {
			return true, nil
		}
	}
	if !started {
		return false, fmt.Errorf("initial entry is missing from immutable fill history")
	}
	return false, nil
}

func (ti *TraderIntegration) queueProtectionRefresh(dec *decision.Decision) {
	if dec == nil {
		return
	}
	if isAIReentryDecision(dec) || !ti.protectionAsync.Load() {
		ti.refreshStopLossAfterExecute(dec)
		return
	}
	defer func() {
		select {
		case ti.protectionWake <- struct{}{}:
		default:
		}
	}()
	cycle, readErr := ti.protectionCycleForDecision(dec)
	if errors.Is(readErr, sql.ErrNoRows) {
		return
	} // The monitor retires closed-cycle orders.
	if readErr != nil {
		logger.Errorf("load protection lifecycle: %v", readErr)
		return
	}
	copyDec := *dec
	copyDec.LeaderPosID = cycle.LeaderPosID
	copyDec.MarginMode = cycle.MarginMode
	payload, err := json.Marshal(&copyDec)
	if err == nil {
		err = ti.store.CopyTrade().QueueCopyGuardProtection(ti.traderID, cycle.LeaderPosID, cycle.ID, string(payload))
	}
	if err != nil {
		logger.Errorf("[%s] persist protection refresh: %v", ti.traderID, err)
		return
	}
}

func (ti *TraderIntegration) drainProtectionRefreshes() {
	jobs, err := ti.store.CopyTrade().ListCopyGuardProtectionJobs(ti.traderID)
	if err != nil {
		return
	}
	for _, job := range jobs {
		var dec decision.Decision
		if err = json.Unmarshal([]byte(job.DecisionJSON), &dec); err != nil {
			logger.Errorf("invalid durable protection job: %v", err)
			continue
		}
		if current, readErr := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, job.LeaderPosID); readErr == nil && current.ID == job.CycleID {
			ti.refreshStopLossAfterExecute(&dec)
		} else if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
			continue
		}
		if err = ti.store.CopyTrade().CompleteCopyGuardProtectionJob(ti.traderID, job.LeaderPosID, job.Revision); err != nil {
			logger.Errorf("complete protection refresh: %v", err)
		}
	}
}

// Called under protectionMu, before either planning or checking an old stop
// for a crossing. Failure must stop this refresh rather than reverting a newly
// observed user price that could not yet be persisted.
func (ti *TraderIntegration) reconcileManualStop(c *store.CopyGuardCycle, quantity, step float64) error {
	mgr, ok := ti.executor.(ProtectiveStopManagerV4)
	if !ok {
		return nil
	}
	stored, err := ti.store.CopyTrade().GetCopyGuardProtectiveOrder(c.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	live, err := ti.resolveProtectiveOrder(mgr, stored.AlgoID, stored.AlgoClientID, c.Symbol)
	if err != nil {
		return err
	}
	if live != nil && isProtectiveStopFired(live.State) {
		ti.observeTrustedProtectiveTrigger(c, stored, live)
		return errCopyGuardRiskExitGate
	}
	if live == nil || !strings.EqualFold(live.State, "live") {
		lister, ok := ti.executor.(trader.ProtectiveStopLister)
		if !ok {
			return nil
		}
		orders, listErr := lister.ListProtectiveStops(c.Symbol)
		if listErr != nil {
			return listErr
		}
		var candidates []trader.ProtectiveStopOrder
		cfg := copyGuardLifecycleConfig(c, ti.engine.config)
		if cfg == nil {
			return fmt.Errorf("protection policy unavailable")
		}
		for _, o := range orders {
			if o.AlgoID == stored.AlgoID || !strings.EqualFold(o.State, "live") || !strings.EqualFold(o.PositionSide, c.Side) || !protectiveMarginScopeMatches(&o, c.MarginMode) || !protectiveTriggerMatches(&o, c.Symbol, cfg.RiskTriggerPriceType) {
				continue
			}
			if !protectiveOrderQuantityMatches(o.CoverageMode, o.Quantity, quantity, step) {
				return fmt.Errorf("external stop coverage is ambiguous")
			}
			candidates = append(candidates, o)
		}
		if len(candidates) == 0 {
			return nil
		}
		if len(candidates) != 1 {
			return fmt.Errorf("multiple external stop candidates")
		}
		live = &candidates[0]
		if err = ti.store.CopyTrade().AcceptManualCopyGuardStop(c.ID, c.ReentryCount, live.AlgoID, live.TriggerPrice); err != nil {
			return err
		}
		return ti.store.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: c.ID, TraderID: c.TraderID, AlgoID: live.AlgoID, AlgoClientID: live.ClientID, Symbol: live.Symbol, Side: live.PositionSide, MarginMode: live.MarginMode, Quantity: live.Quantity, QuantityStep: step, CoverageMode: live.CoverageMode, TriggerPrice: live.TriggerPrice, TriggerType: live.TriggerType, Status: "live"})
	}
	cfg := copyGuardLifecycleConfig(c, ti.engine.config)
	if cfg == nil || !protectiveTriggerMatches(live, c.Symbol, cfg.RiskTriggerPriceType) || !strings.EqualFold(live.PositionSide, c.Side) || !protectiveMarginScopeMatches(live, c.MarginMode) {
		return fmt.Errorf("protective order identity changed")
	}
	control, err := ti.store.CopyTrade().GetCopyGuardStopControl(c.ID, c.ReentryCount)
	if err != nil {
		return err
	}
	if control.RequestPending {
		if sameProtectivePrice(live.TriggerPrice, control.RequestedPrice, 0) {
			return ti.store.CopyTrade().ConfirmCopyGuardStopRequest(c.ID, c.ReentryCount, control.RequestedPrice)
		}
		if sameProtectivePrice(live.TriggerPrice, control.PreviousPrice, 0) && (live.UpdatedAt.IsZero() || control.RequestAt == nil || !live.UpdatedAt.After(control.RequestAt.Add(time.Second))) {
			return nil
		}
	}
	target := stored.TriggerPrice
	if control.ManualPrice > 0 && control.ObservedAlgoID == live.AlgoID && sameProtectivePrice(live.TriggerPrice, control.ManualPrice, 0) && sameProtectivePrice(stored.TriggerPrice, live.TriggerPrice, 0) {
		return nil
	}
	if durable, readErr := ti.store.CopyTrade().GetCopyGuardAttemptFinalStop(c.ID, c.ReentryCount); readErr == nil {
		target = durable
	}
	if live.TriggerPrice <= 0 || math.IsNaN(live.TriggerPrice) || math.IsInf(live.TriggerPrice, 0) {
		return fmt.Errorf("invalid observed stop price")
	}
	if sameProtectivePrice(live.TriggerPrice, target, 0) {
		return nil
	}
	return ti.store.CopyTrade().AcceptManualCopyGuardStop(c.ID, c.ReentryCount, live.AlgoID, live.TriggerPrice)
}
