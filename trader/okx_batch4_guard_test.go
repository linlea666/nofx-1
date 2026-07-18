package trader

// 第四批回归测试（OKX 执行体）：
//   - M12 跟单路径杠杆设置失败 fail-closed（AI 路径保持不阻断）
//   - M14 close 传入数量时必须校验存在匹配仓位

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type okxGuardServerState struct {
	mu          sync.Mutex
	orderPosts  int
	leverFails  bool
	hasPosition bool
}

func newOKXGuardServer(t *testing.T, st *okxGuardServerState) *OKXTrader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == okxLeveragePath:
			st.mu.Lock()
			fail := st.leverFails
			st.mu.Unlock()
			if fail {
				_, _ = w.Write([]byte(`{"code":"2","msg":"set leverage rejected","data":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
		case r.URL.Path == okxOrderPath && r.Method == http.MethodPost:
			st.mu.Lock()
			st.orderPosts++
			st.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"ordId":"order-1","clOrdId":"c1","sCode":"0","sMsg":""}]}`))
		case r.URL.Path == okxOrderPath && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
		case r.URL.Path == okxInstrumentsPath:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"instId":"BTC-USDT-SWAP","instType":"SWAP","state":"live","baseCcy":"BTC","quoteCcy":"USDT","settleCcy":"USDT","ctType":"linear","ctVal":"0.1","lotSz":"1","minSz":"1","maxMktSz":"1000","tickSz":"0.1"}]}`))
		case strings.HasPrefix(r.URL.Path, okxPositionPath):
			st.mu.Lock()
			has := st.hasPosition
			st.mu.Unlock()
			if has {
				_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"instId":"BTC-USDT-SWAP","instType":"SWAP","posSide":"long","pos":"5","avgPx":"100","upl":"0","lever":"5","mgnMode":"isolated","posId":"p1","markPx":"100"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
		}
	}))
	oldURL := okxBaseURL
	okxBaseURL = srv.URL
	t.Cleanup(func() { okxBaseURL = oldURL; srv.Close() })
	return NewOKXTrader("k", "s", "p")
}

// M12：跟单路径（带 clOrdId）杠杆同步失败必须拒单，不允许以旧杠杆开仓。
func TestOKXCopyOpenFailsClosedWhenLeverageSyncFails(t *testing.T) {
	st := &okxGuardServerState{leverFails: true}
	tr := newOKXGuardServer(t, st)

	_, err := tr.OpenLongPreservingOrdersWithClientID("BTCUSDT", 0.2, 5, "cg-lev-1")
	if err == nil || !strings.Contains(err.Error(), "unsynced leverage") {
		t.Fatalf("copy-trade open must fail closed on leverage failure, got %v", err)
	}
	_, err = tr.OpenShortPreservingOrdersWithClientID("BTCUSDT", 0.2, 5, "cg-lev-2")
	if err == nil || !strings.Contains(err.Error(), "unsynced leverage") {
		t.Fatalf("copy-trade short open must fail closed on leverage failure, got %v", err)
	}
	st.mu.Lock()
	posts := st.orderPosts
	st.mu.Unlock()
	if posts != 0 {
		t.Fatalf("no order may be posted before leverage is synced, got %d", posts)
	}
}

// M12 对照：AI 路径（无 clOrdId）保持原行为，杠杆失败仅告警不阻断。
func TestOKXAIOpenProceedsWhenLeverageSyncFails(t *testing.T) {
	st := &okxGuardServerState{leverFails: true}
	tr := newOKXGuardServer(t, st)

	result, err := tr.OpenLongPreservingOrders("BTCUSDT", 0.2, 5)
	if err != nil {
		t.Fatalf("AI-path open must not be blocked by leverage failure: %v", err)
	}
	if result["orderId"] != "order-1" {
		t.Fatalf("unexpected order result: %+v", result)
	}
}

// M14：显式传入数量但无匹配仓位时必须拒绝下 reduce 单
// （此前会带数量盲发订单，单向持仓模式下可能反向开仓）。
func TestOKXCloseWithExplicitQuantityRequiresMatchingPosition(t *testing.T) {
	st := &okxGuardServerState{}
	tr := newOKXGuardServer(t, st)

	if _, err := tr.CloseLong("BTCUSDT", 0.5); err == nil || !strings.Contains(err.Error(), "position not found") {
		t.Fatalf("close long without a matching position must fail, got %v", err)
	}
	if _, err := tr.CloseShort("BTCUSDT", 0.5); err == nil || !strings.Contains(err.Error(), "position not found") {
		t.Fatalf("close short without a matching position must fail, got %v", err)
	}
	st.mu.Lock()
	posts := st.orderPosts
	st.mu.Unlock()
	if posts != 0 {
		t.Fatalf("no reduce order may be posted without a matching position, got %d", posts)
	}

	// 有匹配仓位时部分平仓照常放行
	st.mu.Lock()
	st.hasPosition = true
	st.mu.Unlock()
	tr.invalidatePositionsCache()
	if _, err := tr.CloseLong("BTCUSDT", 0.2); err != nil {
		t.Fatalf("partial close with a live position must succeed: %v", err)
	}
}
