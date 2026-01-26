package copytrade

import (
	"encoding/json"
	"strings"
)

// ============================================================================
// 自定义类型：处理 API 返回的布尔值/字符串不一致问题
// ============================================================================

// FlexBool 灵活布尔类型
// AsterDex API 有时返回 true/false（布尔），有时返回 "true"/"false"（字符串）
// 此类型自动处理两种情况
type FlexBool bool

// UnmarshalJSON 实现 json.Unmarshaler 接口
func (fb *FlexBool) UnmarshalJSON(data []byte) error {
	// 去除首尾空白
	s := strings.TrimSpace(string(data))

	// 情况1：布尔值 true/false
	if s == "true" {
		*fb = true
		return nil
	}
	if s == "false" {
		*fb = false
		return nil
	}

	// 情况2：字符串 "true"/"false"
	if s == `"true"` {
		*fb = true
		return nil
	}
	if s == `"false"` || s == `""` {
		*fb = false
		return nil
	}

	// 情况3：null 或空
	if s == "null" || s == "" {
		*fb = false
		return nil
	}

	// 兜底：尝试标准 JSON 解析
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		*fb = FlexBool(b)
		return nil
	}

	// 最终兜底：默认 false
	*fb = false
	return nil
}

// Bool 返回原生 bool 值
func (fb FlexBool) Bool() bool {
	return bool(fb)
}

// ============================================================================
// AsterDex API 返回结构
// ============================================================================

// AsterDexUserDetailsResp explorer API 错误返回结构
// API: https://explorer.asterdex-testnet.com/explorer
type AsterDexUserDetailsResp struct {
	Status    string `json:"status"`    // "ERROR"
	Type      string `json:"type"`      // "SYS"
	Code      string `json:"code"`      // 错误码 (如 "000002", "100001001")
	ErrorData string `json:"errorData"` // 错误描述
}

// AsterDexUserDetails 用户详情（成功响应）
type AsterDexUserDetails struct {
	Balance AsterDexBalance `json:"balance"`
	Type    string          `json:"type"` // "userDetails"
	Txs     []AsterDexTx    `json:"txs"`  // 交易记录列表
}

// AsterDexBalance 账户余额
type AsterDexBalance struct {
	SpotBalance   float64 `json:"spotBalance"`   // 现货余额
	MarginBalance float64 `json:"marginBalance"` // 保证金余额（总权益）
}

// AsterDexTx 交易记录（顶层结构）
type AsterDexTx struct {
	BlockNumber int64          `json:"blockNumber"` // 区块号
	Hash        string         `json:"hash"`        // 交易哈希（唯一标识）
	Data        AsterDexTxData `json:"data"`        // 交易详情（嵌套对象）
	User        string         `json:"user"`        // 用户地址
	Action      string         `json:"action"`      // "PlaceOrder" | "CancelOrder" | "AdjustLeverage" | "ChangePositionSide" | "ApproveAgent"
	BizType     string         `json:"bizType"`     // "PERP" | "AGENT"
	Error       string         `json:"error"`       // 错误信息（空字符串表示成功）
	Timestamp   int64          `json:"timestamp"`   // 交易时间戳（毫秒）
}

// AsterDexTxData 交易详情（嵌套在 data 字段中）
type AsterDexTxData struct {
	NewClientOrderId string `json:"newClientOrderId,omitempty"` // 客户端订单ID
	Symbol           string `json:"symbol,omitempty"`           // 交易对 (如 "ETHUSDT")
	Side             string `json:"side,omitempty"`             // "BUY" | "SELL"
	Quantity         string `json:"quantity,omitempty"`         // 数量
	Price            string `json:"price,omitempty"`            // 价格
	PositionSide     string `json:"positionSide,omitempty"`     // "LONG" | "SHORT" | "BOTH"
	Type             string `json:"type,omitempty"`             // "MARKET" | "LIMIT" | "STOP_MARKET" | "TAKE_PROFIT_MARKET" | "STOP" | "TAKE_PROFIT"
	TimeInForce      string `json:"timeInForce,omitempty"`      // "GTC" | "IOC" | "FOK" | "GTE_GTC"
	Timestamp        string `json:"timestamp,omitempty"`        // 订单时间戳（字符串）

	// 减仓/平仓标识（使用 FlexBool 处理 bool/string 混合类型）
	ReduceOnly    FlexBool `json:"reduceOnly,omitempty"`    // 是否只减仓
	ClosePosition FlexBool `json:"closePosition,omitempty"` // 是否平仓

	// 止损止盈单相关
	StopPrice    string `json:"stopPrice,omitempty"`    // 触发价格
	WorkingType  string `json:"workingType,omitempty"`  // "MARK_PRICE" | "CONTRACT_PRICE"
	PriceProtect string `json:"priceProtect,omitempty"` // 价格保护

	// 杠杆调整相关
	Leverage string `json:"leverage,omitempty"` // 杠杆倍数

	// 取消订单相关
	OrderId string `json:"orderId,omitempty"` // 订单ID

	// 双向持仓模式切换
	DualSidePosition string `json:"dualSidePosition,omitempty"` // "true"/"false"
}

// ============================================================================
// 辅助方法
// ============================================================================

// IsReduceOnly 判断是否只减仓
func (d *AsterDexTxData) IsReduceOnly() bool {
	return d.ReduceOnly.Bool()
}

// IsClosePosition 判断是否平仓
func (d *AsterDexTxData) IsClosePosition() bool {
	return d.ClosePosition.Bool()
}

// IsValidOrder 判断是否是有效的交易订单（有 symbol 和 side）
func (tx *AsterDexTx) IsValidOrder() bool {
	return tx.Data.Symbol != "" && tx.Data.Side != ""
}

// IsSuccessfulOrder 判断是否是成功下单
func (tx *AsterDexTx) IsSuccessfulOrder() bool {
	return tx.Action == "PlaceOrder" && tx.Error == "" && tx.IsValidOrder()
}

// ============================================================================
// Action 判断逻辑
// ============================================================================

// DetermineAsterAction 根据 AsterDex 交易记录判断 Action 类型
// 判断规则：
//   - closePosition = true → ActionClose（全部平仓）
//   - reduceOnly = true → ActionReduce（部分减仓）
//   - 其他 → ActionOpen（新开仓/加仓，由 Engine 进一步区分）
func DetermineAsterAction(tx AsterDexTx) ActionType {
	// 1. 平仓优先（closePosition 为 true）
	if tx.Data.IsClosePosition() {
		return ActionClose
	}

	// 2. 减仓判断（reduceOnly 为 true）
	if tx.Data.IsReduceOnly() {
		return ActionReduce
	}

	// 3. 开仓/加仓（由 Engine 根据映射表区分）
	return ActionOpen
}

// DetermineAsterPositionSide 根据交易记录判断持仓方向
// 返回标准化的 SideType
func DetermineAsterPositionSide(tx AsterDexTx) SideType {
	switch tx.Data.PositionSide {
	case "LONG":
		return SideLong
	case "SHORT":
		return SideShort
	case "BOTH":
		// BOTH 模式：根据 side 和 reduceOnly 推断
		if tx.Data.Side == "BUY" {
			if tx.Data.IsReduceOnly() {
				return SideShort // 买入减仓 = 平空
			}
			return SideLong // 买入开仓 = 开多
		}
		// SELL
		if tx.Data.IsReduceOnly() {
			return SideLong // 卖出减仓 = 平多
		}
		return SideShort // 卖出开仓 = 开空
	default:
		// 兜底：根据 side 判断
		if tx.Data.Side == "BUY" {
			return SideLong
		}
		return SideShort
	}
}

// DetermineAsterTradeSide 根据交易记录判断交易方向
// 返回标准化的 "buy" | "sell"
func DetermineAsterTradeSide(tx AsterDexTx) string {
	if tx.Data.Side == "BUY" {
		return "buy"
	}
	return "sell"
}

// GenerateAsterPosID 为 AsterDex 生成虚拟 posId
// 与 Hyperliquid 类似，使用 symbol + positionSide 组合作为唯一标识
func GenerateAsterPosID(symbol string, side SideType) string {
	return symbol + "_" + string(side)
}
