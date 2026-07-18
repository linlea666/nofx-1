package trader

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

func newBinanceProtectiveTestTrader(client *futures.Client) *FuturesTrader {
	return &FuturesTrader{
		client: client, cacheDuration: 15 * time.Second,
		instrumentsCache: map[string]*binanceExecutionInstrument{
			"BTCUSDT": {
				ExecutionInstrument: ExecutionInstrument{SourceSymbol: "BTCUSDT", NativeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", SettleAsset: "USDT", MarketType: "UM", ContractType: "PERPETUAL", Status: "TRADING", PriceTickSize: 0.1, BaseQuantityStep: 0.001},
				StepSize:            0.001, MinQuantity: 0.001, MaxQuantity: 100, TickSize: 0.1, MinNotional: 5,
			},
		},
		instrumentsCacheTime: time.Now(),
	}
}

func TestBinanceProtectiveReplacementReturnsPendingWithoutCancelingOld(t *testing.T) {
	existing := &ProtectiveStopOrder{AlgoID: "10", ClientID: "old", Symbol: "BTCUSDT", PositionSide: "long", Quantity: 1, TriggerPrice: 90, State: "live"}
	req := ProtectiveStopRequest{Symbol: "BTCUSDT", PositionSide: "long", Quantity: 1, TriggerPrice: 95, TriggerType: "mark", ClientID: "cg1a0"}
	expectedClientID := deriveBinanceProtectiveClientID(req.ClientID, "replace|10|BTCUSDT|long|1.000000000000|95.000000000000|mark")
	deleteCount := 0
	client := futures.NewClient("", "")
	client.BaseURL = "https://binance.protection.test"
	client.HTTPClient = &http.Client{Transport: binanceHardeningRoundTripFunc(func(httpReq *http.Request) (*http.Response, error) {
		if httpReq.Method == http.MethodDelete {
			deleteCount++
		}
		switch {
		case httpReq.Method == http.MethodGet && httpReq.URL.Query().Get("clientAlgoId") == expectedClientID:
			return binanceHardeningJSONResponse(t, http.StatusBadRequest, map[string]interface{}{"code": -2013, "msg": "Order does not exist."}), nil
		case httpReq.Method == http.MethodPost && httpReq.URL.Path == "/fapi/v1/algoOrder":
			if err := httpReq.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if got := httpReq.Form.Get("clientAlgoId"); got != expectedClientID {
				t.Fatalf("replacement client id=%q want %q", got, expectedClientID)
			}
			return binanceHardeningJSONResponse(t, http.StatusOK, binanceAlgoResponse("20", expectedClientID, "NEW", "95")), nil
		case httpReq.Method == http.MethodGet && httpReq.URL.Query().Get("algoId") == "20":
			return binanceHardeningJSONResponse(t, http.StatusOK, binanceAlgoResponse("20", expectedClientID, "NEW", "95")), nil
		default:
			t.Fatalf("unexpected protective replacement request: %s %s", httpReq.Method, httpReq.URL.String())
			return nil, nil
		}
	})}

	result, err := newBinanceProtectiveTestTrader(client).EnsureProtectiveStop(existing, req)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Current == nil || result.Current.AlgoID != "20" || result.Retiring == nil || result.Retiring.AlgoID != "10" || !result.ReplacementPending {
		t.Fatalf("replacement state not recoverable: %+v", result)
	}
	if deleteCount != 0 {
		t.Fatalf("executor canceled old order before durable persistence: deletes=%d", deleteCount)
	}
}

func TestBinanceProtectiveTerminalClientIDAdvancesDeterministically(t *testing.T) {
	req := ProtectiveStopRequest{Symbol: "BTCUSDT", PositionSide: "long", Quantity: 1, TriggerPrice: 95, TriggerType: "mark", ClientID: "cg-terminal"}
	alternate := deriveBinanceProtectiveClientID(req.ClientID, "terminal|11|canceled|long|1.000000000000|95.000000000000|mark|0")
	client := futures.NewClient("", "")
	client.BaseURL = "https://binance.protection.test"
	client.HTTPClient = &http.Client{Transport: binanceHardeningRoundTripFunc(func(httpReq *http.Request) (*http.Response, error) {
		switch {
		case httpReq.Method == http.MethodGet && httpReq.URL.Query().Get("clientAlgoId") == req.ClientID:
			return binanceHardeningJSONResponse(t, http.StatusOK, binanceAlgoResponse("11", req.ClientID, "CANCELED", "90")), nil
		case httpReq.Method == http.MethodGet && httpReq.URL.Query().Get("clientAlgoId") == alternate:
			return binanceHardeningJSONResponse(t, http.StatusBadRequest, map[string]interface{}{"code": -2013, "msg": "Order does not exist."}), nil
		case httpReq.Method == http.MethodPost:
			if err := httpReq.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if got := httpReq.Form.Get("clientAlgoId"); got != alternate {
				t.Fatalf("terminal id did not advance deterministically: got %q want %q", got, alternate)
			}
			return binanceHardeningJSONResponse(t, http.StatusOK, binanceAlgoResponse("12", alternate, "NEW", "95")), nil
		default:
			t.Fatalf("unexpected request: %s %s", httpReq.Method, httpReq.URL.String())
			return nil, nil
		}
	})}
	placed, err := newBinanceProtectiveTestTrader(client).PlaceProtectiveStop(req)
	if err != nil {
		t.Fatal(err)
	}
	if placed.ClientID != alternate || placed.AlgoID != "12" {
		t.Fatalf("unexpected replacement: %+v", placed)
	}
}

func binanceAlgoResponse(algoID, clientID, state, trigger string) map[string]interface{} {
	id, _ := strconv.ParseInt(strings.TrimSpace(algoID), 10, 64)
	return map[string]interface{}{
		"algoId": id, "clientAlgoId": clientID, "symbol": "BTCUSDT",
		"side": "SELL", "positionSide": "LONG", "quantity": "1", "algoStatus": state,
		"triggerPrice": trigger, "workingType": "MARK_PRICE", "closePosition": true,
	}
}
