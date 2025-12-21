package copytrade

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"nofx/decision"
	"nofx/logger"
	"nofx/store"
)

// Engine 跟单引擎
type Engine struct {
	traderID string
	config   *CopyConfig
	provider LeaderProvider

	// 流式 Provider（如果支持）
	streamingProvider StreamingProvider
	isStreamingMode   bool

	// 跟随者账户信息（由外部注入）
	getFollowerBalance   func() float64
	getFollowerPositions func() map[string]*Position

	// 数据库存储（用于仓位映射）
	store *store.Store

	// 去重（使用时间戳过期）
	seenFills map[string]time.Time
	seenMu    sync.RWMutex
	seenTTL   time.Duration

	// 状态缓存
	leaderState       *AccountState
	leaderStateMu     sync.RWMutex
	lastStateSync     time.Time
	stateSyncInterval time.Duration

	// 决策输出
	decisionCh chan *decision.FullDecision

	// 预警日志
	warnings   []Warning
	warningsMu sync.Mutex

	// 运行状态
	running bool
	stopCh  chan struct{}
	mu      sync.RWMutex

	// 统计
	stats *EngineStats
}

// EngineOption 引擎配置选项
type EngineOption func(*Engine)

// WithStreamingMode 启用流式模式（WebSocket 事件驱动）
func WithStreamingMode() EngineOption {
	return func(e *Engine) {
		e.isStreamingMode = true
	}
}

// NewEngine 创建跟单引擎
func NewEngine(
	traderID string,
	config *CopyConfig,
	getBalance func() float64,
	getPositions func() map[string]*Position,
	opts ...EngineOption,
) (*Engine, error) {
	e := &Engine{
		traderID:             traderID,
		config:               config,
		getFollowerBalance:   getBalance,
		getFollowerPositions: getPositions,
		seenFills:            make(map[string]time.Time),
		seenTTL:              1 * time.Hour,
		stateSyncInterval:    30 * time.Second,
		decisionCh:           make(chan *decision.FullDecision, 10),
		stopCh:               make(chan struct{}),
		stats:                &EngineStats{StartTime: time.Now()},
	}

	// 应用选项
	for _, opt := range opts {
		opt(e)
	}

	// 根据配置选择 Provider 类型
	if e.isStreamingMode {
		// 尝试创建流式 Provider（目前只有 Hyperliquid 支持）
		streamingProvider, err := NewStreamingProvider(config.ProviderType)
		if err != nil {
			// 不支持流式模式，回退到轮询模式
			logger.Warnf("⚠️ [%s] %s 不支持流式模式，回退到轮询模式", traderID, config.ProviderType)
			e.isStreamingMode = false
		} else {
			e.streamingProvider = streamingProvider
			e.provider = streamingProvider // StreamingProvider 也实现了 LeaderProvider
			logger.Infof("✅ [%s] 使用流式模式 (WebSocket)", traderID)
			return e, nil
		}
	}

	// 轮询模式（默认，或流式模式不可用时回退）
	provider, err := NewProvider(config.ProviderType)
	if err != nil {
		return nil, err
	}
	e.provider = provider
	logger.Infof("✅ [%s] 使用轮询模式 (REST)", traderID)

	return e, nil
}

// GetDecisionChannel 获取决策输出通道
func (e *Engine) GetDecisionChannel() <-chan *decision.FullDecision {
	return e.decisionCh
}

// GetStats 获取统计信息
func (e *Engine) GetStats() *EngineStats {
	return e.stats
}

// SetStore 设置数据库存储（用于仓位映射）
func (e *Engine) SetStore(st *store.Store) {
	e.store = st
}

// InitIgnoredPositions 初始化领航员历史仓位（启动跟单时调用）
// 将领航员当前所有持仓标记为 ignored，后续这些仓位的操作都不跟随
// 这样可以 100% 准确地区分"新开仓"和"历史仓位操作"
func (e *Engine) InitIgnoredPositions() error {
	if e.store == nil {
		return fmt.Errorf("store not initialized")
	}

	// 获取领航员当前所有持仓
	state, err := e.provider.GetAccountState(e.config.LeaderID)
	if err != nil {
		return fmt.Errorf("获取领航员持仓失败: %w", err)
	}

	if state == nil || len(state.Positions) == 0 {
		logger.Infof("📊 [%s] 领航员当前无持仓，无需标记历史仓位", e.traderID)
		return nil
	}

	// 将所有持仓标记为 ignored
	ignoredCount := 0
	for key, pos := range state.Positions {
		// 确定 posId：优先用原生的，否则用 map key（symbol_side 格式）作为虚拟 posId
		posID := pos.PosID
		if posID == "" {
			// Hyperliquid 等无原生 posId 的交易所，用 symbol_side 作为虚拟 posId
			// key 格式为 "BTCUSDT_long"、"ETHUSDT_short"
			posID = key
			logger.Debugf("📊 [%s] 持仓 %s %s 使用虚拟 posId: %s", e.traderID, pos.Symbol, pos.Side, posID)
		}

		err := e.store.CopyTrade().SaveIgnoredPosition(
			e.traderID,
			e.config.LeaderID,
			posID,
			pos.Symbol,
			string(pos.Side),
			pos.MarginMode,
		)
		if err != nil {
			logger.Warnf("⚠️ [%s] 标记历史仓位失败 posId=%s: %v", e.traderID, posID, err)
			continue
		}

		ignoredCount++
		logger.Infof("📊 [%s] 标记历史仓位 | posId=%s %s %s %s",
			e.traderID, posID, pos.Symbol, pos.Side, pos.MarginMode)
	}

	logger.Infof("✅ [%s] 历史仓位初始化完成 | 共标记 %d 个仓位为 ignored", e.traderID, ignoredCount)
	return nil
}

// Start 启动引擎
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return fmt.Errorf("engine already running")
	}
	e.running = true
	e.mu.Unlock()

	mode := "轮询"
	if e.isStreamingMode {
		mode = "流式(WebSocket)"
	}
	logger.Infof("🚀 [%s] 跟单引擎启动 | provider=%s leader=%s ratio=%.0f%% mode=%s",
		e.traderID, e.config.ProviderType, e.config.LeaderID, e.config.CopyRatio*100, mode)

	// 流式模式：WebSocket 事件驱动
	if e.isStreamingMode && e.streamingProvider != nil {
		return e.startStreamingMode(ctx)
	}

	// 轮询模式：REST 定时轮询（OKX 或回退模式）
	return e.startPollingMode(ctx)
}

// startStreamingMode 启动流式模式（WebSocket 事件驱动）
func (e *Engine) startStreamingMode(ctx context.Context) error {
	// 设置 Fill 回调：收到成交时立即处理
	e.streamingProvider.SetOnFill(func(fill Fill) {
		// 去重检查
		if e.isSeen(fill.ID) {
			return
		}
		e.markSeen(fill.ID)

		e.stats.SignalsReceived++
		e.stats.LastSignalTime = time.Now()

		// 构造信号并处理
		signal := e.buildSignal(&fill)
		logger.Infof("📡 [%s] 收到信号(WS) | %s %s %s | 价格=%.4f 数量=%.4f 价值=%.2f",
			e.traderID, fill.Symbol, fill.Action, fill.PositionSide,
			fill.Price, fill.Size, fill.Value)

		e.processSignal(signal)
	})

	// 设置状态更新回调：持仓变化时更新缓存
	e.streamingProvider.SetOnStateUpdate(func(state *AccountState) {
		e.leaderStateMu.Lock()
		e.leaderState = state
		e.lastStateSync = time.Now()
		e.leaderStateMu.Unlock()
	})

	// 连接并订阅
	if err := e.streamingProvider.Connect(e.config.LeaderID); err != nil {
		return fmt.Errorf("streaming provider connect failed: %w", err)
	}

	// 初始同步领航员状态
	if err := e.syncLeaderState(); err != nil {
		logger.Warnf("⚠️ [%s] 初始状态同步失败: %v", e.traderID, err)
	}

	// 获取历史成交作为去重基线
	e.initSeenFills()

	logger.Infof("✅ [%s] 流式模式已启动，等待 WebSocket 推送...", e.traderID)
	return nil
}

// startPollingMode 启动轮询模式（REST 定时轮询）
func (e *Engine) startPollingMode(ctx context.Context) error {
	// 初始同步领航员状态
	if err := e.syncLeaderState(); err != nil {
		logger.Warnf("⚠️ [%s] 初始状态同步失败: %v", e.traderID, err)
	}

	// 获取历史成交作为去重基线
	e.initSeenFills()

	// 启动轮询协程
	go e.pollLoop(ctx)

	return nil
}

// Stop 停止引擎
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running {
		return
	}

	// 关闭流式 Provider
	if e.streamingProvider != nil {
		e.streamingProvider.Close()
	}

	close(e.stopCh)
	e.running = false

	logger.Infof("🛑 [%s] 跟单引擎已停止", e.traderID)
}

// ============================================================================
// 核心轮询逻辑
// ============================================================================

func (e *Engine) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.poll()
		}
	}
}

func (e *Engine) poll() {
	// 获取最近 1 分钟的成交
	since := time.Now().Add(-1 * time.Minute)
	fills, err := e.provider.GetFills(e.config.LeaderID, since)
	if err != nil {
		logger.Warnf("⚠️ [%s] 获取成交记录失败: %v", e.traderID, err)
		return
	}

	// 同步领航员状态
	if time.Since(e.lastStateSync) > e.stateSyncInterval {
		if err := e.syncLeaderState(); err != nil {
			logger.Warnf("⚠️ [%s] 状态同步失败: %v", e.traderID, err)
		}
	}

	// 按时间排序（确保反向开仓按顺序处理）
	sort.Slice(fills, func(i, j int) bool {
		return fills[i].Timestamp.Before(fills[j].Timestamp)
	})

	// 处理新成交
	for _, fill := range fills {
		if e.isSeen(fill.ID) {
			continue
		}
		e.markSeen(fill.ID)

		e.stats.SignalsReceived++
		e.stats.LastSignalTime = time.Now()

		// 构造信号
		signal := e.buildSignal(&fill)

		logger.Infof("📡 [%s] 收到信号 | %s %s %s | 价格=%.4f 数量=%.4f 价值=%.2f",
			e.traderID, fill.Symbol, fill.Action, fill.PositionSide,
			fill.Price, fill.Size, fill.Value)

		// 处理信号
		e.processSignal(signal)
	}
}

func (e *Engine) buildSignal(fill *Fill) *TradeSignal {
	e.leaderStateMu.RLock()
	defer e.leaderStateMu.RUnlock()

	signal := &TradeSignal{
		LeaderID:     e.config.LeaderID,
		ProviderType: e.config.ProviderType,
		Fill:         fill,
	}

	if e.leaderState != nil {
		signal.LeaderEquity = e.leaderState.TotalEquity
	}

	return signal
}

// ============================================================================
// 统一信号匹配（核心逻辑）
// ============================================================================

// SignalMatchResult 信号匹配结果
type SignalMatchResult struct {
	ShouldFollow   bool       // 是否跟随
	Reason         string     // 原因
	Action         ActionType // 实际动作类型
	PosID          string     // 领航员仓位 ID
	MarginMode     string     // 保证金模式
	LeaderPosition *Position  // 领航员仓位（可能为 nil，表示已平仓）
}

// matchSignalWithMapping 统一信号匹配（核心方法）
// ============================================================================
// 统一处理所有信号类型：开仓/加仓/减仓/平仓
// 核心思想：
//   - 开仓/加仓：从领航员持仓列表获取 posId，查数据库映射判断
//   - 减仓/平仓：反向查找法 - 从本地 active 映射出发，对比领航员持仓
//
// ============================================================================
func (e *Engine) matchSignalWithMapping(signal *TradeSignal) *SignalMatchResult {
	fill := signal.Fill

	if e.store == nil {
		return &SignalMatchResult{
			ShouldFollow: false,
			Reason:       "数据库未初始化",
		}
	}

	// 构建领航员持仓 posId -> Position 映射（一次构建，全程复用）
	leaderPosMap := e.buildLeaderPosMap()

	// ============================================================
	// 场景 1: 开仓/加仓信号
	// ============================================================
	if fill.Action == ActionOpen || fill.Action == ActionAdd {
		return e.matchOpenAddSignal(signal, leaderPosMap)
	}

	// ============================================================
	// 场景 2: 减仓/平仓信号（反向查找法）
	// ============================================================
	return e.matchCloseReduceSignal(signal, leaderPosMap)
}

// buildLeaderPosMap 构建领航员持仓映射 (posId -> Position)
func (e *Engine) buildLeaderPosMap() map[string]*Position {
	e.leaderStateMu.RLock()
	defer e.leaderStateMu.RUnlock()

	posMap := make(map[string]*Position)
	if e.leaderState == nil || e.leaderState.Positions == nil {
		return posMap
	}

	for key, pos := range e.leaderState.Positions {
		if pos.PosID != "" {
			posMap[pos.PosID] = pos
		} else {
			// Hyperliquid 等无 posId 的交易所，用 symbol_side 作为 key
			posMap[key] = pos
		}
	}
	return posMap
}

// matchOpenAddSignal 匹配开仓/加仓信号
// 核心思想：
//  1. 新开仓：找领航员持仓中没有本地映射的 posId
//  2. 加仓：通过 lastKnownSize 变化判断是哪个仓位被加仓（size 增加的那个）
func (e *Engine) matchOpenAddSignal(signal *TradeSignal, leaderPosMap map[string]*Position) *SignalMatchResult {
	fill := signal.Fill

	// 收集所有 symbol+side 匹配的仓位
	var matchedPositions []*Position
	for _, pos := range leaderPosMap {
		if pos.Symbol == fill.Symbol && pos.Side == fill.PositionSide {
			matchedPositions = append(matchedPositions, pos)
		}
	}

	if len(matchedPositions) == 0 {
		return &SignalMatchResult{
			ShouldFollow: false,
			Reason:       fmt.Sprintf("领航员持仓中找不到 %s %s", fill.Symbol, fill.PositionSide),
		}
	}

	// ============================================================
	// 第一轮：查找新开仓（无映射或 closed 状态的 posId）
	// ============================================================
	var newPosition *Position

	for _, pos := range matchedPositions {
		posID := pos.PosID
		if posID == "" {
			posID = fmt.Sprintf("%s_%s", fill.Symbol, fill.PositionSide)
		}

		mapping, err := e.store.CopyTrade().GetMapping(e.traderID, posID)
		if err != nil {
			logger.Warnf("⚠️ [%s] 查询映射失败: %v (posId=%s)", e.traderID, err, posID)
			continue
		}

		if mapping == nil {
			// 无映射 = 新开仓（优先）
			logger.Infof("📊 [%s] 发现新 posId | posId=%s mgnMode=%s → 新开仓候选",
				e.traderID, posID, pos.MarginMode)
			newPosition = pos
			break
		}

		if mapping.Status == "closed" {
			// 已关闭 = 可重新开仓
			logger.Infof("📊 [%s] 仓位已关闭 | posId=%s → 新开仓候选",
				e.traderID, posID)
			newPosition = pos
			break
		}

		if mapping.Status == "ignored" {
			// 🔑 关键区分：根据数据源（ProviderType）使用不同的判断逻辑
			if e.config.ProviderType == "okx" {
				// OKX: ignored 状态永远不跟
				// 原因：OKX 的 posId 是真实的，平仓后失效，新开仓会分配新的 posId
				// 所以 ignored 的 posId 永远不会再被使用，直接跳过
				logger.Infof("📊 [%s] 历史仓位 | posId=%s status=ignored → 不跟随（OKX新开仓会用新posId）",
					e.traderID, posID)
				continue
			}

			// Hyperliquid: 需要判断是否是真正的重新开仓
			// 原因：Hyperliquid 的 posId 是虚拟的（symbol_side），平仓后重开会复用同一个 posId
			// 通过 ActionOpen（startPosition=0）判断是否是全新开仓
			if fill.Action == ActionOpen {
				logger.Infof("📊 [%s] 历史仓位重新开仓 | posId=%s (ignored → active) → 跟随新开仓（Hyperliquid）",
					e.traderID, posID)
				newPosition = pos
				break
			}
			// ActionAdd = 对历史仓位加仓，继续跳过
			logger.Infof("📊 [%s] 历史仓位加仓 | posId=%s status=ignored → 跳过",
				e.traderID, posID)
		}
	}

	// 优先处理新开仓
	if newPosition != nil {
		posID := newPosition.PosID
		if posID == "" {
			posID = fmt.Sprintf("%s_%s", fill.Symbol, fill.PositionSide)
		}
		logger.Infof("📊 [%s] 新开仓 | posId=%s mgnMode=%s → 跟随开仓",
			e.traderID, posID, newPosition.MarginMode)
		return &SignalMatchResult{
			ShouldFollow:   true,
			Reason:         fmt.Sprintf("新开仓(posId=%s)，跟随开仓", posID),
			Action:         ActionOpen,
			PosID:          posID,
			MarginMode:     newPosition.MarginMode,
			LeaderPosition: newPosition,
		}
	}

	// ============================================================
	// 第二轮：查找加仓（通过 lastKnownSize 变化判断）
	// 关键：找 currentSize > lastKnownSize 的仓位，说明这个仓位被加仓了
	// ============================================================
	var addPosition *Position
	var addMapping *store.CopyTradePositionMapping
	var maxSizeIncrease float64

	for _, pos := range matchedPositions {
		posID := pos.PosID
		if posID == "" {
			posID = fmt.Sprintf("%s_%s", fill.Symbol, fill.PositionSide)
		}

		mapping, err := e.store.CopyTrade().GetMapping(e.traderID, posID)
		if err != nil || mapping == nil {
			continue
		}

		if mapping.Status != "active" {
			continue
		}

		// 查找领航员当前持仓
		leaderPos, exists := leaderPosMap[posID]
		if !exists {
			continue
		}

		currentSize := leaderPos.Size
		lastKnownSize := mapping.LastKnownSize

		// 判断 size 是否增加（加仓）
		if currentSize > lastKnownSize {
			sizeIncrease := currentSize - lastKnownSize
			logger.Infof("📊 [%s] posId=%s size 变化 | 上次=%.4f 当前=%.4f 增加=%.4f",
				e.traderID, posID, lastKnownSize, currentSize, sizeIncrease)

			// 取 size 增加最多的那个仓位（防止多个仓位同时变化时的误判）
			if sizeIncrease > maxSizeIncrease {
				maxSizeIncrease = sizeIncrease
				addPosition = leaderPos
				addMapping = mapping
			}
		}
	}

	// 找到了加仓目标
	if addPosition != nil && addMapping != nil {
		posID := addPosition.PosID
		if posID == "" {
			posID = fmt.Sprintf("%s_%s", fill.Symbol, fill.PositionSide)
		}
		logger.Infof("📊 [%s] 精确匹配加仓 | posId=%s mgnMode=%s size增加=%.4f → 跟随加仓",
			e.traderID, posID, addMapping.MarginMode, maxSizeIncrease)
		return &SignalMatchResult{
			ShouldFollow:   true,
			Reason:         fmt.Sprintf("已跟随仓位(posId=%s)，加仓", posID),
			Action:         ActionAdd,
			PosID:          posID,
			MarginMode:     addMapping.MarginMode,
			LeaderPosition: addPosition,
		}
	}

	// ============================================================
	// 第三轮：兜底 - 只有一个 active 仓位时，直接加仓
	// ============================================================
	var singleActivePos *Position
	var singleActiveMapping *store.CopyTradePositionMapping
	activeCount := 0

	for _, pos := range matchedPositions {
		posID := pos.PosID
		if posID == "" {
			posID = fmt.Sprintf("%s_%s", fill.Symbol, fill.PositionSide)
		}

		mapping, err := e.store.CopyTrade().GetMapping(e.traderID, posID)
		if err != nil || mapping == nil || mapping.Status != "active" {
			continue
		}

		activeCount++
		singleActivePos = pos
		singleActiveMapping = mapping
	}

	if activeCount == 1 && singleActivePos != nil {
		posID := singleActivePos.PosID
		if posID == "" {
			posID = fmt.Sprintf("%s_%s", fill.Symbol, fill.PositionSide)
		}
		logger.Infof("📊 [%s] 唯一 active 仓位 | posId=%s status=active → 加仓",
			e.traderID, posID)
		return &SignalMatchResult{
			ShouldFollow:   true,
			Reason:         fmt.Sprintf("已跟随仓位(posId=%s)，加仓", posID),
			Action:         ActionAdd,
			PosID:          posID,
			MarginMode:     singleActiveMapping.MarginMode,
			LeaderPosition: singleActivePos,
		}
	}

	// 多个 active 仓位但无法判断加仓目标
	if activeCount > 1 {
		logger.Warnf("⚠️ [%s] 多个 active 仓位 (%d个)，无法判断加仓目标，跳过",
			e.traderID, activeCount)
		return &SignalMatchResult{
			ShouldFollow: false,
			Reason:       fmt.Sprintf("多个 %s %s active 仓位，无法判断加仓目标", fill.Symbol, fill.PositionSide),
		}
	}

	// 所有仓位都是 ignored
	return &SignalMatchResult{
		ShouldFollow: false,
		Reason:       fmt.Sprintf("所有 %s %s 仓位都是历史仓位，不跟随", fill.Symbol, fill.PositionSide),
	}
}

// matchCloseReduceSignal 匹配减仓/平仓信号（反向查找法 + posId 精确匹配）
// 核心思想：从本地 active 映射出发，通过 size 变化精确确定是哪个 posId 被操作
func (e *Engine) matchCloseReduceSignal(signal *TradeSignal, leaderPosMap map[string]*Position) *SignalMatchResult {
	fill := signal.Fill

	// 1. 查本地所有 active 映射
	activeMappings, err := e.store.CopyTrade().FindActiveBySymbolSide(e.traderID, fill.Symbol, string(fill.PositionSide))
	if err != nil {
		logger.Errorf("❌ [%s] 查询活跃映射失败: %v", e.traderID, err)
		return &SignalMatchResult{
			ShouldFollow: false,
			Reason:       fmt.Sprintf("查询活跃映射失败: %v", err),
		}
	}

	if len(activeMappings) == 0 {
		logger.Infof("📊 [%s] 无活跃映射 | %s %s → 不跟随",
			e.traderID, fill.Symbol, fill.PositionSide)
		return &SignalMatchResult{
			ShouldFollow: false,
			Reason:       fmt.Sprintf("无活跃映射(%s %s)，不跟随", fill.Symbol, fill.PositionSide),
		}
	}

	// 2. 遍历映射，通过 posId + size 变化精确匹配
	for _, mapping := range activeMappings {
		leaderPos := leaderPosMap[mapping.LeaderPosID]

		// 场景 1: posId 消失 = 全平（直接通过 posId 匹配）
		if leaderPos == nil {
			logger.Infof("📊 [%s] 领航员已平仓 | posId=%s 不在持仓列表 → 全量平仓",
				e.traderID, mapping.LeaderPosID)
			return &SignalMatchResult{
				ShouldFollow:   true,
				Reason:         fmt.Sprintf("领航员已平仓(posId=%s)", mapping.LeaderPosID),
				Action:         ActionClose,
				PosID:          mapping.LeaderPosID,
				MarginMode:     mapping.MarginMode,
				LeaderPosition: nil, // nil 表示已平仓
			}
		}

		// 场景 2: posId 还在，通过 size 变化判断是否是这个仓位被减仓
		// lastKnownSize > currentSize = 这个仓位被减仓了
		if mapping.LastKnownSize > 0 && mapping.LastKnownSize > leaderPos.Size {
			sizeDiff := mapping.LastKnownSize - leaderPos.Size
			logger.Infof("📊 [%s] posId=%s size变化 | 上次=%.4f 当前=%.4f 减少=%.4f",
				e.traderID, mapping.LeaderPosID, mapping.LastKnownSize, leaderPos.Size, sizeDiff)

			// 判断是全平还是减仓
			if leaderPos.Size < mapping.LastKnownSize*0.05 {
				// 剩余不足 5% = 视为全平
				logger.Infof("📊 [%s] 剩余(%.4f) < 5%% → 视为全平 | posId=%s",
					e.traderID, leaderPos.Size, mapping.LeaderPosID)
				return &SignalMatchResult{
					ShouldFollow:   true,
					Reason:         fmt.Sprintf("近乎全平(posId=%s)", mapping.LeaderPosID),
					Action:         ActionClose,
					PosID:          mapping.LeaderPosID,
					MarginMode:     mapping.MarginMode,
					LeaderPosition: leaderPos,
				}
			}

			// 部分减仓
			logger.Infof("📊 [%s] 部分减仓 | posId=%s 领航员剩余=%.4f",
				e.traderID, mapping.LeaderPosID, leaderPos.Size)
			return &SignalMatchResult{
				ShouldFollow:   true,
				Reason:         fmt.Sprintf("部分减仓(posId=%s)", mapping.LeaderPosID),
				Action:         ActionReduce,
				PosID:          mapping.LeaderPosID,
				MarginMode:     mapping.MarginMode,
				LeaderPosition: leaderPos,
			}
		}
	}

	// 兜底：如果只有一个映射且 lastKnownSize 为 0（旧数据），使用 fill.Size 判断
	if len(activeMappings) == 1 {
		mapping := activeMappings[0]
		leaderPos := leaderPosMap[mapping.LeaderPosID]

		if leaderPos != nil {
			// 用 fill.Size vs leaderPos.Size 判断是否是全平
			if fill.Size >= leaderPos.Size*0.95 {
				logger.Infof("📊 [%s] 减仓量(%.4f) ≈ 当前持仓(%.4f) → 视为全平 | posId=%s (兜底)",
					e.traderID, fill.Size, leaderPos.Size, mapping.LeaderPosID)
				return &SignalMatchResult{
					ShouldFollow:   true,
					Reason:         fmt.Sprintf("减仓量≈持仓量(posId=%s)，视为全平", mapping.LeaderPosID),
					Action:         ActionClose,
					PosID:          mapping.LeaderPosID,
					MarginMode:     mapping.MarginMode,
					LeaderPosition: leaderPos,
				}
			}

			// 部分减仓
			logger.Infof("📊 [%s] 部分减仓 | posId=%s 领航员剩余=%.4f (兜底)",
				e.traderID, mapping.LeaderPosID, leaderPos.Size)
			return &SignalMatchResult{
				ShouldFollow:   true,
				Reason:         fmt.Sprintf("部分减仓(posId=%s)", mapping.LeaderPosID),
				Action:         ActionReduce,
				PosID:          mapping.LeaderPosID,
				MarginMode:     mapping.MarginMode,
				LeaderPosition: leaderPos,
			}
		}
	}

	// 所有映射都在领航员持仓中，但没有 size 变化（可能是重复信号）
	logger.Infof("📊 [%s] 未检测到 size 变化 | %s %s → 跳过",
		e.traderID, fill.Symbol, fill.PositionSide)
	return &SignalMatchResult{
		ShouldFollow: false,
		Reason:       "未检测到 size 变化，可能是重复信号",
	}
}

// findLeaderPosition 在领航员持仓映射中查找指定 symbol+side 的仓位
// ============================================================================
// 信号处理（核心逻辑 - 统一入口）
// ============================================================================

func (e *Engine) processSignal(signal *TradeSignal) {
	fill := signal.Fill

	// ========================================
	// Step 1: 统一数据准备（只拉取一次）
	// ========================================
	if err := e.syncLeaderState(); err != nil {
		logger.Warnf("⚠️ [%s] 领航员状态同步失败: %v", e.traderID, err)
	}

	// 重新构建 signal 以获取最新的 LeaderEquity
	signal = e.buildSignal(fill)

	// ========================================
	// Step 2: 统一信号匹配（核心判断）
	// ========================================
	matchResult := e.matchSignalWithMapping(signal)

	if !matchResult.ShouldFollow {
		logger.Infof("🎯 [%s] ❌ 跳过 | %s | 原因: %s", e.traderID, fill.Symbol, matchResult.Reason)
		e.stats.SignalsSkipped++
		return
	}
	logger.Infof("🎯 [%s] ✅ 跟随 | %s | 原因: %s", e.traderID, fill.Symbol, matchResult.Reason)
	e.stats.SignalsFollowed++

	// 回填匹配结果到 signal（供后续逻辑使用）
	signal.LeaderPosID = matchResult.PosID
	signal.LeaderPosition = matchResult.LeaderPosition

	// ========================================
	// Step 3: 计算跟单仓位
	// ========================================
	copySize, warnings := e.calculateCopySize(signal)

	// 记录所有预警（不阻止交易）
	for _, w := range warnings {
		e.logWarning(w)
	}

	// ========================================
	// Step 4: 构造 Decision
	// ========================================
	dec := e.buildDecisionV2(signal, matchResult, copySize)

	// ========================================
	// Step 5: 推送决策
	// ========================================
	fullDec := &decision.FullDecision{
		SystemPrompt:        e.buildSystemPromptLog(),
		UserPrompt:          e.buildUserPromptLog(signal),
		CoTTrace:            e.buildCoTTrace(signal, matchResult.Action, copySize, warnings),
		Decisions:           []decision.Decision{dec},
		RawResponse:         fmt.Sprintf("Copy trade signal from %s:%s", e.config.ProviderType, e.config.LeaderID),
		Timestamp:           time.Now(),
		AIRequestDurationMs: 0,
	}

	select {
	case e.decisionCh <- fullDec:
		e.stats.DecisionsGenerated++
		logger.Infof("⚡ [%s] 决策生成 | %s %s | 金额=%.2f",
			e.traderID, dec.Action, dec.Symbol, copySize)
	default:
		logger.Warnf("⚠️ [%s] 决策通道已满，丢弃", e.traderID)
	}
}

// buildDecisionV2 构建决策（使用统一匹配结果）
func (e *Engine) buildDecisionV2(signal *TradeSignal, match *SignalMatchResult, copySize float64) decision.Decision {
	fill := signal.Fill

	// 获取领航员当前持仓数量（用于 lastKnownSize 追踪）
	leaderPosSize := float64(0)
	if match.LeaderPosition != nil {
		leaderPosSize = match.LeaderPosition.Size
	}

	dec := decision.Decision{
		Symbol:        fill.Symbol,
		Action:        e.mapAction(match.Action, fill.PositionSide),
		Reasoning:     fmt.Sprintf("Copy trading: %s following %s leader %s", match.Action, e.config.ProviderType, e.config.LeaderID),
		EntryPrice:    fill.Price,
		LeaderPosID:   match.PosID,
		LeaderPosSize: leaderPosSize,    // 传递领航员当前持仓数量
		MarginMode:    match.MarginMode, // 直接使用匹配结果中的 marginMode
	}

	// ============================================================
	// 开仓/加仓：设置仓位大小和杠杆
	// ============================================================
	if match.Action == ActionOpen || match.Action == ActionAdd {
		dec.PositionSizeUSD = copySize
		dec.Leverage = e.getLeaderLeverage(signal)
		dec.Confidence = 90
		logger.Infof("📊 [%s] %s | 金额=%.2f 杠杆=%dx 模式=%s 入场价=%.4f",
			e.traderID, match.Action, copySize, dec.Leverage, dec.MarginMode, fill.Price)
	}

	// ============================================================
	// 减仓：计算比例
	// ============================================================
	if match.Action == ActionReduce {
		ratio := e.calculateReduceRatioV2(signal, match)

		// 边界保护：减仓超过 95% 时，直接全量平仓
		if ratio >= 0.95 {
			logger.Infof("📊 [%s] 减仓比例 %.1f%% ≥ 95%%，转为全量平仓", e.traderID, ratio*100)
			dec.CloseRatio = 0
			dec.Reasoning = fmt.Sprintf("Copy trading: close (reduce %.0f%% → full close) following %s leader %s",
				ratio*100, e.config.ProviderType, e.config.LeaderID)
		} else {
			dec.CloseRatio = ratio
			dec.Reasoning = fmt.Sprintf("Copy trading: reduce %.0f%% following %s leader %s",
				ratio*100, e.config.ProviderType, e.config.LeaderID)
			logger.Infof("📊 [%s] 部分平仓 %.1f%% marginMode=%s", e.traderID, ratio*100, dec.MarginMode)
		}
	}

	// ============================================================
	// 平仓：全量平仓
	// ============================================================
	if match.Action == ActionClose {
		dec.CloseRatio = 0 // 0 = 全量平仓
		logger.Infof("📊 [%s] 全量平仓 marginMode=%s", e.traderID, dec.MarginMode)
	}

	return dec
}

// calculateReduceRatioV2 计算减仓比例（使用统一匹配结果）
func (e *Engine) calculateReduceRatioV2(signal *TradeSignal, match *SignalMatchResult) float64 {
	reduceSize := signal.Fill.Size

	leaderCurrentSize := float64(0)
	if match.LeaderPosition != nil {
		leaderCurrentSize = match.LeaderPosition.Size
	}

	// 推算减仓前的仓位 = 当前仓位 + 本次减仓量
	leaderPreviousSize := leaderCurrentSize + reduceSize

	if leaderPreviousSize <= 0 {
		logger.Infof("📊 [%s] %s 减仓比例 | 减仓量=%.4f 当前=%.4f → 100%% (异常)",
			e.traderID, signal.Fill.Symbol, reduceSize, leaderCurrentSize)
		return 1.0
	}

	ratio := reduceSize / leaderPreviousSize

	logger.Infof("📊 [%s] %s 减仓比例 | 减仓量=%.4f 当前=%.4f 减仓前=%.4f → %.1f%%",
		e.traderID, signal.Fill.Symbol, reduceSize, leaderCurrentSize, leaderPreviousSize, ratio*100)

	return ratio
}

// ============================================================================
// 比例计算
// ============================================================================

// calculateCopySize 计算跟单仓位大小
func (e *Engine) calculateCopySize(signal *TradeSignal) (float64, []Warning) {
	var warnings []Warning
	fill := signal.Fill

	// 领航员的成交价值
	leaderTradeValue := fill.Value

	// 领航员的账户权益
	leaderEquity := signal.LeaderEquity
	if leaderEquity <= 0 {
		leaderEquity = 1 // 防止除零
	}

	// 领航员该笔交易占其账户的比例
	leaderTradeRatio := leaderTradeValue / leaderEquity

	// 跟随者账户权益
	followerEquity := e.getFollowerBalance()
	if followerEquity <= 0 {
		warnings = append(warnings, Warning{
			Timestamp: time.Now(),
			Symbol:    fill.Symbol,
			Type:      "zero_balance",
			Message:   "跟随者余额为零，无法跟单",
			Executed:  false,
		})
		return 0, warnings
	}

	// 计算跟单金额
	copySize := e.config.CopyRatio * leaderTradeRatio * followerEquity

	logger.Infof("📊 [%s] 比例计算 | %s | 领航员: 交易=%.2f 权益=%.2f 占比=%.2f%% | 跟随者: 权益=%.2f 系数=%.0f%% → 跟单=%.2f",
		e.traderID, fill.Symbol,
		leaderTradeValue, leaderEquity, leaderTradeRatio*100,
		followerEquity, e.config.CopyRatio*100, copySize)

	// 最小金额检查：如果低于阈值，自动提升到阈值（解决小账户精度问题）
	// 使用配置的阈值，如果未配置则使用默认值 12 USDT
	// 🆕 从 10 提升到 12 USDT，预留精度损失余量（Hyperliquid 最小订单 $10）
	// （避免因数量精度向下取整导致订单价值不足 $10）
	minTradeThreshold := e.config.MinTradeWarn
	if minTradeThreshold <= 0 {
		minTradeThreshold = 12.0 // 默认最小 12 USDT，预留精度损失余量
	}
	if copySize > 0 && copySize < minTradeThreshold {
		originalSize := copySize
		copySize = minTradeThreshold // 自动提升到最小阈值
		logger.Infof("📊 [%s] 跟单金额 %.2f < 阈值 %.2f，自动提升到 %.2f USDT",
			e.traderID, originalSize, minTradeThreshold, copySize)
		warnings = append(warnings, Warning{
			Timestamp:   time.Now(),
			Symbol:      fill.Symbol,
			Type:        "size_boosted",
			Message:     fmt.Sprintf("跟单金额 %.2f 低于阈值，已提升到 %.2f USDT", originalSize, minTradeThreshold),
			SignalValue: leaderTradeValue,
			CopyValue:   copySize,
			Executed:    true,
		})
	}

	if e.config.MaxTradeWarn > 0 && copySize > e.config.MaxTradeWarn {
		warnings = append(warnings, Warning{
			Timestamp:   time.Now(),
			Symbol:      fill.Symbol,
			Type:        "high_value",
			Message:     fmt.Sprintf("跟单金额较大 (%.2f > %.2f)，仍执行", copySize, e.config.MaxTradeWarn),
			SignalValue: leaderTradeValue,
			CopyValue:   copySize,
			Executed:    true,
		})
	}

	return copySize, warnings
}

// getLeaderLeverage 获取领航员杠杆
// 优先级：1.信号中的持仓杠杆 2.缓存的持仓 3.默认值(10x)
func (e *Engine) getLeaderLeverage(signal *TradeSignal) int {
	// 1. 如果不同步杠杆，返回默认值
	if !e.config.SyncLeverage {
		return 10 // 默认 10x
	}

	// 2. 如果信号中有持仓信息，使用该杠杆
	if signal.LeaderPosition != nil && signal.LeaderPosition.Leverage > 0 {
		return signal.LeaderPosition.Leverage
	}

	// 3. 从缓存的领航员状态获取
	e.leaderStateMu.RLock()
	defer e.leaderStateMu.RUnlock()

	if e.leaderState != nil && e.leaderState.Positions != nil {
		for _, pos := range e.leaderState.Positions {
			if pos.Symbol == signal.Fill.Symbol && pos.Side == signal.Fill.PositionSide && pos.Leverage > 0 {
				return pos.Leverage
			}
		}
	}

	// 4. 默认值
	return 10
}

func (e *Engine) mapAction(action ActionType, side SideType) string {
	switch {
	case action == ActionOpen && side == SideLong:
		return "open_long"
	case action == ActionOpen && side == SideShort:
		return "open_short"
	case action == ActionAdd && side == SideLong:
		return "open_long" // 加仓用 open，在 updatePositionMapping 中通过数据库区分
	case action == ActionAdd && side == SideShort:
		return "open_short"
	case action == ActionClose && side == SideLong:
		return "close_long"
	case action == ActionClose && side == SideShort:
		return "close_short"
	case action == ActionReduce && side == SideLong:
		return "reduce_long" // 减仓用 reduce，与平仓区分
	case action == ActionReduce && side == SideShort:
		return "reduce_short"
	default:
		return "hold"
	}
}

// ============================================================================
// 日志构建
// ============================================================================

func (e *Engine) buildSystemPromptLog() string {
	return fmt.Sprintf(`# Copy Trading Mode

Provider: %s
Leader ID: %s
Copy Ratio: %.0f%%

## Core Rules:
- Only follow new positions (not leader's historical positions)
- Unconditional execution (warnings are for logging only)
- Sync Leverage: %v
`, e.config.ProviderType, e.config.LeaderID, e.config.CopyRatio*100, e.config.SyncLeverage)
}

func (e *Engine) buildUserPromptLog(signal *TradeSignal) string {
	fill := signal.Fill
	return fmt.Sprintf(`## Trade Signal

Time: %s
Symbol: %s
Action: %s %s
Price: %.4f
Size: %.4f (%.2f USDT)
Leader Equity: %.2f USDT
`,
		fill.Timestamp.Format("2006-01-02 15:04:05"),
		fill.Symbol, fill.Action, fill.PositionSide,
		fill.Price, fill.Size, fill.Value,
		signal.LeaderEquity)
}

func (e *Engine) buildCoTTrace(signal *TradeSignal, action ActionType, copySize float64, warnings []Warning) string {
	fill := signal.Fill

	warningSection := ""
	if len(warnings) > 0 {
		warningSection = "\n## ⚠️ Warnings (Not Blocking)\n"
		for _, w := range warnings {
			warningSection += fmt.Sprintf("- [%s] %s\n", w.Type, w.Message)
		}
	}

	return fmt.Sprintf(`# Copy Trading Decision

## Signal
- Symbol: %s
- Action: %s → %s
- Price: %.4f
- Value: %.2f USDT

## Calculation
- Leader Equity: %.2f USDT
- Trade Ratio: %.4f%%
- Follower Equity: %.2f USDT
- Copy Ratio: %.0f%%
- Copy Size: %.2f USDT
%s
## Decision
Following leader's %s action on %s.
`,
		fill.Symbol, fill.Action, action,
		fill.Price, fill.Value,
		signal.LeaderEquity, (fill.Value/signal.LeaderEquity)*100,
		e.getFollowerBalance(), e.config.CopyRatio*100, copySize,
		warningSection,
		action, fill.Symbol)
}

// ============================================================================
// 辅助方法
// ============================================================================

func (e *Engine) syncLeaderState() error {
	state, err := e.provider.GetAccountState(e.config.LeaderID)
	if err != nil {
		return err
	}

	e.leaderStateMu.Lock()
	e.leaderState = state
	e.lastStateSync = time.Now()
	e.leaderStateMu.Unlock()

	logger.Debugf("👁️ [%s] 领航员状态同步 | 权益=%.2f 持仓数=%d",
		e.traderID, state.TotalEquity, len(state.Positions))

	return nil
}

func (e *Engine) initSeenFills() {
	since := time.Now().Add(-5 * time.Minute)
	fills, err := e.provider.GetFills(e.config.LeaderID, since)
	if err != nil {
		logger.Warnf("⚠️ [%s] 初始化去重基线失败: %v", e.traderID, err)
		return
	}

	for _, fill := range fills {
		e.markSeen(fill.ID)
	}

	logger.Infof("🔧 [%s] 去重基线初始化完成 | 已标记 %d 条历史成交", e.traderID, len(fills))
}

func (e *Engine) isSeen(id string) bool {
	e.seenMu.RLock()
	defer e.seenMu.RUnlock()

	seenTime, exists := e.seenFills[id]
	if !exists {
		return false
	}

	if time.Since(seenTime) > e.seenTTL {
		return false // 已过期
	}

	return true
}

func (e *Engine) markSeen(id string) {
	e.seenMu.Lock()
	defer e.seenMu.Unlock()

	e.seenFills[id] = time.Now()

	// 定期清理过期记录
	if len(e.seenFills) > 1000 && len(e.seenFills)%100 == 0 {
		e.cleanExpiredFills()
	}
}

func (e *Engine) cleanExpiredFills() {
	now := time.Now()
	for id, seenTime := range e.seenFills {
		if now.Sub(seenTime) > e.seenTTL {
			delete(e.seenFills, id)
		}
	}
	logger.Debugf("🧹 [%s] 清理过期去重记录，剩余 %d 条", e.traderID, len(e.seenFills))
}

func (e *Engine) logWarning(w Warning) {
	e.warningsMu.Lock()
	e.warnings = append(e.warnings, w)
	e.stats.WarningsCount++
	e.warningsMu.Unlock()

	logger.Warnf("⚠️ [%s] 预警:%s | %s | %s", e.traderID, w.Type, w.Symbol, w.Message)
}
