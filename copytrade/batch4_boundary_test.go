package copytrade

// 第四批回归测试（copytrade 包部分）：
//   - M3 OKX 领航员快照过滤 size==0 残影仓位
//   - M5 Smart Money 空快照确认后保留基线，后续空快照持续携带确认标记

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOKXAccountStateFiltersZeroSizePositions(t *testing.T) {
	// GetAccountState 先请求资产再请求持仓，按调用顺序区分响应。
	calls := 0
	p := &OKXProvider{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		var body []byte
		if calls == 1 {
			body, _ = json.Marshal(OKXAssetResp{Code: "0", Data: []OKXAsset{{Currency: "USDT", Amount: "1000"}}})
		} else {
			body, _ = json.Marshal(OKXPositionResp{Code: "0", Data: []OKXPositionData{{PosData: []OKXPosition{
				{InstId: "BTC-USDT-SWAP", PosSide: "long", Pos: "2", AvgPx: "100", MgnMode: "cross", PosId: "live-1"},
				{InstId: "ETH-USDT-SWAP", PosSide: "long", Pos: "0", AvgPx: "100", MgnMode: "cross", PosId: "ghost-1"},
				{InstId: "SOL-USDT-SWAP", PosSide: "net", Pos: "0", AvgPx: "100", MgnMode: "cross", PosId: "ghost-2"},
			}}}})
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}}

	state, err := p.GetAccountState("leader")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Positions) != 1 {
		t.Fatalf("zero-size ghost positions must be filtered, got %d: %+v", len(state.Positions), state.Positions)
	}
	pos, ok := state.Positions["live-1"]
	if !ok || pos.Size != 2 || pos.Side != SideLong {
		t.Fatalf("live position lost by the filter: %+v", state.Positions)
	}
}

// M5：提供方确认空快照后不得把基线归零——否则下一轮空快照不再携带
// EmptySnapshotConfirmed，引擎层会再跑一轮完整确认窗口（双层窗口串联）。
func TestSmartMoneyEmptySnapshotStaysConfirmedAcrossPolls(t *testing.T) {
	positionCalls := 0
	p := NewBinanceSmartMoneyProvider("p", "c")
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/smart-money/profile/query-positions"):
			positionCalls++
			if positionCalls == 1 {
				return smartHTTPResponse(200, `{"code":"000000","data":{"total":1,"list":[{"symbol":"BTCUSDT","side":"LONG","amount":"1","entryPrice":"100","markPrice":"100","leverage":"2"}]}}`), nil
			}
			return smartHTTPResponse(200, `{"code":"000000","data":{"total":0,"list":[]}}`), nil
		case strings.Contains(req.URL.Path, "/smart-money/profile"):
			return smartHTTPResponse(200, `{"code":"000000","data":{"enable":true,"sharingPosition":true,"umMarginBalance":"100"}}`), nil
		case req.URL.Path == "/fapi/v1/exchangeInfo":
			return smartHTTPResponse(200, `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","status":"TRADING"}]}`), nil
		default:
			return smartHTTPResponse(404, `{}`), nil
		}
	})

	if _, err := p.GetAccountState("5082050984257986817"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetAccountState("5082050984257986817"); err == nil {
		t.Fatal("first empty snapshot must wait for the confirmation window")
	}
	p.mu.Lock()
	p.emptySnapshotCandidateAt = time.Now().Add(-smartMoneyEmptyConfirmDelay)
	p.mu.Unlock()

	first, err := p.GetAccountState("5082050984257986817")
	if err != nil || !first.EmptySnapshotConfirmed {
		t.Fatalf("confirmed empty snapshot expected: state=%+v err=%v", first, err)
	}
	// 关键断言：下一轮空快照仍必须带确认标记，不得重开第二个窗口
	second, err := p.GetAccountState("5082050984257986817")
	if err != nil || second == nil || !second.EmptySnapshotConfirmed {
		t.Fatalf("subsequent empty snapshots must stay confirmed (no second window): state=%+v err=%v", second, err)
	}
}
