package trader

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

func TestBinanceContinuitySplitsFullPagesWithoutTruncatingTrades(t *testing.T) {
	start := time.Now().Add(-10 * 24 * time.Hour)
	client := futures.NewClient("", "")
	calls := 0
	client.HTTPClient = &http.Client{Transport: binanceHardeningRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		q := req.URL.Query()
		if req.URL.Path != "/fapi/v1/userTrades" {
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		begin, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
		end, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
		if q.Get("fromId") != "" || q.Get("fromID") != "" || end-begin >= int64((7*24*time.Hour)/time.Millisecond) {
			t.Fatalf("invalid time window: %v", q)
		}
		rows := []map[string]interface{}{}
		for id := 1; id <= 3; id++ {
			at := start.Add(time.Duration(id) * time.Hour).UnixMilli()
			if at >= begin && at <= end {
				rows = append(rows, map[string]interface{}{"id": id, "orderId": id + 100, "symbol": "ETHUSDT", "side": "BUY", "positionSide": "LONG", "price": "100", "qty": "1", "time": at})
			}
		}
		if len(rows) > 2 {
			rows = rows[:2]
		}
		return binanceHardeningJSONResponse(t, http.StatusOK, rows), nil
	})}
	ex := &FuturesTrader{client: client}
	fills, err := ex.GetTradesForSymbol("ETHUSDT", start, 2)
	if err != nil || len(fills) != 3 || calls < 4 {
		t.Fatalf("truncated history: %+v %v pages=%d", fills, err, calls)
	}
}

func TestOKXContinuityFiltersMarginScopeAndFailsOnUnknownOrder(t *testing.T) {
	now := time.Now().Add(-time.Minute)
	unknown := false
	queries := 0
	ex := newOKXTestServer(t, func(path string) (int, string) {
		u, _ := url.Parse(path)
		switch {
		case strings.Contains(path, okxPositionModePath):
			return 200, `{"code":"0","data":[{"sCode":"0"}]}`
		case strings.Contains(path, okxFillsHistoryPath):
			return 200, fmt.Sprintf(`{"code":"0","data":[{"billId":"1","tradeId":"1","ordId":"cross-order","instId":"ETH-USDT-SWAP","side":"buy","posSide":"long","fillSz":"1","fillPx":"100","fillTime":"%d"},{"billId":"2","tradeId":"2","ordId":"isolated-order","instId":"ETH-USDT-SWAP","side":"buy","posSide":"long","fillSz":"1","fillPx":"100","fillTime":"%d"}]}`, now.UnixMilli(), now.UnixMilli())
		case strings.Contains(path, "/api/v5/public/instruments"):
			return 200, `{"code":"0","data":[{"instId":"ETH-USDT-SWAP","ctVal":"0.1","lotSz":"1","minSz":"1","tickSz":"0.01"}]}`
		case u.Path == "/api/v5/trade/order":
			queries++
			if unknown {
				return 200, `{"code":"51603","msg":"order does not exist","data":[]}`
			}
			mode := "cross"
			if u.Query().Get("ordId") == "isolated-order" {
				mode = "isolated"
			}
			return 200, fmt.Sprintf(`{"code":"0","data":[{"ordId":%q,"instId":"ETH-USDT-SWAP","state":"filled","tdMode":%q,"accFillSz":"1","avgPx":"100"}]}`, u.Query().Get("ordId"), mode)
		default:
			t.Fatalf("unexpected OKX request: %s", path)
			return 500, ""
		}
	})
	fills, err := ex.GetTradesForPosition("ETHUSDT", "cross", now.Add(-time.Minute))
	if err != nil || len(fills) != 1 || fills[0].OrderID != "cross-order" || fills[0].Quantity != .1 {
		t.Fatalf("wrong scoped history: %+v %v", fills, err)
	}
	if _, err = ex.GetTradesForPosition("ETHUSDT", "cross", now.Add(-time.Minute)); err != nil || queries != 2 {
		t.Fatalf("immutable order scope not cached: %d %v", queries, err)
	}
	ex.fillOrderModes.Delete("ETHUSDT|cross-order")
	unknown = true
	if _, err = ex.GetTradesForPosition("ETHUSDT", "cross", now.Add(-time.Minute)); err == nil {
		t.Fatal("unknown order silently assigned to current margin scope")
	}
}
