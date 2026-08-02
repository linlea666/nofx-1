package notifier

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type failingMailSender struct{ err error }

func (s failingMailSender) Send(context.Context, []string, string, string) error { return s.err }

type blockingContextMailSender struct{ entered chan struct{} }

func (s blockingContextMailSender) Send(ctx context.Context, _ []string, _, _ string) error {
	close(s.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestBuildBodyIncludesTraderNameAndStableID(t *testing.T) {
	body := buildBody(Alert{Category: "copy_trade", TraderID: "trader-id", TraderName: "主账户-A", Title: "AI 决策", Time: time.Now()})
	for _, want := range []string{"账户名称 (Trader Name): 主账户-A", "账号 (TraderID): trader-id"} {
		if !strings.Contains(body, want) {
			t.Fatalf("email body missing %q: %s", want, body)
		}
	}
}

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

// TestLoadFromEnvParsesBinanceCopyActionEnabled 验证 PR-Notify-1：
// NOTIFY_BINANCE_COPY_ACTION_ENABLED env 默认 false，明确写 true/false/1/0 时正确解析。
func TestLoadFromEnvParsesBinanceCopyActionEnabled(t *testing.T) {
	cases := []struct {
		env  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"on", true},
		{"false", false},
		{"0", false},
		{"no", false},
		{"off", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("NOTIFY_BINANCE_COPY_ACTION_ENABLED", tc.env)
			cfg := LoadFromEnv()
			if cfg.NotifyBinanceCopyActionEnabled != tc.want {
				t.Fatalf("env=%q got=%v want=%v", tc.env, cfg.NotifyBinanceCopyActionEnabled, tc.want)
			}
		})
	}
}

// TestCopyTradeActionEnabledReflectsGlobalConfig 验证 CopyTradeActionEnabled() 全局
// getter 正确反映 Init() 后的 globalCfg.NotifyBinanceCopyActionEnabled。
//
// 注意：测试结束后必须还原全局状态，否则会污染同 process 内其他测试。
func TestCopyTradeActionEnabledReflectsGlobalConfig(t *testing.T) {
	prevGlobal := global
	prevCfg := globalCfg
	prevInited := globalInited
	t.Cleanup(func() {
		globalMu.Lock()
		global = prevGlobal
		globalCfg = prevCfg
		globalInited = prevInited
		globalMu.Unlock()
	})

	// 未初始化：永远 false
	globalMu.Lock()
	globalInited = false
	globalCfg = Config{NotifyBinanceCopyActionEnabled: true}
	globalMu.Unlock()
	if CopyTradeActionEnabled() {
		t.Fatalf("globalInited=false 时应永远 false，无论 cfg 字段值如何")
	}

	// 初始化 + 关闭：false
	globalMu.Lock()
	globalInited = true
	globalCfg = Config{NotifyBinanceCopyActionEnabled: false}
	globalMu.Unlock()
	if CopyTradeActionEnabled() {
		t.Fatalf("env 关闭时应返回 false")
	}

	// 初始化 + 启用：true
	globalMu.Lock()
	globalInited = true
	globalCfg = Config{NotifyBinanceCopyActionEnabled: true}
	globalMu.Unlock()
	if !CopyTradeActionEnabled() {
		t.Fatalf("env 启用时应返回 true")
	}
}

// TestLoadFromEnvKeepsExistingDefaultsWhenCopyActionEnvIsSet 确保新增 env 不影响其他默认值。
func TestLoadFromEnvKeepsExistingDefaultsWhenCopyActionEnvIsSet(t *testing.T) {
	// 清空其他 env 避免本机环境干扰
	keys := []string{"NOTIFY_EMAIL_ENABLED", "SMTP_HOST", "SMTP_USER", "SMTP_PASS",
		"NOTIFY_TO", "NOTIFY_MIN_INTERVAL", "NOTIFY_QUEUE_SIZE", "NOTIFY_SEND_TIMEOUT_SECONDS", "NOTIFY_SEND_ON_STARTUP"}
	for _, k := range keys {
		t.Setenv(k, "")
		_ = os.Unsetenv(k) // 防止 Setenv 设为空串而不是 unset
	}
	t.Setenv("NOTIFY_BINANCE_COPY_ACTION_ENABLED", "true")

	cfg := LoadFromEnv()
	if !cfg.NotifyBinanceCopyActionEnabled {
		t.Fatalf("expected NotifyBinanceCopyActionEnabled=true")
	}
	if cfg.SMTPPort != 465 {
		t.Fatalf("默认 SMTPPort 应为 465，实际 %d", cfg.SMTPPort)
	}
	if cfg.MinInterval != 60*time.Second {
		t.Fatalf("默认 MinInterval 应为 60s，实际 %v", cfg.MinInterval)
	}
	if cfg.QueueSize != 100 {
		t.Fatalf("默认 QueueSize 应为 100，实际 %d", cfg.QueueSize)
	}
	if cfg.SendTimeout != 15*time.Second {
		t.Fatalf("默认 SendTimeout 应为 15s，实际 %v", cfg.SendTimeout)
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

func TestEmailNotifierSMTPFailureReleasesDedupAndRateReservations(t *testing.T) {
	n := &emailNotifier{
		cfg: Config{MinInterval: time.Hour, QueueSize: 2}, client: failingMailSender{err: errors.New("smtp 554")},
		queue: make(chan Alert, 2), stopCh: make(chan struct{}),
	}
	alert := Alert{Category: "copy_trade", TraderID: "t", Title: "summary", RateKey: "summary-rate", DedupKey: "summary-dedup"}
	n.Notify(alert)
	queued := <-n.queue
	n.send(queued)
	if _, exists := n.deduped.Load(alert.DedupKey); exists {
		t.Fatal("SMTP failure must release the dedup reservation")
	}
	if _, exists := n.lastSent.Load(alert.RateKey); exists {
		t.Fatal("SMTP failure must release the rate reservation")
	}
	n.Notify(alert)
	if got := len(n.queue); got != 1 {
		t.Fatalf("retry after SMTP failure was not queued: len=%d", got)
	}
}

func TestDeliveryStatusHookReportsQueuedAndDeduped(t *testing.T) {
	n := &emailNotifier{cfg: Config{MinInterval: 0, QueueSize: 2}, queue: make(chan Alert, 2), stopCh: make(chan struct{})}
	statuses := []DeliveryStatus{}
	alert := Alert{Category: "copy_trade", Title: "AI protected", DedupKey: "REENTRY_FILLED|t|7|2|1", StatusHook: func(status DeliveryStatus, _ error) {
		statuses = append(statuses, status)
	}}
	n.Notify(alert)
	n.Notify(alert)
	if len(statuses) != 2 || statuses[0] != DeliveryQueued || statuses[1] != DeliveryDeduped {
		t.Fatalf("unexpected delivery audit sequence: %v", statuses)
	}
}

func TestNoopNotifierReportsDisabled(t *testing.T) {
	status := DeliveryStatus("")
	noopNotifier{}.Notify(Alert{StatusHook: func(got DeliveryStatus, _ error) { status = got }})
	if status != DeliveryDisabled {
		t.Fatalf("noop delivery status=%s want disabled", status)
	}
}

func TestEmailNotifierShutdownCancelsInFlightSMTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := &emailNotifier{
		cfg:    Config{MinInterval: 0, QueueSize: 1, SendTimeout: time.Hour},
		client: blockingContextMailSender{entered: make(chan struct{})},
		queue:  make(chan Alert, 1), stopCh: make(chan struct{}), ctx: ctx, cancel: cancel,
	}
	n.wg.Add(1)
	go n.worker()
	n.Notify(Alert{Category: "system", Title: "blocking smtp"})
	select {
	case <-n.client.(blockingContextMailSender).entered:
	case <-time.After(time.Second):
		t.Fatal("SMTP sender did not start")
	}
	done := make(chan struct{})
	go func() {
		n.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel in-flight SMTP")
	}
}
