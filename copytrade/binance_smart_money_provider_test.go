package copytrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func smartHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestNormalizeTopTraderID(t *testing.T) {
	for _, input := range []string{"5082050984257986817", "https://www.binance.com/zh-CN/smart-money/profile/5082050984257986817/"} {
		got, err := NormalizeTopTraderID(input)
		if err != nil || got != "5082050984257986817" {
			t.Fatalf("NormalizeTopTraderID(%q)=%q,%v", input, got, err)
		}
	}
	if _, err := NormalizeTopTraderID("portfolio-123"); err == nil {
		t.Fatal("non-numeric topTraderId must be rejected")
	}
	for _, invalid := range []string{"123", "50820509842579868170"} {
		if _, err := NormalizeTopTraderID(invalid); err == nil {
			t.Fatalf("non-19-digit topTraderId %q must be rejected", invalid)
		}
	}
}

func TestSmartMoneyProviderFullPaginationAndExactSettlementAssets(t *testing.T) {
	positions := make([]map[string]interface{}, 0, 10)
	for i := 0; i < 7; i++ {
		symbol := fmt.Sprintf("FILL%dUSDT", i)
		if i == 0 {
			symbol = "XAUUSDT"
		}
		positions = append(positions, map[string]interface{}{"symbol": symbol, "side": "LONG", "amount": "1", "markPrice": "1", "entryPrice": "1", "leverage": "2"})
	}
	positions = append(positions,
		map[string]interface{}{"symbol": "BTCUSDT", "side": "LONG", "amount": "0.1", "markPrice": "60000", "entryPrice": "59000", "leverage": "5"},
		// side is deliberately stale/conflicting: amount sign is authoritative.
		map[string]interface{}{"symbol": "BTCUSDC", "side": "LONG", "amount": "-0.2", "markPrice": "60000", "entryPrice": "61000", "leverage": "10"},
		map[string]interface{}{"symbol": "BTCUSD1", "side": "LONG", "amount": "0.3", "markPrice": "60000", "entryPrice": "58000", "leverage": "3"},
	)
	page1, _ := json.Marshal(positions[:9])
	page2, _ := json.Marshal(positions[9:])
	positionPages := 0
	p := NewBinanceSmartMoneyProvider("p20t-value", "csrf-value")
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/smart-money/profile/query-positions"):
			positionPages++
			if !strings.Contains(req.Header.Get("Cookie"), "p20t=p20t-value") || req.Header.Get("csrftoken") != "csrf-value" {
				t.Fatalf("authenticated headers missing: %+v", req.Header)
			}
			if req.URL.Query().Get("marketType") != "UM" || req.URL.Query().Get("rows") != "9" {
				t.Fatalf("unexpected pagination query: %s", req.URL.RawQuery)
			}
			body := page1
			if req.URL.Query().Get("page") == "2" {
				body = page2
			}
			return smartHTTPResponse(200, fmt.Sprintf(`{"code":"000000","success":true,"data":{"total":10,"list":%s}}`, body)), nil
		case strings.Contains(req.URL.Path, "/smart-money/profile"):
			return smartHTTPResponse(200, `{"code":"000000","success":true,"data":{"topTraderId":"5082050984257986817","traderName":"public-leader","enable":true,"sharingPosition":true,"umMarginBalance":"100000"}}`), nil
		case req.URL.Path == "/fapi/v1/exchangeInfo":
			var symbols []string
			for _, pos := range positions {
				symbol := pos["symbol"].(string)
				quote := "USDT"
				if strings.HasSuffix(symbol, "USDC") {
					quote = "USDC"
				}
				if strings.HasSuffix(symbol, "USD1") {
					quote = "USD1"
				}
				contractType := "PERPETUAL"
				if symbol == "XAUUSDT" {
					contractType = "TRADIFI_PERPETUAL"
				}
				symbols = append(symbols, fmt.Sprintf(`{"symbol":%q,"baseAsset":%q,"quoteAsset":%q,"marginAsset":%q,"contractType":%q,"status":"TRADING"}`, symbol, strings.TrimSuffix(symbol, quote), quote, quote, contractType))
			}
			return smartHTTPResponse(200, `{"symbols":[`+strings.Join(symbols, ",")+`]}`), nil
		case req.URL.Path == "/api/v3/ticker/price":
			price := "0.999"
			if req.URL.Query().Get("symbol") == "USD1USDT" {
				price = "1.001"
			}
			return smartHTTPResponse(200, `{"price":"`+price+`"}`), nil
		default:
			return smartHTTPResponse(404, `{}`), nil
		}
	})

	state, err := p.GetAccountState("5082050984257986817")
	if err != nil {
		t.Fatal(err)
	}
	if positionPages != 2 || len(state.Positions) != 10 {
		t.Fatalf("pages=%d positions=%d", positionPages, len(state.Positions))
	}
	checks := map[string]struct {
		quote string
		usd   float64
		side  SideType
	}{
		"BTCUSDT": {"USDT", 6000, SideLong}, "BTCUSDC": {"USDC", 0.2 * 60000 * 0.999, SideShort},
		"BTCUSD1": {"USD1", 0.3 * 60000 * 1.001, SideLong}, "XAUUSDT": {"USDT", 1, SideLong},
	}
	for _, pos := range state.Positions {
		want, ok := checks[pos.Symbol]
		if !ok {
			continue
		}
		if pos.Instrument == nil || pos.Instrument.QuoteAsset != want.quote || pos.ValueCurrency != want.quote || !pos.ValueUSDValid {
			t.Fatalf("exact identity lost for %s: %+v", pos.Symbol, pos)
		}
		if pos.Symbol == "XAUUSDT" && pos.Instrument.ContractType != "TRADIFI_PERPETUAL" {
			t.Fatalf("TradFi contract type was not preserved: %+v", pos)
		}
		if pos.Side != want.side {
			t.Fatalf("%s side=%s want=%s; amount sign must be authoritative", pos.Symbol, pos.Side, want.side)
		}
		if diff := pos.PositionValue - want.usd; diff > 1e-8 || diff < -1e-8 {
			t.Fatalf("%s USD value=%f want=%f", pos.Symbol, pos.PositionValue, want.usd)
		}
	}
	if obs := p.LastSourceHealthObservation(); obs.Status != "HEALTHY" || !obs.CompleteSnapshot {
		t.Fatalf("unexpected health observation: %+v", obs)
	}
}

func TestSmartMoneyProviderSupportsDualSideAndRejectsDuplicatePagination(t *testing.T) {
	t.Run("same contract long and short remain distinct", func(t *testing.T) {
		p := NewBinanceSmartMoneyProvider("p", "c")
		p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.Contains(req.URL.Path, "query-positions"):
				return smartHTTPResponse(200, `{"code":"000000","data":{"total":2,"list":[
					{"symbol":"BTCUSDT","amount":"1","markPrice":"100"},
					{"symbol":"BTCUSDT","amount":"-2","markPrice":"100"}]}}`), nil
			case strings.Contains(req.URL.Path, "/smart-money/profile"):
				return smartHTTPResponse(200, `{"code":"000000","data":{"enable":true,"sharingPosition":true,"umMarginBalance":"1000"}}`), nil
			case req.URL.Path == "/fapi/v1/exchangeInfo":
				return smartHTTPResponse(200, `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","status":"TRADING"}]}`), nil
			default:
				return smartHTTPResponse(404, `{}`), nil
			}
		})
		state, err := p.GetAccountState("5082050984257986817")
		if err != nil || len(state.Positions) != 2 {
			t.Fatalf("dual-side snapshot failed: state=%+v err=%v", state, err)
		}
		if state.Positions["smart_5082050984257986817_BTCUSDT_long"] == nil ||
			state.Positions["smart_5082050984257986817_BTCUSDT_short"] == nil {
			t.Fatalf("long/short lifecycle identities collapsed: %+v", state.Positions)
		}
	})

	t.Run("duplicate contract across pages rejects whole snapshot", func(t *testing.T) {
		rows := make([]string, 0, 9)
		for i := 0; i < 9; i++ {
			rows = append(rows, fmt.Sprintf(`{"symbol":"DUP%dUSDT","amount":"1"}`, i))
		}
		p := NewBinanceSmartMoneyProvider("p", "c")
		p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			pageRows := strings.Join(rows, ",")
			if req.URL.Query().Get("page") == "2" {
				pageRows = rows[0]
			}
			return smartHTTPResponse(200, `{"code":"000000","data":{"total":10,"list":[`+pageRows+`]}}`), nil
		})
		if positions, err := p.fetchAllPositions("5082050984257986817"); err == nil || positions != nil {
			t.Fatalf("duplicate pagination identity must fail closed: positions=%+v err=%v", positions, err)
		}
	})
}

func TestSmartMoneyProviderPrivateAndSecondPageFailureFailClosed(t *testing.T) {
	t.Run("private is explicit", func(t *testing.T) {
		p := NewBinanceSmartMoneyProvider("p", "c")
		p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return smartHTTPResponse(200, `{"code":"000000","success":true,"data":{"enable":true,"sharingPosition":false,"umMarginBalance":"1"}}`), nil
		})
		_, err := p.GetAccountState("5082050984257986817")
		if !errors.Is(err, ErrBinanceSmartMoneyPrivate) || p.LastSourceHealthObservation().Status != "PRIVATE" {
			t.Fatalf("err=%v obs=%+v", err, p.LastSourceHealthObservation())
		}
	})

	t.Run("disabled is explicit", func(t *testing.T) {
		p := NewBinanceSmartMoneyProvider("p", "c")
		p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return smartHTTPResponse(200, `{"code":"000000","success":true,"data":{"enable":false,"sharingPosition":true}}`), nil
		})
		_, err := p.GetAccountState("5082050984257986817")
		if !errors.Is(err, ErrBinanceSmartMoneyDisabled) || p.LastSourceHealthObservation().Status != "DISABLED" {
			t.Fatalf("err=%v obs=%+v", err, p.LastSourceHealthObservation())
		}
	})

	t.Run("auth and transient HTTP errors remain distinct", func(t *testing.T) {
		auth := NewBinanceSmartMoneyProvider("p", "c")
		auth.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return smartHTTPResponse(200, `{"code":"100001005","message":"Please log in first"}`), nil
		})
		if _, err := auth.GetAccountState("5082050984257986817"); !errors.Is(err, ErrBinanceCredentialsExpired) ||
			auth.LastSourceHealthObservation().Status != "AUTH_FAILED" {
			t.Fatalf("auth classification failed: err=%v obs=%+v", err, auth.LastSourceHealthObservation())
		}

		transient := NewBinanceSmartMoneyProvider("p", "c")
		transient.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return smartHTTPResponse(429, `rate limited`), nil
		})
		if _, err := transient.GetAccountState("5082050984257986817"); err == nil ||
			errors.Is(err, ErrBinanceCredentialsExpired) || transient.LastSourceHealthObservation().Status == "AUTH_FAILED" {
			t.Fatalf("transient HTTP error misclassified as auth: err=%v obs=%+v", err, transient.LastSourceHealthObservation())
		}
	})

	t.Run("second page failure invalidates whole snapshot", func(t *testing.T) {
		p := NewBinanceSmartMoneyProvider("p", "c")
		p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "query-positions") {
				if req.URL.Query().Get("page") == "2" {
					return smartHTTPResponse(502, `bad gateway`), nil
				}
				rows := make([]string, 0, 9)
				for i := 0; i < 9; i++ {
					rows = append(rows, fmt.Sprintf(`{"symbol":"PAGE%dUSDT","side":"LONG","amount":"1","markPrice":"1"}`, i))
				}
				return smartHTTPResponse(200, `{"code":"000000","data":{"total":10,"list":[`+strings.Join(rows, ",")+`]}}`), nil
			}
			return smartHTTPResponse(200, `{"code":"000000","data":{"enable":true,"sharingPosition":true,"umMarginBalance":"100"}}`), nil
		})
		state, err := p.GetAccountState("5082050984257986817")
		if err == nil || state != nil || p.LastSourceHealthObservation().CompleteSnapshot {
			t.Fatalf("partial page must fail closed: state=%+v err=%v obs=%+v", state, err, p.LastSourceHealthObservation())
		}
	})
}

func TestSmartMoneyProviderKeepsCompletePositionsWhenMarginBalanceUnavailable(t *testing.T) {
	p := NewBinanceSmartMoneyProvider("p", "c")
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/smart-money/profile/query-positions"):
			return smartHTTPResponse(200, `{"code":"000000","data":{"total":1,"list":[{"symbol":"BTCUSDT","side":"LONG","amount":"1","entryPrice":"100","markPrice":"100","leverage":"2"}]}}`), nil
		case strings.Contains(req.URL.Path, "/smart-money/profile"):
			return smartHTTPResponse(200, `{"code":"000000","data":{"enable":true,"sharingPosition":true,"umMarginBalance":"0"}}`), nil
		case req.URL.Path == "/fapi/v1/exchangeInfo":
			return smartHTTPResponse(200, `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","status":"TRADING"}]}`), nil
		default:
			return smartHTTPResponse(404, `{}`), nil
		}
	})

	state, err := p.GetAccountState("5082050984257986817")
	if err != nil || state == nil || len(state.Positions) != 1 {
		t.Fatalf("invalid equity must not hide a complete position snapshot: state=%+v err=%v", state, err)
	}
	if state.TotalEquity != 0 {
		t.Fatalf("invalid equity must remain fail-closed for risk increases, got %.2f", state.TotalEquity)
	}
	if obs := p.LastSourceHealthObservation(); obs.Status != "HEALTHY" || !obs.CompleteSnapshot {
		t.Fatalf("position visibility should remain healthy: %+v", obs)
	}
}

func TestSmartMoneyProviderUsesStaleVisibilityButNotStaleEquity(t *testing.T) {
	profileCalls := 0
	p := NewBinanceSmartMoneyProvider("p", "c")
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/smart-money/profile/query-positions"):
			return smartHTTPResponse(200, `{"code":"000000","data":{"total":1,"list":[{"symbol":"BTCUSDT","amount":"1","entryPrice":"100","markPrice":"100","leverage":"2"}]}}`), nil
		case strings.Contains(req.URL.Path, "/smart-money/profile"):
			profileCalls++
			if profileCalls > 1 {
				return smartHTTPResponse(503, `temporary unavailable`), nil
			}
			return smartHTTPResponse(200, `{"code":"000000","data":{"enable":true,"sharingPosition":true,"umMarginBalance":"1000"}}`), nil
		case req.URL.Path == "/fapi/v1/exchangeInfo":
			return smartHTTPResponse(200, `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","status":"TRADING"}]}`), nil
		default:
			return smartHTTPResponse(404, `{}`), nil
		}
	})

	first, err := p.GetAccountState("5082050984257986817")
	if err != nil || first.TotalEquity != 1000 {
		t.Fatalf("warm profile failed: state=%+v err=%v", first, err)
	}
	p.mu.Lock()
	p.profileFetchedAt = time.Now().Add(-smartMoneyProfileTTL - time.Second)
	p.mu.Unlock()

	second, err := p.GetAccountState("5082050984257986817")
	if err != nil || second == nil || len(second.Positions) != 1 {
		t.Fatalf("transient profile failure must retain a complete risk-reducing snapshot: state=%+v err=%v", second, err)
	}
	if second.TotalEquity != 0 {
		t.Fatalf("stale equity must block risk increases, got %.2f", second.TotalEquity)
	}
}

func TestSmartMoneyProviderRejectsPositionWithoutAmountDirection(t *testing.T) {
	p := NewBinanceSmartMoneyProvider("p", "c")
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "query-positions") {
			return smartHTTPResponse(200, `{"code":"000000","data":{"total":1,"list":[{"symbol":"BTCUSDT","side":"LONG","amount":"0","markPrice":"100"}]}}`), nil
		}
		return smartHTTPResponse(200, `{"code":"000000","data":{"enable":true,"sharingPosition":true,"umMarginBalance":"100"}}`), nil
	})

	state, err := p.GetAccountState("5082050984257986817")
	if err == nil || state != nil || !strings.Contains(err.Error(), "invalid amount") {
		t.Fatalf("directionless position must reject the whole snapshot: state=%+v err=%v", state, err)
	}
	if p.LastSourceHealthObservation().CompleteSnapshot {
		t.Fatal("invalid position snapshot must never be marked complete")
	}
}

func TestSmartMoneyProviderSuddenEmptyBypassesCachedProfileAndCannotMasqueradeAsClose(t *testing.T) {
	profileCalls := 0
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
			profileCalls++
			sharing := "true"
			if profileCalls > 1 {
				sharing = "false"
			}
			return smartHTTPResponse(200, `{"code":"000000","data":{"traderName":"leader","enable":true,"sharingPosition":`+sharing+`,"umMarginBalance":"1000"}}`), nil
		case req.URL.Path == "/fapi/v1/exchangeInfo":
			return smartHTTPResponse(200, `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","status":"TRADING"}]}`), nil
		default:
			return smartHTTPResponse(404, `{}`), nil
		}
	})

	state, err := p.GetAccountState("5082050984257986817")
	if err != nil || len(state.Positions) != 1 {
		t.Fatalf("initial complete snapshot: state=%+v err=%v", state, err)
	}
	state, err = p.GetAccountState("5082050984257986817")
	if !errors.Is(err, ErrBinanceSmartMoneyPrivate) || state != nil {
		t.Fatalf("sudden empty must fail as PRIVATE, state=%+v err=%v", state, err)
	}
	if profileCalls != 2 {
		t.Fatalf("sudden empty must bypass cached profile, profile calls=%d", profileCalls)
	}
	if obs := p.LastSourceHealthObservation(); obs.Status != "PRIVATE" || obs.CompleteSnapshot {
		t.Fatalf("unexpected health after hidden empty snapshot: %+v", obs)
	}
}

func TestSmartMoneyProviderRefreshesPrivacyBeforeAcceptingNonEmptySnapshot(t *testing.T) {
	profileCalls := 0
	positionCalls := 0
	p := NewBinanceSmartMoneyProvider("p", "c")
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/smart-money/profile/query-positions"):
			positionCalls++
			return smartHTTPResponse(200, `{"code":"000000","data":{"total":1,"list":[{"symbol":"BTCUSDT","amount":"1","markPrice":"100"}]}}`), nil
		case strings.Contains(req.URL.Path, "/smart-money/profile"):
			profileCalls++
			sharing := profileCalls == 1
			return smartHTTPResponse(200, fmt.Sprintf(`{"code":"000000","data":{"enable":true,"sharingPosition":%t,"umMarginBalance":"100"}}`, sharing)), nil
		case req.URL.Path == "/fapi/v1/exchangeInfo":
			return smartHTTPResponse(200, `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","status":"TRADING"}]}`), nil
		default:
			return smartHTTPResponse(404, `{}`), nil
		}
	})

	if state, err := p.GetAccountState("5082050984257986817"); err != nil || len(state.Positions) != 1 {
		t.Fatalf("initial public snapshot failed: state=%+v err=%v", state, err)
	}
	state, err := p.GetAccountState("5082050984257986817")
	if !errors.Is(err, ErrBinanceSmartMoneyPrivate) || state != nil {
		t.Fatalf("non-empty cached positions must not bypass a fresh privacy check: state=%+v err=%v", state, err)
	}
	if profileCalls != 2 || positionCalls != 1 {
		t.Fatalf("privacy must be refreshed before the second positions request: profiles=%d positions=%d", profileCalls, positionCalls)
	}
}

func TestSmartMoneyProviderMarksGenuineEmptySnapshotConfirmedOnce(t *testing.T) {
	positionCalls := 0
	p := NewBinanceSmartMoneyProvider("p", "c")
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/smart-money/profile/query-positions"):
			positionCalls++
			if positionCalls == 1 {
				return smartHTTPResponse(200, `{"code":"000000","data":{"total":1,"list":[{"symbol":"BTCUSDT","amount":"1","markPrice":"100"}]}}`), nil
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
	if state, err := p.GetAccountState("5082050984257986817"); !errors.Is(err, ErrBinanceSmartMoneyEmptyUnconfirmed) || state != nil {
		t.Fatalf("first genuine empty snapshot must wait for confirmation: state=%+v err=%v", state, err)
	}
	p.mu.Lock()
	p.emptySnapshotCandidateAt = time.Now().Add(-smartMoneyEmptyConfirmDelay)
	p.mu.Unlock()
	state, err := p.GetAccountState("5082050984257986817")
	if err != nil || state == nil || len(state.Positions) != 0 || !state.EmptySnapshotConfirmed {
		t.Fatalf("provider-confirmed empty snapshot missing confirmation marker: state=%+v err=%v", state, err)
	}
}

func TestSmartMoneyDiagnosticsPaginateUMAndRemainReadOnly(t *testing.T) {
	start := time.UnixMilli(1781625600000)
	end := time.UnixMilli(1784303999999)
	historyRows := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		historyRows = append(historyRows, fmt.Sprintf(`{"id":%d,"symbol":"HIST%dUSDT","side":"Long","positionSide":"LONG","avgCost":"10.5","avgClosePrice":11.5,"closingPnl":"2.25","opened":"1781625600000","closed":1781625700000}`, i+1, i))
	}
	orderRows := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		orderRows = append(orderRows, fmt.Sprintf(`{"id":"latest-%d","orderId":%d,"symbol":"ORDER%dUSDC","side":"BUY","positionSide":"LONG","action":"OPEN","type":"MARKET","price":"20.5","quantity":"3","executedQty":3,"time":"1781625600000"}`, i, 100+i, i))
	}
	historyPages := 0
	orderPages := 0
	p := NewBinanceSmartMoneyProvider("p20t-diag", "csrf-diag")
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/query-um-position-history"):
			historyPages++
			if !strings.Contains(req.Header.Get("Cookie"), "p20t=p20t-diag") || req.Header.Get("csrftoken") != "csrf-diag" {
				t.Fatalf("history did not reuse web auth: %+v", req.Header)
			}
			if req.URL.Query().Get("startTime") != "1781625600000" || req.URL.Query().Get("endTime") != "1784303999999" || req.URL.Query().Get("rows") != "9" {
				t.Fatalf("unexpected history query: %s", req.URL.RawQuery)
			}
			if req.URL.Query().Get("marketType") != "" {
				t.Fatalf("UM-specific history endpoint must not invent a marketType: %s", req.URL.RawQuery)
			}
			rows := historyRows[:9]
			if req.URL.Query().Get("page") == "2" {
				rows = historyRows[9:]
			}
			return smartHTTPResponse(200, `{"code":"000000","data":{"total":10,"list":[`+strings.Join(rows, ",")+`]}}`), nil
		case strings.Contains(req.URL.Path, "/query-order-history"):
			orderPages++
			if !strings.Contains(req.Header.Get("Cookie"), "p20t=p20t-diag") || req.Header.Get("csrftoken") != "csrf-diag" {
				t.Fatalf("latest operations did not reuse web auth: %+v", req.Header)
			}
			if req.URL.Query().Get("marketType") != "UM" || req.URL.Query().Get("rows") != "10" {
				t.Fatalf("latest operations must be UM: %s", req.URL.RawQuery)
			}
			rows := orderRows[:10]
			if req.URL.Query().Get("page") == "2" {
				rows = orderRows[10:]
			}
			return smartHTTPResponse(200, `{"code":"000000","data":{"totalCount":"11","records":[`+strings.Join(rows, ",")+`]}}`), nil
		case strings.Contains(req.URL.Path, "/smart-money/profile"):
			return smartHTTPResponse(200, `{"code":"000000","data":{"enable":true,"sharingPosition":true,"sharingPositionHistory":true,"sharingLatestRecord":true,"umMarginBalance":"100"}}`), nil
		default:
			return smartHTTPResponse(404, `{}`), nil
		}
	})

	history, err := p.GetSmartMoneyPositionHistory("5082050984257986817", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if historyPages != 2 || len(history) != 10 {
		t.Fatalf("history pages=%d records=%d", historyPages, len(history))
	}
	if history[0].ID != "1" || history[0].Symbol != "HIST0USDT" || history[0].AvgCost != 10.5 || history[0].PnL != 2.25 || history[0].OpenedAt != 1781625600000 || len(history[0].Raw) == 0 {
		t.Fatalf("history normalization failed: %+v", history[0])
	}

	latest, err := p.GetSmartMoneyLatestOperations("5082050984257986817", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if orderPages != 2 || len(latest) != 11 {
		t.Fatalf("latest pages=%d records=%d", orderPages, len(latest))
	}
	if latest[0].OrderID != "100" || latest[0].Symbol != "ORDER0USDC" || latest[0].Operation != "OPEN" || latest[0].Quantity != 3 || latest[0].CreatedAt != 1781625600000 || len(latest[0].Raw) == 0 {
		t.Fatalf("latest operation normalization failed: %+v", latest[0])
	}

	// The delayed feed remains impossible to consume through Provider.GetFills.
	fills, err := p.GetFills("5082050984257986817", start)
	if err != nil || fills != nil {
		t.Fatalf("diagnostic latest operations leaked into trading fills: fills=%+v err=%v", fills, err)
	}
}

func TestSmartMoneyDiagnosticsFailClosedOnPrivacyAndPartialPagination(t *testing.T) {
	start := time.UnixMilli(1781625600000)
	end := time.UnixMilli(1784303999999)

	t.Run("profile sharing flags gate each diagnostic feed", func(t *testing.T) {
		requests := 0
		p := NewBinanceSmartMoneyProvider("p", "c")
		p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			if !strings.HasSuffix(req.URL.Path, "/smart-money/profile") {
				t.Fatalf("private diagnostic feed must not be queried: %s", req.URL.Path)
			}
			return smartHTTPResponse(200, `{"code":"000000","data":{"enable":true,"sharingPositionHistory":false,"sharingLatestRecord":false}}`), nil
		})
		if records, err := p.GetSmartMoneyPositionHistory("5082050984257986817", start, end); !errors.Is(err, ErrBinanceSmartMoneyHistoryPrivate) || records != nil {
			t.Fatalf("private history must fail closed: records=%+v err=%v", records, err)
		}
		if records, err := p.GetSmartMoneyLatestOperations("5082050984257986817", start, end); !errors.Is(err, ErrBinanceSmartMoneyLatestPrivate) || records != nil {
			t.Fatalf("private latest operations must fail closed: records=%+v err=%v", records, err)
		}
		if requests != 2 {
			t.Fatalf("each private diagnostic feed must refresh its sharing flag, requests=%d", requests)
		}
	})

	t.Run("second page failure invalidates all diagnostic rows", func(t *testing.T) {
		p := NewBinanceSmartMoneyProvider("p", "c")
		p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.Contains(req.URL.Path, "/query-um-position-history"):
				if req.URL.Query().Get("page") == "2" {
					return smartHTTPResponse(502, `bad gateway`), nil
				}
				rows := make([]string, 0, 9)
				for i := 0; i < 9; i++ {
					rows = append(rows, fmt.Sprintf(`{"id":%d,"symbol":"ASSET%dUSDT"}`, i, i))
				}
				return smartHTTPResponse(200, `{"code":"000000","data":{"total":10,"list":[`+strings.Join(rows, ",")+`]}}`), nil
			case strings.Contains(req.URL.Path, "/smart-money/profile"):
				return smartHTTPResponse(200, `{"code":"000000","data":{"enable":true,"sharingPositionHistory":true}}`), nil
			default:
				return smartHTTPResponse(404, `{}`), nil
			}
		})
		records, err := p.GetSmartMoneyPositionHistory("5082050984257986817", start, end)
		if err == nil || records != nil {
			t.Fatalf("partial diagnostic history must fail closed: records=%+v err=%v", records, err)
		}
	})

	if _, err := NewBinanceSmartMoneyProvider("p", "c").GetSmartMoneyPositionHistory("5082050984257986817", end, start); err == nil {
		t.Fatal("invalid diagnostic range must fail before making a request")
	}
}
