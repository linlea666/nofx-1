package copytrade

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nofx/notifier"
	"nofx/store"
)

type blockingIncidentNotifier struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	alerts  []notifier.Alert
}

func (n *blockingIncidentNotifier) Notify(alert notifier.Alert) {
	select {
	case n.entered <- struct{}{}:
	default:
	}
	<-n.release
	n.mu.Lock()
	n.alerts = append(n.alerts, alert)
	n.mu.Unlock()
	if alert.StatusHook != nil {
		alert.StatusHook(notifier.DeliveryQueued, nil)
	}
}

func (n *blockingIncidentNotifier) Shutdown() {}

func seedIncidentTrader(t *testing.T, st *store.Store, id, sourceMode string, enabled bool) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance) VALUES(?,?,?,?,100)`, id, id, "model", "exchange"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO copy_trade_configs(trader_id,provider_type,leader_id,enabled,binance_source_mode) VALUES(?,?,?,?,?)`, id, "binance", "leader-"+id, enabled, sourceMode); err != nil {
		t.Fatal(err)
	}
}

func TestSourceIncidentNotificationDoesNotBlockSignalPath(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "incident-notification.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedIncidentTrader(t, st, "smart-a", "smart_money", true)
	seedIncidentTrader(t, st, "smart-b", "smart_money", true)
	seedIncidentTrader(t, st, "regular", "copy_management", true)
	if err := st.User().Create(&store.User{ID: "user", Email: "user@example.com", PasswordHash: "hash", OTPVerified: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Equity().Save(&store.EquitySnapshot{TraderID: "smart-a", TotalEquity: 100}); err != nil {
		t.Fatal(err)
	}

	blocking := &blockingIncidentNotifier{entered: make(chan struct{}, 1), release: make(chan struct{})}
	restoreNotifier := notifier.SetGlobalForTesting(blocking, true)
	t.Cleanup(restoreNotifier)

	engine := &Engine{
		traderID: "smart-a",
		store:    st,
		config: &CopyConfig{
			ProviderType:      ProviderBinance,
			BinanceSourceMode: BinanceSourceSmartMoney,
			LeaderID:          "leader-smart-a",
		},
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	health := &store.CopyTradeSourceHealth{TraderID: "smart-a", LeaderID: "leader-smart-a", SourceGeneration: 1, LastError: "HTTP 429"}
	done := make(chan struct{})
	go func() {
		engine.recordSmartMoneySourceIncident(SourceHealthObservation{DirectRateLimit: true, CheckedAt: now}, health)
		close(done)
	}()

	select {
	case <-done:
		// The durable claim was queued without waiting for member lookup or SMTP.
	case <-time.After(time.Second):
		t.Fatal("source incident notification blocked signal processing")
	}
	select {
	case <-blocking.entered:
	case <-time.After(time.Second):
		t.Fatal("notification worker did not receive the durable incident")
	}
	databaseDone := make(chan error, 1)
	go func() {
		if _, err := st.User().GetByEmail("user@example.com"); err != nil {
			databaseDone <- err
			return
		}
		_, err := st.Equity().GetLatest("smart-a", 5)
		databaseDone <- err
	}()
	select {
	case err := <-databaseDone:
		if err != nil {
			t.Fatalf("login/history database path failed while notifier was blocked: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked notifier starved login/history database access")
	}
	close(blocking.release)

	deadline := time.Now().Add(time.Second)
	for {
		blocking.mu.Lock()
		if len(blocking.alerts) == 1 {
			alert := blocking.alerts[0]
			blocking.mu.Unlock()
			if !strings.Contains(alert.Body, "smart-a") || !strings.Contains(alert.Body, "smart-b") || strings.Contains(alert.Body, "regular") {
				t.Fatalf("credential incident members were not resolved by lightweight source query: %s", alert.Body)
			}
			break
		}
		blocking.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("notification was not handed to notifier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(time.Second)
	for {
		var status string
		if err := st.DB().QueryRow(`SELECT frozen_mail_status FROM copy_trade_source_incidents ORDER BY id DESC LIMIT 1`).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "QUEUED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notification status hook did not complete: %s", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSourceIncidentNotificationQueueFullRestoresRetryableStatus(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "incident-queue-full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedIncidentTrader(t, st, "smart", "smart_money", true)

	now := time.Now().UTC().Truncate(time.Millisecond)
	incident, action, err := st.CopyTrade().RecordSourceIncidentObservation(store.SourceIncidentObservation{
		ScopeKey: smartMoneyCredentialIncidentScope, ScopeKind: store.SourceIncidentScopeCredential,
		TraderID: "smart", LeaderID: "leader", Cause: "HTTP_429", Error: "limited", Failed: true, ObservedAt: now,
	})
	if err != nil || action != store.SourceIncidentMailFrozen {
		t.Fatalf("claim incident: action=%q err=%v", action, err)
	}

	full := &sourceIncidentNotificationDispatcher{
		jobs: make(chan sourceIncidentNotificationJob, 1), failures: make(chan sourceIncidentNotificationFailure, 1),
	}
	go full.runFailures()
	full.jobs <- sourceIncidentNotificationJob{}
	previous := smartMoneyIncidentNotificationDispatcher
	smartMoneyIncidentNotificationDispatcher = full
	t.Cleanup(func() { smartMoneyIncidentNotificationDispatcher = previous })

	engine := &Engine{traderID: "smart", store: st}
	engine.enqueueSmartMoneySourceIncidentNotification(incident, action, &store.CopyTradeSourceHealth{LeaderID: "leader"}, now)

	deadline := time.Now().Add(time.Second)
	for {
		var status string
		var nextAttempt interface{}
		if err := st.DB().QueryRow(`SELECT frozen_mail_status,frozen_mail_next_attempt_at FROM copy_trade_source_incidents WHERE id=?`, incident.ID).Scan(&status, &nextAttempt); err != nil {
			t.Fatal(err)
		}
		if status == "FAILED" && nextAttempt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue-full claim was not restored as retryable: status=%s next=%v", status, nextAttempt)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
