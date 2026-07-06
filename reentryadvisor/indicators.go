package reentryadvisor

import (
	"math"
	"sort"

	"nofx/market"
)

// ============================================================================
// 本地指标算法（独立新写）
//
// 项目现状：market/ 包只有 EMA/MACD/RSI/ATR（私有）且面向 AI 交易模式；
// CVD / 量比 / 支撑阻力 / OI-价格四象限 / 资金费分档均无实现，此处从零编写。
// 全部为纯函数（输入 K 线/序列 → 输出数值），无副作用，便于单测。
// ============================================================================

// cvdSeries 累计成交量差（CVD）：
// 每根 K 线 delta = 主动买量 − 主动卖量 = 2×takerBuy − volume，逐根累计。
// 现货与合约 K 线同构，通用。返回与输入等长的累计序列。
func cvdSeries(klines []market.Kline) []float64 {
	out := make([]float64, len(klines))
	var cum float64
	for i, k := range klines {
		cum += 2*k.TakerBuyBaseVolume - k.Volume
		out[i] = cum
	}
	return out
}

// seriesSlope 序列末段线性斜率（最小二乘，x 取 0..n-1）。
// n > len(series) 时用全序列；不足 2 点返回 0。
func seriesSlope(series []float64, n int) float64 {
	if n > len(series) {
		n = len(series)
	}
	if n < 2 {
		return 0
	}
	tail := series[len(series)-n:]
	var sumX, sumY, sumXY, sumXX float64
	for i, y := range tail {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	fn := float64(n)
	denom := fn*sumXX - sumX*sumX
	if denom == 0 {
		return 0
	}
	return (fn*sumXY - sumX*sumY) / denom
}

// volumeRatio 量比：最近 recent 根均量 / 之前 base 根均量。
// >1 放量、<1 缩量；数据不足返回 0。
func volumeRatio(klines []market.Kline, recent, base int) float64 {
	if recent <= 0 || base <= 0 || len(klines) < recent+base {
		return 0
	}
	var recentSum, baseSum float64
	n := len(klines)
	for _, k := range klines[n-recent:] {
		recentSum += k.Volume
	}
	for _, k := range klines[n-recent-base : n-recent] {
		baseSum += k.Volume
	}
	if baseSum == 0 {
		return 0
	}
	return (recentSum / float64(recent)) / (baseSum / float64(base))
}

// srLevel 支撑/阻力聚类价位
type srLevel struct {
	Price   float64 `json:"price"`
	Touches int     `json:"touches"` // 摆动点聚到该簇的次数（越多越强）
}

// findSwingPoints 摆动高/低点：bar i 的 high 是 i±k 窗口最大值 → 摆动高点；
// low 同理。返回 (highs, lows) 价位列表。
func findSwingPoints(klines []market.Kline, k int) (highs, lows []float64) {
	if k <= 0 {
		k = 3
	}
	for i := k; i < len(klines)-k; i++ {
		isHigh, isLow := true, true
		for j := i - k; j <= i+k; j++ {
			if j == i {
				continue
			}
			if klines[j].High >= klines[i].High {
				isHigh = false
			}
			if klines[j].Low <= klines[i].Low {
				isLow = false
			}
		}
		if isHigh {
			highs = append(highs, klines[i].High)
		}
		if isLow {
			lows = append(lows, klines[i].Low)
		}
	}
	return highs, lows
}

// clusterLevels 将摆动点按价距聚类（tolerance 为绝对价差，建议 0.25×ATR）。
// 返回按触碰次数降序的簇中心列表。
func clusterLevels(points []float64, tolerance float64) []srLevel {
	if len(points) == 0 || tolerance <= 0 {
		return nil
	}
	sorted := append([]float64(nil), points...)
	sort.Float64s(sorted)
	var clusters []srLevel
	start := 0
	for i := 1; i <= len(sorted); i++ {
		if i == len(sorted) || sorted[i]-sorted[i-1] > tolerance {
			group := sorted[start:i]
			var sum float64
			for _, p := range group {
				sum += p
			}
			clusters = append(clusters, srLevel{Price: sum / float64(len(group)), Touches: len(group)})
			start = i
		}
	}
	sort.Slice(clusters, func(a, b int) bool { return clusters[a].Touches > clusters[b].Touches })
	return clusters
}

// nearestSupportResistance 最近支撑（当前价下方最高簇）与最近阻力（上方最低簇）。
// 返回值为 0 表示该侧无有效簇。
func nearestSupportResistance(klines []market.Kline, currentPrice, atr float64) (support, resistance float64, supportTouches, resistanceTouches int) {
	if len(klines) == 0 || currentPrice <= 0 {
		return 0, 0, 0, 0
	}
	tolerance := 0.25 * atr
	if tolerance <= 0 {
		tolerance = currentPrice * 0.002 // ATR 缺失时退化为 0.2% 价距
	}
	highs, lows := findSwingPoints(klines, 3)
	// 支撑候选=摆动低点簇，阻力候选=摆动高点簇（经典口径）
	for _, c := range clusterLevels(lows, tolerance) {
		if c.Price < currentPrice && (support == 0 || c.Price > support) {
			support, supportTouches = c.Price, c.Touches
		}
	}
	for _, c := range clusterLevels(highs, tolerance) {
		if c.Price > currentPrice && (resistance == 0 || c.Price < resistance) {
			resistance, resistanceTouches = c.Price, c.Touches
		}
	}
	return support, resistance, supportTouches, resistanceTouches
}

// priceOIQuadrant 价格-OI 四象限解读（拥挤度/驱动来源判断的经典框架）
func priceOIQuadrant(priceChangePct, oiChangePct float64) string {
	const eps = 0.05 // ±0.05% 内视为持平
	switch {
	case priceChangePct > eps && oiChangePct > eps:
		return "价涨+OI增：新多进场推动（趋势较健康）"
	case priceChangePct > eps && oiChangePct < -eps:
		return "价涨+OI减：空头回补驱动（上涨持续性存疑）"
	case priceChangePct < -eps && oiChangePct > eps:
		return "价跌+OI增：新空进场推动（下跌较健康）"
	case priceChangePct < -eps && oiChangePct < -eps:
		return "价跌+OI减：多头平仓驱动（下跌持续性存疑）"
	default:
		return "价格/OI 变化不显著"
	}
}

// fundingState 资金费状态分档。永续基准资金费恰为 +0.01%/8h（0.0001），
// 属常态而非偏热，故 normal 上界取 0.00015（基准 + 少量浮动）。
func fundingState(rate float64) string {
	abs := math.Abs(rate)
	direction := "多付空"
	if rate < 0 {
		direction = "空付多"
	}
	switch {
	case abs <= 0.00015:
		return "normal（常态区间）"
	case abs < 0.0006:
		return "elevated（偏热，" + direction + "）"
	default:
		return "extreme（过热拥挤，" + direction + "）"
	}
}

// percentileRank value 在 history 中的百分位（0~100）；history 为空返回 0
func percentileRank(history []float64, value float64) float64 {
	if len(history) == 0 {
		return 0
	}
	below := 0
	for _, h := range history {
		if h <= value {
			below++
		}
	}
	return 100 * float64(below) / float64(len(history))
}

// pctChange 序列首尾涨跌幅（%）；无效输入返回 0
func pctChange(first, last float64) float64 {
	if first == 0 {
		return 0
	}
	return (last - first) / first * 100
}

// round 保留 n 位小数（数据包瘦身用）
func round(v float64, n int) float64 {
	p := math.Pow10(n)
	return math.Round(v*p) / p
}
