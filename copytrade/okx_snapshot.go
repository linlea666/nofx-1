package copytrade

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type OKXSourceHTTPError struct {
	Status              int
	RetryAfter, Message string
}

func (e *OKXSourceHTTPError) Error() string {
	return fmt.Sprintf("OKX source HTTP %d: %s", e.Status, e.Message)
}

type okxSnapshotGate struct {
	mu       sync.Mutex
	state    *AccountState
	until    time.Time
	failures int
}

var okxSnapshotGates sync.Map

func (p *OKXProvider) snapshotGate(leader string) *okxSnapshotGate {
	// The transport component isolates tests/custom endpoints while default
	// production providers for the same public leader share one request stream.
	key := fmt.Sprintf("%p|%s", p.client.Transport, leader)
	gate, _ := okxSnapshotGates.LoadOrStore(key, &okxSnapshotGate{})
	return gate.(*okxSnapshotGate)
}

func (p *OKXProvider) UseAuthoritativeSnapshotHotPath() bool { return true }
func (p *OKXProvider) SuggestedSourcePollDelay(active bool) time.Duration {
	if active {
		return time.Second
	}
	return 3 * time.Second
}

func (p *OKXProvider) GetPositionSnapshot(leader string) (*AccountState, error) {
	g := p.snapshotGate(leader)
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.until) {
		return nil, fmt.Errorf("OKX source backoff until %s", g.until.UTC().Format(time.RFC3339))
	}
	if g.state != nil && time.Since(g.state.Timestamp) < 500*time.Millisecond {
		return cloneLeaderSnapshot(g.state), nil
	}
	state, err := p.readPositionSnapshot(leader)
	if err != nil {
		var httpErr *OKXSourceHTTPError
		if (errors.As(err, &httpErr) && httpErr.Status == http.StatusTooManyRequests) || strings.Contains(err.Error(), "code=50011") {
			g.failures++
			backoff := smartMoneyFallbackBackoff(g.failures)
			if httpErr != nil {
				if seconds, parseErr := strconv.Atoi(httpErr.RetryAfter); parseErr == nil && seconds > 0 {
					backoff = time.Duration(seconds) * time.Second
				} else if date, parseErr := http.ParseTime(httpErr.RetryAfter); parseErr == nil && time.Until(date) > 0 {
					backoff = time.Until(date)
				}
			}
			g.until = time.Now().Add(backoff)
		}
		return nil, err
	}
	g.state = state
	g.failures = 0
	return cloneLeaderSnapshot(state), nil
}

func cloneLeaderSnapshot(source *AccountState) *AccountState {
	copy := *source
	copy.Positions = make(map[string]*Position, len(source.Positions))
	for id, p := range source.Positions {
		position := *p
		copy.Positions[id] = &position
	}
	return &copy
}

func (e *Engine) currentSourceSnapshot() (*AccountState, error) {
	if p, ok := e.provider.(interface {
		GetPositionSnapshot(string) (*AccountState, error)
	}); ok {
		return p.GetPositionSnapshot(e.config.LeaderID)
	}
	return e.provider.GetAccountState(e.config.LeaderID)
}
