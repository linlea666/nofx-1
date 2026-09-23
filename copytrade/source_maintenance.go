package copytrade

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Recent account samples are a sizing prerequisite, never a prerequisite for
// source exits. Each account endpoint refreshes independently of position
// polling; stale/failed samples defer opening instead of changing the formula.
func (e *Engine) startOKXAccountSampler(ctx context.Context) {
	provider, ok := e.provider.(interface{ GetLeaderEquity(string) (float64, error) })
	if !ok {
		return
	}
	sample := func(read func() (float64, error)) func() (float64, error) {
		var mu sync.RWMutex
		var value float64
		var sampled time.Time
		var lastErr error
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				v, err := read()
				mu.Lock()
				value, sampled, lastErr = v, time.Now(), err
				mu.Unlock()
				select {
				case <-ctx.Done():
					return
				case <-e.stopCh:
					return
				case <-ticker.C:
				}
			}
		}()
		return func() (float64, error) {
			mu.RLock()
			defer mu.RUnlock()
			if lastErr != nil {
				return 0, lastErr
			}
			if sampled.IsZero() || time.Since(sampled) > 3*time.Second || invalidPositive(value) {
				return 0, fmt.Errorf("fresh account equity pending")
			}
			return value, nil
		}
	}
	e.leaderEquityForSizing = sample(func() (float64, error) { return provider.GetLeaderEquity(e.config.LeaderID) })
	readFollower := e.getFollowerEquity
	if readFollower == nil {
		readFollower = e.getFollowerBalance
	}
	if readFollower != nil {
		get := sample(func() (float64, error) { return readFollower(), nil })
		e.getFollowerEquity = func() float64 { v, _ := get(); return v }
	}
}

// Expensive private fill/history reconciliation is independent of the OKX
// position signal loop. These maintenance counters have exactly one writer.
func (e *Engine) sourceMaintenanceLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-ticker.C:
		}
		e.checkManualStoppedClosed(e.buildLeaderPosMap())
		if e.config.RiskPolicyVersion >= 4 {
			e.checkStoppedByRisk()
			if e.config.FollowExitPolicyVersion < 2 {
				e.checkReentryConditions()
				e.evaluateCloseInvalidations()
			}
			e.refreshEstimatedBaselines()
		}
	}
}
