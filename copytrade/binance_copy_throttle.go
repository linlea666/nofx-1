package copytrade

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Binance web throttles the copy-management position endpoint (user-position)
// per account: a probe on 2026-11-22 passed 10 reads in a minute and was
// rejected with 90801003 on the 11th; a rejected account stays blocked for
// several minutes and retries inside the block renew it. Every program using
// the same Binance login shares this budget. This throttle is the single
// choke point for that endpoint and applies to Binance copy-management
// sources only; Smart Money and every other source keep their own cadence.
const (
	// 6 reads/minute while the leader holds positions leaves headroom under
	// the ~10/minute budget.
	binanceCopyActivePollInterval = 10 * time.Second
	binanceCopyIdlePollInterval   = 20 * time.Second
	// Reads closer together than this reuse the last complete snapshot (the
	// startup baseline followed by the first sync, pre-submit revalidation,
	// operator actions and previews), so they never exceed the poll budget.
	binanceCopyMinRequestInterval = 10 * time.Second
	// Longer than the observed block window, so the first retry is not itself
	// rejected and renews the block.
	binanceCopyRateLimitCooldown = 10 * time.Minute
)

// BinanceCopyRateLimitError reports that the copy-management position endpoint
// is cooling down after a rate limit; no request is sent until Until.
type BinanceCopyRateLimitError struct {
	Until time.Time
	Cause error
}

func (e *BinanceCopyRateLimitError) Error() string {
	return fmt.Sprintf("币安跟单持仓接口限流冷却中（90801003），%s 后再试；冷却期内不发请求，避免延长封锁",
		e.Until.Local().Format("15:04:05"))
}

func (e *BinanceCopyRateLimitError) Unwrap() error { return e.Cause }

type binanceCopySnapshot struct {
	state *AccountState
	at    time.Time
}

// binanceCopyThrottle is shared by every copy-management reader of one
// credential: engines, previews and revalidations.
type binanceCopyThrottle struct {
	// fetchMu serializes reads so concurrent callers share one request.
	fetchMu sync.Mutex

	mu            sync.Mutex
	lastRequestAt time.Time
	cooldownUntil time.Time
	snapshots     map[string]binanceCopySnapshot
}

var binanceCopyThrottles sync.Map

func binanceCopyThrottleFor(p20t string) *binanceCopyThrottle {
	throttle, _ := binanceCopyThrottles.LoadOrStore(smartMoneyCredentialKey(p20t), &binanceCopyThrottle{})
	return throttle.(*binanceCopyThrottle)
}

// cooldownRemaining is how long the endpoint must stay untouched.
func (t *binanceCopyThrottle) cooldownRemaining(now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cooldownUntil.After(now) {
		return 0
	}
	return t.cooldownUntil.Sub(now)
}

// read returns a recent complete snapshot of leaderID or performs one spaced,
// serialized request through fetch. A rate-limited response starts the
// cooldown; callers then fail closed until it ends.
func (t *binanceCopyThrottle) read(leaderID string, fetch func() (*AccountState, error)) (*AccountState, error) {
	t.fetchMu.Lock()
	defer t.fetchMu.Unlock()

	t.mu.Lock()
	now := time.Now()
	if snapshot, ok := t.snapshots[leaderID]; ok && now.Sub(snapshot.at) < binanceCopyMinRequestInterval {
		t.mu.Unlock()
		return cloneLeaderSnapshot(snapshot.state), nil
	}
	if t.cooldownUntil.After(now) {
		until := t.cooldownUntil
		t.mu.Unlock()
		return nil, &BinanceCopyRateLimitError{Until: until}
	}
	wait := binanceCopyMinRequestInterval - now.Sub(t.lastRequestAt)
	t.mu.Unlock()
	if !t.lastRequestAt.IsZero() && wait > 0 {
		// Another leader on this credential was just read; keep the spacing.
		time.Sleep(wait)
	}

	state, err := fetch()

	t.mu.Lock()
	defer t.mu.Unlock()
	var backoff *SmartMoneyBackoffError
	if !errors.As(err, &backoff) {
		// The local gate backoff sends nothing; everything else reached Binance.
		t.lastRequestAt = time.Now()
	}
	var httpErr *BinanceHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusTooManyRequests {
		t.cooldownUntil = time.Now().Add(binanceCopyRateLimitCooldown)
		return nil, &BinanceCopyRateLimitError{Until: t.cooldownUntil, Cause: err}
	}
	if err != nil {
		return nil, err
	}
	if t.snapshots == nil {
		t.snapshots = map[string]binanceCopySnapshot{}
	}
	t.snapshots[leaderID] = binanceCopySnapshot{state: cloneLeaderSnapshot(state), at: t.lastRequestAt}
	return state, nil
}
