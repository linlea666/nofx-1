package copytrade

import (
	"errors"
	"net/http"
	"strings"
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
	p := NewBinanceProvider("classic-gate-p20t", "csrf")
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
