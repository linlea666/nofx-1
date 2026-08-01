package reentryadvisor

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"nofx/copytrade"
	"nofx/market"
	"nofx/store"
)

// ============================================================================
// 决策数据包组装：Copy Guard 仓位层（既有表读取）+ 市场层（Binance 快照）
//
// 市场层降级策略：Binance 无该币种合约 → market=null、futures_available=false，
// 仓位层照常完整输出（+OKX ATR）；单个字段拉取失败只记 missing_fields 不整体失败。
// ============================================================================

// 数据包内各周期的（拉取根数, 入包根数）。拉取多于入包是为了指标计算需要
// 足够历史（量比 base 20、S/R 摆动窗口、CVD 累计段）。
var timeframeSpec = []struct {
	tf      string
	fetch   int
	include int
}{
	{"5m", 240, 24},
	{"15m", 240, 24},
	{"30m", 240, 16},
	{"1h", 240, 24},
	{"4h", 240, 24},
	{"1d", 240, 30},
}

// cvdTimeframes 计算现货/合约 CVD 的周期子集
var cvdTimeframes = []string{"5m", "15m", "1h", "4h"}

// DataPack 完整决策数据包（datapack_json 的结构）
type DataPack struct {
	Meta      MetaSection    `json:"meta"`
	CopyGuard GuardSection   `json:"copy_guard"`
	Market    *MarketSection `json:"market,omitempty"`
}

type MetaSection struct {
	SnapshotAt       string            `json:"snapshot_at"`
	Symbol           string            `json:"symbol"`
	Side             string            `json:"side"`
	FuturesAvailable bool              `json:"binance_futures_available"`
	SpotAvailable    bool              `json:"binance_spot_available"`
	DataSources      string            `json:"data_sources"`
	MissingFields    []string          `json:"missing_fields,omitempty"`
	Note             string            `json:"note,omitempty"`
	SourceTimestamps map[string]string `json:"source_timestamps,omitempty"`
	UnavailableData  map[string]string `json:"unavailable_data"`
}

type GuardSection struct {
	SignalID    int64  `json:"signal_id"`
	CandidateID int64  `json:"candidate_id,omitempty"`
	CycleID     int64  `json:"cycle_id"`
	TraderID    string `json:"trader_id"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	MarginMode  string `json:"margin_mode,omitempty"`

	AccountEquityAtCycleOpen float64 `json:"account_equity_at_cycle_open"`
	RecommendedNotional      float64 `json:"recommended_notional_usdt"`
	TriggerPrice             float64 `json:"signal_trigger_price"`

	// ATR：与引擎门控同源（OKX 标记价 K 线），保证与信号判定一致
	GateATR          float64 `json:"gate_atr_okx"`
	GateATRTimeframe string  `json:"gate_atr_timeframe"`
	ATRAtCycleEntry  float64 `json:"atr_at_cycle_entry"`
	ATRExpansionPct  float64 `json:"atr_expansion_vs_entry_pct"` // 当前 ATR 相对首仓时的扩张幅度

	StopCount                int     `json:"stop_count"`
	AutoReentryCount         int     `json:"auto_reentry_count"`
	DistanceATRRatio         float64 `json:"last_stop_distance_atr_ratio"` // 止损距离/ATR（<0.3 为噪音档）
	ReentryBoundary          float64 `json:"reentry_boundary_price"`
	ReentryBoundaryAvailable bool    `json:"reentry_boundary_available"`
	ChaseLimit               float64 `json:"chase_limit_price,omitempty"`
	ChaseLimitAvailable      bool    `json:"chase_limit_available"`
	Protectable              bool    `json:"new_stop_protectable_precheck"`
	SignalReason             string  `json:"signal_reason"`

	Leader   LeaderInfo     `json:"leader"`
	Attempts []AttemptEntry `json:"attempts"`
	LastStop *LastStopInfo  `json:"last_stop,omitempty"`

	Policy              map[string]interface{}           `json:"risk_policy"`
	ProtectionStatus    string                           `json:"protection_status"`
	ProtectionCoverage  float64                          `json:"protection_coverage"`
	ProtectionError     string                           `json:"protection_error,omitempty"`
	CurrentAccount      *copytrade.ExecutionRiskSnapshot `json:"current_account,omitempty"`
	PreviousAIDecisions []AIDecisionHistory              `json:"previous_ai_decisions,omitempty"`
	RecentEvents        []GuardEventHistory              `json:"recent_events,omitempty"`
	LeaderTimeline      []LeaderTimelinePoint            `json:"leader_timeline,omitempty"`
}

type AIDecisionHistory struct {
	AnalysisID    int64    `json:"analysis_id"`
	Decision      string   `json:"decision"`
	Confidence    float64  `json:"confidence"`
	SnapshotPrice float64  `json:"snapshot_price"`
	SnapshotAt    string   `json:"snapshot_at"`
	OutcomePnL    *float64 `json:"outcome_pnl,omitempty"`
}

type GuardEventHistory struct {
	Type      string                 `json:"type"`
	Price     float64                `json:"price"`
	PnL       float64                `json:"pnl"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt string                 `json:"created_at"`
}

type LeaderTimelinePoint struct {
	EntryPrice float64 `json:"entry_price"`
	Size       float64 `json:"size"`
	MarkPrice  float64 `json:"mark_price"`
	ATR        float64 `json:"atr"`
	CreatedAt  string  `json:"created_at"`
}

func buildDataPackForCandidate(st *store.Store, bn *binanceClient, c *store.CopyGuardReentryCandidate) (*DataPack, error) {
	if c == nil {
		return nil, fmt.Errorf("nil reentry candidate")
	}
	sig := &store.CopyGuardManualReentrySignal{ID: c.ID, CycleID: c.CycleID, TraderID: c.TraderID, LeaderPosID: c.LeaderPosID, Symbol: c.Symbol, Side: c.Side, MarginMode: c.MarginMode, TriggerPrice: c.TriggerPrice, ATR: c.ATR, DistanceATRRatio: c.DistanceATRRatio, RecommendedNotional: c.MaxNotional, StopCount: c.StopCount, ReentryCount: c.ReentryCount, LeaderSize: c.LeaderSize, LeaderEntryPrice: c.LeaderEntryPrice, Protectable: c.Protectable, Reason: "AI guarded candidate"}
	pack, err := buildDataPack(st, bn, sig)
	if err != nil {
		return nil, err
	}
	pack.CopyGuard.CandidateID = c.ID
	pack.CopyGuard.SignalID = 0
	if cycle, cycleErr := st.CopyTrade().GetCopyGuardCycle(c.CycleID); cycleErr == nil {
		pack.CopyGuard.ProtectionStatus = cycle.ProtectionStatus
		pack.CopyGuard.ProtectionCoverage = cycle.ProtectionCoverage
		pack.CopyGuard.ProtectionError = cycle.ProtectionError
	}
	if account, accountErr := copytrade.GetExecutionRiskSnapshotForTrader(c.TraderID, c.CycleID); accountErr == nil {
		pack.CopyGuard.CurrentAccount = account
	} else {
		pack.Meta.MissingFields = append(pack.Meta.MissingFields, "execution_account_snapshot")
	}
	if history, historyErr := st.ReentryAI().ListReentryAnalysesByCycle(c.CycleID, 30); historyErr == nil {
		for _, previous := range history {
			pack.CopyGuard.PreviousAIDecisions = append(pack.CopyGuard.PreviousAIDecisions, AIDecisionHistory{AnalysisID: previous.ID, Decision: previous.Verdict, Confidence: previous.Confidence, SnapshotPrice: previous.SnapshotPrice, SnapshotAt: previous.SnapshotAt.UTC().Format(time.RFC3339), OutcomePnL: previous.OutcomePnL})
		}
	}
	if events, eventErr := st.CopyTrade().ListCopyGuardEvents(c.CycleID); eventErr == nil {
		start := 0
		if len(events) > 50 {
			start = len(events) - 50
		}
		for _, event := range events[start:] {
			pack.CopyGuard.RecentEvents = append(pack.CopyGuard.RecentEvents, GuardEventHistory{Type: event.Type, Price: event.Price, PnL: event.PnL, Metadata: event.Metadata, CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339)})
		}
	}
	if samples, sampleErr := st.CopyTrade().ListCopyGuardWatchSamples(c.CycleID); sampleErr == nil {
		start := 0
		if len(samples) > 60 {
			start = len(samples) - 60
		}
		for _, sample := range samples[start:] {
			pack.CopyGuard.LeaderTimeline = append(pack.CopyGuard.LeaderTimeline, LeaderTimelinePoint{EntryPrice: sample.LeaderEntryPrice, Size: sample.LeaderSize, MarkPrice: sample.MarkPrice, ATR: sample.ATR, CreatedAt: sample.CreatedAt.UTC().Format(time.RFC3339)})
		}
	}
	if executionPrice, priceErr := copytrade.GetExecutionMarketPriceForTrader(c.TraderID, c.Symbol); priceErr == nil && executionPrice > 0 {
		pack.CopyGuard.TriggerPrice = executionPrice
		if pack.Market == nil {
			pack.Market = &MarketSection{Klines: map[string]*KlineSummary{}}
		}
		pack.Market.CurrentPrice = executionPrice
		pack.Market.CurrentPriceSource = "follower_execution_exchange"
		pack.Meta.DataSources = "follower_execution_exchange(price, primary) + okx_mark_price_atr + binance_public_market(auxiliary)"
		if pack.Meta.SourceTimestamps == nil {
			pack.Meta.SourceTimestamps = map[string]string{}
		}
		pack.Meta.SourceTimestamps["follower_execution_exchange"] = time.Now().UTC().Format(time.RFC3339)
	}
	return pack, nil
}

type LeaderInfo struct {
	StillHolding                 bool    `json:"still_holding_same_side"`
	Size                         float64 `json:"position_size"`
	EntryPrice                   float64 `json:"entry_price"`
	EntryAtCycleOpen             float64 `json:"entry_price_at_cycle_open"`
	UnrealizedPnLPct             float64 `json:"unrealized_pnl_pct_est"`
	SizeVsCycleBaseline          float64 `json:"size_vs_cycle_baseline_ratio"` // 当前仓位/周期基线仓位（>1=加过仓）
	SizeVsCycleBaselineAvailable bool    `json:"size_vs_cycle_baseline_available"`
	BehaviorClass                string  `json:"behavior_class"`
	AddingWhileLosing            bool    `json:"adding_while_losing"`
	ConcentrationSpike           bool    `json:"concentration_spike"`
}

type AttemptEntry struct {
	AttemptNo  int     `json:"attempt_no"` // 0=首仓
	Status     string  `json:"status"`
	EntryPrice float64 `json:"entry_price"`
	ExitPrice  float64 `json:"exit_price,omitempty"`
	Notional   float64 `json:"notional_usdt"`
	PnL        float64 `json:"pnl_usdt"`
	Fee        float64 `json:"fee_usdt"`
	ATR        float64 `json:"atr_at_entry,omitempty"`
	OpenedAt   string  `json:"opened_at"`
	ClosedAt   string  `json:"closed_at,omitempty"`
}

type LastStopInfo struct {
	Price                  float64 `json:"stop_price"`
	DistanceFromCurrentATR float64 `json:"current_price_distance_atr"` // 当前价距上次止损价（ATR 倍数，正=沿持仓方向恢复）
	StopClusterSpreadATR   float64 `json:"stop_cluster_spread_atr"`    // 各次止损价最大间距（ATR 倍数，小=同一噪声区反复扫损）
}

type MarketSection struct {
	CurrentPrice       float64 `json:"current_price"`
	CurrentPriceSource string  `json:"current_price_source"`

	Klines map[string]*KlineSummary `json:"klines"`

	ContractCVD map[string]*CVDSummary `json:"contract_cvd,omitempty"`
	SpotCVD     map[string]*CVDSummary `json:"spot_cvd,omitempty"`

	OpenInterest *OISummary      `json:"open_interest,omitempty"`
	Funding      *FundingSummary `json:"funding,omitempty"`
	LongShort    *LSSummary      `json:"long_short_ratio,omitempty"`
	Basis        *BasisSummary   `json:"basis,omitempty"`

	SupportResistance map[string]*SRSummary `json:"support_resistance,omitempty"`

	// 现货/合约成交额对比：判断恢复由现货真实买盘驱动还是纯合约投机
	SpotToContractVolumeRatio24h float64 `json:"spot_to_contract_volume_ratio_24h,omitempty"`
}

type KlineSummary struct {
	// bars: [open_time_ms, open, high, low, close, volume]，旧→新
	Bars           [][]float64                `json:"bars"`
	PctChange      float64                    `json:"pct_change_window"` // 入包窗口首尾涨跌幅 %
	VolumeRatio520 float64                    `json:"volume_ratio_5_20"` // 近5根均量/前20根均量（>1 放量）
	ATR14          float64                    `json:"atr_14"`
	MovingAverages map[string]*IndicatorValue `json:"moving_averages"`
	VWAP           *IndicatorValue            `json:"vwap"`
	SwingAnchors   *SwingAnchorSummary        `json:"swing_anchors"`
	Fibonacci      map[string]*IndicatorValue `json:"fibonacci_retracement"`
	Structure      string                     `json:"high_low_structure"`
	BreakoutState  string                     `json:"breakout_retest_false_break"`
	RecentHigh     float64                    `json:"recent_high"`
	RecentLow      float64                    `json:"recent_low"`
}

type IndicatorValue struct {
	Available bool    `json:"available"`
	Value     float64 `json:"value,omitempty"`
}

type SwingAnchorSummary struct {
	Available bool    `json:"available"`
	Low       float64 `json:"low,omitempty"`
	High      float64 `json:"high,omitempty"`
	LowAt     string  `json:"low_at,omitempty"`
	HighAt    string  `json:"high_at,omitempty"`
}

type CVDSummary struct {
	SeriesTail []float64 `json:"series_tail"` // 最近 20 个累计值（旧→新，以窗口起点为 0）
	Last       float64   `json:"last"`
	SlopeSign  string    `json:"slope_recent"` // rising / falling / flat（近 20 根线性斜率）
	Divergence string    `json:"divergence_note,omitempty"`
}

type OISummary struct {
	Latest      float64            `json:"latest"`
	LatestUSD   float64            `json:"latest_usd"`
	ChangePct   map[string]float64 `json:"change_pct"` // 1h/4h/24h
	SeriesTail  []float64          `json:"series_1h_tail"`
	PriceOIRead string             `json:"price_oi_read_4h"` // 四象限解读
}

type FundingSummary struct {
	CurrentRate        float64 `json:"current_rate"`
	State              string  `json:"state"`
	NextFundingMinutes float64 `json:"next_funding_minutes"`
	Avg10d             float64 `json:"avg_rate_10d"`
	Percentile10d      float64 `json:"current_percentile_10d"`
}

type LSSummary struct {
	GlobalAccountsRatio float64 `json:"global_accounts_ratio"` // >1 = 做多人数占优
	TopPositionsRatio   float64 `json:"top_positions_ratio"`   // 大户持仓量多空比
	GlobalTrend24h      string  `json:"global_trend_24h"`      // rising / falling / flat
}

type BasisSummary struct {
	MarkPrice  float64 `json:"mark_price"`
	IndexPrice float64 `json:"index_price"`
	BasisPct   float64 `json:"basis_pct"` // 正=升水（多头拥挤倾向）
}

type SRSummary struct {
	NearestSupport         float64 `json:"nearest_support"`
	SupportTouches         int     `json:"support_touches"`
	SupportDistanceATR     float64 `json:"support_distance_atr"`
	NearestResistance      float64 `json:"nearest_resistance"`
	ResistanceTouches      int     `json:"resistance_touches"`
	ResistanceDistanceATR  float64 `json:"resistance_distance_atr"`
	SupportZoneLow         float64 `json:"support_zone_low"`
	SupportZoneHigh        float64 `json:"support_zone_high"`
	SupportStrength        int     `json:"support_strength"`
	SupportLastTouchAt     string  `json:"support_last_touch_at"`
	SupportRoleReversal    bool    `json:"support_role_reversal"`
	SupportExhaustion      string  `json:"support_exhaustion"`
	ResistanceZoneLow      float64 `json:"resistance_zone_low"`
	ResistanceZoneHigh     float64 `json:"resistance_zone_high"`
	ResistanceStrength     int     `json:"resistance_strength"`
	ResistanceLastTouchAt  string  `json:"resistance_last_touch_at"`
	ResistanceRoleReversal bool    `json:"resistance_role_reversal"`
	ResistanceExhaustion   string  `json:"resistance_exhaustion"`
}

// buildDataPack 为一条人工重入信号组装完整数据包。
// 除仓位层核心数据（信号/周期/attempt 表）失败会返回错误外，市场层任何
// 字段失败只降级并记录 missing_fields。
func buildDataPack(st *store.Store, bn *binanceClient, sig *store.CopyGuardManualReentrySignal) (*DataPack, error) {
	cycle, err := st.CopyTrade().GetCopyGuardCycle(sig.CycleID)
	if err != nil {
		return nil, fmt.Errorf("读取周期 %d 失败: %w", sig.CycleID, err)
	}
	attempts, err := st.CopyTrade().ListCopyGuardAttempts(sig.CycleID)
	if err != nil {
		return nil, fmt.Errorf("读取周期 %d attempts 失败: %w", sig.CycleID, err)
	}

	pack := &DataPack{
		Meta: MetaSection{
			SnapshotAt:       time.Now().UTC().Format(time.RFC3339),
			Symbol:           sig.Symbol,
			Side:             sig.Side,
			DataSources:      "binance_futures(fapi) + binance_spot + okx_mark_price_atr(与门控同源)",
			SourceTimestamps: map[string]string{"public_market_snapshot": time.Now().UTC().Format(time.RFC3339)},
			UnavailableData: map[string]string{
				"onchain": "UNAVAILABLE", "etf_flows": "UNAVAILABLE", "macro": "UNAVAILABLE",
				"options": "UNAVAILABLE", "cost_basis_chips": "UNAVAILABLE",
			},
		},
	}

	// ---------- Copy Guard 仓位层 ----------
	guard := GuardSection{
		SignalID:                 sig.ID,
		CycleID:                  sig.CycleID,
		TraderID:                 sig.TraderID,
		Symbol:                   sig.Symbol,
		Side:                     sig.Side,
		MarginMode:               sig.MarginMode,
		AccountEquityAtCycleOpen: round(cycle.AccountEquity, 2),
		RecommendedNotional:      round(sig.RecommendedNotional, 2),
		TriggerPrice:             sig.TriggerPrice,
		ATRAtCycleEntry:          cycle.ATRAtEntry,
		StopCount:                sig.StopCount,
		AutoReentryCount:         sig.ReentryCount,
		DistanceATRRatio:         round(sig.DistanceATRRatio, 4),
		ReentryBoundary:          sig.ReentryBoundary,
		Protectable:              sig.Protectable,
		SignalReason:             sig.Reason,
		Policy:                   extractRiskPolicy(cycle.PolicySnapshot),
	}

	// ATR：优先取实时（与引擎同源 OKX 标记价），失败回退信号快照值
	atrTF := "1h"
	if v, ok := guard.Policy["risk_atr_timeframe"].(string); ok && v != "" {
		atrTF = v
	}
	guard.GateATRTimeframe = atrTF
	// ATR 周期跟随策略快照（引擎门控同参），仅缺失时才回退默认 14
	atrPeriod := 14
	if v, ok := guard.Policy["risk_atr_period"].(float64); ok && v > 0 {
		atrPeriod = int(v)
	}
	if atr, err := market.GetOKXATRWithMaxAge(sig.Symbol, atrTF, atrPeriod, 2*time.Hour); err == nil && atr > 0 {
		guard.GateATR = atr
	} else {
		guard.GateATR = sig.ATR
		if err != nil {
			pack.Meta.MissingFields = append(pack.Meta.MissingFields, "okx_atr_live(已回退信号快照值)")
		}
	}
	if guard.ATRAtCycleEntry > 0 && guard.GateATR > 0 {
		guard.ATRExpansionPct = round((guard.GateATR/guard.ATRAtCycleEntry-1)*100, 2)
	}

	// 最近观察采样：追价上限（chase_limit 不在信号快照里，从采样表补）
	if sample, err := st.CopyTrade().GetLatestCopyGuardWatchSample(sig.CycleID); err == nil && sample != nil {
		if sample.ReentryBoundary > 0 {
			guard.ReentryBoundary = sample.ReentryBoundary
		}
		guard.ChaseLimit = sample.ChaseLimit
	} else if err != nil && err != sql.ErrNoRows {
		pack.Meta.MissingFields = append(pack.Meta.MissingFields, "chase_limit")
	}

	for _, a := range attempts {
		e := AttemptEntry{
			AttemptNo:  a.AttemptNo,
			Status:     a.Status,
			EntryPrice: a.EntryPrice,
			ExitPrice:  a.ExitPrice,
			Notional:   round(a.Notional, 2),
			PnL:        round(a.PnL, 4),
			Fee:        round(a.Fee, 4),
			ATR:        a.ATR,
			OpenedAt:   a.OpenedAt.UTC().Format(time.RFC3339),
		}
		if a.ClosedAt != nil {
			e.ClosedAt = a.ClosedAt.UTC().Format(time.RFC3339)
		}
		guard.Attempts = append(guard.Attempts, e)
	}

	// ---------- 市场层（Binance 快照，可整体缺失） ----------
	pack.Market = buildMarketSection(bn, sig.Symbol, guard.GateATR, &pack.Meta)

	// 当前价：市场层可用时取 Binance 标记价/最新收盘，否则回退信号触发价
	currentPrice := sig.TriggerPrice
	if pack.Market != nil && pack.Market.CurrentPrice > 0 {
		currentPrice = pack.Market.CurrentPrice
	} else {
		pack.Meta.Note = "该币种无 Binance 市场数据，市场层缺失；current_price 使用信号触发价，请结合 OKX 行情人工判断"
	}

	// 领航员状态（基于信号快照 + 当前价估算）
	guard.Leader = LeaderInfo{
		StillHolding:     sig.LeaderSize > 0,
		Size:             sig.LeaderSize,
		EntryPrice:       sig.LeaderEntryPrice,
		EntryAtCycleOpen: cycle.LeaderEntryPrice,
	}
	if sig.LeaderEntryPrice > 0 && currentPrice > 0 {
		pnl := pctChange(sig.LeaderEntryPrice, currentPrice)
		if strings.EqualFold(sig.Side, "short") {
			pnl = -pnl
		}
		guard.Leader.UnrealizedPnLPct = round(pnl, 3)
	}
	if cycle.BaselineAvailable && cycle.BaselineLeaderSize > 0 {
		guard.Leader.SizeVsCycleBaseline = round(sig.LeaderSize/cycle.BaselineLeaderSize, 3)
		guard.Leader.SizeVsCycleBaselineAvailable = true
		ratio := guard.Leader.SizeVsCycleBaseline
		switch {
		case ratio < 0.8:
			guard.Leader.BehaviorClass = "REDUCING"
		case ratio >= 1.2 && guard.Leader.UnrealizedPnLPct < 0:
			guard.Leader.BehaviorClass = "AVERAGING_DOWN"
			guard.Leader.AddingWhileLosing = true
		case ratio >= 1.2:
			guard.Leader.BehaviorClass = "TREND_ADDING"
		default:
			guard.Leader.BehaviorClass = "HOLDING"
		}
		guard.Leader.ConcentrationSpike = ratio >= 5
	} else {
		guard.Leader.BehaviorClass = "UNKNOWN"
	}
	guard.ReentryBoundaryAvailable = guard.ReentryBoundary > 0
	guard.ChaseLimitAvailable = guard.ChaseLimit > 0
	if !guard.ReentryBoundaryAvailable {
		pack.Meta.MissingFields = append(pack.Meta.MissingFields, "reentry_boundary")
	}
	if !guard.ChaseLimitAvailable {
		pack.Meta.MissingFields = append(pack.Meta.MissingFields, "chase_limit")
	}
	if !guard.Leader.SizeVsCycleBaselineAvailable {
		pack.Meta.MissingFields = append(pack.Meta.MissingFields, "baseline_leader_size")
	}

	// 止损簇分析：上次止损价距当前价（沿持仓方向为正）+ 各次止损价最大间距
	guard.LastStop = buildLastStopInfo(attempts, sig.Side, currentPrice, guard.GateATR)

	pack.CopyGuard = guard
	return pack, nil
}

// buildLastStopInfo 从 attempts 提取止损簇信息；无 STOPPED attempt 返回 nil
func buildLastStopInfo(attempts []*store.CopyGuardAttempt, side string, currentPrice, atr float64) *LastStopInfo {
	var stopPrices []float64
	var lastStop float64
	lastNo := -1
	for _, a := range attempts {
		if a.Status != "STOPPED" {
			continue
		}
		price := a.StopFillPrice
		if price <= 0 {
			price = a.ExitPrice
		}
		if price <= 0 {
			continue
		}
		stopPrices = append(stopPrices, price)
		if a.AttemptNo > lastNo {
			lastNo = a.AttemptNo
			lastStop = price
		}
	}
	if lastStop <= 0 {
		return nil
	}
	info := &LastStopInfo{Price: lastStop}
	if atr > 0 && currentPrice > 0 {
		// 多单：价高于止损价 = 沿方向恢复（正）；空单相反
		dist := (currentPrice - lastStop) / atr
		if strings.EqualFold(side, "short") {
			dist = -dist
		}
		info.DistanceFromCurrentATR = round(dist, 3)
	}
	if len(stopPrices) > 1 && atr > 0 {
		min, max := stopPrices[0], stopPrices[0]
		for _, p := range stopPrices[1:] {
			if p < min {
				min = p
			}
			if p > max {
				max = p
			}
		}
		info.StopClusterSpreadATR = round((max-min)/atr, 3)
	}
	return info
}

// buildMarketSection Binance 市场层；合约不可用返回 nil
func buildMarketSection(bn *binanceClient, symbol string, atr float64, meta *MetaSection) *MarketSection {
	// 先探测主周期：失败即视为 Binance 无此币种合约
	primaryKlines, err := bn.futuresKlines(symbol, "1h", 240)
	if err != nil || len(primaryKlines) == 0 {
		meta.FuturesAvailable = false
		meta.SpotAvailable = false
		return nil
	}
	meta.FuturesAvailable = true
	closedPrimary := closedKlinesAt(primaryKlines, time.Now())
	if len(closedPrimary) == 0 {
		meta.MissingFields = append(meta.MissingFields, "closed_futures_klines_1h")
		return nil
	}

	m := &MarketSection{
		Klines:            map[string]*KlineSummary{},
		ContractCVD:       map[string]*CVDSummary{},
		SupportResistance: map[string]*SRSummary{},
	}

	// 多周期 K 线 + 量比 + 合约 CVD
	futuresByTF := map[string][]market.Kline{"1h": closedPrimary}
	for _, spec := range timeframeSpec {
		klines := futuresByTF[spec.tf]
		if klines == nil {
			klines, err = bn.futuresKlines(symbol, spec.tf, spec.fetch)
			if err != nil {
				meta.MissingFields = append(meta.MissingFields, "futures_klines_"+spec.tf)
				continue
			}
			klines = closedKlinesAt(klines, time.Now())
			if len(klines) == 0 {
				meta.MissingFields = append(meta.MissingFields, "closed_futures_klines_"+spec.tf)
				continue
			}
			futuresByTF[spec.tf] = klines
		}
		m.Klines[spec.tf] = summarizeKlines(klines, spec.include)
		if latest := klines[len(klines)-1]; latest.CloseTime > 0 {
			meta.SourceTimestamps["binance_futures_closed_kline_"+spec.tf] = time.UnixMilli(latest.CloseTime).UTC().Format(time.RFC3339)
		}
	}
	for _, tf := range cvdTimeframes {
		if klines := futuresByTF[tf]; len(klines) > 0 {
			if cvd := summarizeCVD(klines); cvd != nil {
				m.ContractCVD[tf] = cvd
			}
		}
	}

	// 当前价：最新 1h K 线收盘（premiumIndex 成功时用标记价覆盖）
	m.CurrentPrice = primaryKlines[len(primaryKlines)-1].Close
	m.CurrentPriceSource = "binance_futures_1h_last_close"

	// 现货 CVD + 现货/合约成交额比
	spotOK := false
	spotCVD := map[string]*CVDSummary{}
	var spotQuote24h float64
	for _, tf := range cvdTimeframes {
		limit := 60
		if tf == "1h" || tf == "4h" {
			limit = 120
		}
		klines, err := bn.spotKlines(symbol, tf, limit)
		if err != nil {
			continue
		}
		klines = closedKlinesAt(klines, time.Now())
		if len(klines) == 0 {
			meta.MissingFields = append(meta.MissingFields, "closed_spot_klines_"+tf)
			continue
		}
		spotOK = true
		if cvd := summarizeCVD(klines); cvd != nil {
			spotCVD[tf] = cvd
		}
		if tf == "1h" && len(klines) >= 24 {
			for _, k := range klines[len(klines)-24:] {
				spotQuote24h += k.QuoteVolume
			}
		}
	}
	meta.SpotAvailable = spotOK
	if spotOK {
		m.SpotCVD = spotCVD
		if spotQuote24h > 0 && len(closedPrimary) >= 24 {
			var contractQuote24h float64
			for _, k := range closedPrimary[len(closedPrimary)-24:] {
				contractQuote24h += k.QuoteVolume
			}
			if contractQuote24h > 0 {
				m.SpotToContractVolumeRatio24h = round(spotQuote24h/contractQuote24h, 4)
			}
		}
	} else {
		meta.MissingFields = append(meta.MissingFields, "spot_cvd(该币种无 Binance 现货)")
	}

	// OI：当前 + 1h 历史 48 点
	if oiHist, err := bn.openInterestHist(symbol, "1h", 48); err == nil && len(oiHist) > 0 {
		latest := oiHist[len(oiHist)-1]
		oi := &OISummary{
			Latest:    latest.Value,
			LatestUSD: round(latest.ValueUSD, 0),
			ChangePct: map[string]float64{},
		}
		for label, back := range map[string]int{"1h": 1, "4h": 4, "24h": 24} {
			if len(oiHist) > back {
				oi.ChangePct[label] = round(pctChange(oiHist[len(oiHist)-1-back].Value, latest.Value), 3)
			}
		}
		tail := oiHist
		if len(tail) > 24 {
			tail = tail[len(tail)-24:]
		}
		for _, p := range tail {
			oi.SeriesTail = append(oi.SeriesTail, round(p.Value, 2))
		}
		// 四象限：4h 价格变化 × 4h OI 变化
		var price4hChange float64
		if k := futuresByTF["1h"]; len(k) >= 5 {
			price4hChange = pctChange(k[len(k)-5].Close, k[len(k)-1].Close)
		}
		oi.PriceOIRead = priceOIQuadrant(price4hChange, oi.ChangePct["4h"])
		m.OpenInterest = oi
	} else {
		meta.MissingFields = append(meta.MissingFields, "open_interest")
	}

	// 资金费 + 基差
	if premium, err := bn.premiumIndex(symbol); err == nil {
		if premium.MarkPrice > 0 {
			m.CurrentPrice = premium.MarkPrice
			m.CurrentPriceSource = "binance_futures_mark_price"
		}
		funding := &FundingSummary{
			CurrentRate: premium.LastFundingRate,
			State:       fundingState(premium.LastFundingRate),
		}
		if premium.NextFundingTime > 0 {
			funding.NextFundingMinutes = round(time.Until(time.UnixMilli(premium.NextFundingTime)).Minutes(), 1)
		}
		if hist, err := bn.fundingHistory(symbol, 30); err == nil && len(hist) > 0 {
			var sum float64
			for _, h := range hist {
				sum += h
			}
			funding.Avg10d = sum / float64(len(hist))
			funding.Percentile10d = round(percentileRank(hist, premium.LastFundingRate), 1)
		} else {
			meta.MissingFields = append(meta.MissingFields, "funding_history")
		}
		m.Funding = funding
		if premium.IndexPrice > 0 {
			m.Basis = &BasisSummary{
				MarkPrice:  premium.MarkPrice,
				IndexPrice: premium.IndexPrice,
				BasisPct:   round((premium.MarkPrice-premium.IndexPrice)/premium.IndexPrice*100, 4),
			}
		}
	} else {
		meta.MissingFields = append(meta.MissingFields, "funding_and_basis")
	}

	// 多空比
	ls := &LSSummary{}
	lsOK := false
	if global, err := bn.longShortRatio(symbol, "global", "1h", 24); err == nil && len(global) > 0 {
		ls.GlobalAccountsRatio = global[len(global)-1].Ratio
		series := make([]float64, len(global))
		for i, p := range global {
			series[i] = p.Ratio
		}
		ls.GlobalTrend24h = slopeLabel(seriesSlope(series, len(series)), 0.001)
		lsOK = true
	}
	if top, err := bn.longShortRatio(symbol, "top", "1h", 1); err == nil && len(top) > 0 {
		ls.TopPositionsRatio = top[len(top)-1].Ratio
		lsOK = true
	}
	if lsOK {
		m.LongShort = ls
	} else {
		meta.MissingFields = append(meta.MissingFields, "long_short_ratio")
	}

	// 支撑/阻力：1h 与 4h 各算一组（摆动点聚类，tolerance=0.25×ATR）
	for _, tf := range []string{"1h", "4h"} {
		klines := futuresByTF[tf]
		if len(klines) == 0 {
			continue
		}
		sup, res, supT, resT := nearestSupportResistance(klines, m.CurrentPrice, atr)
		sr := &SRSummary{
			NearestSupport: sup, SupportTouches: supT,
			NearestResistance: res, ResistanceTouches: resT,
		}
		populateSRZoneEvidence(sr, klines, m.CurrentPrice, atr)
		if atr > 0 {
			if sup > 0 {
				sr.SupportDistanceATR = round((m.CurrentPrice-sup)/atr, 3)
			}
			if res > 0 {
				sr.ResistanceDistanceATR = round((res-m.CurrentPrice)/atr, 3)
			}
		}
		m.SupportResistance[tf] = sr
	}

	return m
}

// closedKlinesAt excludes the exchange's currently forming candle from every
// structural/volume/CVD input. Live execution price remains a separate field;
// deterministic triggers and consecutive ABANDON confirmation must only use
// completed bars.
func closedKlinesAt(klines []market.Kline, now time.Time) []market.Kline {
	cutoff := now.UnixMilli()
	out := make([]market.Kline, 0, len(klines))
	for _, kline := range klines {
		if kline.CloseTime > 0 && kline.CloseTime < cutoff {
			out = append(out, kline)
		}
	}
	return out
}

// summarizeKlines 截取入包窗口并计算窗口涨跌幅 + 量比
func summarizeKlines(klines []market.Kline, include int) *KlineSummary {
	s := &KlineSummary{VolumeRatio520: round(volumeRatio(klines, 5, 20), 3), MovingAverages: map[string]*IndicatorValue{}, Fibonacci: map[string]*IndicatorValue{}}
	tail := klines
	if len(tail) > include {
		tail = tail[len(tail)-include:]
	}
	for _, k := range tail {
		s.Bars = append(s.Bars, []float64{
			float64(k.OpenTime),
			roundSig(k.Open), roundSig(k.High), roundSig(k.Low), roundSig(k.Close),
			round(k.Volume, 2),
		})
	}
	if len(tail) > 1 {
		s.PctChange = round(pctChange(tail[0].Open, tail[len(tail)-1].Close), 3)
	}
	s.ATR14 = roundSig(klineATR(klines, 14))
	for _, period := range []int{20, 50, 100, 200} {
		key := fmt.Sprintf("ma%d", period)
		if len(klines) >= period {
			var sum float64
			for _, k := range klines[len(klines)-period:] {
				sum += k.Close
			}
			s.MovingAverages[key] = &IndicatorValue{Available: true, Value: roundSig(sum / float64(period))}
		} else {
			s.MovingAverages[key] = &IndicatorValue{Available: false}
		}
	}
	s.Structure, s.RecentHigh, s.RecentLow = highLowStructure(klines)
	s.BreakoutState = breakoutState(klines)
	s.VWAP = closedKlineVWAP(tail)
	s.SwingAnchors, s.Fibonacci = closedKlineSwingFibonacci(klines, 120)
	return s
}

func closedKlineVWAP(klines []market.Kline) *IndicatorValue {
	var weighted, volume float64
	for _, k := range klines {
		if k.Volume <= 0 {
			continue
		}
		price := (k.High + k.Low + k.Close) / 3
		if k.QuoteVolume > 0 {
			weighted += k.QuoteVolume
		} else {
			weighted += price * k.Volume
		}
		volume += k.Volume
	}
	if volume <= 0 {
		return &IndicatorValue{Available: false}
	}
	return &IndicatorValue{Available: true, Value: roundSig(weighted / volume)}
}

func closedKlineSwingFibonacci(klines []market.Kline, window int) (*SwingAnchorSummary, map[string]*IndicatorValue) {
	levels := map[string]*IndicatorValue{}
	if window <= 0 || len(klines) < 2 {
		return &SwingAnchorSummary{Available: false}, levels
	}
	if len(klines) > window {
		klines = klines[len(klines)-window:]
	}
	lowIndex, highIndex := 0, 0
	for i := 1; i < len(klines); i++ {
		if klines[i].Low < klines[lowIndex].Low {
			lowIndex = i
		}
		if klines[i].High > klines[highIndex].High {
			highIndex = i
		}
	}
	low, high := klines[lowIndex].Low, klines[highIndex].High
	if low <= 0 || high <= low {
		return &SwingAnchorSummary{Available: false}, levels
	}
	anchors := &SwingAnchorSummary{
		Available: true, Low: roundSig(low), High: roundSig(high),
		LowAt:  time.UnixMilli(klines[lowIndex].CloseTime).UTC().Format(time.RFC3339),
		HighAt: time.UnixMilli(klines[highIndex].CloseTime).UTC().Format(time.RFC3339),
	}
	rangeSize := high - low
	for label, ratio := range map[string]float64{"23.6%": .236, "38.2%": .382, "50.0%": .5, "61.8%": .618, "78.6%": .786} {
		levels[label] = &IndicatorValue{Available: true, Value: roundSig(high - rangeSize*ratio)}
	}
	return anchors, levels
}

func klineATR(klines []market.Kline, period int) float64 {
	if period <= 0 || len(klines) < period+1 {
		return 0
	}
	var total float64
	start := len(klines) - period
	for i := start; i < len(klines); i++ {
		prevClose := klines[i-1].Close
		tr := math.Max(klines[i].High-klines[i].Low, math.Max(math.Abs(klines[i].High-prevClose), math.Abs(klines[i].Low-prevClose)))
		total += tr
	}
	return total / float64(period)
}

func highLowStructure(klines []market.Kline) (string, float64, float64) {
	highs, lows := findSwingPoints(klines, 2)
	if len(highs) < 2 || len(lows) < 2 {
		return "INSUFFICIENT", 0, 0
	}
	lastHigh, prevHigh := highs[len(highs)-1], highs[len(highs)-2]
	lastLow, prevLow := lows[len(lows)-1], lows[len(lows)-2]
	switch {
	case lastHigh > prevHigh && lastLow > prevLow:
		return "HH_HL_UPTREND", lastHigh, lastLow
	case lastHigh < prevHigh && lastLow < prevLow:
		return "LH_LL_DOWNTREND", lastHigh, lastLow
	default:
		return "MIXED_RANGE", lastHigh, lastLow
	}
}

func breakoutState(klines []market.Kline) string {
	if len(klines) < 22 {
		return "INSUFFICIENT"
	}
	latest := klines[len(klines)-1]
	window := klines[len(klines)-21 : len(klines)-1]
	high, low := window[0].High, window[0].Low
	for _, k := range window[1:] {
		if k.High > high {
			high = k.High
		}
		if k.Low < low {
			low = k.Low
		}
	}
	switch {
	case latest.High > high && latest.Close <= high:
		return "FALSE_BREAK_ABOVE"
	case latest.Low < low && latest.Close >= low:
		return "FALSE_BREAK_BELOW"
	case latest.Close > high:
		return "CLOSED_BREAKOUT_ABOVE"
	case latest.Close < low:
		return "CLOSED_BREAKOUT_BELOW"
	default:
		return "INSIDE_20_BAR_RANGE"
	}
}

func populateSRZoneEvidence(sr *SRSummary, klines []market.Kline, currentPrice, atr float64) {
	if sr == nil || len(klines) == 0 {
		return
	}
	tolerance := 0.25 * atr
	if tolerance <= 0 {
		tolerance = currentPrice * 0.002
	}
	exhaustion := func(touches int) string {
		if touches <= 1 {
			return "FRESH"
		}
		if touches <= 3 {
			return "TESTED"
		}
		return "WEAKENED"
	}
	lastTouch := func(level float64, support bool) string {
		if level <= 0 {
			return "UNAVAILABLE"
		}
		for i := len(klines) - 1; i >= 0; i-- {
			touched := math.Abs(klines[i].Low-level) <= tolerance
			if !support {
				touched = math.Abs(klines[i].High-level) <= tolerance
			}
			if touched {
				return time.UnixMilli(klines[i].CloseTime).UTC().Format(time.RFC3339)
			}
		}
		return "UNAVAILABLE"
	}
	roleReversal := func(level float64) bool {
		if level <= 0 || len(klines) < 3 {
			return false
		}
		above, below := false, false
		for _, k := range klines {
			above = above || k.Close > level+tolerance
			below = below || k.Close < level-tolerance
		}
		return above && below
	}
	if sr.NearestSupport > 0 {
		sr.SupportZoneLow, sr.SupportZoneHigh = roundSig(sr.NearestSupport-tolerance), roundSig(sr.NearestSupport+tolerance)
		sr.SupportStrength = int(math.Min(100, 45+float64(sr.SupportTouches)*12))
		sr.SupportLastTouchAt, sr.SupportRoleReversal, sr.SupportExhaustion = lastTouch(sr.NearestSupport, true), roleReversal(sr.NearestSupport), exhaustion(sr.SupportTouches)
	}
	if sr.NearestResistance > 0 {
		sr.ResistanceZoneLow, sr.ResistanceZoneHigh = roundSig(sr.NearestResistance-tolerance), roundSig(sr.NearestResistance+tolerance)
		sr.ResistanceStrength = int(math.Min(100, 45+float64(sr.ResistanceTouches)*12))
		sr.ResistanceLastTouchAt, sr.ResistanceRoleReversal, sr.ResistanceExhaustion = lastTouch(sr.NearestResistance, false), roleReversal(sr.NearestResistance), exhaustion(sr.ResistanceTouches)
	}
}

// summarizeCVD CVD 摘要：窗口末 20 值（以窗口起点归零）+ 斜率 + 价量背离
func summarizeCVD(klines []market.Kline) *CVDSummary {
	series := cvdSeries(klines)
	if len(series) == 0 {
		return nil
	}
	const window = 20
	start := 0
	if len(series) > window {
		start = len(series) - window
	}
	base := 0.0
	if start > 0 {
		base = series[start-1]
	}
	s := &CVDSummary{}
	for _, v := range series[start:] {
		s.SeriesTail = append(s.SeriesTail, round(v-base, 2))
	}
	s.Last = s.SeriesTail[len(s.SeriesTail)-1]
	slope := seriesSlope(series, window)
	// 平坦阈值：相对窗口内均量的量纲
	var avgVol float64
	for _, k := range klines[start:] {
		avgVol += k.Volume
	}
	if n := len(klines) - start; n > 0 {
		avgVol /= float64(n)
	}
	s.SlopeSign = slopeLabel(slope, avgVol*0.01)

	// 背离：窗口内价格与 CVD 方向相反
	priceChange := pctChange(klines[start].Close, klines[len(klines)-1].Close)
	switch {
	case priceChange > 0.1 && s.SlopeSign == "falling":
		s.Divergence = "价涨但CVD走弱（买盘不济，疑似诱多/回补驱动）"
	case priceChange < -0.1 && s.SlopeSign == "rising":
		s.Divergence = "价跌但CVD走强（卖压不济，疑似诱空/存在承接）"
	}
	return s
}

func slopeLabel(slope, flatThreshold float64) string {
	if flatThreshold < 0 {
		flatThreshold = 0
	}
	switch {
	case slope > flatThreshold:
		return "rising"
	case slope < -flatThreshold:
		return "falling"
	default:
		return "flat"
	}
}

// roundSig 价格保留 6 位有效数字（兼容高价币与小数币）
func roundSig(v float64) float64 {
	if v == 0 {
		return 0
	}
	digits := 6 - int(math.Ceil(math.Log10(math.Abs(v))))
	if digits < 0 {
		digits = 0
	}
	if digits > 10 {
		digits = 10
	}
	return round(v, digits)
}

// extractRiskPolicy 从周期 policy_snapshot 提取 risk_* 与 min_trade_warn 字段
func extractRiskPolicy(snapshot string) map[string]interface{} {
	out := map[string]interface{}{}
	if snapshot == "" {
		return out
	}
	var full map[string]interface{}
	if err := json.Unmarshal([]byte(snapshot), &full); err != nil {
		return out
	}
	for k, v := range full {
		if strings.HasPrefix(k, "risk_") || k == "min_trade_warn" || k == "copy_ratio" {
			out[k] = v
		}
	}
	return out
}
