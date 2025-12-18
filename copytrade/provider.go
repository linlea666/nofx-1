package copytrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nofx/logger"
)

// ============================================================================
// Provider 接口
// ============================================================================

// LeaderProvider 领航员数据提供者接口
type LeaderProvider interface {
	// GetFills 获取最近成交记录
	GetFills(leaderID string, since time.Time) ([]Fill, error)

	// GetAccountState 获取账户状态（资产 + 持仓）
	GetAccountState(leaderID string) (*AccountState, error)

	// Type 返回提供者类型
	Type() ProviderType
}

// StreamingProvider 流式数据提供者接口（支持 WebSocket 推送）
// 继承 LeaderProvider，额外支持事件驱动模式
type StreamingProvider interface {
	LeaderProvider

	// SetOnFill 设置成交回调（收到领航员成交时触发）
	SetOnFill(callback func(Fill))

	// SetOnStateUpdate 设置状态更新回调（持仓变化时触发）
	SetOnStateUpdate(callback func(*AccountState))

	// Connect 连接并开始订阅指定领航员
	Connect(leaderID string) error

	// Close 关闭连接
	Close() error

	// IsStreaming 是否为流式 Provider
	IsStreaming() bool
}

// NewProvider 创建 Provider（REST 轮询模式）
func NewProvider(providerType ProviderType) (LeaderProvider, error) {
	switch providerType {
	case ProviderHyperliquid:
		return NewHyperliquidProvider(), nil
	case ProviderOKX:
		return NewOKXProvider(), nil
	default:
		return nil, fmt.Errorf("unsupported provider type: %s", providerType)
	}
}

// NewStreamingProvider 创建流式 Provider（WebSocket 事件驱动模式）
// 目前只有 Hyperliquid 支持
func NewStreamingProvider(providerType ProviderType) (StreamingProvider, error) {
	switch providerType {
	case ProviderHyperliquid:
		return NewHLWebSocketProvider(), nil
	default:
		return nil, fmt.Errorf("provider %s does not support streaming mode", providerType)
	}
}

// ============================================================================
// Hyperliquid Provider
// ============================================================================

const (
	HLInfoAPI = "https://api.hyperliquid.xyz/info"
)

// HyperliquidProvider Hyperliquid 数据提供者
type HyperliquidProvider struct {
	client *http.Client
}

// NewHyperliquidProvider 创建 Hyperliquid Provider
func NewHyperliquidProvider() *HyperliquidProvider {
	return &HyperliquidProvider{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *HyperliquidProvider) Type() ProviderType {
	return ProviderHyperliquid
}

// GetFills 获取成交记录
func (p *HyperliquidProvider) GetFills(leaderID string, since time.Time) ([]Fill, error) {
	req := map[string]string{
		"type": "userFills",
		"user": leaderID,
	}

	var rawFills []HLFillRaw
	if err := p.post(req, &rawFills); err != nil {
		return nil, fmt.Errorf("get fills failed: %w", err)
	}

	var fills []Fill
	for _, raw := range rawFills {
		ts := time.UnixMilli(raw.Time)
		if ts.Before(since) {
			continue
		}

		fill := Fill{
			ID:        fmt.Sprintf("%d", raw.TID),
			Symbol:    normalizeSymbol(raw.Coin),
			Price:     parseFloat(raw.Px),
			Size:      parseFloat(raw.Sz),
			Timestamp: ts,
			ClosedPnL: parseFloat(raw.ClosedPnl),
			Raw:       raw,
		}

		// 解析方向
		fill.Side, fill.PositionSide, fill.Action = parseHLDirection(raw.Side, raw.Dir, raw.StartPosition)

		// 计算成交价值
		fill.Value = fill.Price * fill.Size

		fills = append(fills, fill)
	}

	return fills, nil
}

// GetAccountState 获取账户状态
func (p *HyperliquidProvider) GetAccountState(leaderID string) (*AccountState, error) {
	req := map[string]string{
		"type": "clearinghouseState",
		"user": leaderID,
	}

	var raw HLClearinghouseState
	if err := p.post(req, &raw); err != nil {
		return nil, fmt.Errorf("get account state failed: %w", err)
	}

	state := &AccountState{
		TotalEquity:      parseFloat(raw.MarginSummary.AccountValue),
		AvailableBalance: parseFloat(raw.Withdrawable),
		Positions:        make(map[string]*Position),
		Timestamp:        time.UnixMilli(raw.Time),
	}

	// 解析持仓
	for _, ap := range raw.AssetPositions {
		pos := ap.Position
		symbol := normalizeSymbol(pos.Coin)

		size := parseFloat(pos.Szi)
		side := SideLong
		if size < 0 {
			side = SideShort
			size = -size
		}

		if size == 0 {
			continue // 跳过空仓位
		}

		key := PositionKey(symbol, side)
		state.Positions[key] = &Position{
			Symbol:        symbol,
			Side:          side,
			Size:          size,
			EntryPrice:    parseFloat(pos.EntryPx),
			Leverage:      pos.Leverage.Value,
			MarginMode:    pos.Leverage.Type,
			UnrealizedPnL: parseFloat(pos.UnrealizedPnl),
			PositionValue: parseFloat(pos.PositionValue),
		}
	}

	return state, nil
}

func (p *HyperliquidProvider) post(req interface{}, result interface{}) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	resp, err := p.client.Post(HLInfoAPI, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

// parseHLDirection 解析 Hyperliquid 的交易方向
func parseHLDirection(side, dir, startPosition string) (tradeSide string, posSide SideType, action ActionType) {
	startPos := parseFloat(startPosition)

	switch dir {
	case "Open Long":
		if startPos == 0 {
			return "buy", SideLong, ActionOpen
		}
		return "buy", SideLong, ActionAdd
	case "Close Long":
		return "sell", SideLong, ActionClose // 具体是 reduce 还是 close 由 engine 判断
	case "Open Short":
		if startPos == 0 {
			return "sell", SideShort, ActionOpen
		}
		return "sell", SideShort, ActionAdd
	case "Close Short":
		return "buy", SideShort, ActionClose // 具体是 reduce 还是 close 由 engine 判断

	// 🔄 反向开仓处理（Hyperliquid 特有）
	// 反向开仓 = 平掉原仓位 + 开新方向仓位（一次交易完成）
	// 处理策略：将新方向视为新开仓，如果我们没有原仓位则直接跟随新方向
	case "Long > Short":
		// 从多翻空：新方向是 Short，当作新开仓处理
		return "sell", SideShort, ActionOpen
	case "Short > Long":
		// 从空翻多：新方向是 Long，当作新开仓处理
		return "buy", SideLong, ActionOpen

	default:
		// 兜底
		if side == "B" {
			return "buy", SideLong, ActionAdd
		}
		return "sell", SideShort, ActionAdd
	}
}

// ============================================================================
// OKX Provider
// ============================================================================

const (
	OKXTradeRecordsAPI = "https://www.okx.com/priapi/v5/ecotrade/public/community/user/trade-records"
	OKXAssetAPI        = "https://www.okx.com/priapi/v5/ecotrade/public/community/user/asset"
	OKXPositionAPI     = "https://www.okx.com/priapi/v5/ecotrade/public/community/user/position-current"
)

// OKXProvider OKX 数据提供者
type OKXProvider struct {
	client *http.Client
}

// NewOKXProvider 创建 OKX Provider
func NewOKXProvider() *OKXProvider {
	return &OKXProvider{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *OKXProvider) Type() ProviderType {
	return ProviderOKX
}

// GetFills 获取成交记录
func (p *OKXProvider) GetFills(uniqueName string, since time.Time) ([]Fill, error) {
	now := time.Now()
	url := fmt.Sprintf(
		"%s?uniqueName=%s&startModify=%d&endModify=%d&instType=SWAP&limit=50&t=%d",
		OKXTradeRecordsAPI,
		uniqueName,
		since.UnixMilli(),
		now.UnixMilli(),
		now.UnixMilli(),
	)

	var resp OKXTradeRecordsResp
	if err := p.get(url, &resp); err != nil {
		return nil, err
	}

	if resp.Code != "0" {
		return nil, fmt.Errorf("OKX API error: %s", resp.Msg)
	}

	var fills []Fill
	for _, raw := range resp.Data {
		fill := Fill{
			ID:        raw.OrdId,
			Symbol:    normalizeOKXSymbol(raw.InstId),
			Price:     parseFloat(raw.AvgPx),
			Size:      parseFloat(raw.Sz),
			Value:     parseFloat(raw.Value),
			Timestamp: time.UnixMilli(parseInt64(raw.FillTime)),
			Raw:       raw,
		}

		// 解析方向
		fill.Side, fill.PositionSide, fill.Action = parseOKXDirection(raw.Side, raw.PosSide)

		fills = append(fills, fill)
	}

	return fills, nil
}

// GetAccountState 获取账户状态
func (p *OKXProvider) GetAccountState(uniqueName string) (*AccountState, error) {
	now := time.Now().UnixMilli()

	// 1. 获取资产
	assetURL := fmt.Sprintf("%s?uniqueName=%s&t=%d", OKXAssetAPI, uniqueName, now)
	var assetResp OKXAssetResp
	if err := p.get(assetURL, &assetResp); err != nil {
		return nil, err
	}

	// 2. 获取持仓
	posURL := fmt.Sprintf("%s?uniqueName=%s&t=%d", OKXPositionAPI, uniqueName, now)
	var posResp OKXPositionResp
	if err := p.get(posURL, &posResp); err != nil {
		return nil, err
	}

	state := &AccountState{
		Positions: make(map[string]*Position),
		Timestamp: time.Now(),
	}

	// 解析资产（USDT 为总权益）
	for _, asset := range assetResp.Data {
		if asset.Currency == "USDT" {
			state.TotalEquity = parseFloat(asset.Amount)
			state.AvailableBalance = state.TotalEquity
			break
		}
	}

	// 解析持仓
	for _, pd := range posResp.Data {
		for _, pos := range pd.PosData {
			symbol := normalizeOKXSymbol(pos.InstId)
			side := SideType(pos.PosSide)

			key := PositionKey(symbol, side)
			state.Positions[key] = &Position{
				Symbol:        symbol,
				Side:          side,
				Size:          parseFloat(pos.Pos),
				EntryPrice:    parseFloat(pos.AvgPx),
				MarkPrice:     parseFloat(pos.MarkPx),
				Leverage:      parseInt(pos.Lever),
				MarginMode:    pos.MgnMode,
				UnrealizedPnL: parseFloat(pos.Upl),
				PositionValue: parseFloat(pos.NotionalUsd),
			}
		}
	}

	return state, nil
}

func (p *OKXProvider) get(url string, result interface{}) error {
	resp, err := p.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

// parseOKXDirection 解析 OKX 交易方向
func parseOKXDirection(side, posSide string) (tradeSide string, positionSide SideType, action ActionType) {
	positionSide = SideType(posSide)

	// OKX: side = "buy" | "sell", posSide = "long" | "short"
	if side == "buy" && posSide == "long" {
		return "buy", SideLong, ActionOpen // 或 add，由 engine 判断
	} else if side == "sell" && posSide == "long" {
		return "sell", SideLong, ActionClose // 或 reduce
	} else if side == "sell" && posSide == "short" {
		return "sell", SideShort, ActionOpen // 或 add
	} else if side == "buy" && posSide == "short" {
		return "buy", SideShort, ActionClose // 或 reduce
	}

	return side, positionSide, ActionOpen
}

// ============================================================================
// API 返回结构（Hyperliquid）
// ============================================================================

// HLFillRaw userFills 返回结构
type HLFillRaw struct {
	Coin          string `json:"coin"`
	Px            string `json:"px"`
	Sz            string `json:"sz"`
	Side          string `json:"side"` // "B" | "A"
	Time          int64  `json:"time"`
	StartPosition string `json:"startPosition"`
	Dir           string `json:"dir"` // "Open Long" | "Close Short" | ...
	ClosedPnl     string `json:"closedPnl"`
	Hash          string `json:"hash"`
	Oid           int64  `json:"oid"`
	TID           int64  `json:"tid"`
	FeeToken      string `json:"feeToken"`
}

// HLClearinghouseState clearinghouseState 返回结构
type HLClearinghouseState struct {
	MarginSummary struct {
		AccountValue    string `json:"accountValue"`
		TotalNtlPos     string `json:"totalNtlPos"`
		TotalRawUsd     string `json:"totalRawUsd"`
		TotalMarginUsed string `json:"totalMarginUsed"`
	} `json:"marginSummary"`
	Withdrawable   string `json:"withdrawable"`
	AssetPositions []struct {
		Type     string `json:"type"`
		Position struct {
			Coin          string `json:"coin"`
			Szi           string `json:"szi"`
			Leverage      struct {
				Type  string `json:"type"`
				Value int    `json:"value"`
			} `json:"leverage"`
			EntryPx       string `json:"entryPx"`
			PositionValue string `json:"positionValue"`
			UnrealizedPnl string `json:"unrealizedPnl"`
			LiquidationPx string `json:"liquidationPx,omitempty"`
			MarginUsed    string `json:"marginUsed"`
		} `json:"position"`
	} `json:"assetPositions"`
	Time int64 `json:"time"`
}

// ============================================================================
// API 返回结构（OKX）
// ============================================================================

// OKXTradeRecordsResp trade-records 返回结构
type OKXTradeRecordsResp struct {
	Code string           `json:"code"`
	Msg  string           `json:"msg"`
	Data []OKXTradeRecord `json:"data"`
}

type OKXTradeRecord struct {
	AvgPx    string `json:"avgPx"`
	BaseName string `json:"baseName"`
	CTime    string `json:"cTime"`
	FillTime string `json:"fillTime"`
	InstId   string `json:"instId"`
	InstType string `json:"instType"`
	Lever    string `json:"lever"`
	OrdId    string `json:"ordId"`
	OrdType  string `json:"ordType"`
	PosSide  string `json:"posSide"` // "long" | "short"
	Side     string `json:"side"`    // "buy" | "sell"
	Sz       string `json:"sz"`
	Value    string `json:"value"`
}

// OKXAssetResp asset 返回结构
type OKXAssetResp struct {
	Code string     `json:"code"`
	Msg  string     `json:"msg"`
	Data []OKXAsset `json:"data"`
}

type OKXAsset struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
}

// OKXPositionResp position-current 返回结构
type OKXPositionResp struct {
	Code string            `json:"code"`
	Msg  string            `json:"msg"`
	Data []OKXPositionData `json:"data"`
}

type OKXPositionData struct {
	PosData []OKXPosition `json:"posData"`
}

type OKXPosition struct {
	AvgPx       string `json:"avgPx"`
	InstId      string `json:"instId"`
	Lever       string `json:"lever"`
	LiqPx       string `json:"liqPx"`
	Margin      string `json:"margin"`
	MarkPx      string `json:"markPx"`
	MgnMode     string `json:"mgnMode"` // "isolated" | "cross"
	NotionalUsd string `json:"notionalUsd"`
	Pos         string `json:"pos"`
	PosSide     string `json:"posSide"`
	Upl         string `json:"upl"`
}

// ============================================================================
// 工具函数
// ============================================================================

// normalizeSymbol 统一符号格式: BTCUSDT
func normalizeSymbol(coin string) string {
	coin = strings.ToUpper(coin)
	if !strings.HasSuffix(coin, "USDT") {
		coin = coin + "USDT"
	}
	return coin
}

// normalizeOKXSymbol OKX 符号格式化: "BTC-USDT-SWAP" -> "BTCUSDT"
func normalizeOKXSymbol(instId string) string {
	parts := strings.Split(instId, "-")
	if len(parts) >= 2 {
		return strings.ToUpper(parts[0] + parts[1])
	}
	return strings.ToUpper(instId)
}

// parseFloat 安全解析浮点数
func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		logger.Warnf("parseFloat failed: %s", s)
		return 0
	}
	return f
}

// parseInt 安全解析整数
func parseInt(s string) int {
	if s == "" {
		return 0
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		logger.Warnf("parseInt failed: %s", s)
		return 0
	}
	return i
}

// parseInt64 安全解析 int64
func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		logger.Warnf("parseInt64 failed: %s", s)
		return 0
	}
	return i
}

