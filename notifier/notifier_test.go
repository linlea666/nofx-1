package notifier

import (
	"os"
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
		"NOTIFY_TO", "NOTIFY_MIN_INTERVAL", "NOTIFY_QUEUE_SIZE", "NOTIFY_SEND_ON_STARTUP"}
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
