package trader

import (
	"fmt"
	"strconv"
	"strings"
)

// isInsufficientMarginError 识别交易所返回的"保证金/可用余额不足"类错误。
//
// 用于跟单开仓失败时判断是否应当"减半重试"（PR-4 / 修复 E'）。仅对跟单路径生效，
// 避免影响 AI 决策的固有逻辑（AI 决策本就有自己的余额预扣机制）。
//
// 各家关键字归纳（统一小写匹配）：
//   - OKX     : "insufficient USDT margin"（okx_trader 业务包装）
//                "code=51008"（保证金不足）
//                "code=51131"（仓位保证金不足）
//   - Binance : "insufficient margin" / "margin is insufficient"
//                "code=-2010"（NEW_ORDER_REJECTED，常见保证金不足）
//                "code=-2019"（MARGIN_INSUFFICIENT）
//   - Bybit   : "insufficient balance" / "ab not enough"
//                "retcode=110007" / "retcode=110045"
//   - Aster   : 走 Binance 风格 fapi，复用 -2019
//   - HL      : "insufficient margin available" / "tx would lead to negative balance"
//   - Bitget  : "insufficient margin balance" / "account margin not enough"
//
// 注意：避免误判"position not found / size too small"等其他错误（这些是良性关闭，
// 由 isBenignCloseError 在 copytrade 层处理）。
func isInsufficientMarginError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())

	// 1) 直接匹配"insufficient"组合
	if strings.Contains(msg, "insufficient") {
		if strings.Contains(msg, "margin") ||
			strings.Contains(msg, "balance") ||
			strings.Contains(msg, "funds") ||
			strings.Contains(msg, "asset") ||
			strings.Contains(msg, "usdt") {
			return true
		}
	}

	// 2) Binance 风格："margin is insufficient"
	if strings.Contains(msg, "margin is insufficient") {
		return true
	}

	// 3) HL / 通用："negative balance" / "not enough margin"
	if strings.Contains(msg, "negative balance") ||
		strings.Contains(msg, "not enough margin") ||
		strings.Contains(msg, "margin not enough") ||
		strings.Contains(msg, "account margin not enough") {
		return true
	}

	// 4) 各家错误码精确匹配
	codeKeywords := []string{
		"code=51008",    // OKX 保证金不足
		"code=51131",    // OKX 仓位保证金不足
		"code=-2010",    // Binance / Aster NEW_ORDER_REJECTED
		"code=-2019",    // Binance / Aster MARGIN_INSUFFICIENT
		"retcode=110007", // Bybit insufficient balance
		"retcode=110045", // Bybit margin not enough
	}
	for _, kw := range codeKeywords {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// SafeFloat64 Safely extract float64 value from map
func SafeFloat64(data map[string]interface{}, key string) (float64, error) {
	value, ok := data[key]
	if !ok {
		return 0, fmt.Errorf("key '%s' not found", key)
	}

	switch v := value.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case string:
		// Try to parse string as float64
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot parse string '%s' as float64: %w", v, err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("value for key '%s' is not a number (type: %T)", key, v)
	}
}

// SafeString Safely extract string value from map
func SafeString(data map[string]interface{}, key string) (string, error) {
	value, ok := data[key]
	if !ok {
		return "", fmt.Errorf("key '%s' not found", key)
	}

	switch v := value.(type) {
	case string:
		return v, nil
	case fmt.Stringer:
		return v.String(), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// SafeInt Safely extract int value from map
func SafeInt(data map[string]interface{}, key string) (int, error) {
	value, ok := data[key]
	if !ok {
		return 0, fmt.Errorf("key '%s' not found", key)
	}

	switch v := value.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case string:
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("cannot parse string '%s' as int: %w", v, err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("value for key '%s' is not an integer (type: %T)", key, v)
	}
}
