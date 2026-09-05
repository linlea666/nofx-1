package trader

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestOKXCopyGuardExitUsesImmutableScopeAndFreshResidual(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		t.Run(side, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []map[string]interface{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == okxOrderPath && r.Method == http.MethodPost:
					var body map[string]interface{}
					_ = json.NewDecoder(r.Body).Decode(&body)
					mu.Lock()
					bodies = append(bodies, body)
					mu.Unlock()
					fmt.Fprint(w, `{"code":"0","data":[{"ordId":"exit1","sCode":"0"}]}`)
				case r.URL.Path == okxInstrumentsPath:
					fmt.Fprint(w, `{"code":"0","data":[{"instId":"BTC-USDT-SWAP","instType":"SWAP","state":"live","ctType":"linear","ctVal":"0.1","lotSz":"1","minSz":"1","tickSz":"0.1"}]}`)
				case r.URL.Path == okxPositionPath:
					fmt.Fprintf(w, `{"code":"0","data":[{"instId":"BTC-USDT-SWAP","posSide":"%s","pos":"9","mgnMode":"cross","posId":"other","avgPx":"100","lever":"10","markPx":"100"},{"instId":"BTC-USDT-SWAP","posSide":"%s","pos":"2","mgnMode":"isolated","posId":"target","avgPx":"100","lever":"10","markPx":"100"}]}`, side, side)
				default:
					fmt.Fprint(w, `{"code":"0","data":[]}`)
				}
			}))
			oldURL := okxBaseURL
			okxBaseURL = srv.URL
			t.Cleanup(func() { okxBaseURL = oldURL; srv.Close() })
			tr := NewOKXTrader("fake", "fake", "fake")
			_ = tr.SetMarginMode("BTCUSDT", true) // Mutable ordinary-order setting must be irrelevant.
			before := 0
			req := CopyGuardExitRequest{CycleID: 1, Symbol: "BTCUSDT", Side: side, MarginMode: "isolated", PositionID: "target", Quantity: .5, ClientOrderID: "exit1", BeforeSubmit: func() error { before++; return nil }}
			if _, err := tr.CloseCopyGuardPosition(req); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			if len(bodies) != 1 || bodies[0]["tdMode"] != "isolated" || bodies[0]["posSide"] != side || bodies[0]["sz"] != "2" || bodies[0]["clOrdId"] != "exit1" || before != 1 {
				t.Fatalf("incorrect scoped residual close: %+v boundary=%d", bodies, before)
			}
			mu.Unlock()
			req.PositionID, req.ClientOrderID = "missing", "exit2"
			if _, err := tr.CloseCopyGuardPosition(req); err == nil {
				t.Fatal("unknown position identity accepted")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 1 {
				t.Fatal("mismatched position posted an order")
			}
		})
	}
}

func TestOKXMarkObservationPreservesVenueTimestamp(t *testing.T) {
	now := time.Now().Add(-20 * time.Second).Truncate(time.Millisecond)
	tr := newOKXTestServer(t, func(path string) (int, string) {
		return 200, fmt.Sprintf(`{"code":"0","data":[{"instId":"BTC-USDT-SWAP","markPx":"100","ts":"%d"}]}`, now.UnixMilli())
	})
	m, err := tr.GetMarkPriceObservation("BTCUSDT")
	if err != nil || !m.ObservedAt.Equal(now) || !m.ValidAt(time.Now()) || m.Source == "" {
		t.Fatalf("observation=%+v err=%v", m, err)
	}
}
