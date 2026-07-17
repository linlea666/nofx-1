package copytrade

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"nofx/logger"
)

// ============================================================================
// OKX 公开 instruments 接口（无需凭证）
//
// 用途：拿 tickSz（价格档位）做 SL 价对齐，并用
// lotSz × ctVal 得到以币数表示的最小仓位步长。
//
// 为什么不复用 trader/okx_trader.go 的 getInstrument？
//   - trader 包通过 DecisionExecutor 接口被 copytrade 反向调用，
//     copytrade → trader 会形成循环依赖
//   - 在 trader.Trader 接口加 GetTickSize 会触及 8 个 trader 实现，
//     违反"范围纪律"原则
//
// 替代方案：copytrade 直接调 OKX 公开 instruments API（无需签名）
// API 文档：https://www.okx.com/api/v5/public/instruments?instType=SWAP&instId=BTC-USDT-SWAP
//
// 缓存策略：5 分钟（合约规格几乎不变，旧 OKX 实现也用 5 分钟）
// ============================================================================

const okxPublicInstrumentsAPI = "https://www.okx.com/api/v5/public/instruments"

type okxInstrumentCacheEntry struct {
	tickSz float64
	lotSz  float64
	ctVal  float64
	ts     time.Time
}

var (
	okxInstrumentCache    = make(map[string]okxInstrumentCacheEntry)
	okxInstrumentCacheMu  sync.RWMutex
	okxInstrumentCacheTTL = 5 * time.Minute

	okxInstrumentHTTPClient = &http.Client{Timeout: 5 * time.Second}
)

// okxInstrumentResp OKX 公开 instruments API 响应
type okxInstrumentResp struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		InstID string `json:"instId"`
		TickSz string `json:"tickSz"`
		LotSz  string `json:"lotSz"`
		CtVal  string `json:"ctVal"`
	} `json:"data"`
}

// getOKXTickSize 获取 OKX SWAP 合约的价格档位（tickSz）
//
// 参数：
//   - symbol: 标准化交易对（如 "BTCUSDT"，内部会转换为 "BTC-USDT-SWAP"）
//
// 返回：
//   - tickSz > 0: 价格档位（如 BTC 是 0.1）
//   - tickSz = 0, err != nil: 接口失败
//
// 用法：alignToTickSize(rawPrice, tickSz) 把任意价格对齐到档位
func getOKXTickSize(symbol string) (float64, error) {
	spec, err := getOKXInstrumentSpec(symbol)
	if err != nil {
		return 0, err
	}
	return spec.tickSz, nil
}

// getOKXBaseQuantityStep returns the smallest position quantity represented in
// base-asset units. OKX reports lotSz in contracts while Copy Guard and the
// trader adapter exchange base-asset quantities, hence lotSz * ctVal.
func getOKXBaseQuantityStep(symbol string) (float64, error) {
	spec, err := getOKXInstrumentSpec(symbol)
	if err != nil {
		return 0, err
	}
	step := spec.lotSz * spec.ctVal
	if step <= 0 {
		return 0, fmt.Errorf("invalid OKX base quantity step for %s: lotSz=%v ctVal=%v", symbol, spec.lotSz, spec.ctVal)
	}
	return step, nil
}

func getOKXInstrumentSpec(symbol string) (okxInstrumentCacheEntry, error) {
	if symbol == "" {
		return okxInstrumentCacheEntry{}, fmt.Errorf("symbol is empty")
	}

	instID := convertSymbolToOKXInstID(symbol)

	// 命中缓存
	okxInstrumentCacheMu.RLock()
	if entry, ok := okxInstrumentCache[instID]; ok {
		if time.Since(entry.ts) < okxInstrumentCacheTTL {
			okxInstrumentCacheMu.RUnlock()
			return entry, nil
		}
	}
	okxInstrumentCacheMu.RUnlock()

	url := fmt.Sprintf("%s?instType=SWAP&instId=%s", okxPublicInstrumentsAPI, instID)
	resp, err := okxInstrumentHTTPClient.Get(url)
	if err != nil {
		return okxInstrumentCacheEntry{}, fmt.Errorf("get okx instrument failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return okxInstrumentCacheEntry{}, fmt.Errorf("okx instrument HTTP %d: %s", resp.StatusCode, string(body))
	}

	var data okxInstrumentResp
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return okxInstrumentCacheEntry{}, fmt.Errorf("decode okx instrument failed: %w", err)
	}

	if data.Code != "0" {
		return okxInstrumentCacheEntry{}, fmt.Errorf("okx instrument API error: code=%s msg=%s", data.Code, data.Msg)
	}

	if len(data.Data) == 0 {
		return okxInstrumentCacheEntry{}, fmt.Errorf("okx instrument not found: %s", instID)
	}

	tickSz, err := strconv.ParseFloat(data.Data[0].TickSz, 64)
	if err != nil || tickSz <= 0 {
		return okxInstrumentCacheEntry{}, fmt.Errorf("invalid tickSz %q: %v", data.Data[0].TickSz, err)
	}
	lotSz, lotErr := strconv.ParseFloat(data.Data[0].LotSz, 64)
	ctVal, ctErr := strconv.ParseFloat(data.Data[0].CtVal, 64)
	if lotErr != nil || lotSz <= 0 || ctErr != nil || ctVal <= 0 {
		return okxInstrumentCacheEntry{}, fmt.Errorf("invalid OKX quantity spec lotSz=%q ctVal=%q", data.Data[0].LotSz, data.Data[0].CtVal)
	}
	entry := okxInstrumentCacheEntry{tickSz: tickSz, lotSz: lotSz, ctVal: ctVal, ts: time.Now()}

	// 写入缓存
	okxInstrumentCacheMu.Lock()
	okxInstrumentCache[instID] = entry
	okxInstrumentCacheMu.Unlock()

	logger.Debugf("📐 OKX instrument spec | %s tickSz=%s lotSz=%s ctVal=%s", instID, data.Data[0].TickSz, data.Data[0].LotSz, data.Data[0].CtVal)
	return entry, nil
}

// convertSymbolToOKXInstID "BTCUSDT" → "BTC-USDT-SWAP"
//
// 规则：以 USDT 结尾的统一格式 → OKX SWAP 永续合约
// 设计上跟单只跑 OKX SWAP，所以这个转换足够覆盖
func convertSymbolToOKXInstID(symbol string) string {
	if len(symbol) < 4 {
		return symbol
	}
	// 末尾是 USDT 的标准格式
	if symbol[len(symbol)-4:] == "USDT" {
		return symbol[:len(symbol)-4] + "-USDT-SWAP"
	}
	return symbol
}

// alignToTickSize 把价格对齐到价格档位
//
// 多单 SL（应低于入场价）→ floor 向下对齐（让 SL 更紧 / 提前触发）
// 空单 SL（应高于入场价）→ ceil 向上对齐
//
// 设计原则：「宁可更安全（更早触发），不要因为档位失配挂单失败」
//
// 入参 tickSize <= 0 时退化为 4 位小数（兜底，避免传 0 导致 NaN）
func alignToTickSize(price, tickSize float64, roundDown bool) float64 {
	if tickSize <= 0 {
		// Fallback: 保留 4 位小数（OKX 大部分合约 tickSz 在 1e-4 ~ 1e-1 范围）
		tickSize = 0.0001
	}
	if roundDown {
		return math.Floor(price/tickSize) * tickSize
	}
	return math.Ceil(price/tickSize) * tickSize
}
