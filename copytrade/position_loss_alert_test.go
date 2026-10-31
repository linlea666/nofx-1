package copytrade

import (
	"errors"
	"strings"
	"testing"
	"time"

	"nofx/decision"
	"nofx/notifier"
)

type positionLossAlertExecutor struct {
	positions []map[string]interface{}
	err       error
}

func (e *positionLossAlertExecutor) ExecuteDecision(*decision.Decision) error { return nil }
func (e *positionLossAlertExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
func (e *positionLossAlertExecutor) GetPositions() ([]map[string]interface{}, error) {
	return e.positions, e.err
}

func lossAlertPosition(key string, pnlPct float64) map[string]interface{} {
	return map[string]interface{}{
		"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "leverage": 10,
		"entry_price": 100.0, "mark_price": 95.0, "liquidation_price": 91.0, "quantity": 1.0,
		"unrealized_pnl": -5.0, "unrealized_pnl_pct": pnlPct, "margin_used": 9.5,
		"position_key": key,
	}
}

func resetPositionLossAlertsForTest(t *testing.T) *notifier.CaptureNotifier {
	t.Helper()
	positionLossAlerts.Lock()
	positionLossAlerts.entries = make(map[string]map[string]*positionLossAlertEntry)
	positionLossAlerts.Unlock()
	captured := &notifier.CaptureNotifier{}
	t.Cleanup(notifier.SetGlobalForTesting(captured, false))
	return captured
}

func TestPositionLossAlertSendsOncePerCrossingAndRearmsAfterRecovery(t *testing.T) {
	captured := resetPositionLossAlertsForTest(t)
	ex := &positionLossAlertExecutor{positions: []map[string]interface{}{lossAlertPosition("BTCUSDT|long|cross|1", -30)}}
	ti := NewTraderIntegration("t1", ex, nil)

	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 0 {
		t.Fatal("alert sent before threshold")
	}
	ex.positions[0]["unrealized_pnl_pct"] = -50.0
	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 1 {
		t.Fatalf("expected exactly one alert at threshold, got %d", len(captured.Alerts))
	}
	alert := captured.Alerts[0]
	if alert.Category != "copy_trade" || alert.TraderID != "t1" || !strings.Contains(alert.Title, "BTCUSDT long") ||
		!strings.Contains(alert.Title, "-50.00%") || alert.StatusHook == nil {
		t.Fatalf("unexpected alert: %+v", alert)
	}
	for _, want := range []string{"手动调整止损", "强平价格: 91", "距强平: 4.21%", "-40.00%"} {
		if !strings.Contains(alert.Body, want) {
			t.Fatalf("body missing %q: %s", want, alert.Body)
		}
	}

	// Deeper loss and oscillation inside the hysteresis band never resend.
	for _, pct := range []float64{-70, -45, -41, -60} {
		ex.positions[0]["unrealized_pnl_pct"] = pct
		ti.checkPositionLossAlerts("exchange:a", 50)
	}
	if len(captured.Alerts) != 1 {
		t.Fatalf("repeated alert while still underwater: %d", len(captured.Alerts))
	}

	// Recovery above -40% rearms; a new crossing alerts once more.
	ex.positions[0]["unrealized_pnl_pct"] = -39.0
	ti.checkPositionLossAlerts("exchange:a", 50)
	ex.positions[0]["unrealized_pnl_pct"] = -55.0
	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 2 {
		t.Fatalf("expected rearmed alert, got %d", len(captured.Alerts))
	}
}

func TestPositionLossAlertClosedPositionClearsStateAndFetchErrorKeepsIt(t *testing.T) {
	captured := resetPositionLossAlertsForTest(t)
	ex := &positionLossAlertExecutor{positions: []map[string]interface{}{lossAlertPosition("BTCUSDT|long|isolated|", -60)}}
	ti := NewTraderIntegration("t1", ex, nil)
	ti.checkPositionLossAlerts("exchange:a", 50)

	// A failed snapshot must not be mistaken for a flat account.
	ex.err = errors.New("venue timeout")
	ti.checkPositionLossAlerts("exchange:a", 50)
	ex.err = nil
	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 1 {
		t.Fatalf("fetch error reset alert state: %d", len(captured.Alerts))
	}

	saved := ex.positions
	ex.positions = nil
	ti.checkPositionLossAlerts("exchange:a", 50)
	ex.positions = saved
	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 2 {
		t.Fatalf("new position after close should be monitored again: %d", len(captured.Alerts))
	}
}

func TestPositionLossAlertSharedAccountSendsOnce(t *testing.T) {
	captured := resetPositionLossAlertsForTest(t)
	pos := []map[string]interface{}{lossAlertPosition("BTCUSDT|long|cross|1", -80)}
	a := NewTraderIntegration("t1", &positionLossAlertExecutor{positions: pos}, nil)
	b := NewTraderIntegration("t2", &positionLossAlertExecutor{positions: pos}, nil)
	a.checkPositionLossAlerts("exchange:shared", 50)
	b.checkPositionLossAlerts("exchange:shared", 50)
	if len(captured.Alerts) != 1 {
		t.Fatalf("shared account position alerted %d times", len(captured.Alerts))
	}
	// Different accounts are independent.
	c := NewTraderIntegration("t3", &positionLossAlertExecutor{positions: pos}, nil)
	c.checkPositionLossAlerts("exchange:other", 50)
	if len(captured.Alerts) != 2 {
		t.Fatalf("independent account suppressed: %d", len(captured.Alerts))
	}
}

func TestPositionLossAlertFailedDeliveryRetriesAfterDelay(t *testing.T) {
	captured := resetPositionLossAlertsForTest(t)
	ex := &positionLossAlertExecutor{positions: []map[string]interface{}{lossAlertPosition("BTCUSDT|long|cross|1", -55)}}
	ti := NewTraderIntegration("t1", ex, nil)
	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 1 {
		t.Fatal("missing first alert")
	}
	// Queued/sent/rate-limited outcomes keep the one-shot state.
	captured.Alerts[0].StatusHook(notifier.DeliveryQueued, nil)
	captured.Alerts[0].StatusHook(notifier.DeliverySent, nil)
	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 1 {
		t.Fatal("successful delivery was retried")
	}

	captured.Alerts[0].StatusHook(notifier.DeliveryFailed, errors.New("smtp down"))
	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 1 {
		t.Fatal("retry did not wait for the retry delay")
	}
	positionLossAlerts.Lock()
	positionLossAlerts.entries["exchange:a"]["BTCUSDT|long|cross|1"].retryAt = time.Now().Add(-time.Second)
	positionLossAlerts.Unlock()
	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 2 {
		t.Fatalf("failed delivery was not retried: %d", len(captured.Alerts))
	}
	// A stale hook from the first attempt must not affect the new attempt.
	captured.Alerts[0].StatusHook(notifier.DeliveryFailed, errors.New("late"))
	positionLossAlerts.Lock()
	retryAt := positionLossAlerts.entries["exchange:a"]["BTCUSDT|long|cross|1"].retryAt
	positionLossAlerts.Unlock()
	if !retryAt.IsZero() {
		t.Fatal("stale delivery hook rescheduled the current alert")
	}
}

func TestPositionLossAlertIgnoresInvalidDataAndStaleState(t *testing.T) {
	captured := resetPositionLossAlertsForTest(t)
	bad := lossAlertPosition("BTCUSDT|long|cross|1", -90)
	delete(bad, "unrealized_pnl_pct")
	ex := &positionLossAlertExecutor{positions: []map[string]interface{}{bad, nil}}
	ti := NewTraderIntegration("t1", ex, nil)
	ti.checkPositionLossAlerts("exchange:a", 50)
	ti.checkPositionLossAlerts("exchange:a", 0)
	if len(captured.Alerts) != 0 {
		t.Fatal("alert sent without a valid pnl percentage or with alerting disabled")
	}

	ex.positions = []map[string]interface{}{lossAlertPosition("BTCUSDT|long|cross|1", -90)}
	ti.checkPositionLossAlerts("exchange:a", 50)
	positionLossAlerts.Lock()
	positionLossAlerts.entries["exchange:a"]["BTCUSDT|long|cross|1"].lastSeen = time.Now().Add(-positionLossAlertStaleAfter - time.Second)
	positionLossAlerts.Unlock()
	ti.checkPositionLossAlerts("exchange:a", 50)
	if len(captured.Alerts) != 2 {
		t.Fatalf("stale state (unobserved while stopped) should not suppress: %d", len(captured.Alerts))
	}
}

func TestPositionLossAlertRearmLevelForSmallThresholds(t *testing.T) {
	if got := positionLossAlertRearmPct(50); got != -40 {
		t.Fatalf("rearm for 50 = %v", got)
	}
	if got := positionLossAlertRearmPct(8); got != -4 {
		t.Fatalf("rearm for 8 = %v", got)
	}
}
