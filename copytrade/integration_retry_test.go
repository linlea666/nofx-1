package copytrade

import (
	"context"
	"errors"
	"testing"
	"time"

	"nofx/decision"
)

// retryScriptExecutor 按脚本返回错误的 mock 执行器：
// 第 i 次 ExecuteDecision 返回 errs[i]，超出脚本后返回 nil（成功）。
type retryScriptExecutor struct {
	calls int
	errs  []error
}

func (e *retryScriptExecutor) ExecuteDecision(*decision.Decision) error {
	i := e.calls
	e.calls++
	if i < len(e.errs) {
		return e.errs[i]
	}
	return nil
}
func (e *retryScriptExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
func (e *retryScriptExecutor) GetPositions() ([]map[string]interface{}, error) { return nil, nil }

func newRetryTestIntegration(ex DecisionExecutor) *TraderIntegration {
	ctx, cancel := context.WithCancel(context.Background())
	return &TraderIntegration{
		traderID: "retry-test-trader",
		executor: ex,
		ctx:      ctx,
		cancel:   cancel,
	}
}

func shortRetryBackoffs(t *testing.T) {
	t.Helper()
	old := executeDecisionRetryBackoffs
	executeDecisionRetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { executeDecisionRetryBackoffs = old })
}

// 可重试判定：仅"保证订单未进交易所"的错误可重试。
func TestIsRetryableExecutionError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// 可重试：OKX 限流（网关拒绝，请求未处理）—— cycle 71 后 06:40 事故的真实错误串
		{"okx 50011 wrapped in positions read", errors.New("failed to get positions: failed to get positions: OKX API error: code=50011, msg=Too Many Requests"), true},
		{"okx 50011 on order placement", errors.New("failed to place order: OKX API error: code=50011, msg=Too Many Requests"), true},
		{"generic too many requests", errors.New("HTTP 429 Too Many Requests"), true},
		// 可重试：下单前读取阶段失败（auto_trader 错误包装前缀）
		{"positions read timeout", errors.New("failed to get positions: context deadline exceeded"), true},
		{"balance read failure", errors.New("failed to get account balance: connection reset"), true},
		{"market price read failure", errors.New("failed to get market price: timeout"), true},
		// 不可重试：业务性拒单
		{"duplicate position", errors.New("❌ ETHUSDT already has long position, close it first"), false},
		{"insufficient margin", errors.New("failed to open long position: Insufficient USDT margin in account"), false},
		{"position not found on close", errors.New("position not found"), false},
		// 不可重试：下单阶段歧义错误（订单可能已进交易所）
		{"order placement timeout", errors.New("failed to open long position: context deadline exceeded"), false},
		{"nil error", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableExecutionError(tc.err); got != tc.want {
				t.Fatalf("isRetryableExecutionError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// 瞬态失败（50011）两次后恢复：第三次成功，决策不丢。
func TestExecuteDecisionWithRetryRecoversFromTransient(t *testing.T) {
	shortRetryBackoffs(t)
	rateLimited := errors.New("failed to get positions: OKX API error: code=50011, msg=Too Many Requests")
	ex := &retryScriptExecutor{errs: []error{rateLimited, rateLimited}}
	ti := newRetryTestIntegration(ex)

	if err := ti.executeDecisionWithRetry(&decision.Decision{Action: "open_long", Symbol: "ETHUSDT"}); err != nil {
		t.Fatalf("expected recovery after transient failures, got %v", err)
	}
	if ex.calls != 3 {
		t.Fatalf("expected 3 attempts (1 initial + 2 retries), got %d", ex.calls)
	}
}

// 不可重试错误必须立即失败，不消耗重试预算。
func TestExecuteDecisionWithRetryStopsOnNonRetryable(t *testing.T) {
	shortRetryBackoffs(t)
	fatal := errors.New("❌ ETHUSDT already has long position, close it first")
	ex := &retryScriptExecutor{errs: []error{fatal, fatal, fatal, fatal}}
	ti := newRetryTestIntegration(ex)

	if err := ti.executeDecisionWithRetry(&decision.Decision{Action: "open_long", Symbol: "ETHUSDT"}); err == nil {
		t.Fatal("expected non-retryable error to surface")
	}
	if ex.calls != 1 {
		t.Fatalf("non-retryable error must not be retried, got %d attempts", ex.calls)
	}
}

// 重试耗尽后返回最后一次错误，走原失败分支（调用次数 = 1 + len(backoffs)）。
func TestExecuteDecisionWithRetryExhausted(t *testing.T) {
	shortRetryBackoffs(t)
	rateLimited := errors.New("failed to get positions: OKX API error: code=50011, msg=Too Many Requests")
	ex := &retryScriptExecutor{errs: []error{rateLimited, rateLimited, rateLimited, rateLimited, rateLimited}}
	ti := newRetryTestIntegration(ex)

	err := ti.executeDecisionWithRetry(&decision.Decision{Action: "open_long", Symbol: "ETHUSDT"})
	if err == nil {
		t.Fatal("expected error after retry exhaustion")
	}
	wantCalls := 1 + len(executeDecisionRetryBackoffs)
	if ex.calls != wantCalls {
		t.Fatalf("expected %d attempts, got %d", wantCalls, ex.calls)
	}
}

// ctx 取消后不再继续重试（引擎停止时快速退出）。
func TestExecuteDecisionWithRetryHonorsContextCancel(t *testing.T) {
	old := executeDecisionRetryBackoffs
	executeDecisionRetryBackoffs = []time.Duration{time.Hour}
	t.Cleanup(func() { executeDecisionRetryBackoffs = old })

	rateLimited := errors.New("failed to get positions: OKX API error: code=50011, msg=Too Many Requests")
	ex := &retryScriptExecutor{errs: []error{rateLimited, rateLimited}}
	ti := newRetryTestIntegration(ex)
	ti.cancel()

	done := make(chan error, 1)
	go func() { done <- ti.executeDecisionWithRetry(&decision.Decision{Action: "open_long", Symbol: "ETHUSDT"}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error when context canceled during retry")
		}
		if ex.calls != 1 {
			t.Fatalf("canceled context must not re-execute, got %d attempts", ex.calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executeDecisionWithRetry did not return after context cancel")
	}
}
