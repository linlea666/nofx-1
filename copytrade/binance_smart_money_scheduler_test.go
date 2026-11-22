package copytrade

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSmartMoneyFallbackBackoffSchedule(t *testing.T) {
	want := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second, 300 * time.Second}
	for i, expected := range want {
		if got := smartMoneyFallbackBackoff(i + 1); got != expected {
			t.Fatalf("failure %d backoff=%s want=%s", i+1, got, expected)
		}
	}
}

func TestSmartMoneyCredentialGateHonorsRetryAfterAndIsCredentialScoped(t *testing.T) {
	limited := &smartMoneyCredentialGate{}
	other := &smartMoneyCredentialGate{}
	_, _, err := limited.do(func() ([]byte, int, error) {
		return nil, http.StatusTooManyRequests, &BinanceHTTPError{StatusCode: http.StatusTooManyRequests, RetryAfter: 45 * time.Second}
	})
	if err == nil {
		t.Fatal("429 must be returned to the source health path")
	}
	until, total := limited.snapshot()
	if total != 1 || time.Until(until) < 44*time.Second {
		t.Fatalf("Retry-After was not persisted: until=%s total=%d", until, total)
	}
	called := false
	if _, _, err = other.do(func() ([]byte, int, error) {
		called = true
		return []byte("ok"), http.StatusOK, nil
	}); err != nil || !called {
		t.Fatalf("one credential backoff blocked another credential: called=%v err=%v", called, err)
	}
	called = false
	_, _, err = limited.do(func() ([]byte, int, error) {
		called = true
		return nil, 0, nil
	})
	var backoffErr *SmartMoneyBackoffError
	if !errors.As(err, &backoffErr) || called {
		t.Fatalf("backoff must suppress only the limited credential request: called=%v err=%v", called, err)
	}
}

func TestSmartMoneyCredentialGateRestoresCadenceAfterCompleteSnapshots(t *testing.T) {
	gate := &smartMoneyCredentialGate{consecutive429: 2}
	if got := gate.suggestedPollDelay(true); got != 15*time.Second {
		t.Fatalf("pre-recovery delay=%s", got)
	}
	gate.recordHealthySnapshot()
	if got := gate.suggestedPollDelay(true); got != 15*time.Second {
		t.Fatalf("first healthy snapshot delay=%s", got)
	}
	gate.recordHealthySnapshot()
	if got := gate.suggestedPollDelay(true); got != 6*time.Second {
		t.Fatalf("second healthy snapshot delay=%s", got)
	}
	gate.recordHealthySnapshot()
	if got := gate.suggestedPollDelay(true); got != smartMoneyActivePollInterval {
		t.Fatalf("third healthy snapshot did not restore active cadence: %s", got)
	}
}

func TestClassicBinanceSourceRequestsUseCredentialScoped429Gate(t *testing.T) {
	const credential = "classic-gate-p20t"
	key := smartMoneyCredentialKey(credential)
	// The production gate is intentionally process-global. Tests must remove
	// their credential-scoped entry before and after use or a repeated/full
	// suite can inherit this test's 60-second 429 backoff.
	smartMoneyCredentialGates.Delete(key)
	t.Cleanup(func() { smartMoneyCredentialGates.Delete(key) })
	p := NewBinanceProvider(credential, "csrf")
	calls := 0
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		response := smartHTTPResponse(http.StatusTooManyRequests, `{"code":"429"}`)
		response.Header.Set("Retry-After", "60")
		return response, nil
	})
	_, _, firstErr := p.sourceWebRequest(http.MethodPost, "https://example.invalid/positions", map[string]interface{}{})
	if firstErr == nil || !strings.Contains(firstErr.Error(), "429") || calls != 1 {
		t.Fatalf("first request must preserve 429: calls=%d err=%v", calls, firstErr)
	}
	_, _, secondErr := p.sourceWebRequest(http.MethodPost, "https://example.invalid/positions", map[string]interface{}{})
	var backoffErr *SmartMoneyBackoffError
	if !errors.As(secondErr, &backoffErr) || calls != 1 {
		t.Fatalf("backoff must suppress another classic source request: calls=%d err=%v", calls, secondErr)
	}
}

// Binance web throttles copy-management positions with HTTP 200 and business
// code 90801003. It must enter the same backoff as an HTTP 429, otherwise the
// 3-second poll keeps the limiter tripped and every leader signal is skipped.
func TestClassicBinanceBusinessRateLimitCodeBacksOff(t *testing.T) {
	const credential = "classic-business-rate-limit-p20t"
	key := smartMoneyCredentialKey(credential)
	smartMoneyCredentialGates.Delete(key)
	t.Cleanup(func() { smartMoneyCredentialGates.Delete(key) })
	p := NewBinanceProvider(credential, "csrf")
	calls := 0
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return smartHTTPResponse(http.StatusOK, `{"code":"90801003","message":"Too many requests. Please use WebSocket for updates.","data":null,"success":false}`), nil
	})
	_, status, firstErr := p.sourceWebRequest(http.MethodPost, "https://example.invalid/positions", map[string]interface{}{})
	var httpErr *BinanceHTTPError
	if !errors.As(firstErr, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests || status != http.StatusTooManyRequests || calls != 1 {
		t.Fatalf("business rate-limit code must surface as 429: status=%d calls=%d err=%v", status, calls, firstErr)
	}
	until, total := smartMoneyGateForCredential(credential).snapshot()
	if total != 1 || time.Until(until) < 14*time.Second {
		t.Fatalf("business rate-limit code must start the fallback backoff: until=%s total=%d", until, total)
	}
	_, _, secondErr := p.sourceWebRequest(http.MethodPost, "https://example.invalid/positions", map[string]interface{}{})
	var backoffErr *SmartMoneyBackoffError
	if !errors.As(secondErr, &backoffErr) || calls != 1 {
		t.Fatalf("backoff must suppress the next request: calls=%d err=%v", calls, secondErr)
	}
	if delay := p.SuggestedSourcePollDelay(true); delay < 14*time.Second {
		t.Fatalf("poll cadence must honour the backoff, got %s", delay)
	}
}

func TestBinanceWebRateLimitCodeDetection(t *testing.T) {
	for body, want := range map[string]bool{
		`{"code":"90801003","message":"Too many requests."}`: true,
		`{"code":90801003}`:           true,
		`{"code":"000000","data":[]}`: false,
		`{"code":"100001005"}`:        false,
		`not json`:                    false,
	} {
		if got := isBinanceWebRateLimited([]byte(body)); got != want {
			t.Fatalf("isBinanceWebRateLimited(%s)=%v want %v", body, got, want)
		}
	}
}

// A credential lease covers the complete paginated snapshot, not one HTTP
// page. Otherwise two leaders sharing the global credential can interleave
// dozens of pages and recreate the production 429 storm even though each page
// individually passes through requestMu.
func TestSmartMoneyCredentialGateSerializesCompleteSnapshots(t *testing.T) {
	gate := &smartMoneyCredentialGate{}
	var active int32
	var maxActive int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			gate.doSnapshot(func() {
				current := atomic.AddInt32(&active, 1)
				for {
					previous := atomic.LoadInt32(&maxActive)
					if current <= previous || atomic.CompareAndSwapInt32(&maxActive, previous, current) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt32(&active, -1)
			})
		}()
	}
	close(start)
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("complete snapshots overlapped under one credential: max=%d", maxActive)
	}
}
