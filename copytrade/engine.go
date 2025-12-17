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

	// 跟随者账户信息（由外部注入）
	getFollowerBalance   func() float64
	getFollowerPositions func() map[string]*Position

	// 去重（使用时间戳过期）
	seenFills map[string]time.Time
	seenMu    sync.RWMutex
	seenTTL   time.Duration

	// 状态缓存
	leaderState     *AccountState
	leaderStateMu   sync.RWMutex
	lastStateSync   time.Time
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

// NewEngine 创建跟单引擎
func NewEngine(
	traderID string,
	config *CopyConfig,
	getBalance func() float64,
	getPositions func() map[string]*Position,
	opts ...EngineOption,
) (*Engine, error) {
	provider, err := NewProvider(config.ProviderType)
	if err != nil {
		return nil, err
	}

	e := &Engine{
		traderID:             traderID,
		config:               config,
		provider:             provider,
		getFollowerBalance:   getBalance,
		getFollowerPositions: getPositions,
		seenFills:            make(map[string]time.Time),
		seenTTL:              1 * time.Hour,
		stateSyncInterval:    30 * time.Second,
		decisionCh:           make(chan *decision.FullDecision, 10),
		stopCh:               make(chan struct{}),
		stats:                &EngineStats{StartTime: time.Now()},
	}

	for _, opt := range opts {
		opt(e)
	}

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

	logger.Infof("🚀 [%s] 跟单引擎启动 | provider=%s leader=%s ratio=%.0f%%",
		e.traderID, e.config.ProviderType, e.config.LeaderID, e.config.CopyRatio*100)

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
		key := PositionKey(fill.Symbol, fill.PositionSide)
		if pos, ok := e.leaderState.Positions[key]; ok {
			signal.LeaderPosition = pos
		}
	}

	return signal
}

// ============================================================================
// 信号处理（核心逻辑）
// ============================================================================

func (e *Engine) processSignal(signal *TradeSignal) {
	fill := signal.Fill

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

// shouldFollowSignal 🎯 核心：判断是否应该跟随该信号（只跟新开仓原则）
func (e *Engine) shouldFollowSignal(signal *TradeSignal) (follow bool, reason string) {
	fill := signal.Fill

	// 获取本地仓位（从交易所/数据库，不是内存）
	localPositions := e.getFollowerPositions()
	key := PositionKey(fill.Symbol, fill.PositionSide)
	localPosition := localPositions[key]
	hasLocalPosition := localPosition != nil && localPosition.Size > 0

	switch fill.Action {
	case ActionOpen:
		if !hasLocalPosition {
			return true, "新开仓信号，本地无持仓"
		}
		return true, "加仓信号，跟随已有仓位"

	case ActionAdd:
		if !hasLocalPosition {
			return false, "忽略：领航员历史仓位加仓，我们未跟随该仓位"
		}
		return true, "加仓信号，跟随已有仓位"

	case ActionReduce, ActionClose:
		if !hasLocalPosition {
			return false, "忽略：领航员历史仓位操作，我们未跟随该仓位"
		}
		return true, "平仓/减仓信号，跟随已有仓位"

	default:
		return false, fmt.Sprintf("未知操作类型: %s", fill.Action)
	}
}

// determineAction 判断实际动作类型（减仓 vs 平仓）
func (e *Engine) determineAction(signal *TradeSignal) ActionType {
	fill := signal.Fill

	// 开仓/加仓直接返回
	if fill.Action == ActionOpen || fill.Action == ActionAdd {
		return fill.Action
	}

	// 减仓/平仓：通过领航员实时持仓判断
	if signal.LeaderPosition == nil || signal.LeaderPosition.Size == 0 {
		return ActionClose // 领航员仓位清零 = 平仓
	}
	return ActionReduce // 领航员仓位仍有 = 减仓
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

	// 预警检查（不阻止交易）
	if e.config.MinTradeWarn > 0 && copySize < e.config.MinTradeWarn {
		warnings = append(warnings, Warning{
			Timestamp:    time.Now(),
			Symbol:       fill.Symbol,
			Type:         "low_value",
			Message:      fmt.Sprintf("跟单金额较小 (%.2f < %.2f)，仍执行", copySize, e.config.MinTradeWarn),
			SignalValue:  leaderTradeValue,
			CopyValue:    copySize,
			Executed:     true,
		})
	}

	if e.config.MaxTradeWarn > 0 && copySize > e.config.MaxTradeWarn {
		warnings = append(warnings, Warning{
			Timestamp:    time.Now(),
			Symbol:       fill.Symbol,
			Type:         "high_value",
			Message:      fmt.Sprintf("跟单金额较大 (%.2f > %.2f)，仍执行", copySize, e.config.MaxTradeWarn),
			SignalValue:  leaderTradeValue,
			CopyValue:    copySize,
			Executed:     true,
		})
	}

	return copySize, warnings
}

// calculateReduceRatio 计算减仓比例
func (e *Engine) calculateReduceRatio(signal *TradeSignal) float64 {
	reduceSize := signal.Fill.Size

	leaderCurrentSize := float64(0)
	if signal.LeaderPosition != nil {
		leaderCurrentSize = signal.LeaderPosition.Size
	}
	leaderPreviousSize := leaderCurrentSize + reduceSize

	if leaderPreviousSize <= 0 {
		return 1.0 // 全部平仓
	}

	ratio := reduceSize / leaderPreviousSize

	logger.Debugf("📊 [%s] 减仓计算 | 减仓量=%.4f 减仓前=%.4f → 比例=%.2f%%",
		e.traderID, reduceSize, leaderPreviousSize, ratio*100)

	return ratio
}

// ============================================================================
// Decision 构建
// ============================================================================

func (e *Engine) buildDecision(signal *TradeSignal, action ActionType, copySize float64) decision.Decision {
	fill := signal.Fill

	dec := decision.Decision{
		Symbol:    fill.Symbol,
		Action:    e.mapAction(action, fill.PositionSide),
		Reasoning: fmt.Sprintf("Copy trading: %s following %s leader %s", action, e.config.ProviderType, e.config.LeaderID),
	}

	// 开仓/加仓参数
	if action == ActionOpen || action == ActionAdd {
		dec.PositionSizeUSD = copySize

		// 获取领航员杠杆
		dec.Leverage = e.getLeaderLeverage(signal)
		logger.Infof("📊 [%s] 跟单杠杆: %dx (SyncLeverage=%v)", e.traderID, dec.Leverage, e.config.SyncLeverage)

		dec.Confidence = 90
	}

	// 减仓参数
	if action == ActionReduce {
		ratio := e.calculateReduceRatio(signal)
		dec.CloseRatio = ratio // 设置减仓比例，执行层将按此比例部分平仓
		dec.Reasoning = fmt.Sprintf("Copy trading: reduce %.0f%% following %s leader %s",
			ratio*100, e.config.ProviderType, e.config.LeaderID)
		logger.Infof("📊 [%s] 减仓比例: %.0f%% (非全量平仓)", e.traderID, ratio*100)
	}

	// 平仓参数 - CloseRatio 保持 0 表示全量平仓
	if action == ActionClose {
		dec.CloseRatio = 0 // 0 = 全量平仓
		logger.Infof("📊 [%s] 全量平仓信号", e.traderID)
	}

	return dec
}

// getLeaderLeverage 获取领航员杠杆
// 优先级：1.信号中的持仓杠杆 2.实时获取 3.默认值
func (e *Engine) getLeaderLeverage(signal *TradeSignal) int {
	// 1. 如果不同步杠杆，返回默认值
	if !e.config.SyncLeverage {
		return 10 // 默认 10x
	}

	// 2. 如果信号中有持仓信息，使用该杠杆
	if signal.LeaderPosition != nil && signal.LeaderPosition.Leverage > 0 {
		return signal.LeaderPosition.Leverage
	}

	// 3. 实时获取领航员当前持仓的杠杆
	if e.provider != nil {
		state, err := e.provider.GetAccountState(e.config.LeaderID)
		if err == nil && state.Positions != nil {
			key := PositionKey(signal.Fill.Symbol, signal.Fill.PositionSide)
			if pos, ok := state.Positions[key]; ok && pos.Leverage > 0 {
				logger.Infof("🔍 [%s] 实时获取领航员 %s 杠杆: %dx", e.traderID, signal.Fill.Symbol, pos.Leverage)
				return pos.Leverage
			}
		}
	}

	// 4. 默认值
	logger.Warnf("⚠️ [%s] 无法获取领航员杠杆，使用默认值 10x", e.traderID)
	return 10
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

