package copytrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nofx/decision"
	"nofx/logger"
	"nofx/notifier"
	"nofx/store"
)

// DecisionExecutor 决策执行器接口
// 用于避免循环导入，由 trader.AutoTrader 实现
type DecisionExecutor interface {
	ExecuteDecision(dec *decision.Decision) error
	GetAccountInfo() (map[string]interface{}, error)
	GetPositions() ([]map[string]interface{}, error)
}

// TraderIntegration 跟单与交易执行的集成
type TraderIntegration struct {
	traderID    string
	executor    DecisionExecutor
	engine      *Engine
	store       *store.Store
	ctx         context.Context
	cancel      context.CancelFunc
	running     bool
	cycleNumber int // 跟单周期计数器
}

// NewTraderIntegration 创建交易集成
func NewTraderIntegration(
	traderID string,
	executor DecisionExecutor,
	st *store.Store,
) *TraderIntegration {
	ctx, cancel := context.WithCancel(context.Background())
	return &TraderIntegration{
		traderID: traderID,
		executor: executor,
		store:    st,
		ctx:      ctx,
		cancel:   cancel,
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
		ProviderType:     ProviderType(copyConfig.ProviderType),
		LeaderID:         copyConfig.LeaderID,
		CopyRatio:        copyConfig.CopyRatio,
		SyncLeverage:     copyConfig.SyncLeverage,
		SyncMarginMode:   copyConfig.SyncMarginMode,
		MinTradeWarn:     copyConfig.MinTradeWarn,
		MaxTradeWarn:     copyConfig.MaxTradeWarn,
		BinanceP20T:      copyConfig.BinanceP20T,
		BinanceCSRFToken: copyConfig.BinanceCSRFToken,
	}

	// 创建引擎（Hyperliquid 使用流式模式，OKX 使用轮询模式）
	var engineOpts []EngineOption
	if engineConfig.ProviderType == ProviderHyperliquid {
		engineOpts = append(engineOpts, WithStreamingMode())
	}
	// Binance 启用全局凭证热加载（store.BinanceCreds 实现了 BinanceCredentialsLoader 接口）
	// OKX/HL 路径下 NewProviderWithLoader 不会消费 loader，零影响。
	if engineConfig.ProviderType == ProviderBinance && ti.store != nil {
		engineOpts = append(engineOpts, WithBinanceCredentialsLoader(ti.store.BinanceCreds()))
	}

	engine, err := NewEngine(
		ti.traderID,
		engineConfig,
		ti.getBalanceFunc(),
		ti.getPositionsFunc(),
		engineOpts...,
	)
	if err != nil {
		return fmt.Errorf("failed to create copy trade engine: %w", err)
	}

	// 设置数据库存储（用于仓位映射）
	engine.SetStore(ti.store)

	// 🔑 初始化历史仓位：将领航员当前持仓标记为 ignored
	// 这样后续这些仓位的操作都不会跟随，只跟新开仓
	if err := engine.InitIgnoredPositions(); err != nil {
		logger.Warnf("⚠️ [%s] 初始化历史仓位失败: %v（继续启动）", ti.traderID, err)
	}

	ti.engine = engine

	// 启动引擎
	if err := engine.Start(ti.ctx); err != nil {
		// 异步发送邮件告警（未启用通知器时为 no-op）
		notifier.Notify(notifier.Alert{
			Category: "copy_trade",
			TraderID: ti.traderID,
			Title:    "跟单引擎启动失败",
			Body: fmt.Sprintf(
				"跟单引擎启动失败 (Copy Trade Engine Start Failed)\n\n"+
					"Trader ID: %s\n"+
					"Provider:  %s\n"+
					"Leader ID: %s\n\n"+
					"错误信息 (Error):\n%s",
				ti.traderID,
				copyConfig.ProviderType,
				copyConfig.LeaderID,
				err.Error(),
			),
			Fields: map[string]string{
				"Provider": copyConfig.ProviderType,
				"Leader":   copyConfig.LeaderID,
				"Reason":   err.Error(),
			},
		})
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
	ti.cycleNumber++

	// 构建决策记录
	decisionActions := make([]store.DecisionAction, 0, len(fullDec.Decisions))
	executionLogs := make([]string, 0)

	for i := range fullDec.Decisions {
		dec := &fullDec.Decisions[i]

		// 记录决策日志
		ti.logDecision(fullDec, dec)

		// 执行交易
		startTime := time.Now()
		err := ti.executor.ExecuteDecision(dec)

		// 构建决策动作记录
		action := store.DecisionAction{
			Action:    dec.Action,
			Symbol:    dec.Symbol,
			Leverage:  dec.Leverage,
			Price:     dec.EntryPrice, // 使用领航员成交价格作为入场价
			Reasoning: dec.Reasoning,
			Timestamp: time.Now(),
		}

		if err != nil {
			traderName := ti.traderDisplayName()
			logger.Errorf("❌ [%s/%s] 跟单执行失败 | %s %s | error=%v",
				traderName, ti.traderID, dec.Action, dec.Symbol, err)
			executionLogs = append(executionLogs, fmt.Sprintf("❌ %s %s 失败: %v", dec.Action, dec.Symbol, err))
			ti.saveSignalLog(dec, "failed", err.Error())
			alertKey := ti.execFailureDedupKey(dec, err)

			// 异步发送邮件告警（未启用通知器时为 no-op，零阻塞、零副作用）
			notifier.Notify(notifier.Alert{
				Category: "copy_trade",
				TraderID: ti.traderID,
				Title:    fmt.Sprintf("%s | %s %s 失败", traderName, dec.Action, dec.Symbol),
				Body:     ti.buildExecFailureAlertBody(dec, err, traderName),
				RateKey:  alertKey,
				DedupKey: alertKey,
				Fields: map[string]string{
					"TraderName":  traderName,
					"Provider":    string(ti.engine.config.ProviderType),
					"Leader":      ti.engine.config.LeaderID,
					"Action":      dec.Action,
					"Symbol":      dec.Symbol,
					"EntryPrice":  fmt.Sprintf("%.4f", dec.EntryPrice),
					"PositionUSD": fmt.Sprintf("%.2f", dec.PositionSizeUSD),
					"Leverage":    fmt.Sprintf("%dx", dec.Leverage),
					"MarginMode":  dec.MarginMode,
					"LeaderPosID": dec.LeaderPosID,
					"Reason":      err.Error(),
				},
			})
		} else {
			duration := time.Since(startTime).Milliseconds()
			logger.Infof("✅ [%s] 跟单执行成功 | %s %s | 耗时=%dms",
				ti.traderID, dec.Action, dec.Symbol, duration)
			executionLogs = append(executionLogs, fmt.Sprintf("✅ %s %s 成功 (耗时 %dms)", dec.Action, dec.Symbol, duration))
			ti.saveSignalLog(dec, "executed", "")

			// 执行成功后更新仓位映射
			ti.updatePositionMapping(dec)
		}

		decisionActions = append(decisionActions, action)
	}

	// 保存到 decision_records 表，复用现有日志系统
	ti.saveDecisionRecord(fullDec, decisionActions, executionLogs)
}

func (ti *TraderIntegration) execFailureDedupKey(dec *decision.Decision, err error) string {
	providerType := ""
	leaderID := ""
	if ti.engine != nil && ti.engine.config != nil {
		providerType = string(ti.engine.config.ProviderType)
		leaderID = ti.engine.config.LeaderID
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	return fmt.Sprintf("copy_trade_exec_failed|%s|%s|%s|%s|%s|%s|%.8f|%.8f|%.2f|%s|%s",
		ti.traderID,
		providerType,
		leaderID,
		dec.Action,
		dec.Symbol,
		dec.LeaderPosID,
		dec.LeaderPosSize,
		dec.CloseRatio,
		dec.PositionSizeUSD,
		dec.MarginMode,
		errText,
	)
}

// saveDecisionRecord 保存跟单决策到 decision_records 表
func (ti *TraderIntegration) saveDecisionRecord(fullDec *decision.FullDecision, actions []store.DecisionAction, executionLogs []string) {
	// 构建跟单的思维链（类似 AI 的 CoT）
	cotTrace := ti.buildCopyTradeCoT(fullDec)

	// 获取当前账户状态
	accountState := store.AccountSnapshot{}
	var totalEquity, availableBalance, unrealizedPnL float64
	if info, err := ti.executor.GetAccountInfo(); err == nil {
		if equity, ok := info["total_equity"].(float64); ok {
			accountState.TotalBalance = equity
			totalEquity = equity
		}
		if available, ok := info["available_balance"].(float64); ok {
			accountState.AvailableBalance = available
			availableBalance = available
		}
		// 尝试两种字段名，兼容不同返回格式
		if pnl, ok := info["unrealized_profit"].(float64); ok {
			unrealizedPnL = pnl
		} else if pnl, ok := info["unrealized_pnl"].(float64); ok {
			unrealizedPnL = pnl
		}
	}

	// 获取当前持仓
	positions := make([]store.PositionSnapshot, 0)
	if posData, err := ti.executor.GetPositions(); err == nil {
		for _, p := range posData {
			pos := store.PositionSnapshot{}
			if s, ok := p["symbol"].(string); ok {
				pos.Symbol = s
			}
			if s, ok := p["side"].(string); ok {
				pos.Side = s
			}
			if v, ok := p["quantity"].(float64); ok {
				pos.PositionAmt = v
			}
			if v, ok := p["entryPrice"].(float64); ok {
				pos.EntryPrice = v
			}
			if v, ok := p["markPrice"].(float64); ok {
				pos.MarkPrice = v
			}
			if v, ok := p["unrealizedPnl"].(float64); ok {
				pos.UnrealizedProfit = v
			}
			positions = append(positions, pos)
		}
	}

	record := &store.DecisionRecord{
		TraderID:            ti.traderID,
		CycleNumber:         ti.cycleNumber,
		Timestamp:           time.Now(),
		SystemPrompt:        "Copy Trading Mode",
		InputPrompt:         fmt.Sprintf("跟单领航员: %s (%s)", ti.engine.config.LeaderID, ti.engine.config.ProviderType),
		CoTTrace:            cotTrace,
		DecisionJSON:        fmt.Sprintf(`{"mode":"copy_trade","leader":"%s"}`, ti.engine.config.LeaderID),
		CandidateCoins:      []string{},
		ExecutionLog:        executionLogs,
		Success:             true,
		Decisions:           actions,
		AccountState:        accountState,
		Positions:           positions,
		AIRequestDurationMs: 0, // 跟单没有 AI 请求
	}

	if err := ti.store.Decision().LogDecision(record); err != nil {
		logger.Warnf("⚠️ [%s] 保存跟单决策记录失败: %v", ti.traderID, err)
	} else {
		logger.Infof("📝 [%s] 跟单决策记录已保存: cycle=%d", ti.traderID, ti.cycleNumber)
	}

	// 保存权益快照（用于前端绘制净值曲线）
	ti.saveEquitySnapshot(totalEquity, availableBalance, unrealizedPnL, len(positions))
}

// saveEquitySnapshot 保存权益快照（复用 store.Equity() 接口）
func (ti *TraderIntegration) saveEquitySnapshot(totalEquity, availableBalance, unrealizedPnL float64, positionCount int) {
	if ti.store == nil || totalEquity <= 0 {
		return
	}

	// 计算保证金使用率
	marginUsedPct := 0.0
	if totalEquity > 0 {
		marginUsedPct = ((totalEquity - availableBalance) / totalEquity) * 100
	}

	snapshot := &store.EquitySnapshot{
		TraderID:      ti.traderID,
		Timestamp:     time.Now().UTC(),
		TotalEquity:   totalEquity,
		Balance:       totalEquity - unrealizedPnL, // 钱包余额 = 总权益 - 未实现盈亏
		UnrealizedPnL: unrealizedPnL,
		PositionCount: positionCount,
		MarginUsedPct: marginUsedPct,
	}

	if err := ti.store.Equity().Save(snapshot); err != nil {
		logger.Warnf("⚠️ [%s] 保存权益快照失败: %v", ti.traderID, err)
	} else {
		logger.Debugf("💾 [%s] 权益快照已保存: equity=%.2f", ti.traderID, totalEquity)
	}
}

// buildCopyTradeCoT 构建跟单的思维链描述
func (ti *TraderIntegration) buildCopyTradeCoT(fullDec *decision.FullDecision) string {
	var cot string
	cot += "## 📋 跟单决策分析\n\n"
	cot += fmt.Sprintf("**领航员**: %s\n", ti.engine.config.LeaderID)
	cot += fmt.Sprintf("**数据源**: %s\n", ti.engine.config.ProviderType)
	cot += fmt.Sprintf("**跟单比例**: %.0f%%\n\n", ti.engine.config.CopyRatio*100)

	for _, dec := range fullDec.Decisions {
		cot += fmt.Sprintf("### %s %s\n", dec.Action, dec.Symbol)
		cot += fmt.Sprintf("- **操作**: %s\n", dec.Action)
		cot += fmt.Sprintf("- **币种**: %s\n", dec.Symbol)
		if dec.PositionSizeUSD > 0 {
			cot += fmt.Sprintf("- **金额**: $%.2f\n", dec.PositionSizeUSD)
		}
		if dec.Leverage > 0 {
			cot += fmt.Sprintf("- **杠杆**: %dx\n", dec.Leverage)
		}
		cot += fmt.Sprintf("- **原因**: %s\n\n", dec.Reasoning)
	}

	return cot
}

// logDecision 记录决策日志（兼容现有 AI 决策日志格式）
func (ti *TraderIntegration) logDecision(fullDec *decision.FullDecision, dec *decision.Decision) {
	// 使用现有的决策日志格式，复用 decision_logs/<trader_id>/ 目录
	// 这样可以在前端无缝显示跟单日志
	logger.Infof("📝 [%s] 跟单决策 | %s %s | reasoning=%s",
		ti.traderID, dec.Action, dec.Symbol, dec.Reasoning)
}

// buildExecFailureAlertBody 构造跟单执行失败的告警正文
func (ti *TraderIntegration) buildExecFailureAlertBody(dec *decision.Decision, err error, traderName string) string {
	providerType := ""
	leaderID := ""
	if ti.engine != nil && ti.engine.config != nil {
		providerType = string(ti.engine.config.ProviderType)
		leaderID = ti.engine.config.LeaderID
	}
	return fmt.Sprintf(
		"跟单执行失败 (Copy Trade Execution Failed)\n\n"+
			"Trader Name: %s\n"+
			"Trader ID:   %s\n"+
			"Provider:    %s\n"+
			"Leader ID:   %s\n"+
			"Action:      %s\n"+
			"Symbol:      %s\n"+
			"EntryPrice:  %.4f\n"+
			"PositionUSD: %.2f\n"+
			"Leverage:    %dx\n"+
			"MarginMode:  %s\n"+
			"LeaderPosID: %s\n\n"+
			"错误信息 (Error):\n%s",
		traderName,
		ti.traderID,
		providerType,
		leaderID,
		dec.Action,
		dec.Symbol,
		dec.EntryPrice,
		dec.PositionSizeUSD,
		dec.Leverage,
		dec.MarginMode,
		dec.LeaderPosID,
		err.Error(),
	)
}

func (ti *TraderIntegration) traderDisplayName() string {
	if ti == nil {
		return ""
	}
	if ti.store == nil {
		return ti.traderID
	}
	trader, err := ti.store.Trader().GetByID(ti.traderID)
	if err != nil || trader == nil {
		return ti.traderID
	}
	name := strings.TrimSpace(trader.Name)
	if name == "" {
		return ti.traderID
	}
	return name
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

// updatePositionMapping 更新仓位映射（执行成功后调用）
// 根据 action 类型执行不同操作：
//   - open_long/open_short: 保存新映射 或 加仓（根据数据库是否已有映射判断）
//   - close_long/close_short: 关闭映射 或 减仓（根据是否还有持仓判断）
func (ti *TraderIntegration) updatePositionMapping(dec *decision.Decision) {
	// 无 posId 时跳过（Hyperliquid 或其他场景）
	if dec.LeaderPosID == "" {
		return
	}

	copyTradeStore := ti.store.CopyTrade()

	// 从 action 推断操作类型
	switch dec.Action {
	case "open_long", "open_short":
		// 推断本次决策对应的方向
		expectedSide := "long"
		if dec.Action == "open_short" {
			expectedSide = "short"
		}

		// 判断是新开仓还是加仓：查数据库映射
		existingMapping, err := copyTradeStore.GetActiveMapping(ti.traderID, dec.LeaderPosID)
		if err != nil {
			logger.Warnf("⚠️ [%s] 查询映射失败: %v", ti.traderID, err)
		}

		// 🔑 F2: 反手方向检测自愈
		// 触发条件：领航员在 OKX NET 模式下复用同一 posId 但翻转方向
		// （例如领航员先平 LONG 再开 SHORT，OKX 复用槽位 posId）
		// 处理：将旧映射关闭，让下面的"无映射"分支走新开仓写入正确方向
		if existingMapping != nil && existingMapping.Side != expectedSide {
			logger.Warnf("⚠️ [%s] posId 方向变更检测 | posId=%s 旧 side=%s 新 side=%s → 关闭旧映射并重建",
				ti.traderID, dec.LeaderPosID, existingMapping.Side, expectedSide)
			if err := copyTradeStore.CloseMapping(ti.traderID, dec.LeaderPosID, dec.EntryPrice); err != nil {
				logger.Warnf("⚠️ [%s] 关闭旧方向映射失败: %v（继续写入新映射）", ti.traderID, err)
			}
			existingMapping = nil // 走下方"新开仓"分支重建
		}

		if existingMapping != nil {
			// 映射已存在 → 加仓：增加加仓次数
			if err := copyTradeStore.IncrementAddCount(ti.traderID, dec.LeaderPosID); err != nil {
				logger.Warnf("⚠️ [%s] 更新加仓次数失败: %v", ti.traderID, err)
			} else {
				logger.Infof("📝 [%s] 加仓次数已更新 | posId=%s %s (第 %d 次加仓)",
					ti.traderID, dec.LeaderPosID, dec.Symbol, existingMapping.AddCount+1)
			}
			// 更新 lastKnownSize（领航员当前持仓数量）
			if dec.LeaderPosSize > 0 {
				if err := copyTradeStore.UpdateLastKnownSize(ti.traderID, dec.LeaderPosID, dec.LeaderPosSize); err != nil {
					logger.Warnf("⚠️ [%s] 更新 lastKnownSize 失败: %v", ti.traderID, err)
				}
			}
		} else {
			// 无映射 → 新开仓：保存映射
			mapping := &store.CopyTradePositionMapping{
				TraderID:      ti.traderID,
				LeaderPosID:   dec.LeaderPosID,
				LeaderID:      ti.engine.config.LeaderID,
				Symbol:        dec.Symbol,
				Side:          expectedSide,
				MarginMode:    dec.MarginMode,
				OpenedAt:      time.Now(),
				OpenPrice:     dec.EntryPrice,
				OpenSizeUSD:   dec.PositionSizeUSD,
				LastKnownSize: dec.LeaderPosSize, // 记录领航员当前持仓数量
			}

			if err := copyTradeStore.SavePositionMapping(mapping); err != nil {
				logger.Warnf("⚠️ [%s] 保存仓位映射失败: %v", ti.traderID, err)
			} else {
				logger.Infof("📝 [%s] 仓位映射已保存 | posId=%s %s %s %s lastKnownSize=%.4f",
					ti.traderID, dec.LeaderPosID, dec.Symbol, expectedSide, dec.MarginMode, dec.LeaderPosSize)
			}
		}

	case "reduce_long", "reduce_short":
		// 减仓：增加减仓次数
		if err := copyTradeStore.IncrementReduceCount(ti.traderID, dec.LeaderPosID); err != nil {
			logger.Warnf("⚠️ [%s] 更新减仓次数失败: %v", ti.traderID, err)
		}
		// 更新 lastKnownSize（领航员当前持仓数量）
		if dec.LeaderPosSize > 0 {
			if err := copyTradeStore.UpdateLastKnownSize(ti.traderID, dec.LeaderPosID, dec.LeaderPosSize); err != nil {
				logger.Warnf("⚠️ [%s] 更新 lastKnownSize 失败: %v", ti.traderID, err)
			}
		}

	case "close_long", "close_short":
		// 平仓：关闭映射
		if err := copyTradeStore.CloseMapping(ti.traderID, dec.LeaderPosID, dec.EntryPrice); err != nil {
			logger.Warnf("⚠️ [%s] 关闭仓位映射失败: %v", ti.traderID, err)
		} else {
			logger.Infof("📝 [%s] 仓位映射已关闭 | posId=%s %s",
				ti.traderID, dec.LeaderPosID, dec.Symbol)
		}
	}
}

// ============================================================================
// 回调函数（获取跟随者账户信息）
// ============================================================================

// getBalanceFunc 返回获取余额的函数
// 🔑 使用可用余额(available_balance)而非总权益(total_equity)
// 原因：总权益包含已用保证金，但跟单开仓只能用可用余额
// 效果：避免计算出的跟单金额超过实际可用资金导致下单失败
func (ti *TraderIntegration) getBalanceFunc() func() float64 {
	return func() float64 {
		info, err := ti.executor.GetAccountInfo()
		if err != nil {
			logger.Warnf("⚠️ [%s] 获取账户余额失败: %v", ti.traderID, err)
			return 0
		}

		// 🔑 优先使用可用余额（实际可开仓的资金）
		// 这样跟单金额会基于实际可用资金计算，避免因余额不足导致下单失败
		if avail, ok := info["available_balance"].(float64); ok && avail > 0 {
			return avail
		}

		// 回退：如果没有 available_balance 或为 0，使用 total_equity
		// (兼容老接口或特殊情况，如减仓/平仓时可用余额为0但仍需计算)
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
		exchangePositions, err := ti.executor.GetPositions()
		if err != nil {
			logger.Warnf("⚠️ [%s] 获取持仓失败: %v", ti.traderID, err)
			return positions
		}

		// 转换为跟单模块的持仓格式
		// 兼容不同 trader 的字段名格式
		for _, pos := range exchangePositions {
			symbol, _ := pos["symbol"].(string)
			sideStr, _ := pos["side"].(string)

			// 数量字段: 优先 positionAmt (OKX), 回退 quantity (Binance)
			quantity := getFloatField(pos, "positionAmt", "quantity")

			// 入场价: 优先 entryPrice (OKX), 回退 entry_price (Binance)
			entryPrice := getFloatField(pos, "entryPrice", "entry_price")

			// 标记价: 优先 markPrice (OKX), 回退 mark_price (Binance)
			markPrice := getFloatField(pos, "markPrice", "mark_price")

			// 杠杆: float64 或 int
			leverage := getIntOrFloatField(pos, "leverage")

			// 未实现盈亏: 优先 unRealizedProfit (OKX), 回退 unrealized_pnl (Binance)
			unrealizedPnl := getFloatField(pos, "unRealizedProfit", "unrealized_pnl")

			// 保证金模式: OKX 特有，用于区分全仓/逐仓
			marginMode := getStringField(pos, "marginMode", "mgnMode")

			// 仓位唯一标识: OKX 特有，用于精确匹配仓位
			posId := getStringField(pos, "posId")

			if quantity == 0 {
				continue
			}

			side := SideLong
			if sideStr == "short" || sideStr == "sell" {
				side = SideShort
			}

			// 关键改进：使用 posId 作为 key（如果有），否则回退到 mgnMode key
			// posId 是 OKX 为每个仓位生成的唯一标识，100% 准确
			var key string
			if posId != "" {
				key = posId // 使用 posId 作为 key（推荐）
			} else {
				key = PositionKeyWithMode(symbol, side, marginMode) // 回退兼容
			}

			// 调试日志：显示每个持仓的详细信息
			logger.Debugf("📊 [%s] 持仓解析: %s | side=%s → %s | mgnMode=%s | posId=%s | 数量=%.4f 杠杆=%d",
				ti.traderID, symbol, sideStr, side, marginMode, posId, quantity, leverage)

			positions[key] = &Position{
				Symbol:        symbol,
				Side:          side,
				Size:          absFloat(quantity),
				EntryPrice:    entryPrice,
				MarkPrice:     markPrice,
				Leverage:      leverage,
				MarginMode:    marginMode,
				UnrealizedPnL: unrealizedPnl,
				PositionValue: absFloat(quantity) * markPrice,
				PosID:         posId,
			}
		}

		return positions
	}
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// getFloatField 从 map 中获取 float64 字段，支持多个字段名回退
func getFloatField(m map[string]interface{}, keys ...string) float64 {
	for _, key := range keys {
		if val, ok := m[key]; ok {
			switch v := val.(type) {
			case float64:
				return v
			case float32:
				return float64(v)
			case int:
				return float64(v)
			case int64:
				return float64(v)
			}
		}
	}
	return 0
}

// getStringField 从 map 中获取 string 字段，支持多个字段名回退
func getStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if val, ok := m[key]; ok {
			if s, ok := val.(string); ok {
				return s
			}
		}
	}
	return ""
}

// getIntOrFloatField 从 map 中获取 int 字段，支持 float64 类型转换
func getIntOrFloatField(m map[string]interface{}, key string) int {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		case float32:
			return int(v)
		}
	}
	return 0
}

// ============================================================================
// 全局集成管理
// ============================================================================

var (
	// integrations 存储所有跟单集成实例（注：目前只在启动时使用，无并发问题）
	integrations = make(map[string]*TraderIntegration)
)

// StartCopyTradingForTrader 为指定 trader 启动跟单
// 这是外部调用的主入口
func StartCopyTradingForTrader(
	traderID string,
	executor DecisionExecutor,
	st *store.Store,
) error {
	integration := NewTraderIntegration(traderID, executor, st)
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
