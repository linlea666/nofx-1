package copytrade

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func binanceCopyThrottleTestProvider(t *testing.T, positionBody func() string) (*BinanceProvider, *int) {
	t.Helper()
	positionCalls := 0
	p := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		switch {
		case strings.Contains(req.URL.Path, "/lead-portfolio/detail"):
			body = `{"code":"000000","data":{"copyPortfolioId":"copy-123","hasCopy":true,"marginBalance":"1000"}}`
		case strings.Contains(req.URL.Path, "/user-position"):
			positionCalls++
			body = positionBody()
		default:
			t.Fatalf("unexpected request path: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() {
		binanceCopyThrottles.Delete(smartMoneyCredentialKey("p20t"))
		smartMoneyCredentialGates.Delete(smartMoneyCredentialKey("p20t"))
	})
	smartMoneyCredentialGates.Delete(smartMoneyCredentialKey("p20t"))
	return p, &positionCalls
}

const binanceThrottleTestPosition = `{"code":"000000","data":[{"id":"1_ETHUSDT_LONG","symbol":"ETHUSDT","positionSide":"LONG","positionAmount":"0.5","entryPrice":"2000","markPrice":"2000","notionalValue":"1000","positionInitialMargin":"100"}]}`

// Startup reads the baseline and then immediately syncs: in production the
// second read in the same second was rejected with 90801003 and the trader
// refused to start. Both must be served by one request.
func TestBinanceCopyBackToBackReadsShareOneRequest(t *testing.T) {
	p, calls := binanceCopyThrottleTestProvider(t, func() string { return binanceThrottleTestPosition })
	first, err := p.GetAccountState("lead")
	if err != nil {
		t.Fatal(err)
	}
	first.Positions["1_ETHUSDT_LONG"].Size = 99 // callers own their copy
	second, err := p.GetAccountState("lead")
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("back-to-back reads sent %d position requests, want 1", *calls)
	}
	if got := second.Positions["1_ETHUSDT_LONG"].Size; got != 0.5 {
		t.Fatalf("shared snapshot was mutated through another caller: size=%v", got)
	}
}

// A rejected read must stop every request for the full cooldown instead of
// probing every few minutes and renewing Binance's block.
func TestBinanceCopyRateLimitStartsCooldownWithoutRequests(t *testing.T) {
	p, calls := binanceCopyThrottleTestProvider(t, func() string {
		return `{"code":"90801003","message":"Too many requests. Please use WebSocket for updates.","success":false}`
	})
	_, err := p.GetAccountState("lead")
	var limited *BinanceCopyRateLimitError
	if !errors.As(err, &limited) || time.Until(limited.Until) < binanceCopyRateLimitCooldown-time.Minute {
		t.Fatalf("rate limit must start the cooldown: %v", err)
	}
	if _, err = p.GetAccountState("lead"); !errors.As(err, &limited) || *calls != 1 {
		t.Fatalf("cooldown must not send requests: calls=%d err=%v", *calls, err)
	}
	if delay := p.SuggestedSourcePollDelay(true); delay < binanceCopyRateLimitCooldown-time.Minute {
		t.Fatalf("poll cadence must wait out the cooldown, got %s", delay)
	}
	if !strings.Contains(err.Error(), "冷却") {
		t.Fatalf("start failures must explain the cooldown: %v", err)
	}
}

func TestBinanceCopyManagementCadenceIsConservative(t *testing.T) {
	p, _ := binanceCopyThrottleTestProvider(t, func() string { return binanceThrottleTestPosition })
	if got := p.SuggestedSourcePollDelay(true); got != binanceCopyActivePollInterval {
		t.Fatalf("active cadence=%s want %s", got, binanceCopyActivePollInterval)
	}
	if got := p.SuggestedSourcePollDelay(false); got != binanceCopyIdlePollInterval {
		t.Fatalf("idle cadence=%s want %s", got, binanceCopyIdlePollInterval)
	}
	// Smart Money (public leaders) keeps its own cadence.
	smart := NewBinanceSmartMoneyProvider("smart-cadence-p20t", "csrf")
	t.Cleanup(func() { smartMoneyCredentialGates.Delete(smartMoneyCredentialKey("smart-cadence-p20t")) })
	if got := smart.SuggestedSourcePollDelay(true); got != smartMoneyActivePollInterval {
		t.Fatalf("smart money cadence changed: %s", got)
	}
}
