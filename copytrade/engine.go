package copytrade

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"nofx/decision"
	"nofx/logger"
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

	// 去重（使用时间戳过期）
	seenFills map[string]time.Time
	seenMu    sync.RWMutex
	seenTTL   time.Duration

	// 状态缓存
	leaderState       *AccountState
	prevLeaderState   *AccountState // 上一次状态（用于对比变化）
	leaderStateMu     sync.RWMutex
	lastStateSync     time.Time
	stateSyncInterval time.Duration
	
	// 持仓变化追踪（用于 OKX 区分全仓/逐仓）
	changedPositions map[string]*Position // symbol_side -> 最近变化的仓位

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

		// 附加该币种的持仓信息
		// 优先级：1.变化追踪的仓位 2.当前持仓查找
		baseKey := fill.Symbol + "_" + string(fill.PositionSide)
		
		// 1. 优先使用变化追踪的仓位（最精确）
		if changedPos, ok := e.changedPositions[baseKey]; ok {
			signal.LeaderPosition = changedPos
			logger.Debugf("📊 [%s] 使用变化追踪的仓位 | %s %s mgnMode=%s", 
				e.traderID, fill.Symbol, fill.PositionSide, changedPos.MarginMode)
		} else {
			// 2. 回退：从当前持仓中查找（兼容单仓位场景）
			for _, pos := range e.leaderState.Positions {
				if pos.Symbol == fill.Symbol && pos.Side == fill.PositionSide {
					signal.LeaderPosition = pos
					break
				}
			}
		}
	}

	return signal
}

// ============================================================================
// 信号处理（核心逻辑）
// ============================================================================

func (e *Engine) processSignal(signal *TradeSignal) {
	fill := signal.Fill

	// 🔄 强制同步领航员状态
	// - Open/Add: 获取最新持仓信息（OKX 需要区分全仓/逐仓）
	// - Close: 获取准确的剩余仓位（判断减仓 vs 平仓）
	needSync := fill.Action == ActionClose || fill.Action == ActionOpen || fill.Action == ActionAdd
	if needSync {
		if err := e.syncLeaderState(); err != nil {
			logger.Warnf("⚠️ [%s] %s 操作前状态同步失败: %v", e.traderID, fill.Action, err)
		} else {
			// 重新构建 signal 以使用最新状态
			signal = e.buildSignal(fill)
			logger.Debugf("🔄 [%s] %s 操作已刷新领航员状态", e.traderID, fill.Action)
		}
	}

	// 1. 🎯 核心规则：只跟新开仓（本地仓位对比法）
	follow, reason := e.shouldFollowSignal(signal)
	if !follow {
		logger.Infof("🎯 [%s] ❌ 跳过 | %s | 原因: %s", e.traderID, fill.Symbol, reason)
		e.stats.SignalsSkipped++
		return
	}
	logger.Infof("🎯 [%s] ✅ 跟随 | %s | 原因: %s", e.traderID, fill.Symbol, reason)
	e.stats.SignalsFollowed++

	// 2. 判断实际动作类型（减仓 vs 平仓）
	actualAction := e.determineAction(signal)

	// 3. 计算跟单仓位（带预警，不限制）
	copySize, warnings := e.calculateCopySize(signal)

	// 4. 记录所有预警（不阻止交易）
	for _, w := range warnings {
		e.logWarning(w)
	}

	// 5. 构造 Decision
	dec := e.buildDecision(signal, actualAction, copySize)

	// 6. 包装为 FullDecision
	fullDec := &decision.FullDecision{
		SystemPrompt:        e.buildSystemPromptLog(),
		UserPrompt:          e.buildUserPromptLog(signal),
		CoTTrace:            e.buildCoTTrace(signal, actualAction, copySize, warnings),
		Decisions:           []decision.Decision{dec},
		RawResponse:         fmt.Sprintf("Copy trade signal from %s:%s", e.config.ProviderType, e.config.LeaderID),
		Timestamp:           time.Now(),
		AIRequestDurationMs: 0,
	}

	// 7. 推送决策
	select {
	case e.decisionCh <- fullDec:
		e.stats.DecisionsGenerated++
		logger.Infof("⚡ [%s] 决策生成 | %s %s | 金额=%.2f",
			e.traderID, dec.Action, dec.Symbol, copySize)
	default:
		logger.Warnf("⚠️ [%s] 决策通道已满，丢弃", e.traderID)
	}
}

// shouldFollowSignal 🎯 核心规则：只跟新开仓（本地仓位对比法）
// ============================================================
// 判断逻辑：
//   - 本地有仓位 → 跟随（加仓/减仓/平仓）
//   - 本地无仓位 + 领航员开仓 → 跟随（新开仓）
//   - 本地无仓位 + 领航员加仓/减仓/平仓 → 跳过（历史仓位操作）
//
// OKX 特殊处理：
//   - OKX API 不提供 startPosition，无法直接区分开仓/加仓
//   - 通过比较领航员当前持仓量与本次交易量来推断：
//   - 当前持仓 ≈ 本次交易量 → 新开仓
//   - 当前持仓 > 本次交易量 * 1.2 → 历史仓位加仓
//
// ============================================================
func (e *Engine) shouldFollowSignal(signal *TradeSignal) (follow bool, reason string) {
	fill := signal.Fill

	// 获取本地仓位（实时从交易所获取）
	localPositions := e.getFollowerPositions()

	// 获取领航员的保证金模式（用于 OKX 区分全仓/逐仓）
	leaderMgnMode := ""
	if signal.LeaderPosition != nil {
		leaderMgnMode = signal.LeaderPosition.MarginMode
	}

	// 使用带保证金模式的 Key
	key := PositionKeyWithMode(fill.Symbol, fill.PositionSide, leaderMgnMode)
	localPosition := localPositions[key]
	hasLocalPosition := localPosition != nil && localPosition.Size > 0

	switch fill.Action {
	case ActionOpen:
		// 开仓信号
		if hasLocalPosition {
			return true, "开仓信号，本地已有仓位 → 跟随加仓"
		}

		// 本地无仓位时，需要判断领航员是"新开仓"还是"历史仓位加仓"
		// 🔍 通过领航员当前持仓量推断（适用于所有 Provider）
		if signal.LeaderPosition != nil {
			leaderCurrentSize := signal.LeaderPosition.Size
			thisTradeSize := fill.Size

			// 如果领航员当前持仓明显大于本次交易量，说明是历史仓位加仓
			// 阈值 1.2：允许一定误差（滑点、部分成交等）
			if leaderCurrentSize > thisTradeSize*1.2 {
				logger.Infof("📊 [%s] 历史仓位检测 | %s %s | 领航员当前持仓=%.4f > 本次交易=%.4f*1.2 → 判定为历史仓位加仓",
					e.traderID, fill.Symbol, fill.PositionSide, leaderCurrentSize, thisTradeSize)
				return false, fmt.Sprintf("忽略：领航员历史仓位加仓（当前持仓%.4f > 本次交易%.4f），我们未跟随该仓位", leaderCurrentSize, thisTradeSize)
			}
			logger.Infof("📊 [%s] 新开仓确认 | %s %s | 领航员当前持仓=%.4f ≈ 本次交易=%.4f → 确认为新开仓",
				e.traderID, fill.Symbol, fill.PositionSide, leaderCurrentSize, thisTradeSize)
		}

		return true, "新开仓，本地无持仓 → 跟随开仓"

	case ActionAdd:
		// 加仓信号：本地有仓位才跟随
		if !hasLocalPosition {
			return false, "忽略：领航员历史仓位加仓，我们未跟随该仓位"
		}
		return true, "加仓信号，本地有仓位 → 跟随加仓"

	case ActionReduce, ActionClose:
		// 减仓/平仓信号：本地有仓位才跟随
		if !hasLocalPosition {
			return false, "忽略：领航员历史仓位操作，我们未跟随该仓位"
		}
		return true, "减仓/平仓信号，本地有仓位 → 跟随操作"

	default:
		return false, fmt.Sprintf("未知操作类型: %s", fill.Action)
	}
}

// determineAction 判断实际动作类型（减仓 vs 平仓）
// 核心逻辑：通过领航员当前持仓状态判断
//   - 领航员仓位清零 → 平仓（全平我们的仓位）
//   - 领航员仓位还有 → 减仓（按比例减我们的仓位）
func (e *Engine) determineAction(signal *TradeSignal) ActionType {
	fill := signal.Fill

	// 开仓/加仓：需要检查本地是否有仓位来判断是新开仓还是加仓
	if fill.Action == ActionOpen || fill.Action == ActionAdd {
		// 检查本地是否已有仓位
		localPositions := e.getFollowerPositions()

		// 获取领航员的保证金模式（用于 OKX 区分全仓/逐仓）
		leaderMgnMode := ""
		if signal.LeaderPosition != nil {
			leaderMgnMode = signal.LeaderPosition.MarginMode
		}

		// 使用带保证金模式的 Key（OKX: 区分全仓/逐仓独立仓位）
		key := PositionKeyWithMode(fill.Symbol, fill.PositionSide, leaderMgnMode)

		// 🔍 调试日志：显示本地所有持仓
		logger.Infof("📊 [%s] 本地持仓检查 | 信号: %s %s mgnMode=%s | 查找 key=%s",
			e.traderID, fill.Symbol, fill.PositionSide, leaderMgnMode, key)
		for k, v := range localPositions {
			logger.Infof("   - 持仓: %s | 方向=%s 模式=%s 数量=%.4f", k, v.Side, v.MarginMode, v.Size)
		}

		localPosition := localPositions[key]
		hasLocalPosition := localPosition != nil && localPosition.Size > 0

		if hasLocalPosition {
			// 本地已有仓位 → 加仓
			logger.Infof("📊 [%s] %s %s %s → 加仓 | 本地已有仓位 %.4f", e.traderID, fill.Symbol, fill.PositionSide, leaderMgnMode, localPosition.Size)
			return ActionAdd
		}
		// 本地无仓位 → 新开仓
		logger.Infof("📊 [%s] %s %s %s → 新开仓 | 本地无此方向+模式持仓", e.traderID, fill.Symbol, fill.PositionSide, leaderMgnMode)
		return ActionOpen
	}

	// ============================================================
	// 减仓 vs 平仓判断：通过领航员实时持仓状态
	// 这和"只跟新开仓"原则一致：都是通过持仓状态对比来决策
	// ============================================================

	if signal.LeaderPosition == nil {
		logger.Infof("📊 [%s] %s → 平仓 | 原因: 领航员持仓数据为空（可能已清仓）", e.traderID, fill.Symbol)
		return ActionClose
	}

	if signal.LeaderPosition.Size == 0 {
		logger.Infof("📊 [%s] %s → 平仓 | 原因: 领航员仓位已清零", e.traderID, fill.Symbol)
		return ActionClose
	}

	logger.Infof("📊 [%s] %s → 减仓 | 领航员剩余仓位=%.4f（非清零，按比例减仓）", e.traderID, fill.Symbol, signal.LeaderPosition.Size)
	return ActionReduce
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
	// 使用配置的阈值，如果未配置则使用默认值 5 USDT
	minTradeThreshold := e.config.MinTradeWarn
	if minTradeThreshold <= 0 {
		minTradeThreshold = 5.0 // 默认最小 5 USDT，确保能通过交易所精度要求
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

// calculateReduceRatio 计算减仓比例
// 公式: 减仓比例 = 本次减仓量 / 减仓前总仓位
// 例如: 领航员从 0.03 ETH 减到 0.02 ETH，减仓量=0.01，比例=0.01/0.03=33%
func (e *Engine) calculateReduceRatio(signal *TradeSignal) float64 {
	reduceSize := signal.Fill.Size // 本次减仓数量

	// 获取领航员当前剩余仓位
	leaderCurrentSize := float64(0)
	if signal.LeaderPosition != nil {
		leaderCurrentSize = signal.LeaderPosition.Size
	}

	// 推算减仓前的仓位 = 当前仓位 + 本次减仓量
	leaderPreviousSize := leaderCurrentSize + reduceSize

	// 边界检查
	if leaderPreviousSize <= 0 {
		logger.Infof("📊 [%s] %s 减仓比例 | 减仓量=%.4f 当前=%.4f 减仓前=%.4f → 100%% (异常，视为全平)",
			e.traderID, signal.Fill.Symbol, reduceSize, leaderCurrentSize, leaderPreviousSize)
		return 1.0
	}

	ratio := reduceSize / leaderPreviousSize

	logger.Infof("📊 [%s] %s 减仓比例 | 减仓量=%.4f 当前=%.4f 减仓前=%.4f → %.1f%%",
		e.traderID, signal.Fill.Symbol, reduceSize, leaderCurrentSize, leaderPreviousSize, ratio*100)

	return ratio
}

// ============================================================================
// Decision 构建
// ============================================================================

func (e *Engine) buildDecision(signal *TradeSignal, action ActionType, copySize float64) decision.Decision {
	fill := signal.Fill

	dec := decision.Decision{
		Symbol:     fill.Symbol,
		Action:     e.mapAction(action, fill.PositionSide),
		Reasoning:  fmt.Sprintf("Copy trading: %s following %s leader %s", action, e.config.ProviderType, e.config.LeaderID),
		EntryPrice: fill.Price, // 记录领航员成交价格，用于前端显示
	}

	// ============================================================
	// 开仓/加仓：设置仓位大小和杠杆
	// ============================================================
	if action == ActionOpen || action == ActionAdd {
		dec.PositionSizeUSD = copySize
		dec.Leverage = e.getLeaderLeverage(signal)
		dec.MarginMode = e.getLeaderMarginMode(signal)
		dec.Confidence = 90
		logger.Infof("📊 [%s] %s | 金额=%.2f 杠杆=%dx 模式=%s 入场价=%.4f", e.traderID, action, copySize, dec.Leverage, dec.MarginMode, fill.Price)
	}

	// ============================================================
	// 减仓：计算比例，按比例部分平仓
	// ============================================================
	if action == ActionReduce {
		ratio := e.calculateReduceRatio(signal)

		// 边界保护：减仓超过 95% 时，直接全量平仓
		// 避免因精度问题导致 CloseRatio=1.0 时执行层误判
		if ratio >= 0.95 {
			logger.Infof("📊 [%s] 减仓比例 %.1f%% ≥ 95%%，转为全量平仓", e.traderID, ratio*100)
			dec.CloseRatio = 0 // 0 = 全量平仓
			dec.Reasoning = fmt.Sprintf("Copy trading: close (reduce %.0f%% → full close) following %s leader %s",
				ratio*100, e.config.ProviderType, e.config.LeaderID)
		} else {
			dec.CloseRatio = ratio
			dec.Reasoning = fmt.Sprintf("Copy trading: reduce %.0f%% following %s leader %s",
				ratio*100, e.config.ProviderType, e.config.LeaderID)
			logger.Infof("📊 [%s] 部分平仓 %.1f%%", e.traderID, ratio*100)
		}
	}

	// ============================================================
	// 平仓：全量平仓
	// ============================================================
	if action == ActionClose {
		dec.CloseRatio = 0 // 0 = 全量平仓
		logger.Infof("📊 [%s] 全量平仓", e.traderID)
	}

	return dec
}

// getLeaderLeverage 获取领航员杠杆
// 优先级：1.信号中的持仓杠杆 2.变化追踪的仓位 3.缓存的持仓 4.默认值(10x)
// 优化：使用缓存数据，避免重复 API 调用
func (e *Engine) getLeaderLeverage(signal *TradeSignal) int {
	// 1. 如果不同步杠杆，返回默认值
	if !e.config.SyncLeverage {
		return 10 // 默认 10x
	}

	// 2. 如果信号中有持仓信息，使用该杠杆
	if signal.LeaderPosition != nil && signal.LeaderPosition.Leverage > 0 {
		return signal.LeaderPosition.Leverage
	}

	// 3. 从变化追踪的仓位获取（最近操作的仓位）
	baseKey := signal.Fill.Symbol + "_" + string(signal.Fill.PositionSide)
	e.leaderStateMu.RLock()
	if changedPos, ok := e.changedPositions[baseKey]; ok && changedPos.Leverage > 0 {
		e.leaderStateMu.RUnlock()
		logger.Infof("🔍 [%s] 从变化追踪获取领航员 %s 杠杆: %dx", e.traderID, signal.Fill.Symbol, changedPos.Leverage)
		return changedPos.Leverage
	}

	// 4. 从缓存的领航员状态获取（避免重复 API 调用）
	if e.leaderState != nil && e.leaderState.Positions != nil {
		for _, pos := range e.leaderState.Positions {
			if pos.Symbol == signal.Fill.Symbol && pos.Side == signal.Fill.PositionSide && pos.Leverage > 0 {
				e.leaderStateMu.RUnlock()
				logger.Infof("🔍 [%s] 从缓存获取领航员 %s 杠杆: %dx", e.traderID, signal.Fill.Symbol, pos.Leverage)
				return pos.Leverage
			}
		}
	}
	e.leaderStateMu.RUnlock()

	// 5. 默认值
	logger.Warnf("⚠️ [%s] 无法获取领航员杠杆，使用默认值 10x", e.traderID)
	return 10
}

// getLeaderMarginMode 获取领航员保证金模式
// 优先级：1.信号中的持仓模式 2.变化追踪的仓位 3.缓存的持仓 4.默认值(cross)
// 优化：使用缓存数据，避免重复 API 调用
func (e *Engine) getLeaderMarginMode(signal *TradeSignal) string {
	// 1. 如果不同步保证金模式，返回默认值
	if !e.config.SyncMarginMode {
		return "cross" // 默认全仓
	}

	// 2. 如果信号中有持仓信息，使用该模式
	if signal.LeaderPosition != nil && signal.LeaderPosition.MarginMode != "" {
		logger.Infof("🔍 [%s] 使用领航员 %s 保证金模式: %s", e.traderID, signal.Fill.Symbol, signal.LeaderPosition.MarginMode)
		return signal.LeaderPosition.MarginMode
	}

	// 3. 从变化追踪的仓位获取（最近操作的仓位，最精确）
	baseKey := signal.Fill.Symbol + "_" + string(signal.Fill.PositionSide)
	e.leaderStateMu.RLock()
	if changedPos, ok := e.changedPositions[baseKey]; ok && changedPos.MarginMode != "" {
		e.leaderStateMu.RUnlock()
		logger.Infof("🔍 [%s] 从变化追踪获取领航员 %s 保证金模式: %s", e.traderID, signal.Fill.Symbol, changedPos.MarginMode)
		return changedPos.MarginMode
	}

	// 4. 从缓存的领航员状态获取（避免重复 API 调用）
	if e.leaderState != nil && e.leaderState.Positions != nil {
		for _, pos := range e.leaderState.Positions {
			if pos.Symbol == signal.Fill.Symbol && pos.Side == signal.Fill.PositionSide && pos.MarginMode != "" {
				e.leaderStateMu.RUnlock()
				logger.Infof("🔍 [%s] 从缓存获取领航员 %s 保证金模式: %s", e.traderID, signal.Fill.Symbol, pos.MarginMode)
				return pos.MarginMode
			}
		}
	}
	e.leaderStateMu.RUnlock()

	// 5. 默认值
	logger.Warnf("⚠️ [%s] 无法获取领航员保证金模式，使用默认值 cross", e.traderID)
	return "cross"
}

func (e *Engine) mapAction(action ActionType, side SideType) string {
	switch {
	case action == ActionOpen && side == SideLong:
		return "open_long"
	case action == ActionOpen && side == SideShort:
		return "open_short"
	case action == ActionAdd && side == SideLong:
		return "open_long"
	case action == ActionAdd && side == SideShort:
		return "open_short"
	case action == ActionClose && side == SideLong:
		return "close_long"
	case action == ActionClose && side == SideShort:
		return "close_short"
	case action == ActionReduce && side == SideLong:
		return "close_long" // 减仓也用 close，执行层处理数量
	case action == ActionReduce && side == SideShort:
		return "close_short"
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
	// 对比新旧持仓，找出变化的仓位（用于 OKX 区分全仓/逐仓）
	e.detectPositionChanges(e.leaderState, state)
	
	// 保存历史状态
	e.prevLeaderState = e.leaderState
	e.leaderState = state
	e.lastStateSync = time.Now()
	e.leaderStateMu.Unlock()

	logger.Debugf("👁️ [%s] 领航员状态同步 | 权益=%.2f 持仓数=%d",
		e.traderID, state.TotalEquity, len(state.Positions))

	return nil
}

// detectPositionChanges 对比新旧持仓，找出变化的仓位
// 核心逻辑：通过持仓数量变化来确定哪个仓位（全仓/逐仓）发生了操作
func (e *Engine) detectPositionChanges(oldState, newState *AccountState) {
	if e.changedPositions == nil {
		e.changedPositions = make(map[string]*Position)
	}
	
	if newState == nil || newState.Positions == nil {
		return
	}
	
	// 获取旧持仓 map
	oldPositions := make(map[string]*Position)
	if oldState != nil && oldState.Positions != nil {
		oldPositions = oldState.Positions
	}
	
	// 遍历新持仓，对比变化
	for key, newPos := range newState.Positions {
		oldPos := oldPositions[key]
		
		// 基础 key（不含 mgnMode）用于快速查找
		baseKey := newPos.Symbol + "_" + string(newPos.Side)
		
		if oldPos == nil {
			// 新增仓位
			e.changedPositions[baseKey] = newPos
			logger.Debugf("📊 [%s] 持仓变化检测 | 新增: %s %s %s size=%.4f", 
				e.traderID, newPos.Symbol, newPos.Side, newPos.MarginMode, newPos.Size)
		} else if absFloat(newPos.Size-oldPos.Size) > 0.0001 {
			// 仓位数量变化
			e.changedPositions[baseKey] = newPos
			logger.Debugf("📊 [%s] 持仓变化检测 | 变化: %s %s %s size: %.4f → %.4f", 
				e.traderID, newPos.Symbol, newPos.Side, newPos.MarginMode, oldPos.Size, newPos.Size)
		}
	}
	
	// 检测平仓（旧有新无）
	for key, oldPos := range oldPositions {
		if _, exists := newState.Positions[key]; !exists {
			baseKey := oldPos.Symbol + "_" + string(oldPos.Side)
			// 仓位消失，记录最后的模式
			e.changedPositions[baseKey] = oldPos
			logger.Debugf("📊 [%s] 持仓变化检测 | 清仓: %s %s %s", 
				e.traderID, oldPos.Symbol, oldPos.Side, oldPos.MarginMode)
		}
	}
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
