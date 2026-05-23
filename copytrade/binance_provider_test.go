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
