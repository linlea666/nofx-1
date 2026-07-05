package copytrade

import (
	"math"
	"testing"
)

func TestComputeRiskDistanceV4ATRGovernsWhenCapsAreWider(t *testing.T) {
	// 账户线 0.2 → 账户距离 = 1000×0.2/1000×100 = 20，远宽于 ATR 3 → ATR 主导。
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.2, RiskSlippageBufferBPS: 10, RiskATRCacheMaxAgeMinutes: 120}
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Distance != 3 || r.GovernedBy != "atr" {
		t.Fatalf("unexpected result: %+v", r)
	}
	// 3% of 1000 plus 10bps = 31 USD. Leverage must not enter this formula.
	if math.Abs(r.ExpectedLossUSD-31) > 1e-9 {
		t.Fatalf("expected 31, got %f", r.ExpectedLossUSD)
	}
}

func TestComputeRiskDistanceV4AccountCapReportsNoiseConflict(t *testing.T) {
	// 账户线 1%：距离 = 1000×0.01/2000×100 = 0.5 < ATR 3 → 账户 cap 主导且
	// 落在噪音区内。v5 起账户线在任何 stop mode 下都是硬 cap。
	c := &CopyConfig{RiskPolicyVersion: 4, RiskStopMode: "account_hard_limit", RiskAccountPct: 0.01}
	r, err := ComputeRiskDistanceV4(c, 100, 2000, 1000, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account_cap" || !r.NoiseConflict {
		t.Fatalf("unexpected result: %+v", r)
	}
}

// TestComputeRiskDistanceV4MarginCapBoundsStopDistance reproduces the WLD
// incident: 20x leverage with an ATR distance of ~3% of price meant the stop
// only fired around -60% margin loss. With the margin-loss cap the distance is
// bounded by entry × maxLoss / leverage regardless of how wide the ATR is.
func TestComputeRiskDistanceV4MarginCapBoundsStopDistance(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.2, RiskLeverageFallback: true, RiskLeverageMaxLoss: 0.3}
	// entry=100, leverage=20, maxLoss=30% → cap distance = 100×0.3/20 = 1.5 < ATR 3.
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 20)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-1.5) > 1e-9 || r.GovernedBy != "margin_cap" || !r.NoiseConflict {
		t.Fatalf("expected margin_cap distance 1.5 with noise conflict, got %+v", r)
	}
	// Cap wider than ATR must not loosen the stop.
	r, err = ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 5) // cap = 100×0.3/5 = 6 > 3
	if err != nil {
		t.Fatal(err)
	}
	if r.Distance != 3 || r.GovernedBy != "atr" {
		t.Fatalf("expected atr-governed distance 3, got %+v", r)
	}
	// Disabled fallback keeps pure ATR behaviour.
	c.RiskLeverageFallback = false
	r, err = ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 20)
	if err != nil {
		t.Fatal(err)
	}
	if r.Distance != 3 || r.GovernedBy != "atr" {
		t.Fatalf("expected atr-governed distance with fallback disabled, got %+v", r)
	}
}

// TestComputeRiskDistanceV4NoLoosening：v5 移除噪音下限——高杠杆下保证金 cap
// 塌缩到噪音区内（100x → 0.3% 价距）时，任何机制都不得把它放宽（v4.1 的
// noise_floor 曾放宽到 2×ATR，是实盘 -40% 止损的直接根因）。宽度问题由重入
// 噪音档（distance/ATR < 0.3 默认禁入）与 UI 易扫损提示处理。
func TestComputeRiskDistanceV4NoLoosening(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.2, RiskLeverageFallback: true, RiskLeverageMaxLoss: 0.3, RiskATRMultiplier: 1.5}
	// entry=100, leverage=100 → margin cap distance = 100×0.3/100 = 0.3
	// atrDistance=3（raw ATR 2 × multiplier 1.5）
	r, err := ComputeRiskDistanceV4(c, 100, 1000, 1000, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.3) > 1e-9 || r.GovernedBy != "margin_cap" || !r.NoiseConflict {
		t.Fatalf("tight margin cap must never be loosened, got %+v", r)
	}
	// distance/ATR = 0.3/2 = 0.15 < 0.3 → 易扫损配置，重入噪音档的输入
	if math.Abs(r.DistanceATRRatio-0.15) > 1e-9 {
		t.Fatalf("DistanceATRRatio must expose the noise tier input, got %+v", r)
	}
	// 保证金口径：损失 = 0.3% 名义 = 3 USD，保证金 = 1000/100 = 10 → 30%
	if math.Abs(r.ExpectedMarginLossPct-0.3) > 1e-9 {
		t.Fatalf("ExpectedMarginLossPct must be loss/margin, got %+v", r)
	}
}

// TestComputeRiskDistanceV4AccountHardBackstopCapsEveryMode verifies that
// RiskAccountPct is a catastrophe backstop in every stop mode.
func TestComputeRiskDistanceV4AccountHardBackstopCapsEveryMode(t *testing.T) {
	// volatility_priority, notional 2000 vs equity 1000, account pct 1% →
	// account distance = 1000×0.01/2000×100 = 0.5 < ATR 3.
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskAccountPct: 0.01}
	r, err := ComputeRiskDistanceV4(c, 100, 2000, 1000, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account_cap" || !r.NoiseConflict {
		t.Fatalf("account backstop must cap the distance in volatility_priority, got %+v", r)
	}
	// 账户线也压过保证金 cap：100x margin cap 0.3 < 0.5 → margin_cap 主导；
	// 若保证金 cap 更宽（10x → 3），账户线 0.5 接管。
	c.RiskLeverageFallback, c.RiskLeverageMaxLoss = true, 0.3
	r, err = ComputeRiskDistanceV4(c, 100, 2000, 1000, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.Distance-0.5) > 1e-9 || r.GovernedBy != "account_cap" {
		t.Fatalf("account backstop must outrank a wider margin cap, got %+v", r)
	}
}

func TestValidateRiskPolicyV4(t *testing.T) {
	c := &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskStopMode: "volatility_priority", RiskATRPeriod: 14, RiskATRCacheMaxAgeMinutes: 120, RiskATRMultiplier: 1.5, RiskATRFallbackPct: 0.02, RiskTriggerPriceType: "mark", RiskAccountPct: 0.02, RiskLiquidationBufferATR: 0.5, RiskMaxReentries: 2, RiskReentryRatio: 0.5, RiskReentryBandATR: 0.5, RiskReentryCooldownSeconds: 60, RiskReentryMaxATRExpansion: 2}
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatal(err)
	}
	// v5：不可保护处置模式仅接受 close/follow
	c.RiskUnprotectableAction = "panic"
	if err := ValidateRiskPolicyV4(c); err == nil {
		t.Fatal("invalid risk_unprotectable_action must be rejected")
	}
	c.RiskUnprotectableAction = "follow"
	if err := ValidateRiskPolicyV4(c); err != nil {
		t.Fatal(err)
	}
}
