package trader

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrProtectiveStopNotFound reports that the exchange confirmed the protective
// stop does not exist (queries succeeded but returned no record). Callers must
// distinguish this from transient query failures: only a confirmed absence may
// invalidate local state or trigger re-creation.
var ErrProtectiveStopNotFound = errors.New("protective stop not found")

var ErrExecutionInstrumentUnsupported = errors.New("execution instrument unsupported")

// IsProtectiveStopAlreadyTriggerable reports that a venue refused a protective
// stop because its trigger price is already on the wrong side of the market —
// OKX 51280 and Binance -2021 ("Order would immediately trigger").
//
// This is a deterministic rejection of the price, not a transient fault. Both
// venues re-evaluate identically on every attempt, so the generic retry path
// burns its full budget (10 attempts with backoff, several minutes) placing the
// same doomed order while the position runs unprotected. Callers must instead
// route straight to the unprotectable disposition, which either re-derives the
// stop from a fresh price or hands the position over with an alert.
//
// Matching is on substrings because both adapters wrap venue payloads in
// formatted strings rather than typed errors; the numeric code is always
// present and is paired with a phrase check so an unrelated message that merely
// contains the digits cannot be misread as a price rejection.
func IsProtectiveStopAlreadyTriggerable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "51280") &&
		(strings.Contains(msg, "trigger price") || strings.Contains(msg, "last price")) {
		return true
	}
	return strings.Contains(msg, "-2021") || strings.Contains(msg, "immediately trigger")
}

// ExecutionInstrument keeps contract identity separate from USD value
// normalization. An execution venue may only accept an exact base+quote/settle
// match; it must never silently substitute USDC/USD1 with USDT.
type ExecutionInstrument struct {
	SourceSymbol string
	NativeSymbol string
	BaseAsset    string
	QuoteAsset   string
	SettleAsset  string
	MarketType   string
	ContractType string
	Status       string
	// PriceTickSize and BaseQuantityStep are expressed in the units used by
	// Copy Guard: quote-asset price and base-asset quantity respectively.
	// Keeping them on the resolved execution contract prevents Binance stops
	// from accidentally using OKX precision metadata.
	PriceTickSize      float64
	BaseQuantityStep   float64
	NativeQuantityStep float64
	MinBaseQuantity    float64
	MinNotional        float64
}

type ExecutionInstrumentResolver interface {
	ResolveExecutionInstrument(symbol string) (*ExecutionInstrument, error)
}

// ClosedPnLRecord represents a single closed position record from exchange
type ClosedPnLRecord struct {
	Symbol             string  // Trading pair (e.g., "BTCUSDT")
	Side               string  // "long" or "short"
	EntryPrice         float64 // Entry price
	ExitPrice          float64 // Exit/close price
	Quantity           float64 // Position size in the exchange's native unit (OKX: contracts)
	QuantityCoins      float64 // Position size in coins (OKX: contracts × ctVal); 0 when the exchange already reports coins or ctVal is unavailable
	RealizedPnL        float64 // Realized profit/loss
	Fee                float64 // Trading fee/commission
	FundingFee         float64
	LiquidationPenalty float64
	GrossPnL           float64
	Leverage           int       // Leverage used
	MarginMode         string    // "cross" or "isolated"
	EntryTime          time.Time // Position open time
	ExitTime           time.Time // Position close time
	OrderID            string    // Close order ID
	CloseType          string    // "manual", "stop_loss", "take_profit", "liquidation", "unknown"
	ExchangeID         string    // Exchange-specific position ID
	Fills              []ClosedPnLRecord
}

// TradeRecord represents a single trade/fill from exchange
// Used for reconstructing position history with unified algorithm
type TradeRecord struct {
	TradeID      string    // Unique trade ID from exchange
	OrderID      string    // Exchange order ID; multiple fills may share it
	Symbol       string    // Trading pair (e.g., "BTCUSDT")
	Side         string    // "BUY" or "SELL"
	PositionSide string    // "LONG", "SHORT", or "BOTH" (for one-way mode)
	Price        float64   // Execution price
	Quantity     float64   // Executed quantity
	RealizedPnL  float64   // Realized PnL (non-zero for closing trades)
	Fee          float64   // Trading fee/commission
	Time         time.Time // Trade execution time
}

// SymbolTradeHistoryProvider exposes immutable per-fill history. Position
// history is often cumulative/mutable and must not be used as a fill identity
// when several local lots share one exchange position lifecycle.
type SymbolTradeHistoryProvider interface {
	GetTradesForSymbol(symbol string, start time.Time, limit int) ([]TradeRecord, error)
}

type PendingOrderSnapshot struct {
	ID         string
	Symbol     string
	Status     string
	Protective bool
}

// PendingOrderProvider is optional. Explicit stopped-trader reconciliation
// uses it together with a fresh position read before retiring stale local rows.
type PendingOrderProvider interface {
	GetPendingOrdersFresh() ([]PendingOrderSnapshot, error)
}

// ClosedPnLBySymbolProvider provides an exact symbol-scoped reconciliation
// path for venues (such as Binance) that do not expose a stable position ID.
type ClosedPnLBySymbolProvider interface {
	GetClosedPnLBySymbol(symbol string, start time.Time, limit int) ([]ClosedPnLRecord, error)
}

type ProtectiveStopRequest struct {
	Symbol       string
	PositionSide string
	MarginMode   string
	Quantity     float64
	TriggerPrice float64
	TriggerType  string
	ClientID     string
}

type ProtectiveStopOrder struct {
	AlgoID        string
	ClientID      string
	Symbol        string
	PositionSide  string
	MarginMode    string
	Quantity      float64
	TriggerPrice  float64
	TriggerType   string
	State         string
	ActualOrderID string
}

// ProtectiveStopManager is intentionally separate from Trader so non-OKX implementations remain unchanged.
type ProtectiveStopManager interface {
	PlaceProtectiveStop(req ProtectiveStopRequest) (*ProtectiveStopOrder, error)
	AmendProtectiveStop(algoID string, req ProtectiveStopRequest) error
	GetProtectiveStop(algoID, symbol string) (*ProtectiveStopOrder, error)
	GetProtectiveStopByClientID(clientID, symbol string) (*ProtectiveStopOrder, error)
	CancelProtectiveStop(algoID, symbol string) error
}

// ProtectiveStopEnsureResult represents a safe replacement. If retiring is
// non-nil, the new current order is already live but the old order could not
// yet be confirmed terminal; callers must persist and monitor both IDs.
type ProtectiveStopEnsureResult struct {
	Current            *ProtectiveStopOrder
	Retiring           *ProtectiveStopOrder
	ReplacementPending bool
}

type ProtectiveStopEnsurer interface {
	EnsureProtectiveStop(existing *ProtectiveStopOrder, req ProtectiveStopRequest) (*ProtectiveStopEnsureResult, error)
}

// CopyTradeOrderExecutor lets copy trading preserve unrelated pending/algo orders.
// It is optional so existing exchanges and normal AI trading keep their behavior.
type CopyTradeOrderExecutor interface {
	OpenLongPreservingOrders(symbol string, quantity float64, leverage int) (map[string]interface{}, error)
	OpenShortPreservingOrders(symbol string, quantity float64, leverage int) (map[string]interface{}, error)
	CloseLongPreservingOrders(symbol string, quantity float64) (map[string]interface{}, error)
	CloseShortPreservingOrders(symbol string, quantity float64) (map[string]interface{}, error)
}

// CopyTradeIdempotentOrderExecutor is used by copy-trade execution paths that
// carry a stable client order ID (Copy Guard reentry's cycle-level cgr ID and
// the engine's fill-level decision ID) so a retry, replay or restart cannot
// create a second order for the same logical action: the exchange rejects the
// duplicate clOrdId instead of filling it again.
type CopyTradeIdempotentOrderExecutor interface {
	OpenLongPreservingOrdersWithClientID(symbol string, quantity float64, leverage int, clientOrderID string) (map[string]interface{}, error)
	OpenShortPreservingOrdersWithClientID(symbol string, quantity float64, leverage int, clientOrderID string) (map[string]interface{}, error)
	CloseLongPreservingOrdersWithClientID(symbol string, quantity float64, clientOrderID string) (map[string]interface{}, error)
	CloseShortPreservingOrdersWithClientID(symbol string, quantity float64, clientOrderID string) (map[string]interface{}, error)
}

// CopyTradeMarketOrderRequest carries the persistence callback to the exact
// transport boundary. Implementations must invoke BeforeSubmit after all
// deterministic local validation and immediately before the exchange request;
// when adopting an existing idempotent order they invoke it before returning
// the adopted result.
type CopyTradeMarketOrderRequest struct {
	Action        string
	Symbol        string
	Quantity      float64
	Leverage      int
	ClientOrderID string
	BeforeSubmit  func() error
}

type CopyTradeSubmissionBoundaryExecutor interface {
	ExecuteCopyTradeMarketOrder(request CopyTradeMarketOrderRequest) (map[string]interface{}, error)
}

// CopyTradePreSubmitError proves the adapter rejected a request before its
// BeforeSubmit callback succeeded. The copy-trade layer uses ReasonCode rather
// than string matching so the source revision stays safely replayable.
type CopyTradePreSubmitError struct{ Cause error }

func (e *CopyTradePreSubmitError) Error() string {
	if e == nil || e.Cause == nil {
		return "copy trade rejected before exchange submission"
	}
	return fmt.Sprintf("copy trade rejected before exchange submission: %v", e.Cause)
}
func (e *CopyTradePreSubmitError) Unwrap() error      { return e.Cause }
func (e *CopyTradePreSubmitError) ReasonCode() string { return "PRE_SUBMIT" }

// CopyTradeOpenPreparer moves venue-specific leverage cooldowns before the
// final price/minimum calculation without imposing those semantics on other
// exchanges.
type CopyTradeOpenPreparer interface {
	PrepareCopyTradeOpen(symbol string, leverage int) error
}

type ClientOrderStatusProvider interface {
	GetOrderStatusByClientID(symbol, clientOrderID string) (map[string]interface{}, error)
}

type ClientOrderCanceler interface {
	CancelOrderByClientID(symbol, clientOrderID string) error
}

// ClosedPnLByPositionProvider is OKX-only: it queries closed-position history
// by the exchange position id so Copy Guard reconciliation can match exactly
// one lifecycle instead of scanning a time window.
type ClosedPnLByPositionProvider interface {
	GetClosedPnLByPositionID(symbol, posID string, limit int) ([]ClosedPnLRecord, error)
}

// FreshPositionProvider bypasses exchange position caches after a mutation.
type FreshPositionProvider interface {
	GetPositionsFresh() ([]map[string]interface{}, error)
}

// Trader Unified trader interface
// Supports multiple trading platforms (Binance, Hyperliquid, etc.)
type Trader interface {
	// GetBalance Get account balance
	GetBalance() (map[string]interface{}, error)

	// GetPositions Get all positions
	GetPositions() ([]map[string]interface{}, error)

	// OpenLong Open long position
	OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error)

	// OpenShort Open short position
	OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error)

	// CloseLong Close long position (quantity=0 means close all)
	CloseLong(symbol string, quantity float64) (map[string]interface{}, error)

	// CloseShort Close short position (quantity=0 means close all)
	CloseShort(symbol string, quantity float64) (map[string]interface{}, error)

	// SetLeverage Set leverage
	SetLeverage(symbol string, leverage int) error

	// SetMarginMode Set position mode (true=cross margin, false=isolated margin)
	SetMarginMode(symbol string, isCrossMargin bool) error

	// GetMarketPrice Get market price
	GetMarketPrice(symbol string) (float64, error)

	// SetStopLoss Set stop-loss order
	SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error

	// SetTakeProfit Set take-profit order
	SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error

	// CancelStopLossOrders Cancel only stop-loss orders (BUG fix: don't delete take-profit when adjusting stop-loss)
	CancelStopLossOrders(symbol string) error

	// CancelTakeProfitOrders Cancel only take-profit orders (BUG fix: don't delete stop-loss when adjusting take-profit)
	CancelTakeProfitOrders(symbol string) error

	// CancelAllOrders Cancel all pending orders for this symbol
	CancelAllOrders(symbol string) error

	// CancelStopOrders Cancel stop-loss/take-profit orders for this symbol (for adjusting stop-loss/take-profit positions)
	CancelStopOrders(symbol string) error

	// FormatQuantity Format quantity to correct precision
	FormatQuantity(symbol string, quantity float64) (string, error)

	// GetOrderStatus Get order status
	// Returns: status(FILLED/NEW/CANCELED), avgPrice, executedQty, commission
	GetOrderStatus(symbol string, orderID string) (map[string]interface{}, error)

	// GetClosedPnL Get closed position PnL records from exchange
	// startTime: start time for query (usually last sync time)
	// limit: max number of records to return
	// Returns accurate exit price, fees, and close reason for positions closed externally
	GetClosedPnL(startTime time.Time, limit int) ([]ClosedPnLRecord, error)
}
