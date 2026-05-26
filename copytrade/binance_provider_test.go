package copytrade

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newBinanceProviderWithTransport(fn roundTripFunc) *BinanceProvider {
	p := NewBinanceProvider("p20t", "csrf")
	p.client.Transport = fn
	return p
}

func TestBinanceValidateCredentials(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		reqErr      error
		wantExpired bool
		wantErr     bool
	}{
		{
			name:   "valid account info",
			status: http.StatusOK,
			body:   `{"code":"000000","success":true,"data":{"userId":"1133692136"}}`,
		},
		{
			name:        "auth code expired",
			status:      http.StatusOK,
			body:        `{"code":"100001005","message":"not login","data":{}}`,
			wantExpired: true,
			wantErr:     true,
		},
		{
			name:        "missing user id is invalid credentials",
			status:      http.StatusOK,
			body:        `{"code":"000000","success":true,"data":{}}`,
			wantExpired: true,
			wantErr:     true,
		},
		{
			name:        "forbidden is invalid credentials",
			status:      http.StatusForbidden,
			body:        `{"code":"403"}`,
			wantExpired: true,
			wantErr:     true,
		},
		{
			name:    "network error is not credential expiry",
			reqErr:  errors.New("temporary network failure"),
			wantErr: true,
		},
		{
			name:    "server error is not credential expiry",
			status:  http.StatusBadGateway,
			body:    `bad gateway`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
				if tt.reqErr != nil {
					return nil, tt.reqErr
				}
				return &http.Response{
					StatusCode: tt.status,
					Body:       io.NopCloser(strings.NewReader(tt.body)),
					Header:     make(http.Header),
				}, nil
			})

			err := p.ValidateCredentials()
			if tt.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := errors.Is(err, ErrBinanceCredentialsExpired); got != tt.wantExpired {
				t.Fatalf("expired mismatch: got=%v want=%v err=%v", got, tt.wantExpired, err)
			}
		})
	}
}

func TestBinanceEmptyCredentialsAreExpired(t *testing.T) {
	p := NewBinanceProvider("", "")
	if err := p.ValidateCredentials(); !errors.Is(err, ErrBinanceCredentialsExpired) {
		t.Fatalf("expected ErrBinanceCredentialsExpired, got %v", err)
	}
	if _, err := p.postCopyTrade(BinanceCopyTradePositionAPI, map[string]interface{}{}); !errors.Is(err, ErrBinanceCredentialsExpired) {
		t.Fatalf("expected postCopyTrade ErrBinanceCredentialsExpired, got %v", err)
	}
}

// fakeCredsLoader 测试用 credLoader（独立于 store 包，保持 copytrade 包测试自洽）
type fakeCredsLoader struct {
	p20t string
	csrf string
	err  error
}

func (f *fakeCredsLoader) LoadBinanceCredentials(label string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.p20t, f.csrf, nil
}

// TestBinanceCredentialsLoaderPriority 验证 credentials() 的优先级：
//  1. credLoader 返回非空对 → 使用全局
//  2. credLoader 返回空 → 降级到本地 fallbackP20T/fallbackCSRF
//  3. 都为空 → ErrBinanceCredentialsExpired
func TestBinanceCredentialsLoaderPriority(t *testing.T) {
	t.Run("loader provides creds, fallback ignored", func(t *testing.T) {
		loader := &fakeCredsLoader{p20t: "global-p20t", csrf: "global-csrf"}
		p := NewBinanceProviderWithLoader(loader, "default", "fallback-p20t", "fallback-csrf")
		gotP20T, gotCSRF, err := p.credentials()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if gotP20T != "global-p20t" || gotCSRF != "global-csrf" {
			t.Fatalf("expected global creds, got %q/%q", gotP20T, gotCSRF)
		}
	})

	t.Run("loader empty falls back to local", func(t *testing.T) {
		loader := &fakeCredsLoader{} // 空值（未配置）
		p := NewBinanceProviderWithLoader(loader, "default", "fallback-p20t", "fallback-csrf")
		gotP20T, gotCSRF, err := p.credentials()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if gotP20T != "fallback-p20t" || gotCSRF != "fallback-csrf" {
			t.Fatalf("expected fallback creds, got %q/%q", gotP20T, gotCSRF)
		}
	})

	t.Run("loader empty + no fallback returns expired", func(t *testing.T) {
		p := NewBinanceProviderWithLoader(&fakeCredsLoader{}, "default", "", "")
		_, _, err := p.credentials()
		if !errors.Is(err, ErrBinanceCredentialsExpired) {
			t.Fatalf("expected ErrBinanceCredentialsExpired, got %v", err)
		}
	})

	t.Run("loader returns error propagates", func(t *testing.T) {
		boom := errors.New("db boom")
		p := NewBinanceProviderWithLoader(&fakeCredsLoader{err: boom}, "default", "fallback", "fallback")
		_, _, err := p.credentials()
		if err == nil || !strings.Contains(err.Error(), "db boom") {
			t.Fatalf("expected propagated error, got %v", err)
		}
	})

	t.Run("legacy NewBinanceProvider unchanged", func(t *testing.T) {
		p := NewBinanceProvider("legacy-p20t", "legacy-csrf")
		gotP20T, gotCSRF, err := p.credentials()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if gotP20T != "legacy-p20t" || gotCSRF != "legacy-csrf" {
			t.Fatalf("expected legacy creds, got %q/%q", gotP20T, gotCSRF)
		}
	})
}

// TestBinanceProviderHotReloadViaLoader 验证 loader 内容变化后，
// Provider 下次 HTTP 调用立即使用新值（通过 mock loader + 抓 cookie 验证）。
func TestBinanceProviderHotReloadViaLoader(t *testing.T) {
	loader := &fakeCredsLoader{p20t: "p20t-v1", csrf: "csrf-v1"}

	var capturedP20T, capturedCSRF []string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		ck, _ := req.Cookie("p20t")
		if ck != nil {
			capturedP20T = append(capturedP20T, ck.Value)
		}
		capturedCSRF = append(capturedCSRF, req.Header.Get("csrftoken"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"code":"000000","data":{"userId":"u1"}}`)),
			Header:     make(http.Header),
		}, nil
	})

	p := NewBinanceProviderWithLoader(loader, "default", "", "")
	p.client.Transport = transport

	// v1 凭证
	if err := p.ValidateCredentials(); err != nil {
		t.Fatalf("v1 validate: %v", err)
	}

	// 模拟用户在前端更新凭证（loader 内容变化）
	loader.p20t = "p20t-v2"
	loader.csrf = "csrf-v2"

	// 下一次调用应使用 v2（无需重启 Provider）
	if err := p.ValidateCredentials(); err != nil {
		t.Fatalf("v2 validate: %v", err)
	}

	if len(capturedP20T) != 2 || capturedP20T[0] != "p20t-v1" || capturedP20T[1] != "p20t-v2" {
		t.Fatalf("hot reload p20t mismatch: %+v", capturedP20T)
	}
	if len(capturedCSRF) != 2 || capturedCSRF[0] != "csrf-v1" || capturedCSRF[1] != "csrf-v2" {
		t.Fatalf("hot reload csrf mismatch: %+v", capturedCSRF)
	}
}

// TestBinanceGetCopyPortfolioDetail 验证 detail-list 接口解析、缓存命中与未命中、
// 凭证过期、leadPortfolioId 不在跟单列表四种关键路径。
func TestBinanceGetCopyPortfolioDetail(t *testing.T) {
	const targetID = "4959394188752686849"
	const otherID = "5025603063662907905"
	body := `{"code":"000000","data":[
		{"leadPortfolioId":"` + otherID + `","copyPortfolioId":"copy-other","nickname":"OTHER",
		 "netCopyAmount":20.0,"marginBalance":20.0,"unrealizedPnl":0,"realizedPnl":0,
		 "copyMode":"FIXED_RATIO","leadStatus":"ACTIVE","isPaused":false},
		{"leadPortfolioId":"` + targetID + `","copyPortfolioId":"copy-target","nickname":"Btc熊猫",
		 "netCopyAmount":200.0,"marginBalance":199.99632798,"unrealizedPnl":0.32603648,
		 "realizedPnl":-0.3297085,"copyMode":"FIXED_RATIO","leadStatus":"ACTIVE","isPaused":false}
	]}`

	t.Run("hit and parse correctly", func(t *testing.T) {
		var calls int
		p := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if !strings.Contains(req.URL.Path, "/copy-portfolio/detail-list") {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			if req.URL.Query().Get("ongoing") != "true" {
				t.Fatalf("ongoing=true missing")
			}
			cookie, _ := req.Cookie("p20t")
			if cookie == nil || cookie.Value == "" {
				t.Fatalf("p20t cookie missing")
			}
			if req.Header.Get("csrftoken") == "" {
				t.Fatalf("csrftoken header missing")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})

		d, err := p.GetCopyPortfolioDetail(targetID)
		if err != nil {
			t.Fatalf("first call err: %v", err)
		}
		if d.MarginBalance < 199.99 || d.MarginBalance > 200.0 {
			t.Fatalf("marginBalance=%v want ~199.996", d.MarginBalance)
		}
		if d.CopyMode != "FIXED_RATIO" || d.NetCopyAmount != 200.0 {
			t.Fatalf("unexpected detail: %+v", d)
		}

		// 第二次调用（不同 leadPortfolioId）应命中已刷新的 cache，不再发请求
		d2, err := p.GetCopyPortfolioDetail(otherID)
		if err != nil {
			t.Fatalf("second call err: %v", err)
		}
		if d2.MarginBalance != 20.0 {
			t.Fatalf("other marginBalance=%v want 20", d2.MarginBalance)
		}
		if calls != 1 {
			t.Fatalf("expected 1 HTTP call (cache hit), got %d", calls)
		}
	})

	t.Run("leadPortfolioId not in list", func(t *testing.T) {
		p := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		_, err := p.GetCopyPortfolioDetail("never-followed-id")
		if !errors.Is(err, ErrBinanceNotCopying) {
			t.Fatalf("want ErrBinanceNotCopying, got %v", err)
		}
	})

	t.Run("auth code expired", func(t *testing.T) {
		p := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"code":"100001005","message":"not login"}`)),
				Header:     make(http.Header),
			}, nil
		})
		_, err := p.GetCopyPortfolioDetail(targetID)
		if !errors.Is(err, ErrBinanceCredentialsExpired) {
			t.Fatalf("want ErrBinanceCredentialsExpired, got %v", err)
		}
	})

	t.Run("empty credentials short-circuits to expired", func(t *testing.T) {
		p := NewBinanceProvider("", "")
		_, err := p.GetCopyPortfolioDetail(targetID)
		if !errors.Is(err, ErrBinanceCredentialsExpired) {
			t.Fatalf("want ErrBinanceCredentialsExpired, got %v", err)
		}
	})

	// stale fallback：曾成功过的缓存在网络失败时仍可用，避免上层错配 fallback。
	// 这是修复"anchor 失败时 fallback 量纲偏小"问题的核心保障。
	t.Run("stale cache survives transient failure", func(t *testing.T) {
		var calls int
		var failNext bool
		p := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if failNext {
				return nil, errors.New("simulated network error")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})

		d, err := p.GetCopyPortfolioDetail(targetID)
		if err != nil {
			t.Fatalf("warm-up call err: %v", err)
		}
		expectedMB := d.MarginBalance

		// 强制使缓存过期（让下次调用走 refresh 路径）
		p.mu.Lock()
		p.copyDetailsAt = p.copyDetailsAt.Add(-2 * binanceCopyDetailTTL)
		p.mu.Unlock()

		failNext = true
		d2, err := p.GetCopyPortfolioDetail(targetID)
		if err != nil {
			t.Fatalf("expected stale fallback to succeed, got err: %v", err)
		}
		if d2 == nil || d2.MarginBalance != expectedMB {
			t.Fatalf("stale fallback returned wrong value: got=%+v want marginBalance=%v", d2, expectedMB)
		}
		if calls != 2 {
			t.Fatalf("expected 2 HTTP calls (warm + failed refresh), got %d", calls)
		}
	})

	// 完全无缓存 + refresh 失败 → 应该返回 error 而不是悄悄成功
	t.Run("no cache + refresh fails returns error", func(t *testing.T) {
		p := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("simulated network error")
		})
		_, err := p.GetCopyPortfolioDetail(targetID)
		if err == nil {
			t.Fatalf("expected error when no cache and refresh fails")
		}
	})
}

func TestBinanceGetPositionHistory(t *testing.T) {
	p := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		switch {
		case strings.Contains(req.URL.Path, "/lead-portfolio/detail"):
			body = `{"code":"000000","data":{"copyPortfolioId":"copy-123","hasCopy":true,"marginBalance":"100.5"}}`
		case strings.Contains(req.URL.Path, "/position-history"):
			body = `{"code":"000000","data":{"total":1,"list":[{"id":1,"symbol":"ETHUSDT","side":"Long","status":"All Closed","avgCost":2028.4,"avgClosePrice":2041.57,"maxOpenInterest":0.039,"closedVolume":0.039,"closingPnl":0.5136,"opened":1779522626558,"closed":1779548800844,"updateTime":1779548800843}]}}`
		default:
			t.Fatalf("unexpected request path: %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	records, err := p.GetPositionHistory("leader-portfolio")
	if err != nil {
		t.Fatalf("GetPositionHistory error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records len=%d want 1", len(records))
	}
	if records[0].Symbol != "ETHUSDT" || records[0].Side != "Long" || records[0].AvgClosePrice != 2041.57 {
		t.Fatalf("unexpected record: %+v", records[0])
	}
}

func TestBinanceGetAccountStateInfersLeverageFromPositionMargins(t *testing.T) {
	p := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		switch {
		case strings.Contains(req.URL.Path, "/lead-portfolio/detail"):
			body = `{"code":"000000","data":{"copyPortfolioId":"copy-123","hasCopy":true,"marginBalance":"100.5"}}`
		case strings.Contains(req.URL.Path, "/user-position"):
			body = `{"code":"000000","data":[{
				"id":"1239518824_ETHUSDT_LONG",
				"symbol":"ETHUSDT",
				"positionSide":"LONG",
				"positionAmount":"0.02",
				"entryPrice":"2000",
				"markPrice":"2010",
				"notionalValue":"80",
				"initialMargin":"8",
				"positionInitialMargin":"4"
			},{
				"id":"1239518824_ETHUSDT_SHORT",
				"symbol":"ETHUSDT",
				"positionSide":"SHORT",
				"positionAmount":"-0.036",
				"entryPrice":"2110.62",
				"markPrice":"2099.83",
				"notionalValue":"-75.59388000",
				"initialMargin":"3.97862535",
				"positionInitialMargin":"3.97862535"
			},{
				"id":"1239518824_BTCUSDT_BOTH",
				"symbol":"BTCUSDT",
				"positionSide":"BOTH",
				"positionAmount":"-0.002",
				"entryPrice":"60000",
				"markPrice":"60000",
				"notionalValue":"-120",
				"initialMargin":"6",
				"positionInitialMargin":"0"
			}]}`
		default:
			t.Fatalf("unexpected request path: %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	state, err := p.GetAccountState("leader-portfolio")
	if err != nil {
		t.Fatalf("GetAccountState error: %v", err)
	}

	longPos := state.Positions["1239518824_ETHUSDT_LONG"]
	if longPos == nil {
		t.Fatalf("expected long position")
	}
	if longPos.Leverage != 20 {
		t.Fatalf("expected long leverage 20x from positionInitialMargin, got %dx", longPos.Leverage)
	}
	if longPos.PositionValue != 80 {
		t.Fatalf("expected long position value 80, got %.8f", longPos.PositionValue)
	}

	shortPos := state.Positions["1239518824_ETHUSDT_SHORT"]
	if shortPos == nil {
		t.Fatalf("expected short position")
	}
	if shortPos.Side != SideShort {
		t.Fatalf("expected short side, got %s", shortPos.Side)
	}
	if shortPos.Leverage != 19 {
		t.Fatalf("expected short leverage 19x from abs(notional)/positionInitialMargin, got %dx", shortPos.Leverage)
	}
	if shortPos.PositionValue != 75.59388 {
		t.Fatalf("expected positive short position value 75.59388, got %.8f", shortPos.PositionValue)
	}

	bothPos := state.Positions["1239518824_BTCUSDT_BOTH"]
	if bothPos == nil {
		t.Fatalf("expected BOTH position")
	}
	if bothPos.Side != SideShort {
		t.Fatalf("expected BOTH negative amount to map to short, got %s", bothPos.Side)
	}
	if bothPos.Leverage != 20 {
		t.Fatalf("expected BOTH leverage 20x from initialMargin fallback, got %dx", bothPos.Leverage)
	}
}
