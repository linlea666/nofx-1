package copytrade

// Binance 跟单数据源 Provider
//
// 接口说明：
//   - 使用 Binance 网站私有 web 接口（www.binance.com/bapi/...），非官方 fapi
//   - 官方 API（fapi.binance.com）只能查"自己账户"，无法读其他跟单领航员持仓
//   - 私有 web 接口需要 p20t（登录 cookie）+ csrftoken（CSRF header）双因子认证
//
// 关键约束：
//   - 用户必须先在 binance.com 上跟单了某个领航员，得到 portfolioId 才能用此 provider
//   - p20t 会过期（通常 7-30 天），过期时返回错误码 100001005 / 100002002
//   - 凭证过期由 ErrBinanceCredentialsExpired 通报，engine 端会触发邮件告警
//
// 设计原则：
//   - 与现有 OKX/Hyperliquid Provider 物理隔离，互不依赖
//   - 复用 copytrade 包内的标准结构体 Fill/Position/AccountState
//   - 复用工具函数 parseFloat / normalizeSymbol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"nofx/logger"
)

// Binance Web 跟单专用 API
const (
	// BinanceCopyTradePositionAPI 领航员当前持仓
	BinanceCopyTradePositionAPI = "https://www.binance.com/bapi/futures/v6/private/future/user-data/user-position"

	// BinanceCopyTradeTradeHistoryAPI 领航员成交记录
	BinanceCopyTradeTradeHistoryAPI = "https://www.binance.com/bapi/futures/v1/private/future/copy-trade/copy-portfolio/trade-history"

	// Binance 错误码：未登录/认证失败 → 凭证过期
	BinanceErrNotLogin   = "100001005"
	BinanceErrAuthFailed = "100002002"

	// 跟单类型：固定 COPY（跟随者视角）
	BinanceCopyTradeType = "COPY"

	// 成功码
	BinanceCodeSuccess = "000000"
)

// ErrBinanceCredentialsExpired Binance Web 凭证过期错误
// 上层捕获后应触发邮件告警，提示用户重新粘贴 cURL
var ErrBinanceCredentialsExpired = errors.New("binance web credentials expired or invalid; please update p20t/csrftoken")

// BinanceProvider Binance 跟单数据提供者（轮询模式）
type BinanceProvider struct {
	client    *http.Client
	p20t      string // 登录 cookie
	csrfToken string // CSRF header

	// 内部状态（线程安全）
	mu             sync.Mutex
	leaderUserID   string          // 从持仓接口 id 字段（如 "1239518824_ETHUSDT_LONG"）首次解析获得
	seenFillKeys   map[string]bool // (time|symbol|side|price|qty|posSide) 五元组指纹去重
	seenKeysExpiry time.Time       // 指纹缓存过期时间（>24h 清理）
}

// NewBinanceProvider 创建 Binance 数据源
// p20t/csrfToken 从前端跟单配置传入（用户从浏览器抓取）
func NewBinanceProvider(p20t, csrfToken string) *BinanceProvider {
	return &BinanceProvider{
		client:         &http.Client{Timeout: 15 * time.Second},
		p20t:           strings.TrimSpace(p20t),
		csrfToken:      strings.TrimSpace(csrfToken),
		seenFillKeys:   make(map[string]bool),
		seenKeysExpiry: time.Now().Add(24 * time.Hour),
	}
}

// Type 返回提供者类型
func (p *BinanceProvider) Type() ProviderType {
	return ProviderBinance
}

// ============================================================================
// LeaderProvider 接口实现
// ============================================================================

// GetAccountState 获取领航员账户状态（持仓）
// portfolioID: Binance 跟单关系的 portfolioId（用户在跟单管理页"项目ID"字段获取）
func (p *BinanceProvider) GetAccountState(portfolioID string) (*AccountState, error) {
	raw, err := p.postCopyTrade(BinanceCopyTradePositionAPI, map[string]interface{}{
		"copyTradeType": BinanceCopyTradeType,
		"portfolioId":   portfolioID,
	})
	if err != nil {
		return nil, err
	}

	var resp BinancePositionResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode binance position response: %w; body=%s", err, truncate(string(raw), 200))
	}

	if isBinanceAuthError(resp.Code) {
		logger.Warnf("⚠️ Binance copy-trade credentials expired | code=%s msg=%s portfolioId=%s",
			resp.Code, resp.Message, portfolioID)
		return nil, ErrBinanceCredentialsExpired
	}

	if resp.Code != BinanceCodeSuccess {
		return nil, fmt.Errorf("binance position api error: code=%s msg=%s", resp.Code, resp.Message)
	}

	state := &AccountState{
		Positions: make(map[string]*Position),
		Timestamp: time.Now(),
	}

	// 解析所有持仓
	totalInitialMargin := 0.0
	for _, rp := range resp.Data {
		// 第一次解析时记录 leader userId（用于 Fill 拼装 PosID）
		p.captureLeaderUserID(rp.ID)

		symbol := normalizeSymbol(rp.Symbol)

		// 方向映射：LONG/SHORT 直接映射；BOTH 模式按 positionAmount 正负号判断
		side := mapBinanceSide(rp.PositionSide)
		amount := parseFloat(rp.PositionAmount)
		if side == "" {
			if amount > 0 {
				side = SideLong
			} else if amount < 0 {
				side = SideShort
			} else {
				continue // 空仓
			}
		}

		size := math.Abs(amount)
		if size == 0 {
			continue
		}

		// 杠杆反推：notionalValue / initialMargin（常见 5x/10x/20x/40x/50x/100x 都精确）
		initialMargin := parseFloat(rp.InitialMargin)
		notional := parseFloat(rp.NotionalValue)
		leverage := 0
		if initialMargin > 0 {
			leverage = int(math.Round(notional / initialMargin))
		}
		totalInitialMargin += initialMargin

		// 全/逐仓判定：isolatedWallet/isolatedMargin > 0 即逐仓，否则全仓
		marginMode := "cross"
		if parseFloat(rp.IsolatedWallet) > 0 || parseFloat(rp.IsolatedMargin) > 0 {
			marginMode = "isolated"
		}

		// PosID 直接使用 Binance 返回的 id："{userId}_{symbol}_{positionSide}"
		// 兜底：若空则按相同格式拼装，保证与 Fill 端一致
		posID := rp.ID
		if posID == "" {
			posID = p.buildPosID(symbol, rp.PositionSide)
		}

		state.Positions[posID] = &Position{
			Symbol:        symbol,
			Side:          side,
			Size:          size,
			EntryPrice:    parseFloat(rp.EntryPrice),
			MarkPrice:     parseFloat(rp.MarkPrice),
			Leverage:      leverage,
			MarginMode:    marginMode,
			UnrealizedPnL: parseFloat(rp.UnrealizedProfit),
			PositionValue: notional,
			PosID:         posID,
		}
	}

	// Binance 持仓接口不直接返回账户总权益。
	// 引擎在跟单计算中使用 leader equity 估算比例，对绝对值不敏感，
	// 这里用 sum(initialMargin) 作为下限估算（领航员至少需要这么多保证金）。
	// 真实跟单决策主要看持仓变化，所以这个近似值足够工作。
	state.TotalEquity = totalInitialMargin
	state.AvailableBalance = totalInitialMargin

	return state, nil
}

// GetFills 获取领航员最近成交记录
func (p *BinanceProvider) GetFills(portfolioID string, since time.Time) ([]Fill, error) {
	raw, err := p.postCopyTrade(BinanceCopyTradeTradeHistoryAPI, map[string]interface{}{
		"copyTradeType": BinanceCopyTradeType,
		"portfolioId":   portfolioID,
		"page":          1,
		"rows":          50,
	})
	if err != nil {
		return nil, err
	}

	var resp BinanceTradeHistoryResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode binance trade-history response: %w; body=%s", err, truncate(string(raw), 200))
	}

	if isBinanceAuthError(resp.Code) {
		logger.Warnf("⚠️ Binance copy-trade credentials expired | code=%s msg=%s portfolioId=%s",
			resp.Code, resp.Message, portfolioID)
		return nil, ErrBinanceCredentialsExpired
	}

	if resp.Code != BinanceCodeSuccess {
		return nil, fmt.Errorf("binance trade-history api error: code=%s msg=%s", resp.Code, resp.Message)
	}

	sinceMs := since.UnixMilli()
	var fills []Fill

	p.mu.Lock()
	defer p.mu.Unlock()

	// 定期清理指纹缓存，防止内存累积
	if time.Now().After(p.seenKeysExpiry) {
		p.seenFillKeys = make(map[string]bool)
		p.seenKeysExpiry = time.Now().Add(24 * time.Hour)
	}

	for _, tr := range resp.Data.List {
		// 时间过滤
		if tr.Time < sinceMs {
			continue
		}

		// 五元组指纹去重（Binance 成交无 fillId/orderId）
		fp := fillFingerprint(tr)
		if p.seenFillKeys[fp] {
			continue
		}
		p.seenFillKeys[fp] = true

		symbol := normalizeSymbol(tr.Symbol)
		side := strings.ToLower(tr.Side) // BUY -> buy, SELL -> sell
		posSide := mapBinanceSide(tr.PositionSide)
		// BOTH 模式兜底：根据 side 推断（虽然罕见）
		if posSide == "" {
			if side == "buy" {
				posSide = SideLong
			} else {
				posSide = SideShort
			}
		}

		fill := Fill{
			ID:           fp, // 用指纹作为唯一 ID
			Symbol:       symbol,
			Side:         side,
			PositionSide: posSide,
			Price:        tr.Price,
			Size:         tr.Qty,
			Value:        tr.Quantity, // Binance "quantity" 字段 = 成交额 USDT
			Timestamp:    time.UnixMilli(tr.Time),
			ClosedPnL:    tr.RealizedProfit,
			Raw:          tr,
		}

		// 推断 Action：realizedProfit != 0 一定是平仓/减仓
		fill.Action = guessBinanceAction(side, posSide, tr.RealizedProfit)

		fills = append(fills, fill)
	}

	return fills, nil
}

// ============================================================================
// 内部方法
// ============================================================================

// postCopyTrade 发送 Binance 跟单专用 POST 请求
//
// 必需 header：
//   - content-type: application/json
//   - csrftoken: <csrftoken>
//   - clienttype: web
//
// 必需 cookie：
//   - p20t: <p20t value>
func (p *BinanceProvider) postCopyTrade(url string, body interface{}) ([]byte, error) {
	if p.p20t == "" || p.csrfToken == "" {
		return nil, fmt.Errorf("binance credentials not configured (p20t/csrftoken empty)")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("csrftoken", p.csrfToken)
	req.Header.Set("clienttype", "web")
	req.Header.Set("user-agent", "Mozilla/5.0 (compatible; NOFX/1.0)")
	req.Header.Set("accept", "*/*")
	req.AddCookie(&http.Cookie{Name: "p20t", Value: p.p20t})

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binance request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("binance read body failed: %w", err)
	}

	// 200 + code=100001005 也是凭证错误，让上层用 code 判定（已 read body 完毕）
	if resp.StatusCode == http.StatusUnauthorized {
		// 上层会进一步用 resp.Code 判定，先返回 raw 让 JSON 解析正常走
		return rawBody, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binance http %d: %s", resp.StatusCode, truncate(string(rawBody), 200))
	}

	return rawBody, nil
}

// captureLeaderUserID 从持仓 id 字段首次解析 leader userId
// id 形式："1239518824_ETHUSDT_LONG"
func (p *BinanceProvider) captureLeaderUserID(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.leaderUserID != "" {
		return
	}
	parts := strings.SplitN(id, "_", 2)
	if len(parts) >= 1 && parts[0] != "" {
		p.leaderUserID = parts[0]
	}
}

// buildPosID 兜底拼装 PosID（持仓接口未返回 id 时使用）
// 格式与 Binance 原生 id 保持一致：{userId}_{SYMBOL}_{POSSIDE_UPPER}
func (p *BinanceProvider) buildPosID(symbol, positionSide string) string {
	p.mu.Lock()
	userID := p.leaderUserID
	p.mu.Unlock()
	if userID == "" {
		userID = "unknown"
	}
	return userID + "_" + symbol + "_" + strings.ToUpper(positionSide)
}

// ============================================================================
// 工具函数
// ============================================================================

// isBinanceAuthError 判断是否为认证错误（凭证过期）
func isBinanceAuthError(code string) bool {
	return code == BinanceErrNotLogin || code == BinanceErrAuthFailed
}

// mapBinanceSide 将 Binance positionSide 映射到 SideType
// LONG -> long, SHORT -> short, BOTH/其他 -> "" (caller 应用兜底逻辑)
func mapBinanceSide(positionSide string) SideType {
	switch strings.ToUpper(positionSide) {
	case "LONG":
		return SideLong
	case "SHORT":
		return SideShort
	}
	return ""
}

// guessBinanceAction 根据 side+positionSide+realizedProfit 推断动作类型
// Binance 成交记录没有 startPosition 字段，无法精确判定 open vs add / close vs reduce
// 引擎层会用 lastKnownSize 进一步精确判断
func guessBinanceAction(side string, posSide SideType, realized float64) ActionType {
	// realizedProfit 非零 → 一定是平仓/减仓
	if realized != 0 {
		if posSide == SideLong && side == "sell" {
			return ActionClose
		}
		if posSide == SideShort && side == "buy" {
			return ActionClose
		}
	}
	// 否则默认为开仓（引擎会进一步区分 open/add）
	return ActionOpen
}

// fillFingerprint 五元组指纹（用于 Fill 去重）
// Binance 成交无唯一 id，组合 time+symbol+side+price+qty+positionSide 作为去重 key
func fillFingerprint(tr BinanceTradeRecord) string {
	return fmt.Sprintf("%d|%s|%s|%.8f|%.8f|%s",
		tr.Time, tr.Symbol, tr.Side, tr.Price, tr.Qty, tr.PositionSide)
}

// truncate 截断字符串（用于错误日志）
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ============================================================================
// API 响应结构
// ============================================================================

// BinancePositionResp /bapi/futures/v6/private/future/user-data/user-position 响应
type BinancePositionResp struct {
	Code    string               `json:"code"`
	Message string               `json:"message"`
	Success bool                 `json:"success"`
	Data    []BinancePositionRaw `json:"data"`
}

// BinancePositionRaw 单条领航员持仓原始数据
// 字段命名严格对应 Binance 返回 JSON
type BinancePositionRaw struct {
	ID                     string `json:"id"`                     // "{userId}_{symbol}_{positionSide}"
	Symbol                 string `json:"symbol"`                 // "ETHUSDT"
	PositionSide           string `json:"positionSide"`           // "LONG"/"SHORT"/"BOTH"
	PositionAmount         string `json:"positionAmount"`         // 持仓数量（币本位，可带负号表示空头 BOTH 模式）
	EntryPrice             string `json:"entryPrice"`             // 开仓均价
	BreakEvenPrice         string `json:"breakEvenPrice"`         // 保本价
	MarkPrice              string `json:"markPrice"`              // 标记价
	UnrealizedProfit       string `json:"unrealizedProfit"`       // 未实现盈亏
	LiquidationPrice       string `json:"liquidationPrice"`       // 强平价（"0" 表示无）
	IsolatedMargin         string `json:"isolatedMargin"`         // 逐仓保证金（>0 即逐仓）
	NotionalValue          string `json:"notionalValue"`          // 名义价值（USDT）
	Collateral             string `json:"collateral"`             // 保证金币种 ("USDT")
	IsolatedWallet         string `json:"isolatedWallet"`         // 逐仓钱包余额（>0 即逐仓）
	CumRealized            string `json:"cumRealized"`            // 累计已实现盈亏
	InitialMargin          string `json:"initialMargin"`          // 当前占用保证金（用于反推杠杆）
	MaintMargin            string `json:"maintMargin"`            // 维持保证金
	PositionInitialMargin  string `json:"positionInitialMargin"`  // 持仓初始保证金
	OpenOrderInitialMargin string `json:"openOrderInitialMargin"` // 挂单初始保证金
	Adl                    int    `json:"adl"`                    // 自动减仓队列等级
	AskNotional            string `json:"askNotional"`            // 卖盘名义价值
	BidNotional            string `json:"bidNotional"`            // 买盘名义价值
	UpdateTime             int64  `json:"updateTime"`             // 持仓更新时间（毫秒）
}

// BinanceTradeHistoryResp /bapi/futures/v1/private/future/copy-trade/copy-portfolio/trade-history 响应
type BinanceTradeHistoryResp struct {
	Code    string                  `json:"code"`
	Message string                  `json:"message"`
	Success bool                    `json:"success"`
	Data    BinanceTradeHistoryData `json:"data"`
}

// BinanceTradeHistoryData trade-history 数据载荷
type BinanceTradeHistoryData struct {
	IndexValue string               `json:"indexValue"`
	Total      int                  `json:"total"`
	List       []BinanceTradeRecord `json:"list"`
}

// BinanceTradeRecord 单条成交记录
// 注意：price/qty/fee 等字段 Binance 返回为 number 而非 string
type BinanceTradeRecord struct {
	Time                int64   `json:"time"`                // 成交时间（毫秒）
	Symbol              string  `json:"symbol"`              // "ETHUSDT"
	Side                string  `json:"side"`                // "BUY"/"SELL"
	Price               float64 `json:"price"`               // 成交价
	Fee                 float64 `json:"fee"`                 // 手续费（负数=支出）
	FeeAsset            string  `json:"feeAsset"`            // "USDT"
	Quantity            float64 `json:"quantity"`            // 成交额（USDT，= price * qty）
	QuantityAsset       string  `json:"quantityAsset"`       // 报价币种
	RealizedProfit      float64 `json:"realizedProfit"`      // 已实现盈亏（非零=平仓/减仓）
	RealizedProfitAsset string  `json:"realizedProfitAsset"` // 盈亏币种
	BaseAsset           string  `json:"baseAsset"`           // "ETH"
	Qty                 float64 `json:"qty"`                 // 成交数量（币本位）
	PositionSide        string  `json:"positionSide"`        // "LONG"/"SHORT"/"BOTH"
	ActiveBuy           bool    `json:"activeBuy"`           // 是否主动买入（taker）
}
