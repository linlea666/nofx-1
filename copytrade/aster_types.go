package copytrade

// ============================================================================
// AsterDex API 返回结构
// ============================================================================

// AsterDexUserDetailsResp explorer API 返回结构
// API: https://explorer.asterdex-testnet.com/explorer
type AsterDexUserDetailsResp struct {
	Status    string                `json:"status"`
	Type      string                `json:"type"`
	Code      string                `json:"code"`
	ErrorData string                `json:"errorData,omitempty"`
	Data      *AsterDexUserDetails  `json:"data"`
}

// AsterDexUserDetails 用户详情
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

// AsterDexTx 交易记录
type AsterDexTx struct {
	TxHash        string `json:"txHash"`        // 交易哈希（唯一标识）
	Symbol        string `json:"symbol"`        // 交易对 (如 "ETHUSDT")
	Side          string `json:"side"`          // "BUY" | "SELL"
	PositionSide  string `json:"positionSide"`  // "LONG" | "SHORT" | "BOTH"
	Type          string `json:"type"`          // "MARKET" | "LIMIT" | "STOP_MARKET" | "TAKE_PROFIT_MARKET" | "STOP" | "TAKE_PROFIT"
	Quantity      string `json:"quantity"`      // 数量
	Price         string `json:"price"`         // 价格
	ReduceOnly    bool   `json:"reduceOnly"`    // 是否只减仓（关键字段）
	ClosePosition bool   `json:"closePosition"` // 是否平仓（关键字段）
	Status        string `json:"status"`        // "FILLED" | "NEW" | "CANCELED" | "PARTIALLY_FILLED"
	CreateTime    int64  `json:"createTime"`    // 创建时间戳（毫秒）
	UpdateTime    int64  `json:"updateTime"`    // 更新时间戳（毫秒）

	// 可选字段（部分接口返回）
	OrigQuantity     string `json:"origQuantity,omitempty"`     // 原始数量
	ExecutedQuantity string `json:"executedQuantity,omitempty"` // 已执行数量
	AvgPrice         string `json:"avgPrice,omitempty"`         // 成交均价
	StopPrice        string `json:"stopPrice,omitempty"`        // 触发价格（止损/止盈单）
	WorkingType      string `json:"workingType,omitempty"`      // "MARK_PRICE" | "CONTRACT_PRICE"
	TimeInForce      string `json:"timeInForce,omitempty"`      // "GTC" | "IOC" | "FOK"
}

// ============================================================================
// Action 判断逻辑
// ============================================================================

// DetermineAsterAction 根据 AsterDex 交易记录判断 Action 类型
// 判断规则：
//   - closePosition = true → ActionClose（全部平仓）
//   - reduceOnly = true → ActionReduce（部分减仓）
//   - reduceOnly = false && closePosition = false → ActionOpen（新开仓/加仓，由 Engine 进一步区分）
func DetermineAsterAction(tx AsterDexTx) ActionType {
	// 1. 平仓优先（closePosition 为 true）
	if tx.ClosePosition {
		return ActionClose
	}

	// 2. 减仓判断（reduceOnly 为 true）
	if tx.ReduceOnly {
		return ActionReduce
	}

	// 3. 开仓/加仓（由 Engine 根据映射表区分）
	return ActionOpen
}

// DetermineAsterPositionSide 根据交易记录判断持仓方向
// 返回标准化的 SideType
func DetermineAsterPositionSide(tx AsterDexTx) SideType {
	switch tx.PositionSide {
	case "LONG":
		return SideLong
	case "SHORT":
		return SideShort
	case "BOTH":
		// BOTH 模式：根据 side 和 reduceOnly 推断
		if tx.Side == "BUY" {
			if tx.ReduceOnly {
				return SideShort // 买入减仓 = 平空
			}
			return SideLong // 买入开仓 = 开多
		}
		// SELL
		if tx.ReduceOnly {
			return SideLong // 卖出减仓 = 平多
		}
		return SideShort // 卖出开仓 = 开空
	default:
		// 兜底：根据 side 判断
		if tx.Side == "BUY" {
			return SideLong
		}
		return SideShort
	}
}

// DetermineAsterTradeSide 根据交易记录判断交易方向
// 返回标准化的 "buy" | "sell"
func DetermineAsterTradeSide(tx AsterDexTx) string {
	if tx.Side == "BUY" {
		return "buy"
	}
	return "sell"
}

// GenerateAsterPosID 为 AsterDex 生成虚拟 posId
// 与 Hyperliquid 类似，使用 symbol + positionSide 组合作为唯一标识
func GenerateAsterPosID(symbol string, side SideType) string {
	return symbol + "_" + string(side)
}
