package copytrade

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

	// 减仓/平仓标识（字符串类型 "true"/"false"）
	ReduceOnly    string `json:"reduceOnly,omitempty"`    // 是否只减仓
	ClosePosition string `json:"closePosition,omitempty"` // 是否平仓

	// 止损止盈单相关
	StopPrice    string `json:"stopPrice,omitempty"`    // 触发价格
	WorkingType  string `json:"workingType,omitempty"`  // "MARK_PRICE" | "CONTRACT_PRICE"
	PriceProtect string `json:"priceProtect,omitempty"` // 价格保护

	// 杠杆调整相关
	Leverage string `json:"leverage,omitempty"` // 杠杆倍数

	// 取消订单相关
	OrderId string `json:"orderId,omitempty"` // 订单ID
}

// ============================================================================
// 辅助方法
// ============================================================================

// IsReduceOnly 判断是否只减仓
func (d *AsterDexTxData) IsReduceOnly() bool {
	return d.ReduceOnly == "true"
}

// IsClosePosition 判断是否平仓
func (d *AsterDexTxData) IsClosePosition() bool {
	return d.ClosePosition == "true"
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
//   - closePosition = "true" → ActionClose（全部平仓）
//   - reduceOnly = "true" → ActionReduce（部分减仓）
//   - reduceOnly != "true" && closePosition != "true" → ActionOpen（新开仓/加仓，由 Engine 进一步区分）
func DetermineAsterAction(tx AsterDexTx) ActionType {
	// 1. 平仓优先（closePosition 为 "true"）
	if tx.Data.IsClosePosition() {
		return ActionClose
	}

	// 2. 减仓判断（reduceOnly 为 "true"）
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
