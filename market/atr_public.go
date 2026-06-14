package market

import (
	"fmt"
	"sync"
	"time"
)

// ============================================================================
// ATR 公开接口（供 copytrade 等外部模块复用）
//
// 设计目的：market 包内的 calculateATR 是私有函数，AI 决策模式内部使用。
// 跟单模块的"账户保护止损"也需要 ATR 做噪音防护下界，但不应重复实现。
// 这里提取一个带缓存的公开接口，复用核心算法。
//
// 缓存策略：同 symbol+timeframe 30 秒内返回缓存结果，避免高频跟单频繁请求 K 线。
// 数据源：复用现有 APIClient.GetKlines（币安 K 线接口，公开 API 无凭证依赖）。
// 失败降级：拿不到 K 线时返回 (0, error)，由调用方决定回退策略。
// ============================================================================

type atrCacheEntry struct {
	value float64
	ts    time.Time
}

var (
	atrCache   = make(map[string]atrCacheEntry)
	atrCacheMu sync.RWMutex

	// ATR 缓存 TTL：30s 足以覆盖跟单 3s 轮询周期内的多次开仓/加仓决策
	atrCacheTTL = 30 * time.Second

	// 共享 APIClient 实例（NewAPIClient 内部会通过 hook 接入用户自定义 HTTP 客户端）
	sharedAPIClient     *APIClient
	sharedAPIClientOnce sync.Once
)

// getSharedAPIClient 懒加载共享 APIClient
func getSharedAPIClient() *APIClient {
	sharedAPIClientOnce.Do(func() {
		sharedAPIClient = NewAPIClient()
	})
	return sharedAPIClient
}

// GetATR 取指定 symbol + 时间周期的 ATR(period) 值
//
// 参数：
//   - symbol: 标准化交易对（如 "BTCUSDT"，会原样传给币安 K 线 API）
//   - timeframe: K 线周期（"15m" / "1h" / "4h" 等，币安原生格式）
//   - period: ATR 周期（默认 14；最少需要 period+1 根 K 线）
//
// 返回：
//   - atr > 0: 成功，单位为价格绝对值（如 BTC 可能返回几百 USD）
//   - atr = 0, err != nil: 失败（K 线接口异常 / 数据不足）
//
// 缓存：同 (symbol, timeframe, period) 30s 内复用上次结果。
//
// 用法（跟单 SL 兜底）：
//
//	atr, err := market.GetATR("BTCUSDT", "1h", 14)
//	if err == nil && atr > 0 {
//	    slDistance = max(accountRiskDistance, 1.5 * atr) // 双控
//	}
func GetATR(symbol, timeframe string, period int) (float64, error) {
	if symbol == "" {
		return 0, fmt.Errorf("symbol is empty")
	}
	if period <= 0 {
		period = 14
	}
	if timeframe == "" {
		timeframe = "1h"
	}

	cacheKey := fmt.Sprintf("%s|%s|%d", symbol, timeframe, period)

	// 命中缓存
	atrCacheMu.RLock()
	if entry, ok := atrCache[cacheKey]; ok {
		if time.Since(entry.ts) < atrCacheTTL {
			atrCacheMu.RUnlock()
			return entry.value, nil
		}
	}
	atrCacheMu.RUnlock()

	// 拉 K 线：拉 period × 3 根（保留余量给 Wilder smoothing 收敛）
	limit := period * 3
	if limit < 50 {
		limit = 50
	}

	klines, err := getSharedAPIClient().GetKlines(symbol, timeframe, limit)
	if err != nil {
		return 0, fmt.Errorf("get klines failed: %w", err)
	}
	if len(klines) <= period {
		return 0, fmt.Errorf("insufficient klines: got %d, need >%d", len(klines), period)
	}

	atr := calculateATR(klines, period)
	if atr <= 0 {
		return 0, fmt.Errorf("calculated ATR is zero or negative")
	}

	// 写入缓存
	atrCacheMu.Lock()
	atrCache[cacheKey] = atrCacheEntry{value: atr, ts: time.Now()}
	atrCacheMu.Unlock()

	return atr, nil
}
