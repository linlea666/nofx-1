// Package copytrade 真人领航员跟单模块
// 实现监听领航员交易信号，按比例同步跟单
package copytrade

import (
	"time"
)

// ProviderType 数据源类型
type ProviderType string

const (
	ProviderHyperliquid ProviderType = "hyperliquid"
	ProviderOKX         ProviderType = "okx"
	ProviderBinance     ProviderType = "binance"
)

// ActionType 交易动作类型
type ActionType string

const (
	ActionOpen   ActionType = "open"   // 开仓
	ActionClose  ActionType = "close"  // 平仓
	ActionAdd    ActionType = "add"    // 加仓
	ActionReduce ActionType = "reduce" // 减仓
)

// ============================================================================
// 全平判断阈值常量（统一定义，避免多处硬编码）
// ============================================================================
const (
	// FullCloseThreshold 全平阈值：减仓量 >= 仓位的 95% 视为全平
	// 用于 Provider 层判断 sz/startPosition
	FullCloseThreshold = 0.95

	// NearZeroThreshold 近零阈值：剩余仓位 < 5% 视为全平
	// 用于 Engine 层判断 currentSize/lastKnownSize
	NearZeroThreshold = 0.05

	// AccumulatedCloseThreshold 累积减仓触发全平阈值
	// 当累积减仓比例 >= 90% 时，自动触发全量平仓
	AccumulatedCloseThreshold = 0.90

	// MinOrderValue 最小订单价值（适用于 Hyperliquid 等有此限制的交易所）
	// 减仓价值低于此值时跳过，等待后续全平
	MinOrderValue = 10.0
)

// SideType 持仓方向
type SideType string

const (
	SideLong  SideType = "long"
	SideShort SideType = "short"
	SideNet   SideType = "net" // OKX 单向持仓模式的原始值，需标准化为 long/short
)

// Fill 成交记录（标准化结构）
type Fill struct {
	ID           string     // 唯一标识 (HL: tid, OKX: ordId)
	Symbol       string     // 交易对 (BTCUSDT 格式)
	Side         string     // "buy" | "sell"
	PositionSide SideType   // "long" | "short"
	Action       ActionType // "open" | "close" | "add" | "reduce"
	Price        float64    // 成交价格
	Size         float64    // 成交数量
	Value        float64    // 成交价值 (USDT)
	Timestamp    time.Time  // 成交时间
	ClosedPnL    float64    // 平仓盈亏 (如有)

	// 原始数据（调试用）
	Raw interface{} `json:"-"`
}

// Position 持仓信息
type Position struct {
	Symbol        string
	Side          SideType // "long" | "short"
	Size          float64  // 持仓数量
	EntryPrice    float64  // 开仓均价
	MarkPrice     float64  // 标记价格
	Leverage      int      // 杠杆
	MarginMode    string   // "cross" | "isolated"
	UnrealizedPnL float64
	PositionValue float64 // 仓位价值
	PosID         string  // OKX 仓位唯一标识（用于精确匹配）
}

// AccountState 账户状态
type AccountState struct {
	TotalEquity      float64              // 总权益
	AvailableBalance float64              // 可用余额
	Positions        map[string]*Position // 当前持仓 (symbol_side -> position)
	Timestamp        time.Time
}

// TradeSignal 交易信号（经过处理的成交事件）
type TradeSignal struct {
	LeaderID     string       // 领航员 ID
	ProviderType ProviderType // "hyperliquid" | "okx"
	Fill         *Fill        // 成交记录

	// 领航员账户快照（用于比例计算）
	LeaderEquity   float64   // 领航员总权益
	LeaderPosition *Position // 该币种的持仓（如有）
	LeaderPosID    string    // 领航员仓位 ID（OKX 独有，用于精确匹配）
}

// CopyConfig 跟单配置
type CopyConfig struct {
	ProviderType   ProviderType `json:"provider_type"`    // "hyperliquid" | "okx" | "binance"
	LeaderID       string       `json:"leader_id"`        // 领航员地址/uniqueName，Binance 模式下复用为 portfolioId
	CopyRatio      float64      `json:"copy_ratio"`       // 跟单系数 (1.0 = 100%)
	SyncLeverage   bool         `json:"sync_leverage"`    // 同步杠杆
	SyncMarginMode bool         `json:"sync_margin_mode"` // 同步保证金模式

	// 预警阈值（不限制，只记录预警）
	MinTradeWarn float64 `json:"min_trade_warn"` // 低于此金额记录预警
	MaxTradeWarn float64 `json:"max_trade_warn"` // 高于此金额记录预警 (0=不预警)

	// Binance Web 私有接口凭证（仅 ProviderType=binance 时使用）
	// 由用户从浏览器开发者工具中抓取，会过期，过期时通过邮件告警
	BinanceP20T      string `json:"binance_p20t,omitempty"`       // 登录 cookie p20t
	BinanceCSRFToken string `json:"binance_csrf_token,omitempty"` // CSRF header csrftoken

	// ============================================================
	// 账户保护 / 止损兜底（v3 风控）
	// 仅 OKX 路径生效；HL/Binance 路径下被忽略
	// 字段含义见 store/copytrade.go CopyTradeConfig
	// ============================================================
	RiskStopLossEnabled  bool    `json:"risk_stop_loss_enabled"`
	RiskAccountPct       float64 `json:"risk_account_pct"`
	RiskATREnabled       bool    `json:"risk_atr_enabled"`
	RiskATRMultiplier    float64 `json:"risk_atr_multiplier"`
	RiskATRTimeframe     string  `json:"risk_atr_timeframe"`
	RiskLeverageFallback bool    `json:"risk_leverage_fallback"`
	RiskLeverageMaxLoss  float64 `json:"risk_leverage_max_loss"`
	RiskReentryEnabled   bool    `json:"risk_reentry_enabled"`
	RiskReentryRatio     float64 `json:"risk_reentry_ratio"`
	RiskReentryTolerance float64 `json:"risk_reentry_tolerance"`

	// v3.2 反加仓铁律（详见 store.CopyTradeConfig 同名字段注释）
	RiskReentryBlockAddback     bool    `json:"risk_reentry_block_addback"`
	RiskReentryAddbackTolerance float64 `json:"risk_reentry_addback_tolerance"`
	RiskPolicyVersion           int     `json:"risk_policy_version"`
	RiskStopMode                string  `json:"risk_stop_mode"`
	RiskATRPeriod               int     `json:"risk_atr_period"`
	RiskATRCacheMaxAgeMinutes   int     `json:"risk_atr_cache_max_age_minutes"`
	RiskATRFallbackPct          float64 `json:"risk_atr_fallback_pct"`
	RiskTriggerPriceType        string  `json:"risk_trigger_price_type"`
	RiskSlippageBufferBPS       float64 `json:"risk_slippage_buffer_bps"`
	RiskLiquidationBufferATR    float64 `json:"risk_liquidation_buffer_atr"`
	RiskMaxReentries            int     `json:"risk_max_reentries"`
	RiskReentryBandATR          float64 `json:"risk_reentry_band_atr"`
	RiskReentryCooldownSeconds  int     `json:"risk_reentry_cooldown_seconds"`
	RiskReentryMaxChaseATR      float64 `json:"risk_reentry_max_chase_atr"`
	RiskReentryMaxATRExpansion  float64 `json:"risk_reentry_max_atr_expansion"`
	RiskWatchTimeoutMinutes     int     `json:"risk_watch_timeout_minutes"`
	RiskMigrationConfirmed      bool    `json:"risk_migration_confirmed"`
}

// FillRiskDefaults 兜底默认值（与 store.CopyTradeConfig.FillRiskDefaults 保持一致）
// 调用时机：integration 从 store.CopyTradeConfig 构造 engine.CopyConfig 时
//
// RiskAccountPct 默认 0.5 (=50%) 是用户明确选择的激进配置（v3.1）
// 详见 store.CopyTradeConfig.FillRiskDefaults 的注释
func (c *CopyConfig) FillRiskDefaults() {
	if c.RiskAccountPct == 0 {
		if c.RiskPolicyVersion >= 4 {
			c.RiskAccountPct = 0.02
		} else {
			c.RiskAccountPct = 0.5
		}
	}
	if c.RiskATRMultiplier == 0 {
		c.RiskATRMultiplier = 1.5
	}
	if c.RiskATRTimeframe == "" {
		c.RiskATRTimeframe = "1h"
	}
	if c.RiskLeverageMaxLoss == 0 {
		c.RiskLeverageMaxLoss = 0.5
	}
	if c.RiskReentryRatio == 0 {
		c.RiskReentryRatio = 0.5
	}
	// v3.3：与 store.CopyTradeConfig.FillRiskDefaults 保持一致
	if c.RiskReentryTolerance == 0 {
		c.RiskReentryTolerance = 0.02
	}
	// v3.2 反加仓铁律默认：允许加仓 ≤ 20%
	if c.RiskReentryAddbackTolerance == 0 {
		c.RiskReentryAddbackTolerance = 1.20
	}
	if c.RiskPolicyVersion >= 4 {
		if c.RiskStopMode == "" {
			c.RiskStopMode = "volatility_priority"
		}
		if c.RiskATRPeriod == 0 {
			c.RiskATRPeriod = 14
		}
		if c.RiskATRCacheMaxAgeMinutes == 0 {
			c.RiskATRCacheMaxAgeMinutes = 120
		}
		if c.RiskATRFallbackPct == 0 {
			c.RiskATRFallbackPct = 0.02
		}
		if c.RiskTriggerPriceType == "" {
			c.RiskTriggerPriceType = "mark"
		}
		if c.RiskReentryMaxATRExpansion == 0 {
			c.RiskReentryMaxATRExpansion = 2
		}
	}
}

// Warning 预警记录
type Warning struct {
	Timestamp    time.Time `json:"timestamp"`
	Symbol       string    `json:"symbol"`
	Type         string    `json:"type"` // "low_value" | "high_value" | "insufficient_balance" | etc.
	Message      string    `json:"message"`
	SignalAction string    `json:"signal_action"`
	SignalValue  float64   `json:"signal_value"`
	CopyValue    float64   `json:"copy_value"`
	Executed     bool      `json:"executed"` // 预警不阻止执行，始终为 true
}

// EngineStats 引擎统计
type EngineStats struct {
	SignalsReceived    int64     `json:"signals_received"`
	SignalsFollowed    int64     `json:"signals_followed"`
	SignalsSkipped     int64     `json:"signals_skipped"`
	DecisionsGenerated int64     `json:"decisions_generated"`
	WarningsCount      int64     `json:"warnings_count"`
	LastSignalTime     time.Time `json:"last_signal_time"`
	StartTime          time.Time `json:"start_time"`
}

// ============================================================================
// 风控事件（v3 风控邮件告警机制）
//
// 设计：engine 内部检测到风控事件 → 推入 stopRiskCh channel；
// integration 层消费事件 → 发邮件告警（能拿 trader name，与现有 sendCopyActionAlert 风格一致）
//
// 解耦理由：engine 不依赖 notifier / store.Trader().GetByID，保持引擎纯洁；
// integration 是邮件告警的统一出口（已有 sendCopyActionAlert / 失败告警等路径）
// ============================================================================

// RiskEventType 风控事件类型
type RiskEventType string

const (
	// RiskEventStopLossTriggered 账户保护止损被交易所触发（OKX algo 条件单触发后由对账识别）
	RiskEventStopLossTriggered RiskEventType = "stop_loss_triggered"
	// RiskEventReentryInitiated 二次进场决策已生成（判据 E 满足后）
	// 注意：仅"决策生成"事件，不代表"执行成功"。实际执行成功的告警由 integration 在 executeFullDecision 内发
	RiskEventReentryInitiated RiskEventType = "reentry_initiated"
)

// RiskEvent 风控事件（用于 engine → integration 的告警通知）
type RiskEvent struct {
	Type        RiskEventType
	Timestamp   time.Time
	Symbol      string
	Side        string
	MarginMode  string
	LeaderPosID string

	// SL 触发快照（仅 Type=RiskEventStopLossTriggered 有效）
	LeaderPnL  float64 // SL 触发时领航员该 posId 浮亏（负值）
	LeaderSize float64 // SL 触发时领航员持仓数量
	AddCount   int     // SL 触发时跟单系统已跟随的加仓次数（审计用）

	// 重入快照（仅 Type=RiskEventReentryInitiated 有效）
	ReentryEntryPrice float64 // 重入入场价基准
	ReentrySize       float64 // 重入金额（USDT）
}

// PositionKey 生成仓位的唯一键 (不含保证金模式，向后兼容)
func PositionKey(symbol string, side SideType) string {
	return symbol + "_" + string(side)
}

// PositionKeyWithMode 生成含保证金模式的仓位键 (OKX 全仓/逐仓区分)
// 用于 OKX 交易所，同一币种同一方向的全仓和逐仓是独立仓位
func PositionKeyWithMode(symbol string, side SideType, mgnMode string) string {
	if mgnMode == "" || mgnMode == "cross" {
		// 默认/全仓：使用基础 key (向后兼容)
		return symbol + "_" + string(side)
	}
	// 逐仓：加上模式后缀
	return symbol + "_" + string(side) + "_" + mgnMode
}

// OppositeSide 返回相反方向
func OppositeSide(side SideType) SideType {
	if side == SideLong {
		return SideShort
	}
	return SideLong
}

// GenerateHLPosID 为 Hyperliquid 生成虚拟 posId
// 因为 HL 同一币种只能有一个方向的仓位，用 leaderID+symbol+side 组合作为唯一标识
func GenerateHLPosID(leaderID, symbol string, side SideType) string {
	return "hl_" + leaderID + "_" + symbol + "_" + string(side)
}
