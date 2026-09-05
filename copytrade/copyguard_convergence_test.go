package copytrade

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

// TestFollowerPositionQuantityAttributesByCycleScope reproduces the production
// divergence: one leader ran a cross and an isolated position on the same
// symbol/side, so Copy Guard opened two cycles that each protect only their own
// slice. Measuring one slice against the account-wide sum made coverage
// permanently irreconcilable and drove a 3s re-arm loop.
func TestFollowerPositionQuantityAttributesByCycleScope(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "trader-1",
		executor: &livePositionExecutor{positions: []map[string]interface{}{
			{"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "posId": "cross-pos", "positionAmt": 0.0231},
			{"symbol": "BTCUSDT", "side": "long", "mgnMode": "isolated", "posId": "iso-pos", "positionAmt": 0.0026},
		}},
	}

	for _, tc := range []struct {
		name       string
		marginMode string
		posID      string
		want       float64
	}{
		{"cross cycle by posId", "cross", "cross-pos", 0.0231},
		{"isolated cycle by posId", "isolated", "iso-pos", 0.0026},
		{"cross cycle without posId", "cross", "", 0.0231},
		{"isolated cycle without posId", "isolated", "", 0.0026},
	} {
		qty, ok := ti.followerPositionQuantity("BTCUSDT", "long", tc.marginMode, tc.posID, false)
		if !ok || qty != tc.want {
			t.Fatalf("%s: got qty=%.8f ok=%v, want %.8f (account-wide sum would be 0.0257)",
				tc.name, qty, ok, tc.want)
		}
	}
}

// TestFollowerPositionQuantityFailsClosedOnUnattributableScope covers the
// fail-open that caused the incident: a dropped mgnMode field made the margin-mode
// filter match everything. An unusable scope must report "unknown", never a sum
// that would be read as insufficient coverage.
func TestFollowerPositionQuantityFailsClosedOnUnattributableScope(t *testing.T) {
	t.Run("margin mode not reported by the exchange", func(t *testing.T) {
		ti := &TraderIntegration{
			traderID: "trader-1",
			executor: &livePositionExecutor{positions: []map[string]interface{}{
				{"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.0231},
				{"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.0026},
			}},
		}
		if qty, ok := ti.followerPositionQuantity("BTCUSDT", "long", "cross", "", false); ok {
			t.Fatalf("missing margin mode was silently aggregated: qty=%.8f", qty)
		}
	})

	t.Run("several positions share one scope", func(t *testing.T) {
		ti := &TraderIntegration{
			traderID: "trader-1",
			executor: &livePositionExecutor{positions: []map[string]interface{}{
				{"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "posId": "a", "positionAmt": 0.0231},
				{"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "posId": "b", "positionAmt": 0.0026},
			}},
		}
		if qty, ok := ti.followerPositionQuantity("BTCUSDT", "long", "cross", "unknown-pos", false); ok {
			t.Fatalf("ambiguous scope was silently aggregated: qty=%.8f", qty)
		}
		// The same scope stays resolvable when the cycle's own posId is present.
		if qty, ok := ti.followerPositionQuantity("BTCUSDT", "long", "cross", "b", false); !ok || qty != 0.0026 {
			t.Fatalf("posId attribution failed: qty=%.8f ok=%v", qty, ok)
		}
	})

	t.Run("a flat scope is known, not unknown", func(t *testing.T) {
		ti := &TraderIntegration{traderID: "trader-1", executor: &livePositionExecutor{}}
		qty, ok := ti.followerPositionQuantity("BTCUSDT", "long", "cross", "gone", false)
		if !ok || qty != 0 {
			t.Fatalf("closed position must read as flat: qty=%.8f ok=%v", qty, ok)
		}
	})
}

// TestProtectionRearmThrottleConvergesOscillatingCycle covers the counter that
// protection_retries cannot provide: it is reset as soon as the cycle reports
// healthy, so "arm succeeds -> verification fails -> arm again" never reaches the
// retry cap and re-arms forever at poll cadence.
func TestProtectionRearmThrottleConvergesOscillatingCycle(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-rearm-throttle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ti := &TraderIntegration{
		traderID:         "trader-1",
		store:            st,
		protectionRearms: make(map[int64]*protectionRearmTracker),
	}
	cycle := &store.CopyGuardCycle{PolicySnapshot: `{"version":4}`, ID: 425, Symbol: "BTCUSDT", Side: "long", ProtectionStatus: "DEGRADED"}

	for i := 1; i < protectionRearmWindowMax; i++ {
		if ti.noteProtectionRearm(cycle) {
			t.Fatalf("re-arm %d throttled before the window limit", i)
		}
		if ti.protectionRearmThrottled(cycle.ID) {
			t.Fatalf("cycle reported as throttled after only %d re-arms", i)
		}
	}
	if !ti.noteProtectionRearm(cycle) {
		t.Fatalf("re-arm %d did not trigger throttling", protectionRearmWindowMax)
	}
	if !ti.protectionRearmThrottled(cycle.ID) {
		t.Fatal("throttled cadence was not retained for the cycle")
	}
	// Continued oscillation must not re-announce.
	for i := 0; i < 5; i++ {
		if !ti.noteProtectionRearm(cycle) {
			t.Fatal("throttling was lost while the oscillation continued")
		}
	}
	events, err := st.CopyTrade().ListCopyGuardEvents(425)
	if err != nil {
		t.Fatal(err)
	}
	throttled := 0
	for _, event := range events {
		if event.Type == "PROTECTION_RETRY_THROTTLED" {
			throttled++
		}
	}
	if throttled != 1 {
		t.Fatalf("expected exactly one throttling notice, got %d", throttled)
	}

	// A new window (the oscillation stopped and later resumed) starts clean, so a
	// transient fault is never permanently penalised.
	ti.protectionRearms[cycle.ID].windowStart = time.Now().Add(-protectionRearmWindow - time.Second)
	if ti.noteProtectionRearm(cycle) || ti.protectionRearmThrottled(cycle.ID) {
		t.Fatal("expired window did not reset the re-arm counter")
	}
}

// TestStopRiskThresholdAlertConverges covers the alert the user reported as
// spamming: the condition is a state recomputed every 3s, so announcing it per
// evaluation produced hundreds of events and emails. It must announce on first
// crossing, on a materially deeper overshoot, and on recovery-then-recrossing.
func TestStopRiskThresholdAlertConverges(t *testing.T) {
	ti := &TraderIntegration{
		traderID:       "trader-1",
		stopRiskAlerts: make(map[string]*stopRiskAlertState),
	}
	const cycleID = int64(431)
	const threshold = 4.0

	alert, tier := ti.shouldAlertStopRiskThreshold(cycleID, 0, true, 5.0, threshold)
	if !alert {
		t.Fatal("first crossing must be announced")
	}
	firstTier := tier

	// Live price/ATR drift inside the same band, and even a small deepening, must
	// stay silent.
	for _, lossPct := range []float64{5.0, 5.0001, 5.4, 4.9, 5.2} {
		if again, _ := ti.shouldAlertStopRiskThreshold(cycleID, 0, true, lossPct, threshold); again {
			t.Fatalf("re-announced the same band at expected loss %.4f%%", lossPct)
		}
	}

	// A full band deeper is materially different and is worth one more notice.
	// 6.5% against a 4% line lands one stopRiskAlertTierStep above 5.0%.
	deeper, deeperTier := ti.shouldAlertStopRiskThreshold(cycleID, 0, true, 6.5, threshold)
	if !deeper || deeperTier <= firstTier {
		t.Fatalf("materially deeper overshoot was suppressed: alert=%v tier=%d first=%d", deeper, deeperTier, firstTier)
	}

	// A different re-entry attempt is a different position and tracks separately.
	if attemptAlert, _ := ti.shouldAlertStopRiskThreshold(cycleID, 1, true, 5.0, threshold); !attemptAlert {
		t.Fatal("a new attempt must be able to announce its own first crossing")
	}

	// Recovery rearms, so a later recrossing is reported instead of silently lost.
	if cleared, _ := ti.shouldAlertStopRiskThreshold(cycleID, 0, false, 1.0, threshold); cleared {
		t.Fatal("a cleared condition must not alert")
	}
	if recrossed, _ := ti.shouldAlertStopRiskThreshold(cycleID, 0, true, 5.0, threshold); !recrossed {
		t.Fatal("recrossing after recovery must be announced again")
	}
}

// TestProtectionSignatureBucketQuantizesDedupeKeys pins why dedupe keys must not
// contain raw floats: a stop price recomputed from live ATR drifts on every poll,
// so a float key changes every time and de-duplication degrades to "record
// everything".
func TestProtectionSignatureBucketQuantizesDedupeKeys(t *testing.T) {
	base := protectionSignatureBucket(42123.5)
	if protectionSignatureBucket(42125.0) != base {
		t.Fatal("negligible price drift produced a new dedupe bucket")
	}
	if protectionSignatureBucket(42123.5*1.01) == base {
		t.Fatal("a 1% price move must produce a new dedupe bucket")
	}
	if protectionSignatureBucket(0) != 0 {
		t.Fatal("zero must map to a stable bucket")
	}
	// Scale-free: the same relative move separates buckets for small quantities.
	if protectionSignatureBucket(0.0231) == protectionSignatureBucket(0.0257) {
		t.Fatal("distinct protective quantities collapsed into one bucket")
	}
}
