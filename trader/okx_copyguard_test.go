package trader

import (
	"errors"
	"fmt"
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
			if strings.Contains(path, "orders-algo-pending") {
				return 200, `{"code":"0","msg":"","data":[{"algoId":"999","algoClOrdId":"cg10a0","posSide":"long","tdMode":"cross","sz":"1","slTriggerPx":"1711.63","slTriggerPxType":"mark","state":"live","ordId":""}]}`
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
	})
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
