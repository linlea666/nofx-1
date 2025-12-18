package copytrade

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"nofx/logger"
)

// ============================================================================
// Hyperliquid WebSocket Provider（事件驱动模式）
// ============================================================================

const (
	HLWebSocketURL = "wss://api.hyperliquid.xyz/ws"
	// 心跳间隔（官方要求 60 秒内必须有消息，我们用 30 秒）
	HLHeartbeatInterval = 30 * time.Second
	// 重连延迟
	HLReconnectDelay = 3 * time.Second
)

// HLWebSocketProvider Hyperliquid WebSocket 数据提供者
type HLWebSocketProvider struct {
	leaderID string
	conn     *websocket.Conn
	connMu   sync.Mutex

	// 回调函数
	onFill        func(Fill)
	onStateUpdate func(*AccountState)

	// 状态缓存（由 WebSocket 推送持续更新）
	latestState *AccountState
	stateMu     sync.RWMutex

	// Fill 缓存（用于 GetFills 接口兼容）
	recentFills []Fill
	fillsMu     sync.RWMutex
	fillsTTL    time.Duration

	// 控制
	stopCh    chan struct{}
	running   bool
	runningMu sync.RWMutex
}

// NewHLWebSocketProvider 创建 Hyperliquid WebSocket Provider
func NewHLWebSocketProvider() *HLWebSocketProvider {
	return &HLWebSocketProvider{
		recentFills: make([]Fill, 0),
		fillsTTL:    5 * time.Minute, // Fill 缓存 5 分钟
		stopCh:      make(chan struct{}),
	}
}

// ============================================================================
// StreamingProvider 接口实现
// ============================================================================

func (p *HLWebSocketProvider) Type() ProviderType {
	return ProviderHyperliquid
}

func (p *HLWebSocketProvider) IsStreaming() bool {
	return true
}

func (p *HLWebSocketProvider) SetOnFill(callback func(Fill)) {
	p.onFill = callback
}

func (p *HLWebSocketProvider) SetOnStateUpdate(callback func(*AccountState)) {
	p.onStateUpdate = callback
}

// Connect 连接并订阅指定领航员
func (p *HLWebSocketProvider) Connect(leaderID string) error {
	p.leaderID = leaderID

	if err := p.connect(); err != nil {
		return err
	}

	// 启动消息处理和心跳
	go p.readLoop()
	go p.heartbeatLoop()

	p.runningMu.Lock()
	p.running = true
	p.runningMu.Unlock()

	logger.Infof("🔌 [HL-WS] 已连接并订阅领航员: %s", leaderID)
	return nil
}

// Close 关闭连接
func (p *HLWebSocketProvider) Close() error {
	p.runningMu.Lock()
	if !p.running {
		p.runningMu.Unlock()
		return nil
	}
	p.running = false
	p.runningMu.Unlock()

	close(p.stopCh)

	p.connMu.Lock()
	defer p.connMu.Unlock()
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}

// GetFills 获取最近成交（从缓存读取，保持接口兼容）
func (p *HLWebSocketProvider) GetFills(leaderID string, since time.Time) ([]Fill, error) {
	p.fillsMu.RLock()
	defer p.fillsMu.RUnlock()

	var result []Fill
	for _, fill := range p.recentFills {
		if fill.Timestamp.After(since) {
			result = append(result, fill)
		}
	}
	return result, nil
}

// GetAccountState 获取账户状态（从缓存读取）
func (p *HLWebSocketProvider) GetAccountState(leaderID string) (*AccountState, error) {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()

	if p.latestState == nil {
		return nil, fmt.Errorf("no state available yet")
	}
	return p.latestState, nil
}

// ============================================================================
// WebSocket 连接管理
// ============================================================================

func (p *HLWebSocketProvider) connect() error {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	// 关闭旧连接
	if p.conn != nil {
		p.conn.Close()
	}

	// 建立新连接
	conn, _, err := websocket.DefaultDialer.Dial(HLWebSocketURL, nil)
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}
	p.conn = conn

	// 订阅 userFills
	if err := p.subscribe("userFills", p.leaderID); err != nil {
		return fmt.Errorf("subscribe userFills failed: %w", err)
	}

	// 订阅 clearinghouseState
	if err := p.subscribe("clearinghouseState", p.leaderID); err != nil {
		return fmt.Errorf("subscribe clearinghouseState failed: %w", err)
	}

	logger.Infof("🔌 [HL-WS] WebSocket 连接成功，已订阅 userFills + clearinghouseState")
	return nil
}

func (p *HLWebSocketProvider) subscribe(subType, user string) error {
	msg := map[string]interface{}{
		"method": "subscribe",
		"subscription": map[string]string{
			"type": subType,
			"user": user,
		},
	}

	data, _ := json.Marshal(msg)
	return p.conn.WriteMessage(websocket.TextMessage, data)
}

func (p *HLWebSocketProvider) reconnect() {
	p.runningMu.RLock()
	running := p.running
	p.runningMu.RUnlock()

	if !running {
		return
	}

	logger.Warnf("⚠️ [HL-WS] 连接断开，%v 后重连...", HLReconnectDelay)
	time.Sleep(HLReconnectDelay)

	for {
		p.runningMu.RLock()
		running := p.running
		p.runningMu.RUnlock()

		if !running {
			return
		}

		if err := p.connect(); err != nil {
			logger.Warnf("⚠️ [HL-WS] 重连失败: %v，%v 后重试...", err, HLReconnectDelay)
			time.Sleep(HLReconnectDelay)
			continue
		}

		logger.Infof("✅ [HL-WS] 重连成功")
		return
	}
}

// ============================================================================
// 消息处理
// ============================================================================

func (p *HLWebSocketProvider) readLoop() {
	for {
		p.runningMu.RLock()
		running := p.running
		p.runningMu.RUnlock()

		if !running {
			return
		}

		p.connMu.Lock()
		conn := p.conn
		p.connMu.Unlock()

		if conn == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			logger.Warnf("⚠️ [HL-WS] 读取消息失败: %v", err)
			go p.reconnect()
			return
		}

		p.handleMessage(message)
	}
}

func (p *HLWebSocketProvider) handleMessage(message []byte) {
	var msg struct {
		Channel string          `json:"channel"`
		Data    json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(message, &msg); err != nil {
		return
	}

	switch msg.Channel {
	case "userFills":
		p.handleUserFills(msg.Data)
	case "clearinghouseState":
		p.handleClearinghouseState(msg.Data)
	case "subscriptionResponse":
		logger.Debugf("📡 [HL-WS] 订阅确认: %s", string(msg.Data))
	case "pong":
		// 心跳响应，忽略
	default:
		logger.Debugf("📡 [HL-WS] 未知消息类型: %s", msg.Channel)
	}
}

func (p *HLWebSocketProvider) handleUserFills(data json.RawMessage) {
	var fillsMsg struct {
		IsSnapshot bool      `json:"isSnapshot"`
		User       string    `json:"user"`
		Fills      []WsFill  `json:"fills"`
	}

	if err := json.Unmarshal(data, &fillsMsg); err != nil {
		logger.Warnf("⚠️ [HL-WS] 解析 userFills 失败: %v", err)
		return
	}

	// 跳过快照消息（历史数据）
	if fillsMsg.IsSnapshot {
		logger.Debugf("📡 [HL-WS] 收到快照，包含 %d 条历史成交", len(fillsMsg.Fills))
		return
	}

	// 处理新成交
	for _, wsFill := range fillsMsg.Fills {
		fill := p.convertWsFill(wsFill)

		// 添加到缓存
		p.addFillToCache(fill)

		// 触发回调
		if p.onFill != nil {
			logger.Infof("📡 [HL-WS] 收到成交推送 | %s %s %s | 价格=%.4f 数量=%.4f",
				fill.Symbol, fill.Action, fill.PositionSide, fill.Price, fill.Size)
			p.onFill(fill)
		}
	}
}

func (p *HLWebSocketProvider) handleClearinghouseState(data json.RawMessage) {
	var state WsClearinghouseState
	if err := json.Unmarshal(data, &state); err != nil {
		logger.Warnf("⚠️ [HL-WS] 解析 clearinghouseState 失败: %v", err)
		return
	}

	accountState := p.convertClearinghouseState(state)

	// 更新缓存
	p.stateMu.Lock()
	p.latestState = accountState
	p.stateMu.Unlock()

	// 触发回调
	if p.onStateUpdate != nil {
		p.onStateUpdate(accountState)
	}
}

// ============================================================================
// 心跳保活
// ============================================================================

func (p *HLWebSocketProvider) heartbeatLoop() {
	ticker := time.NewTicker(HLHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.sendPing()
		}
	}
}

func (p *HLWebSocketProvider) sendPing() {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	if p.conn == nil {
		return
	}

	msg := map[string]string{"method": "ping"}
	data, _ := json.Marshal(msg)
	if err := p.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		logger.Warnf("⚠️ [HL-WS] 发送心跳失败: %v", err)
	}
}

// ============================================================================
// 数据转换
// ============================================================================

// WsFill WebSocket 成交数据结构
type WsFill struct {
	Coin          string `json:"coin"`
	Px            string `json:"px"`
	Sz            string `json:"sz"`
	Side          string `json:"side"`
	Time          int64  `json:"time"`
	StartPosition string `json:"startPosition"`
	Dir           string `json:"dir"`
	ClosedPnl     string `json:"closedPnl"`
	Hash          string `json:"hash"`
	Oid           int64  `json:"oid"`
	Crossed       bool   `json:"crossed"`
	Fee           string `json:"fee"`
	Tid           int64  `json:"tid"`
}

func (p *HLWebSocketProvider) convertWsFill(raw WsFill) Fill {
	price, _ := strconv.ParseFloat(raw.Px, 64)
	size, _ := strconv.ParseFloat(raw.Sz, 64)
	closedPnl, _ := strconv.ParseFloat(raw.ClosedPnl, 64)

	// 解析方向和动作
	action, side := parseHLDir(raw.Dir)

	return Fill{
		ID:           raw.Hash,
		Symbol:       raw.Coin + "USDT",
		Price:        price,
		Size:         size,
		Side:         raw.Side,
		Action:       action,
		PositionSide: side,
		Timestamp:    time.UnixMilli(raw.Time),
		ClosedPnL:    closedPnl,
		Value:        price * size,
	}
}

// WsClearinghouseState WebSocket 持仓状态结构（与 REST 版本 HLClearinghouseState 字段类型略有不同）
type WsClearinghouseState struct {
	AssetPositions []struct {
		Type     string `json:"type"`
		Position struct {
			Coin          string `json:"coin"`
			Szi           string `json:"szi"`
			EntryPx       string `json:"entryPx"`
			PositionValue string `json:"positionValue"`
			UnrealizedPnl string `json:"unrealizedPnl"`
			Leverage      struct {
				Type  string  `json:"type"`
				Value float64 `json:"value"`
			} `json:"leverage"`
		} `json:"position"`
	} `json:"assetPositions"`
	MarginSummary struct {
		AccountValue    float64 `json:"accountValue"`
		TotalNtlPos     float64 `json:"totalNtlPos"`
		TotalRawUsd     float64 `json:"totalRawUsd"`
		TotalMarginUsed float64 `json:"totalMarginUsed"`
	} `json:"marginSummary"`
	Withdrawable float64 `json:"withdrawable"`
}

func (p *HLWebSocketProvider) convertClearinghouseState(state WsClearinghouseState) *AccountState {
	positions := make(map[string]*Position)

	for _, ap := range state.AssetPositions {
		pos := ap.Position
		szi, _ := strconv.ParseFloat(pos.Szi, 64)
		if szi == 0 {
			continue
		}

		entryPx, _ := strconv.ParseFloat(pos.EntryPx, 64)
		posValue, _ := strconv.ParseFloat(pos.PositionValue, 64)
		upl, _ := strconv.ParseFloat(pos.UnrealizedPnl, 64)

		side := SideLong
		if szi < 0 {
			side = SideShort
			szi = -szi
		}

		key := PositionKey(pos.Coin+"USDT", side)
		positions[key] = &Position{
			Symbol:        pos.Coin + "USDT",
			Side:          side,
			Size:          szi,
			EntryPrice:    entryPx,
			Leverage:      int(pos.Leverage.Value),
			MarginMode:    pos.Leverage.Type, // "cross" or "isolated"
			UnrealizedPnL: upl,
			PositionValue: posValue,
		}
	}

	return &AccountState{
		TotalEquity:      state.MarginSummary.AccountValue,
		AvailableBalance: state.Withdrawable,
		Positions:        positions,
		Timestamp:        time.Now(),
	}
}

func (p *HLWebSocketProvider) addFillToCache(fill Fill) {
	p.fillsMu.Lock()
	defer p.fillsMu.Unlock()

	// 添加新 Fill
	p.recentFills = append(p.recentFills, fill)

	// 清理过期 Fill
	cutoff := time.Now().Add(-p.fillsTTL)
	var valid []Fill
	for _, f := range p.recentFills {
		if f.Timestamp.After(cutoff) {
			valid = append(valid, f)
		}
	}
	p.recentFills = valid
}

// parseHLDir 解析 Hyperliquid 的 dir 字段
// dir: "Open Long" | "Close Long" | "Open Short" | "Close Short"
func parseHLDir(dir string) (ActionType, SideType) {
	switch dir {
	case "Open Long":
		return ActionOpen, SideLong
	case "Close Long":
		return ActionClose, SideLong
	case "Open Short":
		return ActionOpen, SideShort
	case "Close Short":
		return ActionClose, SideShort
	default:
		// 尝试从旧格式解析
		if len(dir) > 0 {
			if dir[0] == 'B' {
				return ActionOpen, SideLong
			} else if dir[0] == 'A' {
				return ActionOpen, SideShort
			}
		}
		return ActionOpen, SideLong
	}
}

