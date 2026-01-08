package copytrade

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"nofx/logger"

	"github.com/gorilla/websocket"
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

	// REST Provider（用于按需获取账户状态，解决 WS 时序问题）
	restProvider *HyperliquidProvider

	// 回调函数
	onFill        func(Fill)
	onStateUpdate func(*AccountState)

	// 状态缓存（由 REST 获取或 WebSocket 推送更新）
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
		restProvider: NewHyperliquidProvider(), // 复用 REST Provider 获取账户状态
		recentFills:  make([]Fill, 0),
		fillsTTL:     5 * time.Minute, // Fill 缓存 5 分钟
		stopCh:       make(chan struct{}),
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
	state := p.latestState
	p.stateMu.RUnlock()

	if state != nil {
		return state, nil
	}

	// 🔑 缓存为空（如启动时 WS 还未连接），使用 REST API 作为 fallback
	// 这样 InitIgnoredPositions() 可以在启动时成功获取领航员持仓
	if p.restProvider == nil {
		return nil, fmt.Errorf("no state available yet and no REST provider")
	}

	logger.Infof("📡 [HL-WS] 缓存为空，使用 REST API 获取领航员状态: %s", leaderID)
	newState, err := p.restProvider.GetAccountState(leaderID)
	if err != nil {
		return nil, fmt.Errorf("REST 获取账户状态失败: %w", err)
	}

	logger.Infof("📡 [HL-WS] REST 获取成功 | 权益=%.2f 持仓数=%d",
		newState.TotalEquity, len(newState.Positions))

	// 更新缓存，后续可以直接使用
	p.stateMu.Lock()
	p.latestState = newState
	p.stateMu.Unlock()

	return newState, nil
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
		go p.readLoop() // 重连成功后重启读取循环
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
		IsSnapshot bool     `json:"isSnapshot"`
		User       string   `json:"user"`
		Fills      []WsFill `json:"fills"`
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

	// 如果有新成交，先通过 REST 获取最新账户状态（解决 WS 时序问题）
	if len(fillsMsg.Fills) > 0 {
		p.refreshAccountState()
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

// refreshAccountState 通过 REST 获取最新账户状态（混合模式）
// 在收到交易信号时调用，确保获取到准确的领航员权益和持仓信息
// 同时触发 onStateUpdate 回调，让 Engine 也更新 leaderState 缓存
func (p *HLWebSocketProvider) refreshAccountState() {
	if p.restProvider == nil || p.leaderID == "" {
		return
	}

	state, err := p.restProvider.GetAccountState(p.leaderID)
	if err != nil {
		logger.Warnf("⚠️ [HL-WS] REST 获取账户状态失败: %v", err)
		return
	}

	// 更新本地缓存
	p.stateMu.Lock()
	p.latestState = state
	p.stateMu.Unlock()

	logger.Infof("📡 [HL-WS] REST 获取账户状态成功 | 权益=%.2f 持仓数=%d",
		state.TotalEquity, len(state.Positions))

	// 触发回调，让 Engine 的 leaderState 也同步更新
	// 这样加仓/减仓/平仓判断可以使用最新的持仓数据
	if p.onStateUpdate != nil {
		p.onStateUpdate(state)
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
		go p.reconnect() // 心跳失败触发重连
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
	startPos, _ := strconv.ParseFloat(raw.StartPosition, 64)

	// 🔑 使用 startPosition + size 精确判断动作类型
	// startPosition=0 + "Open Long/Short" = 新开仓
	// startPosition>0 + "Open Long/Short" = 加仓
	// size >= startPos*0.95 + "Close Long/Short" = 完全平仓
	// size < startPos*0.95 + "Close Long/Short" = 减仓
	action, side := parseHLDirWithStartPos(raw.Dir, startPos, size)

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

// parseHLDirWithStartPos 使用 startPosition 和 size 精确判断动作类型
// 🔑 核心逻辑：
//   - "Open Long/Short" + startPosition=0 → ActionOpen（新开仓）
//   - "Open Long/Short" + startPosition>0 → ActionAdd（加仓）
//   - "Close Long/Short" + sz >= startPos*0.95 → ActionClose（完全平仓）
//   - "Close Long/Short" + sz < startPos*0.95 → ActionReduce（减仓）
func parseHLDirWithStartPos(dir string, startPos float64, size float64) (ActionType, SideType) {
	switch dir {
	case "Open Long":
		if startPos == 0 {
			return ActionOpen, SideLong
		}
		return ActionAdd, SideLong
	case "Open Short":
		if startPos == 0 {
			return ActionOpen, SideShort
		}
		return ActionAdd, SideShort
	case "Close Long":
		// 🔑 使用 sz/startPosition 判断完全平仓 vs 减仓
		// sz >= startPos*0.95 表示平掉了 95% 以上，视为完全平仓
		if startPos > 0 && size >= startPos*0.95 {
			logger.Infof("📡 [HL-WS] Close Long: sz(%.4f) >= 95%% of startPos(%.4f) → 完全平仓", size, startPos)
			return ActionClose, SideLong
		}
		logger.Infof("📡 [HL-WS] Close Long: sz(%.4f) < 95%% of startPos(%.4f) → 减仓", size, startPos)
		return ActionReduce, SideLong
	case "Close Short":
		// 🔑 同样逻辑判断空仓平仓
		if startPos > 0 && size >= startPos*0.95 {
			logger.Infof("📡 [HL-WS] Close Short: sz(%.4f) >= 95%% of startPos(%.4f) → 完全平仓", size, startPos)
			return ActionClose, SideShort
		}
		logger.Infof("📡 [HL-WS] Close Short: sz(%.4f) < 95%% of startPos(%.4f) → 减仓", size, startPos)
		return ActionReduce, SideShort

	// 🔄 反向开仓处理
	case "Long > Short":
		logger.Infof("📡 [HL-WS] 检测到反向开仓: %s → 转换为 Open Short", dir)
		return ActionOpen, SideShort
	case "Short > Long":
		logger.Infof("📡 [HL-WS] 检测到反向开仓: %s → 转换为 Open Long", dir)
		return ActionOpen, SideLong

	default:
		logger.Warnf("⚠️ [HL-WS] 未知 dir: %s，默认为 Open Long", dir)
		return ActionOpen, SideLong
	}
}

// parseHLDir 解析 Hyperliquid 的 dir 字段（旧版本，不使用 startPosition）
// dir: "Open Long" | "Close Long" | "Open Short" | "Close Short" | "Long > Short" | "Short > Long"
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

	// 🔄 反向开仓处理（Hyperliquid 特有）
	// 反向开仓 = 平掉原仓位 + 开新方向仓位（一次交易完成）
	// 处理策略：将新方向视为新开仓
	case "Long > Short":
		// 从多翻空：新方向是 Short，当作新开仓处理
		logger.Infof("📡 [HL-WS] 检测到反向开仓: %s → 转换为 Open Short", dir)
		return ActionOpen, SideShort
	case "Short > Long":
		// 从空翻多：新方向是 Long，当作新开仓处理
		logger.Infof("📡 [HL-WS] 检测到反向开仓: %s → 转换为 Open Long", dir)
		return ActionOpen, SideLong

	default:
		// 尝试从旧格式解析
		if len(dir) > 0 {
			if dir[0] == 'B' {
				return ActionOpen, SideLong
			} else if dir[0] == 'A' {
				return ActionOpen, SideShort
			}
		}
		logger.Warnf("⚠️ [HL-WS] 未知的 dir 类型: %s，默认按 Open Long 处理", dir)
		return ActionOpen, SideLong
	}
}
