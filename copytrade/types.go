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
	PosID         string   // OKX 仓位唯一标识（用于精确匹配）
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
}

// Warning 预警记录
type Warning struct {
	Timestamp    time.Time `json:"timestamp"`
	Symbol       string    `json:"symbol"`
	Type         string    `json:"type"`    // "low_value" | "high_value" | "insufficient_balance" | etc.
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

