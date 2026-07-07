package reentryadvisor

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"nofx/market"
)

// ============================================================================
// A2：市场指标实时预览
//
// 与信号无关的纯市场层快照：复用数据包的 buildMarketSection（同一套指标口径，
// 预览所见即数据包所得），供用户持续观察指标质量、评估增删指标。
// 60s 结果缓存：Binance 权重接口（OI 历史/多空比等）扛不住每人每秒刷，
// 且指标本身以 1h 级别为主，60s 粒度足够。
// ============================================================================

const previewCacheTTL = 60 * time.Second

// MarketPreview 预览结果（market 为 nil 表示 Binance 无该币种合约）
type MarketPreview struct {
	Symbol           string         `json:"symbol"`
	GeneratedAt      time.Time      `json:"generated_at"`
	FuturesAvailable bool           `json:"futures_available"`
	SpotAvailable    bool           `json:"spot_available"`
	ATR              float64        `json:"atr_okx_1h"` // OKX 1h ATR（与门控同源；0=获取失败，S/R 距离将缺省）
	Market           *MarketSection `json:"market"`
	MissingFields    []string       `json:"missing_fields,omitempty"`
}

type previewCacheEntry struct {
	data *MarketPreview
	at   time.Time
}

var (
	previewMu    sync.Mutex
	previewCache = map[string]*previewCacheEntry{}
	// previewFlight 同 symbol 并发请求合并：缓存过期瞬间的多个刷新请求
	// 只有一个真正打 Binance 权重接口，其余等待共享结果。
	previewFlight singleflight.Group
	symbolRe      = regexp.MustCompile(`^[A-Z0-9]{2,20}$`)
)

// GetMarketPreview 生成（或返回缓存的）某币种市场指标预览。
// 插件未启动时也可用：预览只依赖 Binance 客户端，不依赖轮询循环。
func GetMarketPreview(symbol string) (*MarketPreview, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if !symbolRe.MatchString(symbol) {
		return nil, fmt.Errorf("无效的交易对格式（示例：BTCUSDT）")
	}

	v, err, _ := previewFlight.Do(symbol, func() (interface{}, error) {
		return buildMarketPreview(symbol)
	})
	if err != nil {
		return nil, err
	}
	return v.(*MarketPreview), nil
}

func buildMarketPreview(symbol string) (*MarketPreview, error) {
	previewMu.Lock()
	if e, ok := previewCache[symbol]; ok && time.Since(e.at) < previewCacheTTL {
		previewMu.Unlock()
		return e.data, nil
	}
	previewMu.Unlock()

	defaultAdvisorMu.RLock()
	a := defaultAdvisor
	defaultAdvisorMu.RUnlock()
	bn := newBinanceClient()
	if a != nil {
		bn = a.bn // 复用插件的 K 线缓存
	}

	p := &MarketPreview{Symbol: symbol, GeneratedAt: time.Now().UTC()}
	// ATR 尽力而为：预览无周期上下文，统一用 1h（S/R 距离、量纲换算用）
	if atr, err := market.GetOKXATRWithMaxAge(symbol, "1h", 14, 2*time.Hour); err == nil && atr > 0 {
		p.ATR = atr
	} else {
		p.MissingFields = append(p.MissingFields, "okx_atr_1h")
	}
	meta := &MetaSection{}
	p.Market = buildMarketSection(bn, symbol, p.ATR, meta)
	p.FuturesAvailable = meta.FuturesAvailable
	p.SpotAvailable = meta.SpotAvailable
	p.MissingFields = append(p.MissingFields, meta.MissingFields...)

	previewMu.Lock()
	// 简单防膨胀：缓存达 50 个币种时清空重来（预览是人肉操作，量级极小）
	if len(previewCache) >= 50 {
		previewCache = map[string]*previewCacheEntry{}
	}
	previewCache[symbol] = &previewCacheEntry{data: p, at: time.Now()}
	previewMu.Unlock()
	return p, nil
}
