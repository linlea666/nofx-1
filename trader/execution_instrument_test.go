package trader

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

type instrumentRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn instrumentRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func instrumentJSONResponse(t *testing.T, body interface{}) *http.Response {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal instrument response: %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}
}

func binanceCatalogSymbol(symbol, base, quote, margin, status, contractType string) map[string]interface{} {
	return map[string]interface{}{
		"symbol":       symbol,
		"baseAsset":    base,
		"quoteAsset":   quote,
		"marginAsset":  margin,
		"status":       status,
		"contractType": contractType,
		"filters": []map[string]interface{}{
			{"filterType": "PRICE_FILTER", "tickSize": "0.10"},
			{"filterType": "LOT_SIZE", "minQty": "0.001", "stepSize": "0.001"},
			{"filterType": "MIN_NOTIONAL", "notional": "5"},
		},
	}
}

func newBinanceCatalogTestTrader(t *testing.T, symbols ...map[string]interface{}) *FuturesTrader {
	t.Helper()
	client := futures.NewClient("", "")
	client.BaseURL = "https://binance.catalog.test"
	client.HTTPClient = &http.Client{Transport: instrumentRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/fapi/v1/exchangeInfo" {
			t.Fatalf("unexpected Binance catalog request: %s", req.URL.String())
		}
		return instrumentJSONResponse(t, map[string]interface{}{"symbols": symbols}), nil
	})}
	return &FuturesTrader{
		client:           client,
		cacheDuration:    15 * time.Second,
		instrumentsCache: make(map[string]*binanceExecutionInstrument),
	}
}

func TestBinanceResolveExecutionInstrumentExactSettlement(t *testing.T) {
	trader := newBinanceCatalogTestTrader(t,
		binanceCatalogSymbol("BTCUSDT", "BTC", "USDT", "USDT", "TRADING", "PERPETUAL"),
		binanceCatalogSymbol("BTCUSDC", "BTC", "USDC", "USDC", "TRADING", "PERPETUAL"),
		binanceCatalogSymbol("BTCUSD1", "BTC", "USD1", "USD1", "TRADING", "PERPETUAL"),
		binanceCatalogSymbol("XAUUSDT", "XAU", "USDT", "USDT", "TRADING", "TRADIFI_PERPETUAL"),
	)

	for _, tc := range []struct {
		symbol       string
		base         string
		quote        string
		contractType string
	}{
		{symbol: "BTCUSDT", base: "BTC", quote: "USDT", contractType: "PERPETUAL"},
		{symbol: "BTCUSDC", base: "BTC", quote: "USDC", contractType: "PERPETUAL"},
		{symbol: "BTCUSD1", base: "BTC", quote: "USD1", contractType: "PERPETUAL"},
		{symbol: "XAUUSDT", base: "XAU", quote: "USDT", contractType: "TRADIFI_PERPETUAL"},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			got, err := trader.ResolveExecutionInstrument(tc.symbol)
			if err != nil {
				t.Fatalf("ResolveExecutionInstrument(%s): %v", tc.symbol, err)
			}
			if got.SourceSymbol != tc.symbol || got.NativeSymbol != tc.symbol {
				t.Fatalf("symbol identity changed: %+v", got)
			}
			if got.BaseAsset != tc.base || got.QuoteAsset != tc.quote || got.SettleAsset != tc.quote {
				t.Fatalf("unexpected exact asset identity: %+v", got)
			}
			if got.MarketType != "UM" || got.ContractType != tc.contractType || got.Status != "TRADING" {
				t.Fatalf("unexpected contract metadata: %+v", got)
			}
		})
	}

	if _, err := trader.ResolveExecutionInstrument("BTCEUR"); !errors.Is(err, ErrExecutionInstrumentUnsupported) {
		t.Fatalf("unknown quote suffix must fail closed, got %v", err)
	}
}

func TestBinanceResolveExecutionInstrumentRejectsUnsafeCatalogEntries(t *testing.T) {
	valid := func() map[string]interface{} {
		return binanceCatalogSymbol("BTCUSDT", "BTC", "USDT", "USDT", "TRADING", "PERPETUAL")
	}
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{
			name: "non trading",
			mutate: func(symbol map[string]interface{}) {
				symbol["status"] = "BREAK"
			},
		},
		{
			name: "unknown contract type",
			mutate: func(symbol map[string]interface{}) {
				symbol["contractType"] = "CURRENT_QUARTER"
			},
		},
		{
			name: "missing quantity precision",
			mutate: func(symbol map[string]interface{}) {
				symbol["filters"] = []map[string]interface{}{
					{"filterType": "PRICE_FILTER", "tickSize": "0.10"},
					{"filterType": "MIN_NOTIONAL", "notional": "5"},
				}
			},
		},
		{
			name: "missing price precision",
			mutate: func(symbol map[string]interface{}) {
				symbol["filters"] = []map[string]interface{}{
					{"filterType": "LOT_SIZE", "minQty": "0.001", "stepSize": "0.001"},
					{"filterType": "MIN_NOTIONAL", "notional": "5"},
				}
			},
		},
		{
			name: "missing min notional",
			mutate: func(symbol map[string]interface{}) {
				symbol["filters"] = []map[string]interface{}{
					{"filterType": "PRICE_FILTER", "tickSize": "0.10"},
					{"filterType": "LOT_SIZE", "minQty": "0.001", "stepSize": "0.001"},
				}
			},
		},
		{
			name: "missing margin asset",
			mutate: func(symbol map[string]interface{}) {
				symbol["marginAsset"] = ""
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			symbol := valid()
			tc.mutate(symbol)
			trader := newBinanceCatalogTestTrader(t, symbol)
			if _, err := trader.ResolveExecutionInstrument("BTCUSDT"); !errors.Is(err, ErrExecutionInstrumentUnsupported) {
				t.Fatalf("unsafe catalog entry must fail closed, got %v", err)
			}
		})
	}
}

func okxCatalogInstrument(instID, base, quote, settle, state string) map[string]string {
	return map[string]string{
		"instId":     instID,
		"instType":   "SWAP",
		"ctVal":      "0.01",
		"ctValCcy":   base,
		"ctMult":     "1",
		"lotSz":      "1",
		"minSz":      "1",
		"maxMktSz":   "100000",
		"tickSz":     "0.1",
		"ctType":     "linear",
		"state":      state,
		"baseCcy":    "",
		"quoteCcy":   "",
		"settleCcy":  settle,
		"uly":        base + "-" + quote,
		"instFamily": base + "-" + quote,
	}
}

func newOKXCatalogTestTrader(t *testing.T, instruments map[string]map[string]string) (*OKXTrader, *int) {
	t.Helper()
	requestCount := 0
	client := &http.Client{Transport: instrumentRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requestCount++
		if req.URL.Path != okxInstrumentsPath {
			t.Fatalf("unexpected OKX catalog request: %s", req.URL.String())
		}
		instID := req.URL.Query().Get("instId")
		data := []map[string]string{}
		if instrument := instruments[instID]; instrument != nil {
			data = append(data, instrument)
		}
		return instrumentJSONResponse(t, map[string]interface{}{"code": "0", "msg": "", "data": data}), nil
	})}
	return &OKXTrader{
		httpClient:       client,
		cacheDuration:    15 * time.Second,
		instrumentsCache: make(map[string]*OKXInstrument),
		symbolMgnModes:   make(map[string]string),
	}, &requestCount
}

func TestOKXResolveExecutionInstrumentExactSettlement(t *testing.T) {
	instruments := map[string]map[string]string{
		"BTC-USDT-SWAP": okxCatalogInstrument("BTC-USDT-SWAP", "BTC", "USDT", "USDT", "live"),
		"BTC-USDC-SWAP": okxCatalogInstrument("BTC-USDC-SWAP", "BTC", "USDC", "USDC", "live"),
		"BTC-USD1-SWAP": okxCatalogInstrument("BTC-USD1-SWAP", "BTC", "USD1", "USD1", "live"),
	}
	trader, requestCount := newOKXCatalogTestTrader(t, instruments)

	for _, tc := range []struct {
		symbol string
		native string
		quote  string
	}{
		{symbol: "BTCUSDT", native: "BTC-USDT-SWAP", quote: "USDT"},
		{symbol: "BTCUSDC", native: "BTC-USDC-SWAP", quote: "USDC"},
		{symbol: "BTCUSD1", native: "BTC-USD1-SWAP", quote: "USD1"},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			got, err := trader.ResolveExecutionInstrument(tc.symbol)
			if err != nil {
				t.Fatalf("ResolveExecutionInstrument(%s): %v", tc.symbol, err)
			}
			if got.SourceSymbol != tc.symbol || got.NativeSymbol != tc.native {
				t.Fatalf("symbol identity changed: %+v", got)
			}
			if got.BaseAsset != "BTC" || got.QuoteAsset != tc.quote || got.SettleAsset != tc.quote {
				t.Fatalf("unexpected exact asset identity: %+v", got)
			}
			if got.MarketType != "SWAP" || got.Status != "live" {
				t.Fatalf("unexpected contract metadata: %+v", got)
			}
		})
	}

	before := *requestCount
	if _, err := trader.ResolveExecutionInstrument("BTCEUR"); !errors.Is(err, ErrExecutionInstrumentUnsupported) {
		t.Fatalf("unknown quote suffix must fail closed, got %v", err)
	}
	if *requestCount != before {
		t.Fatal("unknown quote suffix must fail before any OKX request")
	}
}

func TestOKXResolveExecutionInstrumentRejectsUnsafeCatalogEntries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{
			name: "non live",
			mutate: func(instrument map[string]string) {
				instrument["state"] = "suspend"
			},
		},
		{
			name: "quote identity mismatch",
			mutate: func(instrument map[string]string) {
				instrument["quoteCcy"] = "USDC"
			},
		},
		{
			name: "contract value currency mismatch",
			mutate: func(instrument map[string]string) {
				instrument["ctValCcy"] = "ETH"
			},
		},
		{
			name: "underlying identity mismatch",
			mutate: func(instrument map[string]string) {
				instrument["uly"] = "ETH-USDT"
			},
		},
		{
			name: "settlement identity mismatch",
			mutate: func(instrument map[string]string) {
				instrument["settleCcy"] = "USDC"
			},
		},
		{
			name: "missing contract value",
			mutate: func(instrument map[string]string) {
				instrument["ctVal"] = "0"
			},
		},
		{
			name: "missing quantity precision",
			mutate: func(instrument map[string]string) {
				instrument["lotSz"] = "0"
			},
		},
		{
			name: "missing minimum size",
			mutate: func(instrument map[string]string) {
				instrument["minSz"] = "0"
			},
		},
		{
			name: "missing price precision",
			mutate: func(instrument map[string]string) {
				instrument["tickSz"] = "0"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instrument := okxCatalogInstrument("BTC-USDT-SWAP", "BTC", "USDT", "USDT", "live")
			tc.mutate(instrument)
			trader, _ := newOKXCatalogTestTrader(t, map[string]map[string]string{"BTC-USDT-SWAP": instrument})
			if _, err := trader.ResolveExecutionInstrument("BTCUSDT"); !errors.Is(err, ErrExecutionInstrumentUnsupported) {
				t.Fatalf("unsafe catalog entry must fail closed, got %v", err)
			}
		})
	}
}

func TestParseOKXExecutionSymbolDoesNotFallbackSettlement(t *testing.T) {
	tests := []struct {
		input  string
		native string
		base   string
		quote  string
	}{
		{input: "BTCUSDT", native: "BTC-USDT-SWAP", base: "BTC", quote: "USDT"},
		{input: "BTCUSDC", native: "BTC-USDC-SWAP", base: "BTC", quote: "USDC"},
		{input: "BTCUSD1", native: "BTC-USD1-SWAP", base: "BTC", quote: "USD1"},
	}
	for _, tc := range tests {
		instID, base, quote, err := parseOKXExecutionSymbol(tc.input)
		if err != nil {
			t.Fatalf("parseOKXExecutionSymbol(%s): %v", tc.input, err)
		}
		if instID != tc.native || base != tc.base || quote != tc.quote {
			t.Fatalf("parseOKXExecutionSymbol(%s) = (%s,%s,%s), want (%s,%s,%s)", tc.input, instID, base, quote, tc.native, tc.base, tc.quote)
		}
	}
	if _, _, _, err := parseOKXExecutionSymbol("BTCEUR"); !errors.Is(err, ErrExecutionInstrumentUnsupported) {
		t.Fatalf("unknown settlement must fail closed, got %v", err)
	}
	if _, _, _, err := parseOKXExecutionSymbol("BTC-USDT-FUTURES"); !errors.Is(err, ErrExecutionInstrumentUnsupported) {
		t.Fatalf("non-SWAP native symbol must fail closed, got %v", err)
	}
}
