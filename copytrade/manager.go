package copytrade

import (
	"context"
	"fmt"
	"sync"

	"nofx/decision"
	"nofx/logger"
)

// Manager 跟单管理器
// 管理多个跟单引擎实例，每个 trader_id 一个引擎
type Manager struct {
	engines map[string]*Engine
	mu      sync.RWMutex
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewManager 创建跟单管理器
func NewManager() *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		engines: make(map[string]*Engine),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// StartEngine 为指定 trader 启动跟单引擎
func (m *Manager) StartEngine(
	traderID string,
	config *CopyConfig,
	getBalance func() float64,
	getPositions func() map[string]*Position,
) (<-chan *decision.FullDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查是否已存在
	if engine, exists := m.engines[traderID]; exists {
		if engine.running {
			return engine.GetDecisionChannel(), fmt.Errorf("engine already running for trader %s", traderID)
		}
		// 已存在但未运行，先删除旧的
		delete(m.engines, traderID)
	}

	// 创建新引擎
	engine, err := NewEngine(traderID, config, getBalance, getPositions)
	if err != nil {
		return nil, fmt.Errorf("create engine failed: %w", err)
	}

	// 启动引擎
	if err := engine.Start(m.ctx); err != nil {
		return nil, fmt.Errorf("start engine failed: %w", err)
	}

	m.engines[traderID] = engine

	logger.Infof("🔧 [%s] 跟单管理器: 引擎已启动 | provider=%s leader=%s",
		traderID, config.ProviderType, config.LeaderID)

	return engine.GetDecisionChannel(), nil
}

// StopEngine 停止指定 trader 的跟单引擎
func (m *Manager) StopEngine(traderID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	engine, exists := m.engines[traderID]
	if !exists {
		return fmt.Errorf("engine not found for trader %s", traderID)
	}

	engine.Stop()
	delete(m.engines, traderID)

	logger.Infof("🔧 [%s] 跟单管理器: 引擎已停止", traderID)

	return nil
}

// RestartEngine 重启指定 trader 的跟单引擎（配置更新时使用）
func (m *Manager) RestartEngine(
	traderID string,
	config *CopyConfig,
	getBalance func() float64,
	getPositions func() map[string]*Position,
) (<-chan *decision.FullDecision, error) {
	// 先停止
	_ = m.StopEngine(traderID)

	// 再启动
	return m.StartEngine(traderID, config, getBalance, getPositions)
}

// GetEngine 获取指定 trader 的引擎
func (m *Manager) GetEngine(traderID string) *Engine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.engines[traderID]
}

// GetStats 获取指定 trader 的统计信息
func (m *Manager) GetStats(traderID string) *EngineStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	engine, exists := m.engines[traderID]
	if !exists {
		return nil
	}

	return engine.GetStats()
}

// ListEngines 列出所有运行中的引擎
func (m *Manager) ListEngines() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var traderIDs []string
	for id := range m.engines {
		traderIDs = append(traderIDs, id)
	}
	return traderIDs
}

// IsRunning 检查指定 trader 的引擎是否在运行
func (m *Manager) IsRunning(traderID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	engine, exists := m.engines[traderID]
	if !exists {
		return false
	}

	engine.mu.RLock()
	defer engine.mu.RUnlock()
	return engine.running
}

// Shutdown 关闭所有引擎
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.cancel() // 取消上下文，通知所有引擎停止

	for traderID, engine := range m.engines {
		engine.Stop()
		logger.Infof("🔧 [%s] 跟单引擎已关闭", traderID)
	}

	m.engines = make(map[string]*Engine)

	logger.Infof("🔧 跟单管理器: 所有引擎已关闭")
}

// ============================================================================
// 全局单例（可选使用）
// ============================================================================

var (
	globalManager *Manager
	globalOnce    sync.Once
)

// GetGlobalManager 获取全局跟单管理器
func GetGlobalManager() *Manager {
	globalOnce.Do(func() {
		globalManager = NewManager()
	})
	return globalManager
}
