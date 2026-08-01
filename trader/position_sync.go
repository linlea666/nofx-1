package trader

import (
	"fmt"
	"math"
	"nofx/logger"
	"nofx/store"
	"strings"
	"sync"
	"time"
)

// PositionSyncManager Position status synchronization manager
// Responsible for periodically synchronizing exchange positions, detecting manual closures and other changes
type PositionSyncManager struct {
	store                *store.Store
	interval             time.Duration
	historySyncInterval  time.Duration // Interval for full history sync
	stopCh               chan struct{}
	wg                   sync.WaitGroup
	traderCache          map[string]Trader                  // trader_id -> Trader instance cache
	configCache          map[string]*store.TraderFullConfig // trader_id -> config cache
	cacheMutex           sync.RWMutex
	lastHistorySync      map[string]time.Time // trader_id -> last history sync time
	lastHistorySyncMutex sync.RWMutex
	historyRunMutex      sync.Mutex
	externalSyncMutex    sync.Mutex
	shortfallMutex       sync.Mutex
	shortfallObserved    map[string]positionShortfallObservation
	ownershipWarnedAt    map[string]time.Time
}

type positionShortfallObservation struct {
	exchangeQuantity float64
	localQuantity    float64
	observedAt       time.Time
}

// NewPositionSyncManager Create position synchronization manager
func NewPositionSyncManager(st *store.Store, interval time.Duration) *PositionSyncManager {
	if interval == 0 {
		interval = 10 * time.Second
	}
	return &PositionSyncManager{
		store:               st,
		interval:            interval,
		historySyncInterval: 5 * time.Minute, // Sync closed positions every 5 minutes
		stopCh:              make(chan struct{}),
		traderCache:         make(map[string]Trader),
		configCache:         make(map[string]*store.TraderFullConfig),
		lastHistorySync:     make(map[string]time.Time),
		shortfallObserved:   make(map[string]positionShortfallObservation),
		ownershipWarnedAt:   make(map[string]time.Time),
	}
}

// Start Start position synchronization service
func (m *PositionSyncManager) Start() {
	m.wg.Add(1)
	go m.run()
	logger.Info("📊 Position sync manager started")

	// Run startup sync in background
	go m.startupSync()
}

// Stop Stop position synchronization service
func (m *PositionSyncManager) Stop() {
	close(m.stopCh)
	m.wg.Wait()

	// Clear cache
	m.cacheMutex.Lock()
	m.traderCache = make(map[string]Trader)
	m.configCache = make(map[string]*store.TraderFullConfig)
	m.cacheMutex.Unlock()
	m.shortfallMutex.Lock()
	m.shortfallObserved = make(map[string]positionShortfallObservation)
	m.ownershipWarnedAt = make(map[string]time.Time)
	m.shortfallMutex.Unlock()

	logger.Info("📊 Position sync manager stopped")
}

// run Main loop
func (m *PositionSyncManager) run() {
	defer m.wg.Done()

	// Execute immediately on startup
	m.syncPositions()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.syncPositions()
		}
	}
}

// syncPositions Synchronize all position statuses
func (m *PositionSyncManager) syncPositions() {
	// Closed-accounting reconciliation is independent of whether any local
	// position remains OPEN. This is what lets a late exchange fill repair a
	// row that was provisionally closed as UNSCORABLE.
	m.syncDueClosedAccounting()

	// Get all OPEN status positions
	localPositions, err := m.store.Position().GetAllOpenPositions()
	if err != nil {
		logger.Infof("⚠️  Failed to get local positions: %v", err)
		return
	}

	// Group by trader_id
	positionsByTrader := make(map[string][]*store.TraderPosition)
	for _, pos := range localPositions {
		positionsByTrader[pos.TraderID] = append(positionsByTrader[pos.TraderID], pos)
	}

	// Process each trader
	for traderID, traderPositions := range positionsByTrader {
		m.syncTraderPositions(traderID, traderPositions)
	}
}

// syncTraderPositions Synchronize positions for a single trader
func (m *PositionSyncManager) syncTraderPositions(traderID string, localPositions []*store.TraderPosition) {
	lifecycle, lifecycleErr := m.store.Trader().GetLifecycle(traderID)
	if lifecycleErr != nil {
		// Legacy physically deleted traders are represented by tombstones.
		// Their OPEN rows remain audit evidence and must never be fabricated as
		// exchange-closed merely because configuration is absent.
		return
	}
	if lifecycle.Status != store.TraderLifecycleRunning || !lifecycle.IsRunning {
		return
	}
	// Get or create trader instance
	trader, err := m.getOrCreateTrader(traderID)
	if err != nil {
		logger.Infof("⚠️ Failed to get trader instance (ID: %s): %v", traderID, err)
		return
	}

	// Get exchange info for history sync
	config, _ := m.getTraderConfig(traderID)
	exchangeID := ""
	exchangeType := ""
	if config != nil {
		exchangeID = config.Exchange.ID             // UUID for database association
		exchangeType = config.Exchange.ExchangeType // "binance", "bybit" etc for trader creation
	}

	// Maybe run periodic history sync
	if exchangeID != "" && exchangeType != "" {
		m.maybeRunHistorySync(traderID, exchangeID, exchangeType, trader)
	}

	// Get current exchange positions
	exchangePositions, err := trader.GetPositions()
	if err != nil {
		logger.Infof("⚠️  Failed to get exchange positions (ID: %s): %v", traderID, err)
		return
	}

	// Build exchange position map: symbol_side -> position
	// Note: Exchange returns side as "long"/"short" (lowercase), database stores "LONG"/"SHORT" (uppercase)
	exchangeMap := make(map[string]map[string]interface{})
	for _, pos := range exchangePositions {
		symbol, _ := pos["symbol"].(string)
		side, _ := pos["side"].(string) // Note: use "side" not "positionSide"
		if symbol == "" || side == "" {
			continue
		}
		// Normalize side to uppercase for matching with database
		normalizedSide := strings.ToUpper(side)
		key := fmt.Sprintf("%s_%s", symbol, normalizedSide)
		exchangeMap[key] = pos
	}

	localByMarket := make(map[string][]*store.TraderPosition)
	for _, localPos := range localPositions {
		key := fmt.Sprintf("%s_%s", localPos.Symbol, localPos.Side)
		localByMarket[key] = append(localByMarket[key], localPos)
	}
	missingOpenQuantity := false
	for key, lots := range localByMarket {
		exchangePos, exists := exchangeMap[key]
		exchangeQty := 0.0
		if exists {
			exchangeQty = math.Abs(getFloatFromMap(exchangePos, "positionAmt"))
		}
		localQty := 0.0
		var oldest *store.TraderPosition
		for _, lot := range lots {
			localQty += math.Abs(lot.Quantity)
			if oldest == nil || lot.EntryTime.Before(oldest.EntryTime) {
				oldest = lot
			}
		}
		tolerance := math.Max(1e-8, localQty*1e-8)
		switch {
		case exchangeQty+tolerance < localQty && oldest != nil:
			// A just-filled position may be absent from one or more cached
			// exchange snapshots. Require a stable shortfall across a visibility
			// grace period before treating it as a manual/forced close.
			if m.positionShortfallConfirmed(traderID, key, exchangeQty, localQty, oldest.EntryTime) {
				// Reconcile once per account/symbol/side. The authoritative fill
				// allocator distributes exact close quantity FIFO across all lots.
				m.closeLocalPosition(oldest, trader, "manual", exchangeQty <= tolerance)
			}
		case exchangeQty > localQty+tolerance:
			m.clearPositionShortfall(traderID, key)
			missingOpenQuantity = true
		default:
			m.clearPositionShortfall(traderID, key)
		}
	}
	if missingOpenQuantity && exchangeID != "" && exchangeType != "" {
		m.syncExternalPositions(traderID, exchangeID, exchangeType, trader)
	}
}

func (m *PositionSyncManager) positionVisibilityGrace() time.Duration {
	grace := 2 * m.interval
	if grace < 30*time.Second {
		grace = 30 * time.Second
	}
	return grace
}

func (m *PositionSyncManager) positionShortfallConfirmed(traderID, marketKey string, exchangeQty, localQty float64, oldestEntry time.Time) bool {
	now := time.Now()
	key := traderID + "|" + marketKey
	tolerance := math.Max(1e-8, math.Max(math.Abs(exchangeQty), math.Abs(localQty))*1e-8)
	m.shortfallMutex.Lock()
	defer m.shortfallMutex.Unlock()
	observation, ok := m.shortfallObserved[key]
	if !ok || math.Abs(observation.exchangeQuantity-exchangeQty) > tolerance ||
		math.Abs(observation.localQuantity-localQty) > tolerance {
		m.shortfallObserved[key] = positionShortfallObservation{
			exchangeQuantity: exchangeQty,
			localQuantity:    localQty,
			observedAt:       now,
		}
		return false
	}
	grace := m.positionVisibilityGrace()
	return now.Sub(observation.observedAt) >= grace && (oldestEntry.IsZero() || now.Sub(oldestEntry) >= grace)
}

func (m *PositionSyncManager) clearPositionShortfall(traderID, marketKey string) {
	m.shortfallMutex.Lock()
	delete(m.shortfallObserved, traderID+"|"+marketKey)
	m.shortfallMutex.Unlock()
}

// closeLocalPosition Mark local position as closed
func (m *PositionSyncManager) closeLocalPosition(pos *store.TraderPosition, trader Trader, reason string, fullyClosed bool) {
	// Try to get accurate closure data from exchange first
	closedPnLRecord := m.findClosedPnLRecord(trader, pos)

	var exitPrice, realizedPnL, fee float64
	var closeReason string

	if closedPnLRecord != nil {
		// Use accurate data from exchange
		exitPrice = closedPnLRecord.ExitPrice
		realizedPnL = closedPnLRecord.RealizedPnL
		fee = closedPnLRecord.Fee
		closeReason = closedPnLRecord.CloseType
		logger.Infof("📊 Found accurate closure data from exchange for %s %s", pos.Symbol, pos.Side)
	} else {
		if !fullyClosed {
			logger.Infof("⏳ Authoritative partial-close fill not visible yet; keeping local lot open: %s %s",
				pos.Symbol, pos.Side)
			return
		}
		// Fallback: use market price and calculate PnL
		exitPrice = pos.EntryPrice // Default to entry price
		if price, err := trader.GetMarketPrice(pos.Symbol); err == nil && price > 0 {
			exitPrice = price
		}

		// Calculate PnL
		if pos.Side == "LONG" {
			realizedPnL = (exitPrice - pos.EntryPrice) * pos.Quantity
		} else {
			realizedPnL = (pos.EntryPrice - exitPrice) * pos.Quantity
		}
		closeReason = reason
		fee = 0
		logger.Infof("⚠️  Using market price for closure (no exchange data): %s %s", pos.Symbol, pos.Side)
	}

	var err error
	if closedPnLRecord != nil {
		_, err = m.store.Position().ClosePositionWithAllocations(
			pos.ID, pos.ExchangeID, closeRecordFills(closedPnLRecord), exitPrice, closeReason,
		)
	} else {
		// A mark-price estimate is useful for diagnostics but is not a real
		// settlement record. Keep the row unscorable rather than polluting
		// trusted realized PnL.
		err = m.store.Position().ClosePositionUnscorable(pos.ID, exitPrice, closeReason)
	}

	if err != nil {
		logger.Infof("⚠️  Failed to update position status: %v", err)
	} else {
		logger.Infof("📊 Position closed trader=%q id=%s %s %s @ %.4f → %.4f, PnL: %.2f, Fee: %.4f (%s)",
			m.store.Trader().ResolveDisplayName(pos.TraderID), pos.TraderID,
			pos.Symbol, pos.Side, pos.EntryPrice, exitPrice, realizedPnL, fee, closeReason)
	}
}

// findClosedPnLRecord Try to find matching ClosedPnL record from exchange
// For Binance, directly query trades for the specific symbol (more reliable than Income API)
func (m *PositionSyncManager) findClosedPnLRecord(trader Trader, pos *store.TraderPosition) *ClosedPnLRecord {
	// Prefer immutable fills. OKX position-history rows are cumulative under
	// one posId and therefore cannot safely identify individual local lots.
	if okxTrader, ok := trader.(*OKXTrader); ok {
		record, err := m.findClosedPnLFromTrades(okxTrader, pos, 90*24*time.Hour)
		if err != nil {
			logger.Infof("⚠️  Failed to get immutable OKX fills for %s: %v", pos.Symbol, err)
			return nil
		}
		return record
	}
	if provider, ok := trader.(SymbolTradeHistoryProvider); ok {
		record, err := m.findClosedPnLFromTrades(provider, pos, 24*time.Hour)
		if err != nil {
			logger.Infof("⚠️  Failed to get immutable fills for %s: %v", pos.Symbol, err)
			return nil
		}
		return record
	}

	// Fallback: use GetClosedPnL for other exchanges
	startTime := time.Now().Add(-24 * time.Hour)
	records, err := trader.GetClosedPnL(startTime, 100)
	if err != nil {
		logger.Infof("⚠️  Failed to get closed PnL records: %v", err)
		return nil
	}

	return m.aggregateClosedRecords(records, pos)
}

// findClosedPnLFromTrades queries immutable fills for one symbol.
func (m *PositionSyncManager) findClosedPnLFromTrades(trader SymbolTradeHistoryProvider, pos *store.TraderPosition, lookback time.Duration) (*ClosedPnLRecord, error) {
	// Use the local lot entry boundary, capped to the venue's supported window.
	// A one-hour-only query permanently stranded fills after a longer restart
	// or exchange-history delay.
	startTime := time.Now().Add(-lookback)
	if candidate := pos.EntryTime.Add(-time.Minute); candidate.After(startTime) {
		startTime = candidate
	}
	trades, err := trader.GetTradesForSymbol(pos.Symbol, startTime, 100)
	if err != nil {
		return nil, err
	}

	if len(trades) == 0 {
		logger.Infof("⚠️  No trades found for %s in the reconciliation window", pos.Symbol)
		return nil, nil
	}

	// Find all closing trades (realizedPnl != 0) that match this position
	var totalQty, totalPnL, totalFee float64
	var weightedExitPrice float64
	var latestExitTime time.Time
	var latestTradeID string
	matchCount := 0
	var matchedFills []ClosedPnLRecord

	posSide := strings.ToLower(pos.Side)

	for _, trade := range trades {
		// A break-even close legitimately has zero realized PnL. Direction and
		// position side identify a close; PnL must never be used as the action
		// classifier. Also reject fills from before this local lot existed.
		if !trade.Time.IsZero() && trade.Time.Before(pos.EntryTime) {
			continue
		}

		if !isClosingTradeForPosition(trade, posSide) {
			continue
		}

		// Aggregate this trade
		totalQty += trade.Quantity
		totalPnL += trade.RealizedPnL
		totalFee += trade.Fee
		weightedExitPrice += trade.Price * trade.Quantity
		matchCount++
		tradeID := trade.TradeID
		if tradeID == "" {
			tradeID = fmt.Sprintf("%s|%s|%d|%.12g|%.12g", trade.OrderID, trade.Symbol, trade.Time.UnixNano(), trade.Quantity, trade.Price)
		}
		matchedFills = append(matchedFills, ClosedPnLRecord{
			Symbol: trade.Symbol, Side: posSide, ExitPrice: trade.Price,
			Quantity: trade.Quantity, RealizedPnL: trade.RealizedPnL,
			Fee: trade.Fee, ExitTime: trade.Time, OrderID: trade.OrderID,
			ExchangeID: tradeID,
		})

		if trade.Time.After(latestExitTime) {
			latestExitTime = trade.Time
			latestTradeID = trade.TradeID
		}
	}

	if matchCount == 0 {
		logger.Infof("⚠️  No closing trades found for %s %s", pos.Symbol, pos.Side)
		return nil, nil
	}

	avgExitPrice := weightedExitPrice / totalQty

	logger.Infof("📊 Found %d closing trades for %s %s: qty=%.4f, exitPrice=%.6f, pnl=%.4f, fee=%.4f",
		matchCount, pos.Symbol, pos.Side, totalQty, avgExitPrice, totalPnL, totalFee)

	return &ClosedPnLRecord{
		Symbol:      pos.Symbol,
		Side:        posSide,
		EntryPrice:  pos.EntryPrice,
		ExitPrice:   avgExitPrice,
		Quantity:    totalQty,
		RealizedPnL: totalPnL,
		Fee:         totalFee,
		ExitTime:    latestExitTime,
		EntryTime:   pos.EntryTime,
		OrderID:     latestTradeID,
		ExchangeID:  latestTradeID,
		CloseType:   "unknown",
		Fills:       matchedFills,
	}, nil
}

// GetTraderForReconciliation constructs an exchange client from durable
// configuration even when the running AutoTrader has already been removed.
func (m *PositionSyncManager) GetTraderForReconciliation(traderID string) (Trader, error) {
	return m.getOrCreateTrader(traderID)
}

// ReconcileStoppedTrader is an explicit, fail-closed archive preparation.
// It never runs for RUNNING traders and never mutates local rows unless fresh
// exchange positions and both regular/algo pending-order snapshots are empty.
func (m *PositionSyncManager) ReconcileStoppedTrader(traderID string) ([]store.TraderLifecycleBlocker, error) {
	lifecycle, err := m.store.Trader().GetLifecycle(traderID)
	if err != nil {
		return nil, err
	}
	switch lifecycle.Status {
	case store.TraderLifecycleStopping, store.TraderLifecycleStoppingReconcileRequired, store.TraderLifecycleStopped:
	default:
		return nil, fmt.Errorf("trader lifecycle %s does not allow stopped reconciliation", lifecycle.Status)
	}
	executor, err := m.getOrCreateTrader(traderID)
	if err != nil {
		return nil, err
	}
	traderConfig, err := m.store.Trader().GetByID(traderID)
	if err != nil {
		return nil, err
	}
	localPositions, err := m.store.Position().GetOpenPositions(traderID)
	if err != nil {
		return nil, err
	}
	for _, position := range localPositions {
		if position.ExchangeID != "" && position.ExchangeID != traderConfig.ExchangeID {
			return nil, fmt.Errorf(
				"local position %d belongs to exchange %s, current trader exchange is %s",
				position.ID, position.ExchangeID, traderConfig.ExchangeID,
			)
		}
	}
	freshProvider, ok := executor.(FreshPositionProvider)
	if !ok {
		return nil, fmt.Errorf("exchange does not support authoritative fresh position snapshots")
	}
	pendingProvider, ok := executor.(PendingOrderProvider)
	if !ok {
		return nil, fmt.Errorf("exchange does not support authoritative pending-order snapshots")
	}
	exchangePositions, err := freshProvider.GetPositionsFresh()
	if err != nil {
		return nil, fmt.Errorf("fresh exchange position read failed: %w", err)
	}
	pendingOrders, err := pendingProvider.GetPendingOrdersFresh()
	if err != nil {
		return nil, fmt.Errorf("fresh exchange pending-order read failed: %w", err)
	}
	blockers := make([]store.TraderLifecycleBlocker, 0, len(exchangePositions)+len(pendingOrders))
	for index, position := range exchangePositions {
		symbol, _ := position["symbol"].(string)
		blockers = append(blockers, store.TraderLifecycleBlocker{
			Code: "EXCHANGE_POSITION_PRESENT", ResourceID: fmt.Sprintf("position:%d", index),
			Symbol: symbol, Status: "OPEN",
		})
	}
	for _, order := range pendingOrders {
		code := "EXCHANGE_ORDER_PENDING"
		if order.Protective {
			code = "EXCHANGE_PROTECTIVE_ORDER_PENDING"
		}
		blockers = append(blockers, store.TraderLifecycleBlocker{
			Code: code, ResourceID: order.ID, Symbol: order.Symbol, Status: order.Status,
		})
	}
	if len(blockers) > 0 {
		return blockers, nil
	}
	if len(localPositions) == 0 {
		_, err = m.store.CopyTrade().RetireStoppedTraderCopyGuardState(
			traderID, "fresh exchange positions=0; regular/algo pending orders=0; local open positions=0",
		)
		if err != nil {
			return nil, fmt.Errorf("retire stopped Copy Guard state: %w", err)
		}
		return nil, nil
	}
	oldestByMarket := make(map[string]*store.TraderPosition)
	for _, position := range localPositions {
		key := strings.ToUpper(position.Symbol) + "|" + strings.ToUpper(position.Side)
		if current := oldestByMarket[key]; current == nil || position.EntryTime.Before(current.EntryTime) {
			oldestByMarket[key] = position
		}
	}
	accountUsers, err := m.store.Trader().CountNonArchivedByExchange(traderConfig.ExchangeID)
	if err != nil {
		return nil, err
	}
	evidence := "fresh exchange positions=0; regular/algo pending orders=0; immutable fills allocated where available; residual historical attribution unavailable"
	if accountUsers <= 1 {
		tradeProvider, supported := executor.(SymbolTradeHistoryProvider)
		if !supported {
			return nil, fmt.Errorf("exchange does not support immutable symbol fill history")
		}
		fillLookback := 24 * time.Hour
		if _, isOKX := executor.(*OKXTrader); isOKX {
			fillLookback = 90 * 24 * time.Hour
		}
		for _, oldest := range oldestByMarket {
			record, historyErr := m.findClosedPnLFromTrades(tradeProvider, oldest, fillLookback)
			if historyErr != nil {
				return nil, fmt.Errorf("immutable fill read failed for %s %s: %w", oldest.Symbol, oldest.Side, historyErr)
			}
			if record == nil {
				continue
			}
			allocated, allocationErr := m.store.Position().ClosePositionWithAllocations(
				oldest.ID, oldest.ExchangeID, closeRecordFills(record), record.ExitPrice,
				"AUTHORITATIVE_EXCHANGE_FLAT",
			)
			if allocationErr != nil {
				return nil, fmt.Errorf("allocate immutable close fills for %s %s: %w", oldest.Symbol, oldest.Side, allocationErr)
			}
			if !allocated {
				if auditErr := m.store.Position().ClosePositionUnscorableWithEvidence(
					oldest.ID, record.ExitPrice, "AUTHORITATIVE_EXCHANGE_FLAT", evidence,
				); auditErr != nil {
					return nil, auditErr
				}
			}
		}
	} else {
		evidence = fmt.Sprintf(
			"fresh exchange positions=0; regular/algo pending orders=0; shared exchange account has %d non-archived traders; fill ownership is ambiguous",
			accountUsers,
		)
	}

	// Any remainder cannot be attributed to a visible immutable fill. The
	// account is authoritatively flat and order-free, so retain it as audit
	// history but exclude it from trusted PnL.
	residual, err := m.store.Position().GetOpenPositions(traderID)
	if err != nil {
		return nil, err
	}
	for _, position := range residual {
		exitPrice := position.EntryPrice
		if price, priceErr := executor.GetMarketPrice(position.Symbol); priceErr == nil && price > 0 {
			exitPrice = price
		}
		if err = m.store.Position().ClosePositionUnscorableWithEvidence(
			position.ID, exitPrice, "AUTHORITATIVE_EXCHANGE_FLAT", evidence,
		); err != nil {
			return nil, err
		}
	}
	if _, err = m.store.CopyTrade().RetireStoppedTraderCopyGuardState(traderID, evidence); err != nil {
		return nil, fmt.Errorf("retire stopped Copy Guard state: %w", err)
	}
	return nil, nil
}

func isClosingTradeForPosition(trade TradeRecord, positionSide string) bool {
	tradeSide := strings.ToUpper(strings.TrimSpace(trade.Side))
	exchangePositionSide := strings.ToUpper(strings.TrimSpace(trade.PositionSide))
	positionSide = strings.ToLower(strings.TrimSpace(positionSide))
	switch exchangePositionSide {
	case "LONG":
		return positionSide == "long" && tradeSide == "SELL"
	case "SHORT":
		return positionSide == "short" && tradeSide == "BUY"
	case "BOTH", "":
		return (positionSide == "long" && tradeSide == "SELL") ||
			(positionSide == "short" && tradeSide == "BUY")
	default:
		return false
	}
}

// aggregateClosedRecords aggregates closed PnL records for a position
func (m *PositionSyncManager) aggregateClosedRecords(records []ClosedPnLRecord, pos *store.TraderPosition) *ClosedPnLRecord {
	if len(records) == 0 {
		return nil
	}

	posSide := strings.ToLower(pos.Side)
	var matchingRecords []ClosedPnLRecord

	for i := range records {
		record := &records[i]
		if record.Symbol != pos.Symbol {
			continue
		}

		recordSide := strings.ToLower(record.Side)
		if recordSide != posSide {
			continue
		}
		if !record.ExitTime.IsZero() && record.ExitTime.Before(pos.EntryTime) {
			continue
		}

		matchingRecords = append(matchingRecords, *record)
	}

	if len(matchingRecords) == 0 {
		return nil
	}

	var totalQty, totalPnL, totalFee float64
	var weightedExitPrice float64
	var latestExitTime time.Time
	var latestOrderID, latestExchangeID string
	var matchedFills []ClosedPnLRecord

	for _, rec := range matchingRecords {
		totalQty += rec.Quantity
		totalPnL += rec.RealizedPnL
		totalFee += rec.Fee
		weightedExitPrice += rec.ExitPrice * rec.Quantity
		if len(rec.Fills) > 0 {
			matchedFills = append(matchedFills, rec.Fills...)
		} else {
			matchedFills = append(matchedFills, rec)
		}

		if rec.ExitTime.After(latestExitTime) {
			latestExitTime = rec.ExitTime
			latestOrderID = rec.OrderID
			latestExchangeID = rec.ExchangeID
		}
	}

	avgExitPrice := weightedExitPrice / totalQty

	logger.Infof("📊 Aggregated %d closing trades for %s %s: qty=%.4f, pnl=%.4f, fee=%.4f",
		len(matchingRecords), pos.Symbol, pos.Side, totalQty, totalPnL, totalFee)

	return &ClosedPnLRecord{
		Symbol:      pos.Symbol,
		Side:        posSide,
		EntryPrice:  pos.EntryPrice,
		ExitPrice:   avgExitPrice,
		Quantity:    totalQty,
		RealizedPnL: totalPnL,
		Fee:         totalFee,
		ExitTime:    latestExitTime,
		EntryTime:   pos.EntryTime,
		OrderID:     latestOrderID,
		ExchangeID:  latestExchangeID,
		CloseType:   "unknown",
		Fills:       matchedFills,
	}
}

// abs returns absolute value of float64
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// getOrCreateTrader Get or create trader instance
func (m *PositionSyncManager) getOrCreateTrader(traderID string) (Trader, error) {
	m.cacheMutex.RLock()
	trader, exists := m.traderCache[traderID]
	m.cacheMutex.RUnlock()

	if exists && trader != nil {
		return trader, nil
	}

	// Need to create new trader instance
	config, err := m.getTraderConfig(traderID)
	if err != nil {
		return nil, fmt.Errorf("failed to get trader config: %w", err)
	}

	trader, err = m.createTrader(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create trader instance: %w", err)
	}

	m.cacheMutex.Lock()
	m.traderCache[traderID] = trader
	m.cacheMutex.Unlock()

	return trader, nil
}

// getTraderConfig Get trader configuration
func (m *PositionSyncManager) getTraderConfig(traderID string) (*store.TraderFullConfig, error) {
	m.cacheMutex.RLock()
	config, exists := m.configCache[traderID]
	m.cacheMutex.RUnlock()

	if exists {
		return config, nil
	}

	// Get from database
	traders, err := m.store.Trader().ListAll()
	if err != nil {
		return nil, fmt.Errorf("failed to get trader list: %w", err)
	}

	var userID string
	for _, t := range traders {
		if t.ID == traderID {
			userID = t.UserID
			break
		}
	}

	if userID == "" {
		return nil, fmt.Errorf("trader not found: %s", traderID)
	}

	config, err = m.store.Trader().GetFullConfig(userID, traderID)
	if err != nil {
		return nil, err
	}

	m.cacheMutex.Lock()
	m.configCache[traderID] = config
	m.cacheMutex.Unlock()

	return config, nil
}

// createTrader Create trader instance based on configuration
func (m *PositionSyncManager) createTrader(config *store.TraderFullConfig) (Trader, error) {
	exchange := config.Exchange

	// Use exchange.ExchangeType to determine specific exchange, not exchange.ID (UUID) or exchange.Type (cex/dex)
	switch exchange.ExchangeType {
	case "binance":
		return NewFuturesTrader(exchange.APIKey, exchange.SecretKey, config.Trader.UserID), nil

	case "bybit":
		return NewBybitTrader(exchange.APIKey, exchange.SecretKey), nil

	case "okx":
		return NewOKXTrader(exchange.APIKey, exchange.SecretKey, exchange.Passphrase), nil

	case "bitget":
		return NewBitgetTrader(exchange.APIKey, exchange.SecretKey, exchange.Passphrase), nil

	case "hyperliquid":
		return NewHyperliquidTrader(exchange.SecretKey, exchange.HyperliquidWalletAddr, exchange.Testnet)

	case "aster":
		return NewAsterTrader(exchange.AsterUser, exchange.AsterSigner, exchange.AsterPrivateKey)

	case "lighter":
		if exchange.LighterWalletAddr == "" || exchange.LighterAPIKeyPrivateKey == "" {
			return nil, fmt.Errorf("Lighter requires wallet address and API Key private key")
		}
		// Lighter only supports mainnet
		return NewLighterTraderV2(
			exchange.LighterWalletAddr,
			exchange.LighterAPIKeyPrivateKey,
			exchange.LighterAPIKeyIndex,
			false, // Always use mainnet for Lighter
		)

	default:
		return nil, fmt.Errorf("unsupported exchange type: %s", exchange.ExchangeType)
	}
}

// InvalidateCache Invalidate cache
func (m *PositionSyncManager) InvalidateCache(traderID string) {
	m.cacheMutex.Lock()
	defer m.cacheMutex.Unlock()

	delete(m.traderCache, traderID)
	delete(m.configCache, traderID)
}

// getFloatFromMap Get float64 value from map
func getFloatFromMap(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch val := v.(type) {
		case float64:
			return val
		case int64:
			return float64(val)
		case int:
			return float64(val)
		case string:
			var f float64
			fmt.Sscanf(val, "%f", &f)
			return f
		}
	}
	return 0
}

// =============================================================================
// Startup and History Sync Methods
// =============================================================================

// startupSync performs initial sync on startup
// 1. Sync existing positions from exchange (to detect external positions)
// 2. Sync closed positions history from exchange
func (m *PositionSyncManager) startupSync() {
	logger.Info("📊 Starting startup sync...")

	// Get all traders
	traders, err := m.store.Trader().ListAll()
	if err != nil {
		logger.Infof("⚠️  Failed to get traders for startup sync: %v", err)
		return
	}

	for _, traderInfo := range traders {
		if traderInfo.LifecycleStatus != store.TraderLifecycleRunning || !traderInfo.IsRunning {
			continue
		}
		traderID := traderInfo.ID

		// Get trader instance
		trader, err := m.getOrCreateTrader(traderID)
		if err != nil {
			logger.Infof("⚠️  Failed to get trader instance for startup sync (ID: %s): %v", traderID, err)
			continue
		}

		// Get exchange info
		config, err := m.getTraderConfig(traderID)
		if err != nil {
			logger.Infof("⚠️  Failed to get trader config for startup sync (ID: %s): %v", traderID, err)
			continue
		}
		exchangeID := config.Exchange.ID             // UUID
		exchangeType := config.Exchange.ExchangeType // "binance", "bybit" etc

		// 1. Sync current open positions from exchange
		m.syncExternalPositions(traderID, exchangeID, exchangeType, trader)

		// 2. Sync closed positions history and delayed local settlements.
		m.maybeRunHistorySync(traderID, exchangeID, exchangeType, trader)
	}

	logger.Info("📊 Startup sync completed")
}

// syncExternalPositions syncs positions that exist on exchange but not locally
// These could be positions opened manually or from other systems
func (m *PositionSyncManager) syncExternalPositions(traderID, exchangeID, exchangeType string, trader Trader) {
	m.externalSyncMutex.Lock()
	defer m.externalSyncMutex.Unlock()
	ok, runningOwners, reason := m.canClaimExternalPositions(traderID, exchangeID)
	if !ok {
		m.warnExternalOwnershipBlocked(traderID, exchangeID, reason)
		return
	}
	// Get current positions from exchange
	exchangePositions, err := trader.GetPositions()
	if err != nil {
		logger.Infof("⚠️  Failed to get exchange positions for external sync (ID: %s): %v", traderID, err)
		return
	}

	// Get local open positions
	localPositions, err := m.store.Position().GetOpenPositions(traderID)
	if err != nil {
		logger.Infof("⚠️  Failed to get local positions for external sync (ID: %s): %v", traderID, err)
		return
	}

	// Build all local lots per symbol/side. A single exchange position may be
	// composed of several local opens/adds; collapsing the map to one row made
	// delayed exchange fills disappear whenever any row already existed.
	localMap := make(map[string][]*store.TraderPosition)
	for _, pos := range localPositions {
		key := fmt.Sprintf("%s_%s", pos.Symbol, pos.Side)
		localMap[key] = append(localMap[key], pos)
	}

	// Find positions that exist on exchange but not locally
	for _, pos := range exchangePositions {
		symbol, _ := pos["symbol"].(string)
		side, _ := pos["side"].(string)
		if symbol == "" || side == "" {
			continue
		}

		// Normalize side
		normalizedSide := side
		if side == "Buy" || side == "LONG" || side == "long" {
			normalizedSide = "LONG"
		} else if side == "Sell" || side == "SHORT" || side == "short" {
			normalizedSide = "SHORT"
		}

		key := fmt.Sprintf("%s_%s", symbol, normalizedSide)
		if runningOwners > 1 {
			owner, candidates, ownerErr := m.store.Trader().ResolveExclusivePositionOwner(exchangeID, symbol, normalizedSide)
			if ownerErr != nil {
				m.warnExternalOwnershipBlocked(traderID, exchangeID, "POSITION_SYNC_OWNER_QUERY_FAILED")
				continue
			}
			if candidates != 1 || owner != traderID {
				m.warnExternalOwnershipBlocked(traderID, exchangeID, "POSITION_OWNERSHIP_AMBIGUOUS")
				continue
			}
		}

		qty := getFloatFromMap(pos, "positionAmt")
		if qty < 0 {
			qty = -qty
		}
		if qty < 0.0000001 {
			continue // No actual position
		}

		entryPrice := getFloatFromMap(pos, "entryPrice")
		leverage := int(getFloatFromMap(pos, "leverage"))
		if leverage == 0 {
			leverage = 1
		}

		// Get entry time if available
		createdTime := getFloatFromMap(pos, "createdTime")
		var entryTime time.Time
		if createdTime > 0 {
			entryTime = time.UnixMilli(int64(createdTime))
		} else {
			entryTime = time.Now() // Use current time as fallback
		}

		localQuantity := 0.0
		for _, local := range localMap[key] {
			localQuantity += math.Abs(local.Quantity)
		}
		missingQuantity := qty - localQuantity
		if missingQuantity <= math.Max(1e-8, qty*1e-8) {
			continue
		}

		// Persist only the exchange-confirmed missing delta. This reconciles a
		// delayed open/add fill without replacing existing strategy lots or
		// creating the full exchange position again.
		exchangePositionID := fmt.Sprintf("%s_%s_reconciled_%d", symbol, normalizedSide, entryTime.UnixMilli())

		newPos := &store.TraderPosition{
			TraderID:           traderID,
			ExchangeID:         exchangeID,
			ExchangeType:       exchangeType,
			ExchangePositionID: exchangePositionID,
			Symbol:             symbol,
			Side:               normalizedSide,
			Quantity:           missingQuantity,
			EntryPrice:         entryPrice,
			EntryTime:          entryTime,
			Leverage:           leverage,
			Source:             "sync", // Mark as synced from exchange
		}

		created, createErr := m.store.Position().CreateOpenPositionIfAbsent(newPos)
		if createErr != nil {
			logger.Infof("⚠️  Failed to create external position record: %v", createErr)
		} else if created {
			logger.Infof("📊 Reconciled exchange-confirmed missing position quantity: trader=%q id=%s %s %s @ %.4f (delta: %.4f, exchange: %.4f, local: %.4f)",
				m.store.Trader().ResolveDisplayName(traderID), traderID,
				symbol, normalizedSide, entryPrice, missingQuantity, qty, localQuantity)
		}
	}
}

func (m *PositionSyncManager) canClaimExternalPositions(traderID, exchangeID string) (bool, int, string) {
	traderInfo, err := m.store.Trader().GetByID(traderID)
	if err != nil {
		return false, 0, "POSITION_SYNC_TRADER_UNAVAILABLE"
	}
	if !traderInfo.IsRunning || traderInfo.LifecycleStatus != store.TraderLifecycleRunning {
		return false, 0, "POSITION_SYNC_TRADER_STOPPED"
	}
	if traderInfo.ExchangeID != exchangeID {
		return false, 0, "POSITION_SYNC_EXCHANGE_MISMATCH"
	}
	running, err := m.store.Trader().CountRunningByExchange(exchangeID)
	if err != nil {
		return false, 0, "POSITION_SYNC_OWNER_QUERY_FAILED"
	}
	if running == 0 {
		return false, 0, "POSITION_SYNC_OWNER_QUERY_FAILED"
	}
	return true, running, ""
}

func (m *PositionSyncManager) warnExternalOwnershipBlocked(traderID, exchangeID, reason string) {
	key := exchangeID + "|" + traderID + "|" + reason
	now := time.Now()
	m.shortfallMutex.Lock()
	last := m.ownershipWarnedAt[key]
	if !last.IsZero() && now.Sub(last) < time.Hour {
		m.shortfallMutex.Unlock()
		return
	}
	m.ownershipWarnedAt[key] = now
	m.shortfallMutex.Unlock()
	logger.Warnf("⚠️ Position sync did not claim external quantity: reason=%s trader=%q id=%s exchange=%s",
		reason, m.store.Trader().ResolveDisplayName(traderID), traderID, exchangeID)
}

// syncClosedPositionsHistory syncs closed positions from exchange history
// IMPORTANT: Only exchanges with position-level history API should sync history:
// - Bybit: /v5/position/closed-pnl (accurate position records)
// - OKX: /api/v5/account/positions-history (accurate position records)
// Other exchanges (Binance, Hyperliquid, Lighter, Aster) only have trade-level data,
// which cannot accurately reconstruct positions. They should NOT sync historical positions.
func (m *PositionSyncManager) syncClosedPositionsHistory(traderID, exchangeID, exchangeType string, trader Trader) {
	// Only sync history for exchanges with position-level API
	// Binance/Hyperliquid/Lighter/Aster only have trade-level data, skip history sync
	switch exchangeType {
	case "bybit", "okx":
		// These exchanges have position-level history API, proceed with sync
	default:
		// Other exchanges don't have accurate position history API
		// Their GetClosedPnL only returns recent trades for closure detection, not for history sync
		return
	}

	// Get last sync time from database
	lastSyncTime, err := m.store.Position().GetLastClosedPositionTime(traderID)
	if err != nil {
		logger.Infof("⚠️  Failed to get last closed position time (ID: %s): %v", traderID, err)
		// First sync: go back 90 days to get more history
		lastSyncTime = time.Now().Add(-90 * 24 * time.Hour)
	}

	// Subtract a small buffer to avoid missing positions at the boundary
	startTime := lastSyncTime.Add(-1 * time.Minute)

	// Pagination loop to get all records
	const batchSize = 500
	totalCreated := 0
	totalSkipped := 0

	for {
		// Get closed positions from exchange
		closedRecords, err := trader.GetClosedPnL(startTime, batchSize)
		if err != nil {
			logger.Infof("⚠️  Failed to get closed PnL records (ID: %s): %v", traderID, err)
			break
		}

		if len(closedRecords) == 0 {
			break
		}

		// Convert to store.ClosedPnLRecord and sync
		storeRecords := make([]store.ClosedPnLRecord, len(closedRecords))
		var latestExitTime time.Time
		for i, rec := range closedRecords {
			storeRecords[i] = store.ClosedPnLRecord{
				Symbol:      rec.Symbol,
				Side:        rec.Side,
				EntryPrice:  rec.EntryPrice,
				ExitPrice:   rec.ExitPrice,
				Quantity:    rec.Quantity,
				RealizedPnL: rec.RealizedPnL,
				Fee:         rec.Fee,
				Leverage:    rec.Leverage,
				EntryTime:   rec.EntryTime,
				ExitTime:    rec.ExitTime,
				OrderID:     rec.OrderID,
				CloseType:   rec.CloseType,
				ExchangeID:  rec.ExchangeID,
			}
			// Track latest exit time for pagination
			if rec.ExitTime.After(latestExitTime) {
				latestExitTime = rec.ExitTime
			}
		}

		created, skipped, err := m.store.Position().SyncClosedPositions(traderID, exchangeID, exchangeType, storeRecords)
		if err != nil {
			logger.Infof("⚠️  Failed to sync closed positions (ID: %s): %v", traderID, err)
			break
		}

		totalCreated += created
		totalSkipped += skipped

		// If we got fewer records than batch size, we've reached the end
		if len(closedRecords) < batchSize {
			break
		}

		// Move start time forward for next batch (add 1ms to avoid duplicate)
		startTime = latestExitTime.Add(time.Millisecond)
	}

	if totalCreated > 0 {
		logger.Infof("📊 Synced %d new closed positions for trader=%q id=%s (skipped %d duplicates)",
			totalCreated, m.store.Trader().ResolveDisplayName(traderID), traderID, totalSkipped)
	}

}

func closeRecordFills(record *ClosedPnLRecord) []store.PositionCloseFill {
	if record == nil {
		return nil
	}
	records := record.Fills
	if len(records) == 0 {
		records = []ClosedPnLRecord{*record}
	}
	fills := make([]store.PositionCloseFill, 0, len(records))
	for _, item := range records {
		tradeID := strings.TrimSpace(item.ExchangeID)
		if tradeID == "" {
			tradeID = strings.TrimSpace(item.OrderID)
		}
		if tradeID == "" {
			tradeID = fmt.Sprintf("%s|%s|%d|%.12g|%.12g", item.Symbol, item.Side, item.ExitTime.UnixNano(), item.Quantity, item.ExitPrice)
		}
		fills = append(fills, store.PositionCloseFill{
			TradeID: tradeID, Symbol: item.Symbol, Side: item.Side,
			Quantity: item.Quantity, ExitPrice: item.ExitPrice,
			RealizedPnL: item.RealizedPnL, Fee: item.Fee,
			FillTime: item.ExitTime, DataQuality: "VERIFIED",
		})
	}
	return fills
}

// reconcilePendingClosedPositions retries exchange settlement for lots that
// were provisionally closed before authoritative fills became visible.
func (m *PositionSyncManager) reconcilePendingClosedPositions(traderID, exchangeID string, trader Trader) {
	positions, err := m.store.Position().GetPendingClosedPositions(traderID, 100)
	if err != nil {
		logger.Infof("⚠️ Failed to read pending closed positions (ID: %s): %v", traderID, err)
		return
	}
	// One authoritative response for a symbol/side is allocated FIFO across all
	// compatible lots. Re-querying it once per pending lot only wastes exchange
	// rate limit and can make reconciliation ordering depend on response timing.
	seenMarket := make(map[string]struct{})
	for _, pos := range positions {
		key := strings.ToUpper(pos.Symbol) + "|" + strings.ToUpper(pos.Side)
		if _, seen := seenMarket[key]; seen {
			continue
		}
		seenMarket[key] = struct{}{}
		record := m.findClosedPnLRecord(trader, pos)
		if record == nil {
			continue
		}
		claimed, reconcileErr := m.store.Position().ClosePositionWithAllocations(
			pos.ID, exchangeID, closeRecordFills(record), record.ExitPrice, record.CloseType,
		)
		if reconcileErr != nil {
			logger.Infof("⚠️ Failed to reconcile delayed close fill (position=%d): %v", pos.ID, reconcileErr)
			continue
		}
		if claimed {
			logger.Infof("📊 Reconciled delayed authoritative close fill: trader=%q id=%s %s %s",
				m.store.Trader().ResolveDisplayName(traderID), traderID, pos.Symbol, pos.Side)
		}
	}
}

// syncDueClosedAccounting runs account-level history and delayed settlement
// reconciliation even when the account has no currently open local positions.
func (m *PositionSyncManager) syncDueClosedAccounting() {
	traders, err := m.store.Trader().ListAll()
	if err != nil {
		logger.Infof("⚠️ Failed to get traders for periodic accounting sync: %v", err)
		return
	}
	for _, traderInfo := range traders {
		if traderInfo == nil || traderInfo.LifecycleStatus != store.TraderLifecycleRunning ||
			!traderInfo.IsRunning {
			// A stopped trader deliberately retains exchange protection and
			// audit state. Periodic reconciliation must remain silent until an
			// explicit restart (or a future operator-driven one-shot sync).
			continue
		}
		traderID := traderInfo.ID
		trader, traderErr := m.getOrCreateTrader(traderID)
		if traderErr != nil {
			continue
		}
		config, configErr := m.getTraderConfig(traderID)
		if configErr != nil || config == nil {
			continue
		}
		m.maybeRunHistorySync(traderID, config.Exchange.ID, config.Exchange.ExchangeType, trader)
	}
}

// maybeRunHistorySync checks if it's time to run authoritative close history
// and delayed-settlement reconciliation for a trader.
func (m *PositionSyncManager) maybeRunHistorySync(traderID, exchangeID, exchangeType string, trader Trader) {
	m.historyRunMutex.Lock()
	defer m.historyRunMutex.Unlock()

	m.lastHistorySyncMutex.RLock()
	lastSync, exists := m.lastHistorySync[traderID]
	m.lastHistorySyncMutex.RUnlock()

	if !exists || time.Since(lastSync) >= m.historySyncInterval {
		m.syncClosedPositionsHistory(traderID, exchangeID, exchangeType, trader)
		m.reconcilePendingClosedPositions(traderID, exchangeID, trader)
		m.lastHistorySyncMutex.Lock()
		m.lastHistorySync[traderID] = time.Now()
		m.lastHistorySyncMutex.Unlock()
	}
}
