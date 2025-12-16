package copytrade

import (
	"context"
	"fmt"
	"time"

	"nofx/decision"
	"nofx/logger"
	"nofx/store"
	"nofx/trader"
)

// TraderIntegration 跟单与交易执行的集成
type TraderIntegration struct {
	traderID   string
	autoTrader *trader.AutoTrader
	engine     *Engine
	store      *store.Store
	ctx        context.Context
	cancel     context.CancelFunc
	running    bool
}

// NewTraderIntegration 创建交易集成
func NewTraderIntegration(
	traderID string,
	autoTrader *trader.AutoTrader,
	st *store.Store,
) *TraderIntegration {
	ctx, cancel := context.WithCancel(context.Background())
	return &TraderIntegration{
		traderID:   traderID,
		autoTrader: autoTrader,
		store:      st,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// StartCopyTrading 启动跟单
func (ti *TraderIntegration) StartCopyTrading() error {
	if ti.running {
		return fmt.Errorf("copy trading already running for trader %s", ti.traderID)
	}

	// 从数据库加载跟单配置
	copyConfig, err := ti.store.CopyTrade().GetByTraderID(ti.traderID)
	if err != nil {
		return fmt.Errorf("failed to get copy trade config: %w", err)
	}

	if !copyConfig.Enabled {
		return fmt.Errorf("copy trade is not enabled for trader %s", ti.traderID)
	}

	// 转换为引擎配置
	engineConfig := &CopyConfig{
		ProviderType:   ProviderType(copyConfig.ProviderType),
		LeaderID:       copyConfig.LeaderID,
		CopyRatio:      copyConfig.CopyRatio,
		SyncLeverage:   copyConfig.SyncLeverage,
		SyncMarginMode: copyConfig.SyncMarginMode,
		MinTradeWarn:   copyConfig.MinTradeWarn,
		MaxTradeWarn:   copyConfig.MaxTradeWarn,
	}

	// 创建引擎
	engine, err := NewEngine(
		ti.traderID,
		engineConfig,
		ti.getBalanceFunc(),
		ti.getPositionsFunc(),
	)
	if err != nil {
		return fmt.Errorf("failed to create copy trade engine: %w", err)
	}

	ti.engine = engine

	// 启动引擎
	if err := engine.Start(ti.ctx); err != nil {
		return fmt.Errorf("failed to start copy trade engine: %w", err)
	}

	// 启动决策消费协程
	go ti.consumeDecisions()

	ti.running = true
	logger.Infof("🚀 [%s] 跟单集成已启动 | provider=%s leader=%s",
		ti.traderID, copyConfig.ProviderType, copyConfig.LeaderID)

	return nil
}

// Stop 停止跟单
func (ti *TraderIntegration) Stop() {
	if !ti.running {
		return
	}

	ti.cancel()

	if ti.engine != nil {
		ti.engine.Stop()
	}

	ti.running = false
	logger.Infof("🛑 [%s] 跟单集成已停止", ti.traderID)
}

// IsRunning 检查是否运行中
func (ti *TraderIntegration) IsRunning() bool {
	return ti.running
}

// GetStats 获取统计信息
func (ti *TraderIntegration) GetStats() *EngineStats {
	if ti.engine == nil {
		return nil
	}
	return ti.engine.GetStats()
}

// consumeDecisions 消费跟单引擎产生的决策
func (ti *TraderIntegration) consumeDecisions() {
	decisionCh := ti.engine.GetDecisionChannel()

	for {
		select {
		case <-ti.ctx.Done():
			return
		case fullDec, ok := <-decisionCh:
			if !ok {
				return
			}
			ti.executeFullDecision(fullDec)
		}
	}
}

// executeFullDecision 执行完整决策
func (ti *TraderIntegration) executeFullDecision(fullDec *decision.FullDecision) {
	for _, dec := range fullDec.Decisions {
		// 记录决策日志
		ti.logDecision(fullDec, &dec)

		// 执行交易
		startTime := time.Now()
		err := ti.autoTrader.ExecuteDecision(&dec)

		if err != nil {
			logger.Errorf("❌ [%s] 跟单执行失败 | %s %s | error=%v",
				ti.traderID, dec.Action, dec.Symbol, err)

			// 保存错误日志
			ti.saveSignalLog(&dec, "failed", err.Error())
		} else {
			logger.Infof("✅ [%s] 跟单执行成功 | %s %s | 耗时=%dms",
				ti.traderID, dec.Action, dec.Symbol, time.Since(startTime).Milliseconds())

			// 保存成功日志
			ti.saveSignalLog(&dec, "executed", "")
		}
	}
}

// logDecision 记录决策日志（兼容现有 AI 决策日志格式）
func (ti *TraderIntegration) logDecision(fullDec *decision.FullDecision, dec *decision.Decision) {
	// 使用现有的决策日志格式，复用 decision_logs/<trader_id>/ 目录
	// 这样可以在前端无缝显示跟单日志
	logger.Infof("📝 [%s] 跟单决策 | %s %s | reasoning=%s",
		ti.traderID, dec.Action, dec.Symbol, dec.Reasoning)
}

// saveSignalLog 保存信号日志到数据库
func (ti *TraderIntegration) saveSignalLog(dec *decision.Decision, status, errorMsg string) {
	log := &store.CopyTradeSignalLog{
		TraderID:     ti.traderID,
		LeaderID:     ti.engine.config.LeaderID,
		ProviderType: string(ti.engine.config.ProviderType),
		SignalID:     fmt.Sprintf("%s_%d", dec.Symbol, time.Now().UnixNano()),
		Symbol:       dec.Symbol,
		Action:       dec.Action,
		PositionSide: "", // 从 action 推断
		CopySize:     dec.PositionSizeUSD,
		Followed:     status == "executed",
		FollowReason: dec.Reasoning,
		Status:       status,
		ErrorMessage: errorMsg,
	}

	if err := ti.store.CopyTrade().SaveSignalLog(log); err != nil {
		logger.Warnf("⚠️ [%s] 保存信号日志失败: %v", ti.traderID, err)
	}
}

// ============================================================================
// 回调函数（获取跟随者账户信息）
// ============================================================================

// getBalanceFunc 返回获取余额的函数
func (ti *TraderIntegration) getBalanceFunc() func() float64 {
	return func() float64 {
		info, err := ti.autoTrader.GetAccountInfo()
		if err != nil {
			logger.Warnf("⚠️ [%s] 获取账户余额失败: %v", ti.traderID, err)
			return 0
		}

		// 从账户信息中提取余额
		if equity, ok := info["total_equity"].(float64); ok {
			return equity
		}
		return 0
	}
}

// getPositionsFunc 返回获取持仓的函数
func (ti *TraderIntegration) getPositionsFunc() func() map[string]*Position {
	return func() map[string]*Position {
		positions := make(map[string]*Position)

		// 获取交易所持仓 (返回 []map[string]interface{})
		exchangePositions, err := ti.autoTrader.GetPositions()
		if err != nil {
			logger.Warnf("⚠️ [%s] 获取持仓失败: %v", ti.traderID, err)
			return positions
		}

		// 转换为跟单模块的持仓格式
		for _, pos := range exchangePositions {
			symbol, _ := pos["symbol"].(string)
			sideStr, _ := pos["side"].(string)
			quantity, _ := pos["quantity"].(float64)
			entryPrice, _ := pos["entry_price"].(float64)
			markPrice, _ := pos["mark_price"].(float64)
			leverage, _ := pos["leverage"].(int)
			unrealizedPnl, _ := pos["unrealized_pnl"].(float64)

			if quantity == 0 {
				continue
			}

			side := SideLong
			if sideStr == "short" || sideStr == "sell" {
				side = SideShort
			}

			key := PositionKey(symbol, side)
			positions[key] = &Position{
				Symbol:        symbol,
				Side:          side,
				Size:          abs(quantity),
				EntryPrice:    entryPrice,
				MarkPrice:     markPrice,
				Leverage:      leverage,
				UnrealizedPnL: unrealizedPnl,
				PositionValue: abs(quantity) * markPrice,
			}
		}

		return positions
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// ============================================================================
// 全局集成管理
// ============================================================================

var (
	integrations   = make(map[string]*TraderIntegration)
	integrationsMu = &struct{}{}
)

// StartCopyTradingForTrader 为指定 trader 启动跟单
// 这是外部调用的主入口
func StartCopyTradingForTrader(
	traderID string,
	autoTrader *trader.AutoTrader,
	st *store.Store,
) error {
	integration := NewTraderIntegration(traderID, autoTrader, st)
	integrations[traderID] = integration
	return integration.StartCopyTrading()
}

// StopCopyTradingForTrader 停止指定 trader 的跟单
func StopCopyTradingForTrader(traderID string) error {
	integration, exists := integrations[traderID]
	if !exists {
		return fmt.Errorf("no copy trading integration found for trader %s", traderID)
	}

	integration.Stop()
	delete(integrations, traderID)
	return nil
}

// GetCopyTradingStats 获取跟单统计
func GetCopyTradingStats(traderID string) *EngineStats {
	integration, exists := integrations[traderID]
	if !exists {
		return nil
	}
	return integration.GetStats()
}

// IsCopyTradingRunning 检查跟单是否运行中
func IsCopyTradingRunning(traderID string) bool {
	integration, exists := integrations[traderID]
	if !exists {
		return false
	}
	return integration.IsRunning()
}

// StopAllCopyTrading 停止所有跟单
func StopAllCopyTrading() {
	for traderID, integration := range integrations {
		integration.Stop()
		logger.Infof("🛑 停止跟单: %s", traderID)
	}
	integrations = make(map[string]*TraderIntegration)
}

