package copytrade

// 第三批回归测试：M8 保护告警去重键小时分桶、M18 最小跟单金额统一兜底。

import (
	"testing"
	"time"
)

// M8：同 cycle 同 kind 的告警在不同小时桶必须得到不同 DedupKey（复发故障
// 可再次告警），同一小时内保持相同（真正的重复仍被去重）；RateKey 不带
// 时间桶，MinInterval 限流语义不变。
func TestProtectionNotifyKeysBucketByHour(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 5, 0, 0, time.UTC)

	rate1, dedup1 := protectionNotifyKeys("trader-1", 42, "missing_escalation", base)
	rate2, dedup2 := protectionNotifyKeys("trader-1", 42, "missing_escalation", base.Add(20*time.Minute))
	if rate1 != rate2 {
		t.Fatalf("rate key must be stable across time: %q vs %q", rate1, rate2)
	}
	if dedup1 != dedup2 {
		t.Fatalf("same hour bucket must dedupe: %q vs %q", dedup1, dedup2)
	}

	_, dedup3 := protectionNotifyKeys("trader-1", 42, "missing_escalation", base.Add(2*time.Hour))
	if dedup3 == dedup1 {
		t.Fatal("a recurring fault in a later hour must produce a fresh dedup key")
	}

	_, dedupOtherKind := protectionNotifyKeys("trader-1", 42, "recovered", base)
	if dedupOtherKind == dedup1 {
		t.Fatal("different kinds must not share dedup keys")
	}
}

// M18：MinTradeWarn 未配置（<=0）时所有路径统一兜底 12 USDT；已配置时用配置值。
func TestMinTradeNotionalOrDefaultUnifiesFallback(t *testing.T) {
	if got := minTradeNotionalOrDefault(0); got != DefaultMinTradeNotional {
		t.Fatalf("unset threshold must fall back to %v, got %v", DefaultMinTradeNotional, got)
	}
	if got := minTradeNotionalOrDefault(-1); got != DefaultMinTradeNotional {
		t.Fatalf("negative threshold must fall back to %v, got %v", DefaultMinTradeNotional, got)
	}
	if got := minTradeNotionalOrDefault(25); got != 25 {
		t.Fatalf("configured threshold must win, got %v", got)
	}
	if DefaultMinTradeNotional != 12.0 {
		t.Fatalf("fallback must stay aligned with store.NewCopyGuardDefaults (12), got %v", DefaultMinTradeNotional)
	}
}
