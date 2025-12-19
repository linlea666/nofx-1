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
)

// ActionType 交易动作类型
type ActionType string

const (
	ActionOpen   ActionType = "open"   // 开仓
	ActionClose  ActionType = "close"  // 平仓
	ActionAdd    ActionType = "add"    // 加仓
	ActionReduce ActionType = "reduce" // 减仓
)

// SideType 持仓方向
type SideType string

const (
	SideLong  SideType = "long"
	SideShort SideType = "short"
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
	ProviderType   ProviderType `json:"provider_type"`    // "hyperliquid" | "okx"
	LeaderID       string       `json:"leader_id"`        // 领航员地址/uniqueName
	CopyRatio      float64      `json:"copy_ratio"`       // 跟单系数 (1.0 = 100%)
	SyncLeverage   bool         `json:"sync_leverage"`    // 同步杠杆
	SyncMarginMode bool         `json:"sync_margin_mode"` // 同步保证金模式

	// 预警阈值（不限制，只记录预警）
	MinTradeWarn float64 `json:"min_trade_warn"` // 低于此金额记录预警
	MaxTradeWarn float64 `json:"max_trade_warn"` // 高于此金额记录预警 (0=不预警)
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

