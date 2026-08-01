package copytrade

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	smartMoneyActivePollInterval = 3 * time.Second
	smartMoneyIdlePollInterval   = 15 * time.Second
	smartMoneyMaxBackoff         = 5 * time.Minute
)

// SmartMoneyBackoffError is a local scheduling decision, not an exchange
// observation. It prevents every engine sharing one Binance credential from
// hammering the endpoint during an explicit 429 window.
type SmartMoneyBackoffError struct {
	Until time.Time
}

func (e *SmartMoneyBackoffError) Error() string {
	if e == nil || e.Until.IsZero() {
		return "binance smart money source is backing off"
	}
	return fmt.Sprintf("binance smart money source backoff until %s", e.Until.UTC().Format(time.RFC3339))
}

type smartMoneyCredentialGate struct {
	requestMu sync.Mutex
	mu        sync.Mutex

	backoffUntil    time.Time
	consecutive429  int
	healthyAfter429 int
	total429        int
}

var smartMoneyCredentialGates sync.Map

func smartMoneyCredentialKey(p20t string) [32]byte {
	return sha256.Sum256([]byte(p20t))
}

func smartMoneyGateForCredential(p20t string) *smartMoneyCredentialGate {
	key := smartMoneyCredentialKey(p20t)
	gate, _ := smartMoneyCredentialGates.LoadOrStore(key, &smartMoneyCredentialGate{})
	return gate.(*smartMoneyCredentialGate)
}

func smartMoneyFallbackBackoff(failures int) time.Duration {
	schedule := [...]time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second, smartMoneyMaxBackoff}
	if failures <= 1 {
		return schedule[0]
	}
	if failures >= len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[failures-1]
}

func (g *smartMoneyCredentialGate) remaining(now time.Time) time.Duration {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.backoffUntil.After(now) {
		return 0
	}
	return g.backoffUntil.Sub(now)
}

func (g *smartMoneyCredentialGate) do(request func() ([]byte, int, error)) ([]byte, int, error) {
	if g == nil {
		return request()
	}
	// Only Smart Money source HTTP calls share this lock. Follower execution,
	// protective orders and exchange reconciliation use independent clients.
	g.requestMu.Lock()
	defer g.requestMu.Unlock()

	now := time.Now()
	if remaining := g.remaining(now); remaining > 0 {
		return nil, http.StatusTooManyRequests, &SmartMoneyBackoffError{Until: now.Add(remaining)}
	}
	raw, status, err := request()
	var httpErr *BinanceHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusTooManyRequests {
		g.mu.Lock()
		g.consecutive429++
		g.total429++
		g.healthyAfter429 = 0
		backoff := httpErr.RetryAfter
		if backoff <= 0 {
			backoff = smartMoneyFallbackBackoff(g.consecutive429)
		}
		if backoff > smartMoneyMaxBackoff {
			backoff = smartMoneyMaxBackoff
		}
		g.backoffUntil = time.Now().Add(backoff)
		g.mu.Unlock()
		return raw, status, err
	}
	return raw, status, err
}

// recordHealthySnapshot advances recovery only after the provider has decoded
// every page and validated a complete source snapshot. Counting individual
// HTTP successes would restore the three-second cadence in the middle of one
// paginated poll and would not prove sustained source health.
func (g *smartMoneyCredentialGate) recordHealthySnapshot() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.backoffUntil = time.Time{}
	if g.consecutive429 == 0 {
		return
	}
	g.healthyAfter429++
	if g.healthyAfter429 >= 3 {
		g.consecutive429 = 0
		g.healthyAfter429 = 0
	}
}

func (g *smartMoneyCredentialGate) suggestedPollDelay(active bool) time.Duration {
	delay := smartMoneyIdlePollInterval
	if active {
		delay = smartMoneyActivePollInterval
	}
	if g == nil {
		return delay
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.consecutive429 == 0 {
		return delay
	}
	// Full-snapshot recovery ladder: 15s -> 6s -> normal 3s. Idle
	// sources remain at their normal 15-second cadence.
	recoveryDelay := 15 * time.Second
	if g.healthyAfter429 >= 2 {
		recoveryDelay = 6 * time.Second
	}
	if recoveryDelay > delay {
		return recoveryDelay
	}
	return delay
}

func (g *smartMoneyCredentialGate) snapshot() (time.Time, int) {
	if g == nil {
		return time.Time{}, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.backoffUntil, g.total429
}

// SourcePollScheduleProvider lets a source tune only its own next poll. It is
// deliberately separate from execution and protection scheduling.
type SourcePollScheduleProvider interface {
	SuggestedSourcePollDelay(activeLeaderPosition bool) time.Duration
}
