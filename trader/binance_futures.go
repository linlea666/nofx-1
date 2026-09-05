package trader

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"nofx/hook"
	"nofx/logger"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
)

// getBrOrderID generates unique order ID (for futures contracts)
// Format: x-{BR_ID}{TIMESTAMP}{RANDOM}
// Futures limit is 32 characters, use this limit consistently
// Uses nanosecond timestamp + random number to ensure global uniqueness (collision probability < 10^-20)
func getBrOrderID() string {
	brID := "KzrpZaP9" // Futures br ID

	// Calculate available space: 32 - len("x-KzrpZaP9") = 32 - 11 = 21 characters
	// Allocation: 13-digit timestamp + 8-digit random = 21 characters (perfect utilization)
	timestamp := time.Now().UnixNano() % 10000000000000 // 13-digit nanosecond timestamp

	// Generate 4-byte random number (8 hex digits)
	randomBytes := make([]byte, 4)
	rand.Read(randomBytes)
	randomHex := hex.EncodeToString(randomBytes)

	// Format: x-KzrpZaP9{13-digit timestamp}{8-digit random}
	// Example: x-KzrpZaP91234567890123abcdef12 (exactly 31 characters)
	orderID := fmt.Sprintf("x-%s%d%s", brID, timestamp, randomHex)

	// Ensure not exceeding 32-character limit (theoretically exactly 31 characters)
	if len(orderID) > 32 {
		orderID = orderID[:32]
	}

	return orderID
}

// FuturesTrader Binance futures trader
type FuturesTrader struct {
	client *futures.Client

	// Balance cache
	cachedBalance     map[string]interface{}
	balanceCacheTime  time.Time
	balanceCacheMutex sync.RWMutex

	// Position cache
	cachedPositions     []map[string]interface{}
	positionsCacheTime  time.Time
	positionsCacheMutex sync.RWMutex

	// Cache validity period (15 seconds)
	cacheDuration time.Duration

	instrumentsCache      map[string]*binanceExecutionInstrument
	instrumentsCacheTime  time.Time
	instrumentsCacheMutex sync.RWMutex

	// Server-time offset upkeep, see binanceClock.
	clock binanceClock
}

// binanceClock tracks when the client's TimeOffset was last derived from
// Binance's own clock.
//
// Resync is lazy (driven by whichever signed call notices staleness) instead of
// a background ticker: traders are created and archived while the process runs,
// so a per-trader ticker without a stop channel would leak one goroutine per
// archived trader.
type binanceClock struct {
	mu       sync.Mutex
	lastSync time.Time
}

type binanceExecutionInstrument struct {
	ExecutionInstrument
	StepSize    float64
	MinQuantity float64
	MaxQuantity float64
	TickSize    float64
	MinNotional float64
}

// NewFuturesTrader creates futures trader
func NewFuturesTrader(apiKey, secretKey string, userId string) *FuturesTrader {
	client := futures.NewClient(apiKey, secretKey)

	hookRes := hook.HookExec[hook.NewBinanceTraderResult](hook.NEW_BINANCE_TRADER, userId, client)
	if hookRes != nil && hookRes.GetResult() != nil {
		client = hookRes.GetResult()
	}

	trader := &FuturesTrader{
		client:           client,
		cacheDuration:    15 * time.Second, // 15-second cache
		instrumentsCache: make(map[string]*binanceExecutionInstrument),
	}

	// Sync time to avoid "Timestamp ahead" errors. This is only the first sample;
	// the offset is kept fresh from the signed read paths (see binanceClock),
	// because a one-shot sync cannot survive a later NTP step correction.
	trader.syncClockLocked("startup")

	// Set dual-side position mode (Hedge Mode)
	// This is required because the code uses PositionSide (LONG/SHORT)
	if err := trader.setDualSidePosition(); err != nil {
		logger.Infof("⚠️ Failed to set dual-side position mode: %v (ignore this warning if already in dual-side mode)", err)
	}

	return trader
}

// setDualSidePosition sets dual-side position mode (called during initialization)
func (t *FuturesTrader) setDualSidePosition() error {
	// Try to set dual-side position mode
	err := t.client.NewChangePositionModeService().
		DualSide(true). // true = dual-side position (Hedge Mode)
		Do(context.Background())

	if err != nil {
		// If error message contains "No need to change", it means already in dual-side position mode
		if strings.Contains(err.Error(), "No need to change position side") {
			logger.Infof("  ✓ Account is already in dual-side position mode (Hedge Mode)")
			return nil
		}
		// Other errors are returned (but won't interrupt initialization in the caller)
		return err
	}

	logger.Infof("  ✓ Account has been switched to dual-side position mode (Hedge Mode)")
	logger.Infof("  ℹ️  Dual-side position mode allows holding both long and short positions simultaneously")
	return nil
}

// ValidateCopyGuardPositionMode queries the account instead of trusting the
// constructor's best-effort mode change. Side-specific closePosition stops are
// only safe in Binance Hedge Mode.
func (t *FuturesTrader) ValidateCopyGuardPositionMode() error {
	readMode := func() (*futures.PositionMode, error) {
		return binanceCallWithClockRetry(t, "get_position_mode", func() (*futures.PositionMode, error) {
			return t.client.NewGetPositionModeService().Do(context.Background())
		})
	}
	mode, err := readMode()
	if err != nil {
		return fmt.Errorf("query Binance position mode: %w", err)
	}
	if mode != nil && mode.DualSidePosition {
		return nil
	}
	if err = t.setDualSidePosition(); err != nil {
		return fmt.Errorf("set Binance Hedge Mode: %w", err)
	}
	mode, err = readMode()
	if err != nil {
		return fmt.Errorf("verify Binance Hedge Mode: %w", err)
	}
	if mode == nil || !mode.DualSidePosition {
		return fmt.Errorf("Binance account is not in Hedge Mode after mode change")
	}
	return nil
}

const (
	// binanceClockResyncInterval bounds how long a derived server-time offset may
	// be reused. Binance rejects any signed request whose timestamp is more than
	// 1000ms ahead of its own clock, and go-binance signs with
	// `timestamp = localNow - TimeOffset`. A permanently frozen offset therefore
	// turns a single NTP step correction into an outage that lasts until the
	// process restarts (observed in production: ~33k -1021 errors per hour while
	// the OS clock itself was correctly synchronised).
	binanceClockResyncInterval = 5 * time.Minute

	// binanceClockSafetyMarginMs biases our timestamps slightly behind Binance's
	// clock. Binance's tolerance is asymmetric: "behind" is accepted up to
	// recvWindow (5000ms by default) while "ahead" is hard-rejected at 1000ms
	// regardless of recvWindow, so the margin must be spent on the safe side.
	binanceClockSafetyMarginMs = 500

	// binanceClockMinResyncGap collapses a burst of concurrent -1021 failures
	// into a single recovery round trip.
	binanceClockMinResyncGap = 5 * time.Second
)

// syncBinanceServerTime derives the client's TimeOffset from Binance's clock.
//
// The offset is sampled at the midpoint of the request window so that symmetric
// network latency cancels out instead of being folded into the offset, then
// biased by binanceClockSafetyMarginMs onto the tolerated side.
func syncBinanceServerTime(client *futures.Client) error {
	start := time.Now()
	serverTime, err := client.NewServerTimeService().Do(context.Background())
	if err != nil {
		return err
	}
	end := time.Now()

	// Local clock reading at the instant Binance sampled its own clock.
	localAtServerSample := start.Add(end.Sub(start) / 2).UnixMilli()
	skew := localAtServerSample - serverTime

	// go-binance signs with localNow-TimeOffset, so offset = skew + margin lands
	// the signed timestamp `margin` milliseconds behind Binance's clock.
	client.TimeOffset = skew + binanceClockSafetyMarginMs
	logger.Infof("⏱ Binance server time synced | skew=%dms offset=%dms rtt=%dms",
		skew, client.TimeOffset, end.Sub(start).Milliseconds())
	return nil
}

// ensureClockFresh resyncs the server-time offset once it ages past
// binanceClockResyncInterval. Called from the signed read paths, which run at
// least once per cache window, so keeping them fresh also keeps order
// submission on the same client fresh.
func (t *FuturesTrader) ensureClockFresh() {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if !t.clock.lastSync.IsZero() && time.Since(t.clock.lastSync) < binanceClockResyncInterval {
		return
	}
	t.syncClockLocked("periodic")
}

// forceClockResync recovers from a rejected timestamp without waiting for the
// periodic window.
func (t *FuturesTrader) forceClockResync(reason string) {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	// A concurrent caller may already have resynced while we waited on the mutex.
	if !t.clock.lastSync.IsZero() && time.Since(t.clock.lastSync) < binanceClockMinResyncGap {
		return
	}
	t.syncClockLocked(reason)
}

func (t *FuturesTrader) syncClockLocked(reason string) {
	if err := syncBinanceServerTime(t.client); err != nil {
		// Keep the previous offset: it is at worst stale, whereas zeroing it would
		// hand every request the raw local clock.
		logger.Warnf("⚠️ Failed to sync Binance server time (reason=%s): %v", reason, err)
		return
	}
	t.clock.lastSync = time.Now()
}

// isBinanceTimestampRejected reports whether Binance refused the request because
// its timestamp fell outside the accepted window.
//
// Deliberately limited to -1021. -1022 ("signature not valid") is a credential
// or payload problem that a resync cannot fix, and retrying it would only mask
// the real cause.
func isBinanceTimestampRejected(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *common.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == -1021
	}
	return false
}

// binanceCallWithClockRetry runs a signed call, healing a stale server-time
// offset in place: refresh when due, and on a timestamp rejection resync once
// and retry so a frozen offset degrades to a transient blip rather than a
// standing outage.
func binanceCallWithClockRetry[T any](t *FuturesTrader, op string, fn func() (T, error)) (T, error) {
	t.ensureClockFresh()
	result, err := fn()
	if !isBinanceTimestampRejected(err) {
		return result, err
	}
	logger.Warnf("⚠️ Binance rejected request timestamp (%s), resyncing server time and retrying once", op)
	t.forceClockResync(op)
	return fn()
}

// GetBalance gets account balance (with cache)
func (t *FuturesTrader) GetBalance() (map[string]interface{}, error) {
	// First check if cache is valid
	t.balanceCacheMutex.RLock()
	if t.cachedBalance != nil && time.Since(t.balanceCacheTime) < t.cacheDuration {
		cacheAge := time.Since(t.balanceCacheTime)
		t.balanceCacheMutex.RUnlock()
		logger.Debugf("✓ Using cached account balance (cache age: %.1f seconds ago)", cacheAge.Seconds())
		return t.cachedBalance, nil
	}
	t.balanceCacheMutex.RUnlock()

	// Cache expired or doesn't exist, call API
	logger.Debugf("🔄 Cache expired, calling Binance API to get account balance...")
	account, err := binanceCallWithClockRetry(t, "get_account", func() (*futures.Account, error) {
		return t.client.NewGetAccountService().Do(context.Background())
	})
	if err != nil {
		logger.Warnf("❌ Binance API call failed: %v", err)
		return nil, fmt.Errorf("failed to get account info: %w", err)
	}

	result := make(map[string]interface{})
	result["totalWalletBalance"], _ = strconv.ParseFloat(account.TotalWalletBalance, 64)
	result["availableBalance"], _ = strconv.ParseFloat(account.AvailableBalance, 64)
	result["totalUnrealizedProfit"], _ = strconv.ParseFloat(account.TotalUnrealizedProfit, 64)

	logger.Infof("✓ Binance API returned: total balance=%s, available=%s, unrealized PnL=%s",
		account.TotalWalletBalance,
		account.AvailableBalance,
		account.TotalUnrealizedProfit)

	// Update cache
	t.balanceCacheMutex.Lock()
	t.cachedBalance = result
	t.balanceCacheTime = time.Now()
	t.balanceCacheMutex.Unlock()

	return result, nil
}

// GetPositions gets all positions (with cache)
func (t *FuturesTrader) GetPositions() ([]map[string]interface{}, error) {
	// First check if cache is valid
	t.positionsCacheMutex.RLock()
	if t.cachedPositions != nil && time.Since(t.positionsCacheTime) < t.cacheDuration {
		cacheAge := time.Since(t.positionsCacheTime)
		t.positionsCacheMutex.RUnlock()
		logger.Debugf("✓ Using cached position information (cache age: %.1f seconds ago)", cacheAge.Seconds())
		return t.cachedPositions, nil
	}
	t.positionsCacheMutex.RUnlock()

	// Cache expired or doesn't exist, call API
	logger.Debugf("🔄 Cache expired, calling Binance API to get position information...")
	positions, err := binanceCallWithClockRetry(t, "get_position_risk", func() ([]*futures.PositionRisk, error) {
		return t.client.NewGetPositionRiskService().Do(context.Background())
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}

	var result []map[string]interface{}
	for _, pos := range positions {
		posAmt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
		if posAmt == 0 {
			continue // Skip positions with zero amount
		}

		posMap := make(map[string]interface{})
		posMap["symbol"] = pos.Symbol
		posMap["positionAmt"], _ = strconv.ParseFloat(pos.PositionAmt, 64)
		posMap["entryPrice"], _ = strconv.ParseFloat(pos.EntryPrice, 64)
		posMap["markPrice"], _ = strconv.ParseFloat(pos.MarkPrice, 64)
		posMap["unRealizedProfit"], _ = strconv.ParseFloat(pos.UnRealizedProfit, 64)
		posMap["leverage"], _ = strconv.ParseFloat(pos.Leverage, 64)
		posMap["liquidationPrice"], _ = strconv.ParseFloat(pos.LiquidationPrice, 64)
		posMap["marginMode"] = pos.MarginType
		posMap["mgnMode"] = pos.MarginType
		// Note: Binance SDK doesn't expose updateTime field, will fallback to local tracking

		// Determine direction
		if posAmt > 0 {
			posMap["side"] = "long"
		} else {
			posMap["side"] = "short"
		}

		result = append(result, posMap)
	}

	// Update cache
	t.positionsCacheMutex.Lock()
	t.cachedPositions = result
	t.positionsCacheTime = time.Now()
	t.positionsCacheMutex.Unlock()

	return result, nil
}

// SetMarginMode sets margin mode
func (t *FuturesTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
	var marginType futures.MarginType
	if isCrossMargin {
		marginType = futures.MarginTypeCrossed
	} else {
		marginType = futures.MarginTypeIsolated
	}

	// Try to set margin mode
	err := t.client.NewChangeMarginTypeService().
		Symbol(symbol).
		MarginType(marginType).
		Do(context.Background())

	marginModeStr := "Cross Margin"
	if !isCrossMargin {
		marginModeStr = "Isolated Margin"
	}

	if err != nil {
		// If error message contains "No need to change", margin mode is already set to target value
		if contains(err.Error(), "No need to change margin type") {
			logger.Infof("  ✓ %s margin mode is already %s", symbol, marginModeStr)
			return nil
		}
		// If there is an open position, margin mode cannot be changed, but this doesn't affect trading
		if contains(err.Error(), "Margin type cannot be changed if there exists position") {
			logger.Infof("  ⚠️ %s has open positions, cannot change margin mode, continuing with current mode", symbol)
			return nil
		}
		// Detect Multi-Assets mode (error code -4168)
		if contains(err.Error(), "Multi-Assets mode") || contains(err.Error(), "-4168") || contains(err.Error(), "4168") {
			logger.Infof("  ⚠️ %s detected Multi-Assets mode, forcing Cross Margin mode", symbol)
			logger.Infof("  💡 Tip: To use Isolated Margin mode, please disable Multi-Assets mode in Binance")
			return nil
		}
		// Detect Unified Account API (Portfolio Margin)
		if contains(err.Error(), "unified") || contains(err.Error(), "portfolio") || contains(err.Error(), "Portfolio") {
			logger.Infof("  ❌ %s detected Unified Account API, unable to trade futures", symbol)
			return fmt.Errorf("please use 'Spot & Futures Trading' API permission, do not use 'Unified Account API'")
		}
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
		// Don't return error, let trading continue
		return nil
	}

	logger.Infof("  ✓ %s margin mode set to %s", symbol, marginModeStr)
	return nil
}

// SetLeverage sets leverage (with smart detection and cooldown period)
func (t *FuturesTrader) SetLeverage(symbol string, leverage int) error {
	// First try to get current leverage (from position information)
	currentLeverage := 0
	positions, err := t.GetPositions()
	if err == nil {
		for _, pos := range positions {
			if pos["symbol"] == symbol {
				if lev, ok := pos["leverage"].(float64); ok {
					currentLeverage = int(lev)
					break
				}
			}
		}
	}

	// If current leverage is already the target leverage, skip
	if currentLeverage == leverage && currentLeverage > 0 {
		logger.Infof("  ✓ %s leverage is already %dx, no need to change", symbol, leverage)
		return nil
	}

	// Change leverage
	_, err = t.client.NewChangeLeverageService().
		Symbol(symbol).
		Leverage(leverage).
		Do(context.Background())

	if err != nil {
		// If error message contains "No need to change", leverage is already the target value
		if contains(err.Error(), "No need to change") {
			logger.Infof("  ✓ %s leverage is already %dx", symbol, leverage)
			return nil
		}
		return fmt.Errorf("failed to set leverage: %w", err)
	}

	logger.Infof("  ✓ %s leverage changed to %dx", symbol, leverage)

	// Wait 5 seconds after changing leverage (to avoid cooldown period errors)
	logger.Infof("  ⏱ Waiting 5 seconds for cooldown period...")
	time.Sleep(5 * time.Second)

	return nil
}

// OpenLong opens a long position
func (t *FuturesTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	// First cancel all pending orders for this symbol (clean up old stop-loss and take-profit orders)
	if err := t.CancelAllOrders(symbol); err != nil {
		logger.Infof("  ⚠ Failed to cancel old pending orders (may not have any): %v", err)
	}

	// Set leverage
	if err := t.SetLeverage(symbol, leverage); err != nil {
		return nil, err
	}

	// Note: Margin mode should be set by the caller (AutoTrader) before opening position via SetMarginMode

	// Format quantity to correct precision
	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}

	// Check if formatted quantity is 0 (prevent rounding errors)
	quantityFloat, parseErr := strconv.ParseFloat(quantityStr, 64)
	if parseErr != nil || quantityFloat <= 0 {
		return nil, fmt.Errorf("position size too small, rounded to 0 (original: %.8f → formatted: %s). Suggest increasing position amount or selecting a lower-priced coin", quantity, quantityStr)
	}

	// Check minimum notional value (Binance requires at least 10 USDT)
	if err := t.CheckMinNotional(symbol, quantityFloat); err != nil {
		return nil, err
	}

	// Create market buy order (using br ID)
	order, err := t.client.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeBuy).
		PositionSide(futures.PositionSideTypeLong).
		Type(futures.OrderTypeMarket).
		Quantity(quantityStr).
		NewClientOrderID(getBrOrderID()).
		Do(context.Background())

	if err != nil {
		return nil, fmt.Errorf("failed to open long position: %w", err)
	}

	logger.Infof("✓ Opened long position successfully: %s quantity: %s", symbol, quantityStr)
	logger.Infof("  Order ID: %d", order.OrderID)

	result := make(map[string]interface{})
	result["orderId"] = order.OrderID
	result["symbol"] = order.Symbol
	result["status"] = order.Status
	return result, nil
}

// OpenShort opens a short position
func (t *FuturesTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	// First cancel all pending orders for this symbol (clean up old stop-loss and take-profit orders)
	if err := t.CancelAllOrders(symbol); err != nil {
		logger.Infof("  ⚠ Failed to cancel old pending orders (may not have any): %v", err)
	}

	// Set leverage
	if err := t.SetLeverage(symbol, leverage); err != nil {
		return nil, err
	}

	// Note: Margin mode should be set by the caller (AutoTrader) before opening position via SetMarginMode

	// Format quantity to correct precision
	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}

	// Check if formatted quantity is 0 (prevent rounding errors)
	quantityFloat, parseErr := strconv.ParseFloat(quantityStr, 64)
	if parseErr != nil || quantityFloat <= 0 {
		return nil, fmt.Errorf("position size too small, rounded to 0 (original: %.8f → formatted: %s). Suggest increasing position amount or selecting a lower-priced coin", quantity, quantityStr)
	}

	// Check minimum notional value (Binance requires at least 10 USDT)
	if err := t.CheckMinNotional(symbol, quantityFloat); err != nil {
		return nil, err
	}

	// Create market sell order (using br ID)
	order, err := t.client.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeSell).
		PositionSide(futures.PositionSideTypeShort).
		Type(futures.OrderTypeMarket).
		Quantity(quantityStr).
		NewClientOrderID(getBrOrderID()).
		Do(context.Background())

	if err != nil {
		return nil, fmt.Errorf("failed to open short position: %w", err)
	}

	logger.Infof("✓ Opened short position successfully: %s quantity: %s", symbol, quantityStr)
	logger.Infof("  Order ID: %d", order.OrderID)

	result := make(map[string]interface{})
	result["orderId"] = order.OrderID
	result["symbol"] = order.Symbol
	result["status"] = order.Status
	return result, nil
}

// CloseLong closes a long position
func (t *FuturesTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	// If quantity is 0, get current position quantity
	if quantity == 0 {
		positions, err := t.GetPositions()
		if err != nil {
			return nil, err
		}

		for _, pos := range positions {
			if pos["symbol"] == symbol && pos["side"] == "long" {
				quantity = pos["positionAmt"].(float64)
				break
			}
		}

		if quantity == 0 {
			return nil, fmt.Errorf("no long position found for %s", symbol)
		}
	}

	// Format quantity
	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}

	// Create market sell order (close long, using br ID)
	order, err := t.client.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeSell).
		PositionSide(futures.PositionSideTypeLong).
		Type(futures.OrderTypeMarket).
		Quantity(quantityStr).
		NewClientOrderID(getBrOrderID()).
		Do(context.Background())

	if err != nil {
		return nil, fmt.Errorf("failed to close long position: %w", err)
	}

	logger.Infof("✓ Closed long position successfully: %s quantity: %s", symbol, quantityStr)

	// After closing position, cancel all pending orders for this symbol (stop-loss and take-profit orders)
	if err := t.CancelAllOrders(symbol); err != nil {
		logger.Infof("  ⚠ Failed to cancel pending orders: %v", err)
	}

	result := make(map[string]interface{})
	result["orderId"] = order.OrderID
	result["symbol"] = order.Symbol
	result["status"] = order.Status
	return result, nil
}

// CloseShort closes a short position
func (t *FuturesTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	// If quantity is 0, get current position quantity
	if quantity == 0 {
		positions, err := t.GetPositions()
		if err != nil {
			return nil, err
		}

		for _, pos := range positions {
			if pos["symbol"] == symbol && pos["side"] == "short" {
				quantity = -pos["positionAmt"].(float64) // Short position quantity is negative, take absolute value
				break
			}
		}

		if quantity == 0 {
			return nil, fmt.Errorf("no short position found for %s", symbol)
		}
	}

	// Format quantity
	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}

	// Create market buy order (close short, using br ID)
	order, err := t.client.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeBuy).
		PositionSide(futures.PositionSideTypeShort).
		Type(futures.OrderTypeMarket).
		Quantity(quantityStr).
		NewClientOrderID(getBrOrderID()).
		Do(context.Background())

	if err != nil {
		return nil, fmt.Errorf("failed to close short position: %w", err)
	}

	logger.Infof("✓ Closed short position successfully: %s quantity: %s", symbol, quantityStr)

	// After closing position, cancel all pending orders for this symbol (stop-loss and take-profit orders)
	if err := t.CancelAllOrders(symbol); err != nil {
		logger.Infof("  ⚠ Failed to cancel pending orders: %v", err)
	}

	result := make(map[string]interface{})
	result["orderId"] = order.OrderID
	result["symbol"] = order.Symbol
	result["status"] = order.Status
	return result, nil
}

func (t *FuturesTrader) OpenLongPreservingOrders(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.OpenLongPreservingOrdersWithClientID(symbol, quantity, leverage, getBrOrderID())
}

func (t *FuturesTrader) PrepareCopyTradeOpen(symbol string, leverage int) error {
	return t.SetLeverage(symbol, leverage)
}

func (t *FuturesTrader) OpenShortPreservingOrders(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.OpenShortPreservingOrdersWithClientID(symbol, quantity, leverage, getBrOrderID())
}

func (t *FuturesTrader) CloseLongPreservingOrders(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.CloseLongPreservingOrdersWithClientID(symbol, quantity, getBrOrderID())
}

func (t *FuturesTrader) CloseShortPreservingOrders(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.CloseShortPreservingOrdersWithClientID(symbol, quantity, getBrOrderID())
}

func (t *FuturesTrader) OpenLongPreservingOrdersWithClientID(symbol string, quantity float64, leverage int, clientOrderID string) (map[string]interface{}, error) {
	return t.createCopyMarketOrder(symbol, quantity, leverage, futures.SideTypeBuy, futures.PositionSideTypeLong, clientOrderID, false, nil)
}

func (t *FuturesTrader) OpenShortPreservingOrdersWithClientID(symbol string, quantity float64, leverage int, clientOrderID string) (map[string]interface{}, error) {
	return t.createCopyMarketOrder(symbol, quantity, leverage, futures.SideTypeSell, futures.PositionSideTypeShort, clientOrderID, false, nil)
}

func (t *FuturesTrader) CloseLongPreservingOrdersWithClientID(symbol string, quantity float64, clientOrderID string) (map[string]interface{}, error) {
	return t.createCopyMarketOrder(symbol, quantity, 0, futures.SideTypeSell, futures.PositionSideTypeLong, clientOrderID, true, nil)
}

func (t *FuturesTrader) CloseShortPreservingOrdersWithClientID(symbol string, quantity float64, clientOrderID string) (map[string]interface{}, error) {
	return t.createCopyMarketOrder(symbol, quantity, 0, futures.SideTypeBuy, futures.PositionSideTypeShort, clientOrderID, true, nil)
}

func (t *FuturesTrader) ExecuteCopyTradeMarketOrder(request CopyTradeMarketOrderRequest) (map[string]interface{}, error) {
	switch request.Action {
	case "open_long":
		return t.createCopyMarketOrder(request.Symbol, request.Quantity, request.Leverage, futures.SideTypeBuy, futures.PositionSideTypeLong, request.ClientOrderID, false, request.BeforeSubmit, request.OnLeverageConfirmed)
	case "open_short":
		return t.createCopyMarketOrder(request.Symbol, request.Quantity, request.Leverage, futures.SideTypeSell, futures.PositionSideTypeShort, request.ClientOrderID, false, request.BeforeSubmit, request.OnLeverageConfirmed)
	case "close_long", "reduce_long":
		return t.createCopyMarketOrder(request.Symbol, request.Quantity, 0, futures.SideTypeSell, futures.PositionSideTypeLong, request.ClientOrderID, true, request.BeforeSubmit)
	case "close_short", "reduce_short":
		return t.createCopyMarketOrder(request.Symbol, request.Quantity, 0, futures.SideTypeBuy, futures.PositionSideTypeShort, request.ClientOrderID, true, request.BeforeSubmit)
	default:
		return nil, fmt.Errorf("unsupported Binance copy market action %q", request.Action)
	}
}

func (t *FuturesTrader) createCopyMarketOrder(symbol string, quantity float64, leverage int, side futures.SideType, positionSide futures.PositionSideType, clientOrderID string, closing bool, beforeSubmit func() error, leverageObservers ...func(int)) (map[string]interface{}, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	var instrument *binanceExecutionInstrument
	var err error
	if closing {
		// A reduction must keep using the exact execution symbol already stored
		// on the mapping. If the live catalog is temporarily unavailable, stale
		// metadata for that exact symbol is safe for quantity formatting; deriving
		// or substituting another quote asset is not.
		instrument, err = t.resolveBinanceInstrumentForReduction(symbol)
	} else {
		instrument, err = t.resolveBinanceInstrument(symbol)
	}
	if err != nil {
		return nil, err
	}
	if clientOrderID == "" {
		return nil, errors.New("copy trade client order id is required")
	}
	if len(clientOrderID) > 36 {
		return nil, fmt.Errorf("copy trade client order id exceeds Binance limit")
	}
	if existing, found, err := t.findOrderByClientID(symbol, clientOrderID); err != nil {
		return nil, fmt.Errorf("query idempotent Binance order: %w", err)
	} else if found {
		if beforeSubmit != nil {
			if err = beforeSubmit(); err != nil {
				return nil, err
			}
		}
		return adoptBinanceCopyOrder(existing)
	}
	if closing && quantity == 0 {
		positions, err := t.GetPositionsFresh()
		if err != nil {
			return nil, err
		}
		wantedSide := strings.ToLower(string(positionSide))
		for _, pos := range positions {
			if binanceMapString(pos, "symbol") == symbol && binanceMapString(pos, "side") == wantedSide {
				quantity = math.Abs(binanceMapFloat(pos, "positionAmt"))
				break
			}
		}
		if quantity == 0 {
			return nil, fmt.Errorf("no %s position found for %s", wantedSide, symbol)
		}
	}
	if !closing {
		if err := t.SetLeverage(symbol, leverage); err != nil {
			return nil, err
		}
		notifyLeverageConfirmed(leverage, leverageObservers)
	}
	quantityStr, err := formatBinanceMarketQuantity(instrument, symbol, math.Abs(quantity))
	if err != nil {
		return nil, err
	}
	if !closing {
		quantityFloat, _ := strconv.ParseFloat(quantityStr, 64)
		if err := t.CheckMinNotional(symbol, quantityFloat); err != nil {
			return nil, err
		}
	}
	if beforeSubmit != nil {
		if err := beforeSubmit(); err != nil {
			return nil, err
		}
	}
	order, err := t.client.NewCreateOrderService().Symbol(symbol).Side(side).PositionSide(positionSide).
		Type(futures.OrderTypeMarket).Quantity(quantityStr).NewClientOrderID(clientOrderID).Do(context.Background())
	if err != nil {
		// An ambiguous response may still have reached Binance. Resolve by the
		// stable client ID before surfacing an error, never submit a second order.
		if existing, found, queryErr := t.findOrderByClientID(symbol, clientOrderID); queryErr == nil && found {
			result, adoptionErr := adoptBinanceCopyOrder(existing)
			if adoptionErr == nil {
				return result, nil
			}
			return nil, fmt.Errorf("create Binance copy trade order returned an ambiguous error and the client id is unusable: %w", adoptionErr)
		}
		return nil, fmt.Errorf("create Binance copy trade order: %w", err)
	}
	t.invalidatePositionsCache()
	return binanceCreateOrderResult(order), nil
}

// adoptBinanceCopyOrder validates the result of an idempotency lookup. A live
// order or an order with a real execution can be safely adopted. A terminal
// zero-fill order permanently consumed the client ID but did not perform the
// decision, so treating it as success would silently lose the copy action.
func adoptBinanceCopyOrder(order *futures.Order) (map[string]interface{}, error) {
	if order == nil {
		return nil, errors.New("Binance client order lookup returned no order")
	}
	executedQty, err := strconv.ParseFloat(strings.TrimSpace(order.ExecutedQuantity), 64)
	if err != nil && strings.TrimSpace(order.ExecutedQuantity) != "" {
		return nil, fmt.Errorf("Binance client order %s has invalid executed quantity %q", order.ClientOrderID, order.ExecutedQuantity)
	}
	if executedQty > 0 {
		return binanceOrderResult(order), nil
	}
	if order.Status == futures.OrderStatusTypeNew {
		return binanceOrderResult(order), nil
	}
	return nil, fmt.Errorf("Binance client order %s is %s with zero executed quantity", order.ClientOrderID, order.Status)
}

func binanceMapString(values map[string]interface{}, key string) string {
	value, _ := values[key].(string)
	return value
}

func binanceMapFloat(values map[string]interface{}, key string) float64 {
	switch value := values[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case string:
		parsed, _ := strconv.ParseFloat(value, 64)
		return parsed
	default:
		return 0
	}
}

func (t *FuturesTrader) findOrderByClientID(symbol, clientOrderID string) (*futures.Order, bool, error) {
	order, err := t.client.NewGetOrderService().Symbol(symbol).OrigClientOrderID(clientOrderID).Do(context.Background())
	if err != nil {
		if isBinanceOrderNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return order, true, nil
}

func isBinanceOrderNotFound(err error) bool {
	var apiErr *common.APIError
	if errors.As(err, &apiErr) && (apiErr.Code == -2013 || apiErr.Code == -2011) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "order does not exist") || strings.Contains(text, "unknown order")
}

func binanceOrderResult(order *futures.Order) map[string]interface{} {
	avgPrice, _ := strconv.ParseFloat(order.AvgPrice, 64)
	executedQty, _ := strconv.ParseFloat(order.ExecutedQuantity, 64)
	return map[string]interface{}{
		"orderId": strconv.FormatInt(order.OrderID, 10), "clOrdId": order.ClientOrderID,
		"symbol": order.Symbol, "status": string(order.Status), "avgPrice": avgPrice,
		"executedQty": executedQty, "side": string(order.Side), "type": string(order.Type),
		"time": order.Time, "updateTime": order.UpdateTime, "commission": 0.0,
	}
}

func binanceCreateOrderResult(order *futures.CreateOrderResponse) map[string]interface{} {
	avgPrice, _ := strconv.ParseFloat(order.AvgPrice, 64)
	executedQty, _ := strconv.ParseFloat(order.ExecutedQuantity, 64)
	return map[string]interface{}{
		"orderId": strconv.FormatInt(order.OrderID, 10), "clOrdId": order.ClientOrderID,
		"symbol": order.Symbol, "status": string(order.Status), "avgPrice": avgPrice,
		"executedQty": executedQty, "side": string(order.Side), "type": string(order.Type),
		"updateTime": order.UpdateTime, "commission": 0.0,
	}
}

func (t *FuturesTrader) invalidatePositionsCache() {
	t.positionsCacheMutex.Lock()
	t.cachedPositions = nil
	t.positionsCacheTime = time.Time{}
	t.positionsCacheMutex.Unlock()
}

func (t *FuturesTrader) GetPositionsFresh() ([]map[string]interface{}, error) {
	t.invalidatePositionsCache()
	return t.GetPositions()
}

func (t *FuturesTrader) ProtectiveStopCoverageMode() string {
	return ProtectiveStopCoverageCloseAll
}

func (t *FuturesTrader) PlaceProtectiveStop(req ProtectiveStopRequest) (*ProtectiveStopOrder, error) {
	inst, err := t.resolveBinanceInstrument(req.Symbol)
	if err != nil {
		return nil, err
	}
	if req.ClientID == "" || len(req.ClientID) > 36 {
		return nil, fmt.Errorf("invalid Binance protective stop client id")
	}
	// Binance permanently reserves client algo ids, including terminal orders.
	// Walk a deterministic chain derived from the terminal exchange order so a
	// retry adopts the same live replacement instead of inventing a new order.
	baseClientID := req.ClientID
	for attempt := 0; attempt < 4; attempt++ {
		existing, err := t.GetProtectiveStopByClientID(req.ClientID, req.Symbol)
		if err == nil {
			if existing != nil && strings.EqualFold(existing.State, "live") {
				return existing, nil
			}
			if existing == nil {
				return nil, fmt.Errorf("query Binance protective stop returned an empty order")
			}
			req.ClientID = deriveBinanceProtectiveClientID(baseClientID, fmt.Sprintf("terminal|%s|%s|%s|%.12f|%.12f|%s|%d", existing.AlgoID, existing.State, req.PositionSide, req.Quantity, req.TriggerPrice, req.TriggerType, attempt))
			continue
		}
		if !errors.Is(err, ErrProtectiveStopNotFound) {
			return nil, fmt.Errorf("query Binance protective stop before create: %w", err)
		}
		break
	}
	positionSide, side, err := binanceProtectiveSides(req.PositionSide)
	if err != nil {
		return nil, err
	}
	workingType := futures.WorkingTypeContractPrice
	if strings.EqualFold(req.TriggerType, "mark") || strings.EqualFold(req.TriggerType, "mark_price") {
		workingType = futures.WorkingTypeMarkPrice
	}
	created, err := t.client.NewCreateAlgoOrderService().Symbol(strings.ToUpper(req.Symbol)).Side(side).
		PositionSide(positionSide).Type(futures.AlgoOrderTypeStopMarket).
		TriggerPrice(QuantizeAndFormatPrice(req.TriggerPrice, inst.PriceTickSize, protectiveStopRoundsDown(req.PositionSide))).WorkingType(workingType).
		ClosePosition(true).ClientAlgoId(req.ClientID).Do(context.Background())
	if err != nil {
		if existing, queryErr := t.GetProtectiveStopByClientID(req.ClientID, req.Symbol); queryErr == nil && existing != nil && strings.EqualFold(existing.State, "live") {
			return existing, nil
		}
		return nil, fmt.Errorf("create Binance protective stop: %w", err)
	}
	quantity := req.Quantity
	return &ProtectiveStopOrder{
		AlgoID: strconv.FormatInt(created.AlgoId, 10), ClientID: created.ClientAlgoId, Symbol: created.Symbol,
		PositionSide: strings.ToLower(string(created.PositionSide)), MarginMode: req.MarginMode,
		Quantity: quantity, TriggerPrice: parseBinanceFloat(created.TriggerPrice), TriggerType: req.TriggerType,
		CoverageMode: ProtectiveStopCoverageCloseAll,
		State:        normalizeBinanceAlgoState(created.AlgoStatus),
	}, nil
}

// AmendProtectiveStop is deliberately unsupported: Binance Algo orders have
// no amend API. Copy Guard calls EnsureProtectiveStop, which safely creates and
// verifies a replacement before retiring the old order.
func (t *FuturesTrader) AmendProtectiveStop(_ string, _ ProtectiveStopRequest) error {
	return errors.New("Binance protective stops require safe replacement")
}

func (t *FuturesTrader) EnsureProtectiveStop(existing *ProtectiveStopOrder, req ProtectiveStopRequest) (*ProtectiveStopEnsureResult, error) {
	if existing == nil || existing.AlgoID == "" {
		current, err := t.PlaceProtectiveStop(req)
		return &ProtectiveStopEnsureResult{Current: current}, err
	}
	if strings.TrimSpace(req.ClientID) == "" {
		return &ProtectiveStopEnsureResult{Retiring: existing}, errors.New("Binance replacement protective stop client id is required")
	}
	req.ClientID = deriveBinanceProtectiveClientID(req.ClientID, fmt.Sprintf("replace|%s|%s|%s|%.12f|%.12f|%s", existing.AlgoID, req.Symbol, req.PositionSide, req.Quantity, req.TriggerPrice, req.TriggerType))
	current, err := t.PlaceProtectiveStop(req)
	if err != nil {
		return &ProtectiveStopEnsureResult{Retiring: existing}, fmt.Errorf("create replacement protective stop: %w", err)
	}
	if current == nil || current.AlgoID == "" || current.AlgoID == existing.AlgoID {
		return &ProtectiveStopEnsureResult{Retiring: existing}, fmt.Errorf("replacement protective stop did not create a distinct exchange order")
	}
	verified, err := t.GetProtectiveStop(current.AlgoID, req.Symbol)
	if err != nil || verified == nil || !strings.EqualFold(verified.State, "live") {
		if err == nil {
			if verified == nil {
				err = errors.New("replacement query returned no order")
			} else {
				err = fmt.Errorf("replacement state=%s", verified.State)
			}
		}
		// The new identifier is deterministic and can be adopted on the next
		// retry, but it must not enter replacement_pending until a query has
		// confirmed it live. Otherwise the recovery loop could retire the last
		// known-good stop while the replacement is still unknown.
		return &ProtectiveStopEnsureResult{Current: current, Retiring: existing}, fmt.Errorf("replacement protective stop not confirmed live: %w", err)
	}
	// Do not cancel the old order here. The integration must first persist both
	// identifiers as replacement_pending, then retire the old order. This closes
	// the crash window between exchange mutation and durable recovery state.
	return &ProtectiveStopEnsureResult{Current: verified, Retiring: existing, ReplacementPending: true}, nil
}

func deriveBinanceProtectiveClientID(base, material string) string {
	digest := sha256.Sum256([]byte(material))
	suffix := fmt.Sprintf("r%x", digest[:5])
	if len(base)+len(suffix) > 36 {
		base = base[:36-len(suffix)]
	}
	return base + suffix
}

func binanceProtectiveSides(side string) (futures.PositionSideType, futures.SideType, error) {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "long":
		return futures.PositionSideTypeLong, futures.SideTypeSell, nil
	case "short":
		return futures.PositionSideTypeShort, futures.SideTypeBuy, nil
	default:
		return "", "", fmt.Errorf("invalid protective position side %q", side)
	}
}

func (t *FuturesTrader) GetProtectiveStop(algoID, symbol string) (*ProtectiveStopOrder, error) {
	id, err := strconv.ParseInt(algoID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid Binance algo id %q", algoID)
	}
	order, err := t.client.NewGetAlgoOrderService().AlgoID(id).Do(context.Background())
	if err != nil {
		if isBinanceOrderNotFound(err) {
			return nil, fmt.Errorf("Binance algo %s: %w", algoID, ErrProtectiveStopNotFound)
		}
		return nil, err
	}
	return t.binanceAlgoToProtective(order, symbol)
}

func (t *FuturesTrader) GetProtectiveStopByClientID(clientID, symbol string) (*ProtectiveStopOrder, error) {
	order, err := t.client.NewGetAlgoOrderService().ClientAlgoID(clientID).Do(context.Background())
	if err != nil {
		if isBinanceOrderNotFound(err) {
			return nil, fmt.Errorf("Binance algo client %s: %w", clientID, ErrProtectiveStopNotFound)
		}
		return nil, err
	}
	return t.binanceAlgoToProtective(order, symbol)
}

func (t *FuturesTrader) binanceAlgoToProtective(order *futures.GetAlgoOrderResp, expectedSymbol string) (*ProtectiveStopOrder, error) {
	if order == nil {
		return nil, errors.New("empty Binance algo order")
	}
	if expectedSymbol != "" && !strings.EqualFold(order.Symbol, expectedSymbol) {
		return nil, fmt.Errorf("Binance algo symbol mismatch: got %s want %s", order.Symbol, expectedSymbol)
	}
	quantity := parseBinanceFloat(order.Quantity)
	coverageMode := ProtectiveStopCoverageExactQuantity
	if order.ClosePosition {
		coverageMode = ProtectiveStopCoverageCloseAll
	}
	triggerType := "contract"
	if order.WorkingType == futures.WorkingTypeMarkPrice {
		triggerType = "mark"
	}
	return &ProtectiveStopOrder{
		AlgoID: strconv.FormatInt(order.AlgoId, 10), ClientID: order.ClientAlgoId, Symbol: order.Symbol,
		PositionSide: strings.ToLower(string(order.PositionSide)), Quantity: quantity,
		TriggerPrice: parseBinanceFloat(order.TriggerPrice), TriggerType: triggerType,
		CoverageMode: coverageMode, State: normalizeBinanceAlgoState(order.AlgoStatus), ActualOrderID: order.ActualOrderId,
	}, nil
}

func (t *FuturesTrader) CancelProtectiveStop(algoID, _ string) error {
	id, err := strconv.ParseInt(algoID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid Binance algo id %q", algoID)
	}
	_, err = t.client.NewCancelAlgoOrderService().AlgoID(id).Do(context.Background())
	if err != nil && isBinanceOrderNotFound(err) {
		return fmt.Errorf("Binance algo %s: %w", algoID, ErrProtectiveStopNotFound)
	}
	return err
}

func normalizeBinanceAlgoState(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "NEW", "ACCEPTED", "WORKING":
		return "live"
	case "TRIGGERED", "FINISHED", "FILLED":
		return "effective"
	case "CANCELED", "CANCELLED":
		return "canceled"
	case "FAILED", "EXPIRED", "REJECTED":
		return "order_failed"
	default:
		return "unknown"
	}
}

func parseBinanceFloat(value string) float64 {
	parsed, _ := strconv.ParseFloat(value, 64)
	return parsed
}

// GetPendingOrdersFresh returns regular and Algo orders from independent
// authoritative endpoints. A failure from either endpoint makes the snapshot
// unknown; archive reconciliation must never interpret it as empty.
func (t *FuturesTrader) GetPendingOrdersFresh() ([]PendingOrderSnapshot, error) {
	orders, err := t.client.NewListOpenOrdersService().Do(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list Binance regular pending orders: %w", err)
	}
	algoOrders, err := t.client.NewListOpenAlgoOrdersService().Do(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list Binance algo pending orders: %w", err)
	}
	snapshots := make([]PendingOrderSnapshot, 0, len(orders)+len(algoOrders))
	for _, order := range orders {
		orderType := strings.ToUpper(string(order.Type))
		protective := strings.Contains(orderType, "STOP") || strings.Contains(orderType, "TAKE_PROFIT")
		snapshots = append(snapshots, PendingOrderSnapshot{
			ID: strconv.FormatInt(order.OrderID, 10), Symbol: order.Symbol,
			Status: string(order.Status), Protective: protective,
		})
	}
	for _, order := range algoOrders {
		snapshots = append(snapshots, PendingOrderSnapshot{
			ID: strconv.FormatInt(order.AlgoId, 10), Symbol: order.Symbol,
			Status: order.AlgoStatus, Protective: order.SlTriggerPrice != "" ||
				order.TpTriggerPrice != "" ||
				strings.Contains(strings.ToUpper(string(order.OrderType)), "STOP") ||
				strings.Contains(strings.ToUpper(string(order.OrderType)), "TAKE_PROFIT"),
		})
	}
	return snapshots, nil
}

// CancelStopLossOrders cancels only stop-loss orders (doesn't affect take-profit orders)
// Now uses both legacy API and new Algo Order API
func (t *FuturesTrader) CancelStopLossOrders(symbol string) error {
	canceledCount := 0
	var cancelErrors []error

	// 1. Cancel legacy stop-loss orders
	orders, err := t.client.NewListOpenOrdersService().
		Symbol(symbol).
		Do(context.Background())

	if err == nil {
		for _, order := range orders {
			orderType := string(order.Type)

			// Only cancel stop-loss orders (don't cancel take-profit orders)
			// Use string comparison since OrderType constants were removed in v2.8.9
			if orderType == "STOP_MARKET" || orderType == "STOP" {
				_, err := t.client.NewCancelOrderService().
					Symbol(symbol).
					OrderID(order.OrderID).
					Do(context.Background())

				if err != nil {
					errMsg := fmt.Sprintf("Order ID %d: %v", order.OrderID, err)
					cancelErrors = append(cancelErrors, fmt.Errorf("%s", errMsg))
					logger.Infof("  ⚠ Failed to cancel legacy stop-loss order: %s", errMsg)
					continue
				}

				canceledCount++
				logger.Infof("  ✓ Canceled legacy stop-loss order (Order ID: %d, Type: %s, Side: %s)", order.OrderID, orderType, order.PositionSide)
			}
		}
	}

	// 2. Cancel Algo stop-loss orders
	algoOrders, err := t.client.NewListOpenAlgoOrdersService().
		Symbol(symbol).
		Do(context.Background())

	if err == nil {
		for _, algoOrder := range algoOrders {
			// Only cancel stop-loss orders
			if algoOrder.OrderType == futures.AlgoOrderTypeStopMarket || algoOrder.OrderType == futures.AlgoOrderTypeStop {
				_, err := t.client.NewCancelAlgoOrderService().
					AlgoID(algoOrder.AlgoId).
					Do(context.Background())

				if err != nil {
					errMsg := fmt.Sprintf("Algo ID %d: %v", algoOrder.AlgoId, err)
					cancelErrors = append(cancelErrors, fmt.Errorf("%s", errMsg))
					logger.Infof("  ⚠ Failed to cancel Algo stop-loss order: %s", errMsg)
					continue
				}

				canceledCount++
				logger.Infof("  ✓ Canceled Algo stop-loss order (Algo ID: %d, Type: %s)", algoOrder.AlgoId, algoOrder.OrderType)
			}
		}
	}

	if canceledCount == 0 && len(cancelErrors) == 0 {
		logger.Infof("  ℹ %s has no stop-loss orders to cancel", symbol)
	} else if canceledCount > 0 {
		logger.Infof("  ✓ Canceled %d stop-loss order(s) for %s", canceledCount, symbol)
	}

	// If all cancellations failed, return error
	if len(cancelErrors) > 0 && canceledCount == 0 {
		return fmt.Errorf("failed to cancel stop-loss orders: %v", cancelErrors)
	}

	return nil
}

// CancelTakeProfitOrders cancels only take-profit orders (doesn't affect stop-loss orders)
// Now uses both legacy API and new Algo Order API
func (t *FuturesTrader) CancelTakeProfitOrders(symbol string) error {
	canceledCount := 0
	var cancelErrors []error

	// 1. Cancel legacy take-profit orders
	orders, err := t.client.NewListOpenOrdersService().
		Symbol(symbol).
		Do(context.Background())

	if err == nil {
		for _, order := range orders {
			orderType := string(order.Type)

			// Only cancel take-profit orders (don't cancel stop-loss orders)
			// Use string comparison since OrderType constants were removed in v2.8.9
			if orderType == "TAKE_PROFIT_MARKET" || orderType == "TAKE_PROFIT" {
				_, err := t.client.NewCancelOrderService().
					Symbol(symbol).
					OrderID(order.OrderID).
					Do(context.Background())

				if err != nil {
					errMsg := fmt.Sprintf("Order ID %d: %v", order.OrderID, err)
					cancelErrors = append(cancelErrors, fmt.Errorf("%s", errMsg))
					logger.Infof("  ⚠ Failed to cancel legacy take-profit order: %s", errMsg)
					continue
				}

				canceledCount++
				logger.Infof("  ✓ Canceled legacy take-profit order (Order ID: %d, Type: %s, Side: %s)", order.OrderID, orderType, order.PositionSide)
			}
		}
	}

	// 2. Cancel Algo take-profit orders
	algoOrders, err := t.client.NewListOpenAlgoOrdersService().
		Symbol(symbol).
		Do(context.Background())

	if err == nil {
		for _, algoOrder := range algoOrders {
			// Only cancel take-profit orders
			if algoOrder.OrderType == futures.AlgoOrderTypeTakeProfitMarket || algoOrder.OrderType == futures.AlgoOrderTypeTakeProfit {
				_, err := t.client.NewCancelAlgoOrderService().
					AlgoID(algoOrder.AlgoId).
					Do(context.Background())

				if err != nil {
					errMsg := fmt.Sprintf("Algo ID %d: %v", algoOrder.AlgoId, err)
					cancelErrors = append(cancelErrors, fmt.Errorf("%s", errMsg))
					logger.Infof("  ⚠ Failed to cancel Algo take-profit order: %s", errMsg)
					continue
				}

				canceledCount++
				logger.Infof("  ✓ Canceled Algo take-profit order (Algo ID: %d, Type: %s)", algoOrder.AlgoId, algoOrder.OrderType)
			}
		}
	}

	if canceledCount == 0 && len(cancelErrors) == 0 {
		logger.Infof("  ℹ %s has no take-profit orders to cancel", symbol)
	} else if canceledCount > 0 {
		logger.Infof("  ✓ Canceled %d take-profit order(s) for %s", canceledCount, symbol)
	}

	// If all cancellations failed, return error
	if len(cancelErrors) > 0 && canceledCount == 0 {
		return fmt.Errorf("failed to cancel take-profit orders: %v", cancelErrors)
	}

	return nil
}

// CancelAllOrders cancels all pending orders for this symbol
// Now uses both legacy API and new Algo Order API
func (t *FuturesTrader) CancelAllOrders(symbol string) error {
	// 1. Cancel all legacy orders
	err := t.client.NewCancelAllOpenOrdersService().
		Symbol(symbol).
		Do(context.Background())

	if err != nil {
		logger.Infof("  ⚠ Failed to cancel legacy orders: %v", err)
	} else {
		logger.Infof("  ✓ Canceled all legacy pending orders for %s", symbol)
	}

	// 2. Cancel all Algo orders
	err = t.client.NewCancelAllAlgoOpenOrdersService().
		Symbol(symbol).
		Do(context.Background())

	if err != nil {
		// Ignore "no algo orders" error
		if !contains(err.Error(), "no algo") && !contains(err.Error(), "No algo") {
			logger.Infof("  ⚠ Failed to cancel Algo orders: %v", err)
		}
	} else {
		logger.Infof("  ✓ Canceled all Algo orders for %s", symbol)
	}

	return nil
}

// CancelStopOrders cancels take-profit/stop-loss orders for this symbol (used to adjust TP/SL positions)
// Now uses both legacy API and new Algo Order API (Binance migrated stop orders to Algo system)
func (t *FuturesTrader) CancelStopOrders(symbol string) error {
	canceledCount := 0

	// 1. Cancel legacy stop orders (for backward compatibility)
	orders, err := t.client.NewListOpenOrdersService().
		Symbol(symbol).
		Do(context.Background())

	if err == nil {
		for _, order := range orders {
			orderType := string(order.Type)

			// Only cancel stop-loss and take-profit orders
			// Use string comparison since OrderType constants were removed in v2.8.9
			if orderType == "STOP_MARKET" ||
				orderType == "TAKE_PROFIT_MARKET" ||
				orderType == "STOP" ||
				orderType == "TAKE_PROFIT" {

				_, err := t.client.NewCancelOrderService().
					Symbol(symbol).
					OrderID(order.OrderID).
					Do(context.Background())

				if err != nil {
					logger.Infof("  ⚠ Failed to cancel legacy order %d: %v", order.OrderID, err)
					continue
				}

				canceledCount++
				logger.Infof("  ✓ Canceled legacy stop order for %s (Order ID: %d, Type: %s)",
					symbol, order.OrderID, orderType)
			}
		}
	}

	// 2. Cancel Algo orders (new API)
	err = t.client.NewCancelAllAlgoOpenOrdersService().
		Symbol(symbol).
		Do(context.Background())

	if err != nil {
		// Ignore "no algo orders" error
		if !contains(err.Error(), "no algo") && !contains(err.Error(), "No algo") {
			logger.Infof("  ⚠ Failed to cancel Algo orders: %v", err)
		}
	} else {
		logger.Infof("  ✓ Canceled all Algo orders for %s", symbol)
		canceledCount++
	}

	if canceledCount == 0 {
		logger.Infof("  ℹ %s has no take-profit/stop-loss orders to cancel", symbol)
	}

	return nil
}

// GetMarketPrice gets market price
func (t *FuturesTrader) GetMarketPrice(symbol string) (float64, error) {
	prices, err := t.client.NewListPricesService().Symbol(symbol).Do(context.Background())
	if err != nil {
		return 0, fmt.Errorf("failed to get price: %w", err)
	}

	if len(prices) == 0 {
		return 0, fmt.Errorf("price not found")
	}

	price, err := strconv.ParseFloat(prices[0].Price, 64)
	if err != nil {
		return 0, err
	}

	return price, nil
}

// GetMarkPriceObservation reads Binance's dedicated mark endpoint and retains
// venue time so live fixed-stop checks can reject stale or missing quotes.
func (t *FuturesTrader) GetMarkPriceObservation(symbol string) (MarkPriceObservation, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return MarkPriceObservation{}, fmt.Errorf("empty Binance mark-price symbol")
	}
	rows, err := t.client.NewPremiumIndexService().Symbol(symbol).Do(context.Background())
	if err != nil {
		return MarkPriceObservation{}, fmt.Errorf("query Binance mark price: %w", err)
	}
	for _, row := range rows {
		if row == nil || !strings.EqualFold(row.Symbol, symbol) {
			continue
		}
		price, parseErr := strconv.ParseFloat(row.MarkPrice, 64)
		if parseErr != nil || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return MarkPriceObservation{}, fmt.Errorf("invalid Binance mark price for %s", symbol)
		}
		return MarkPriceObservation{Price: price, ObservedAt: time.UnixMilli(row.Time), Source: "binance_mark_endpoint"}, nil
	}
	return MarkPriceObservation{}, fmt.Errorf("Binance mark price not found for %s", symbol)
}

// GetMarkPriceHistory reads completed one-minute mark klines. It is report-only
// evidence for Copy Guard shadow gap repair and never participates in live
// order triggering.
func (t *FuturesTrader) GetMarkPriceHistory(symbol string, from, to time.Time) ([]MarkPriceCandle, error) {
	if strings.TrimSpace(symbol) == "" || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, fmt.Errorf("invalid Binance mark-price history range")
	}
	cursor := from.UTC()
	end := to.UTC()
	result := make([]MarkPriceCandle, 0)
	for page := 0; cursor.Before(end); page++ {
		if page >= 1000 {
			return nil, fmt.Errorf("Binance mark-price history exceeded 1000 pages")
		}
		rows, err := t.client.NewMarkPriceKlinesService().Symbol(strings.ToUpper(symbol)).Interval("1m").
			StartTime(cursor.UnixMilli()).EndTime(end.UnixMilli()).Limit(1500).Do(context.Background())
		if err != nil {
			return nil, fmt.Errorf("query Binance mark-price history: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		progress := cursor
		for _, row := range rows {
			if row == nil {
				continue
			}
			openAt := time.UnixMilli(row.OpenTime).UTC()
			closeAt := time.UnixMilli(row.CloseTime).UTC()
			if closeAt.After(end) || closeAt.Before(from) {
				continue
			}
			high, highErr := strconv.ParseFloat(row.High, 64)
			low, lowErr := strconv.ParseFloat(row.Low, 64)
			closePrice, closeErr := strconv.ParseFloat(row.Close, 64)
			if highErr != nil || lowErr != nil || closeErr != nil || high <= 0 || low <= 0 || closePrice <= 0 || low > high {
				return nil, fmt.Errorf("invalid Binance mark-price kline at %d", row.OpenTime)
			}
			result = append(result, MarkPriceCandle{OpenTime: openAt, CloseTime: closeAt, High: high, Low: low, Close: closePrice})
			if next := closeAt.Add(time.Millisecond); next.After(progress) {
				progress = next
			}
		}
		if !progress.After(cursor) {
			break
		}
		cursor = progress
		if len(rows) < 1500 {
			break
		}
	}
	return result, nil
}

// CalculatePositionSize calculates position size
func (t *FuturesTrader) CalculatePositionSize(balance, riskPercent, price float64, leverage int) float64 {
	riskAmount := balance * (riskPercent / 100.0)
	positionValue := riskAmount * float64(leverage)
	quantity := positionValue / price
	return quantity
}

// formatTriggerPrice serializes an algo trigger price on the symbol's tick
// grid. When the instrument catalog is unavailable it keeps the historical
// fixed-width form rather than failing an order the AI trader expects to place.
func (t *FuturesTrader) formatTriggerPrice(symbol string, price float64, roundDown bool) string {
	inst, err := t.resolveBinanceInstrument(symbol)
	if err != nil || inst == nil || inst.PriceTickSize <= 0 {
		logger.Warnf("⚠️ 无法解析 %s 价格步长，触发价沿用固定精度: %v", symbol, err)
		return fmt.Sprintf("%.8f", price)
	}
	return QuantizeAndFormatPrice(price, inst.PriceTickSize, roundDown)
}

// SetStopLoss sets stop-loss order using new Algo Order API
// Binance has migrated stop orders to Algo Order system (error -4120 STOP_ORDER_SWITCH_ALGO)
func (t *FuturesTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	var side futures.SideType
	var posSide futures.PositionSideType

	if positionSide == "LONG" {
		side = futures.SideTypeSell
		posSide = futures.PositionSideTypeLong
	} else {
		side = futures.SideTypeBuy
		posSide = futures.PositionSideTypeShort
	}

	// Use new Algo Order API
	_, err := t.client.NewCreateAlgoOrderService().
		Symbol(symbol).
		Side(side).
		PositionSide(posSide).
		Type(futures.AlgoOrderTypeStopMarket).
		TriggerPrice(t.formatTriggerPrice(symbol, stopPrice, positionSide == "SHORT")).
		WorkingType(futures.WorkingTypeContractPrice).
		ClosePosition(true).
		ClientAlgoId(getBrOrderID()).
		Do(context.Background())

	if err != nil {
		return fmt.Errorf("failed to set stop-loss: %w", err)
	}

	logger.Infof("  Stop-loss price set (Algo Order): %.4f", stopPrice)
	return nil
}

// SetTakeProfit sets take-profit order using new Algo Order API
// Binance has migrated stop orders to Algo Order system (error -4120 STOP_ORDER_SWITCH_ALGO)
func (t *FuturesTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	var side futures.SideType
	var posSide futures.PositionSideType

	if positionSide == "LONG" {
		side = futures.SideTypeSell
		posSide = futures.PositionSideTypeLong
	} else {
		side = futures.SideTypeBuy
		posSide = futures.PositionSideTypeShort
	}

	// Use new Algo Order API
	_, err := t.client.NewCreateAlgoOrderService().
		Symbol(symbol).
		Side(side).
		PositionSide(posSide).
		Type(futures.AlgoOrderTypeTakeProfitMarket).
		TriggerPrice(t.formatTriggerPrice(symbol, takeProfitPrice, positionSide != "SHORT")).
		WorkingType(futures.WorkingTypeContractPrice).
		ClosePosition(true).
		ClientAlgoId(getBrOrderID()).
		Do(context.Background())

	if err != nil {
		return fmt.Errorf("failed to set take-profit: %w", err)
	}

	logger.Infof("  Take-profit price set (Algo Order): %.4f", takeProfitPrice)
	return nil
}

// GetMinNotional gets minimum notional value (Binance requirement)
func (t *FuturesTrader) GetMinNotional(symbol string) float64 {
	inst, err := t.resolveBinanceInstrument(symbol)
	if err != nil {
		return 0
	}
	return inst.MinNotional
}

// CheckMinNotional checks if order meets minimum notional value requirement
func (t *FuturesTrader) CheckMinNotional(symbol string, quantity float64) error {
	inst, err := t.resolveBinanceInstrument(symbol)
	if err != nil {
		return err
	}
	price, err := t.GetMarketPrice(symbol)
	if err != nil {
		return fmt.Errorf("failed to get market price: %w", err)
	}

	notionalValue := quantity * price
	minNotional := inst.MinNotional

	if notionalValue < minNotional {
		return fmt.Errorf(
			"order amount %.2f %s is below minimum requirement %.2f %s (quantity: %.4f, price: %.4f)",
			notionalValue, inst.QuoteAsset, minNotional, inst.QuoteAsset, quantity, price,
		)
	}

	return nil
}

func (t *FuturesTrader) ResolveExecutionInstrument(symbol string) (*ExecutionInstrument, error) {
	inst, err := t.resolveBinanceInstrument(symbol)
	if err != nil {
		return nil, err
	}
	copy := inst.ExecutionInstrument
	return &copy, nil
}

func (t *FuturesTrader) resolveBinanceInstrument(symbol string) (*binanceExecutionInstrument, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, fmt.Errorf("%w: empty Binance symbol", ErrExecutionInstrumentUnsupported)
	}
	t.instrumentsCacheMutex.RLock()
	if inst := t.instrumentsCache[symbol]; validBinanceReductionInstrument(inst, symbol) && inst.MinNotional > 0 && time.Since(t.instrumentsCacheTime) < 5*time.Minute {
		copy := *inst
		t.instrumentsCacheMutex.RUnlock()
		return &copy, nil
	}
	t.instrumentsCacheMutex.RUnlock()

	if err := t.refreshBinanceInstrumentCatalog(); err != nil {
		return nil, err
	}
	t.instrumentsCacheMutex.RLock()
	inst := t.instrumentsCache[symbol]
	if !validBinanceReductionInstrument(inst, symbol) || inst.MinNotional <= 0 {
		t.instrumentsCacheMutex.RUnlock()
		return nil, fmt.Errorf("%w: Binance contract %s is missing, non-TRADING, unsupported type, or lacks precision/minimum-notional metadata", ErrExecutionInstrumentUnsupported, symbol)
	}
	copy := *inst
	t.instrumentsCacheMutex.RUnlock()
	return &copy, nil
}

func (t *FuturesTrader) refreshBinanceInstrumentCatalog() error {
	info, err := t.client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return fmt.Errorf("%w: load Binance USD-M catalog: %v", ErrExecutionInstrumentUnsupported, err)
	}
	catalog := make(map[string]*binanceExecutionInstrument, len(info.Symbols))
	for _, item := range info.Symbols {
		contractType := string(item.ContractType)
		if item.Status != "TRADING" || contractType != "PERPETUAL" && contractType != "TRADIFI_PERPETUAL" {
			continue
		}
		var lotStepSize, lotMinQuantity, lotMaxQuantity float64
		var marketStepSize, marketMinQuantity, marketMaxQuantity float64
		var tickSize, minNotional float64
		hasMarketLotSize := false
		for _, filter := range item.Filters {
			filterType, _ := filter["filterType"].(string)
			switch filterType {
			case "LOT_SIZE":
				lotStepSize, _ = strconv.ParseFloat(fmt.Sprint(filter["stepSize"]), 64)
				lotMinQuantity, _ = strconv.ParseFloat(fmt.Sprint(filter["minQty"]), 64)
				lotMaxQuantity, _ = strconv.ParseFloat(fmt.Sprint(filter["maxQty"]), 64)
			case "MARKET_LOT_SIZE":
				hasMarketLotSize = true
				marketStepSize, _ = strconv.ParseFloat(fmt.Sprint(filter["stepSize"]), 64)
				marketMinQuantity, _ = strconv.ParseFloat(fmt.Sprint(filter["minQty"]), 64)
				marketMaxQuantity, _ = strconv.ParseFloat(fmt.Sprint(filter["maxQty"]), 64)
			case "PRICE_FILTER":
				tickSize, _ = strconv.ParseFloat(fmt.Sprint(filter["tickSize"]), 64)
			case "MIN_NOTIONAL":
				minNotional, _ = strconv.ParseFloat(fmt.Sprint(filter["notional"]), 64)
			}
		}
		// MARKET_LOT_SIZE is authoritative for MARKET orders. Older/test
		// catalogs may omit it, in which case LOT_SIZE remains the compatible
		// fallback. Once present, its precision and limits must win.
		stepSize, minQuantity, maxQuantity := lotStepSize, lotMinQuantity, lotMaxQuantity
		if hasMarketLotSize {
			stepSize, minQuantity, maxQuantity = marketStepSize, marketMinQuantity, marketMaxQuantity
		}
		// Min-notional is an opening constraint, not a requirement for reducing
		// an existing position. Keep otherwise complete instruments in the cache
		// so a close can proceed even if that filter is temporarily absent.
		if item.BaseAsset == "" || item.QuoteAsset == "" || item.MarginAsset == "" || stepSize <= 0 || minQuantity <= 0 || tickSize <= 0 || hasMarketLotSize && maxQuantity <= 0 {
			continue
		}
		catalog[item.Symbol] = &binanceExecutionInstrument{
			ExecutionInstrument: ExecutionInstrument{SourceSymbol: item.Symbol, NativeSymbol: item.Symbol, BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset, SettleAsset: item.MarginAsset, MarketType: "UM", ContractType: contractType, Status: item.Status, PriceTickSize: tickSize, BaseQuantityStep: stepSize, NativeQuantityStep: stepSize, MinBaseQuantity: minQuantity, MinNotional: minNotional},
			StepSize:            stepSize, MinQuantity: minQuantity, MaxQuantity: maxQuantity, TickSize: tickSize, MinNotional: minNotional,
		}
	}
	t.instrumentsCacheMutex.Lock()
	t.instrumentsCache = catalog
	t.instrumentsCacheTime = time.Now()
	t.instrumentsCacheMutex.Unlock()
	return nil
}

func validBinanceReductionInstrument(item *binanceExecutionInstrument, symbol string) bool {
	return item != nil && item.SourceSymbol == symbol && item.NativeSymbol == symbol &&
		item.BaseAsset != "" && item.QuoteAsset != "" && item.SettleAsset != "" &&
		item.StepSize > 0 && item.MinQuantity > 0
}

// resolveBinanceInstrumentForReduction prefers metadata cached for the exact
// stored symbol. A reduction must not depend on refreshing a public catalog,
// and a failed refresh must not evict the only known-safe close metadata. When
// there is no exact cache entry, the normal live validation is still required.
// No symbol or quote-asset derivation is performed here.
func (t *FuturesTrader) resolveBinanceInstrumentForReduction(symbol string) (*binanceExecutionInstrument, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, fmt.Errorf("%w: empty Binance symbol", ErrExecutionInstrumentUnsupported)
	}
	t.instrumentsCacheMutex.RLock()
	var cached *binanceExecutionInstrument
	if item := t.instrumentsCache[symbol]; validBinanceReductionInstrument(item, symbol) {
		copy := *item
		cached = &copy
	}
	t.instrumentsCacheMutex.RUnlock()
	if cached != nil {
		return cached, nil
	}
	if err := t.refreshBinanceInstrumentCatalog(); err != nil {
		return nil, fmt.Errorf("%w: no exact cached Binance metadata for risk reduction of %s: %v", ErrExecutionInstrumentUnsupported, symbol, err)
	}
	t.instrumentsCacheMutex.RLock()
	item := t.instrumentsCache[symbol]
	if !validBinanceReductionInstrument(item, symbol) {
		t.instrumentsCacheMutex.RUnlock()
		return nil, fmt.Errorf("%w: Binance contract %s lacks exact market quantity metadata for risk reduction", ErrExecutionInstrumentUnsupported, symbol)
	}
	copy := *item
	t.instrumentsCacheMutex.RUnlock()
	return &copy, nil
}

// GetSymbolPrecision gets the quantity precision from the validated catalog.
func (t *FuturesTrader) GetSymbolPrecision(symbol string) (int, error) {
	inst, err := t.resolveBinanceInstrument(symbol)
	if err != nil {
		return 0, err
	}
	return calculatePrecision(strconv.FormatFloat(inst.StepSize, 'f', -1, 64)), nil
}

// calculatePrecision calculates precision from stepSize
func calculatePrecision(stepSize string) int {
	// Remove trailing zeros
	stepSize = trimTrailingZeros(stepSize)

	// Find decimal point
	dotIndex := -1
	for i := 0; i < len(stepSize); i++ {
		if stepSize[i] == '.' {
			dotIndex = i
			break
		}
	}

	// If no decimal point or decimal point is at the end, precision is 0
	if dotIndex == -1 || dotIndex == len(stepSize)-1 {
		return 0
	}

	// Return number of digits after decimal point
	return len(stepSize) - dotIndex - 1
}

// trimTrailingZeros removes trailing zeros
func trimTrailingZeros(s string) string {
	// If no decimal point, return directly
	if !stringContains(s, ".") {
		return s
	}

	// Iterate backwards to remove trailing zeros
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}

	// If last character is decimal point, remove it too
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}

	return s
}

// FormatQuantity formats quantity to correct precision
func (t *FuturesTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	inst, err := t.resolveBinanceInstrument(symbol)
	if err != nil {
		return "", err
	}
	return formatBinanceMarketQuantity(inst, symbol, quantity)
}

func formatBinanceMarketQuantity(inst *binanceExecutionInstrument, symbol string, quantity float64) (string, error) {
	if inst == nil || inst.StepSize <= 0 || inst.MinQuantity <= 0 {
		return "", fmt.Errorf("%w: Binance market quantity metadata is incomplete for %s", ErrExecutionInstrumentUnsupported, symbol)
	}
	quantity = math.Floor((quantity+inst.StepSize*1e-9)/inst.StepSize) * inst.StepSize
	if quantity < inst.MinQuantity {
		return "", fmt.Errorf("quantity %.12f is below Binance minimum %.12f for %s", quantity, inst.MinQuantity, symbol)
	}
	if inst.MaxQuantity > 0 && quantity > inst.MaxQuantity {
		return "", fmt.Errorf("quantity %.12f exceeds Binance market maximum %.12f for %s", quantity, inst.MaxQuantity, symbol)
	}
	precision := calculatePrecision(strconv.FormatFloat(inst.StepSize, 'f', -1, 64))
	format := fmt.Sprintf("%%.%df", precision)
	return fmt.Sprintf(format, quantity), nil
}

// Helper functions
func contains(s, substr string) bool {
	return len(s) >= len(substr) && stringContains(s, substr)
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// GetOrderStatus gets order status
func (t *FuturesTrader) GetOrderStatus(symbol string, orderID string) (map[string]interface{}, error) {
	// Convert orderID to int64
	orderIDInt, err := strconv.ParseInt(orderID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid order ID: %s", orderID)
	}

	order, err := t.client.NewGetOrderService().
		Symbol(symbol).
		OrderID(orderIDInt).
		Do(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get order status: %w", err)
	}

	// Parse execution price
	avgPrice, _ := strconv.ParseFloat(order.AvgPrice, 64)
	executedQty, _ := strconv.ParseFloat(order.ExecutedQuantity, 64)

	result := map[string]interface{}{
		"orderId":     order.OrderID,
		"symbol":      order.Symbol,
		"status":      string(order.Status),
		"avgPrice":    avgPrice,
		"executedQty": executedQty,
		"side":        string(order.Side),
		"type":        string(order.Type),
		"time":        order.Time,
		"updateTime":  order.UpdateTime,
	}

	// Binance futures commission fee needs to be obtained through GetUserTrades, not retrieved here for now
	// Can be obtained later through WebSocket or separate query
	result["commission"] = 0.0

	return result, nil
}

func (t *FuturesTrader) GetOrderStatusByClientID(symbol, clientOrderID string) (map[string]interface{}, error) {
	order, found, err := t.findOrderByClientID(strings.ToUpper(strings.TrimSpace(symbol)), clientOrderID)
	if err != nil {
		return nil, fmt.Errorf("failed to get Binance order by client id: %w", err)
	}
	if !found {
		return nil, ErrExecutionOrderNotFound
	}
	return binanceOrderResult(order), nil
}

// CancelOrderByClientID cancels one exact Binance USD-M order without touching
// protective or unrelated orders on the same symbol. A successful cancel
// response is only an acknowledgement; Copy Guard performs the authoritative
// read-after-write state check before releasing a leader-close barrier.
func (t *FuturesTrader) CancelOrderByClientID(symbol, clientOrderID string) error {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	clientOrderID = strings.TrimSpace(clientOrderID)
	if symbol == "" || clientOrderID == "" {
		return fmt.Errorf("symbol and client order id are required")
	}
	_, err := t.client.NewCancelOrderService().
		Symbol(symbol).
		OrigClientOrderID(clientOrderID).
		Do(context.Background())
	if err != nil {
		if isBinanceOrderNotFound(err) {
			// The caller must still query and classify the final order state.
			return nil
		}
		return fmt.Errorf("failed to cancel Binance order by client id: %w", err)
	}
	t.invalidatePositionsCache()
	return nil
}

// GetClosedPnL retrieves recent closing trades from Binance Futures
// Note: Binance does NOT have a position history API, only trade history.
// This returns individual closing trades (realizedPnl != 0) for real-time position closure detection.
// NOT suitable for historical position reconstruction - use only for matching recent closures.
func (t *FuturesTrader) GetClosedPnL(startTime time.Time, limit int) ([]ClosedPnLRecord, error) {
	trades, err := t.GetTrades(startTime, limit)
	if err != nil {
		return nil, err
	}

	// Filter only closing trades (realizedPnl != 0) and convert to ClosedPnLRecord
	var records []ClosedPnLRecord
	for _, trade := range trades {
		if trade.RealizedPnL == 0 {
			continue // Skip opening trades
		}

		// Determine side from trade
		side := "long"
		if trade.PositionSide == "SHORT" || trade.PositionSide == "short" {
			side = "short"
		} else if trade.PositionSide == "BOTH" || trade.PositionSide == "" {
			// One-way mode: selling closes long, buying closes short
			if trade.Side == "SELL" || trade.Side == "Sell" {
				side = "long"
			} else {
				side = "short"
			}
		}

		// Calculate entry price from PnL (mathematically accurate for this trade)
		var entryPrice float64
		if trade.Quantity > 0 {
			if side == "long" {
				entryPrice = trade.Price - trade.RealizedPnL/trade.Quantity
			} else {
				entryPrice = trade.Price + trade.RealizedPnL/trade.Quantity
			}
		}

		records = append(records, ClosedPnLRecord{
			Symbol:      trade.Symbol,
			Side:        side,
			EntryPrice:  entryPrice,
			ExitPrice:   trade.Price,
			Quantity:    trade.Quantity,
			RealizedPnL: trade.RealizedPnL,
			Fee:         trade.Fee,
			ExitTime:    trade.Time,
			EntryTime:   trade.Time, // Approximate
			OrderID:     trade.TradeID,
			ExchangeID:  trade.TradeID,
			CloseType:   "unknown",
		})
	}

	return records, nil
}

// GetClosedPnLBySymbol uses Binance account trades rather than the delayed
// income feed. Binance has no stable futures position id, so Copy Guard uses
// this symbol-scoped path and its own side/time/quantity filters.
func (t *FuturesTrader) GetClosedPnLBySymbol(symbol string, startTime time.Time, limit int) ([]ClosedPnLRecord, error) {
	trades, err := t.GetTradesForSymbol(strings.ToUpper(strings.TrimSpace(symbol)), startTime, limit)
	if err != nil {
		return nil, err
	}
	return buildBinanceClosedPnLRecords(trades, limit), nil
}

func buildBinanceClosedPnLRecords(trades []TradeRecord, limit int) []ClosedPnLRecord {
	type aggregate struct {
		record   ClosedPnLRecord
		notional float64
	}
	byOrder := make(map[string]*aggregate)
	orderKeys := make([]string, 0)
	for _, trade := range trades {
		positionSide := strings.ToUpper(strings.TrimSpace(trade.PositionSide))
		tradeSide := strings.ToUpper(strings.TrimSpace(trade.Side))
		position := ""
		switch {
		case positionSide == "LONG" && tradeSide == "SELL":
			position = "long"
		case positionSide == "SHORT" && tradeSide == "BUY":
			position = "short"
		case (positionSide == "BOTH" || positionSide == "") && trade.RealizedPnL != 0 && tradeSide == "SELL":
			position = "long"
		case (positionSide == "BOTH" || positionSide == "") && trade.RealizedPnL != 0 && tradeSide == "BUY":
			position = "short"
		default:
			continue
		}
		if trade.Quantity <= 0 || trade.Price <= 0 {
			continue
		}
		orderID := trade.OrderID
		if orderID == "" {
			orderID = trade.TradeID
		}
		key := orderID + "|" + position
		agg := byOrder[key]
		if agg == nil {
			agg = &aggregate{record: ClosedPnLRecord{
				Symbol: trade.Symbol, Side: position, EntryTime: trade.Time, ExitTime: trade.Time,
				OrderID: orderID, CloseType: "unknown",
			}}
			byOrder[key] = agg
			orderKeys = append(orderKeys, key)
		}
		agg.notional += trade.Price * trade.Quantity
		agg.record.Quantity += trade.Quantity
		agg.record.QuantityCoins += trade.Quantity
		agg.record.RealizedPnL += trade.RealizedPnL
		agg.record.Fee += trade.Fee
		if trade.Time.Before(agg.record.EntryTime) {
			agg.record.EntryTime = trade.Time
		}
		if trade.Time.After(agg.record.ExitTime) {
			agg.record.ExitTime = trade.Time
		}
	}
	if limit <= 0 || limit > len(orderKeys) {
		limit = len(orderKeys)
	}
	records := make([]ClosedPnLRecord, 0, limit)
	for _, key := range orderKeys {
		agg := byOrder[key]
		if agg == nil || agg.record.Quantity <= 0 {
			continue
		}
		agg.record.ExitPrice = agg.notional / agg.record.Quantity
		if agg.record.Side == "long" {
			agg.record.EntryPrice = agg.record.ExitPrice - agg.record.RealizedPnL/agg.record.Quantity
		} else {
			agg.record.EntryPrice = agg.record.ExitPrice + agg.record.RealizedPnL/agg.record.Quantity
		}
		records = append(records, agg.record)
		if len(records) >= limit {
			break
		}
	}
	return records
}

// GetTrades retrieves trade history from Binance Futures using Income API
// Note: Income API has delays (~minutes), for real-time use GetTradesForSymbol instead
func (t *FuturesTrader) GetTrades(startTime time.Time, limit int) ([]TradeRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// Use Income API to get REALIZED_PNL records (all symbols)
	incomes, err := t.client.NewGetIncomeHistoryService().
		IncomeType("REALIZED_PNL").
		StartTime(startTime.UnixMilli()).
		Limit(int64(limit)).
		Do(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get income history: %w", err)
	}

	var trades []TradeRecord
	for _, income := range incomes {
		pnl, _ := strconv.ParseFloat(income.Income, 64)
		if pnl == 0 {
			continue // Skip zero PnL records
		}

		// Income API doesn't provide full trade details, create a minimal record
		// This is mainly used for detecting recent closures, not historical reconstruction
		trade := TradeRecord{
			TradeID:     strconv.FormatInt(income.TranID, 10),
			Symbol:      income.Symbol,
			RealizedPnL: pnl,
			Time:        time.UnixMilli(income.Time),
			// Note: Income API doesn't provide price, quantity, side, fee
			// For accurate data, use GetTradesForSymbol with specific symbol
		}
		trades = append(trades, trade)
	}

	return trades, nil
}

// GetTradesForSymbol retrieves trade history for a specific symbol
// This is more reliable than using Income API which may have delays
func (t *FuturesTrader) GetTradesForSymbol(symbol string, startTime time.Time, limit int) ([]TradeRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	accountTrades, err := t.client.NewListAccountTradeService().
		Symbol(symbol).
		StartTime(startTime.UnixMilli()).
		Limit(limit).
		Do(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get trade history for %s: %w", symbol, err)
	}

	var trades []TradeRecord
	for _, at := range accountTrades {
		price, _ := strconv.ParseFloat(at.Price, 64)
		qty, _ := strconv.ParseFloat(at.Quantity, 64)
		fee, _ := strconv.ParseFloat(at.Commission, 64)
		pnl, _ := strconv.ParseFloat(at.RealizedPnl, 64)

		trade := TradeRecord{
			TradeID:      strconv.FormatInt(at.ID, 10),
			OrderID:      strconv.FormatInt(at.OrderID, 10),
			Symbol:       at.Symbol,
			Side:         string(at.Side),
			PositionSide: string(at.PositionSide),
			Price:        price,
			Quantity:     qty,
			RealizedPnL:  pnl,
			Fee:          fee,
			Time:         time.UnixMilli(at.Time),
		}
		trades = append(trades, trade)
	}

	return trades, nil
}
