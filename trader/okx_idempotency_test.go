package trader

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestOKXDeterministicClientIDAdoptsExistingFillBeforePost(t *testing.T) {
	var mu sync.Mutex
	orderPosts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path == okxOrderPath && r.Method == http.MethodPost {
			orderPosts++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == okxOrderPath && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"ordId":"existing-1","state":"filled","avgPx":"100","accFillSz":"2","fee":"-0.1","side":"buy","ordType":"market","cTime":"1","uTime":"2"}]}`))
		case r.URL.Path == okxInstrumentsPath:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"instId":"BTC-USDT-SWAP","instType":"SWAP","state":"live","baseCcy":"BTC","quoteCcy":"USDT","settleCcy":"USDT","ctType":"linear","ctVal":"0.1","lotSz":"1","minSz":"1","maxMktSz":"1000","tickSz":"0.1"}]}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
		}
	}))
	oldURL := okxBaseURL
	okxBaseURL = srv.URL
	t.Cleanup(func() { okxBaseURL = oldURL; srv.Close() })

	tr := NewOKXTrader("k", "s", "p")
	result, err := tr.OpenLongPreservingOrdersWithClientID("BTCUSDT", 0.2, 5, "copy-open-1")
	if err != nil {
		t.Fatal(err)
	}
	if result["orderId"] != "existing-1" || result["status"] != "FILLED" {
		t.Fatalf("existing fill not adopted: %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if orderPosts != 0 {
		t.Fatalf("idempotent pre-query must prevent duplicate POST, got %d", orderPosts)
	}
}

func TestOKXDeterministicClientIDRejectsTerminalZeroFill(t *testing.T) {
	tr := newOKXTestServer(t, func(path string) (int, string) {
		if strings.Contains(path, "/api/v5/trade/order?") {
			return 200, `{"code":"0","msg":"","data":[{"ordId":"dead-1","state":"canceled","avgPx":"","accFillSz":"0","fee":"0","side":"buy","ordType":"market","cTime":"1","uTime":"2"}]}`
		}
		return 200, `{"code":"0","msg":"","data":[]}`
	})
	_, err := tr.OpenLongPreservingOrdersWithClientID("BTCUSDT", 0.2, 5, "copy-open-dead")
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal zero-fill id must fail closed, got %v", err)
	}
}

func TestOKXAmbiguousPostAdoptsOrderByClientID(t *testing.T) {
	var mu sync.Mutex
	queries := 0
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == okxOrderPath && r.Method == http.MethodGet:
			mu.Lock()
			queries++
			queryNo := queries
			mu.Unlock()
			if queryNo == 1 {
				_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"ordId":"accepted-after-timeout","state":"filled","avgPx":"100","accFillSz":"2","fee":"-0.1","side":"buy","ordType":"market","cTime":"1","uTime":"2"}]}`))
		case r.URL.Path == okxOrderPath && r.Method == http.MethodPost:
			mu.Lock()
			posts++
			mu.Unlock()
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte(`{"code":"500","msg":"timeout","data":[]}`))
		case r.URL.Path == okxInstrumentsPath:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"instId":"BTC-USDT-SWAP","instType":"SWAP","state":"live","baseCcy":"BTC","quoteCcy":"USDT","settleCcy":"USDT","ctType":"linear","ctVal":"0.1","lotSz":"1","minSz":"1","maxMktSz":"1000","tickSz":"0.1"}]}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
		}
	}))
	oldURL := okxBaseURL
	okxBaseURL = srv.URL
	t.Cleanup(func() { okxBaseURL = oldURL; srv.Close() })

	tr := NewOKXTrader("k", "s", "p")
	result, err := tr.OpenLongPreservingOrdersWithClientID("BTCUSDT", 0.2, 5, "copy-open-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if result["orderId"] != "accepted-after-timeout" {
		t.Fatalf("ambiguous POST was not recovered: %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 || queries != 2 {
		t.Fatalf("request sequence posts=%d queries=%d, want 1/2", posts, queries)
	}
}
