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

// StopLossManager 跟单专用的 SL 管理扩展接口（可选实现）
//
// 设计目的：为账户保护止损（v3 风控）提供撤旧/挂新/拿价的能力，
// 同时不污染核心 DecisionExecutor 接口。AutoTrader 自动实现这三个方法。
//
// 调用方通过 type assertion 检测；不实现就降级到「仅依赖 buildDecisionV2 估算的 dec.StopLoss」。
type StopLossManager interface {
	// SetStopLoss 挂一个 algo 条件止损单（多/空均支持）
	SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error
	// CancelStopLossOrders 撤销该 symbol 的所有 algo 止损单（不影响止盈单）
	CancelStopLossOrders(symbol string) error
	// GetMarketPrice 获取该 symbol 的实时市价（用于 SL 价格校验等）
	GetMarketPrice(symbol string) (float64, error)
}

// mappingFailureCircuitThreshold 跟单 active mapping 的连续失败熔断阈值。
//
// 触发条件：同一 leaderPosID 的开/加/平/减仓决策连续失败 N 次（成功时清零）。
// 触发动作：自动 CloseMapping（断开重试链）+ 发送一次熔断告警邮件。
//
// 设计目的：作为防御性兜底，避免 PR-1 的"良性 close 错误关键字"未覆盖到的新错误
// 类型导致死循环；阈值取 5 既能容忍偶发网络/API 抖动，也能在 15s（5×3s）内收敛。
const mappingFailureCircuitThreshold = 5

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

		// v3 风控字段透传（账户保护止损）
		RiskStopLossEnabled:  copyConfig.RiskStopLossEnabled,
		RiskAccountPct:       copyConfig.RiskAccountPct,
		RiskATREnabled:       copyConfig.RiskATREnabled,
		RiskATRMultiplier:    copyConfig.RiskATRMultiplier,
		RiskATRTimeframe:     copyConfig.RiskATRTimeframe,
		RiskLeverageFallback: copyConfig.RiskLeverageFallback,
		RiskLeverageMaxLoss:  copyConfig.RiskLeverageMaxLoss,
		RiskReentryEnabled:   copyConfig.RiskReentryEnabled,
		RiskReentryRatio:     copyConfig.RiskReentryRatio,
		RiskReentryTolerance: copyConfig.RiskReentryTolerance,
	}
	engineConfig.FillRiskDefaults() // 兜底默认值（旧库迁移 / 前端未传时）

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
	// 启动风控事件消费协程（v3 风控：SL 触发 / 二次进场告警邮件）
	go ti.consumeRiskEvents()

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

		// 🛑 v3 风控：跟单 OKX 路径，执行前先撤旧 SL（避免加仓/减仓后挂出多个 SL）
		// 不区分动作类型：开仓/加仓/减仓/平仓前都撤一次最简单可靠
		// 节流：本函数本身是 channel 串行消费，同 trader 不会并发触发，无需额外节流
		ti.cancelExistingStopLoss(dec, "before-execute")

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
			// 🔑 良性失败识别：close/reduce 类决策遇到"本地无对应仓位"错误时，
			// 说明跟随者本地仓位已经通过其他途径消失（手动平、强平、历史 mapping 残留等）。
			// 这种情况下：
			//   1. 主动关闭 mapping，避免引擎下个轮询又生成同样的 close 信号 → 死循环
			//   2. 不发邮件告警（不是真正的错误，是数据自愈）
			//   3. 状态记 silent_close 区分于真正的 failed
			// 开仓/加仓 (open_*/reduce_*) 永远不会进入这里。
			if ti.isBenignCloseError(dec, err) {
				ti.handleBenignCloseFailure(dec, err)
				executionLogs = append(executionLogs,
					fmt.Sprintf("🟡 %s %s silent_close (本地仓位已不存在，自动回收映射): %v",
						dec.Action, dec.Symbol, err))
			} else {
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

				// 🔧 连续失败熔断（防御兜底）：
				// 同一 leaderPosID 连续失败 ≥ 阈值 → 主动 CloseMapping 并发熔断告警，
				// 避免良性错误关键字未覆盖的新错误形态导致死循环。
				ti.checkAndTripMappingCircuit(dec, err)
			}
		} else {
			duration := time.Since(startTime).Milliseconds()
			logger.Infof("✅ [%s] 跟单执行成功 | %s %s | 耗时=%dms",
				ti.traderID, dec.Action, dec.Symbol, duration)
			executionLogs = append(executionLogs, fmt.Sprintf("✅ %s %s 成功 (耗时 %dms)", dec.Action, dec.Symbol, duration))
			ti.saveSignalLog(dec, "executed", "")

			// 执行成功后更新仓位映射
			ti.updatePositionMapping(dec)

			// 🛑 v3 风控：开仓/加仓/部分减仓 → 用实际成交均价精确重挂 SL
			// 平仓不重挂（仓位已经全清空）
			// 设计：放在 mapping 更新后，保证后续 SL 触发对账能找到正确的 active mapping
			ti.refreshStopLossAfterExecute(dec)

			// 🔧 成功一次即清零连续失败计数；下次再失败重新累计
			if ti.store != nil && dec.LeaderPosID != "" {
				if rerr := ti.store.CopyTrade().ResetMappingFailure(ti.traderID, dec.LeaderPosID); rerr != nil {
					logger.Debugf("[%s] 清零失败计数失败（不影响主流程）: %v", ti.traderID, rerr)
				}
			}

			// 📨 Binance 跟单成功动作邮件通知（PR-Notify-1）
			//
			// 触发条件（同时满足）：
			//   1. 跟单数据源 = Binance（OKX / Hyperliquid 不发，作用域隔离）
			//   2. NOTIFY_BINANCE_COPY_ACTION_ENABLED=true（全局 env 开关，默认关）
			//   3. 全局 notifier 已启用（NOTIFY_EMAIL_ENABLED=true + SMTP 完整）
			//
			// 限流粒度（L1 精修）：<traderID>|<symbol>|<action>
			//   - 同动作 60s 内最多 1 封（避免领航员高频加仓邮件爆量）
			//   - 不同动作（如 open_long 与 close_long）互不压制
			//
			// 与失败/熔断/凭证告警互斥共存：成功路径走这里，失败路径走上面的告警分支。
			if ti.engine.config.ProviderType == ProviderBinance && notifier.CopyTradeActionEnabled() {
				ti.sendCopyActionAlert(dec, duration)
			}
		}

		decisionActions = append(decisionActions, action)
	}

	// 保存到 decision_records 表，复用现有日志系统
	ti.saveDecisionRecord(fullDec, decisionActions, executionLogs)
}

// isBenignCloseError 判断 close/reduce 类决策的失败是否属于"本地仓位已不存在"。
//
// 触发场景（举例）：
//   - 跟随者本地从未真正开仓成功（历史失败遗留的 active mapping）
//   - 跟随者本地仓位被用户手动平掉
//   - 跟随者本地仓位被 OKX/Binance 强平
//   - 跟随者切换账户/重启等导致仓位状态错位
//
// 仅作用于 close_*/reduce_* 决策；open_*/add_* 永远返回 false。
//
// 关键字来源（统一小写匹配）：
//   - "position not found"        OKX (okx_trader.go:856,946)、Bitget (bitget_trader.go:613,676)
//   - "no long position" / "no short position"
//                                 Binance (binance_futures.go:443,498)
//                                 Aster (aster_trader.go:755,846)
//                                 Hyperliquid (hyperliquid_trader.go:516,588)
//                                 Bybit (bybit_trader.go:383,428 — "no X position to close")
//   - "reduceonly order is rejected" / "position size is 0"
//                                 Binance fapi 返回的 reduce-only 拒绝（保险关键字）
func (ti *TraderIntegration) isBenignCloseError(dec *decision.Decision, err error) bool {
	if err == nil || dec == nil {
		return false
	}
	switch dec.Action {
	case "close_long", "close_short", "reduce_long", "reduce_short":
		// 允许进入良性判定
	default:
		return false
	}

	msg := strings.ToLower(err.Error())
	keywords := []string{
		"position not found",
		"no long position",
		"no short position",
		"reduceonly order is rejected",
		"reduce only order is rejected",
		"position size is 0",
	}
	for _, kw := range keywords {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// handleBenignCloseFailure 处理良性 close/reduce 失败：
//   - 主动关闭 mapping（断开死循环）
//   - 写 silent_close 信号日志（与真正 failed 区分）
//   - 不发邮件告警（数据自愈不需要通知用户）
//
// 必须仅在 isBenignCloseError(dec, err) == true 时调用。
func (ti *TraderIntegration) handleBenignCloseFailure(dec *decision.Decision, err error) {
	traderName := ti.traderDisplayName()
	logger.Warnf("🟡 [%s/%s] 良性 close 失败 → 主动关闭映射 | %s %s | error=%v",
		traderName, ti.traderID, dec.Action, dec.Symbol, err)

	if ti.store != nil && dec.LeaderPosID != "" {
		// 复用 SavePositionMapping/CloseMapping 路径，保持 status='closed' 语义一致
		if cerr := ti.store.CopyTrade().CloseMapping(ti.traderID, dec.LeaderPosID, dec.EntryPrice); cerr != nil {
			// 不阻断流程，但记录便于排查；此时 mapping 仍可能保持 active，
			// 下一轮 detectBinancePositionSnapshotFills/matchCloseReduceSignal 会再次进入本路径
			logger.Warnf("⚠️ [%s] 关闭映射失败（已记 silent_close）: %v", ti.traderID, cerr)
		} else {
			logger.Infof("📝 [%s] 仓位映射已自动关闭（良性 close）| posId=%s %s",
				ti.traderID, dec.LeaderPosID, dec.Symbol)
		}
	}

	ti.saveSignalLog(dec, "silent_close", err.Error())
}

// checkAndTripMappingCircuit 累加 active mapping 的连续失败次数；
// 达到阈值则触发熔断：CloseMapping + 发送一次告警邮件。
//
// 说明：
//   - 仅对存在 LeaderPosID 的决策有效（开仓/加仓/平仓/减仓都会带 LeaderPosID）
//   - 良性失败已在上层走 handleBenignCloseFailure 分支，不会进入本路径
//   - 调用 IncrementMappingFailure 返回 0 表示无 active mapping（无需熔断）
//   - 熔断告警与普通失败告警走不同 DedupKey，确保用户能感知"系统主动止损"
func (ti *TraderIntegration) checkAndTripMappingCircuit(dec *decision.Decision, execErr error) {
	if ti.store == nil || dec == nil || dec.LeaderPosID == "" {
		return
	}

	count, err := ti.store.CopyTrade().IncrementMappingFailure(ti.traderID, dec.LeaderPosID, execErr.Error())
	if err != nil {
		logger.Warnf("[%s] 累加失败计数失败: %v", ti.traderID, err)
		return
	}
	if count == 0 {
		// 无 active mapping（可能已被良性失败/历史清理流程关闭），无熔断对象
		return
	}
	if count < mappingFailureCircuitThreshold {
		logger.Debugf("[%s] mapping 失败计数 %d/%d | %s %s",
			ti.traderID, count, mappingFailureCircuitThreshold, dec.Action, dec.Symbol)
		return
	}

	// 触发熔断：先关闭映射，再发独立告警
	traderName := ti.traderDisplayName()
	if cerr := ti.store.CopyTrade().CloseMapping(ti.traderID, dec.LeaderPosID, dec.EntryPrice); cerr != nil {
		logger.Warnf("⚠️ [%s] 熔断关闭映射失败: %v", ti.traderID, cerr)
	}
	logger.Warnf("🛑 [%s/%s] mapping 熔断 | 连续失败 %d 次 → 主动关闭 | %s %s | 最近错误=%v",
		traderName, ti.traderID, count, dec.Action, dec.Symbol, execErr)

	alertKey := fmt.Sprintf("circuit|%s|%s", ti.traderID, dec.LeaderPosID)
	notifier.Notify(notifier.Alert{
		Category: "copy_trade",
		TraderID: ti.traderID,
		Title:    fmt.Sprintf("%s | 跟单映射熔断（%s %s）", traderName, dec.Action, dec.Symbol),
		Body: fmt.Sprintf(
			"跟随者 %s 的 leaderPosID=%s 已连续失败 %d 次（阈值 %d），系统主动关闭该映射，停止重试。\n"+
				"最近一次错误：%v\n"+
				"建议人工排查：领航员当前持仓状态、跟随者账户余额/杠杆/保证金模式，确认无误后可由领航员重新开仓自动恢复跟单。",
			traderName, dec.LeaderPosID, count, mappingFailureCircuitThreshold, execErr),
		RateKey:  alertKey,
		DedupKey: alertKey,
		Fields: map[string]string{
			"TraderName":  traderName,
			"Provider":    string(ti.engine.config.ProviderType),
			"Leader":      ti.engine.config.LeaderID,
			"Action":      dec.Action,
			"Symbol":      dec.Symbol,
			"LeaderPosID": dec.LeaderPosID,
			"FailCount":   fmt.Sprintf("%d", count),
			"Threshold":   fmt.Sprintf("%d", mappingFailureCircuitThreshold),
			"LastError":   execErr.Error(),
		},
	})
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

// sendCopyActionAlert 发送一封"Binance 跟单成功执行动作"邮件。
//
// 调用前置条件（由调用点保证，本函数不再重复判断，保持职责单一）：
//   - 跟单数据源 = Binance
//   - notifier.CopyTradeActionEnabled() == true
//   - 决策已成功执行（err == nil）
//
// 限流键 = "copy_action|<traderID>|<symbol>|<action>"：
//   - 同 trader 同 symbol 同 action 60s 内最多 1 封
//   - 不同 action（open/add/close/reduce）互不压制
//
// 注意：不设 DedupKey（一次性去重），让每个独立动作窗口都能发；
// 限流由全局 MinInterval 控制。
func (ti *TraderIntegration) sendCopyActionAlert(dec *decision.Decision, durationMs int64) {
	if dec == nil {
		return
	}
	traderName := ti.traderDisplayName()
	leaderID := ""
	if ti.engine != nil && ti.engine.config != nil {
		leaderID = ti.engine.config.LeaderID
	}

	rateKey := fmt.Sprintf("copy_action|%s|%s|%s", ti.traderID, dec.Symbol, dec.Action)

	notifier.Notify(notifier.Alert{
		Category: "copy_trade",
		TraderID: ti.traderID,
		Title:    fmt.Sprintf("%s | 跟单成功 %s %s", traderName, dec.Action, dec.Symbol),
		Body:     ti.buildCopyActionAlertBody(dec, traderName, durationMs),
		RateKey:  rateKey,
		Fields: map[string]string{
			"TraderName":    traderName,
			"Provider":      string(ProviderBinance),
			"Leader":        leaderID,
			"Action":        dec.Action,
			"Symbol":        dec.Symbol,
			"EntryPrice":    fmt.Sprintf("%.4f", dec.EntryPrice),
			"PositionUSD":   fmt.Sprintf("%.2f", dec.PositionSizeUSD),
			"Leverage":      fmt.Sprintf("%dx", dec.Leverage),
			"MarginMode":    dec.MarginMode,
			"LeaderPosID":   dec.LeaderPosID,
			"LeaderPosSize": fmt.Sprintf("%.6f", dec.LeaderPosSize),
			"CloseRatio":    fmt.Sprintf("%.4f", dec.CloseRatio),
			"DurationMs":    fmt.Sprintf("%d", durationMs),
		},
	})
}

// buildCopyActionAlertBody 构造跟单成功动作的告警正文。
// 与 buildExecFailureAlertBody 结构对称，便于用户在收件箱中并列阅读。
func (ti *TraderIntegration) buildCopyActionAlertBody(dec *decision.Decision, traderName string, durationMs int64) string {
	leaderID := ""
	if ti.engine != nil && ti.engine.config != nil {
		leaderID = ti.engine.config.LeaderID
	}
	return fmt.Sprintf(
		"跟单执行成功 (Copy Trade Action Executed)\n\n"+
			"Trader Name: %s\n"+
			"Trader ID:   %s\n"+
			"Provider:    binance\n"+
			"Leader ID:   %s\n"+
			"Action:      %s\n"+
			"Symbol:      %s\n"+
			"EntryPrice:  %.4f\n"+
			"PositionUSD: %.2f\n"+
			"Leverage:    %dx\n"+
			"MarginMode:  %s\n"+
			"LeaderPosID: %s\n"+
			"LeaderSize:  %.6f\n"+
			"CloseRatio:  %.4f\n"+
			"Duration:    %dms",
		traderName,
		ti.traderID,
		leaderID,
		dec.Action,
		dec.Symbol,
		dec.EntryPrice,
		dec.PositionSizeUSD,
		dec.Leverage,
		dec.MarginMode,
		dec.LeaderPosID,
		dec.LeaderPosSize,
		dec.CloseRatio,
		durationMs,
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
// v3 风控：账户保护止损（SL）管理钩子
//
// 设计原则：
//  1. 仅 OKX 路径生效（HL/Binance 暂不支持账户保护 SL）
//  2. executor 不实现 StopLossManager 接口时降级（不报错，仅记 Debug 日志）
//  3. 任何 SL 操作失败都不阻断主交易流程（仅记日志 + 由 orphan_algo_sweep 兜底）
// ============================================================================

// shouldManageStopLoss 判断当前决策是否走 v3 风控 SL 管理路径
// 返回 false 时跳过所有 SL 钩子（透明降级）
func (ti *TraderIntegration) shouldManageStopLoss(dec *decision.Decision) bool {
	if ti.engine == nil || ti.engine.config == nil {
		return false
	}
	if ti.engine.config.ProviderType != ProviderOKX {
		return false
	}
	if !ti.engine.config.RiskStopLossEnabled {
		return false
	}
	if dec == nil || dec.Symbol == "" {
		return false
	}
	return true
}

// cancelExistingStopLoss 执行前撤旧 SL
// 调用时机：跟单 OKX 开仓/加仓/减仓/平仓 → ExecuteDecision 之前
// 设计：失败不阻断（只是没撤掉旧单），主流程继续；旧单触发可能比新单更紧，安全降级
func (ti *TraderIntegration) cancelExistingStopLoss(dec *decision.Decision, where string) {
	if !ti.shouldManageStopLoss(dec) {
		return
	}
	slMgr, ok := ti.executor.(StopLossManager)
	if !ok {
		return
	}
	if err := slMgr.CancelStopLossOrders(dec.Symbol); err != nil {
		logger.Debugf("[%s] 撤旧 SL 失败（%s，不阻断主流程）: %v | %s", ti.traderID, where, err, dec.Symbol)
	} else {
		logger.Debugf("🧹 [%s] 撤旧 SL 完成 | %s @%s", ti.traderID, dec.Symbol, where)
	}
}

// refreshStopLossAfterExecute 执行成功后用实际成交均价精确重挂 SL
//
// 调用时机：跟单 OKX 开仓/加仓/部分减仓执行成功 + mapping 更新完成后
// 平仓（close_long/close_short）不重挂（仓位已清空）
//
// 实现细节：
//  1. 通过 GetPositions() 拿到最新本地持仓的 EntryPrice / Quantity（实际成交均价）
//  2. 用 calcStopLossPrice 算 SL 价
//  3. 调 SetStopLoss 挂 algo 单
//
// 失败处理：日志告警 + 由 engine.checkStoppedByRisk 的 orphan 兜底 + 估算版 dec.StopLoss 还在
func (ti *TraderIntegration) refreshStopLossAfterExecute(dec *decision.Decision) {
	if !ti.shouldManageStopLoss(dec) {
		return
	}

	// 平仓不重挂
	switch dec.Action {
	case "close_long", "close_short":
		return
	}

	slMgr, ok := ti.executor.(StopLossManager)
	if !ok {
		logger.Debugf("[%s] executor 未实现 StopLossManager，跳过精确重挂 SL", ti.traderID)
		return
	}

	// 拿最新持仓
	positions, err := ti.executor.GetPositions()
	if err != nil {
		logger.Warnf("⚠️ [%s] 重挂 SL 失败（GetPositions 错误）: %v | %s", ti.traderID, err, dec.Symbol)
		return
	}

	// 匹配本次决策对应的本地仓位
	// 优先按 posId 匹配（OKX 精确），fallback 按 symbol+side+marginMode
	expectedSide := ""
	switch dec.Action {
	case "open_long", "reduce_long":
		expectedSide = "long"
	case "open_short", "reduce_short":
		expectedSide = "short"
	default:
		return
	}

	// 本地仓位匹配：用 symbol+side+marginMode 三元组
	// 关键认知：跟随者账户的本地 posId（pos["posId"]）与领航员的 posId（dec.LeaderPosID）
	// 属于不同账户，永远不会相等，因此不能用 posId 跨账户匹配
	var matchedPos map[string]interface{}
	for _, pos := range positions {
		symbol, _ := pos["symbol"].(string)
		sideStr, _ := pos["side"].(string)
		marginMode := getStringField(pos, "marginMode", "mgnMode")
		quantity := getFloatField(pos, "positionAmt", "quantity")

		if symbol != dec.Symbol || sideStr != expectedSide || quantity == 0 {
			continue
		}

		// 优先按 marginMode 严格匹配（OKX 全仓/逐仓是独立 posId，必须区分）
		if dec.MarginMode != "" && marginMode != "" && marginMode == dec.MarginMode {
			matchedPos = pos
			break
		}
		// 兜底：第一个 symbol+side 匹配的（dec.MarginMode 为空 或 本地未返回 marginMode）
		if matchedPos == nil {
			matchedPos = pos
		}
	}

	if matchedPos == nil {
		logger.Warnf("⚠️ [%s] 重挂 SL：找不到本地仓位 | %s %s posId=%s", ti.traderID, dec.Symbol, expectedSide, dec.LeaderPosID)
		return
	}

	entryPrice := getFloatField(matchedPos, "entryPrice", "entry_price")
	quantity := absFloat(getFloatField(matchedPos, "positionAmt", "quantity"))
	leverage := getIntOrFloatField(matchedPos, "leverage")
	if leverage <= 0 {
		leverage = dec.Leverage
	}
	if entryPrice <= 0 || quantity <= 0 {
		logger.Warnf("⚠️ [%s] 重挂 SL：本地仓位数据异常 | entry=%.4f qty=%.4f", ti.traderID, entryPrice, quantity)
		return
	}

	followerEquity := ti.getBalanceFunc()()
	if followerEquity <= 0 {
		logger.Warnf("⚠️ [%s] 重挂 SL：跟随者权益为零", ti.traderID)
		return
	}

	side := SideLong
	if expectedSide == "short" {
		side = SideShort
	}

	slInput := &StopLossCalcInput{
		Symbol:         dec.Symbol,
		Side:           side,
		EntryPrice:     entryPrice,
		Leverage:       leverage,
		PositionValue:  entryPrice * quantity,
		FollowerEquity: followerEquity,
	}

	slResult, err := calcStopLossPrice(ti.engine.config, slInput)
	if err != nil {
		logger.Warnf("⚠️ [%s] 重挂 SL：算法失败: %v | %s", ti.traderID, err, dec.Symbol)
		return
	}
	if slResult.SLPrice <= 0 {
		if slResult.OpenImmediateHit {
			logger.Warnf("⚠️ [%s] 重挂 SL：SL 距离过近(<0.1%%)，跳过挂单 | %s entry=%.4f", ti.traderID, dec.Symbol, entryPrice)
		}
		return
	}

	// 先撤旧（双保险，覆盖 ExecuteDecision 内部自动挂的估算 SL）
	if err := slMgr.CancelStopLossOrders(dec.Symbol); err != nil {
		logger.Debugf("[%s] 重挂前撤旧 SL 失败（不阻断）: %v", ti.traderID, err)
	}

	// 挂新（用实际成交价计算）
	positionSide := "LONG"
	if expectedSide == "short" {
		positionSide = "SHORT"
	}
	if err := slMgr.SetStopLoss(dec.Symbol, positionSide, quantity, slResult.SLPrice); err != nil {
		logger.Errorf("❌ [%s] 重挂 SL 失败（账户保护可能失效）| %s %s SL=%.4f: %v",
			ti.traderID, dec.Symbol, positionSide, slResult.SLPrice, err)

		// 邮件告警：SL 挂单失败 = 账户保护可能失效，用户必须感知
		// RateKey 按 trader+symbol+side 限流，避免同一仓位反复挂失败时邮件爆量
		traderName := ti.traderDisplayName()
		alertKey := fmt.Sprintf("sl_attach_failed|%s|%s|%s", ti.traderID, dec.Symbol, positionSide)
		notifier.Notify(notifier.Alert{
			Category: "copy_trade",
			TraderID: ti.traderID,
			Title:    fmt.Sprintf("%s | 账户保护 SL 挂单失败（%s %s）", traderName, dec.Symbol, positionSide),
			Body: fmt.Sprintf(
				"账户保护止损单挂单失败 (Stop Loss Attach Failed)\n\n"+
					"Trader Name: %s\n"+
					"Trader ID:   %s\n"+
					"Symbol:      %s\n"+
					"Side:        %s\n"+
					"Entry Price: %.4f\n"+
					"SL Price:    %.4f (距离 %.2f%%, 控线=%s)\n"+
					"Quantity:    %.4f\n\n"+
					"错误信息 (Error):\n%v\n\n"+
					"⚠️ 该仓位当前没有交易所托管的止损单，账户保护可能失效。\n"+
					"建议：手动在 OKX 上检查该仓位的 algo 条件单，或重启该交易员让对账逻辑重新挂单。",
				traderName, ti.traderID, dec.Symbol, positionSide,
				entryPrice, slResult.SLPrice, (slResult.SLDistance/entryPrice)*100, slResult.GovernedBy,
				quantity, err),
			RateKey:  alertKey,
			DedupKey: alertKey,
			Fields: map[string]string{
				"TraderName":  traderName,
				"Symbol":      dec.Symbol,
				"Side":        positionSide,
				"EntryPrice":  fmt.Sprintf("%.4f", entryPrice),
				"SLPrice":     fmt.Sprintf("%.4f", slResult.SLPrice),
				"Quantity":    fmt.Sprintf("%.4f", quantity),
				"GovernedBy":  slResult.GovernedBy,
				"LastError":   err.Error(),
			},
		})
		return
	}

	logger.Infof("🛑 [%s] SL 精确重挂 | %s %s | 入场=%.4f SL=%.4f 距离=%.4f(%.2f%%) 控线=%s qty=%.4f",
		ti.traderID, dec.Symbol, positionSide,
		entryPrice, slResult.SLPrice, slResult.SLDistance,
		(slResult.SLDistance/entryPrice)*100, slResult.GovernedBy, quantity)
}

// ============================================================================
// v3 风控：邮件告警（SL 触发 + 二次进场）
//
// 设计：engine 内部检测到风控事件 → 推入 riskEventCh channel；
// integration 这层协程消费 → 发邮件告警（能拿 trader name，与现有 sendCopyActionAlert 风格一致）
//
// 触发邮件的事件：
//  1. SL 被交易所触发（engine.checkStoppedByRisk 标记 stopped_by_risk 后）
//  2. 二次进场决策已生成（engine.emitReentryDecision 推出决策后）
//
// 注意：决策执行成功的告警仍由 executeFullDecision 的成功分支处理（识别 reentry decision 后单独发）
// ============================================================================

// consumeRiskEvents 消费引擎推送的风控事件，转发为邮件告警
// 调用时机：StartCopyTrading 启动时作为独立协程运行，与 consumeDecisions 平行
func (ti *TraderIntegration) consumeRiskEvents() {
	if ti.engine == nil {
		return
	}
	eventCh := ti.engine.GetRiskEventChannel()
	for {
		select {
		case <-ti.ctx.Done():
			return
		case event, ok := <-eventCh:
			if !ok {
				return
			}
			if event == nil {
				continue
			}
			switch event.Type {
			case RiskEventStopLossTriggered:
				ti.sendStopLossTriggerAlert(event)
			case RiskEventReentryInitiated:
				ti.sendReentryInitiatedAlert(event)
			default:
				logger.Debugf("[%s] 未知风控事件类型: %s", ti.traderID, event.Type)
			}
		}
	}
}

// sendStopLossTriggerAlert 发送账户保护止损触发邮件
//
// 限流：RateKey + DedupKey 都用 "sl_trigger|<traderID>|<posId>"
//   - RateKey：60s 内同 posId 最多 1 封（防止频繁对账重发）
//   - DedupKey：彻底去重（同 posId 一次性，理论上 SL 仅触发一次）
//
// 邮件正文：完整上下文，让用户能立刻判断后续动作（是否手动干预、是否启用二次进场等）
func (ti *TraderIntegration) sendStopLossTriggerAlert(event *RiskEvent) {
	traderName := ti.traderDisplayName()
	providerType := ""
	leaderID := ""
	if ti.engine != nil && ti.engine.config != nil {
		providerType = string(ti.engine.config.ProviderType)
		leaderID = ti.engine.config.LeaderID
	}

	alertKey := fmt.Sprintf("sl_trigger|%s|%s", ti.traderID, event.LeaderPosID)

	// 恢复条件文案（根据是否启用二次进场动态调整）
	recoveryHint := "等领航员完全平掉旧 posId 后，下次他重新开仓时跟单系统自动恢复跟随"
	if ti.engine != nil && ti.engine.config != nil && ti.engine.config.RiskReentryEnabled {
		recoveryHint = fmt.Sprintf(
			"已启用二次进场：若价格回到入场价附近(±%.2f%%)、领航员浮亏收窄一半、未继续加仓 → 自动按 %.0f%% 系数重入；\n"+
				"  或等领航员完全平掉旧 posId，下次他新开仓时自动恢复跟随",
			ti.engine.config.RiskReentryTolerance*100, ti.engine.config.RiskReentryRatio*100)
	}

	body := fmt.Sprintf(
		"🛑 账户保护止损已触发 (Stop Loss Triggered by Exchange)\n\n"+
			"Trader Name:   %s\n"+
			"Trader ID:     %s\n"+
			"Provider:      %s\n"+
			"Leader ID:     %s\n"+
			"Symbol:        %s\n"+
			"Side:          %s\n"+
			"Margin Mode:   %s\n"+
			"Leader PosID:  %s\n\n"+
			"📊 SL 触发时领航员状态:\n"+
			"  浮亏 (PnL):    %.2f USDT\n"+
			"  持仓数量:      %.6f\n"+
			"  跟随加仓次数:  %d\n\n"+
			"📋 跟单状态:\n"+
			"  本地仓位:      已被 OKX algo 条件单兜底平仓\n"+
			"  Mapping:       已转为 stopped_by_risk，该 posId 后续信号被忽略\n\n"+
			"🔄 恢复条件:\n"+
			"  %s\n\n"+
			"💡 提示:\n"+
			"  此次触发是「价格反向走到账户风险线」导致，由交易所托管的 algo 条件单自动平仓。\n"+
			"  系统已自动避免在被打过的仓位上反复进出，保护账户。",
		traderName, ti.traderID, providerType, leaderID,
		event.Symbol, event.Side, event.MarginMode, event.LeaderPosID,
		event.LeaderPnL, event.LeaderSize, event.AddCount,
		recoveryHint)

	notifier.Notify(notifier.Alert{
		Time:     event.Timestamp,
		Category: "copy_trade",
		TraderID: ti.traderID,
		Title:    fmt.Sprintf("%s | 账户保护止损触发 %s %s", traderName, event.Symbol, event.Side),
		Body:     body,
		RateKey:  alertKey,
		DedupKey: alertKey,
		Fields: map[string]string{
			"TraderName":  traderName,
			"Provider":    providerType,
			"Leader":      leaderID,
			"Symbol":      event.Symbol,
			"Side":        event.Side,
			"MarginMode":  event.MarginMode,
			"LeaderPosID": event.LeaderPosID,
			"LeaderPnL":   fmt.Sprintf("%.2f", event.LeaderPnL),
			"LeaderSize":  fmt.Sprintf("%.6f", event.LeaderSize),
			"AddCount":    fmt.Sprintf("%d", event.AddCount),
		},
	})

	logger.Infof("📧 [%s] 已发送 SL 触发告警邮件 | posId=%s", ti.traderID, event.LeaderPosID)
}

// sendReentryInitiatedAlert 发送二次进场决策已生成邮件
//
// 限流：RateKey + DedupKey 都用 "reentry|<traderID>|<posId>"
//   - 同 posId 二次进场限 1 次（与判据 E 的 ReentryUsed 安全阀对应），DedupKey 彻底去重
//
// 注意：这是"决策已生成"告警。实际执行成功/失败的告警分别走：
//   - 成功：executeFullDecision 成功分支（识别 reentry decision 后单独发 sendReentryExecutedAlert）
//   - 失败：execFailureDedupKey 路径（普通跟单失败告警）
func (ti *TraderIntegration) sendReentryInitiatedAlert(event *RiskEvent) {
	traderName := ti.traderDisplayName()
	providerType := ""
	leaderID := ""
	copyRatio := float64(0)
	reentryRatio := float64(0)
	if ti.engine != nil && ti.engine.config != nil {
		providerType = string(ti.engine.config.ProviderType)
		leaderID = ti.engine.config.LeaderID
		copyRatio = ti.engine.config.CopyRatio
		reentryRatio = ti.engine.config.RiskReentryRatio
	}

	alertKey := fmt.Sprintf("reentry|%s|%s", ti.traderID, event.LeaderPosID)

	body := fmt.Sprintf(
		"🔄 二次进场决策已触发 (Reentry Decision Initiated, Judge E)\n\n"+
			"Trader Name:   %s\n"+
			"Trader ID:     %s\n"+
			"Provider:      %s\n"+
			"Leader ID:     %s\n"+
			"Symbol:        %s\n"+
			"Side:          %s\n"+
			"Margin Mode:   %s\n"+
			"Leader PosID:  %s\n\n"+
			"📊 判据 E 满足条件:\n"+
			"  领航员仍持仓:  ✓ size=%.6f\n"+
			"  价格回归:      ✓\n"+
			"  浮亏收窄:      ✓ 当前 %.2f USDT (已收窄到 SL 触发时的 ≤50%%)\n"+
			"  未继续加仓:    ✓ 领航员 size 未超过 SL 触发时\n"+
			"  重入限 1 次:    ✓ 同 posId 仅此一次\n\n"+
			"💰 重入参数:\n"+
			"  入场价基准:    %.4f\n"+
			"  重入金额:      %.2f USDT (=跟单系数 %.0f%% × 重入系数 %.0f%%)\n\n"+
			"⚠️ 风控约束:\n"+
			"  - 同 posId 重入仅 1 次，重入后再次被 SL 触发将永久熔断该 posId\n"+
			"  - 重入决策已推送给执行器，实际执行结果将另行告警（成功/失败）",
		traderName, ti.traderID, providerType, leaderID,
		event.Symbol, event.Side, event.MarginMode, event.LeaderPosID,
		event.LeaderSize, event.LeaderPnL,
		event.ReentryEntryPrice, event.ReentrySize, copyRatio*100, reentryRatio*100)

	notifier.Notify(notifier.Alert{
		Time:     event.Timestamp,
		Category: "copy_trade",
		TraderID: ti.traderID,
		Title:    fmt.Sprintf("%s | 二次进场触发 %s %s", traderName, event.Symbol, event.Side),
		Body:     body,
		RateKey:  alertKey,
		DedupKey: alertKey,
		Fields: map[string]string{
			"TraderName":        traderName,
			"Provider":          providerType,
			"Leader":            leaderID,
			"Symbol":            event.Symbol,
			"Side":              event.Side,
			"LeaderPosID":       event.LeaderPosID,
			"LeaderPnL":         fmt.Sprintf("%.2f", event.LeaderPnL),
			"LeaderSize":        fmt.Sprintf("%.6f", event.LeaderSize),
			"ReentryEntryPrice": fmt.Sprintf("%.4f", event.ReentryEntryPrice),
			"ReentrySize":       fmt.Sprintf("%.2f", event.ReentrySize),
		},
	})

	logger.Infof("📧 [%s] 已发送二次进场触发告警邮件 | posId=%s", ti.traderID, event.LeaderPosID)
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
