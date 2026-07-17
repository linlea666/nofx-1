package market

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
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

// GetOKXATR calculates ATR from completed OKX mark-price candles. The open candle is deliberately
// excluded so stop placement is deterministic across repeated calls in the same interval.
func GetOKXATR(symbol, timeframe string, period int) (float64, error) {
	return getOKXATRFresh(symbol, timeframe, period)
}

// GetOKXATRWithMaxAge falls back to the last successful snapshot when the live
// request fails. Copy Guard uses a two-hour max age; callers outside Copy Guard
// retain the strict fresh behavior through GetOKXATR.
func GetOKXATRWithMaxAge(symbol, timeframe string, period int, maxAge time.Duration) (float64, error) {
	value, err := getOKXATRFresh(symbol, timeframe, period)
	if err == nil || maxAge <= 0 {
		return value, err
	}
	bar := map[string]string{"15m": "15m", "1h": "1H", "4h": "4H"}[strings.ToLower(timeframe)]
	if bar == "" {
		return 0, err
	}
	key := fmt.Sprintf("okx|%s|%s|%d", symbol, bar, period)
	atrCacheMu.RLock()
	cached, ok := atrCache[key]
	atrCacheMu.RUnlock()
	if ok && time.Since(cached.ts) <= maxAge && cached.value > 0 {
		return cached.value, nil
	}
	return 0, err
}

func getOKXATRFresh(symbol, timeframe string, period int) (float64, error) {
	if symbol == "" {
		return 0, fmt.Errorf("symbol is empty")
	}
	if period <= 0 {
		period = 14
	}
	if timeframe == "" {
		timeframe = "1h"
	}
	// ATR policy intentionally accepts only the configured risk timeframes.
	// Five-minute candles are exposed separately for structure detection, but
	// allowing them here would silently turn legacy invalid-timeframe fallback
	// into a much tighter stop.
	bar := map[string]string{"15m": "15m", "1h": "1H", "4h": "4H"}[strings.ToLower(timeframe)]
	if bar == "" {
		return 0, fmt.Errorf("unsupported OKX ATR timeframe: %s", timeframe)
	}
	key := fmt.Sprintf("okx|%s|%s|%d", symbol, bar, period)
	atrCacheMu.RLock()
	cached, ok := atrCache[key]
	atrCacheMu.RUnlock()
	if ok && time.Since(cached.ts) < atrCacheTTL {
		return cached.value, nil
	}
	limit := period * 3
	if limit < 50 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	klines, err := GetOKXCompletedMarkCandles(symbol, timeframe, limit)
	if err != nil {
		return 0, err
	}
	if len(klines) <= period {
		return 0, fmt.Errorf("insufficient completed OKX candles: %d", len(klines))
	}
	atr := calculateATR(klines, period)
	if atr <= 0 {
		return 0, fmt.Errorf("calculated OKX ATR is zero")
	}
	atrCacheMu.Lock()
	atrCache[key] = atrCacheEntry{value: atr, ts: time.Now()}
	atrCacheMu.Unlock()
	return atr, nil
}

// GetOKXCompletedMarkCandles returns only exchange-confirmed, closed mark-price
// candles. Copy Guard uses it for structural invalidation levels; callers must
// never derive a stop from the still-forming candle.
func GetOKXCompletedMarkCandles(symbol, timeframe string, limit int) ([]Kline, error) {
	return getOKXCompletedCandles(symbol, timeframe, limit, "mark-price-candles", false)
}

// GetOKXCompletedCandles returns completed execution-venue trade candles,
// including volume. Copy Guard uses these only as a review wake-up signal;
// mark-price candles remain the source for ATR and structural stop placement.
func GetOKXCompletedCandles(symbol, timeframe string, limit int) ([]Kline, error) {
	return getOKXCompletedCandles(symbol, timeframe, limit, "candles", true)
}

func getOKXCompletedCandles(symbol, timeframe string, limit int, endpoint string, includeVolume bool) ([]Kline, error) {
	bar := map[string]string{"5m": "5m", "15m": "15m", "1h": "1H", "4h": "4H"}[strings.ToLower(timeframe)]
	if bar == "" {
		return nil, fmt.Errorf("unsupported OKX candle timeframe: %s", timeframe)
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	instID := strings.TrimSuffix(strings.ToUpper(symbol), "USDT") + "-USDT-SWAP"
	req, err := http.NewRequest(http.MethodGet, "https://www.okx.com/api/v5/market/"+endpoint, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("instId", instID)
	q.Set("bar", bar)
	q.Set("limit", strconv.Itoa(limit))
	req.URL.RawQuery = q.Encode()
	resp, err := getSharedAPIClient().client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get OKX %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("OKX %s HTTP %d", endpoint, resp.StatusCode)
	}
	var envelope struct {
		Code string     `json:"code"`
		Msg  string     `json:"msg"`
		Data [][]string `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.Code != "0" {
		return nil, fmt.Errorf("OKX %s: %s", endpoint, envelope.Msg)
	}
	klines := make([]Kline, 0, len(envelope.Data))
	now := time.Now().UnixMilli()
	for _, row := range envelope.Data {
		if len(row) < 5 || (len(row) > 5 && row[len(row)-1] == "0") {
			continue
		}
		ts, _ := strconv.ParseInt(row[0], 10, 64)
		if ts <= 0 || ts >= now {
			continue
		}
		o, _ := strconv.ParseFloat(row[1], 64)
		h, _ := strconv.ParseFloat(row[2], 64)
		l, _ := strconv.ParseFloat(row[3], 64)
		c, _ := strconv.ParseFloat(row[4], 64)
		volume := float64(0)
		if includeVolume && len(row) > 5 {
			volume, _ = strconv.ParseFloat(row[5], 64)
		}
		if h <= 0 || l <= 0 || c <= 0 {
			continue
		}
		klines = append(klines, Kline{OpenTime: ts, Open: o, High: h, Low: l, Close: c, Volume: volume})
	}
	sort.Slice(klines, func(i, j int) bool { return klines[i].OpenTime < klines[j].OpenTime })
	if len(klines) == 0 {
		return nil, fmt.Errorf("no completed OKX candles")
	}
	return klines, nil
}
