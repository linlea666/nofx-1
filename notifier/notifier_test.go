package notifier

import (
	"testing"
	"time"
)

func TestEmailNotifierDedupKeySuppressesRepeatedAlerts(t *testing.T) {
	n := &emailNotifier{
		cfg:    Config{MinInterval: 0, QueueSize: 10},
		queue:  make(chan Alert, 10),
		stopCh: make(chan struct{}),
	}

	n.Notify(Alert{Category: "copy_trade", Title: "first", DedupKey: "same-failure"})
	n.Notify(Alert{Category: "copy_trade", Title: "repeat", DedupKey: "same-failure"})

	if got := len(n.queue); got != 1 {
		t.Fatalf("queue len=%d want 1", got)
	}
}

func TestEmailNotifierDedupKeyAllowsDifferentAlerts(t *testing.T) {
	n := &emailNotifier{
		cfg:    Config{MinInterval: 0, QueueSize: 10},
		queue:  make(chan Alert, 10),
		stopCh: make(chan struct{}),
	}

	n.Notify(Alert{Category: "copy_trade", Title: "first", DedupKey: "failure-1"})
	n.Notify(Alert{Category: "copy_trade", Title: "second", DedupKey: "failure-2"})

	if got := len(n.queue); got != 2 {
		t.Fatalf("queue len=%d want 2", got)
	}
}

func TestEmailNotifierDedupKeyRunsBeforeRateLimit(t *testing.T) {
	n := &emailNotifier{
		cfg:    Config{MinInterval: time.Hour, QueueSize: 10},
		queue:  make(chan Alert, 10),
		stopCh: make(chan struct{}),
	}

	n.Notify(Alert{Category: "copy_trade", TraderID: "t", Title: "same title", DedupKey: "failure-1"})
	n.Notify(Alert{Category: "copy_trade", TraderID: "t", Title: "same title", DedupKey: "failure-2"})

	if got := len(n.queue); got != 1 {
		t.Fatalf("queue len=%d want 1 because rate limit still applies after dedupe", got)
	}

	n.lastSent.Delete("copy_trade|t|same title")
	n.Notify(Alert{Category: "copy_trade", TraderID: "t", Title: "same title", DedupKey: "failure-2"})
	if got := len(n.queue); got != 2 {
		t.Fatalf("queue len=%d want 2 after rate limit key is cleared", got)
	}
}

func TestEmailNotifierDedupKeyReleasedWhenQueueIsFull(t *testing.T) {
	n := &emailNotifier{
		cfg:    Config{MinInterval: 0, QueueSize: 1},
		queue:  make(chan Alert, 1),
		stopCh: make(chan struct{}),
	}

	n.Notify(Alert{Category: "copy_trade", Title: "fills queue"})
	n.Notify(Alert{Category: "copy_trade", Title: "dropped", DedupKey: "retryable"})
	<-n.queue

	n.Notify(Alert{Category: "copy_trade", Title: "retry", DedupKey: "retryable"})
	if got := len(n.queue); got != 1 {
		t.Fatalf("queue len=%d want 1 after retrying dropped dedup alert", got)
	}
}
