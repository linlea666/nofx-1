package trader

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

type binanceHardeningRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn binanceHardeningRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func binanceHardeningJSONResponse(t *testing.T, status int, body interface{}) *http.Response {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal Binance test response: %v", err)
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}
}

func TestAdoptBinanceCopyOrderRejectsTerminalZeroFill(t *testing.T) {
	tests := []struct {
		name        string
		status      futures.OrderStatusType
		executedQty string
		wantErr     bool
	}{
		{name: "live new", status: futures.OrderStatusTypeNew, executedQty: "0"},
		{name: "filled", status: futures.OrderStatusTypeFilled, executedQty: "1.25"},
		{name: "partially filled", status: futures.OrderStatusTypePartiallyFilled, executedQty: "0.25"},
		{name: "terminal with a real execution", status: futures.OrderStatusTypeCanceled, executedQty: "0.25"},
		{name: "canceled zero fill", status: futures.OrderStatusTypeCanceled, executedQty: "0", wantErr: true},
		{name: "rejected zero fill", status: futures.OrderStatusTypeRejected, executedQty: "0", wantErr: true},
		{name: "expired zero fill", status: futures.OrderStatusTypeExpired, executedQty: "0", wantErr: true},
		{name: "filled but impossible zero fill", status: futures.OrderStatusTypeFilled, executedQty: "0", wantErr: true},
		{name: "partial but zero fill", status: futures.OrderStatusTypePartiallyFilled, executedQty: "0", wantErr: true},
		{name: "invalid execution quantity", status: futures.OrderStatusTypeFilled, executedQty: "not-a-number", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := adoptBinanceCopyOrder(&futures.Order{
				Symbol: "BTCUSDC", ClientOrderID: "copy-decision-1", OrderID: 123,
				Status: tc.status, ExecutedQuantity: tc.executedQty,
			})
			if tc.wantErr {
				if err == nil || result != nil {
					t.Fatalf("expected unusable client id error, result=%v err=%v", result, err)
				}
				return
			}
			if err != nil || result == nil {
				t.Fatalf("expected safe adoption, result=%v err=%v", result, err)
			}
		})
	}
}

func TestBinanceCopyOrderDoesNotAdoptExistingTerminalZeroFill(t *testing.T) {
	postCount := 0
	client := futures.NewClient("", "")
	client.BaseURL = "https://binance.idempotency.test"
	client.HTTPClient = &http.Client{Transport: binanceHardeningRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			postCount++
		}
		if req.URL.Path != "/fapi/v1/order" || req.Method != http.MethodGet {
			t.Fatalf("terminal idempotency lookup must stop before create, got %s %s", req.Method, req.URL.String())
		}
		return binanceHardeningJSONResponse(t, http.StatusOK, map[string]interface{}{
			"symbol": "BTCUSDC", "orderId": 123, "clientOrderId": "copy-close-terminal",
			"status": "CANCELED", "executedQty": "0", "origQty": "1", "type": "MARKET",
		}), nil
	})}
	trader := &FuturesTrader{
		client:        client,
		cacheDuration: 15 * time.Second,
		instrumentsCache: map[string]*binanceExecutionInstrument{
			"BTCUSDC": {
				ExecutionInstrument: ExecutionInstrument{
					SourceSymbol: "BTCUSDC", NativeSymbol: "BTCUSDC", BaseAsset: "BTC",
					QuoteAsset: "USDC", SettleAsset: "USDC", MarketType: "UM",
					ContractType: "PERPETUAL", Status: "TRADING", BaseQuantityStep: 0.01,
				},
				StepSize: 0.01, MinQuantity: 0.01, MaxQuantity: 100, TickSize: 0.1, MinNotional: 5,
			},
		},
		instrumentsCacheTime: time.Now(),
	}

	result, err := trader.CloseLongPreservingOrdersWithClientID("BTCUSDC", 1, "copy-close-terminal")
	if err == nil || result != nil || !strings.Contains(err.Error(), "zero executed quantity") {
		t.Fatalf("terminal zero-fill client id must not be adopted: result=%v err=%v", result, err)
	}
	if postCount != 0 {
		t.Fatalf("terminal client id triggered %d duplicate creates", postCount)
	}
}

func TestBinanceCancelOrderByClientIDUsesExactIdentity(t *testing.T) {
	var method, symbol, clientID string
	client := futures.NewClient("", "")
	client.BaseURL = "https://binance.cancel-client-id.test"
	client.HTTPClient = &http.Client{Transport: binanceHardeningRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		method = req.Method
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read Binance cancel body: %v", err)
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatalf("parse Binance cancel body: %v", err)
		}
		symbol = form.Get("symbol")
		clientID = form.Get("origClientOrderId")
		return binanceHardeningJSONResponse(t, http.StatusOK, map[string]interface{}{
			"symbol": symbol, "orderId": 123, "clientOrderId": clientID,
			"status": "CANCELED", "executedQty": "0", "origQty": "1",
		}), nil
	})}
	bt := &FuturesTrader{client: client}
	if err := bt.CancelOrderByClientID("btcusdt", "copy-open-123"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodDelete || symbol != "BTCUSDT" || clientID != "copy-open-123" {
		t.Fatalf("unexpected cancel request: method=%s symbol=%s client=%s", method, symbol, clientID)
	}
}

func TestBinanceMarketLotSizeControlsMarketQuantity(t *testing.T) {
	symbol := binanceCatalogSymbol("BTCUSDT", "BTC", "USDT", "USDT", "TRADING", "PERPETUAL")
	symbol["filters"] = []map[string]interface{}{
		{"filterType": "PRICE_FILTER", "tickSize": "0.10"},
		{"filterType": "LOT_SIZE", "minQty": "0.001", "maxQty": "100", "stepSize": "0.001"},
		{"filterType": "MARKET_LOT_SIZE", "minQty": "0.01", "maxQty": "2", "stepSize": "0.01"},
		{"filterType": "MIN_NOTIONAL", "notional": "5"},
	}
	trader := newBinanceCatalogTestTrader(t, symbol)

	resolved, err := trader.ResolveExecutionInstrument("BTCUSDT")
	if err != nil {
		t.Fatalf("resolve Binance market instrument: %v", err)
	}
	if resolved.BaseQuantityStep != 0.01 {
		t.Fatalf("MARKET_LOT_SIZE step must be exposed for market execution, got %v", resolved.BaseQuantityStep)
	}
	got, err := trader.FormatQuantity("BTCUSDT", 1.239)
	if err != nil || got != "1.23" {
		t.Fatalf("market step formatting mismatch: got=%q err=%v", got, err)
	}
	if _, err := trader.FormatQuantity("BTCUSDT", 0.009); err == nil || !strings.Contains(err.Error(), "below Binance minimum") {
		t.Fatalf("MARKET_LOT_SIZE minimum must be enforced, got %v", err)
	}
	if _, err := trader.FormatQuantity("BTCUSDT", 2.01); err == nil || !strings.Contains(err.Error(), "exceeds Binance market maximum") {
		t.Fatalf("MARKET_LOT_SIZE maximum must be enforced, got %v", err)
	}
}

func TestBinanceMalformedMarketLotSizeDoesNotFallBackToLimitPrecision(t *testing.T) {
	symbol := binanceCatalogSymbol("BTCUSDT", "BTC", "USDT", "USDT", "TRADING", "PERPETUAL")
	symbol["filters"] = []map[string]interface{}{
		{"filterType": "PRICE_FILTER", "tickSize": "0.10"},
		{"filterType": "LOT_SIZE", "minQty": "0.001", "maxQty": "100", "stepSize": "0.001"},
		{"filterType": "MARKET_LOT_SIZE", "minQty": "0", "maxQty": "0", "stepSize": "0"},
		{"filterType": "MIN_NOTIONAL", "notional": "5"},
	}
	trader := newBinanceCatalogTestTrader(t, symbol)

	if _, err := trader.ResolveExecutionInstrument("BTCUSDT"); !errors.Is(err, ErrExecutionInstrumentUnsupported) {
		t.Fatalf("present but malformed MARKET_LOT_SIZE must fail closed, got %v", err)
	}
}

func TestBinanceRiskReductionUsesExactStaleMetadataWhenCatalogUnavailable(t *testing.T) {
	var posted url.Values
	var paths []string
	client := futures.NewClient("", "")
	client.BaseURL = "https://binance.reduction.test"
	client.HTTPClient = &http.Client{Transport: binanceHardeningRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.Method+" "+req.URL.Path)
		switch {
		case req.URL.Path == "/fapi/v1/order" && req.Method == http.MethodGet:
			return binanceHardeningJSONResponse(t, http.StatusBadRequest, map[string]interface{}{"code": -2013, "msg": "Order does not exist."}), nil
		case req.URL.Path == "/fapi/v1/order" && req.Method == http.MethodPost:
			if err := req.ParseForm(); err != nil {
				t.Fatalf("parse Binance create order form: %v", err)
			}
			posted = req.Form
			return binanceHardeningJSONResponse(t, http.StatusOK, map[string]interface{}{
				"orderId": 99, "clientOrderId": req.Form.Get("newClientOrderId"),
				"symbol": req.Form.Get("symbol"), "status": "FILLED",
				"executedQty": req.Form.Get("quantity"), "origQty": req.Form.Get("quantity"),
				"side": req.Form.Get("side"), "positionSide": req.Form.Get("positionSide"), "type": "MARKET",
			}), nil
		default:
			t.Fatalf("unexpected Binance reduction request: %s %s", req.Method, req.URL.String())
			return nil, errors.New("unexpected request")
		}
	})}
	trader := &FuturesTrader{
		client:        client,
		cacheDuration: 15 * time.Second,
		instrumentsCache: map[string]*binanceExecutionInstrument{
			"BTCUSDC": {
				ExecutionInstrument: ExecutionInstrument{
					SourceSymbol: "BTCUSDC", NativeSymbol: "BTCUSDC", BaseAsset: "BTC",
					QuoteAsset: "USDC", SettleAsset: "USDC", MarketType: "UM",
					ContractType: "PERPETUAL", Status: "TRADING", BaseQuantityStep: 0.01,
				},
				StepSize: 0.01, MinQuantity: 0.01, MaxQuantity: 100, TickSize: 0.1, MinNotional: 5,
			},
		},
		instrumentsCacheTime: time.Now().Add(-10 * time.Minute),
	}

	result, err := trader.CloseLongPreservingOrdersWithClientID("BTCUSDC", 1.239, "copy-close-usdc-1")
	if err != nil {
		t.Fatalf("risk-reducing close should use exact cached metadata: %v (paths=%v)", err, paths)
	}
	if result == nil || posted == nil {
		t.Fatalf("expected submitted close order, result=%v posted=%v", result, posted)
	}
	if posted.Get("symbol") != "BTCUSDC" {
		t.Fatalf("settlement asset was changed during fallback: %q", posted.Get("symbol"))
	}
	if posted.Get("quantity") != "1.23" {
		t.Fatalf("cached market precision was not applied: %q", posted.Get("quantity"))
	}
	for _, path := range paths {
		if strings.Contains(path, "ticker") {
			t.Fatalf("risk reduction must not require a min-notional price lookup: %v", paths)
		}
	}
}

func TestBinanceRiskReductionDoesNotRequireMinNotionalFilter(t *testing.T) {
	symbol := binanceCatalogSymbol("BTCUSDC", "BTC", "USDC", "USDC", "TRADING", "PERPETUAL")
	symbol["filters"] = []map[string]interface{}{
		{"filterType": "PRICE_FILTER", "tickSize": "0.10"},
		{"filterType": "LOT_SIZE", "minQty": "0.001", "maxQty": "100", "stepSize": "0.001"},
		{"filterType": "MARKET_LOT_SIZE", "minQty": "0.01", "maxQty": "10", "stepSize": "0.01"},
	}
	client := futures.NewClient("", "")
	client.BaseURL = "https://binance.reduction-no-notional.test"
	client.HTTPClient = &http.Client{Transport: binanceHardeningRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Path == "/fapi/v1/exchangeInfo":
			return binanceHardeningJSONResponse(t, http.StatusOK, map[string]interface{}{"symbols": []map[string]interface{}{symbol}}), nil
		case req.URL.Path == "/fapi/v1/order" && req.Method == http.MethodGet:
			return binanceHardeningJSONResponse(t, http.StatusBadRequest, map[string]interface{}{"code": -2013, "msg": "Order does not exist."}), nil
		case req.URL.Path == "/fapi/v1/order" && req.Method == http.MethodPost:
			if err := req.ParseForm(); err != nil {
				t.Fatalf("parse Binance risk-reduction form: %v", err)
			}
			return binanceHardeningJSONResponse(t, http.StatusOK, map[string]interface{}{
				"orderId": 101, "clientOrderId": req.Form.Get("newClientOrderId"),
				"symbol": req.Form.Get("symbol"), "status": "FILLED", "executedQty": req.Form.Get("quantity"),
				"origQty": req.Form.Get("quantity"), "side": req.Form.Get("side"),
				"positionSide": req.Form.Get("positionSide"), "type": "MARKET",
			}), nil
		default:
			t.Fatalf("unexpected Binance no-notional request: %s %s", req.Method, req.URL.String())
			return nil, errors.New("unexpected request")
		}
	})}
	trader := &FuturesTrader{
		client: client, cacheDuration: 15 * time.Second,
		instrumentsCache: make(map[string]*binanceExecutionInstrument),
	}

	if _, err := trader.CloseLongPreservingOrdersWithClientID("BTCUSDC", 1.239, "copy-close-no-notional"); err != nil {
		t.Fatalf("risk reduction must not depend on MIN_NOTIONAL: %v", err)
	}
	// The same catalog entry remains unsuitable for opening new risk.
	if _, err := trader.ResolveExecutionInstrument("BTCUSDC"); !errors.Is(err, ErrExecutionInstrumentUnsupported) {
		t.Fatalf("opening resolution must still require MIN_NOTIONAL, got %v", err)
	}
}

func TestBinanceOpenFailsClosedWhenCatalogUnavailable(t *testing.T) {
	postCount := 0
	client := futures.NewClient("", "")
	client.BaseURL = "https://binance.open.test"
	client.HTTPClient = &http.Client{Transport: binanceHardeningRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			postCount++
		}
		if req.URL.Path != "/fapi/v1/exchangeInfo" {
			t.Fatalf("open must stop at catalog validation, got %s %s", req.Method, req.URL.String())
		}
		return binanceHardeningJSONResponse(t, http.StatusServiceUnavailable, map[string]interface{}{"code": -1000, "msg": "catalog unavailable"}), nil
	})}
	trader := &FuturesTrader{
		client:        client,
		cacheDuration: 15 * time.Second,
		instrumentsCache: map[string]*binanceExecutionInstrument{
			"BTCUSDC": {
				ExecutionInstrument: ExecutionInstrument{SourceSymbol: "BTCUSDC", NativeSymbol: "BTCUSDC", BaseAsset: "BTC", QuoteAsset: "USDC", SettleAsset: "USDC"},
				StepSize:            0.01, MinQuantity: 0.01, MaxQuantity: 100, TickSize: 0.1, MinNotional: 5,
			},
		},
		instrumentsCacheTime: time.Now().Add(-10 * time.Minute),
	}

	if _, err := trader.OpenLongPreservingOrdersWithClientID("BTCUSDC", 1, 5, "copy-open-usdc-1"); !errors.Is(err, ErrExecutionInstrumentUnsupported) {
		t.Fatalf("new risk must fail closed without live catalog validation, got %v", err)
	}
	if postCount != 0 {
		t.Fatalf("open submitted %d POST requests after catalog failure", postCount)
	}
}
