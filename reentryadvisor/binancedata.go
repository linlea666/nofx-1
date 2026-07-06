package reentryadvisor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"nofx/market"
)

// ============================================================================
// Binance 公共行情采集（重入 AI 助手专用）
//
// 复用决策：
//   - 合约 K 线：直接复用 market.APIClient.GetKlines（fapi，已解析 taker buy
//     字段，是 CVD 的原料）；
//   - 现货 K 线 / 资金费历史 / OI 历史 / 多空比 / premiumIndex：独立新写。
//     理由：market/ 包是 AI 交易模式的合约中心数据链，这些端点仅重入分析
//     使用，塞进 market/ 会造成不相关耦合。
//
// 全部为 Binance 免鉴权公共 REST；请求失败按字段降级（调用方标注
// missing_fields），不整体失败。
// ============================================================================

const (
	bnFapiBase = "https://fapi.binance.com"
	bnSpotBase = "https://api.binance.com"
)

// symbolAvailCacheTTL Binance 币种可用性缓存（OKX 特有品种在 Binance 恒缺失，
// 无需反复探测）
const symbolAvailCacheTTL = 24 * time.Hour

// klineCacheTTL K 线短缓存：同一信号多次重新生成 / 多信号同币种时避免重复拉取
const klineCacheTTL = 60 * time.Second

type binanceClient struct {
	http    *http.Client
	futures *market.APIClient

	mu         sync.Mutex
	availCache map[string]availEntry // key: "futures|SYMBOL" / "spot|SYMBOL"
	klineCache map[string]klineEntry // key: "futures|SYMBOL|5m|60"
}

type availEntry struct {
	available bool
	ts        time.Time
}

type klineEntry struct {
	klines []market.Kline
	ts     time.Time
}

func newBinanceClient() *binanceClient {
	return &binanceClient{
		http:       &http.Client{Timeout: 15 * time.Second},
		futures:    market.NewAPIClient(),
		availCache: map[string]availEntry{},
		klineCache: map[string]klineEntry{},
	}
}

// getJSON 发起 GET 并解码到 out；HTTP 非 2xx 返回错误
func (c *binanceClient) getJSON(url string, out interface{}) error {
	resp, err := c.http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// futuresKlines 合约 K 线（带 60s 短缓存与可用性缓存）
func (c *binanceClient) futuresKlines(symbol, interval string, limit int) ([]market.Kline, error) {
	return c.cachedKlines("futures", symbol, interval, limit, func() ([]market.Kline, error) {
		return c.futures.GetKlines(symbol, interval, limit)
	})
}

// spotKlines 现货 K 线（响应格式与合约一致：index 9/10 为 taker buy 量/额）
func (c *binanceClient) spotKlines(symbol, interval string, limit int) ([]market.Kline, error) {
	return c.cachedKlines("spot", symbol, interval, limit, func() ([]market.Kline, error) {
		url := fmt.Sprintf("%s/api/v3/klines?symbol=%s&interval=%s&limit=%d", bnSpotBase, symbol, interval, limit)
		var raw [][]interface{}
		if err := c.getJSON(url, &raw); err != nil {
			return nil, err
		}
		klines := make([]market.Kline, 0, len(raw))
		for _, kr := range raw {
			if len(kr) < 11 {
				continue
			}
			var k market.Kline
			k.OpenTime = int64(asFloat(kr[0]))
			k.Open = parseNumStr(kr[1])
			k.High = parseNumStr(kr[2])
			k.Low = parseNumStr(kr[3])
			k.Close = parseNumStr(kr[4])
			k.Volume = parseNumStr(kr[5])
			k.CloseTime = int64(asFloat(kr[6]))
			k.QuoteVolume = parseNumStr(kr[7])
			k.Trades = int(asFloat(kr[8]))
			k.TakerBuyBaseVolume = parseNumStr(kr[9])
			k.TakerBuyQuoteVolume = parseNumStr(kr[10])
			klines = append(klines, k)
		}
		return klines, nil
	})
}

func asFloat(v interface{}) float64 {
	f, _ := v.(float64)
	return f
}

func parseNumStr(v interface{}) float64 {
	s, ok := v.(string)
	if !ok {
		return asFloat(v)
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// cachedKlines 统一的 K 线缓存 + 可用性探测。拉取失败或空结果视为该市场
// 无此币种（Binance 对无效 symbol 返回错误 JSON，解析即失败），24h 内不再探测。
func (c *binanceClient) cachedKlines(kind, symbol, interval string, limit int, fetch func() ([]market.Kline, error)) ([]market.Kline, error) {
	availKey := kind + "|" + symbol
	cacheKey := fmt.Sprintf("%s|%s|%s|%d", kind, symbol, interval, limit)

	c.mu.Lock()
	if e, ok := c.availCache[availKey]; ok && !e.available && time.Since(e.ts) < symbolAvailCacheTTL {
		c.mu.Unlock()
		return nil, fmt.Errorf("symbol %s not available on binance %s", symbol, kind)
	}
	if e, ok := c.klineCache[cacheKey]; ok && time.Since(e.ts) < klineCacheTTL {
		c.mu.Unlock()
		return e.klines, nil
	}
	c.mu.Unlock()

	klines, err := fetch()
	if err != nil || len(klines) == 0 {
		c.mu.Lock()
		c.availCache[availKey] = availEntry{available: false, ts: time.Now()}
		c.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("empty klines for %s %s", kind, symbol)
		}
		return nil, err
	}
	c.mu.Lock()
	c.availCache[availKey] = availEntry{available: true, ts: time.Now()}
	c.klineCache[cacheKey] = klineEntry{klines: klines, ts: time.Now()}
	c.mu.Unlock()
	return klines, nil
}

// ---------------------------------------------------------------------------
// 合约衍生数据
// ---------------------------------------------------------------------------

// premiumIndexData 资金费/基差原料（fapi/v1/premiumIndex）
type premiumIndexData struct {
	MarkPrice       float64
	IndexPrice      float64
	LastFundingRate float64
	NextFundingTime int64 // ms
}

func (c *binanceClient) premiumIndex(symbol string) (*premiumIndexData, error) {
	var raw struct {
		MarkPrice       string `json:"markPrice"`
		IndexPrice      string `json:"indexPrice"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
	}
	url := fmt.Sprintf("%s/fapi/v1/premiumIndex?symbol=%s", bnFapiBase, symbol)
	if err := c.getJSON(url, &raw); err != nil {
		return nil, err
	}
	out := &premiumIndexData{NextFundingTime: raw.NextFundingTime}
	out.MarkPrice, _ = strconv.ParseFloat(raw.MarkPrice, 64)
	out.IndexPrice, _ = strconv.ParseFloat(raw.IndexPrice, 64)
	out.LastFundingRate, _ = strconv.ParseFloat(raw.LastFundingRate, 64)
	return out, nil
}

// fundingHistory 资金费历史（8h 结算一次，limit=30 ≈ 10 天）
func (c *binanceClient) fundingHistory(symbol string, limit int) ([]float64, error) {
	var raw []struct {
		FundingRate string `json:"fundingRate"`
	}
	url := fmt.Sprintf("%s/fapi/v1/fundingRate?symbol=%s&limit=%d", bnFapiBase, symbol, limit)
	if err := c.getJSON(url, &raw); err != nil {
		return nil, err
	}
	out := make([]float64, 0, len(raw))
	for _, r := range raw {
		f, _ := strconv.ParseFloat(r.FundingRate, 64)
		out = append(out, f)
	}
	return out, nil
}

// oiPoint OI 历史采样点
type oiPoint struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"` // sumOpenInterest（币本位数量）
	ValueUSD  float64 `json:"value_usd"`
}

// openInterestHist OI 历史（period: 5m/15m/30m/1h/4h/1d；最多回溯 30 天）
func (c *binanceClient) openInterestHist(symbol, period string, limit int) ([]oiPoint, error) {
	var raw []struct {
		SumOpenInterest      string `json:"sumOpenInterest"`
		SumOpenInterestValue string `json:"sumOpenInterestValue"`
		Timestamp            int64  `json:"timestamp"`
	}
	url := fmt.Sprintf("%s/futures/data/openInterestHist?symbol=%s&period=%s&limit=%d", bnFapiBase, symbol, period, limit)
	if err := c.getJSON(url, &raw); err != nil {
		return nil, err
	}
	out := make([]oiPoint, 0, len(raw))
	for _, r := range raw {
		v, _ := strconv.ParseFloat(r.SumOpenInterest, 64)
		usd, _ := strconv.ParseFloat(r.SumOpenInterestValue, 64)
		out = append(out, oiPoint{Timestamp: r.Timestamp, Value: v, ValueUSD: usd})
	}
	return out, nil
}

// lsPoint 多空比采样点
type lsPoint struct {
	Timestamp int64   `json:"timestamp"`
	Ratio     float64 `json:"ratio"`
}

// longShortRatio kind: "global"（全体账户人数比）| "top"（大户持仓量比）
func (c *binanceClient) longShortRatio(symbol, kind, period string, limit int) ([]lsPoint, error) {
	endpoint := "globalLongShortAccountRatio"
	if kind == "top" {
		endpoint = "topLongShortPositionRatio"
	}
	var raw []struct {
		LongShortRatio string `json:"longShortRatio"`
		Timestamp      int64  `json:"timestamp"`
	}
	url := fmt.Sprintf("%s/futures/data/%s?symbol=%s&period=%s&limit=%d", bnFapiBase, endpoint, symbol, period, limit)
	if err := c.getJSON(url, &raw); err != nil {
		return nil, err
	}
	out := make([]lsPoint, 0, len(raw))
	for _, r := range raw {
		f, _ := strconv.ParseFloat(r.LongShortRatio, 64)
		out = append(out, lsPoint{Timestamp: r.Timestamp, Ratio: f})
	}
	return out, nil
}
