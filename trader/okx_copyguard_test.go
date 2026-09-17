package trader

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newOKXTestServer redirects okxBaseURL at a local server and restores it on
// cleanup. handler receives (path+query) and returns the raw response body.
func newOKXTestServer(t *testing.T, handler func(pathWithQuery string) (status int, body string)) *OKXTrader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body := handler(r.URL.RequestURI())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	old := okxBaseURL
	okxBaseURL = srv.URL
	t.Cleanup(func() {
		okxBaseURL = old
		srv.Close()
	})
	return NewOKXTrader("k", "s", "p")
}

func TestOKXErrorClassifiers(t *testing.T) {
	if !IsOKXAlgoAlreadyExists(fmt.Errorf("OKX protective stop rejected: [{  51068 cg10a0 already exists within algoClOrdId and attachAlgoClOrd }]")) {
		t.Fatal("51068 already exists must be classified as duplicate placement")
	}
	if IsOKXAlgoAlreadyExists(fmt.Errorf("OKX API error: code=50002")) {
		t.Fatal("unrelated errors must not be classified as 51068")
	}
	if !IsOKXAlgoTerminalCancelError(fmt.Errorf(`OKX cancel amend failed: [{51400 Order cancellation failed as the order has been filled, canceled or does not exist}]`)) {
		t.Fatal("51400 filled/canceled must be classified as terminal cancel")
	}
	if IsOKXAlgoTerminalCancelError(fmt.Errorf("51400 Order cancellation failed for other reason")) {
		t.Fatal("51400 without terminal wording must not be classified as terminal")
	}
}

func TestOKXMarkPriceHistoryIsSortedAndAuthoritative(t *testing.T) {
	start := time.UnixMilli(1700000000000).UTC().Truncate(time.Minute)
	trader := newOKXTestServer(t, func(path string) (int, string) {
		if strings.Contains(path, okxPositionModePath) {
			return 200, `{"code":"0","msg":"","data":[{"sCode":"0"}]}`
		}
		if !strings.Contains(path, okxMarkHistoryPath) || !strings.Contains(path, "instId=BTC-USDT-SWAP") || !strings.Contains(path, "bar=1m") {
			t.Fatalf("unexpected OKX mark-history request: %s", path)
		}
		return 200, fmt.Sprintf(`{"code":"0","msg":"","data":[["%d","101","103","100","102","1"],["%d","100","102","99","101","1"],["%d","102","104","101","103","0"]]}`,
			start.Add(time.Minute).UnixMilli(), start.UnixMilli(), start.Add(2*time.Minute).UnixMilli())
	})
	rows, err := trader.GetMarkPriceHistory("BTCUSDT", start.Add(30*time.Second), start.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !rows[0].OpenTime.Equal(start) || rows[0].Low != 99 || rows[1].High != 103 {
		t.Fatalf("unexpected OKX mark candles: %+v", rows)
	}
}

func TestOKXGetMarkPriceUsesDedicatedEndpointWithoutPosition(t *testing.T) {
	trader := newOKXTestServer(t, func(path string) (int, string) {
		if strings.Contains(path, okxPositionModePath) {
			return 200, `{"code":"0","msg":"","data":[{"sCode":"0"}]}`
		}
		if !strings.Contains(path, okxMarkPricePath) || !strings.Contains(path, "instId=BTC-USDT-SWAP") {
			t.Fatalf("unexpected OKX mark-price request: %s", path)
		}
		return 200, `{"code":"0","msg":"","data":[{"instId":"BTC-USDT-SWAP","markPx":"61234.50"}]}`
	})
	price, err := trader.GetMarkPrice("BTCUSDT")
	if err != nil || price != 61234.5 {
		t.Fatalf("unexpected OKX mark price %.8f err=%v", price, err)
	}
}

func TestOKXCopyGuardPositionModeIsQueriedAndVerified(t *testing.T) {
	t.Run("long short mode", func(t *testing.T) {
		trader := newOKXTestServer(t, func(path string) (int, string) {
			if strings.Contains(path, okxAccountConfigPath) {
				return 200, `{"code":"0","msg":"","data":[{"posMode":"long_short_mode"}]}`
			}
			return 200, `{"code":"0","msg":"","data":[]}`
		})
		if err := trader.ValidateCopyGuardPositionMode(); err != nil {
			t.Fatalf("long/short mode rejected: %v", err)
		}
	})
	t.Run("mode change rejected", func(t *testing.T) {
		trader := newOKXTestServer(t, func(path string) (int, string) {
			if strings.Contains(path, okxAccountConfigPath) {
				return 200, `{"code":"0","msg":"","data":[{"posMode":"net_mode"}]}`
			}
			if strings.Contains(path, okxPositionModePath) {
				return 200, `{"code":"51000","msg":"position mode cannot be changed","data":[]}`
			}
			return 200, `{"code":"0","msg":"","data":[]}`
		})
		if err := trader.ValidateCopyGuardPositionMode(); err == nil {
			t.Fatal("unsafe OKX net mode was accepted")
		}
	})
}

func TestGetProtectiveStopErrorClassification(t *testing.T) {
	t.Run("query failure is not not-found", func(t *testing.T) {
		trader := newOKXTestServer(t, func(string) (int, string) {
			return 200, `{"code":"50011","msg":"rate limited","data":[]}`
		})
		_, err := trader.GetProtectiveStop("123", "ETHUSDT")
		if err == nil || errors.Is(err, ErrProtectiveStopNotFound) {
			t.Fatalf("query failure must not report not-found: %v", err)
		}
	})
	t.Run("confirmed absence returns sentinel", func(t *testing.T) {
		trader := newOKXTestServer(t, func(string) (int, string) {
			return 200, `{"code":"0","msg":"","data":[]}`
		})
		_, err := trader.GetProtectiveStopByClientID("cg10a0", "ETHUSDT")
		if !errors.Is(err, ErrProtectiveStopNotFound) {
			t.Fatalf("confirmed absence must return ErrProtectiveStopNotFound: %v", err)
		}
	})
	t.Run("found order is returned with state", func(t *testing.T) {
		trader := newOKXTestServer(t, func(path string) (int, string) {
			if strings.Contains(path, "order-algo?") {
				return 200, `{"code":"0","msg":"","data":[{"algoId":"999","instId":"ETH-USDT-SWAP","algoClOrdId":"cg10a0","posSide":"long","tdMode":"cross","sz":"1","slTriggerPx":"1711.63","slTriggerPxType":"mark","state":"live","ordId":""}]}`
			}
			if strings.Contains(path, "instruments") {
				return 200, `{"code":"0","msg":"","data":[{"instId":"ETH-USDT-SWAP","ctVal":"0.1"}]}`
			}
			return 200, `{"code":"0","msg":"","data":[]}`
		})
		order, err := trader.GetProtectiveStopByClientID("cg10a0", "ETHUSDT")
		if err != nil {
			t.Fatalf("lookup failed: %v", err)
		}
		if order.AlgoID != "999" || order.State != "live" || order.ClientID != "cg10a0" {
			t.Fatalf("unexpected order: %+v", order)
		}
		if order.Quantity != 0.1 {
			t.Fatalf("protective quantity=%v want base quantity 0.1", order.Quantity)
		}
	})
	t.Run("quantity conversion fails closed without instrument metadata", func(t *testing.T) {
		trader := newOKXTestServer(t, func(path string) (int, string) {
			if strings.Contains(path, "order-algo?") {
				return 200, `{"code":"0","msg":"","data":[{"algoId":"999","instId":"ETH-USDT-SWAP","algoClOrdId":"cg10a0","posSide":"long","tdMode":"cross","sz":"1","slTriggerPx":"1711.63","state":"live"}]}`
			}
			if strings.Contains(path, "instruments") {
				return 200, `{"code":"50011","msg":"rate limited","data":[]}`
			}
			return 200, `{"code":"0","msg":"","data":[]}`
		})
		_, err := trader.GetProtectiveStopByClientID("cg10a0", "ETHUSDT")
		if err == nil || errors.Is(err, ErrProtectiveStopNotFound) {
			t.Fatalf("missing quantity metadata must stay unknown, got %v", err)
		}
	})
}

func TestGetPositionsQuantityConversionFailsClosedWithoutInstrumentMetadata(t *testing.T) {
	trader := newOKXTestServer(t, func(path string) (int, string) {
		switch {
		case strings.Contains(path, "/account/positions"):
			return 200, `{"code":"0","msg":"","data":[{"instId":"ETH-USDT-SWAP","posSide":"long","pos":"2","avgPx":"1700","markPx":"1710","mgnMode":"cross"}]}`
		case strings.Contains(path, "instruments"):
			return 200, `{"code":"50011","msg":"rate limited","data":[]}`
		default:
			return 200, `{"code":"0","msg":"","data":[]}`
		}
	})
	if _, err := trader.GetPositions(); err == nil {
		t.Fatal("position query must fail closed instead of returning raw contracts")
	}
}

func TestOKXImmutableFillHistoryPaginatesAndDeduplicates(t *testing.T) {
	trader := newOKXTestServer(t, func(path string) (int, string) {
		switch {
		case strings.Contains(path, "public/instruments"):
			return 200, `{"code":"0","msg":"","data":[{"instId":"BTC-USDT-SWAP","ctVal":"0.01","lotSz":"0.01"}]}`
		case strings.Contains(path, "fills-history") && strings.Contains(path, "after=2"):
			return 200, `{"code":"0","msg":"","data":[
				{"billId":"2","tradeId":"trade-2","ordId":"order-2","instId":"BTC-USDT-SWAP","side":"sell","posSide":"long","fillSz":"2","fillPx":"62000","fillPnl":"-2","fee":"-0.2","fillTime":"2000"},
				{"billId":"1","tradeId":"trade-1","ordId":"order-1","instId":"BTC-USDT-SWAP","side":"sell","posSide":"long","fillSz":"1","fillPx":"61000","fillPnl":"1","fee":"-0.1","fillTime":"1000"}
			]}`
		case strings.Contains(path, "fills-history") && strings.Contains(path, "after=1"):
			return 200, `{"code":"0","msg":"","data":[]}`
		case strings.Contains(path, "fills-history"):
			if !strings.Contains(path, "begin=") || !strings.Contains(path, "end=") || !strings.Contains(path, "limit=2") {
				t.Fatalf("missing bounded fill query: %s", path)
			}
			return 200, `{"code":"0","msg":"","data":[
				{"billId":"3","tradeId":"trade-3","ordId":"order-3","instId":"BTC-USDT-SWAP","side":"sell","posSide":"long","fillSz":"3","fillPx":"63000","fillPnl":"3","fee":"-0.3","fillTime":"3000"},
				{"billId":"2","tradeId":"trade-2","ordId":"order-2","instId":"BTC-USDT-SWAP","side":"sell","posSide":"long","fillSz":"2","fillPx":"62000","fillPnl":"-2","fee":"-0.2","fillTime":"2000"}
			]}`
		default:
			return 200, `{"code":"0","msg":"","data":[]}`
		}
	})
	trades, err := trader.GetTradesForSymbol("BTCUSDT", time.UnixMilli(500), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 3 {
		t.Fatalf("deduplicated fills=%d want=3: %+v", len(trades), trades)
	}
	if trades[0].TradeID != "trade:BTC-USDT-SWAP:trade-1" ||
		trades[2].TradeID != "trade:BTC-USDT-SWAP:trade-3" {
		t.Fatalf("fills are not chronological: %+v", trades)
	}
	if trades[0].Quantity != 0.01 || trades[2].Quantity != 0.03 {
		t.Fatalf("contracts were not converted to base quantity: %+v", trades)
	}
	if trades[1].Fee != 0.2 || trades[1].Side != "SELL" || trades[1].PositionSide != "LONG" {
		t.Fatalf("fill economics/direction were not normalized: %+v", trades[1])
	}
}

func TestOKXImmutableFillHistoryFailsClosedOnLaterPageError(t *testing.T) {
	trader := newOKXTestServer(t, func(path string) (int, string) {
		if strings.Contains(path, "fills-history") && strings.Contains(path, "after=2") {
			return 200, `{"code":"50011","msg":"rate limited","data":[]}`
		}
		if strings.Contains(path, "fills-history") {
			return 200, `{"code":"0","msg":"","data":[
				{"billId":"3","tradeId":"trade-3","instId":"BTC-USDT-SWAP","side":"sell","posSide":"long","fillSz":"1","fillPx":"63000","fillPnl":"0","fee":"0","fillTime":"3000"},
				{"billId":"2","tradeId":"trade-2","instId":"BTC-USDT-SWAP","side":"sell","posSide":"long","fillSz":"1","fillPx":"62000","fillPnl":"0","fee":"0","fillTime":"2000"}
			]}`
		}
		if strings.Contains(path, "public/instruments") {
			return 200, `{"code":"0","msg":"","data":[{"instId":"BTC-USDT-SWAP","ctVal":"0.01","lotSz":"0.01"}]}`
		}
		return 200, `{"code":"0","msg":"","data":[]}`
	})
	if _, err := trader.GetTradesForSymbol("BTCUSDT", time.UnixMilli(500), 2); err == nil {
		t.Fatal("partial fill history was returned after a later-page failure")
	}
}

func TestOKXPendingOrderSnapshotIncludesRegularAndConditional(t *testing.T) {
	trader := newOKXTestServer(t, func(path string) (int, string) {
		switch {
		case strings.Contains(path, "orders-algo-pending"):
			if !strings.Contains(path, "ordType=conditional") {
				t.Fatalf("conditional query missing ordType: %s", path)
			}
			return 200, `{"code":"0","msg":"","data":[{"algoId":"algo-1","instId":"ETH-USDT-SWAP","state":"live","ordType":"conditional","slTriggerPx":"1700"}]}`
		case strings.Contains(path, "orders-pending"):
			return 200, `{"code":"0","msg":"","data":[{"ordId":"order-1","instId":"BTC-USDT-SWAP","state":"live","ordType":"limit"}]}`
		default:
			return 200, `{"code":"0","msg":"","data":[]}`
		}
	})
	orders, err := trader.GetPendingOrdersFresh()
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 || orders[0].Protective || !orders[1].Protective {
		t.Fatalf("unexpected pending-order snapshot: %+v", orders)
	}
}

// Detail lookups must use the endpoint that actually supports algoClOrdId.
func TestGetProtectiveStopQueryParams(t *testing.T) {
	var captured []string
	tr := newOKXTestServer(t, func(path string) (int, string) {
		if strings.Contains(path, "order-algo?") {
			captured = append(captured, path)
		}
		return 200, `{"code":"0","data":[]}`
	})
	_, _ = tr.GetProtectiveStop("algo-1", "ETHUSDT")
	_, _ = tr.GetProtectiveStopByClientID("cg15a0", "ETHUSDT")
	if len(captured) != 2 || captured[0] != "/api/v5/trade/order-algo?algoId=algo-1" || captured[1] != "/api/v5/trade/order-algo?algoClOrdId=cg15a0" {
		t.Fatalf("wrong detail identity queries: %v", captured)
	}
}

func TestGetProtectiveStop51603IsConfirmedAbsence(t *testing.T) {
	for _, code := range []string{"51603", "50011"} {
		t.Run(code, func(t *testing.T) {
			tr := newOKXTestServer(t, func(path string) (int, string) {
				if strings.Contains(path, "order-algo?") {
					return 200, `{"code":"` + code + `","msg":"Order does not exist","data":[]}`
				}
				return 200, `{"code":"0","data":[]}`
			})
			_, err := tr.GetProtectiveStop("missing", "ETHUSDT")
			if err == nil || errors.Is(err, ErrProtectiveStopNotFound) != (code == "51603") {
				t.Fatalf("wrong query classification: %v", err)
			}
		})
	}
}

func TestProtectiveStopDetailRejectsWrongIdentity(t *testing.T) {
	tr := newOKXTestServer(t, func(path string) (int, string) {
		if strings.Contains(path, "order-algo?") {
			return 200, `{"code":"0","data":[{"algoId":"different","algoClOrdId":"another","instId":"BTC-USDT-SWAP","sz":"1"}]}`
		}
		return 200, `{"code":"0","data":[]}`
	})
	if _, err := tr.GetProtectiveStopByClientID("cg10a0", "ETHUSDT"); err == nil || errors.Is(err, ErrProtectiveStopNotFound) {
		t.Fatalf("wrong order was adopted: %v", err)
	}
}

func TestGetClosedPnLQueryDirection(t *testing.T) {
	var captured []string
	trader := newOKXTestServer(t, func(path string) (int, string) {
		// 构造 OKXTrader 会触发 set-position-mode 等无关请求，这里只关心历史仓位查询
		if strings.Contains(path, "positions-history") {
			captured = append(captured, path)
		}
		return 200, `{"code":"0","msg":"","data":[]}`
	})
	openedAt := time.Now().Add(-5 * time.Minute)
	if _, err := trader.GetClosedPnL(openedAt, 50); err != nil {
		t.Fatalf("GetClosedPnL: %v", err)
	}
	if len(captured) != 1 || !strings.Contains(captured[0], fmt.Sprintf("before=%d", openedAt.UnixMilli())) {
		t.Fatalf("GetClosedPnL must query records newer than openedAt via before=, got %v", captured)
	}
	if strings.Contains(captured[0], "after=") {
		t.Fatalf("after= excludes freshly closed positions and must not be used: %v", captured)
	}
	captured = nil
	if _, err := trader.GetClosedPnLByPositionID("ETHUSDT", "pos-1", 20); err != nil {
		t.Fatalf("GetClosedPnLByPositionID: %v", err)
	}
	if len(captured) != 1 || !strings.Contains(captured[0], "posId=pos-1") {
		t.Fatalf("position id query must filter by posId, got %v", captured)
	}
}

// TestAlgoRequestBodyShapes reproduces the live failure where every amend was
// rejected with 50002 "Incorrect json data format": OKX amend-algos takes a
// single JSON object while cancel-algos takes an array. Position adds could
// never widen the protective stop (cycles 25/27/30, coverage stuck at 37-50%).
func TestAlgoRequestBodyShapes(t *testing.T) {
	bodies := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if r.Method == http.MethodPost && strings.Contains(path, "/trade/") {
			raw, _ := io.ReadAll(r.Body)
			bodies[path] = raw
		}
		w.WriteHeader(200)
		switch {
		case strings.Contains(path, "instruments"):
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"instId":"ETH-USDT-SWAP","ctVal":"0.1","ctMult":"1","lotSz":"1","minSz":"1","maxMktSz":"100000","tickSz":"0.01","ctType":"linear"}]}`))
		case strings.Contains(path, "amend-algos"), strings.Contains(path, "cancel-algos"):
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"algoId":"1","sCode":"0","sMsg":""}]}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
		}
	}))
	old := okxBaseURL
	okxBaseURL = srv.URL
	t.Cleanup(func() {
		okxBaseURL = old
		srv.Close()
	})
	trader := NewOKXTrader("k", "s", "p")

	if err := trader.AmendProtectiveStop("algo-1", ProtectiveStopRequest{Symbol: "ETHUSDT", PositionSide: "short", Quantity: 0.1, TriggerPrice: 1765.77, TriggerType: "mark"}); err != nil {
		t.Fatalf("AmendProtectiveStop: %v", err)
	}
	amendBody := bodies["/api/v5/trade/amend-algos"]
	if len(amendBody) == 0 {
		t.Fatal("amend-algos request body was not captured")
	}
	var amendObj map[string]interface{}
	if err := json.Unmarshal(amendBody, &amendObj); err != nil {
		t.Fatalf("amend-algos body must be a single JSON object (OKX 50002 otherwise), got: %s", amendBody)
	}
	for _, field := range []string{"instId", "algoId", "newSz", "newSlTriggerPx"} {
		if _, ok := amendObj[field]; !ok {
			t.Fatalf("amend-algos body missing %s: %s", field, amendBody)
		}
	}
	if err := trader.AmendProtectiveStop("full-position", ProtectiveStopRequest{Symbol: "ETHUSDT", PositionSide: "short", Quantity: .3, TriggerPrice: 1765.77, TriggerType: "mark", CoverageMode: ProtectiveStopCoverageCloseAll}); err != nil {
		t.Fatal(err)
	}
	var fullPositionBody map[string]interface{}
	if err := json.Unmarshal(bodies["/api/v5/trade/amend-algos"], &fullPositionBody); err != nil {
		t.Fatal(err)
	}
	if _, exists := fullPositionBody["newSz"]; exists {
		t.Fatal("manual closeFraction stop was changed to a fixed-size order")
	}

	if err := trader.CancelProtectiveStop("algo-1", "ETHUSDT"); err != nil {
		t.Fatalf("CancelProtectiveStop: %v", err)
	}
	cancelBody := bodies["/api/v5/trade/cancel-algos"]
	if len(cancelBody) == 0 {
		t.Fatal("cancel-algos request body was not captured")
	}
	var cancelArr []map[string]interface{}
	if err := json.Unmarshal(cancelBody, &cancelArr); err != nil || len(cancelArr) == 0 {
		t.Fatalf("cancel-algos body must stay a JSON array, got: %s", cancelBody)
	}
}

func TestOKXProtectiveDetailRecognizesFullPositionManualStop(t *testing.T) {
	ex := newOKXTestServer(t, func(path string) (int, string) {
		if strings.Contains(path, okxPositionModePath) {
			return 200, `{"code":"0","data":[{"sCode":"0"}]}`
		}
		if !strings.Contains(path, "/api/v5/trade/order-algo?") {
			t.Fatalf("unexpected request: %s", path)
		}
		return 200, `{"code":"0","data":[{"algoId":"manual-all","instId":"ETH-USDT-SWAP","posSide":"long","tdMode":"cross","sz":"","closeFraction":"1","slTriggerPx":"90","slTriggerPxType":"mark","state":"live"}]}`
	})
	order, err := ex.GetProtectiveStop("manual-all", "ETHUSDT")
	if err != nil || order.CoverageMode != ProtectiveStopCoverageCloseAll || order.TriggerPrice != 90 {
		t.Fatalf("full position stop lost: %+v %v", order, err)
	}
}

func TestCancelOrderByClientIDRequestAndAcknowledgement(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == okxCancelOrderPath {
			captured, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"ordId":"123","clOrdId":"nfxABC123","sCode":"0","sMsg":""}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
	}))
	old := okxBaseURL
	okxBaseURL = srv.URL
	t.Cleanup(func() {
		okxBaseURL = old
		srv.Close()
	})
	okx := NewOKXTrader("k", "s", "p")
	if err := okx.CancelOrderByClientID("ETHUSDT", "nfxABC123"); err != nil {
		t.Fatalf("CancelOrderByClientID: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("cancel-order body must be a JSON object, got %s: %v", captured, err)
	}
	if body["instId"] != "ETH-USDT-SWAP" || body["clOrdId"] != "nfxABC123" {
		t.Fatalf("unexpected cancel-order body: %v", body)
	}

	t.Run("per-item rejection is returned", func(t *testing.T) {
		rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"clOrdId":"nfxABC123","sCode":"51400","sMsg":"already filled"}]}`))
		}))
		defer rejecting.Close()
		okxBaseURL = rejecting.URL
		if err := okx.CancelOrderByClientID("ETHUSDT", "nfxABC123"); err == nil {
			t.Fatal("per-item cancel rejection must be returned")
		}
	})
}

func TestValidateOKXAlgoAckChecksPerItemResult(t *testing.T) {
	if err := validateOKXAlgoAck([]byte(`[{"algoId":"1","sCode":"0","sMsg":""}]`), "amend"); err != nil {
		t.Fatalf("successful acknowledgement rejected: %v", err)
	}
	if err := validateOKXAlgoAck([]byte(`[{"algoId":"1","sCode":"51000","sMsg":"invalid quantity"}]`), "amend"); err == nil {
		t.Fatal("per-item rejection must be returned as an error")
	}
	if err := validateOKXAlgoAck([]byte(`[]`), "cancel"); err == nil {
		t.Fatal("empty acknowledgement must be rejected")
	}
}
