package trader

import (
	"errors"
	"testing"
)

// TestIsInsufficientMarginErrorRecognizesAllExchangeFormats 覆盖各交易所返回的
// "保证金/可用余额不足"错误形态（关键字 + 错误码）。
// 防止误判 close 类良性错误（"position not found" 等）。
func TestIsInsufficientMarginErrorRecognizesAllExchangeFormats(t *testing.T) {
	cases := []struct {
		name   string
		errMsg string
		want   bool
	}{
		// OKX
		{"okx insufficient USDT margin",
			"Order failed. Insufficient USDT margin in account", true},
		{"okx code 51008",
			"OKX order error: code=51008, msg=Order failed. Insufficient margin", true},
		{"okx code 51131",
			"code=51131, msg=insufficient position margin", true},

		// Binance
		{"binance insufficient margin",
			"APIError(code=-2010): Account has insufficient balance for requested action", true},
		{"binance margin insufficient",
			"Margin is insufficient", true},
		{"binance code -2019",
			"APIError(code=-2019): Margin is insufficient.", true},

		// Bybit
		{"bybit insufficient balance",
			"insufficient balance for the order", true},
		{"bybit retcode 110007",
			"retcode=110007, retmsg=ab not enough for new order", true},
		{"bybit retcode 110045",
			"retcode=110045, retmsg=Margin not enough for the order", true},

		// HL / 通用
		{"hl insufficient margin available",
			"tx would have insufficient margin available", true},
		{"hl negative balance",
			"tx would lead to negative balance for the user", true},

		// Bitget
		{"bitget account margin not enough",
			"account margin not enough", true},

		// 大小写不敏感
		{"upper case",
			"INSUFFICIENT MARGIN", true},

		// === 不应触发减半重试的错误 ===
		{"position not found (close 良性错误)",
			"long position not found for ETHUSDT", false},
		{"size too small",
			"position size too small, rounded to 0", false},
		{"network error",
			"context deadline exceeded", false},
		{"rate limit",
			"rate limit exceeded", false},
		{"empty",
			"", false},
		{"insufficient 但和保证金无关",
			"insufficient data points for analysis", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			if tc.errMsg == "" {
				got = isInsufficientMarginError(nil)
			} else {
				got = isInsufficientMarginError(errors.New(tc.errMsg))
			}
			if got != tc.want {
				t.Fatalf("err=%q got=%v want=%v", tc.errMsg, got, tc.want)
			}
		})
	}
}

// TestIsInsufficientMarginErrorHandlesNil 边界：nil 应返回 false。
func TestIsInsufficientMarginErrorHandlesNil(t *testing.T) {
	if isInsufficientMarginError(nil) {
		t.Fatalf("nil error should return false")
	}
}
