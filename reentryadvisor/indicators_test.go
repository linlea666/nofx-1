package reentryadvisor

import (
	"math"
	"strings"
	"testing"

	"nofx/market"
)

func TestCVDSeries(t *testing.T) {
	// bar1: 买 60 卖 40 → +20；bar2: 买 30 卖 70 → -40；累计 [20, -20]
	klines := []market.Kline{
		{Volume: 100, TakerBuyBaseVolume: 60},
		{Volume: 100, TakerBuyBaseVolume: 30},
	}
	cvd := cvdSeries(klines)
	if len(cvd) != 2 || cvd[0] != 20 || cvd[1] != -20 {
		t.Fatalf("cvdSeries = %v, want [20 -20]", cvd)
	}
}

func TestVolumeRatio(t *testing.T) {
	// 前 20 根量 10，后 5 根量 30 → 量比 3
	var klines []market.Kline
	for i := 0; i < 20; i++ {
		klines = append(klines, market.Kline{Volume: 10})
	}
	for i := 0; i < 5; i++ {
		klines = append(klines, market.Kline{Volume: 30})
	}
	if r := volumeRatio(klines, 5, 20); math.Abs(r-3) > 1e-9 {
		t.Fatalf("volumeRatio = %v, want 3", r)
	}
	// 数据不足返回 0
	if r := volumeRatio(klines[:10], 5, 20); r != 0 {
		t.Fatalf("insufficient data volumeRatio = %v, want 0", r)
	}
}

func TestSeriesSlope(t *testing.T) {
	if s := seriesSlope([]float64{1, 2, 3, 4, 5}, 5); math.Abs(s-1) > 1e-9 {
		t.Fatalf("rising slope = %v, want 1", s)
	}
	if s := seriesSlope([]float64{5, 4, 3, 2, 1}, 5); math.Abs(s+1) > 1e-9 {
		t.Fatalf("falling slope = %v, want -1", s)
	}
	if s := seriesSlope([]float64{7}, 5); s != 0 {
		t.Fatalf("single point slope = %v, want 0", s)
	}
}

// makeVShape 构造 V 形价格序列：先跌到谷底再涨回，谷底成为摆动低点（支撑），
// 两端高点成为摆动高点（阻力）
func makeVShape() []market.Kline {
	prices := []float64{110, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110, 108, 106, 104, 102, 100, 102, 104, 106, 108}
	klines := make([]market.Kline, len(prices))
	for i, p := range prices {
		klines[i] = market.Kline{Open: p, High: p + 1, Low: p - 1, Close: p, Volume: 10}
	}
	return klines
}

func TestNearestSupportResistance(t *testing.T) {
	klines := makeVShape()
	current := 105.0
	atr := 2.0
	sup, res, supT, resT := nearestSupportResistance(klines, current, atr)
	// 谷底 low=99 是支撑（在当前价下方），峰顶 high=111 是阻力（上方）
	if sup <= 0 || sup >= current {
		t.Fatalf("support = %v, want in (0, %v)", sup, current)
	}
	if res <= current {
		t.Fatalf("resistance = %v, want > %v", res, current)
	}
	if supT < 1 || resT < 1 {
		t.Fatalf("touches = %d/%d, want >= 1", supT, resT)
	}
}

func TestPriceOIQuadrant(t *testing.T) {
	cases := []struct {
		price, oi float64
		contains  string
	}{
		{1.0, 1.0, "新多进场"},
		{1.0, -1.0, "空头回补"},
		{-1.0, 1.0, "新空进场"},
		{-1.0, -1.0, "多头平仓"},
		{0.0, 0.0, "不显著"},
	}
	for _, c := range cases {
		got := priceOIQuadrant(c.price, c.oi)
		if !contains(got, c.contains) {
			t.Fatalf("priceOIQuadrant(%v, %v) = %q, want contains %q", c.price, c.oi, got, c.contains)
		}
	}
}

func TestFundingState(t *testing.T) {
	// Binance 基准资金费 0.0001（0.01%/8h）是常态，不得误判为偏热
	if s := fundingState(0.0001); !contains(s, "normal") {
		t.Fatalf("baseline funding state = %q, want normal", s)
	}
	if s := fundingState(0.00005); !contains(s, "normal") {
		t.Fatalf("normal funding state = %q", s)
	}
	if s := fundingState(0.0003); !contains(s, "elevated") || !contains(s, "多付空") {
		t.Fatalf("elevated funding state = %q", s)
	}
	if s := fundingState(-0.001); !contains(s, "extreme") || !contains(s, "空付多") {
		t.Fatalf("extreme funding state = %q", s)
	}
}

func TestPercentileRank(t *testing.T) {
	hist := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if p := percentileRank(hist, 10); p != 100 {
		t.Fatalf("percentile of max = %v, want 100", p)
	}
	if p := percentileRank(hist, 0.5); p != 0 {
		t.Fatalf("percentile below min = %v, want 0", p)
	}
	if p := percentileRank(nil, 1); p != 0 {
		t.Fatalf("empty history percentile = %v, want 0", p)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
