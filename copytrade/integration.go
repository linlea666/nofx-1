package copytrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"nofx/copyguardmetrics"
	"nofx/decision"
	"nofx/logger"
	"nofx/market"
	"nofx/notifier"
	"nofx/store"
	"nofx/trader"
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

type ProtectiveStopManagerV4 interface {
	PlaceProtectiveStop(req trader.ProtectiveStopRequest) (*trader.ProtectiveStopOrder, error)
	AmendProtectiveStop(algoID string, req trader.ProtectiveStopRequest) error
	GetProtectiveStop(algoID, symbol string) (*trader.ProtectiveStopOrder, error)
	GetProtectiveStopByClientID(clientID, symbol string) (*trader.ProtectiveStopOrder, error)
	CancelProtectiveStop(algoID, symbol string) error
}

type ClosedPnLProvider interface {
	GetClosedPnL(start time.Time, limit int) ([]trader.ClosedPnLRecord, error)
}

// ClosedPnLByPositionProvider is optional (OKX only): callers must fall back
// to the time-window GetClosedPnL scan when the executor lacks it.
type ClosedPnLByPositionProvider interface {
	GetClosedPnLByPositionID(symbol, posID string, limit int) ([]trader.ClosedPnLRecord, error)
}

type ClosedPnLBySymbolProvider interface {
	GetClosedPnLBySymbol(symbol string, start time.Time, limit int) ([]trader.ClosedPnLRecord, error)
}
type OrderStatusProvider interface {
	GetOrderStatus(symbol, orderID string) (map[string]interface{}, error)
}
type ClientOrderStatusProvider interface {
	GetOrderStatusByClientID(symbol, clientOrderID string) (map[string]interface{}, error)
}
type FreshPositionsProvider interface {
	GetPositionsFresh() ([]map[string]interface{}, error)
}
type EmergencyPositionCloser interface {
	ClosePositionMarket(symbol, side string) (string, error)
}

// ProtectiveStopCapabilityChecker lets Copy Guard probe whether the concrete
// execution exchange (not the AutoTrader wrapper) can place exchange-managed
// protective stops, so startup can reject incapable venues up front.
type ProtectiveStopCapabilityChecker interface {
	SupportsProtectiveStops() bool
}

type CopyGuardCapabilityValidator interface {
	ValidateCopyGuardCapabilities() error
}

var (
	_ DecisionExecutor                = (*trader.AutoTrader)(nil)
	_ StopLossManager                 = (*trader.AutoTrader)(nil)
	_ ProtectiveStopManagerV4         = (*trader.AutoTrader)(nil)
	_ ClosedPnLProvider               = (*trader.AutoTrader)(nil)
	_ ClosedPnLByPositionProvider     = (*trader.AutoTrader)(nil)
	_ OrderStatusProvider             = (*trader.AutoTrader)(nil)
	_ ClientOrderStatusProvider       = (*trader.AutoTrader)(nil)
	_ FreshPositionsProvider          = (*trader.AutoTrader)(nil)
	_ ProtectiveStopCapabilityChecker = (*trader.AutoTrader)(nil)
	_ EmergencyPositionCloser         = (*trader.AutoTrader)(nil)
)

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
	traderID             string
	executor             DecisionExecutor
	engine               *Engine
	store                *store.Store
	ctx                  context.Context
	cancel               context.CancelFunc
	running              bool
	lastAttemptReconcile map[int64]time.Time
	// protectiveQueryFailures counts consecutive protective-stop lookup
	// failures per cycle so a single API hiccup does not flag the cycle.
	// Only touched from the monitorV4ProtectiveStops goroutine.
	protectiveQueryFailures map[int64]int
	// lastV4Backfill rate-limits the lifecycle backfill sweep for positions
	// followed before Copy Guard v4 was enabled.
	lastV4Backfill       time.Time
	lastCycleSummaryScan time.Time
	// lastProtectionEvent rate-limits repeated identical protection-issue
	// events (poll runs every 3s; a stuck failure used to write thousands of
	// duplicate rows per cycle). Guarded by protectionEventMu because
	// markProtectionIssue runs from both the monitor and executor goroutines.
	protectionEventMu   sync.Mutex
	lastProtectionEvent map[string]time.Time
	residualCloseLast   map[int64]time.Time
	residualCloseTries  map[int64]int
	cycleNumber         int // 跟单周期计数器
}

// NewTraderIntegration 创建交易集成
func NewTraderIntegration(
	traderID string,
	executor DecisionExecutor,
	st *store.Store,
) *TraderIntegration {
	ctx, cancel := context.WithCancel(context.Background())
	return &TraderIntegration{
		traderID:                traderID,
		executor:                executor,
		store:                   st,
		ctx:                     ctx,
		cancel:                  cancel,
		lastAttemptReconcile:    make(map[int64]time.Time),
		protectiveQueryFailures: make(map[int64]int),
		lastProtectionEvent:     make(map[string]time.Time),
		residualCloseLast:       make(map[int64]time.Time),
		residualCloseTries:      make(map[int64]int),
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
	if SupportsCopyGuard(ProviderType(copyConfig.ProviderType)) && copyConfig.RiskPolicyVersion >= 4 {
		if err := validateV4ExecutorCapabilities(ti.executor); err != nil {
			return fmt.Errorf("Copy Guard v4 runtime unavailable: %w", err)
		}
	}

	// 转换为引擎配置
	engineConfig := &CopyConfig{
		ProviderType:       ProviderType(copyConfig.ProviderType),
		LeaderID:           copyConfig.LeaderID,
		CopyRatio:          copyConfig.CopyRatio,
		SyncLeverage:       copyConfig.SyncLeverage,
		SyncMarginMode:     copyConfig.SyncMarginMode,
		MinTradeWarn:       copyConfig.MinTradeWarn,
		MaxTradeWarn:       copyConfig.MaxTradeWarn,
		BinanceP20T:        copyConfig.BinanceP20T,
		BinanceCSRFToken:   copyConfig.BinanceCSRFToken,
		BinanceSourceMode:  BinanceSourceMode(copyConfig.BinanceSourceMode),
		BinanceTopTraderID: copyConfig.BinanceTopTraderID,
		SourceGeneration:   copyConfig.SourceGeneration,

		// Copy Guard 风控字段透传（v5：两层硬止损 + 可保护性状态机 + 确认式重入）
		RiskStopLossEnabled:        copyConfig.RiskStopLossEnabled,
		RiskAccountPct:             copyConfig.RiskAccountPct,
		RiskATRMultiplier:          copyConfig.RiskATRMultiplier,
		RiskATRTimeframe:           copyConfig.RiskATRTimeframe,
		RiskLeverageFallback:       copyConfig.RiskLeverageFallback,
		RiskLeverageMaxLoss:        copyConfig.RiskLeverageMaxLoss,
		RiskReentryEnabled:         copyConfig.RiskReentryEnabled,
		RiskReentryRatio:           copyConfig.RiskReentryRatio,
		RiskReentryDecisionMode:    copyConfig.RiskReentryDecisionMode,
		RiskCycleLossBudgetPct:     copyConfig.RiskCycleLossBudgetPct,
		RiskPortfolioLossBudgetPct: copyConfig.RiskPortfolioLossBudgetPct,
		RiskRoundTripFeeBPS:        copyConfig.RiskRoundTripFeeBPS,
		RiskAIConfidenceThreshold:  copyConfig.RiskAIConfidenceThreshold,
		RiskAIMinReviewSeconds:     copyConfig.RiskAIMinReviewSeconds,
		RiskAIDailyCallLimit:       copyConfig.RiskAIDailyCallLimit,
		RiskAILifecycleCallLimit:   copyConfig.RiskAILifecycleCallLimit,
		RiskNotificationLevel:      copyConfig.RiskNotificationLevel,

		RiskManualReentryEnabled: copyConfig.RiskManualReentryEnabled,

		RiskPolicyVersion:          copyConfig.RiskPolicyVersion,
		RiskStopMode:               copyConfig.RiskStopMode,
		RiskATRPeriod:              copyConfig.RiskATRPeriod,
		RiskATRCacheMaxAgeMinutes:  copyConfig.RiskATRCacheMaxAgeMinutes,
		RiskATRFallbackPct:         copyConfig.RiskATRFallbackPct,
		RiskTriggerPriceType:       copyConfig.RiskTriggerPriceType,
		RiskSlippageBufferBPS:      copyConfig.RiskSlippageBufferBPS,
		RiskLiquidationBufferATR:   copyConfig.RiskLiquidationBufferATR,
		RiskMaxReentries:           copyConfig.RiskMaxReentries,
		RiskReentryBandATR:         copyConfig.RiskReentryBandATR,
		RiskReentryCooldownSeconds: copyConfig.RiskReentryCooldownSeconds,
		RiskReentryMaxChaseATR:     copyConfig.RiskReentryMaxChaseATR,
		RiskReentryMaxATRExpansion: copyConfig.RiskReentryMaxATRExpansion,
		RiskWatchTimeoutMinutes:    copyConfig.RiskWatchTimeoutMinutes,
		RiskMigrationConfirmed:     copyConfig.RiskMigrationConfirmed,
		RiskAddonBudgetPct:         copyConfig.RiskAddonBudgetPct,

		// v4.1 重入加严 + v5 可保护性/噪音档
		RiskReentryMinRecoveryATR:     copyConfig.RiskReentryMinRecoveryATR,
		RiskReentryCooldownEscalation: copyConfig.RiskReentryCooldownEscalation,
		RiskReentryRecoveryEscalation: copyConfig.RiskReentryRecoveryEscalation,
		RiskUnprotectableAction:       copyConfig.RiskUnprotectableAction,
		RiskReentryNoiseOverride:      copyConfig.RiskReentryNoiseOverride,
	}
	engineConfig.FillRiskDefaults() // 兜底默认值（旧库迁移 / 前端未传时）
	if err := ValidateRiskPolicyV4(engineConfig); err != nil {
		return fmt.Errorf("invalid copy guard v4 policy: %w", err)
	}

	// 创建引擎（Hyperliquid 使用流式模式，OKX 使用轮询模式）
	var engineOpts []EngineOption
	if engineConfig.ProviderType == ProviderHyperliquid {
		engineOpts = append(engineOpts, WithStreamingMode())
	}
	engineOpts = append(engineOpts, WithFollowerEquity(ti.getEquityFunc()), WithFollowerPositionsResult(ti.getPositionsResultFunc()))
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
	ti.engine = engine
	// 未完成意图必须先于任何源基线写入恢复，避免启动初始化推进 mapping
	// 修订号后掩盖一个尚未对账的真实交易所订单。
	ti.recoverExecutionIntents()

	// 🔑 初始化历史仓位：将领航员当前持仓标记为 ignored
	// 这样后续这些仓位的操作都不会跟随，只跟新开仓
	if err := engine.InitIgnoredPositions(); err != nil {
		if engine.isSmartMoneyMode() {
			return fmt.Errorf("Smart Money 首次基线失败，拒绝启动: %w", err)
		}
		logger.Warnf("⚠️ [%s] 初始化历史仓位失败: %v（兼容旧数据源继续启动）", ti.traderID, err)
	}

	// 启动引擎
	if err := engine.Start(ti.ctx); err != nil {
		traderName := ti.traderDisplayName()
		// 异步发送邮件告警（未启用通知器时为 no-op）
		notifier.Notify(notifier.Alert{
			Category:   "copy_trade",
			TraderID:   ti.traderID,
			TraderName: traderName,
			Title:      "跟单引擎启动失败",
			Body: fmt.Sprintf(
				"跟单引擎启动失败 (Copy Trade Engine Start Failed)\n\n"+
					"Trader Name: %s\n"+
					"Trader ID: %s\n"+
					"Provider:  %s\n"+
					"Leader ID: %s\n\n"+
					"错误信息 (Error):\n%s",
				traderName, ti.traderID,
				copyConfig.ProviderType,
				copyConfig.LeaderID,
				err.Error(),
			),
			Fields: map[string]string{
				"TraderName": traderName,
				"Provider":   copyConfig.ProviderType,
				"Leader":     copyConfig.LeaderID,
				"Reason":     err.Error(),
			},
		})
		// Seam D：引擎启动失败（跟随者根本起不来）也进统一日志，便于追踪。
		// 按小时分桶去重，避免重启循环刷屏。
		ti.recordCopyEvent(&store.CopyTradeEvent{
			LeaderID:     copyConfig.LeaderID,
			ProviderType: copyConfig.ProviderType,
			Category:     store.CopyEventCategoryError,
			EventType:    "ENGINE_START_FAILED",
			Severity:     store.CopyEventSeverityError,
			Status:       "failed",
			Summary:      fmt.Sprintf("跟单引擎启动失败: %s", err.Error()),
			Detail:       map[string]interface{}{"error": err.Error()},
			DedupKey:     fmt.Sprintf("err|%s|engine_start|%d", ti.traderID, time.Now().Unix()/3600),
		})
		return fmt.Errorf("failed to start copy trade engine: %w", err)
	}

	// 启动决策消费协程
	go ti.consumeDecisions()
	// 启动风控事件消费协程（v3 风控：SL 触发 / 二次进场告警邮件）
	go ti.consumeRiskEvents()
	if SupportsCopyGuard(engineConfig.ProviderType) && engineConfig.RiskPolicyVersion >= 4 {
		if !engineConfig.RiskStopLossEnabled {
			// 用户关闭了「账户保护止损」开关：撤销交易所上仍存活的保护单。
			// 若不清理，UI 显示"已关闭"但交易所止损单还活着可能触发，
			// 而触发后的对账（checkStoppedByRisk）又被开关 gate 掉 → 状态机断裂。
			// 配置变更会重启 integration，所以放在 Start 里天然覆盖开关切换与重启两条路径。
			ti.cancelProtectionsOnDisable()
		}
		ti.recoverV4PendingStates()
		go ti.monitorV4ProtectiveStops()
	}

	ti.running = true
	logger.Infof("🚀 [%s] 跟单集成已启动 | provider=%s leader=%s",
		ti.traderID, copyConfig.ProviderType, copyConfig.LeaderID)

	return nil
}

// recoverExecutionIntents reconciles durable mutations before source polling
// resumes. It never places an order: RESERVED intents are released for source
// replay, while submitted intents are adopted only when the exchange proves a
// fill for the persisted client order id.
func (ti *TraderIntegration) recoverExecutionIntents() {
	if ti.store == nil {
		return
	}
	intents, err := ti.store.CopyTrade().ListUnfinishedExecutionIntents(ti.traderID)
	if err != nil {
		logger.Warnf("⚠️ [%s] 读取未完成执行意图失败: %v", ti.traderID, err)
		return
	}
	provider, supportsLookup := ti.executor.(ClientOrderStatusProvider)
	for _, intent := range intents {
		if mapping, mappingErr := ti.store.CopyTrade().GetMapping(ti.traderID, intent.LeaderPosID); mappingErr == nil && mapping != nil && mapping.SourceRevision >= intent.SourceRevision {
			dec := &decision.Decision{Symbol: intent.Symbol, Action: intent.Action, IsCopyTrade: true,
				LeaderPosID: intent.LeaderPosID, LeaderPosSize: intent.LeaderTargetSize, MarginMode: intent.MarginMode,
				SourceFillID: intent.SourceFillID, SourceRevision: intent.SourceRevision, ExecutionIntentID: intent.ID,
				ClientOrderID: intent.ClientOrderID, ExchangeOrderID: intent.ExchangeOrderID,
				RequestedQuantity: intent.RequestedQuantity, QuantizedQuantity: intent.QuantizedQuantity, FilledQuantity: intent.FilledQuantity,
				PositionSizeUSD: intent.RequestedNotional, Reasoning: "Copy trading: recovered acknowledged execution intent"}
			if intent.Action == "open_long" || intent.Action == "open_short" || intent.Action == "reduce_long" || intent.Action == "reduce_short" {
				ti.refreshStopLossAfterExecute(dec)
			}
			if dec.ExecutionStatus != store.ExecutionIntentProtected && dec.ExecutionStatus != store.ExecutionIntentReconciling && dec.ExecutionStatus != store.ExecutionIntentFailed {
				ti.transitionExecutionIntent(dec, store.ExecutionIntentFilled, "MAPPING_ALREADY_ACKNOWLEDGED", "")
			}
			logger.Warnf("🟡 [%s] 执行意图已由 mapping 修订确认，跳过重复业务提交 | intent=%d revision=%d", ti.traderID, intent.ID, intent.SourceRevision)
			continue
		}
		if intent.Status == store.ExecutionIntentReserved {
			_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentFailed, "STARTUP_REPLAY_REQUIRED", "reserved before restart; no order submitted", "", 0, 0, 0)
			continue
		}
		if !supportsLookup || intent.ClientOrderID == "" {
			_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentReconciling, "ORDER_LOOKUP_UNAVAILABLE", "startup requires manual reconciliation", "", 0, 0, 0)
			continue
		}
		order, lookupErr := provider.GetOrderStatusByClientID(intent.Symbol, intent.ClientOrderID)
		if lookupErr != nil {
			_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentReconciling, "ORDER_LOOKUP_FAILED", lookupErr.Error(), "", 0, 0, 0)
			continue
		}
		filled := getFloatField(order, "executedQty", "filled_quantity", "quantity")
		state := strings.ToUpper(getStringField(order, "status", "state"))
		orderID := getStringField(order, "orderId", "ordId", "exchange_order_id")
		if filled <= 0 && state != "FILLED" {
			if state == "CANCELED" || state == "REJECTED" || state == "FAILED" || state == "EXPIRED" {
				_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentFailed, "EXCHANGE_TERMINAL_NO_FILL", state, orderID, 0, 0, 0)
			} else {
				_ = ti.store.CopyTrade().UpdateExecutionIntent(intent.ID, store.ExecutionIntentReconciling, "EXCHANGE_ORDER_PENDING", state, orderID, 0, 0, 0)
			}
			continue
		}
		entry := getFloatField(order, "avgPrice", "fillPx", "price")
		dec := &decision.Decision{Symbol: intent.Symbol, Action: intent.Action, IsCopyTrade: true,
			LeaderPosID: intent.LeaderPosID, LeaderPosSize: intent.LeaderTargetSize, MarginMode: intent.MarginMode,
			SourceFillID: intent.SourceFillID, SourceRevision: intent.SourceRevision, ExecutionIntentID: intent.ID,
			ClientOrderID: intent.ClientOrderID, ExchangeOrderID: orderID, EntryPrice: entry,
			RequestedQuantity: intent.RequestedQuantity, QuantizedQuantity: intent.QuantizedQuantity, FilledQuantity: filled,
			Reasoning: "Copy trading: recovered execution intent after restart"}
		if entry > 0 && filled > 0 {
			dec.PositionSizeUSD = entry * filled
		} else {
			dec.PositionSizeUSD = intent.RequestedNotional
		}
		ti.updatePositionMapping(dec)
		ti.refreshStopLossAfterExecute(dec)
		if dec.ExecutionStatus != store.ExecutionIntentProtected && dec.ExecutionStatus != store.ExecutionIntentReconciling && dec.ExecutionStatus != store.ExecutionIntentFailed {
			ti.transitionExecutionIntent(dec, store.ExecutionIntentFilled, "RECOVERED_AFTER_RESTART", "")
		}
		logger.Warnf("🟡 [%s] 已恢复重启前成交的跟单意图 | intent=%d order=%s %s", ti.traderID, intent.ID, orderID, intent.Symbol)
	}
}

func validateV4ExecutorCapabilities(executor DecisionExecutor) error {
	checks := []struct {
		name string
		ok   bool
	}{
		{"protective stop management", implements[ProtectiveStopManagerV4](executor)},
		{"fresh positions", implements[FreshPositionsProvider](executor)},
		{"order status", implements[OrderStatusProvider](executor)},
		{"client order status", implements[ClientOrderStatusProvider](executor)},
		{"closed PnL", implements[ClosedPnLProvider](executor)},
	}
	for _, check := range checks {
		if !check.ok {
			return fmt.Errorf("executor does not support %s", check.name)
		}
	}
	// The interface checks above always pass for *trader.AutoTrader (its
	// protective-stop methods exist at compile time and error only at runtime
	// when the wrapped exchange lacks support). This runtime probe inspects the
	// concrete exchange so a supported data source (e.g. Binance leader) paired
	// with an execution venue that cannot place exchange-managed protective
	// stops fails loudly at start instead of degrading into a retry loop that
	// would eventually force-close positions as unprotectable.
	if checker, ok := executor.(ProtectiveStopCapabilityChecker); ok && !checker.SupportsProtectiveStops() {
		return fmt.Errorf("execution exchange does not support exchange-managed protective stops")
	}
	if validator, ok := executor.(CopyGuardCapabilityValidator); ok {
		if err := validator.ValidateCopyGuardCapabilities(); err != nil {
			return err
		}
	}
	return nil
}

func implements[T any](value interface{}) bool {
	_, ok := value.(T)
	return ok
}

func (ti *TraderIntegration) monitorV4ProtectiveStops() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ti.ctx.Done():
			return
		case <-ticker.C:
			ti.backfillV4Cycles()
			ti.pollV4ProtectiveStops()
			ti.retryDegradedV4Protections()
			ti.recoverV4PendingStates()
			ti.reconcileStoppedV4Attempts()
			ti.reconcilePendingV4Accounting()
			ti.notifyReconciledCycleSummaries()
		}
	}
}

// backfillV4CyclesEvery: cadence of the lifecycle backfill sweep. The sweep is
// cheap when nothing is missing (one mapping list + one cycle lookup each).
const backfillV4CyclesEvery = time.Minute

// backfillV4Cycles creates Copy Guard lifecycles for positions that were
// already being followed before v4 was enabled: their mappings are 'active'
// but no cycle exists, so they had no protection and never appeared in the
// lifecycle UI. Protection attachment is intentionally NOT done here — the new
// cycle starts with protection_status=PENDING, which the existing
// retryDegradedV4Protections channel picks up within one poll tick.
func (ti *TraderIntegration) backfillV4Cycles() {
	if ti.engine == nil || ti.engine.config == nil {
		return
	}
	cfg := ti.engine.config
	if !SupportsCopyGuard(cfg.ProviderType) || cfg.RiskPolicyVersion < 4 || !cfg.RiskStopLossEnabled {
		return
	}
	if time.Since(ti.lastV4Backfill) < backfillV4CyclesEvery {
		return
	}
	ti.lastV4Backfill = time.Now()
	mappings, err := ti.store.CopyTrade().ListActiveMappings(ti.traderID)
	if err != nil || len(mappings) == 0 {
		return
	}
	var positions []map[string]interface{}
	positionsLoaded := false
	for _, mapping := range mappings {
		if _, cerr := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, mapping.LeaderPosID); cerr == nil {
			continue
		}
		if !positionsLoaded {
			var posErr error
			positions, posErr = ti.getFreshPositions()
			if posErr != nil {
				return
			}
			positionsLoaded = true
		}
		entryPrice, qty := 0.0, 0.0
		for _, pos := range positions {
			if getStringField(pos, "symbol") != mapping.Symbol {
				continue
			}
			if !strings.EqualFold(getStringField(pos, "side"), mapping.Side) {
				continue
			}
			mode := getStringField(pos, "mgnMode", "marginMode")
			if mapping.MarginMode != "" && mode != "" && mode != mapping.MarginMode {
				continue
			}
			entryPrice = getFloatField(pos, "entryPrice", "entry_price")
			qty = absFloat(getFloatField(pos, "positionAmt", "quantity"))
			break
		}
		if qty <= 0 || entryPrice <= 0 {
			// Follower does not actually hold this position (e.g. manually
			// closed): nothing to protect, leave the mapping to the engine.
			continue
		}
		leaderEntry := mapping.OpenPrice
		if leaderEntry <= 0 {
			leaderEntry = entryPrice
		}
		policyJSON, _ := json.Marshal(cfg)
		cycle, cerr := ti.store.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: ti.traderID, LeaderID: cfg.LeaderID, LeaderPosID: mapping.LeaderPosID, Symbol: mapping.Symbol, Side: mapping.Side, MarginMode: mapping.MarginMode, Status: store.CopyGuardFollowing, PolicySnapshot: string(policyJSON), LeaderEntryPrice: leaderEntry, FollowerEntryPrice: entryPrice, FollowerNotional: entryPrice * qty, BaselineLeaderSize: mapping.LastKnownSize, AccountEquity: ti.getEquityFunc()(), LastObservedPrice: leaderEntry})
		if cerr != nil {
			logger.Warnf("⚠️ [%s] Copy Guard 存量仓位回填失败 | posId=%s %s: %v", ti.traderID, mapping.LeaderPosID, mapping.Symbol, cerr)
			continue
		}
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "CYCLE_BACKFILLED", Price: entryPrice, Quantity: qty, Notional: entryPrice * qty, Metadata: map[string]interface{}{"reason": "position followed before Copy Guard v4 was enabled", "leader_entry_price": leaderEntry}})
		_ = ti.store.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, entryPrice, entryPrice*qty, qty, 0)
		logger.Infof("📝 [%s] Copy Guard 存量仓位已回填生命周期 | cycle=%d posId=%s %s %s qty=%.4f entry=%.4f", ti.traderID, cycle.ID, mapping.LeaderPosID, mapping.Symbol, mapping.Side, qty, entryPrice)
	}
}

func (ti *TraderIntegration) reconcilePendingV4Accounting() {
	cycles, err := ti.store.CopyTrade().ListCopyGuardCyclesPendingAccounting(ti.traderID)
	if err != nil {
		logger.Errorf("❌ [%s] failed to list pending Copy Guard accounting: %v", ti.traderID, err)
		return
	}
	for _, cycle := range cycles {
		ti.reconcileV4CycleAccounting(cycle)
	}
}

const cycleSummaryScanEvery = time.Minute

// notifyReconciledCycleSummaries derives one non-critical summary email from
// the canonical CYCLE_CLOSED_SUMMARY event. It runs after persistence and
// reconciliation, outside every trading transaction. Delivery transitions are
// written back as events so restarts do not resend a successfully delivered
// summary and transient SMTP/queue failures remain retryable.
func (ti *TraderIntegration) notifyReconciledCycleSummaries() {
	if !ti.lastCycleSummaryScan.IsZero() && time.Since(ti.lastCycleSummaryScan) < cycleSummaryScanEvery {
		return
	}
	ti.lastCycleSummaryScan = time.Now()
	cycles, err := ti.store.CopyTrade().ListCopyGuardCyclesPendingSummaryEmail(ti.traderID, 25)
	if err != nil {
		logger.Warnf("⚠️ [%s] Copy Guard 周期汇总邮件扫描失败: %v", ti.traderID, err)
		return
	}
	for _, cycle := range cycles {
		ti.notifyCycleClosedSummary(cycle)
	}
}

func (ti *TraderIntegration) notifyCycleClosedSummary(cycle *store.CopyGuardCycle) {
	if cycle == nil || cycle.AccountingStatus != store.CopyGuardAccountingReconciled || cycle.ClosedAt == nil {
		return
	}
	var policy store.CopyGuardPolicy
	_ = json.Unmarshal([]byte(cycle.PolicySnapshot), &policy)
	attemptNo := cycle.ReentryCount
	generation := 0
	key := fmt.Sprintf("CYCLE_CLOSED_SUMMARY|%s|%d|%d|%d", ti.traderID, cycle.ID, attemptNo, generation)
	aiSummary, aiEvaluationErr := copyguardmetrics.EvaluateCycleAIDecisions(ti.store, cycle.ID)
	if aiEvaluationErr != nil {
		logger.Warnf("⚠️ [%s] Copy Guard 周期汇总 AI 后验评价失败 cycle=%d: %v", ti.traderID, cycle.ID, aiEvaluationErr)
	}
	if policy.NotificationLevel == "critical" {
		_ = ti.saveCycleSummaryEmailStatus(cycle, notifier.DeliveryDisabled, key, "notification level is critical")
		return
	}
	if aiEvaluationErr != nil {
		return // keep the summary pending; the next scan retries idempotently
	}
	attempts, err := ti.store.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil {
		logger.Warnf("⚠️ [%s] Copy Guard 周期汇总读取 attempt 失败 cycle=%d: %v", ti.traderID, cycle.ID, err)
		return
	}
	stopOnly, first, second, reentry := 0.0, 0.0, 0.0, 0.0
	for _, attempt := range attempts {
		switch attempt.AttemptNo {
		case 0:
			stopOnly = attempt.PnL
		case 1:
			first = attempt.PnL
			reentry += attempt.PnL
		case 2:
			second = attempt.PnL
			reentry += attempt.PnL
		default:
			reentry += attempt.PnL
		}
	}
	traderName := ti.traderDisplayName()
	aiEffect := "本周期没有生产 AI 候选决策"
	if aiSummary != nil && aiSummary.TotalDecisions > 0 {
		aiEffect = fmt.Sprintf("AI 决策: %d 次（可评价 %d / 数据不足 %d）\n最终决策: %s\n最终效果: %s\n错过反转: %d\n正确放弃: %d\n风控避免坏入场: %d\n实际重入盈亏: %+.4f USDT\n评价版本: v%d", aiSummary.TotalDecisions, aiSummary.ScorableDecisions, aiSummary.UnscorableDecisions, aiSummary.FinalDecision, aiSummary.FinalDecisionOutcome, aiSummary.MissedReversals, aiSummary.CorrectAbandons, aiSummary.RiskGateSavedLosses, aiSummary.ActualReentryPnL, aiSummary.EvaluationVersion)
	}
	notifier.Notify(notifier.Alert{
		Category: "copy_trade", TraderID: ti.traderID, TraderName: traderName,
		Title:   fmt.Sprintf("%s | Copy Guard 周期结束汇总 %s %s", traderName, cycle.Symbol, cycle.Side),
		Body:    fmt.Sprintf("Trader Name: %s\nTrader ID: %s\nCycle: %d\nLeader Position: %s\nSymbol / Side: %s / %s\n\n实际 Copy Guard: %+.4f USDT\n无 Copy Guard 基线: %+.4f USDT\n只止损: %+.4f USDT\n第一次重入贡献: %+.4f USDT\n第二次重入贡献: %+.4f USDT\n全部重入贡献: %+.4f USDT\n净保护效果: %+.4f USDT\n手续费: %.4f USDT\n滑点: %.4f USDT\n对账状态: %s\n\n--- AI 决策效果 ---\n%s", traderName, ti.traderID, cycle.ID, cycle.LeaderPosID, cycle.Symbol, cycle.Side, cycle.ActualPnL, cycle.BaselinePnL, stopOnly, first, second, reentry, cycle.NetGuardEffect, cycle.Fees, cycle.Slippage, cycle.AccountingStatus, aiEffect),
		RateKey: key, DedupKey: key,
		Fields: map[string]string{"TraderName": traderName, "CycleID": fmt.Sprint(cycle.ID), "AttemptNo": fmt.Sprint(attemptNo), "Generation": fmt.Sprint(generation), "AccountingStatus": cycle.AccountingStatus},
		StatusHook: func(status notifier.DeliveryStatus, deliveryErr error) {
			if err := ti.saveCycleSummaryEmailStatus(cycle, status, key, errorString(deliveryErr)); err != nil {
				logger.Warnf("⚠️ [%s] Copy Guard 邮件状态落库失败 cycle=%d status=%s: %v", ti.traderID, cycle.ID, status, err)
			}
		},
	})
}

func (ti *TraderIntegration) saveCycleSummaryEmailStatus(cycle *store.CopyGuardCycle, status notifier.DeliveryStatus, key, detail string) error {
	eventType := "CYCLE_SUMMARY_EMAIL_" + strings.ToUpper(string(status))
	return ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: eventType, Metadata: map[string]interface{}{"attempt_no": cycle.ReentryCount, "decision_generation": 0, "dedup_key": key, "delivery_status": status, "detail": detail}})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// accountingDelayedAfter: a closed cycle still unreconciled after this long is
// flagged DELAYED (OKX data late; automatic retry continues).
// accountingUnrecoverableAfter: after this long the cycle is parked as
// UNRECOVERABLE and leaves the automatic retry queue.
const (
	accountingDelayedAfter       = 10 * time.Minute
	accountingUnrecoverableAfter = 24 * time.Hour
)

func (ti *TraderIntegration) reconcileV4CycleAccounting(cycle *store.CopyGuardCycle) {
	if cycle == nil || cycle.AccountingStatus == store.CopyGuardAccountingReconciled {
		return
	}
	markFailure := func(message string) {
		if cycle.ClosedAt == nil {
			return
		}
		if time.Since(*cycle.ClosedAt) >= accountingUnrecoverableAfter {
			if err := ti.store.CopyTrade().MarkCopyGuardAccountingUnrecoverable(cycle.ID, message); err != nil {
				logger.Errorf("❌ [%s] failed to mark accounting unrecoverable cycle=%d: %v", ti.traderID, cycle.ID, err)
				return
			}
			_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "ACCOUNTING_UNRECOVERABLE", Metadata: map[string]interface{}{"error": message}})
			ti.notifyProtection(cycle, "Copy Guard 对账数据不可自动恢复", fmt.Sprintf("平仓后超过24小时仍无法从交易所确认最终盈亏，自动对账已停止。\n请查看日志并核对该周期。\n错误: %s", message), "accounting_unrecoverable")
			return
		}
		if time.Since(*cycle.ClosedAt) < accountingDelayedAfter {
			return
		}
		wasPending := cycle.AccountingStatus == store.CopyGuardAccountingPending
		if err := ti.store.CopyTrade().MarkCopyGuardAccountingDelayed(cycle.ID, message); err != nil {
			logger.Errorf("❌ [%s] failed to mark accounting delayed cycle=%d: %v", ti.traderID, cycle.ID, err)
			return
		}
		if wasPending {
			// Delayed is an automatic-retry state: record the event but do not
			// email the user (the old "needs manual review" mail was noise).
			_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "ACCOUNTING_DELAYED", Metadata: map[string]interface{}{"error": message}})
		}
	}
	followerPosID, exitOrderID := cycle.FollowerPosID, cycle.ExitOrderID
	attemptQty := 0.0
	// notBefore 必须用最后一个 attempt 的开仓时间，不能用 cycle.OpenedAt：
	// OKX 净持仓模式下 posId 跨 attempt 复用，用周期起点做下限会在新平仓记录
	// 尚未生成时误匹配上一个 attempt 的止损平仓记录（实盘 cycle 15：-1.82 被
	// 计了两次）。refTime 保持 nil（取最新记录）：跟随者平仓单延迟成交时记录
	// ExitTime 可能远晚于 ClosedAt，就近窗口会让对账永远失败。
	notBefore := cycle.OpenedAt.Add(-time.Minute)
	if attempts, err := ti.store.CopyTrade().ListCopyGuardAttempts(cycle.ID); err == nil {
		for _, attempt := range attempts {
			if attempt.AttemptNo == cycle.ReentryCount {
				if attempt.FollowerPosID != "" {
					followerPosID = attempt.FollowerPosID
				}
				if attempt.ExitOrderID != "" {
					exitOrderID = attempt.ExitOrderID
				}
				attemptQty = attempt.Quantity
				if !attempt.OpenedAt.IsZero() {
					notBefore = attempt.OpenedAt.Add(-time.Minute)
				}
				break
			}
		}
	}
	if followerPosID == "" && exitOrderID == "" {
		markFailure("missing both follower position id and exit order id")
		return
	}
	exitPrice := 0.0
	if exitOrderID != "" {
		orderProvider, ok := ti.executor.(OrderStatusProvider)
		if !ok {
			markFailure("executor does not support order status")
			return
		}
		order, err := orderProvider.GetOrderStatus(cycle.Symbol, exitOrderID)
		if err != nil {
			markFailure(err.Error())
			return
		}
		if !strings.EqualFold(getStringField(order, "status"), "FILLED") {
			markFailure(fmt.Sprintf("exit order state is %s", getStringField(order, "status")))
			return
		}
		exitPrice = getFloatField(order, "avgPrice")
	}
	matched, err := ti.lookupClosedPnLRecord(cycle.Symbol, cycle.Side, cycle.MarginMode, followerPosID, notBefore, nil, attemptQty)
	if err != nil {
		markFailure(err.Error())
		return
	}
	if matched == nil {
		markFailure("matching OKX position history is not available yet")
		return
	}
	if matched.ExitPrice > 0 {
		exitPrice = matched.ExitPrice
	}
	if err := ti.store.CopyTrade().CompleteCopyGuardAccounting(cycle.ID, cycle.ReentryCount, exitPrice, matched.RealizedPnL, matched.Fee, matched.FundingFee, matched.LiquidationPenalty); err != nil {
		markFailure(err.Error())
		return
	}
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "ACCOUNTING_RECONCILED", Price: exitPrice, Quantity: matched.Quantity, PnL: matched.RealizedPnL, Fee: matched.Fee, Metadata: map[string]interface{}{"follower_pos_id": followerPosID, "exit_order_id": exitOrderID, "funding_fee": matched.FundingFee, "liquidation_penalty": matched.LiquidationPenalty, "match_source": matchSource(followerPosID)}})
}

func matchSource(posID string) string {
	if posID != "" {
		return "pos_id"
	}
	return "fallback_window"
}

// lookupClosedPnLRecord finds the follower's own closed-position record.
// Priority: precise posId query (OKX only) → time-window scan. Without a
// posId the fallback additionally requires margin mode and an approximate
// quantity so a concurrent same-symbol position cannot be mis-attributed.
// refTime selects the record closest to it (within 15 minutes); when nil the
// latest record wins.
func (ti *TraderIntegration) lookupClosedPnLRecord(symbol, side, marginMode, posID string, notBefore time.Time, refTime *time.Time, quantityHint float64) (*trader.ClosedPnLRecord, error) {
	var records []trader.ClosedPnLRecord
	var err error
	if posID != "" {
		if p, ok := ti.executor.(ClosedPnLByPositionProvider); ok {
			records, err = p.GetClosedPnLByPositionID(symbol, posID, 20)
			if err != nil {
				return nil, err
			}
		}
	}
	if records == nil {
		if p, ok := ti.executor.(ClosedPnLBySymbolProvider); ok {
			records, err = p.GetClosedPnLBySymbol(symbol, notBefore, 100)
			if err != nil {
				return nil, err
			}
		}
	}
	if records == nil {
		provider, ok := ti.executor.(ClosedPnLProvider)
		if !ok {
			return nil, fmt.Errorf("executor does not support closed PnL")
		}
		records, err = provider.GetClosedPnL(notBefore, 100)
		if err != nil {
			return nil, err
		}
	}
	var best *trader.ClosedPnLRecord
	bestDistance := 15 * time.Minute
	for i := range records {
		r := &records[i]
		if r.Symbol != symbol || !strings.EqualFold(r.Side, side) || r.ExitTime.Before(notBefore) {
			continue
		}
		if posID != "" && r.ExchangeID != "" && r.ExchangeID != posID {
			continue
		}
		if posID == "" {
			if marginMode != "" && r.MarginMode != "" && r.MarginMode != marginMode {
				continue
			}
			// quantityHint 是币数量；OKX 的 r.Quantity 是合约张数，必须用换算
			// 后的 QuantityCoins 对比，否则会差 ctVal 倍导致误过滤/误匹配。
			recordQty := r.QuantityCoins
			if recordQty <= 0 {
				recordQty = r.Quantity
			}
			if quantityHint > 0 && recordQty > 0 && (recordQty < quantityHint*0.8 || recordQty > quantityHint*1.2) {
				continue
			}
		}
		if refTime != nil {
			distance := r.ExitTime.Sub(*refTime)
			if distance < 0 {
				distance = -distance
			}
			if distance <= bestDistance {
				bestDistance, best = distance, r
			}
		} else if best == nil || r.ExitTime.After(best.ExitTime) {
			best = r
		}
	}
	return best, nil
}

func (ti *TraderIntegration) reconcileStoppedV4Attempts() {
	if _, ok := ti.executor.(ClosedPnLProvider); !ok {
		return
	}
	cycles, err := ti.store.CopyTrade().ListCopyGuardCyclesWithUnreconciledStops(ti.traderID)
	if err != nil {
		return
	}
	for _, cycle := range cycles {
		attempts, attemptErr := ti.store.CopyTrade().ListCopyGuardAttempts(cycle.ID)
		if attemptErr != nil {
			continue
		}
		for _, attempt := range attempts {
			if attempt.Status != "STOPPED" || attempt.Reconciled || attempt.ClosedAt == nil {
				continue
			}
			if last := ti.lastAttemptReconcile[attempt.ID]; !last.IsZero() && time.Since(last) < time.Minute {
				continue
			}
			followerPosID := attempt.FollowerPosID
			if followerPosID == "" {
				followerPosID = cycle.FollowerPosID
			}
			ti.lastAttemptReconcile[attempt.ID] = time.Now()
			// posId 缺失时按 symbol+side+marginMode+数量容差+时间窗兜底匹配，
			// 不再直接放弃（旧逻辑导致重入 attempt 永远无法对账）。
			best, historyErr := ti.lookupClosedPnLRecord(cycle.Symbol, cycle.Side, cycle.MarginMode, followerPosID, attempt.OpenedAt.Add(-time.Minute), attempt.ClosedAt, attempt.Quantity)
			if historyErr != nil || best == nil {
				continue
			}
			if reconcileErr := ti.store.CopyTrade().ReconcileCopyGuardAttempt(cycle.ID, attempt.AttemptNo, best.RealizedPnL, best.Fee, best.FundingFee, best.LiquidationPenalty); reconcileErr == nil {
				_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "ATTEMPT_RECONCILED", Price: best.ExitPrice, Quantity: best.Quantity, PnL: best.RealizedPnL, Fee: best.Fee, Metadata: map[string]interface{}{"attempt": attempt.AttemptNo, "follower_pos_id": followerPosID, "funding_fee": best.FundingFee, "liquidation_penalty": best.LiquidationPenalty}})
				if cycle.ClosedAt != nil {
					_ = ti.store.CopyTrade().FinalizeCopyGuardAccountingFromAttempts(cycle.ID)
				}
			}
		}
	}
}

func (ti *TraderIntegration) pollV4ProtectiveStops() {
	mgr, ok := ti.executor.(ProtectiveStopManagerV4)
	if !ok {
		return
	}
	orders, err := ti.store.CopyTrade().ListActiveCopyGuardProtectiveOrders(ti.traderID)
	if err != nil {
		return
	}
	for _, stored := range orders {
		cycle, cycleErr := ti.store.CopyTrade().GetCopyGuardCycle(stored.CycleID)
		if cycleErr != nil {
			continue
		}
		if cycle.ClosedAt != nil {
			// Orphan: the lifecycle finished but its protective order is still
			// tracked as live. Cancel it on OKX so it cannot fire against a
			// future position; a closed cycle must never raise alerts again.
			if cancelErr := ti.cancelProtectiveOrderForCycle(mgr, cycle, stored); cancelErr != nil {
				logger.Warnf("⚠️ [%s] Copy Guard 孤儿保护单撤销失败(下轮重试) | cycle=%d algoId=%s err=%v", ti.traderID, cycle.ID, stored.AlgoID, cancelErr)
			}
			continue
		}
		if stored.ReplacementPending && stored.PreviousAlgoID != "" {
			if !ti.retryRetiringProtectiveStop(mgr, cycle, stored) {
				continue
			}
		}
		live, err := ti.resolveProtectiveOrder(mgr, stored.AlgoID, stored.AlgoClientID, stored.Symbol)
		if err != nil {
			// Transient query failure: state unknown. Tolerate a few rounds
			// before alarming so one API hiccup does not flag the cycle.
			ti.protectiveQueryFailures[stored.CycleID]++
			if ti.protectiveQueryFailures[stored.CycleID] >= protectiveQueryFailureThreshold {
				if cycle.ProtectionStatus != store.CopyGuardProtectionUnknown {
					ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN", err, cycle.ProtectionCoverage, false)
				}
			}
			continue
		}
		delete(ti.protectiveQueryFailures, stored.CycleID)
		if live == nil {
			// v5 误判修复：保护单在 OKX 上已不存在且本地仓位同时为空 →
			// 大概率是保护单已触发成交、algo 记录被 OKX 清理（或仓位已被
			// 其他路径平掉）。此时绝不能标 DEGRADED——那会让重试链在没有
			// 仓位的情况下重建保护单。止损确认交给 checkStoppedByRisk 的
			// position-absent 路径。
			_ = ti.store.CopyTrade().UpdateCopyGuardProtectiveOrderStatus(stored.CycleID, "invalid")
			if ti.isFollowerPositionFlat(cycle.Symbol, cycle.Side, cycle.MarginMode) {
				logger.Infof("🛑 [%s] Copy Guard 保护单已消失且仓位为空（疑似已触发） | cycle=%d algoId=%s，交由止损确认路径处理", ti.traderID, cycle.ID, stored.AlgoID)
				_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "PROTECTIVE_STOP_GONE", Metadata: map[string]interface{}{"algo_id": stored.AlgoID, "follower_flat": true}})
				continue
			}
			// 仓位仍在但保护单没了：真降级，交给重试链重建。
			ti.markProtectionIssue(cycle, store.CopyGuardProtectionDegraded, "PROTECTION_DEGRADED", fmt.Errorf("protective stop %s no longer exists on OKX", stored.AlgoID), 0, false)
			continue
		}
		state := strings.ToLower(live.State)
		// Coverage must be judged against the real follower position, not the
		// locally stored quantity: after a failed amend the stored number can
		// match the exchange order while the position has grown.
		// 每 3s 例行覆盖率基准：缓存读即可（fresh=false），避免击穿持仓缓存
		baseQty := stored.Quantity
		if posQty, ok := ti.followerPositionQuantity(cycle.Symbol, cycle.Side, cycle.MarginMode, false); ok && posQty > 0 {
			baseQty = posQty
		}
		coverage := protectionCoverage(live.Quantity, baseQty)
		if state == "live" {
			if !protectionQuantityMatches(live.Quantity, baseQty, stored.QuantityStep) {
				ti.markProtectionIssue(cycle, store.CopyGuardProtectionDegraded, "PROTECTION_COVERAGE_LOW", fmt.Errorf("protective quantity %.8f does not match position quantity %.8f", live.Quantity, baseQty), coverage, false)
			} else if cycle.ProtectionStatus != store.CopyGuardProtectionVerified && cycle.ProtectionStatus != store.CopyGuardProtectionClamped {
				// CLAMPED 也是"已保护"的健康态（保护质量降级但单子有效），
				// 不能被 poll 覆写成 VERIFIED，否则 clamp 提示丢失且反复
				// 触发 PROTECTION_RECOVERED 邮件。
				_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionVerified, coverage, "", cycle.FollowerPosID, cycle.EntryOrderID, false)
				_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "PROTECTION_RECOVERED", Price: live.TriggerPrice, Quantity: live.Quantity, Metadata: map[string]interface{}{"algo_id": live.AlgoID, "coverage": coverage}})
				ti.notifyProtection(cycle, "Copy Guard 保护已恢复", "保护单已经重新验证有效。", "recovered")
			}
			// If the exchange protection succeeded but the one-shot candidate
			// completion write failed, retry that audit commit from the normal
			// protection poll. Querying ENTRY_PENDING first avoids repeatedly
			// emitting anomalies for already-completed/missing candidates.
			if protectionQuantityMatches(live.Quantity, baseQty, stored.QuantityStep) && ti.engine != nil && ti.engine.config != nil && ti.engine.config.RiskReentryDecisionMode == "ai_guarded" && cycle.Status == store.CopyGuardFollowingReentry {
				if candidate, candidateErr := ti.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID); candidateErr == nil && candidate.Status == store.ReentryCandidateEntryPending {
					action := "open_long"
					if cycle.Side == "short" {
						action = "open_short"
					}
					ti.enforceAIReentryProtection(&decision.Decision{LeaderPosID: cycle.LeaderPosID, Symbol: cycle.Symbol, Action: action, MarginMode: cycle.MarginMode, EntryPrice: cycle.FollowerEntryPrice, PositionSizeUSD: cycle.FollowerNotional})
				}
			}
			continue
		}
		if state == "canceled" || state == "order_failed" {
			_ = ti.store.CopyTrade().UpdateCopyGuardProtectiveOrderStatus(cycle.ID, state)
			ti.markProtectionIssue(cycle, store.CopyGuardProtectionDegraded, "PROTECTION_DEGRADED", fmt.Errorf("protective stop state=%s", state), coverage, false)
			continue
		}
		if state != "effective" && state != "triggered" && state != "filled" {
			continue
		}
		// Crash window: the stop ledger may have committed while the local
		// protective-order row was still "live". Detect the already-closed
		// attempt before changing the cycle back to STOP_PENDING_FLAT.
		alreadyRecorded := false
		if attempts, attemptsErr := ti.store.CopyTrade().ListCopyGuardAttempts(cycle.ID); attemptsErr == nil {
			for _, attempt := range attempts {
				if attempt.AttemptNo == cycle.ReentryCount && attempt.Status == "STOPPED" && (attempt.StopAlgoID == "" || attempt.StopAlgoID == stored.AlgoID) {
					alreadyRecorded = true
					break
				}
			}
		}
		if alreadyRecorded {
			_ = ti.store.CopyTrade().UpdateCopyGuardProtectiveOrderStatus(cycle.ID, state)
			_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionTriggered, 0, "", cycle.FollowerPosID, cycle.EntryOrderID, false)
			continue
		}
		// 保护单触发不等于仓位已归零。先用 fresh 仓位确认，残仓可交易时
		// reduce-only close-all；只有交易所确认 flat 后才允许进入重入观察。
		if cycle.Status != store.CopyGuardStopPendingFlat && cycle.Status != store.CopyGuardStopPartial {
			_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "STOP_PENDING_FLAT", Price: stored.TriggerPrice, Quantity: stored.Quantity, Metadata: map[string]interface{}{"algo_id": stored.AlgoID, "attempt_no": cycle.ReentryCount, "state": state}})
		}
		_ = ti.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStopPendingFlat, cycle.LeaderEntryPrice, stored.TriggerPrice, 0)
		remain, exists := ti.followerPositionQuantity(cycle.Symbol, cycle.Side, cycle.MarginMode, true)
		if !exists {
			ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN", fmt.Errorf("cannot verify follower is flat after protective stop fired"), cycle.ProtectionCoverage, false)
			continue
		}
		if remain > 0 {
			_ = ti.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStopPartial, cycle.LeaderEntryPrice, stored.TriggerPrice, 0)
			notional := remain * stored.TriggerPrice
			eventType := "STOP_PARTIAL"
			if notional > 0 && notional < MinOrderValue {
				eventType = "STOP_DUST_RESIDUAL"
			}
			if cycle.Status != store.CopyGuardStopPartial {
				_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: eventType, Price: stored.TriggerPrice, Quantity: remain, Notional: notional, Metadata: map[string]interface{}{"algo_id": stored.AlgoID, "attempt_no": cycle.ReentryCount, "remaining_quantity": remain, "state": state}})
				ti.notifyProtection(cycle, "Copy Guard 止损后仍有残仓", fmt.Sprintf("保护止损未完全平仓，剩余数量 %.8f（约 %.2f USDT）。系统将有限次执行 reduce-only 市价退出；归零前禁止重入。", remain, notional), fmt.Sprintf("stop_residual_attempt_%d", cycle.ReentryCount))
			}
			if closer, ok := ti.executor.(EmergencyPositionCloser); ok && ti.residualCloseTries[cycle.ID] < 3 && (ti.residualCloseLast[cycle.ID].IsZero() || time.Since(ti.residualCloseLast[cycle.ID]) >= 10*time.Second) {
				ti.residualCloseLast[cycle.ID] = time.Now()
				ti.residualCloseTries[cycle.ID]++
				if orderID, closeErr := closer.ClosePositionMarket(cycle.Symbol, cycle.Side); closeErr != nil {
					ti.notifyProtection(cycle, "Copy Guard 止损残仓未平", "保护止损部分成交后仍有残仓，自动 reduce-only 平仓失败: "+closeErr.Error(), "stop_residual_failed")
					_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "GUARD_FORCED_EXIT_FAILED", Price: stored.TriggerPrice, Quantity: remain, Metadata: map[string]interface{}{"error": closeErr.Error(), "reason": "partial_stop_residual"}})
				} else {
					_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "FORCED_EXIT", Price: stored.TriggerPrice, Quantity: remain, Metadata: map[string]interface{}{"order_id": orderID, "reason": "partial_stop_residual"}})
				}
			} else if ti.residualCloseTries[cycle.ID] >= 3 && time.Since(ti.residualCloseLast[cycle.ID]) >= unprotectableRecheckDelay {
				// 3 次 reduce-only 强平耗尽仍有残仓：STOP_PARTIAL 不能成为无逃生
				// 终态（残仓裸奔且禁止重入）。升级到 GUARD_UNPROTECTABLE 处置
				// （复用 close/follow 处置模式与升级告警），按不可保护复检节奏重试。
				ti.residualCloseLast[cycle.ID] = time.Now()
				ti.handleUnprotectableCycle(cycle, fmt.Errorf("stop residual close retries exhausted; remaining %.8f %s still open", remain, cycle.Symbol))
			}
			continue
		}
		delete(ti.residualCloseLast, cycle.ID)
		delete(ti.residualCloseTries, cycle.ID)
		mapping, _ := ti.store.CopyTrade().GetMapping(ti.traderID, cycle.LeaderPosID)
		pnl, size := float64(0), float64(0)
		leader := ti.engine.buildLeaderPosMap()[cycle.LeaderPosID]
		if leader != nil {
			pnl, size = leader.UnrealizedPnL, leader.Size
		}
		if mapping != nil {
			_ = ti.store.CopyTrade().MarkStoppedByRisk(ti.traderID, cycle.LeaderPosID, pnl, size, mapping.AddCount)
		}
		atr, _ := market.GetOKXATRWithMaxAge(cycle.Symbol, ti.engine.config.RiskATRTimeframe, ti.engine.config.RiskATRPeriod, riskATRCacheMaxAge(ti.engine.config))
		fillPrice, fee := stored.TriggerPrice, float64(0)
		if live.ActualOrderID != "" {
			if p, ok := ti.executor.(OrderStatusProvider); ok {
				if status, e := p.GetOrderStatus(stored.Symbol, live.ActualOrderID); e == nil {
					fillPrice = getFloatField(status, "avgPrice")
					fee = getFloatField(status, "commission")
					if fillPrice <= 0 {
						fillPrice = stored.TriggerPrice
					}
				}
			}
		}
		move := float64(0)
		if cycle.FollowerEntryPrice > 0 {
			move = (fillPrice - cycle.FollowerEntryPrice) / cycle.FollowerEntryPrice
			if cycle.Side == "short" {
				move = -move
			}
		}
		slippage := cycle.FollowerNotional * math.Abs(fillPrice-stored.TriggerPrice) / stored.TriggerPrice
		stopPnL := cycle.FollowerNotional*move - fee
		if recordErr := ti.store.CopyTrade().RecordCopyGuardStop(cycle, atr, fillPrice, stopPnL, fee, slippage, stored.AlgoID, map[string]interface{}{"algo_id": stored.AlgoID, "state": state, "actual_order_id": live.ActualOrderID, "trigger_price": stored.TriggerPrice, "slippage": slippage, "quantity": stored.Quantity}); recordErr != nil {
			logger.Errorf("[CopyGuard] trader=%s cycle=%d attempt=%d event=STOP_PERSIST_FAILED reason=%v", ti.traderID, cycle.ID, cycle.ReentryCount, recordErr)
			ti.notifyProtection(cycle, "Copy Guard 止损状态落库失败", "交易所已触发止损，但本地状态落库失败；系统保留保护单记录并继续重试，归零确认前禁止重入。错误: "+recordErr.Error(), fmt.Sprintf("stop_persist_failed_%d", cycle.ReentryCount))
			continue
		}
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "STOP_FLAT_CONFIRMED", Price: fillPrice, Metadata: map[string]interface{}{"algo_id": stored.AlgoID, "attempt_no": cycle.ReentryCount, "state": state}})
		// 快照止损时的领航员均价，供重入保守锚点使用（领航员数据不可得时跳过，
		// checkReentryConditions 会退回实时均价）
		if leader != nil && leader.EntryPrice > 0 {
			_ = ti.store.CopyTrade().SnapshotCopyGuardLeaderEntryAtStop(cycle.ID, leader.EntryPrice)
		}
		_ = ti.store.CopyTrade().UpdateCopyGuardProtectiveOrderStatus(cycle.ID, state)
		_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionTriggered, 0, "", cycle.FollowerPosID, cycle.EntryOrderID, false)
	}
}

func protectionCoverage(protected, position float64) float64 {
	r := protectionRatio(protected, position)
	if r > 1 {
		return 1
	}
	if r < 0 {
		return 0
	}
	return r
}

func protectionRatio(protected, position float64) float64 {
	if position <= 0 {
		return 0
	}
	return protected / position
}

// protectionQuantityMatches accepts the exchange rounding difference of one
// base-quantity lot. New v7 orders persist lotSz*ctVal as quantityStep so this
// remains deterministic after restart. Old rows have step=0 and retain the
// legacy 0.1% tolerance until their protection is next rebuilt.
func protectionQuantityMatches(protected, position, quantityStep float64) bool {
	if protected <= 0 || position <= 0 {
		return false
	}
	tolerance := quantityStep
	if tolerance <= 0 {
		tolerance = math.Max(position*0.001, 1e-8)
	}
	epsilon := math.Max(math.Max(math.Abs(protected), math.Abs(position))*1e-12, 1e-12)
	return math.Abs(protected-position) <= tolerance+epsilon
}

func protectionRetryDelay(retries int) time.Duration {
	delays := []time.Duration{0, time.Second, 3 * time.Second, 10 * time.Second, 30 * time.Second}
	if retries < len(delays) {
		return delays[retries]
	}
	return time.Minute
}

// protectiveQueryFailureThreshold: consecutive protective-stop lookup failures
// (3s poll → ~9s) tolerated before the cycle is flagged UNKNOWN.
const protectiveQueryFailureThreshold = 3

// protectionMissingEscalationAfter: when a position has lacked full protection
// for this long, send one escalated alert per cycle (automatic retries keep
// running; this is the safety valve replacing silent indefinite retrying).
const protectionMissingEscalationAfter = 10 * time.Minute

// protectionRetryMaxAttempts (v5): 重试退避封顶。PENDING/DEGRADED（确认无有效
// 保护）连续重试到此次数后不再无限重挂，转入 GUARD_UNPROTECTABLE 处置
// （按 risk_unprotectable_action 平仓或标红裸跑）。周期 65/66 曾在保护单
// 反复被拒时空转 110 次重试、仓位全程裸跑，这里是对应的结构性修复。
// UNKNOWN（状态未知，可能已有保护）不受封顶影响——状态未知时强制平仓
// 比裸跑更危险。
const protectionRetryMaxAttempts = 10

// unprotectableRecheckDelay: follow 模式下 UNPROTECTABLE 周期的复检间隔。
// 行情/保证金变化可能让仓位重新可保护，低频复检以便自动恢复。
const unprotectableRecheckDelay = 5 * time.Minute

func (ti *TraderIntegration) retryDegradedV4Protections() {
	if ti.engine == nil || ti.engine.config == nil || !ti.engine.config.RiskStopLossEnabled {
		return
	}
	cycles, err := ti.store.CopyTrade().ListOpenCopyGuardCycles(ti.traderID)
	if err != nil {
		return
	}
	for _, cycle := range cycles {
		if cycle.Status != store.CopyGuardFollowing && cycle.Status != store.CopyGuardFollowingReentry {
			continue
		}
		switch cycle.ProtectionStatus {
		case store.CopyGuardProtectionPending, store.CopyGuardProtectionUnknown, store.CopyGuardProtectionDegraded, store.CopyGuardProtectionUnprotectable:
		default:
			continue
		}
		if cycle.ProtectionMissingAt != nil && time.Since(*cycle.ProtectionMissingAt) >= protectionMissingEscalationAfter {
			ti.notifyProtection(cycle, "Copy Guard 保护缺失超时", fmt.Sprintf("该仓位已连续 %.0f 分钟没有完整有效的止损保护，系统仍在自动重试。\n建议人工检查执行交易所，必要时手动挂止损或平仓。\n当前状态: %s\n最近错误: %s", time.Since(*cycle.ProtectionMissingAt).Minutes(), cycle.ProtectionStatus, cycle.ProtectionError), "missing_escalation")
		}
		retryDelay := protectionRetryDelay(cycle.ProtectionRetries)
		if cycle.ProtectionStatus == store.CopyGuardProtectionUnprotectable {
			retryDelay = unprotectableRecheckDelay
		}
		if cycle.ProtectionLastRetryAt != nil && time.Since(*cycle.ProtectionLastRetryAt) < retryDelay {
			continue
		}
		claimed, claimErr := ti.store.CopyTrade().BeginCopyGuardProtectionRetry(cycle, retryDelay)
		if claimErr != nil {
			logger.Errorf("❌ [%s] failed to persist Copy Guard protection retry: %v", ti.traderID, claimErr)
			continue
		}
		if !claimed {
			continue
		}
		// v5 重试退避封顶：确认无有效保护且重试用尽 → GUARD_UNPROTECTABLE 处置
		if cycle.ProtectionRetries >= protectionRetryMaxAttempts &&
			(cycle.ProtectionStatus == store.CopyGuardProtectionPending || cycle.ProtectionStatus == store.CopyGuardProtectionDegraded) {
			ti.handleUnprotectableCycle(cycle, fmt.Errorf("protection retries exhausted (%d attempts): %s", cycle.ProtectionRetries, cycle.ProtectionError))
			continue
		}
		action := "open_long"
		if cycle.Side == "short" {
			action = "open_short"
		}
		dec := &decision.Decision{Symbol: cycle.Symbol, Action: action, LeaderPosID: cycle.LeaderPosID, MarginMode: cycle.MarginMode, EntryPrice: cycle.LeaderEntryPrice, Reasoning: "Copy Guard protection retry"}
		ti.refreshStopLossAfterExecute(dec)
	}
}

// handleUnprotectableForDecision 决策路径进入 GUARD_UNPROTECTABLE 处置
// （calcStopLossPrice 判定 clamp 后仍不可保护 / 开仓即触发）。
func (ti *TraderIntegration) handleUnprotectableForDecision(dec *decision.Decision, side string, quantity, entryPrice float64, cause error) {
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil {
		logger.Errorf("❌ [%s] Copy Guard 不可保护但生命周期缺失 | %s %s: %v (cause=%v)", ti.traderID, dec.Symbol, side, err, cause)
		return
	}
	ti.handleUnprotectableCycle(cycle, cause)
	message := "protection cannot be established"
	if cause != nil {
		message = cause.Error()
	}
	if ti.engine != nil && ti.engine.config != nil && ti.engine.config.RiskUnprotectableAction == "follow" {
		ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "GUARD_UNPROTECTABLE_FOLLOWING", message)
		return
	}
	quantity, known := ti.followerPositionQuantity(dec.Symbol, side, dec.MarginMode, true)
	if known && quantity <= 0 {
		ti.transitionExecutionIntent(dec, store.ExecutionIntentFailed, "GUARD_UNPROTECTABLE_EXITED", message)
		return
	}
	ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "GUARD_FORCED_EXIT_PENDING", message)
}

// handleUnprotectableCycle GUARD_UNPROTECTABLE 处置（v5 可保护性状态机终点）：
//   - close（默认）：立即市价平掉跟单仓位，周期进入 STOPPED_WATCHING 观察，
//     记 GUARD_FORCED_EXIT；重入判据后续照常评估（含可保护性预检）。
//   - follow：不平仓（跟随领航员硬扛），保护状态标 UNPROTECTABLE（UI 标红），
//     升级告警一次，每 unprotectableRecheckDelay 复检一次是否恢复可保护。
func (ti *TraderIntegration) handleUnprotectableCycle(cycle *store.CopyGuardCycle, cause error) {
	message := "protection cannot be established"
	if cause != nil {
		message = cause.Error()
	}
	action := "close"
	if ti.engine != nil && ti.engine.config != nil && ti.engine.config.RiskUnprotectableAction == "follow" {
		action = "follow"
	}
	// follow 模式下每 unprotectableRecheckDelay 复检一次会重复进入本函数；
	// 已处于 UNPROTECTABLE 的周期复检未恢复属于常态，不再重复写事件/告警
	// （否则一天 288 条 GUARD_UNPROTECTABLE 污染时间线），只在状态迁入沿记录。
	alreadyUnprotectable := cycle.ProtectionStatus == store.CopyGuardProtectionUnprotectable
	if action == "follow" && alreadyUnprotectable {
		_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionUnprotectable, 0, message, cycle.FollowerPosID, cycle.EntryOrderID, false)
		logger.Debugf("🔁 [%s] Copy Guard 不可保护复检未恢复 | cycle=%d %s %s | %s", ti.traderID, cycle.ID, cycle.Symbol, cycle.Side, message)
		return
	}
	logger.Errorf("🚨 [%s] Copy Guard 仓位不可保护 | cycle=%d %s %s | 处置=%s | %s", ti.traderID, cycle.ID, cycle.Symbol, cycle.Side, action, message)
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "GUARD_UNPROTECTABLE", Metadata: map[string]interface{}{
		"action": action, "error": message, "leader_pos_id": cycle.LeaderPosID, "symbol": cycle.Symbol, "side": cycle.Side, "retries": cycle.ProtectionRetries,
	}})

	if action == "follow" {
		_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionUnprotectable, 0, message, cycle.FollowerPosID, cycle.EntryOrderID, false)
		ti.notifyProtection(cycle, "Copy Guard 仓位不可保护（裸跑中）", fmt.Sprintf("该仓位无法建立任何有效止损保护（含极紧 clamp 止损）。\n处置模式为 follow：仓位继续跟随领航员，但当前没有交易所托管的止损单。\n原因: %s\n系统每 %.0f 分钟自动复检一次；建议人工评估是否手动平仓。", message, unprotectableRecheckDelay.Minutes()), "unprotectable")
		return
	}

	// close 模式：强制离场。保护单在市价平仓确认前保持 live，避免撤单
	// 与平仓之间出现无保护窗口；平仓成功后再精确清理本周期的保护单。
	closeAction := "close_long"
	if cycle.Side == "short" {
		closeAction = "close_short"
	}
	dec := &decision.Decision{Symbol: cycle.Symbol, Action: closeAction, MarginMode: cycle.MarginMode, LeaderPosID: cycle.LeaderPosID, Reasoning: "Copy Guard forced exit: position unprotectable"}
	ti.captureV4FollowerBeforeClose(dec)
	closer, ok := ti.executor.(EmergencyPositionCloser)
	if !ok {
		ti.markProtectionIssue(cycle, store.CopyGuardProtectionDegraded, "GUARD_FORCED_EXIT_FAILED", fmt.Errorf("executor does not support preserving emergency close"), 0, false)
		return
	}
	if _, execErr := closer.ClosePositionMarket(cycle.Symbol, cycle.Side); execErr != nil {
		// 平仓失败：标 DEGRADED 留在重试链里（下一轮到封顶仍会再次进入本处置）
		logger.Errorf("❌ [%s] Copy Guard 强制离场失败 | cycle=%d %s: %v", ti.traderID, cycle.ID, cycle.Symbol, execErr)
		ti.markProtectionIssue(cycle, store.CopyGuardProtectionDegraded, "GUARD_FORCED_EXIT_FAILED", execErr, 0, false)
		return
	}
	if mgr, ok := ti.executor.(ProtectiveStopManagerV4); ok {
		if order, oerr := ti.store.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID); oerr == nil {
			if cancelErr := ti.cancelProtectiveOrderForCycle(mgr, cycle, order); cancelErr != nil {
				logger.Warnf("⚠️ [%s] Copy Guard 强平后撤保护单失败（巡检将继续重试）: %v", ti.traderID, cancelErr)
				ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN", cancelErr, cycle.ProtectionCoverage, false)
			}
		}
	}
	price := float64(0)
	if slMgr, ok := ti.executor.(StopLossManager); ok {
		if p, pErr := slMgr.GetMarketPrice(cycle.Symbol); pErr == nil {
			price = p
		}
	}
	if price <= 0 {
		price = cycle.FollowerEntryPrice
	}
	quantity := float64(0)
	if cycle.FollowerEntryPrice > 0 {
		quantity = cycle.FollowerNotional / cycle.FollowerEntryPrice
	}
	// 领航员快照 → mapping 进观察态（重入判据照常运行）
	pnl, size, addCount := float64(0), float64(0), 0
	if leader := ti.engine.buildLeaderPosMap()[cycle.LeaderPosID]; leader != nil {
		pnl, size = leader.UnrealizedPnL, leader.Size
		if leader.EntryPrice > 0 {
			_ = ti.store.CopyTrade().SnapshotCopyGuardLeaderEntryAtStop(cycle.ID, leader.EntryPrice)
		}
	}
	if mapping, _ := ti.store.CopyTrade().GetMapping(ti.traderID, cycle.LeaderPosID); mapping != nil {
		addCount = mapping.AddCount
	}
	atr, _ := market.GetOKXATRWithMaxAge(cycle.Symbol, ti.engine.config.RiskATRTimeframe, ti.engine.config.RiskATRPeriod, riskATRCacheMaxAge(ti.engine.config))
	if recordErr := ti.store.CopyTrade().RecordCopyGuardStopObserved(cycle.ID, ti.traderID, cycle.ReentryCount, atr, price, quantity, map[string]interface{}{
		"confirmation": "guard_forced_exit", "error": message, "leader_pnl": pnl,
	}); recordErr != nil {
		logger.Errorf("[CopyGuard] trader=%s cycle=%d attempt=%d event=FORCED_EXIT_PERSIST_FAILED reason=%v", ti.traderID, cycle.ID, cycle.ReentryCount, recordErr)
		ti.notifyProtection(cycle, "Copy Guard 已退出但账本落库失败", "仓位已强制退出，但本地止损账本提交失败；系统保持禁止重入并将在仓位缺失检测中重试。错误: "+recordErr.Error(), fmt.Sprintf("forced_exit_persist_%d", cycle.ReentryCount))
		return
	}
	_ = ti.store.CopyTrade().MarkStoppedByRisk(ti.traderID, cycle.LeaderPosID, pnl, size, addCount)
	_ = ti.store.ReentryAI().ConsumeCopyGuardRisk(cycle.ID, cycle.ReentryCount)
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "GUARD_FORCED_EXIT", Price: price, Quantity: quantity, Metadata: map[string]interface{}{
		"error": message, "leader_pos_id": cycle.LeaderPosID,
	}})
	_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionCanceled, 0, message, cycle.FollowerPosID, cycle.EntryOrderID, false)
	logger.Warnf("🛑 [%s] Copy Guard 强制离场完成（不可保护） | cycle=%d %s %s price≈%.4f", ti.traderID, cycle.ID, cycle.Symbol, cycle.Side, price)
	ti.notifyProtection(cycle, "Copy Guard 强制离场（仓位不可保护）", fmt.Sprintf("该仓位无法建立任何有效止损保护（含极紧 clamp 止损），已按处置模式 close 市价平仓。\n原因: %s\n周期进入观察期，满足重入条件且可保护时会再次进场。", message), "forced_exit")
}

func (ti *TraderIntegration) recoverV4PendingStates() {
	cycles, err := ti.store.CopyTrade().ListOpenCopyGuardCycles(ti.traderID)
	if err != nil {
		return
	}
	for _, cycle := range cycles {
		if cycle.Status == store.CopyGuardReentryPending {
			if !cycle.UpdatedAt.IsZero() && time.Since(cycle.UpdatedAt) < 10*time.Second {
				continue
			}
			if cycle.EntryOrderID == "" {
				_ = ti.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardStoppedWatching, cycle.LeaderEntryPrice, cycle.LastObservedPrice, 0)
				_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERED_AFTER_RESTART", Metadata: map[string]interface{}{"result": "returned_to_watching", "reason": "legacy pending order identity unavailable"}})
				// v5.1：人工确认后决策入队但重启丢失（尚未落 entry_order_id）→
				// 回写孤儿 EXECUTING 信号为 FAILED，避免前端永久卡在"执行中…"。
				// 周期已回退 STOPPED_WATCHING，下轮观察会重新生成 PENDING 信号。
				ti.markManualReentryOutcome(cycle.ID, store.ManualReentryStatusFailed, "重启后未确认下单，已回退观察期")
				continue
			}
			provider, ok := ti.executor.(ClientOrderStatusProvider)
			if !ok {
				_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERY_PENDING", Metadata: map[string]interface{}{"client_order_id": cycle.EntryOrderID, "error": "client order lookup unsupported"}})
				ti.notifyProtection(cycle, "Copy Guard 重入状态待确认", "重启后无法确认重入订单状态，系统不会重复下单，请人工检查执行交易所。", "reentry-recovery")
				continue
			}
			_ = ti.store.CopyTrade().UpdateCopyGuardPendingOrder(cycle.ID, cycle.EntryOrderID)
			order, queryErr := provider.GetOrderStatusByClientID(cycle.Symbol, cycle.EntryOrderID)
			if queryErr != nil {
				_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERY_PENDING", Metadata: map[string]interface{}{"client_order_id": cycle.EntryOrderID, "error": queryErr.Error()}})
				continue
			}
			state := strings.ToUpper(getStringField(order, "status"))
			executedQty := getFloatField(order, "executedQty")
			// A canceled market order may still contain a partial fill. Any
			// non-zero exchange quantity is a real position and must enter the
			// protection barrier instead of being treated as a harmless failure.
			if executedQty > 0 {
				if recoverErr := ti.recoverFilledReentry(cycle, order); recoverErr != nil {
					_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERY_PENDING", Metadata: map[string]interface{}{"client_order_id": cycle.EntryOrderID, "state": state, "executed_quantity": executedQty, "error": recoverErr.Error()}})
					ti.notifyProtection(cycle, "Copy Guard 重入成交恢复失败", "交易所确认已有成交，但恢复保护链失败，系统不会重复下单。错误: "+recoverErr.Error(), fmt.Sprintf("reentry-recovery-fill-%d", cycle.ReentryCount+1))
				}
				continue
			}
			switch state {
			case "FILLED":
				// A filled order without an executed quantity is not enough evidence
				// to size protection. Keep it pending and escalate.
				_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERY_PENDING", Metadata: map[string]interface{}{"client_order_id": cycle.EntryOrderID, "state": state, "error": "filled order missing executed quantity"}})
			case "CANCELED", "REJECTED", "FAILED":
				attemptNo := cycle.ReentryCount + 1
				_ = ti.store.ReentryAI().ReleaseCopyGuardRisk(cycle.ID, attemptNo)
				status := store.CopyGuardStoppedWatching
				if candidate, candidateErr := ti.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID); candidateErr == nil && candidate.Status == store.ReentryCandidateEntryPending {
					retry := ti.engine.config.RiskAIMinReviewSeconds
					if retry < 300 {
						retry = 300
					}
					_ = ti.store.ReentryAI().RejectReentryCandidatePreflight(candidate.ID, "exchange confirmed "+state, time.Duration(retry)*time.Second)
					status = store.CopyGuardAIWaiting
				}
				_ = ti.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, status, cycle.LeaderEntryPrice, cycle.LastObservedPrice, 0)
				_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERED_AFTER_RESTART", Metadata: map[string]interface{}{"result": "failed", "state": state, "client_order_id": cycle.EntryOrderID}})
				// v5.1：重启期间人工重入订单被取消/拒绝 → 回写信号 FAILED
				ti.markManualReentryOutcome(cycle.ID, store.ManualReentryStatusFailed, "重启后确认订单为"+state)
			}
		}
	}
}

// recoverFilledReentry converts an exchange-authoritative client-order lookup
// into the normal lifecycle and immediately rebuilds/verifies protection. It is
// used both after restart and after an ambiguous submit response.
func (ti *TraderIntegration) recoverFilledReentry(cycle *store.CopyGuardCycle, order map[string]interface{}) error {
	if cycle == nil || cycle.Status != store.CopyGuardReentryPending {
		return fmt.Errorf("reentry cycle is no longer pending")
	}
	entry := getFloatField(order, "avgPrice")
	executedQuantity := math.Abs(getFloatField(order, "executedQty"))
	// executedQty is historical order evidence, not proof that the position is
	// still open after a restart. A user/forced close in the crash window must
	// not be resurrected into an active lifecycle. Protect the authoritative
	// current position quantity (which may also be smaller after a partial exit).
	quantity, positionKnown := ti.followerPositionQuantity(cycle.Symbol, cycle.Side, cycle.MarginMode, true)
	if !positionKnown {
		return fmt.Errorf("exchange fill exists but current follower position is unavailable")
	}
	if quantity <= 0 {
		return fmt.Errorf("exchange fill quantity %.8f exists but current follower position is flat", executedQuantity)
	}
	if entry <= 0 {
		if mgr, ok := ti.executor.(StopLossManager); ok {
			entry, _ = mgr.GetMarketPrice(cycle.Symbol)
		}
	}
	if entry <= 0 || quantity <= 0 {
		return fmt.Errorf("exchange fill lacks a usable price or quantity")
	}
	notional := entry * quantity
	leaderSize := float64(0)
	if ti.engine != nil {
		if leader := ti.engine.buildLeaderPosMap()[cycle.LeaderPosID]; leader != nil {
			leaderSize = leader.Size
		}
	}
	orderID := getStringField(order, "orderId")
	if err := ti.store.CopyTrade().RecordCopyGuardReentryFilled(cycle, entry, notional, quantity, 0, map[string]interface{}{
		"result":                  "filled",
		"client_order_id":         cycle.EntryOrderID,
		"exchange_order_id":       orderID,
		"recovered_after_restart": true,
		"activate_mapping":        true,
		"leader_size":             leaderSize,
	}); err != nil {
		return err
	}
	ti.markManualReentryOutcome(cycle.ID, store.ManualReentryStatusExecuted, "")
	action := "open_long"
	if cycle.Side == "short" {
		action = "open_short"
	}
	dec := &decision.Decision{Symbol: cycle.Symbol, Action: action, PositionSizeUSD: notional, MarginMode: cycle.MarginMode, Reasoning: "Copy trading reentry recovered", EntryPrice: entry, LeaderPosID: cycle.LeaderPosID, LeaderPosSize: leaderSize, ClientOrderID: cycle.EntryOrderID, ExchangeOrderID: orderID}
	ti.refreshStopLossAfterExecute(dec)
	if ti.engine != nil && ti.engine.config != nil && ti.engine.config.RiskReentryDecisionMode == "ai_guarded" {
		ti.enforceAIReentryProtection(dec)
	}
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
		if ti.engine.config.RiskPolicyVersion >= 4 && strings.Contains(dec.Reasoning, "reentry") && dec.ClientOrderID == "" {
			if cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID); err == nil {
				dec.ClientOrderID = fmt.Sprintf("cgr%da%d", cycle.ID, cycle.ReentryCount+1)
				_ = ti.store.CopyTrade().UpdateCopyGuardPendingOrder(cycle.ID, dec.ClientOrderID)
			}
		}

		// 记录决策日志
		ti.logDecision(fullDec, dec)

		ti.captureV4FollowerBeforeClose(dec)

		// 执行交易（瞬态错误有界重试：限流/读失败等"保证未下单"错误不再一次定死）
		startTime := time.Now()
		var err error
		if ti.engine.config.RiskReentryDecisionMode == "ai_guarded" && strings.Contains(dec.Reasoning, "reentry") {
			err = ti.validateAIReentryImmediatelyBeforeOrder(dec)
		}
		if err == nil {
			err = ti.preflightSmartMoneyExecutionInstrument(dec)
		}
		if err == nil {
			ti.transitionExecutionIntent(dec, store.ExecutionIntentSubmitted, "", "")
			ti.recordAIReentrySubmitted(dec)
			err = ti.executeDecisionWithRetry(dec)
		}

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
			action.Error = err.Error()
			if ti.engine.config.RiskPolicyVersion >= 4 && strings.Contains(dec.Reasoning, "reentry") {
				if cycle, cerr := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID); cerr == nil {
					ti.handleReentryExecutionFailure(cycle, err)
					// v5.1：人工重入执行失败 → 信号回写 FAILED（周期无 EXECUTING
					// 信号时为 no-op，自动重入路径零影响）
					ti.markManualReentryOutcome(cycle.ID, store.ManualReentryStatusFailed, err.Error())
				}
			}
			// 🔑 良性失败识别：close/reduce 类决策遇到"本地无对应仓位"错误时，
			// 说明跟随者本地仓位已经通过其他途径消失（手动平、强平、历史 mapping 残留等）。
			// 这种情况下：
			//   1. 主动关闭 mapping，避免引擎下个轮询又生成同样的 close 信号 → 死循环
			//   2. 不发邮件告警（不是真正的错误，是数据自愈）
			//   3. 状态记 silent_close 区分于真正的 failed
			// 开仓/加仓 (open_*/reduce_*) 永远不会进入这里。
			if errors.Is(err, trader.ErrExecutionInstrumentUnsupported) && (dec.Action == "open_long" || dec.Action == "open_short") {
				ti.transitionExecutionIntent(dec, store.ExecutionIntentSkipped, "EXECUTION_INSTRUMENT_UNSUPPORTED", err.Error())
				ti.handleUnsupportedExecutionInstrument(dec, err)
				executionLogs = append(executionLogs, fmt.Sprintf("⏭️ %s %s 跳过（执行交易所无精确合约）: %v", dec.Action, dec.Symbol, err))
				ti.saveSignalLog(dec, "skipped", err.Error())
			} else if errors.Is(err, trader.ErrExecutionInstrumentUnsupported) {
				ti.transitionExecutionIntent(dec, store.ExecutionIntentFailed, "EXECUTION_INSTRUMENT_UNSUPPORTED", err.Error())
				// A stored active mapping already identifies the exact execution
				// contract. Catalog/precision failures may block new risk, but must
				// never consume a reduce/close signal or close its mapping locally.
				executionLogs = append(executionLogs, fmt.Sprintf("❌ %s %s 风险降低待重试: %v", dec.Action, dec.Symbol, err))
				ti.handleRiskReductionRetry(dec, err, "未消费该信号、未关闭映射；系统将使用活动映射中的精确合约继续重试。")
			} else if errors.Is(err, trader.ErrPartialCloseSkipped) {
				// 减仓被边界约束跳过（未下单）：不是失败也不是成功。
				// 不更新 mapping/lastKnownSize（伪装成功正是旧 bug 根源）、
				// 不重挂 SL、不告警、不熔断、不重放（重放结果相同，
				// 等领航员后续减仓累积到全平阈值自然收敛）。
				logger.Infof("⏭️ [%s] 跟单减仓跳过（未下单） | %s %s | %v",
					ti.traderID, dec.Action, dec.Symbol, err)
				executionLogs = append(executionLogs,
					fmt.Sprintf("⏭️ %s %s 跳过（未下单）: %v", dec.Action, dec.Symbol, err))
				reasonCode := "PARTIAL_CLOSE_SKIPPED"
				if errors.Is(err, trader.ErrPartialCloseSubLot) {
					reasonCode = "SKIPPED_SUBLOT"
					if dec.LeaderPosSize >= 0 && dec.LeaderPosID != "" {
						if updateErr := ti.store.CopyTrade().UpdateLastKnownSize(ti.traderID, dec.LeaderPosID, dec.LeaderPosSize); updateErr != nil {
							logger.Warnf("⚠️ [%s] 微量减仓跳过后推进源修订失败: %v", ti.traderID, updateErr)
						}
					}
				}
				ti.transitionExecutionIntent(dec, store.ExecutionIntentSkipped, reasonCode, err.Error())
				ti.saveSignalLog(dec, "skipped", err.Error())
			} else if ti.isBenignCloseError(dec, err) {
				if ti.benignCloseConfirmedFlat(dec) {
					ti.transitionExecutionIntent(dec, store.ExecutionIntentSkipped, "POSITION_ALREADY_FLAT", err.Error())
					ti.handleBenignCloseFailure(dec, err)
					executionLogs = append(executionLogs,
						fmt.Sprintf("🟡 %s %s silent_close (本地仓位已不存在，自动回收映射): %v",
							dec.Action, dec.Symbol, err))
				} else {
					ti.transitionExecutionIntent(dec, store.ExecutionIntentFailed, "FLATNESS_NOT_CONFIRMED", err.Error())
					executionLogs = append(executionLogs,
						fmt.Sprintf("❌ %s %s 无仓错误未通过实时空仓确认，风险降低待重试: %v",
							dec.Action, dec.Symbol, err))
					ti.handleRiskReductionRetry(dec, err, "交易所实时仓位查询未确认空仓；未消费信号、未关闭映射，等待下一轮重试。")
				}
			} else if ti.reconcileExistingCopyPosition(dec, err) {
				action.Success = true
				action.Error = ""
				executionLogs = append(executionLogs, fmt.Sprintf("🟡 %s %s 已通过交易所订单/仓位对账恢复", dec.Action, dec.Symbol))
				ti.saveSignalLog(dec, "executed", "reconciled existing position")
			} else {
				// 瞬态错误（限流/读阶段失败，保证订单未进交易所）重试耗尽：
				// 释放引擎侧 fill 去重标记，让下轮 poll 重放该信号
				// （fill 仍在回看窗口内时可恢复；非瞬态错误不重放，避免死循环）。
				if isRetryableExecutionError(err) && dec.SourceFillID != "" {
					ti.engine.UnmarkSeen(dec.SourceFillID)
					logger.Warnf("🔁 [%s] 瞬态失败重试耗尽，已释放去重标记等待重放 | %s %s fillID=%s",
						ti.traderID, dec.Action, dec.Symbol, dec.SourceFillID)
				}
				ti.transitionExecutionIntent(dec, store.ExecutionIntentFailed, classifyExecutionFailure(err), err.Error())
				traderName := ti.traderDisplayName()
				logger.Errorf("❌ [%s/%s] 跟单执行失败 | %s %s | error=%v",
					traderName, ti.traderID, dec.Action, dec.Symbol, err)
				executionLogs = append(executionLogs, fmt.Sprintf("❌ %s %s 失败: %v", dec.Action, dec.Symbol, err))
				ti.saveSignalLog(dec, "failed", err.Error())
				alertKey := ti.execFailureDedupKey(dec, err)

				// 异步发送邮件告警（未启用通知器时为 no-op，零阻塞、零副作用）
				notifier.Notify(notifier.Alert{
					Category:   "copy_trade",
					TraderID:   ti.traderID,
					TraderName: traderName,
					Title:      fmt.Sprintf("%s | %s %s 失败", traderName, dec.Action, dec.Symbol),
					Body:       ti.buildExecFailureAlertBody(dec, err, traderName),
					RateKey:    alertKey,
					DedupKey:   alertKey,
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
			action.Success = true
			duration := time.Since(startTime).Milliseconds()
			logger.Infof("✅ [%s] 跟单执行成功 | %s %s | 耗时=%dms",
				ti.traderID, dec.Action, dec.Symbol, duration)
			executionLogs = append(executionLogs, fmt.Sprintf("✅ %s %s 成功 (耗时 %dms)", dec.Action, dec.Symbol, duration))
			ti.saveSignalLog(dec, "executed", "")
			ti.transitionExecutionIntent(dec, store.ExecutionIntentFilled, "", "")

			// 执行成功后更新仓位映射。Copy Guard 重入必须把 mapping、cycle
			// 和新 attempt 原子提交；提交失败时绝不能继续按一个错误 attempt
			// 编号挂保护单，也不能把该成交伪装为完整成功。
			postFillCommitted := true
			if ti.engine.config.RiskPolicyVersion >= 4 && strings.Contains(dec.Reasoning, "reentry") {
				if commitErr := ti.commitCopyGuardReentryFill(dec); commitErr != nil {
					postFillCommitted = false
					ti.handleReentryLifecycleCommitFailure(dec, commitErr)
				}
			} else {
				ti.updatePositionMapping(dec)
			}

			if postFillCommitted {
				// 🛑 v3 风控：开仓/加仓/部分减仓 → 用实际成交均价精确重挂 SL
				// 平仓不重挂（仓位已经全清空）
				// 设计：放在 mapping 更新后，保证后续 SL 触发对账能找到正确的 active mapping
				ti.refreshStopLossAfterExecute(dec)
				if ti.engine.config.RiskReentryDecisionMode == "ai_guarded" {
					if strings.Contains(dec.Reasoning, "reentry") {
						ti.enforceAIReentryProtection(dec)
					} else {
						ti.enforceAIGuardedPositionProtection(dec)
					}
				}
			}

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

		action.ExecutionIntentID = dec.ExecutionIntentID
		action.SourceFillID = dec.SourceFillID
		action.LeaderPosID = dec.LeaderPosID
		action.SourceRevision = dec.SourceRevision
		action.RequestedQuantity = dec.RequestedQuantity
		action.QuantizedQuantity = dec.QuantizedQuantity
		action.FilledQuantity = dec.FilledQuantity
		action.ExchangeOrderID = dec.ExchangeOrderID
		action.ExecutionStatus = dec.ExecutionStatus
		action.ExecutionReasonCode = dec.ExecutionReasonCode
		if action.Success && dec.ExecutionIntentID > 0 {
			switch dec.ExecutionStatus {
			case store.ExecutionIntentProtected, store.ExecutionIntentFilled, store.ExecutionIntentSkipped:
			default:
				action.Success = false
				if action.Error == "" {
					action.Error = dec.ExecutionReasonCode
				}
			}
		}
		if dec.FilledQuantity > 0 {
			action.Quantity = dec.FilledQuantity
		} else if dec.QuantizedQuantity > 0 {
			action.Quantity = dec.QuantizedQuantity
		}
		decisionActions = append(decisionActions, action)
	}

	// 保存到 decision_records 表，复用现有日志系统
	ti.saveDecisionRecord(fullDec, decisionActions, executionLogs)
}

func (ti *TraderIntegration) recordAIReentrySubmitted(dec *decision.Decision) {
	if dec == nil || ti.store == nil || ti.engine == nil || ti.engine.config == nil || ti.engine.config.RiskReentryDecisionMode != "ai_guarded" || !strings.Contains(dec.Reasoning, "reentry") {
		return
	}
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil {
		return
	}
	metadata := map[string]interface{}{"attempt_no": cycle.ReentryCount + 1, "intent_id": dec.ExecutionIntentID, "client_order_id": dec.ClientOrderID}
	if candidate, candidateErr := ti.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID); candidateErr == nil {
		metadata["candidate_id"] = candidate.ID
		metadata["analysis_id"] = candidate.LastAnalysisID
		metadata["decision_generation"] = candidate.DecisionGeneration
	}
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_SUBMITTED", Price: dec.EntryPrice, Notional: dec.PositionSizeUSD, Metadata: metadata})
}

func (ti *TraderIntegration) preflightSmartMoneyExecutionInstrument(dec *decision.Decision) error {
	if ti.engine == nil || !ti.engine.isSmartMoneyMode() || dec == nil {
		return nil
	}
	if dec.Action != "open_long" && dec.Action != "open_short" {
		return nil
	}
	resolver, ok := ti.executor.(trader.ExecutionInstrumentResolver)
	if !ok {
		return fmt.Errorf("%w: execution venue has no exact instrument resolver", trader.ErrExecutionInstrumentUnsupported)
	}
	inst, err := resolver.ResolveExecutionInstrument(dec.Symbol)
	if err != nil {
		return fmt.Errorf("%w: %v", trader.ErrExecutionInstrumentUnsupported, err)
	}
	if ti.store != nil && dec.LeaderPosID != "" {
		_ = ti.store.CopyTrade().ClearUnsupportedExecutionInstrument(ti.traderID, dec.LeaderPosID)
	}
	dec.SourceSymbol = inst.SourceSymbol
	dec.ExecutionSymbol = inst.NativeSymbol
	dec.ExecutionSettleAsset = inst.SettleAsset
	return nil
}

func (ti *TraderIntegration) handleUnsupportedExecutionInstrument(dec *decision.Decision, cause error) {
	if dec == nil || ti.store == nil || ti.engine == nil || ti.engine.config == nil {
		return
	}
	copyStore := ti.store.CopyTrade()
	venue := ""
	if ti.executor != nil {
		venue = fmt.Sprintf("%T", ti.executor)
	}
	if err := copyStore.MarkUnsupportedExecutionInstrument(ti.traderID, dec.LeaderPosID, dec.Symbol, venue, cause.Error()); err != nil {
		logger.Warnf("⚠️ [%s] 保存不支持合约状态失败: %v", ti.traderID, err)
	}
	mapping, err := copyStore.GetMapping(ti.traderID, dec.LeaderPosID)
	if err != nil {
		logger.Warnf("⚠️ [%s] 查询不支持合约基线失败: %v", ti.traderID, err)
	} else if mapping == nil || mapping.Status == store.MappingStatusClosed {
		side := "long"
		if dec.Action == "open_short" {
			side = "short"
		}
		if err := copyStore.RebaselineIgnoredPosition(ti.traderID, ti.engine.config.LeaderID, dec.LeaderPosID, dec.Symbol, side, dec.MarginMode, dec.LeaderPosSize); err != nil {
			logger.Warnf("⚠️ [%s] 保存不支持合约基线失败: %v", ti.traderID, err)
		}
	} else if mapping.Status == store.MappingStatusActive && dec.LeaderPosSize > mapping.LastKnownSize {
		if err := copyStore.UpdateLastKnownSize(ti.traderID, dec.LeaderPosID, dec.LeaderPosSize); err != nil {
			logger.Warnf("⚠️ [%s] 更新不支持合约加仓基线失败: %v", ti.traderID, err)
		}
	}
	traderName := ti.traderDisplayName()
	dedup := fmt.Sprintf("unsupported_instrument|%s|%s|%d", ti.traderID, dec.Symbol, time.Now().Unix()/3600)
	notifier.Notify(notifier.Alert{
		Category: "copy_trade", TraderID: ti.traderID, TraderName: traderName,
		Title:   fmt.Sprintf("%s | 执行交易所不支持 %s", traderName, dec.Symbol),
		Body:    fmt.Sprintf("领航员: %s\n源合约: %s\n原因: %v\n处理: 本次开仓/加仓已跳过并重建基线，不会回退到 USDT 合约；已有映射的减仓/平仓仍保留。", ti.engine.config.LeaderID, dec.Symbol, cause),
		RateKey: dedup, DedupKey: dedup,
	})
	ti.recordCopyEvent(&store.CopyTradeEvent{
		Category: store.CopyEventCategoryError, EventType: "EXECUTION_INSTRUMENT_UNSUPPORTED",
		Severity: store.CopyEventSeverityWarn, Symbol: dec.Symbol, LeaderPosID: dec.LeaderPosID,
		Status: "skipped", Summary: "执行交易所无精确 base+quote/settle 合约，风险增加已跳过",
		Detail: map[string]interface{}{"reason": cause.Error(), "action": dec.Action}, DedupKey: dedup,
	})
}

// executeDecisionRetryBackoffs 决策执行瞬态失败的重试退避序列。
// 总时长 ≤ 14s：对跟单延迟可接受，且在 consumeDecisions 消费 goroutine 内
// 阻塞执行，天然保持同一 trader 决策的先后顺序。
var executeDecisionRetryBackoffs = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// executeDecisionWithRetry 执行决策，仅对"保证订单未进交易所"的瞬态错误
// 做有界退避重试（见 isRetryableExecutionError）。
//
// 背景（OKX 50011 跟单执行失败）：开仓执行链第一步是 GetPositions 前置读，
// 撞上限流会让整个决策立即失败；而 fill 在引擎层已 markSeen、OKX 路径无
// 快照兜底信号 → 一次读失败 = 该次跟单永久错过。重试耗尽后仍走原有失败
// 分支（告警邮件 / 熔断计数 / v5.1 信号回写），行为不变。
func (ti *TraderIntegration) executeDecisionWithRetry(dec *decision.Decision) error {
	executionDecision := dec
	// DecisionExecutor and AutoTrader use normalized base+quote symbols for
	// position lookup; the concrete exchange adapter owns conversion to native
	// IDs such as BTC-USDC-SWAP. Passing NativeSymbol through this abstraction
	// makes OKX GetPositions (BTCUSDC) impossible to match during reduce/close.
	if dec != nil && dec.SourceSymbol != "" && !strings.EqualFold(dec.SourceSymbol, dec.Symbol) {
		clone := *dec
		clone.Symbol = dec.SourceSymbol
		executionDecision = &clone
	}
	err := ti.executor.ExecuteDecision(executionDecision)
	for attempt := 0; err != nil && attempt < len(executeDecisionRetryBackoffs); attempt++ {
		if !isRetryableExecutionError(err) {
			return err
		}
		backoff := executeDecisionRetryBackoffs[attempt]
		logger.Warnf("🔁 [%s] 跟单执行瞬态失败，%v 后重试（第 %d/%d 次） | %s %s | error=%v",
			ti.traderID, backoff, attempt+1, len(executeDecisionRetryBackoffs), dec.Action, dec.Symbol, err)
		select {
		case <-ti.ctx.Done():
			return err
		case <-time.After(backoff):
		}
		err = ti.executor.ExecuteDecision(executionDecision)
	}
	if executionDecision != nil && executionDecision != dec {
		dec.MarginMode = executionDecision.MarginMode
		dec.PositionSizeUSD = executionDecision.PositionSizeUSD
		dec.ClientOrderID = executionDecision.ClientOrderID
		dec.ExchangeOrderID = executionDecision.ExchangeOrderID
		dec.RequestedQuantity = executionDecision.RequestedQuantity
		dec.QuantizedQuantity = executionDecision.QuantizedQuantity
		dec.FilledQuantity = executionDecision.FilledQuantity
		dec.QuantityStepOverride = executionDecision.QuantityStepOverride
	}
	return err
}

// isRetryableExecutionError 判断决策执行失败是否可安全重试。
//
// 可重试的前提是"订单保证没有进入交易所"，避免重试造成重复下单：
//   - OKX 50011 / Too Many Requests：网关限流拒绝，请求未被处理
//     （下单请求被 50011 拒绝时订单同样未创建，重试安全）
//   - "failed to get positions" / "failed to get account balance" /
//     "failed to get market price"：执行链下单前的读取阶段失败
//     （auto_trader 的错误包装前缀），无论底层原因（限流/超时/网络）都
//     发生在任何下单动作之前
//
// 明确不重试：下单阶段的超时等歧义错误（订单可能已进交易所）、
// 业务性拒单（保证金不足有独立减半重试、重复仓位、最小下单额等）。
func isRetryableExecutionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "code=50011") || strings.Contains(msg, "too many requests") {
		return true
	}
	readStagePrefixes := []string{
		"failed to get positions",
		"failed to get account balance",
		"failed to get market price",
	}
	for _, kw := range readStagePrefixes {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

func (ti *TraderIntegration) transitionExecutionIntent(dec *decision.Decision, status, reasonCode, message string) {
	if dec == nil || dec.ExecutionIntentID <= 0 || ti.store == nil {
		return
	}
	dec.ExecutionStatus = status
	dec.ExecutionReasonCode = reasonCode
	if err := ti.store.CopyTrade().UpdateExecutionIntent(dec.ExecutionIntentID, status, reasonCode, message,
		dec.ExchangeOrderID, dec.RequestedQuantity, dec.QuantizedQuantity, dec.FilledQuantity); err != nil {
		logger.Warnf("⚠️ [%s] 更新跟单执行意图失败 | intent=%d status=%s: %v", ti.traderID, dec.ExecutionIntentID, status, err)
	}
}

func classifyExecutionFailure(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "already has") && strings.Contains(msg, "position"):
		return "POSITION_EXISTS"
	case strings.Contains(msg, "minimum") || strings.Contains(msg, "minsz") || strings.Contains(msg, "min notional"):
		return "MIN_NOTIONAL"
	case strings.Contains(msg, "margin") || strings.Contains(msg, "balance"):
		return "INSUFFICIENT_MARGIN"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") || strings.Contains(msg, "connection"):
		return "TRANSPORT_AMBIGUOUS"
	case strings.Contains(msg, "50011") || strings.Contains(msg, "too many requests"):
		return "RATE_LIMIT"
	default:
		return "EXECUTION_FAILED"
	}
}

// reconcileExistingCopyPosition treats a same-side conflict as an exchange
// reconciliation problem, never as evidence that the mapping should be
// circuit-broken. Adoption requires the canonical client order id to resolve
// to a real fill; an unrelated manual position is deliberately not adopted.
func (ti *TraderIntegration) reconcileExistingCopyPosition(dec *decision.Decision, executionErr error) bool {
	if dec == nil || executionErr == nil || dec.ExecutionIntentID <= 0 || dec.ClientOrderID == "" {
		return false
	}
	if dec.Action != "open_long" && dec.Action != "open_short" {
		return false
	}
	msg := strings.ToLower(executionErr.Error())
	if !strings.Contains(msg, "already has") || !strings.Contains(msg, "position") {
		return false
	}
	provider, ok := ti.executor.(ClientOrderStatusProvider)
	if !ok {
		return false
	}
	order, err := provider.GetOrderStatusByClientID(dec.Symbol, dec.ClientOrderID)
	if err != nil {
		ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "POSITION_EXISTS_LOOKUP_PENDING", err.Error())
		return false
	}
	filled := getFloatField(order, "executedQty", "filled_quantity", "quantity")
	state := strings.ToUpper(getStringField(order, "status", "state"))
	if filled <= 0 && state != "FILLED" {
		return false
	}
	dec.FilledQuantity = filled
	dec.ExchangeOrderID = getStringField(order, "orderId", "ordId", "exchange_order_id")
	if price := getFloatField(order, "avgPrice", "fillPx", "price"); price > 0 {
		dec.EntryPrice = price
		if filled > 0 {
			dec.PositionSizeUSD = price * filled
		}
	}
	ti.updatePositionMapping(dec)
	ti.refreshStopLossAfterExecute(dec)
	if dec.ExecutionStatus != store.ExecutionIntentProtected && dec.ExecutionStatus != store.ExecutionIntentReconciling && dec.ExecutionStatus != store.ExecutionIntentFailed {
		ti.transitionExecutionIntent(dec, store.ExecutionIntentFilled, "POSITION_RECONCILED", "")
	}
	logger.Warnf("🟡 [%s] 同向仓位冲突已经交易所订单对账恢复 | intent=%d order=%s %s", ti.traderID, dec.ExecutionIntentID, dec.ExchangeOrderID, dec.Symbol)
	return true
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
//     Binance (binance_futures.go:443,498)
//     Aster (aster_trader.go:755,846)
//     Hyperliquid (hyperliquid_trader.go:516,588)
//     Bybit (bybit_trader.go:383,428 — "no X position to close")
//   - "reduceonly order is rejected" / "position size is 0"
//     Binance fapi 返回的 reduce-only 拒绝（保险关键字）
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

func (ti *TraderIntegration) benignCloseConfirmedFlat(dec *decision.Decision) bool {
	if dec == nil {
		return false
	}
	side := ""
	switch dec.Action {
	case "close_long", "reduce_long":
		side = "long"
	case "close_short", "reduce_short":
		side = "short"
	default:
		return false
	}
	symbol := dec.SourceSymbol
	if symbol == "" {
		symbol = dec.Symbol
	}
	qty, ok := ti.followerPositionQuantity(symbol, side, dec.MarginMode, true)
	return ok && qty == 0
}

func (ti *TraderIntegration) handleRiskReductionRetry(dec *decision.Decision, err error, handling string) {
	if dec == nil || err == nil {
		return
	}
	if dec.SourceFillID != "" && ti.engine != nil {
		ti.engine.UnmarkSeen(dec.SourceFillID)
	}
	traderName := ti.traderDisplayName()
	logger.Errorf("❌ [%s/%s] 风险降低未确认完成，保留映射等待重试 | %s %s | error=%v",
		traderName, ti.traderID, dec.Action, dec.Symbol, err)
	ti.saveSignalLog(dec, "failed", err.Error())
	alertKey := ti.execFailureDedupKey(dec, err)
	notifier.Notify(notifier.Alert{
		Category: "copy_trade", TraderID: ti.traderID, TraderName: traderName,
		Title:   fmt.Sprintf("%s | %s %s 风险降低待重试", traderName, dec.Action, dec.Symbol),
		Body:    ti.buildExecFailureAlertBody(dec, err, traderName) + "\n处理: " + handling,
		RateKey: alertKey, DedupKey: alertKey,
	})
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
		// 自愈关闭映射前先闭合 Copy Guard 生命周期（撤保护单 + 记账收尾），
		// 与正常平仓路径语义对齐；否则周期成为孤儿，遗留的保护单可能在
		// 未来的同币种仓位上误触发。无开启周期时 finalize 内部直接返回。
		if ti.engine != nil && ti.engine.config != nil && SupportsCopyGuard(ti.engine.config.ProviderType) && ti.engine.config.RiskPolicyVersion >= 4 {
			ti.finalizeCopyGuardCycle(dec)
		}
		// 复用 SavePositionMapping/CloseMapping 路径，保持 status='closed' 语义一致
		if cerr := ti.store.CopyTrade().CloseMapping(ti.traderID, dec.LeaderPosID, dec.EntryPrice); cerr != nil {
			// 此时 mapping 仍保持 active；释放 seen key 后，下一轮
			// detectBinancePositionSnapshotFills/matchCloseReduceSignal 会重试本路径。
			if dec.SourceFillID != "" && ti.engine != nil {
				ti.engine.UnmarkSeen(dec.SourceFillID)
			}
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
	if dec.ExecutionIntentID > 0 {
		counted, countErr := ti.store.CopyTrade().MarkExecutionIntentFailureCounted(dec.ExecutionIntentID)
		if countErr != nil {
			logger.Warnf("[%s] 标记执行意图熔断计数失败: %v", ti.traderID, countErr)
			return
		}
		if !counted {
			return
		}
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

	// A circuit breaker may stop retries, but it may never erase ownership of
	// a live exchange position. Unknown position state also fails closed: keep
	// the mapping and reconcile instead of manufacturing an orphan.
	side := "long"
	if strings.HasSuffix(dec.Action, "short") {
		side = "short"
	}
	symbol := dec.SourceSymbol
	if symbol == "" {
		symbol = dec.Symbol
	}
	if ti.executor != nil {
		if qty, known := ti.followerPositionQuantity(symbol, side, dec.MarginMode, true); !known || qty > 0 {
			reason := "live follower position still exists"
			if !known {
				reason = "fresh follower position state is unavailable"
			}
			ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, "CIRCUIT_RECONCILIATION_REQUIRED", reason)
			logger.Errorf("🛑 [%s] mapping 熵断已拦截：%s，保留 mapping 和 Copy Guard 周期 | posId=%s qty=%.8f", ti.traderID, reason, dec.LeaderPosID, qty)
			return
		}
	}

	// 触发熔断：先闭合 Copy Guard 生命周期（撤保护单 + 记账收尾，语义与
	// 正常平仓路径对齐，避免遗留孤儿保护单），再关闭映射，最后发独立告警
	traderName := ti.traderDisplayName()
	if ti.engine != nil && ti.engine.config != nil && SupportsCopyGuard(ti.engine.config.ProviderType) && ti.engine.config.RiskPolicyVersion >= 4 {
		ti.finalizeCopyGuardCycle(dec)
	}
	if cerr := ti.store.CopyTrade().CloseMapping(ti.traderID, dec.LeaderPosID, dec.EntryPrice); cerr != nil {
		logger.Warnf("⚠️ [%s] 熔断关闭映射失败: %v", ti.traderID, cerr)
	}
	logger.Warnf("🛑 [%s/%s] mapping 熔断 | 连续失败 %d 次 → 主动关闭 | %s %s | 最近错误=%v",
		traderName, ti.traderID, count, dec.Action, dec.Symbol, execErr)

	alertKey := fmt.Sprintf("circuit|%s|%s", ti.traderID, dec.LeaderPosID)
	notifier.Notify(notifier.Alert{
		Category:   "copy_trade",
		TraderID:   ti.traderID,
		TraderName: traderName,
		Title:      fmt.Sprintf("%s | 跟单映射熔断（%s %s）", traderName, dec.Action, dec.Symbol),
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

	success := len(actions) > 0
	errorMessages := make([]string, 0)
	for _, action := range actions {
		if !action.Success {
			success = false
			if action.Error != "" {
				errorMessages = append(errorMessages, action.Error)
			}
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
		Success:             success,
		ErrorMessage:        strings.Join(errorMessages, "; "),
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

func (ti *TraderIntegration) handleReentryExecutionFailure(cycle *store.CopyGuardCycle, executionErr error) {
	if cycle == nil || executionErr == nil {
		return
	}
	// A transport timeout after order submission is not a failed order. Keep the
	// client-order lease, reservation and ENTRY_PENDING candidate intact until
	// exchange lookup proves filled or terminal; releasing here could duplicate
	// a real fill on the next AI review.
	if isAmbiguousReentryExecutionError(executionErr) && !errors.Is(executionErr, errAIReentryOrderPreflight) {
		metadata := map[string]interface{}{"error": executionErr.Error(), "attempt_no": cycle.ReentryCount + 1, "client_order_id": cycle.EntryOrderID}
		if provider, ok := ti.executor.(ClientOrderStatusProvider); ok && cycle.EntryOrderID != "" {
			if order, queryErr := provider.GetOrderStatusByClientID(cycle.Symbol, cycle.EntryOrderID); queryErr == nil {
				state := strings.ToUpper(getStringField(order, "status"))
				metadata["exchange_state"] = state
				if getFloatField(order, "executedQty") > 0 {
					if recoverErr := ti.recoverFilledReentry(cycle, order); recoverErr == nil {
						return
					} else {
						metadata["recovery_error"] = recoverErr.Error()
					}
				} else if state == "CANCELED" || state == "REJECTED" || state == "FAILED" {
					// Exchange authoritatively confirms no fill; normal safe-failure
					// handling below may release the reservation.
					goto safeFailure
				}
			} else {
				metadata["lookup_error"] = queryErr.Error()
			}
		}
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERY_PENDING", Metadata: metadata})
		ti.notifyProtection(cycle, "AI 重入订单状态不确定", "交易所返回不确定结果，系统保留幂等订单号并暂停重复入场，正在查询真实订单状态。错误: "+executionErr.Error(), fmt.Sprintf("ai_reentry_uncertain_%d", cycle.ReentryCount+1))
		return
	}

safeFailure:
	attemptNo := cycle.ReentryCount + 1
	_ = ti.store.ReentryAI().ReleaseCopyGuardRisk(cycle.ID, attemptNo)
	status := store.CopyGuardStoppedWatching
	metadata := map[string]interface{}{"error": executionErr.Error(), "attempt_no": attemptNo, "reason_code": classifyAIReentryExecutionError(executionErr)}
	if ti.engine != nil && ti.engine.config != nil && ti.engine.config.RiskReentryDecisionMode == "ai_guarded" {
		status = store.CopyGuardAIWaiting
		if candidate, err := ti.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID); err == nil && candidate.Status == store.ReentryCandidateEntryPending {
			retry := ti.engine.config.RiskAIMinReviewSeconds
			if retry < 300 {
				retry = 300
			}
			_ = ti.store.ReentryAI().RejectReentryCandidatePreflight(candidate.ID, "exchange entry failed: "+executionErr.Error(), time.Duration(retry)*time.Second)
			metadata["candidate_id"] = candidate.ID
			metadata["analysis_id"] = candidate.LastAnalysisID
			metadata["decision_generation"] = candidate.DecisionGeneration
		}
	}
	_ = ti.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, status, cycle.LeaderEntryPrice, cycle.LastObservedPrice, 0)
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_FAILED", Metadata: metadata})
}

func classifyAIReentryExecutionError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(err.Error())
	checks := []struct {
		code   string
		tokens []string
	}{
		{"SNAPSHOT_STALE", []string{"expired", "过期", "stale"}},
		{"PRICE_OUT_OF_RANGE", []string{"approved range", "入场区间", "0.25 atr", "漂移"}},
		{"MIN_NOTIONAL", []string{"minimum", "最小下单额", "名义"}},
		{"RISK_CAP", []string{"risk", "风险预算", "risk cap"}},
		{"POSITION_EXISTS", []string{"already has", "已有同向仓位"}},
		{"PROTECTION_UNAVAILABLE", []string{"protection", "保护", "stop loss"}},
		{"LEADER_CLOSED", []string{"leader position closed", "领航员", "reversed"}},
	}
	for _, check := range checks {
		for _, token := range check.tokens {
			if strings.Contains(message, token) {
				return check.code
			}
		}
	}
	return "EXECUTION_FAILED"
}

var errAIReentryOrderPreflight = errors.New("AI reentry final preflight rejected")

func aiDecisionExpired(analysis *store.ReentryAIAnalysis, ttlSeconds int, now time.Time) (bool, time.Duration) {
	if analysis == nil {
		return true, 0
	}
	if analysis.DecisionExpiresAt != nil {
		remaining := analysis.DecisionExpiresAt.Sub(now)
		return remaining <= 0, -remaining
	}
	if ttlSeconds < 15 || ttlSeconds > 60 {
		ttlSeconds = 30
	}
	age := now.Sub(analysis.SnapshotAt)
	return age < 0 || age > time.Duration(ttlSeconds)*time.Second, age
}

func (ti *TraderIntegration) validateAIReentryImmediatelyBeforeOrder(dec *decision.Decision) error {
	if dec == nil || ti.engine == nil || ti.engine.config == nil {
		return fmt.Errorf("%w: execution context unavailable", errAIReentryOrderPreflight)
	}
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil || cycle.Status != store.CopyGuardReentryPending {
		return fmt.Errorf("%w: cycle is no longer pending entry", errAIReentryOrderPreflight)
	}
	candidate, err := ti.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID)
	if err != nil || candidate.Status != store.ReentryCandidateEntryPending || candidate.ReentryCount != cycle.ReentryCount {
		return fmt.Errorf("%w: candidate lease is stale", errAIReentryOrderPreflight)
	}
	if cycle.ReentryCount >= ti.engine.config.RiskMaxReentries {
		return fmt.Errorf("%w: maximum reentry attempts reached", errAIReentryOrderPreflight)
	}
	if dec.PositionSizeUSD <= 0 || dec.PositionSizeUSD > candidate.MaxNotional*candidate.SizeFactor+math.Max(0.01, dec.PositionSizeUSD*1e-9) {
		return fmt.Errorf("%w: queued notional exceeds AI deterministic cap", errAIReentryOrderPreflight)
	}
	analysis, err := ti.store.ReentryAI().GetReentryAnalysis(candidate.LastAnalysisID)
	if err != nil || analysis.CandidateID != candidate.ID {
		return fmt.Errorf("%w: candidate analysis unavailable", errAIReentryOrderPreflight)
	}
	ttl := candidate.DecisionTTLSeconds
	if expired, _ := aiDecisionExpired(analysis, ttl, time.Now()); expired {
		return fmt.Errorf("%w: AI result expired before exchange submission", errAIReentryOrderPreflight)
	}
	leader := ti.engine.buildLeaderPosMap()[candidate.LeaderPosID]
	if leader == nil || leader.Size <= 0 || (leader.Side != "" && string(leader.Side) != candidate.Side) {
		return fmt.Errorf("%w: leader position closed or reversed", errAIReentryOrderPreflight)
	}
	price := leader.MarkPrice
	if mgr, ok := ti.executor.(StopLossManager); ok {
		if current, priceErr := mgr.GetMarketPrice(candidate.Symbol); priceErr == nil && current > 0 {
			price = current
		}
	}
	if price <= 0 || price < candidate.EntryPriceLow || price > candidate.EntryPriceHigh || candidate.ATR <= 0 || math.Abs(price-analysis.SnapshotPrice) > 0.25*candidate.ATR {
		return fmt.Errorf("%w: price left the approved range or drift limit", errAIReentryOrderPreflight)
	}
	positions, err := ti.getFreshPositions()
	if err != nil {
		return fmt.Errorf("%w: follower position refresh failed: %v", errAIReentryOrderPreflight, err)
	}
	for _, pos := range positions {
		if getStringField(pos, "symbol") == candidate.Symbol && strings.EqualFold(getStringField(pos, "side"), candidate.Side) && math.Abs(getFloatField(pos, "positionAmt", "quantity")) > 0 {
			return fmt.Errorf("%w: follower already has the candidate position", errAIReentryOrderPreflight)
		}
	}
	return nil
}

func isAmbiguousReentryExecutionError(err error) bool {
	if err == nil || errors.Is(err, errAIReentryOrderPreflight) || isRetryableExecutionError(err) {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, token := range []string{"timeout", "deadline exceeded", "context canceled", "unexpected eof", "connection reset", "broken pipe", "network is unreachable", "temporarily unavailable", "empty response"} {
		if strings.Contains(message, token) {
			return true
		}
	}
	return false
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
		Category:   "copy_trade",
		TraderID:   ti.traderID,
		TraderName: traderName,
		Title:      fmt.Sprintf("%s | 跟单成功 %s %s", traderName, dec.Action, dec.Symbol),
		Body:       ti.buildCopyActionAlertBody(dec, traderName, durationMs),
		RateKey:    rateKey,
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
	return ti.store.Trader().ResolveDisplayName(ti.traderID)
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

	// 统一跟单事件日志（Seam A）：动作事件（开仓/加仓/减仓/平仓，全 provider）。
	// 复用本唯一入口，避免散落埋点；best-effort，绝不影响上游流程。
	ti.recordActionEvent(dec, status, errorMsg, log.SignalID)
}

// recordCopyEvent 统一跟单事件写入器（best-effort）。
// 写入失败仅告警、绝不上抛，保证交易主链路零影响。
func (ti *TraderIntegration) recordCopyEvent(e *store.CopyTradeEvent) {
	if ti.store == nil || e == nil {
		return
	}
	if e.TraderID == "" {
		e.TraderID = ti.traderID
	}
	if e.LeaderID == "" && ti.engine != nil && ti.engine.config != nil {
		e.LeaderID = ti.engine.config.LeaderID
	}
	if e.ProviderType == "" && ti.engine != nil && ti.engine.config != nil {
		e.ProviderType = string(ti.engine.config.ProviderType)
	}
	if err := ti.store.CopyTrade().LogCopyEvent(e); err != nil {
		logger.Warnf("⚠️ [%s] 保存跟单事件日志失败: %v", ti.traderID, err)
	}
}

// recordActionEvent 把一次跟单执行结果落成统一动作事件。
// status ∈ {executed, skipped, silent_close, failed}（saveSignalLog 的四个来源）。
func (ti *TraderIntegration) recordActionEvent(dec *decision.Decision, status, errorMsg, signalID string) {
	eventType, side := classifyCopyAction(dec.Action)
	if eventType == "" {
		return // hold/wait 等非动作决策不记录
	}

	// 开仓 vs 加仓：执行成功前本地已有 active 映射即为加仓（与 updatePositionMapping 同口径）。
	// 二次入场（reentry）视为重新开仓，其重入语义由 Seam B 的 reentry 事件单独记录。
	if eventType == store.CopyEventTypeOpen && dec.LeaderPosID != "" &&
		!strings.Contains(dec.Reasoning, "reentry") {
		if m, err := ti.store.CopyTrade().GetActiveMapping(ti.traderID, dec.LeaderPosID); err == nil && m != nil {
			eventType = store.CopyEventTypeAdd
		}
	}

	evStatus, severity := "success", store.CopyEventSeverityInfo
	switch status {
	case "executed":
		evStatus, severity = "success", store.CopyEventSeverityInfo
	case "skipped":
		evStatus, severity = "skipped", store.CopyEventSeverityInfo
	case "silent_close":
		// 良性 close 自愈（本地仓位已不存在），非失败也非常态。
		evStatus, severity = "success", store.CopyEventSeverityWarn
	case "failed":
		evStatus, severity = "failed", store.CopyEventSeverityError
	}

	// 幂等键：仅在有领航员 fill ID 时启用（引擎瞬态失败重放同一 fill 的相同结果去重）。
	dedup := ""
	if dec.SourceFillID != "" {
		dedup = fmt.Sprintf("a|%s|%s|%s|%s", ti.traderID, dec.SourceFillID, eventType, evStatus)
	}

	ti.recordCopyEvent(&store.CopyTradeEvent{
		Category:    store.CopyEventCategoryAction,
		EventType:   eventType,
		Severity:    severity,
		Symbol:      dec.Symbol,
		Side:        side,
		MarginMode:  dec.MarginMode,
		LeaderPosID: dec.LeaderPosID,
		SignalID:    signalID,
		Status:      evStatus,
		Price:       dec.EntryPrice,
		Notional:    dec.PositionSizeUSD,
		Summary:     buildActionSummary(eventType, dec, evStatus, errorMsg),
		Detail:      buildActionDetail(dec, errorMsg),
		DedupKey:    dedup,
	})
}

// classifyCopyAction 把决策 action 归一化为动作事件类型与方向。
func classifyCopyAction(action string) (eventType, side string) {
	switch action {
	case "open_long":
		return store.CopyEventTypeOpen, "long"
	case "open_short":
		return store.CopyEventTypeOpen, "short"
	case "reduce_long":
		return store.CopyEventTypeReduce, "long"
	case "reduce_short":
		return store.CopyEventTypeReduce, "short"
	case "close_long":
		return store.CopyEventTypeClose, "long"
	case "close_short":
		return store.CopyEventTypeClose, "short"
	}
	return "", ""
}

func buildActionSummary(eventType string, dec *decision.Decision, evStatus, errorMsg string) string {
	label := map[string]string{
		store.CopyEventTypeOpen:   "开仓",
		store.CopyEventTypeAdd:    "加仓",
		store.CopyEventTypeReduce: "减仓",
		store.CopyEventTypeClose:  "平仓",
	}[eventType]
	switch evStatus {
	case "failed":
		return fmt.Sprintf("%s %s 执行失败: %s", label, dec.Symbol, errorMsg)
	case "skipped":
		return fmt.Sprintf("%s %s 跳过（未下单）", label, dec.Symbol)
	default:
		if dec.PositionSizeUSD > 0 {
			return fmt.Sprintf("%s %s %.2f USDT", label, dec.Symbol, dec.PositionSizeUSD)
		}
		return fmt.Sprintf("%s %s", label, dec.Symbol)
	}
}

func buildActionDetail(dec *decision.Decision, errorMsg string) map[string]interface{} {
	d := map[string]interface{}{}
	if dec.ExecutionIntentID > 0 {
		d["intent_id"] = dec.ExecutionIntentID
	}
	if dec.SourceFillID != "" {
		d["source_fill_id"] = dec.SourceFillID
	}
	if dec.LeaderPosID != "" {
		d["leader_pos_id"] = dec.LeaderPosID
	}
	if dec.SourceRevision > 0 {
		d["source_revision"] = dec.SourceRevision
	}
	if dec.RequestedQuantity > 0 {
		d["requested_quantity"] = dec.RequestedQuantity
	}
	if dec.QuantizedQuantity > 0 {
		d["quantized_quantity"] = dec.QuantizedQuantity
	}
	if dec.FilledQuantity > 0 {
		d["filled_quantity"] = dec.FilledQuantity
	}
	if dec.ExecutionStatus != "" {
		d["final_status"] = dec.ExecutionStatus
	}
	if dec.ExecutionReasonCode != "" {
		d["reason_code"] = dec.ExecutionReasonCode
	}
	if dec.Leverage > 0 {
		d["leverage"] = dec.Leverage
	}
	if dec.CloseRatio > 0 {
		d["close_ratio"] = dec.CloseRatio
	}
	if dec.LeaderPosSize > 0 {
		d["leader_pos_size"] = dec.LeaderPosSize
	}
	if dec.ExchangeOrderID != "" {
		d["exchange_order_id"] = dec.ExchangeOrderID
	}
	if dec.SourceSymbol != "" {
		d["source_symbol"] = dec.SourceSymbol
	}
	if dec.ExecutionSymbol != "" {
		d["execution_symbol"] = dec.ExecutionSymbol
	}
	if dec.ValueCurrency != "" {
		d["value_currency"] = dec.ValueCurrency
	}
	if dec.ExecutionSettleAsset != "" {
		d["execution_settle_asset"] = dec.ExecutionSettleAsset
	}
	if errorMsg != "" {
		d["error"] = errorMsg
	}
	if len(d) == 0 {
		return nil
	}
	return d
}

// commitCopyGuardReentryFill is the post-exchange transactional commit for a
// reentry. The store changes the stopped mapping, cycle and new attempt in one
// transaction so protection is never built against a half-committed lifecycle.
func (ti *TraderIntegration) commitCopyGuardReentryFill(dec *decision.Decision) error {
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil {
		return err
	}
	quantity := 0.0
	if dec.EntryPrice > 0 {
		quantity = dec.PositionSizeUSD / dec.EntryPrice
	}
	metadata := map[string]interface{}{
		"activate_mapping":  true,
		"leader_size":       dec.LeaderPosSize,
		"client_order_id":   dec.ClientOrderID,
		"exchange_order_id": dec.ExchangeOrderID,
	}
	if err = ti.store.CopyTrade().RecordCopyGuardReentryFilled(cycle, dec.EntryPrice, dec.PositionSizeUSD, quantity, 0, metadata); err != nil {
		return err
	}
	ti.markManualReentryOutcome(cycle.ID, store.ManualReentryStatusExecuted, "")
	return nil
}

// handleReentryLifecycleCommitFailure handles the exceptional case where the
// exchange filled an entry but the local lifecycle transaction could not be
// committed. It never continues to protection using a stale attempt number.
// The position is closed, the candidate is terminal, and the in-flight risk is
// retained as consumed rather than incorrectly returned to the live budget.
func (ti *TraderIntegration) handleReentryLifecycleCommitFailure(dec *decision.Decision, cause error) {
	cycle, cycleErr := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if cycleErr != nil {
		logger.Errorf("[CopyGuard] trader=%s event=REENTRY_LIFECYCLE_COMMIT_FAILED reason=%v lifecycle_error=%v", ti.traderID, cause, cycleErr)
		return
	}
	attemptNo := cycle.ReentryCount + 1
	reason := "post-fill lifecycle commit failed: " + cause.Error()
	metadata := map[string]interface{}{"attempt_no": attemptNo, "client_order_id": dec.ClientOrderID, "exchange_order_id": dec.ExchangeOrderID, "error": cause.Error()}
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERY_PENDING", Price: dec.EntryPrice, Notional: dec.PositionSizeUSD, Metadata: metadata})

	closer, ok := ti.executor.(EmergencyPositionCloser)
	if !ok {
		metadata["forced_exit_error"] = "executor does not support emergency close"
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "GUARD_FORCED_EXIT_FAILED", Metadata: metadata})
		ti.notifyProtection(cycle, "AI 重入成交账本提交失败", reason+"；执行器不支持紧急平仓，系统已冻结本候选，禁止重复入场。", fmt.Sprintf("reentry_commit_failed_%d", attemptNo))
		return
	}
	if _, closeErr := closer.ClosePositionMarket(cycle.Symbol, cycle.Side); closeErr != nil {
		metadata["forced_exit_error"] = closeErr.Error()
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "GUARD_FORCED_EXIT_FAILED", Metadata: metadata})
		ti.notifyProtection(cycle, "AI 重入成交账本提交失败且退出失败", reason+"；强制退出失败: "+closeErr.Error(), fmt.Sprintf("reentry_commit_exit_failed_%d", attemptNo))
		return
	}
	if qty, known := ti.followerPositionQuantity(cycle.Symbol, cycle.Side, cycle.MarginMode, true); !known || qty > 0 {
		metadata["residual_quantity"] = qty
		metadata["forced_exit_error"] = "position flatness not confirmed"
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "GUARD_FORCED_EXIT_FAILED", Metadata: metadata})
		ti.notifyProtection(cycle, "AI 重入异常退出待确认", reason+"；退出后未能确认仓位归零，系统保持冻结并禁止重复入场。", fmt.Sprintf("reentry_commit_flat_unknown_%d", attemptNo))
		return
	}
	_ = ti.store.ReentryAI().ConsumeCopyGuardRisk(cycle.ID, attemptNo)
	if candidate, candidateErr := ti.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID); candidateErr == nil {
		metadata["candidate_id"] = candidate.ID
		_ = ti.store.ReentryAI().MarkReentryCandidateStatus(candidate.ID, store.ReentryCandidateInvalidated, reason)
	}
	status := store.CopyGuardStoppedWatching
	if ti.engine.config.RiskReentryDecisionMode == "ai_guarded" {
		status = store.CopyGuardAIAbandoned
	}
	_ = ti.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, status, cycle.LeaderEntryPrice, cycle.LastObservedPrice, 0)
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "FORCED_EXIT", Price: dec.EntryPrice, Notional: dec.PositionSizeUSD, Metadata: metadata})
	ti.notifyProtection(cycle, "AI 重入异常已强制退出", reason+"；仓位已确认归零，本候选已终止。", fmt.Sprintf("reentry_commit_forced_exit_%d", attemptNo))
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
			if SupportsCopyGuard(ti.engine.config.ProviderType) && ti.engine.config.RiskPolicyVersion >= 4 {
				if oldCycle, cycleErr := copyTradeStore.GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID); cycleErr == nil {
					if mgr, ok := ti.executor.(ProtectiveStopManagerV4); ok {
						if order, orderErr := copyTradeStore.GetCopyGuardProtectiveOrder(oldCycle.ID); orderErr == nil {
							if cancelErr := ti.cancelProtectiveOrderForCycle(mgr, oldCycle, order); cancelErr != nil {
								ti.markProtectionIssue(oldCycle, store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN", cancelErr, oldCycle.ProtectionCoverage, false)
							}
						}
					}
					emitWatchSummary(copyTradeStore, ti.traderID, oldCycle, dec.EntryPrice)
					_ = copyTradeStore.CloseCopyGuardCycle(oldCycle.ID, store.CopyGuardLeaderReversed, oldCycle.ActualPnL, oldCycle.BaselinePnL, oldCycle.Fees, oldCycle.FundingFee, oldCycle.LiquidationPenalty, oldCycle.Slippage)
					_ = copyTradeStore.SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: oldCycle.ID, TraderID: ti.traderID, Type: "LEADER_REVERSED", Price: dec.EntryPrice, Metadata: map[string]interface{}{"old_side": oldCycle.Side, "new_side": expectedSide}})
				}
			}
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
				SourceSymbol:  dec.SourceSymbol, ExecutionSymbol: dec.ExecutionSymbol,
				SourceQuoteAsset: dec.ValueCurrency, ExecutionSettleAsset: dec.ExecutionSettleAsset,
			}

			if err := copyTradeStore.SavePositionMapping(mapping); err != nil {
				logger.Warnf("⚠️ [%s] 保存仓位映射失败: %v", ti.traderID, err)
			} else {
				logger.Infof("📝 [%s] 仓位映射已保存 | posId=%s %s %s %s lastKnownSize=%.4f",
					ti.traderID, dec.LeaderPosID, dec.Symbol, expectedSide, dec.MarginMode, dec.LeaderPosSize)
				if SupportsCopyGuard(ti.engine.config.ProviderType) && ti.engine.config.RiskPolicyVersion >= 4 && ti.engine.config.RiskStopLossEnabled {
					policyJSON, _ := json.Marshal(ti.engine.config)
					cycle, cerr := copyTradeStore.EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: ti.traderID, LeaderID: ti.engine.config.LeaderID, LeaderPosID: dec.LeaderPosID, Symbol: dec.Symbol, Side: expectedSide, MarginMode: dec.MarginMode, Status: store.CopyGuardFollowing, PolicySnapshot: string(policyJSON), LeaderEntryPrice: dec.EntryPrice, FollowerEntryPrice: dec.EntryPrice, FollowerNotional: dec.PositionSizeUSD, BaselineLeaderSize: dec.LeaderPosSize, AccountEquity: ti.getEquityFunc()(), LastObservedPrice: dec.EntryPrice})
					if cerr != nil {
						logger.Warnf("⚠️ [%s] 创建 Copy Guard 生命周期失败: %v", ti.traderID, cerr)
					} else {
						_ = copyTradeStore.SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "INITIAL_ENTRY_FILLED", Price: dec.EntryPrice, Notional: dec.PositionSizeUSD})
						_ = copyTradeStore.OpenCopyGuardAttempt(cycle.ID, 0, dec.EntryPrice, dec.PositionSizeUSD, 0, 0)
					}
				}
			}
		}
		if SupportsCopyGuard(ti.engine.config.ProviderType) && ti.engine.config.RiskPolicyVersion >= 4 && dec.ExchangeOrderID != "" {
			if cycle, cycleErr := copyTradeStore.GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID); cycleErr == nil {
				_ = copyTradeStore.UpdateCopyGuardExecutionOrder(cycle.ID, dec.ExchangeOrderID, "")
				_ = copyTradeStore.UpdateCopyGuardAttemptIdentity(cycle.ID, cycle.ReentryCount, "", dec.ExchangeOrderID, "")
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
		if SupportsCopyGuard(ti.engine.config.ProviderType) && ti.engine.config.RiskPolicyVersion >= 4 && dec.ExchangeOrderID != "" {
			if cycle, cycleErr := copyTradeStore.GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID); cycleErr == nil {
				_ = copyTradeStore.UpdateCopyGuardExecutionOrder(cycle.ID, "", dec.ExchangeOrderID)
			}
		}

	case "close_long", "close_short":
		if SupportsCopyGuard(ti.engine.config.ProviderType) && ti.engine.config.RiskPolicyVersion >= 4 {
			ti.finalizeCopyGuardCycle(dec)
		}
		// 平仓：关闭映射
		if err := copyTradeStore.CloseMapping(ti.traderID, dec.LeaderPosID, dec.EntryPrice); err != nil {
			logger.Warnf("⚠️ [%s] 关闭仓位映射失败: %v", ti.traderID, err)
		} else {
			logger.Infof("📝 [%s] 仓位映射已关闭 | posId=%s %s",
				ti.traderID, dec.LeaderPosID, dec.Symbol)
		}
	}
}

func (ti *TraderIntegration) finalizeCopyGuardCycle(dec *decision.Decision) {
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil {
		return
	}
	if mgr, ok := ti.executor.(ProtectiveStopManagerV4); ok {
		if order, e := ti.store.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID); e == nil {
			if cancelErr := ti.cancelProtectiveOrderForCycle(mgr, cycle, order); cancelErr == nil {
				_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionCanceled, 0, "", cycle.FollowerPosID, cycle.EntryOrderID, false)
			} else {
				ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN", cancelErr, cycle.ProtectionCoverage, false)
			}
		}
	}
	// own-path 口径：每个 attempt 按自身名义持有到领航员平仓价的反事实盈亏。
	// attempt 数据不完整时回退旧口径（影子名义），保证基线始终有值。
	attempts, _ := ti.store.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	baseline, ok := ComputeOwnPathBaseline(cycle, attempts, dec.EntryPrice)
	if !ok {
		baseline = cycle.BaselineRealizedPnL
		if cycle.LeaderEntryPrice > 0 {
			move := (dec.EntryPrice - cycle.LeaderEntryPrice) / cycle.LeaderEntryPrice
			if cycle.Side == "short" {
				move = -move
			}
			baseline += cycle.BaselineNotional * move
		}
	}
	// 观察期收尾统计（挽回/错过、门控占比等）须在周期关闭前写入；
	// 领航员平仓价即 dec.EntryPrice（close 信号的成交价）
	emitWatchSummary(ti.store.CopyTrade(), ti.traderID, cycle, dec.EntryPrice)
	if err := ti.store.CopyTrade().BeginCopyGuardAccounting(cycle.ID, store.CopyGuardLeaderClosed, dec.ExchangeOrderID, baseline); err != nil {
		logger.Errorf("❌ [%s] failed to begin Copy Guard accounting cycle=%d: %v", ti.traderID, cycle.ID, err)
		return
	}
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "LEADER_CLOSED", Price: dec.EntryPrice, Metadata: map[string]interface{}{"baseline_pnl": baseline, "exit_order_id": dec.ExchangeOrderID, "accounting_status": store.CopyGuardAccountingPending}})
	if pending, err := ti.store.CopyTrade().GetCopyGuardCycle(cycle.ID); err == nil {
		ti.reconcileV4CycleAccounting(pending)
	}
}

func (ti *TraderIntegration) captureV4FollowerBeforeClose(dec *decision.Decision) {
	if dec == nil || ti.engine == nil || ti.engine.config == nil || !SupportsCopyGuard(ti.engine.config.ProviderType) || ti.engine.config.RiskPolicyVersion < 4 {
		return
	}
	if dec.Action != "close_long" && dec.Action != "close_short" {
		return
	}
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil {
		return
	}
	positions, err := ti.getFreshPositions()
	if err != nil {
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "ACCOUNTING_IDENTITY_CAPTURE_FAILED", Metadata: map[string]interface{}{"error": err.Error()}})
		return
	}
	for _, pos := range positions {
		if getStringField(pos, "symbol") != cycle.Symbol || !strings.EqualFold(getStringField(pos, "side"), cycle.Side) {
			continue
		}
		mode := getStringField(pos, "mgnMode", "marginMode")
		if cycle.MarginMode != "" && mode != "" && mode != cycle.MarginMode {
			continue
		}
		entry := getFloatField(pos, "entryPrice", "entry_price")
		quantity := absFloat(getFloatField(pos, "positionAmt", "quantity"))
		posID := getStringField(pos, "posId")
		_ = ti.store.CopyTrade().UpdateCopyGuardFollowerPosition(cycle.ID, posID, entry, entry*quantity)
		_ = ti.store.CopyTrade().UpdateCopyGuardAttemptPosition(cycle.ID, cycle.ReentryCount, entry, entry*quantity, quantity, cycle.ATRAtEntry)
		_ = ti.store.CopyTrade().UpdateCopyGuardAttemptIdentity(cycle.ID, cycle.ReentryCount, posID, "", "")
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "ACCOUNTING_IDENTITY_CAPTURED", Price: entry, Quantity: quantity, Metadata: map[string]interface{}{"follower_pos_id": posID}})
		return
	}
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "ACCOUNTING_IDENTITY_CAPTURE_FAILED", Metadata: map[string]interface{}{"error": "fresh follower position not found"}})
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

// getEquityFunc is intentionally separate from available balance. Margin already in use must not
// make an existing position's risk denominator shrink.
func (ti *TraderIntegration) getEquityFunc() func() float64 {
	return func() float64 {
		info, err := ti.executor.GetAccountInfo()
		if err != nil {
			logger.Warnf("⚠️ [%s] 获取账户权益失败: %v", ti.traderID, err)
			return 0
		}
		for _, key := range []string{"total_equity", "totalEquity", "totalWalletBalance", "wallet_balance"} {
			if equity, ok := info[key].(float64); ok && equity > 0 {
				return equity
			}
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

func (ti *TraderIntegration) getPositionsResultFunc() func() (map[string]*Position, error) {
	return func() (map[string]*Position, error) {
		exchangePositions, err := ti.executor.GetPositions()
		if err != nil {
			return nil, err
		}
		return convertFollowerPositions(exchangePositions), nil
	}
}

func convertFollowerPositions(exchangePositions []map[string]interface{}) map[string]*Position {
	positions := make(map[string]*Position)
	for _, pos := range exchangePositions {
		symbol, _ := pos["symbol"].(string)
		sideStr, _ := pos["side"].(string)
		quantity := getFloatField(pos, "positionAmt", "quantity")
		if quantity == 0 {
			continue
		}
		side := SideLong
		if sideStr == "short" || sideStr == "sell" {
			side = SideShort
		}
		marginMode := getStringField(pos, "marginMode", "mgnMode")
		posID := getStringField(pos, "posId")
		key := posID
		if key == "" {
			key = PositionKeyWithMode(symbol, side, marginMode)
		}
		mark := getFloatField(pos, "markPrice", "mark_price")
		positions[key] = &Position{Symbol: symbol, Side: side, Size: absFloat(quantity), EntryPrice: getFloatField(pos, "entryPrice", "entry_price"), MarkPrice: mark, Leverage: getIntOrFloatField(pos, "leverage"), MarginMode: marginMode, UnrealizedPnL: getFloatField(pos, "unRealizedProfit", "unrealized_pnl"), PositionValue: absFloat(quantity) * mark, PosID: posID}
	}
	return positions
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
// Copy Guard 保护单管理钩子（v5）
//
// 设计原则：
//  1. SupportsCopyGuard 数据源生效（OKX/Binance；HL 不支持账户保护 SL）
//  2. 任何 SL 操作失败都不阻断主交易流程，但必须进入可保护性状态机
//     （clamp → GUARD_UNPROTECTABLE → close/follow），禁止静默裸跑
// ============================================================================

// shouldManageStopLoss 判断当前决策是否走 Copy Guard 保护单管理路径
// 返回 false 时跳过所有 SL 钩子（透明降级）
func (ti *TraderIntegration) shouldManageStopLoss(dec *decision.Decision) bool {
	if ti.engine == nil || ti.engine.config == nil {
		return false
	}
	if !SupportsCopyGuard(ti.engine.config.ProviderType) {
		return false
	}
	// Copy Guard protective stops are a v4+ feature: cycles are only created for
	// v4 configs. Gating on the version here keeps this in lockstep with cycle
	// creation so we never try to attach a protective stop to a lifecycle that
	// was never opened (legacy configs, e.g. a Binance follower whose stored
	// risk_stop_loss_enabled defaults to true but risk_policy_version is 0).
	// For OKX this is a no-op: an OKX follower with stop loss enabled is always
	// upgraded to v4 (upgradeLegacyRiskPolicy).
	if ti.engine.config.RiskPolicyVersion < 4 {
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

// refreshStopLossAfterExecute 执行成功后用实际成交均价精确重挂保护单
//
// 调用时机：跟单开仓/加仓/部分减仓执行成功 + mapping 更新完成后（Copy Guard 数据源）
// 平仓（close_long/close_short）不重挂（仓位已清空，只撤本周期保护单）
//
// 实现细节：
//  1. 通过 GetPositionsFresh() 拿到最新本地持仓的 EntryPrice / Quantity
//  2. 用 calcStopLossPrice 算 SL 价（含 clamp / 不可保护判定）
//  3. 经 upsertV4Protection 状态机挂 algo 单并验证
//
// 失败处理：进入可保护性状态机（重试退避 → 封顶后 GUARD_UNPROTECTABLE 处置），
// 禁止静默裸跑
func (ti *TraderIntegration) refreshStopLossAfterExecute(dec *decision.Decision) {
	if !ti.shouldManageStopLoss(dec) {
		return
	}

	// close: cancel only the order belonging to this lifecycle, after close succeeds.
	switch dec.Action {
	case "close_long", "close_short":
		ti.cancelV4Protection(dec)
		return
	}

	// 拿最新持仓
	positions, err := ti.getFreshPositions()
	if err != nil {
		logger.Warnf("⚠️ [%s] 重挂 SL 失败（GetPositions 错误）: %v | %s", ti.traderID, err, dec.Symbol)
		ti.markProtectionIssueForDecision(dec, store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN", err, 0)
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
		// 仅在任一侧缺失 marginMode 时降级，不能拿另一个保证金模式的仓位挂保护。
		if matchedPos == nil && (dec.MarginMode == "" || marginMode == "") {
			matchedPos = pos
		}
	}

	if matchedPos == nil {
		logger.Warnf("⚠️ [%s] 重挂 SL：找不到本地仓位 | %s %s posId=%s", ti.traderID, dec.Symbol, expectedSide, dec.LeaderPosID)
		ti.markProtectionIssueForDecision(dec, store.CopyGuardProtectionDegraded, "PROTECTION_DEGRADED", fmt.Errorf("fresh follower position not found"), 0)
		return
	}

	entryPrice := getFloatField(matchedPos, "entryPrice", "entry_price")
	quantity := absFloat(getFloatField(matchedPos, "positionAmt", "quantity"))
	liquidationPrice := getFloatField(matchedPos, "liquidationPrice", "liquidation_price")
	leverage := getIntOrFloatField(matchedPos, "leverage")
	if leverage <= 0 {
		leverage = dec.Leverage
	}
	if entryPrice <= 0 || quantity <= 0 {
		logger.Warnf("⚠️ [%s] 重挂 SL：本地仓位数据异常 | entry=%.4f qty=%.4f", ti.traderID, entryPrice, quantity)
		ti.markProtectionIssueForDecision(dec, store.CopyGuardProtectionDegraded, "PROTECTION_DEGRADED", fmt.Errorf("invalid fresh position entry or quantity"), 0)
		return
	}

	followerEquity := ti.getEquityFunc()()
	if followerEquity <= 0 {
		logger.Warnf("⚠️ [%s] 重挂 SL：跟随者权益为零", ti.traderID)
		ti.markProtectionIssueForDecision(dec, store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN", fmt.Errorf("account equity unavailable"), 0)
		return
	}

	side := SideLong
	if expectedSide == "short" {
		side = SideShort
	}

	slInput := &StopLossCalcInput{
		Symbol:           dec.Symbol,
		Side:             side,
		EntryPrice:       entryPrice,
		Leverage:         leverage,
		PositionValue:    entryPrice * quantity,
		FollowerEquity:   followerEquity,
		LiquidationPrice: liquidationPrice,
	}
	// Protective order precision belongs to the execution contract, not the
	// source venue. AutoTrader exposes the underlying Binance/OKX resolver, so
	// BTCUSDC and BTCUSD1 retain their exact settlement asset and tick/step.
	if resolver, ok := ti.executor.(trader.ExecutionInstrumentResolver); ok {
		inst, resolveErr := resolver.ResolveExecutionInstrument(dec.Symbol)
		if resolveErr != nil || inst == nil || inst.PriceTickSize <= 0 || inst.BaseQuantityStep <= 0 {
			if resolveErr == nil {
				resolveErr = fmt.Errorf("execution instrument precision is incomplete")
			}
			ti.handleUnprotectableForDecision(dec, expectedSide, quantity, entryPrice, fmt.Errorf("resolve protective execution contract: %w", resolveErr))
			return
		}
		slInput.PriceTickSize = inst.PriceTickSize
		slInput.BaseQuantityStep = inst.BaseQuantityStep
	}

	slResult, err := calcStopLossPrice(ti.engine.config, slInput)
	if err != nil {
		logger.Warnf("⚠️ [%s] 重挂 SL：算法失败: %v | %s", ti.traderID, err, dec.Symbol)
		if ti.engine.config.RiskReentryDecisionMode == "ai_guarded" {
			ti.handleUnprotectableForDecision(dec, expectedSide, quantity, entryPrice, err)
			return
		}
		ti.markProtectionIssueForDecision(dec, store.CopyGuardProtectionDegraded, "PROTECTION_DEGRADED", err, 0)
		return
	}
	// AI-guarded entries, add-ons and reentries share the same actual-fill
	// ProtectionPlan. The preflight plan is only an estimate; after execution we
	// recompute from the real average entry and current completed-candle
	// structure. A wider valid stop shrinks/rejects the position, never gets
	// squeezed into market noise.
	if ti.engine.config.RiskReentryDecisionMode == "ai_guarded" {
		structure := structuralInvalidationPrice(dec.Symbol, side, entryPrice)
		planATR := slResult.ATRValue
		if planATR <= 0 && ti.engine.config.RiskATRMultiplier > 0 {
			planATR = slResult.ATRDistance / ti.engine.config.RiskATRMultiplier
		}
		cycle, cycleErr := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
		if cycleErr != nil {
			ti.handleUnprotectableForDecision(dec, expectedSide, quantity, entryPrice, fmt.Errorf("risk cycle unavailable after fill: %w", cycleErr))
			return
		}
		usage, usageErr := ti.store.ReentryAI().GetCopyGuardRiskUsageExcludingAttempt(ti.traderID, cycle.ID, cycle.ReentryCount)
		availableRisk, capacityErr := AvailableCopyGuardRiskUSD(ti.engine.config, followerEquity, usage)
		if dec.QuantityStepOverride {
			availableRisk, capacityErr = AvailableCopyGuardFollowFirstRiskUSD(ti.engine.config, followerEquity, usage)
		}
		if usageErr != nil || capacityErr != nil {
			if usageErr != nil {
				capacityErr = usageErr
			}
			ti.handleUnprotectableForDecision(dec, expectedSide, quantity, entryPrice, fmt.Errorf("risk capacity unavailable after fill: %w", capacityErr))
			return
		}
		plan, planErr := BuildProtectionPlan(ti.engine.config, side, entryPrice, planATR, structure, followerEquity, availableRisk/followerEquity, slInput.PositionValue)
		if planErr != nil || plan.MaxNotional+math.Max(0.01, slInput.PositionValue*0.001) < slInput.PositionValue {
			if planErr == nil {
				planErr = fmt.Errorf("actual notional %.4f exceeds protected risk size %.4f", slInput.PositionValue, plan.MaxNotional)
			}
			ti.handleUnprotectableForDecision(dec, expectedSide, quantity, entryPrice, planErr)
			return
		}
		governedBy := "atr"
		if plan.StructureInvalidation > 0 && plan.StopDistance > slResult.ATRDistance+1e-9 {
			governedBy = "structure"
		}
		friction := (ti.engine.config.RiskSlippageBufferBPS + ti.engine.config.RiskRoundTripFeeBPS) / 10000
		expectedLoss := slInput.PositionValue * (plan.StopDistance/entryPrice + friction)
		slResult.SLDistance = plan.StopDistance
		slResult.ATRDistance = plan.ATR * ti.engine.config.RiskATRMultiplier
		slResult.ATRValue = plan.ATR
		slResult.GovernedBy = governedBy
		slResult.ExpectedLossUSD = expectedLoss
		slResult.ExpectedLossPct = expectedLoss / followerEquity
		if margin := slInput.PositionValue / float64(leverage); margin > 0 {
			slResult.ExpectedMarginLossPct = expectedLoss / margin
		}
		slResult.DistanceATRRatio = plan.StopDistance / plan.ATR
		slResult, err = finalizeStopLossPrice(slInput, slResult, ti.engine.config.RiskLiquidationBufferATR)
		if err != nil {
			ti.handleUnprotectableForDecision(dec, expectedSide, quantity, entryPrice, err)
			return
		}
	}
	// v5 可保护性状态机：算不出可挂的止损价（clamp 后仍不可行 / 开仓即触发）
	// → 不再无限重试，直接进入 GUARD_UNPROTECTABLE 处置（按配置平仓或标红裸跑）
	if slResult.SLPrice <= 0 {
		cause := fmt.Errorf("no valid protective trigger price")
		if slResult.Unprotectable {
			cause = fmt.Errorf("stop price inside liquidation buffer even after clamp (liq=%.4f)", liquidationPrice)
		} else if slResult.OpenImmediateHit {
			cause = fmt.Errorf("stop distance below 0.1%% of entry (entry=%.4f)", entryPrice)
		}
		if slResult.Unprotectable || slResult.OpenImmediateHit {
			ti.handleUnprotectableForDecision(dec, expectedSide, quantity, entryPrice, cause)
			return
		}
		ti.markProtectionIssueForDecision(dec, store.CopyGuardProtectionDegraded, "PROTECTION_DEGRADED", cause, 0)
		return
	}
	if priceProvider, ok := ti.executor.(StopLossManager); ok {
		if current, priceErr := priceProvider.GetMarketPrice(dec.Symbol); priceErr == nil && current > 0 {
			crossed := (expectedSide == "long" && current <= slResult.SLPrice) || (expectedSide == "short" && current >= slResult.SLPrice)
			if crossed {
				ti.handleUnprotectableForDecision(dec, expectedSide, quantity, entryPrice, fmt.Errorf("stop %.8f already crossed by current price %.8f", slResult.SLPrice, current))
				return
			}
		}
	}
	if slResult.LiquidationPriceIgnored {
		logger.Warnf("⚠️ [%s] 强平价方向异常已忽略 | %s %s entry=%.4f liq=%.4f（继续按 ATR 挂保护单）", ti.traderID, dec.Symbol, expectedSide, entryPrice, liquidationPrice)
		if cycle, cycleErr := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID); cycleErr == nil {
			_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "LIQ_PRICE_IGNORED", Price: entryPrice, Metadata: map[string]interface{}{"liquidation_price": liquidationPrice, "side": expectedSide, "reason": "direction implausible"}})
		}
	}
	ti.upsertV4Protection(dec, expectedSide, quantity, entryPrice, slResult)
}

// enforceAIReentryProtection is the final commit barrier for AI reentry. A
// filled order is not considered successful until a freshly verified stop
// covers the whole position. If protection cannot be proven synchronously,
// the deterministic layer exits immediately; the AI decision never gets to
// leave an unprotected position running.
func (ti *TraderIntegration) enforceAIReentryProtection(dec *decision.Decision) {
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil {
		return
	}
	candidate, candidateErr := ti.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID)
	protected := (cycle.ProtectionStatus == store.CopyGuardProtectionVerified || cycle.ProtectionStatus == store.CopyGuardProtectionClamped) && cycle.ProtectionCoverage >= 0.999
	if protected {
		// A protected position is safe to keep, but it is not an auditable AI
		// success unless the exact ENTRY_PENDING candidate can be committed. Do
		// not manufacture candidate=0 success mail when storage is missing.
		if candidateErr != nil || candidate == nil {
			reason := "AI reentry is protected but its candidate cannot be loaded"
			if candidateErr != nil {
				reason += ": " + candidateErr.Error()
			}
			_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERY_PENDING", Metadata: map[string]interface{}{"attempt_no": cycle.ReentryCount, "reason": reason, "protection_coverage": cycle.ProtectionCoverage}})
			ti.notifyProtection(cycle, "AI 重入保护成功但审计关联缺失", reason+"；仓位已有完整保护，系统不会重复入场，但需恢复候选关联。", fmt.Sprintf("reentry_candidate_missing_%d", cycle.ReentryCount))
			return
		}
		if candidate.Status == store.ReentryCandidateReentered {
			return
		}
		if candidate.Status != store.ReentryCandidateEntryPending {
			reason := fmt.Sprintf("protected AI reentry has unexpected candidate status %s", candidate.Status)
			_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERY_PENDING", Metadata: map[string]interface{}{"attempt_no": cycle.ReentryCount, "candidate_id": candidate.ID, "reason": reason, "protection_coverage": cycle.ProtectionCoverage}})
			ti.notifyProtection(cycle, "AI 重入候选状态异常", reason+"；仓位已有完整保护，系统不会重复入场。", fmt.Sprintf("reentry_candidate_state_%d", cycle.ReentryCount))
			return
		}
		completed, completeErr := ti.store.ReentryAI().CompleteReentryCandidate(candidate.ID)
		if completeErr != nil {
			logger.Warnf("[CopyGuard] trader=%s cycle=%d candidate=%d event=AI_CANDIDATE_COMPLETE_FAILED reason=%v", ti.traderID, cycle.ID, candidate.ID, completeErr)
			_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_RECOVERY_PENDING", Metadata: map[string]interface{}{"attempt_no": cycle.ReentryCount, "candidate_id": candidate.ID, "reason": "candidate completion failed", "error": completeErr.Error(), "protection_coverage": cycle.ProtectionCoverage}})
			ti.notifyProtection(cycle, "AI 重入已保护但候选提交失败", "仓位已有完整保护；候选完成状态落库失败，保护巡检会继续幂等重试。错误: "+completeErr.Error(), fmt.Sprintf("reentry_candidate_complete_%d", cycle.ReentryCount))
			return
		}
		if !completed {
			return
		}
		generation := candidate.DecisionGeneration
		candidateID := candidate.ID
		attemptNo := cycle.ReentryCount
		if !store.ShouldSendCopyGuardEmail(ti.engine.config.RiskNotificationLevel, "REENTRY_FILLED") {
			return
		}
		key := fmt.Sprintf("REENTRY_FILLED|%s|%d|%d|%d", ti.traderID, cycle.ID, attemptNo, generation)
		traderName := ti.traderDisplayName()
		notifier.Notify(notifier.Alert{
			Category: "copy_trade", TraderID: ti.traderID, TraderName: traderName,
			Title:   fmt.Sprintf("%s | AI 重入成交且保护生效 %s %s", traderName, cycle.Symbol, cycle.Side),
			Body:    fmt.Sprintf("Trader Name: %s\nTrader ID: %s\nCycle: %d\nAttempt: %d\nCandidate: %d\nActual Notional: %.2f USDT\nStop Price: verified on exchange\nProtection Coverage: %.2f%%", traderName, ti.traderID, cycle.ID, attemptNo, candidateID, cycle.FollowerNotional, cycle.ProtectionCoverage*100),
			RateKey: key, DedupKey: key,
			Fields: map[string]string{"TraderName": traderName, "CycleID": fmt.Sprint(cycle.ID), "AttemptNo": fmt.Sprint(attemptNo), "CandidateID": fmt.Sprint(candidateID), "Generation": fmt.Sprint(generation), "ProtectionCoverage": fmt.Sprintf("%.4f", cycle.ProtectionCoverage)},
		})
		return
	}

	reason := fmt.Sprintf("AI reentry protection not verified (status=%s coverage=%.4f)", cycle.ProtectionStatus, cycle.ProtectionCoverage)
	candidateID := int64(0)
	if candidateErr == nil && candidate != nil {
		candidateID = candidate.ID
	}
	if candidateErr == nil && candidate != nil {
		_ = ti.store.ReentryAI().MarkReentryCandidateStatus(candidate.ID, store.ReentryCandidateInvalidated, reason)
	}
	closer, ok := ti.executor.(EmergencyPositionCloser)
	if !ok {
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "GUARD_FORCED_EXIT_FAILED", Metadata: map[string]interface{}{"attempt_no": cycle.ReentryCount, "candidate_id": candidateID, "reason": reason, "error": "executor does not support emergency close"}})
		ti.notifyProtection(cycle, "AI 重入保护失败且无法自动退出", reason, fmt.Sprintf("ai_reentry_unprotected_%d", cycle.ReentryCount))
		return
	}
	if _, closeErr := closer.ClosePositionMarket(cycle.Symbol, cycle.Side); closeErr != nil {
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "GUARD_FORCED_EXIT_FAILED", Metadata: map[string]interface{}{"attempt_no": cycle.ReentryCount, "error": closeErr.Error(), "reason": reason}})
		ti.notifyProtection(cycle, "AI 重入保护失败且强制退出失败", reason+": "+closeErr.Error(), fmt.Sprintf("ai_reentry_forced_exit_failed_%d", cycle.ReentryCount))
		return
	}
	_ = ti.store.ReentryAI().ConsumeCopyGuardRisk(cycle.ID, cycle.ReentryCount)
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "FORCED_EXIT", Price: dec.EntryPrice, Notional: dec.PositionSizeUSD, Metadata: map[string]interface{}{"attempt_no": cycle.ReentryCount, "candidate_id": candidateID, "reason": reason}})
}

func (ti *TraderIntegration) enforceAIGuardedPositionProtection(dec *decision.Decision) {
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil || (cycle.Status != store.CopyGuardFollowing && cycle.Status != store.CopyGuardFollowingReentry) {
		return
	}
	if (cycle.ProtectionStatus == store.CopyGuardProtectionVerified || cycle.ProtectionStatus == store.CopyGuardProtectionClamped) && cycle.ProtectionCoverage >= 0.999 {
		return
	}
	ti.handleUnprotectableCycle(cycle, fmt.Errorf("new AI-guarded position is not fully protected (status=%s coverage=%.4f)", cycle.ProtectionStatus, cycle.ProtectionCoverage))
}

func (ti *TraderIntegration) upsertV4Protection(dec *decision.Decision, side string, quantity, entryPrice float64, result *StopLossCalcResult) {
	incrementRetry := dec.Reasoning != "Copy Guard protection retry"
	mgr, ok := ti.executor.(ProtectiveStopManagerV4)
	if !ok {
		ti.markProtectionIssueForDecision(dec, store.CopyGuardProtectionDegraded, "PROTECTION_CREATE_FAILED", fmt.Errorf("executor lacks ProtectiveStopManagerV4"), 0)
		return
	}
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil {
		logger.Errorf("❌ [%s] Copy Guard lifecycle missing: %v", ti.traderID, err)
		return
	}
	// 回填开仓时因 API 限流缺失的权益快照（account_equity=0 会让账户级
	// 保护与周期熔断判定失效）
	if cycle.AccountEquity <= 0 {
		if equity := ti.getEquityFunc()(); equity > 0 {
			if ti.store.CopyTrade().BackfillCopyGuardAccountEquity(cycle.ID, equity) == nil {
				cycle.AccountEquity = equity
			}
		}
	}
	followerPosID := ""
	if positions, posErr := ti.getFreshPositions(); posErr == nil {
		for _, pos := range positions {
			if getStringField(pos, "symbol") == dec.Symbol && getStringField(pos, "side") == side && (dec.MarginMode == "" || getStringField(pos, "mgnMode", "marginMode") == dec.MarginMode) {
				followerPosID = getStringField(pos, "posId")
				break
			}
		}
	}
	_ = ti.store.CopyTrade().UpdateCopyGuardFollowerPosition(cycle.ID, followerPosID, entryPrice, entryPrice*quantity)
	_ = ti.store.CopyTrade().UpdateCopyGuardAttemptPosition(cycle.ID, cycle.ReentryCount, entryPrice, entryPrice*quantity, quantity, result.ATRValue)
	_ = ti.store.CopyTrade().UpdateCopyGuardAttemptIdentity(cycle.ID, cycle.ReentryCount, followerPosID, cycle.EntryOrderID, "")
	// M9：upsert 每个 poll 周期都会经过这里；持续故障期间无去重会把事件表
	// 刷爆（同 cycle 27 PROTECTION_COVERAGE_LOW 前科）。复用统一去重：
	// 同 attempt + 同参数 60 秒内只落一条，参数变化（数量/止损价）立即记录。
	pendingKey := fmt.Sprintf("attempt=%d qty=%.8f stop=%.8f", cycle.ReentryCount, quantity, result.SLPrice)
	if ti.shouldRecordProtectionEvent(cycle.ID, "PROTECTION_PENDING", pendingKey) {
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "PROTECTION_PENDING", Price: result.SLPrice, Quantity: quantity, Metadata: map[string]interface{}{
			"attempt_no": cycle.ReentryCount, "planned_qty": quantity, "stop_price": result.SLPrice,
		}})
	}
	req := trader.ProtectiveStopRequest{Symbol: dec.Symbol, PositionSide: side, MarginMode: dec.MarginMode, Quantity: quantity, TriggerPrice: result.SLPrice, TriggerType: ti.engine.config.RiskTriggerPriceType, ClientID: fmt.Sprintf("cg%da%d", cycle.ID, cycle.ReentryCount)}
	stored, storedErr := ti.store.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	// 上一次替换尚未完成退休：新单已确认 live，仓位实际受保护。此时若再走
	// Place/Ensure，Binance（无 amend）每轮都会新建 closePosition 保护单并把
	// 旧单挤出跟踪，形成连环孤儿挂单。这里只推进退休流程；未完成则本轮返回，
	// 由巡检（pollV4ProtectiveStops）与后续重试继续收敛。
	if storedErr == nil && stored.ReplacementPending && stored.PreviousAlgoID != "" {
		if !ti.retryRetiringProtectiveStop(mgr, cycle, stored) {
			return
		}
		stored, storedErr = ti.store.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	}
	var live *trader.ProtectiveStopOrder

	// Resolve the actual exchange-side state before acting so we never amend a
	// triggered order, re-place a live one (51068), or invalidate tracking on a
	// transient query failure (root cause of the cycle-10 dead loop).
	amendAlgoID, amendClientID := "", ""
	var amendExisting *trader.ProtectiveStopOrder
	if storedErr == nil && stored.AlgoID != "" && stored.Status == "live" {
		actual, resolveErr := ti.resolveProtectiveOrder(mgr, stored.AlgoID, stored.AlgoClientID, dec.Symbol)
		switch {
		case resolveErr != nil:
			// State unknown: keep the tracked order untouched and retry later.
			ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN", resolveErr, protectionCoverage(stored.Quantity, quantity), incrementRetry)
			return
		case actual == nil:
			// OKX confirmed the order no longer exists: safe to rebuild.
			_ = ti.store.CopyTrade().UpdateCopyGuardProtectiveOrderStatus(cycle.ID, "invalid")
		case isProtectiveStopFired(actual.State):
			// The stop already fired between polls; let pollV4ProtectiveStops
			// record the stop instead of failing an amend here.
			logger.Infof("🛑 [%s] Copy Guard 保护单已触发(state=%s)，跳过重挂 | cycle=%d algoId=%s", ti.traderID, actual.State, cycle.ID, actual.AlgoID)
			return
		case strings.EqualFold(actual.State, "live"):
			amendAlgoID, amendClientID = actual.AlgoID, actual.ClientID
			amendExisting = actual
		default:
			// canceled / order_failed and similar terminal states: rebuild.
			_ = ti.store.CopyTrade().UpdateCopyGuardProtectiveOrderStatus(cycle.ID, strings.ToLower(actual.State))
		}
	}

	if amendAlgoID != "" {
		if ensurer, supportsEnsure := ti.executor.(trader.ProtectiveStopEnsurer); supportsEnsure {
			ensured, ensureErr := ensurer.EnsureProtectiveStop(amendExisting, req)
			err = ensureErr
			if ensured != nil {
				live = ensured.Current
				if live != nil && ensured.ReplacementPending && ensured.Retiring != nil {
					pending := &store.CopyGuardProtectiveOrder{
						CycleID: cycle.ID, TraderID: ti.traderID, AlgoID: live.AlgoID, AlgoClientID: live.ClientID,
						Symbol: dec.Symbol, Side: side, MarginMode: dec.MarginMode, Quantity: live.Quantity,
						QuantityStep: result.QuantityStep, TriggerPrice: live.TriggerPrice, TriggerType: ti.engine.config.RiskTriggerPriceType,
						Status: "live", PreviousAlgoID: ensured.Retiring.AlgoID, PreviousAlgoClientID: ensured.Retiring.ClientID, ReplacementPending: true,
					}
					persistErr := ti.store.CopyTrade().UpsertCopyGuardProtectiveOrder(pending)
					if persistErr != nil {
						if err != nil {
							err = fmt.Errorf("%v; persist replacement state: %w", err, persistErr)
						} else {
							err = fmt.Errorf("persist replacement state: %w", persistErr)
						}
					} else if !ti.retryRetiringProtectiveStop(mgr, cycle, pending) {
						// Both ids are durable. Keep the replacement pending and let
						// the poller finish retirement without creating another order.
						return
					}
				}
			}
		} else {
			err = mgr.AmendProtectiveStop(amendAlgoID, req)
			if err == nil {
				live = &trader.ProtectiveStopOrder{AlgoID: amendAlgoID, ClientID: amendClientID, State: "live"}
			}
		}
	} else {
		live, err = mgr.PlaceProtectiveStop(req)
		if err != nil && trader.IsOKXAlgoAlreadyExists(err) {
			// 51068: an algo with the same client id already exists on OKX.
			// Adopt it instead of looping on failed placements.
			adopted, adoptErr := ti.adoptProtectiveOrderByClientID(mgr, cycle, req, result.TickSize, result.QuantityStep)
			switch {
			case adoptErr != nil:
				err = fmt.Errorf("%v; adoption by client id failed: %v", err, adoptErr)
			case adopted == nil:
				// The conflicting order is terminal (canceled/failed) but OKX
				// still reserves its client id: place with a fresh unique id.
				req.ClientID = fmt.Sprintf("cg%da%dr%d", cycle.ID, cycle.ReentryCount, time.Now().Unix()%1000000)
				live, err = mgr.PlaceProtectiveStop(req)
			case isProtectiveStopFired(adopted.State):
				// The adopted order already fired; poll will record the stop.
				logger.Infof("🛑 [%s] Copy Guard 接管的保护单已触发(state=%s) | cycle=%d algoId=%s", ti.traderID, adopted.State, cycle.ID, adopted.AlgoID)
				return
			default:
				live, err = adopted, nil
			}
		}
	}
	if err != nil {
		coverage := 0.0
		if storedErr == nil {
			coverage = protectionCoverage(stored.Quantity, quantity)
		}
		ti.markProtectionIssue(cycle, store.CopyGuardProtectionDegraded, "PROTECTION_CREATE_FAILED", err, coverage, incrementRetry)
		return
	}
	if live == nil {
		ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN", fmt.Errorf("protective stop acknowledgement is empty"), 0, incrementRetry)
		return
	}
	// Verify against OKX BEFORE recording the new quantity locally: a failed
	// verification must leave the DB row consistent with the actual exchange
	// order, otherwise the poll loop compares against polluted numbers.
	verified, verifyErr := ti.verifyProtectiveStopWithGrace(mgr, live.AlgoID, dec.Symbol)
	if verifyErr != nil {
		status, event := store.CopyGuardProtectionUnknown, "PROTECTION_VERIFY_UNKNOWN"
		if errors.Is(verifyErr, trader.ErrProtectiveStopNotFound) {
			// OKX confirmed the just-placed/amended order is gone.
			status, event = store.CopyGuardProtectionDegraded, "PROTECTION_DEGRADED"
		}
		ti.markProtectionIssue(cycle, status, event, verifyErr, 0, incrementRetry)
		return
	}
	coverage := protectionCoverage(verified.Quantity, quantity)
	valid := strings.EqualFold(verified.PositionSide, side) && (dec.MarginMode == "" || verified.MarginMode == "" || verified.MarginMode == dec.MarginMode) && math.Abs(verified.TriggerPrice-result.SLPrice) <= math.Max(result.TickSize, 1e-8) && protectionQuantityMatches(verified.Quantity, quantity, result.QuantityStep) && strings.EqualFold(verified.State, "live")
	if !valid {
		// Record what actually exists on OKX (not the requested quantity) so
		// local tracking stays truthful while retries converge.
		_ = ti.store.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: cycle.ID, TraderID: ti.traderID, AlgoID: verified.AlgoID, AlgoClientID: verified.ClientID, Symbol: dec.Symbol, Side: side, MarginMode: dec.MarginMode, Quantity: verified.Quantity, QuantityStep: result.QuantityStep, TriggerPrice: verified.TriggerPrice, TriggerType: ti.engine.config.RiskTriggerPriceType, Status: strings.ToLower(verified.State)})
		ti.markProtectionIssue(cycle, store.CopyGuardProtectionDegraded, "PROTECTION_DEGRADED", fmt.Errorf("protective stop verification mismatch state=%s coverage=%.4f", verified.State, coverage), coverage, incrementRetry)
		return
	}
	_ = ti.store.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: cycle.ID, TraderID: ti.traderID, AlgoID: verified.AlgoID, AlgoClientID: verified.ClientID, Symbol: dec.Symbol, Side: side, MarginMode: dec.MarginMode, Quantity: verified.Quantity, QuantityStep: result.QuantityStep, TriggerPrice: result.SLPrice, TriggerType: ti.engine.config.RiskTriggerPriceType, Status: "live"})
	if ti.engine.config.RiskReentryDecisionMode == "ai_guarded" {
		equity := cycle.AccountEquity
		if equity <= 0 {
			equity = ti.getEquityFunc()()
		}
		var reserveErr error
		if dec.QuantityStepOverride && result.ExpectedLossUSD > equity*ti.engine.config.RiskAccountPct+1e-9 {
			reserveErr = ti.store.ReentryAI().ReserveCopyGuardRiskFollowFirst(ti.traderID, cycle.ID, cycle.ReentryCount, result.ExpectedLossUSD, equity, ti.engine.config.RiskAccountPct, ti.engine.config.RiskCycleLossBudgetPct, ti.engine.config.RiskPortfolioLossBudgetPct)
		} else {
			reserveErr = ti.store.ReentryAI().ReserveCopyGuardRisk(ti.traderID, cycle.ID, cycle.ReentryCount, result.ExpectedLossUSD, equity, ti.engine.config.RiskAccountPct, ti.engine.config.RiskCycleLossBudgetPct, ti.engine.config.RiskPortfolioLossBudgetPct)
		}
		if reserveErr != nil {
			ti.handleUnprotectableForDecision(dec, side, quantity, entryPrice, fmt.Errorf("risk reservation failed after fill: %w", reserveErr))
			return
		}
		if dec.QuantityStepOverride && result.ExpectedLossUSD > equity*ti.engine.config.RiskAccountPct+1e-9 {
			_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "RISK_STEP_OVERRIDE_ACCEPTED", Price: entryPrice, Quantity: quantity, Notional: entryPrice * quantity, Metadata: map[string]interface{}{"attempt_no": cycle.ReentryCount, "expected_loss_usd": result.ExpectedLossUSD, "attempt_budget_usd": equity * ti.engine.config.RiskAccountPct, "cycle_hard_budget_usd": equity * ti.engine.config.RiskCycleLossBudgetPct, "portfolio_hard_budget_usd": equity * ti.engine.config.RiskPortfolioLossBudgetPct}})
		}
	}
	// v5 可保护性状态机：clamp 挂出的保护单是"已保护但降级"，健康状态记
	// CLAMPED（UI 醒目提示随时可能被扫损），不算 VERIFIED。
	healthStatus, healthMsg := store.CopyGuardProtectionVerified, ""
	if result.Clamped {
		healthStatus = store.CopyGuardProtectionClamped
		healthMsg = fmt.Sprintf("trigger clamped to liquidation safety line (sl=%.4f, distance %.2f%%)", result.SLPrice, (result.SLDistance/entryPrice)*100)
		logger.Warnf("⚠️ [%s] Copy Guard 保护单已 clamp 到强平安全线 | cycle=%d %s %s SL=%.4f 距离=%.4f", ti.traderID, cycle.ID, dec.Symbol, side, result.SLPrice, result.SLDistance)
	}
	_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, healthStatus, coverage, healthMsg, followerPosID, "", false)
	slDistancePct := float64(0)
	if entryPrice > 0 {
		slDistancePct = result.SLDistance / entryPrice
	}
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "PROTECTION_ACTIVE", Price: result.SLPrice, Quantity: quantity, Metadata: map[string]interface{}{
		"attempt_no":               cycle.ReentryCount,
		"actual_qty":               verified.Quantity,
		"planned_qty":              quantity,
		"quantity_step":            result.QuantityStep,
		"stop_price":               result.SLPrice,
		"risk_budget":              result.ExpectedLossUSD,
		"algo_id":                  live.AlgoID,
		"expected_loss_pct":        result.ExpectedLossPct,
		"expected_loss_usd":        result.ExpectedLossUSD,
		"expected_margin_loss_pct": result.ExpectedMarginLossPct,
		"distance_atr_ratio":       result.DistanceATRRatio,
		"governed_by":              result.GovernedBy,
		"noise_conflict":           result.NoiseConflict,
		"clamped":                  result.Clamped,
		"entry_price":              entryPrice,
		"sl_distance":              result.SLDistance,
		"sl_distance_pct":          slDistancePct,
		"atr_value":                result.ATRValue,
		"atr_distance":             result.ATRDistance,
		"leverage":                 dec.Leverage,
		"account_equity":           cycle.AccountEquity,
	}})
	protectionReason := ""
	if result.Clamped {
		protectionReason = "PROTECTION_CLAMPED"
	}
	ti.transitionExecutionIntent(dec, store.ExecutionIntentProtected, protectionReason, "")
	if ti.engine.config.RiskReentryDecisionMode == "ai_guarded" && cycle.Status == store.CopyGuardFollowingReentry {
		ti.enforceAIReentryProtection(dec)
	}
	if result.Clamped {
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "PROTECTION_CLAMPED", Price: result.SLPrice, Quantity: quantity, Metadata: map[string]interface{}{
			"algo_id":         live.AlgoID,
			"entry_price":     entryPrice,
			"sl_distance_pct": slDistancePct,
			"governed_by":     result.GovernedBy,
		}})
	}
}

// isProtectiveStopFired reports whether the OKX algo state means the stop has
// been triggered (order sent / filled). Matches the states handled by
// pollV4ProtectiveStops when recording a stop.
func isProtectiveStopFired(state string) bool {
	switch strings.ToLower(state) {
	case "effective", "triggered", "filled":
		return true
	}
	return false
}

// verifyProtectiveStopWithGrace re-queries a freshly placed or amended algo
// order. Right after placement OKX may briefly answer 51603 (order not yet
// visible), so lookups are retried a few times before the failure is reported.
func (ti *TraderIntegration) verifyProtectiveStopWithGrace(mgr ProtectiveStopManagerV4, algoID, symbol string) (*trader.ProtectiveStopOrder, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		verified, err := mgr.GetProtectiveStop(algoID, symbol)
		if err == nil {
			return verified, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// resolveProtectiveOrder returns the actual exchange-side protective order.
// Contract:
//   - (order, nil): the order exists on OKX (any state)
//   - (nil, nil): OKX confirmed the order no longer exists
//   - (nil, err): state unknown because the lookups failed; the caller must
//     keep local tracking untouched and retry later
func (ti *TraderIntegration) resolveProtectiveOrder(mgr ProtectiveStopManagerV4, algoID, clientID, symbol string) (*trader.ProtectiveStopOrder, error) {
	confirmedAbsent := false
	var queryErr error
	if algoID != "" {
		order, err := mgr.GetProtectiveStop(algoID, symbol)
		if err == nil {
			return order, nil
		}
		if errors.Is(err, trader.ErrProtectiveStopNotFound) {
			confirmedAbsent = true
		} else {
			queryErr = err
		}
	}
	if clientID != "" {
		order, err := mgr.GetProtectiveStopByClientID(clientID, symbol)
		if err == nil {
			return order, nil
		}
		if errors.Is(err, trader.ErrProtectiveStopNotFound) {
			confirmedAbsent = true
		} else {
			queryErr = err
		}
	}
	if queryErr != nil {
		return nil, queryErr
	}
	if confirmedAbsent {
		return nil, nil
	}
	return nil, fmt.Errorf("protective stop identifiers are empty")
}

func (ti *TraderIntegration) retryRetiringProtectiveStop(mgr ProtectiveStopManagerV4, cycle *store.CopyGuardCycle, stored *store.CopyGuardProtectiveOrder) bool {
	// Re-confirm the promoted replacement before touching the retiring order.
	// A process may have crashed after persistence, and the new order may have
	// since been canceled/rejected. The old live stop is the safety anchor.
	current, currentErr := ti.resolveProtectiveOrder(mgr, stored.AlgoID, stored.AlgoClientID, stored.Symbol)
	if currentErr != nil {
		ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "PROTECTION_REPLACEMENT_PENDING", fmt.Errorf("replacement protective stop state unknown: %w", currentErr), cycle.ProtectionCoverage, false)
		return false
	}
	if current != nil && isProtectiveStopFired(current.State) {
		// Let the normal poll path reconcile the fired current stop. The retiring
		// order remains tracked and is canceled when the lifecycle closes.
		return false
	}
	if current == nil || !strings.EqualFold(current.State, "live") {
		old, oldErr := ti.resolveProtectiveOrder(mgr, stored.PreviousAlgoID, stored.PreviousAlgoClientID, stored.Symbol)
		if oldErr != nil {
			ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "PROTECTION_REPLACEMENT_PENDING", fmt.Errorf("replacement is not live and retiring stop state is unknown: %w", oldErr), cycle.ProtectionCoverage, false)
			return false
		}
		if old != nil && strings.EqualFold(old.State, "live") {
			quantity := old.Quantity
			if quantity <= 0 {
				quantity = stored.Quantity
			}
			triggerPrice := old.TriggerPrice
			if triggerPrice <= 0 {
				triggerPrice = stored.TriggerPrice
			}
			if restoreErr := ti.store.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{
				CycleID: stored.CycleID, TraderID: stored.TraderID, AlgoID: old.AlgoID, AlgoClientID: old.ClientID,
				Symbol: stored.Symbol, Side: stored.Side, MarginMode: stored.MarginMode, Quantity: quantity,
				QuantityStep: stored.QuantityStep, TriggerPrice: triggerPrice, TriggerType: stored.TriggerType, Status: "live",
			}); restoreErr != nil {
				ti.markProtectionIssue(cycle, store.CopyGuardProtectionUnknown, "PROTECTION_REPLACEMENT_PENDING", fmt.Errorf("restore retiring protective stop tracking: %w", restoreErr), cycle.ProtectionCoverage, false)
				return false
			}
		}
		state := "absent"
		if current != nil {
			state = current.State
		}
		ti.markProtectionIssue(cycle, store.CopyGuardProtectionDegraded, "PROTECTION_REPLACEMENT_ROLLED_BACK", fmt.Errorf("replacement is not live (state=%s); retained previous protection", state), cycle.ProtectionCoverage, false)
		return false
	}

	old, err := ti.resolveProtectiveOrder(mgr, stored.PreviousAlgoID, stored.PreviousAlgoClientID, stored.Symbol)
	if err != nil {
		ti.markReplacementRetirementPending(cycle, fmt.Errorf("retiring protective stop state unknown: %w", err))
		return false
	}
	if old == nil || strings.EqualFold(old.State, "canceled") || strings.EqualFold(old.State, "order_failed") {
		_ = ti.store.CopyTrade().CompleteCopyGuardProtectiveReplacement(stored.CycleID)
		return true
	}
	if err := mgr.CancelProtectiveStop(stored.PreviousAlgoID, stored.Symbol); err != nil && !errors.Is(err, trader.ErrProtectiveStopNotFound) {
		ti.markReplacementRetirementPending(cycle, fmt.Errorf("new stop is live but retiring stop cancellation failed: %w", err))
		return false
	}
	old, err = ti.resolveProtectiveOrder(mgr, stored.PreviousAlgoID, stored.PreviousAlgoClientID, stored.Symbol)
	if err == nil && (old == nil || strings.EqualFold(old.State, "canceled") || strings.EqualFold(old.State, "order_failed")) {
		_ = ti.store.CopyTrade().CompleteCopyGuardProtectiveReplacement(stored.CycleID)
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: stored.CycleID, TraderID: ti.traderID, Type: "PROTECTION_REPLACEMENT_COMPLETED", Metadata: map[string]interface{}{"current_algo_id": stored.AlgoID, "retired_algo_id": stored.PreviousAlgoID}})
		return true
	}
	if err != nil {
		ti.markReplacementRetirementPending(cycle, fmt.Errorf("retiring protective stop termination unconfirmed: %w", err))
		return false
	}
	ti.markReplacementRetirementPending(cycle, fmt.Errorf("retiring protective stop remains state=%s", old.State))
	return false
}

// markReplacementRetirementPending 记录"新保护单已确认 live、旧单退休未完成"
// 的状态。仓位实际处于受保护状态，健康态记 VERIFIED（并复位重试计数），
// 不得标 DEGRADED——那会把周期送回 retryDegradedV4Protections → upsert 链路，
// 在 Binance（无 amend）上每轮重挂新单造成连环孤儿挂单，重试耗尽后还会被
// 误升级为 unprotectable 强平。退休进度由 pollV4ProtectiveStops 每轮继续推进。
func (ti *TraderIntegration) markReplacementRetirementPending(cycle *store.CopyGuardCycle, cause error) {
	message := fmt.Sprintf("replacement live; retiring stop pending: %v", cause)
	_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionVerified, cycle.ProtectionCoverage, message, cycle.FollowerPosID, cycle.EntryOrderID, false)
	if ti.shouldRecordProtectionEvent(cycle.ID, "PROTECTION_REPLACEMENT_PENDING", message) {
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "PROTECTION_REPLACEMENT_PENDING", Metadata: map[string]interface{}{"error": message, "leader_pos_id": cycle.LeaderPosID, "symbol": cycle.Symbol, "side": cycle.Side}})
	}
	logger.Warnf("⚠️ [%s] Copy Guard 替换退休未完成（新单已生效，巡检继续撤旧单） | cycle=%d %s %s: %v", ti.traderID, cycle.ID, cycle.Symbol, cycle.Side, cause)
}

// adoptProtectiveOrderByClientID takes over an existing OKX algo order that
// conflicts with our client id (error 51068). Returns:
//   - (order, nil): adopted; when live it has been amended to the requested
//     trigger price/quantity if they diverged
//   - (nil, nil): the existing order is terminal (canceled/failed) and cannot
//     be adopted; the caller should place with a fresh client id
//   - (nil, err): lookup or amend failed; retry later
func (ti *TraderIntegration) adoptProtectiveOrderByClientID(mgr ProtectiveStopManagerV4, cycle *store.CopyGuardCycle, req trader.ProtectiveStopRequest, tickSize, quantityStep float64) (*trader.ProtectiveStopOrder, error) {
	existing, err := mgr.GetProtectiveStopByClientID(req.ClientID, req.Symbol)
	if err != nil {
		return nil, err
	}
	state := strings.ToLower(existing.State)
	if state != "live" && !isProtectiveStopFired(existing.State) {
		return nil, nil
	}
	// Take over bookkeeping first so even a follow-up amend failure leaves the
	// tracked algoId pointing at the real order (poll can then manage it).
	_ = ti.store.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: cycle.ID, TraderID: ti.traderID, AlgoID: existing.AlgoID, AlgoClientID: existing.ClientID, Symbol: req.Symbol, Side: req.PositionSide, MarginMode: req.MarginMode, Quantity: existing.Quantity, QuantityStep: quantityStep, TriggerPrice: existing.TriggerPrice, TriggerType: req.TriggerType, Status: "live"})
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "PROTECTIVE_STOP_ADOPTED", Price: existing.TriggerPrice, Quantity: existing.Quantity, Metadata: map[string]interface{}{"algo_id": existing.AlgoID, "algo_client_id": existing.ClientID, "state": existing.State}})
	if isProtectiveStopFired(existing.State) {
		return existing, nil
	}
	priceOK := math.Abs(existing.TriggerPrice-req.TriggerPrice) <= math.Max(tickSize, 1e-8)
	if !priceOK || !protectionQuantityMatches(existing.Quantity, req.Quantity, quantityStep) {
		if amendErr := mgr.AmendProtectiveStop(existing.AlgoID, req); amendErr != nil {
			return nil, amendErr
		}
	}
	return &trader.ProtectiveStopOrder{AlgoID: existing.AlgoID, ClientID: existing.ClientID, Symbol: req.Symbol, PositionSide: req.PositionSide, MarginMode: req.MarginMode, Quantity: req.Quantity, TriggerPrice: req.TriggerPrice, State: "live"}, nil
}

// followerPositionQuantity returns the follower's current position size for
// the given symbol/side/marginMode. ok is false when positions cannot be
// fetched (unknown state must not be mistaken for flat).
//
// fresh 语义：
//   - true：强制绕过交易所持仓缓存（GetPositionsFresh）。仅用于"误判代价高"
//     的事件路径（如保护单消失后的空仓判定——误把有仓判成无仓会漏报降级，
//     误把无仓判成有仓会重建无仓保护单，v5 修过这个坑）。
//   - false：走带 TTL 的缓存读。用于每 3s 的例行巡检（如保护单覆盖率基准），
//     15s 内的数据足够；例行巡检若强制刷新会持续击穿缓存，是 OKX 50011
//     限流（跟单执行失败）的根因之一。
func (ti *TraderIntegration) followerPositionQuantity(symbol, side, marginMode string, fresh bool) (float64, bool) {
	var positions []map[string]interface{}
	var err error
	if fresh {
		positions, err = ti.getFreshPositions()
	} else {
		positions, err = ti.executor.GetPositions()
	}
	if err != nil {
		return 0, false
	}
	total := 0.0
	for _, pos := range positions {
		if getStringField(pos, "symbol") != symbol {
			continue
		}
		if !strings.EqualFold(getStringField(pos, "side"), side) {
			continue
		}
		mode := getStringField(pos, "mgnMode", "marginMode")
		if marginMode != "" && mode != "" && mode != marginMode {
			continue
		}
		total += absFloat(getFloatField(pos, "positionAmt", "quantity"))
	}
	return total, true
}

// isFollowerPositionFlat reports whether the follower currently has no open
// position for the given symbol/side/marginMode. Returns false when positions
// cannot be fetched (unknown must not be treated as flat).
func (ti *TraderIntegration) isFollowerPositionFlat(symbol, side, marginMode string) bool {
	// 空仓判定必须用最新数据：结果直接决定"止损已触发"与"保护降级"的分流
	qty, ok := ti.followerPositionQuantity(symbol, side, marginMode, true)
	return ok && qty == 0
}

// cancelProtectiveOrderForCycle cancels the tracked protective order and
// normalizes OKX 51400 (already filled/canceled/purged) into a normal terminal
// state when the follower position is flat, instead of raising a protection
// issue on an already-finished lifecycle. Returns nil when the order reached a
// terminal state either way.
func (ti *TraderIntegration) cancelProtectiveOrderForCycle(mgr ProtectiveStopManagerV4, cycle *store.CopyGuardCycle, order *store.CopyGuardProtectiveOrder) error {
	terminalCancel := func(algoID string, requireFlatForMissing bool) error {
		if strings.TrimSpace(algoID) == "" {
			return nil
		}
		err := mgr.CancelProtectiveStop(algoID, order.Symbol)
		if err == nil {
			return nil
		}
		if errors.Is(err, trader.ErrProtectiveStopNotFound) || trader.IsOKXAlgoTerminalCancelError(err) {
			if !requireFlatForMissing || cycle.ClosedAt != nil || ti.isFollowerPositionFlat(cycle.Symbol, cycle.Side, cycle.MarginMode) {
				return nil
			}
			return fmt.Errorf("protective stop is missing/terminal but follower flatness is not confirmed: %w", err)
		}
		return err
	}
	var currentErr error
	if order.Status == "" || order.Status == "live" {
		currentErr = terminalCancel(order.AlgoID, true)
	}
	var retiringErr error
	if order.ReplacementPending && order.PreviousAlgoID != "" {
		// The promoted current order is tracked independently; absence of the
		// retiring order is the desired terminal state and needs no flat check.
		retiringErr = terminalCancel(order.PreviousAlgoID, false)
	}
	if retiringErr != nil {
		return fmt.Errorf("retiring protective stop cancellation failed: %w", retiringErr)
	}
	if currentErr == nil {
		_ = ti.store.CopyTrade().UpdateCopyGuardProtectiveOrderStatus(cycle.ID, "canceled")
		_ = ti.store.CopyTrade().CompleteCopyGuardProtectiveReplacement(cycle.ID)
		detail := "canceled"
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "PROTECTIVE_STOP_TERMINAL", Metadata: map[string]interface{}{"algo_id": order.AlgoID, "detail": detail}})
		return nil
	}
	return currentErr
}

func (ti *TraderIntegration) getFreshPositions() ([]map[string]interface{}, error) {
	if fresh, ok := ti.executor.(FreshPositionsProvider); ok {
		return fresh.GetPositionsFresh()
	}
	return ti.executor.GetPositions()
}

func (ti *TraderIntegration) markProtectionIssueForDecision(dec *decision.Decision, status, eventType string, cause error, coverage float64) {
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil {
		logger.Errorf("❌ [%s] Copy Guard protection issue without lifecycle | %s: %v", ti.traderID, dec.Symbol, cause)
		return
	}
	ti.markProtectionIssue(cycle, status, eventType, cause, coverage, dec.Reasoning != "Copy Guard protection retry")
	ti.transitionExecutionIntent(dec, store.ExecutionIntentReconciling, eventType, cause.Error())
}

func (ti *TraderIntegration) markProtectionIssue(cycle *store.CopyGuardCycle, status, eventType string, cause error, coverage float64, incrementRetry bool) {
	message := "unknown protection error"
	if cause != nil {
		message = cause.Error()
	}
	_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, status, coverage, message, cycle.FollowerPosID, cycle.EntryOrderID, incrementRetry)
	metadata := map[string]interface{}{"error": message, "coverage": coverage, "retry": cycle.ProtectionRetries + 1, "leader_pos_id": cycle.LeaderPosID, "follower_pos_id": cycle.FollowerPosID, "symbol": cycle.Symbol, "side": cycle.Side, "margin_mode": cycle.MarginMode}
	if cycle.ProtectionMissingAt != nil {
		metadata["first_failure_at"] = cycle.ProtectionMissingAt.UTC().Format(time.RFC3339)
		metadata["failure_duration_seconds"] = time.Since(*cycle.ProtectionMissingAt).Seconds()
	} else {
		metadata["first_failure_at"] = time.Now().UTC().Format(time.RFC3339)
		metadata["failure_duration_seconds"] = 0
	}
	if order, err := ti.store.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID); err == nil {
		metadata["algo_id"] = order.AlgoID
		metadata["algo_client_id"] = order.AlgoClientID
		metadata["protected_quantity"] = order.Quantity
		metadata["trigger_price"] = order.TriggerPrice
	}
	// 事件限频：poll 每 3 秒跑一轮，同一错误持续期间会把事件表刷爆
	// （实盘 cycle 27 积累了 3000+ 条重复 PROTECTION_COVERAGE_LOW）。
	// 健康状态照常每轮更新，只有"同 cycle + 同事件类型 + 同错误"的重复
	// 事件在 60 秒内只落一条；错误内容变化时立即记录。
	if ti.shouldRecordProtectionEvent(cycle.ID, eventType, message) {
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: eventType, Metadata: metadata})
	}
	logger.Errorf("❌ [%s] Copy Guard protection %s | cycle=%d posId=%s %s %s coverage=%.2f%% error=%s", ti.traderID, status, cycle.ID, cycle.LeaderPosID, cycle.Symbol, cycle.Side, coverage*100, message)
	// 首次失败只记日志+事件，不发邮件：自动重试大多能在数轮内自愈；
	// 邮件只保留两类——持续缺失超过 10 分钟的升级告警（retryDegradedV4Protections）
	// 与恢复通知（pollV4ProtectiveStops），避免瞬时故障刷屏。
}

// protectionEventDedupeWindow: minimum interval between two identical
// protection-issue events for the same cycle.
const protectionEventDedupeWindow = time.Minute

func (ti *TraderIntegration) shouldRecordProtectionEvent(cycleID int64, eventType, message string) bool {
	key := fmt.Sprintf("%d|%s|%s", cycleID, eventType, message)
	now := time.Now()
	ti.protectionEventMu.Lock()
	defer ti.protectionEventMu.Unlock()
	if last, ok := ti.lastProtectionEvent[key]; ok && now.Sub(last) < protectionEventDedupeWindow {
		return false
	}
	// Opportunistic pruning keeps the map bounded across long uptimes.
	if len(ti.lastProtectionEvent) > 512 {
		for k, v := range ti.lastProtectionEvent {
			if now.Sub(v) > 10*protectionEventDedupeWindow {
				delete(ti.lastProtectionEvent, k)
			}
		}
	}
	ti.lastProtectionEvent[key] = now
	return true
}

// protectionNotifyKeys 生成保护类告警的限流键与去重键（M8）。
// RateKey 不带时间桶：同 cycle 同类告警受 MinInterval 限流，压制短时风暴。
// DedupKey 带小时桶：notifier 的 DedupKey 是进程生命周期一次性去重，长生命
// 周期 cycle 内同类故障复发（如保护单再次缺失）若共用裸 key 将永远静默；
// 小时桶保证复发故障至多每小时提醒一次，与文件内既有的小时分桶告警模式一致。
func protectionNotifyKeys(traderID string, cycleID int64, kind string, now time.Time) (rateKey, dedupKey string) {
	rateKey = fmt.Sprintf("copy_guard_protection|%s|%d|%s", traderID, cycleID, kind)
	dedupKey = fmt.Sprintf("%s|%d", rateKey, now.Unix()/3600)
	return rateKey, dedupKey
}

func (ti *TraderIntegration) notifyProtection(cycle *store.CopyGuardCycle, title, body, kind string) {
	key, dedupKey := protectionNotifyKeys(ti.traderID, cycle.ID, kind, time.Now())
	// 标题与字段带交易员显示名：保护类邮件此前只有 TraderID，多交易员场景
	// 下用户无法直接判断是哪个账户出的问题（小白可读性缺陷，全局修复）
	traderName := ti.traderDisplayName()
	notifier.Notify(notifier.Alert{Category: "copy_trade", TraderID: ti.traderID, TraderName: traderName, Title: fmt.Sprintf("%s | %s | %s %s", traderName, title, cycle.Symbol, cycle.Side), Body: fmt.Sprintf("Trader Name: %s\nTrader ID:   %s\n\n%s", traderName, ti.traderID, body), RateKey: key, DedupKey: dedupKey, Fields: map[string]string{"TraderName": traderName, "CycleID": fmt.Sprint(cycle.ID), "LeaderPosID": cycle.LeaderPosID, "Symbol": cycle.Symbol, "Side": cycle.Side, "MarginMode": cycle.MarginMode}})
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

// cancelProtectionsOnDisable 关闭「账户保护止损」开关后的清理：
// 撤销该 trader 在交易所上仍 live 的全部 Copy Guard 保护单并记事件，
// 保证 UI"已关闭"与交易所实际状态一致。撤单失败仅告警（下次重启会再清理）。
func (ti *TraderIntegration) cancelProtectionsOnDisable() {
	mgr, ok := ti.executor.(ProtectiveStopManagerV4)
	if !ok {
		return
	}
	orders, err := ti.store.CopyTrade().ListActiveCopyGuardProtectiveOrders(ti.traderID)
	if err != nil || len(orders) == 0 {
		return
	}
	logger.Infof("🧹 [%s] 止损开关已关闭，撤销 %d 个存活保护单", ti.traderID, len(orders))
	for _, order := range orders {
		cycle, cerr := ti.store.CopyTrade().GetCopyGuardCycle(order.CycleID)
		if cerr != nil {
			logger.Warnf("⚠️ [%s] 保护单清理：读取周期失败 cycle=%d: %v", ti.traderID, order.CycleID, cerr)
			continue
		}
		if cancelErr := ti.cancelProtectiveOrderForCycle(mgr, cycle, order); cancelErr != nil {
			logger.Warnf("⚠️ [%s] 保护单清理：撤单失败 cycle=%d algoId=%s: %v", ti.traderID, cycle.ID, order.AlgoID, cancelErr)
			continue
		}
		_ = ti.store.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionCanceled, 0, "stop loss disabled by user", cycle.FollowerPosID, cycle.EntryOrderID, false)
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "PROTECTION_DISABLED_CANCELED", Metadata: map[string]interface{}{
			"algo_id": order.AlgoID, "symbol": order.Symbol, "reason": "risk_stop_loss_enabled=false",
		}})
	}
}

func (ti *TraderIntegration) cancelV4Protection(dec *decision.Decision) {
	mgr, ok := ti.executor.(ProtectiveStopManagerV4)
	if !ok {
		return
	}
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, dec.LeaderPosID)
	if err != nil {
		return
	}
	order, err := ti.store.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil {
		return
	}
	if err := ti.cancelProtectiveOrderForCycle(mgr, cycle, order); err != nil {
		logger.Warnf("⚠️ [%s] 撤销 v4 保护单失败: %v", ti.traderID, err)
	}
}

// sendStopLossTriggerAlert 发送账户保护止损触发邮件
//
// 去重按 cycle + attempt；同一领航员仓位的第二次止损不能被第一次压掉。
//
// 邮件正文：完整上下文，让用户能立刻判断后续动作（是否手动干预、是否启用二次进场等）
func (ti *TraderIntegration) sendStopLossTriggerAlert(event *RiskEvent) {
	if ti.engine != nil && ti.engine.config != nil && !store.ShouldSendCopyGuardEmail(ti.engine.config.RiskNotificationLevel, "STOP_FLAT_CONFIRMED") {
		return
	}
	traderName := ti.traderDisplayName()
	providerType := ""
	leaderID := ""
	if ti.engine != nil && ti.engine.config != nil {
		providerType = string(ti.engine.config.ProviderType)
		leaderID = ti.engine.config.LeaderID
	}

	cycleID, attemptNo := event.CycleID, event.ReentryCount
	if cycleID == 0 {
		if cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, event.LeaderPosID); err == nil {
			cycleID, attemptNo = cycle.ID, cycle.ReentryCount
		}
	}
	alertKey := fmt.Sprintf("STOP_FLAT_CONFIRMED|%s|%d|%d|0", ti.traderID, cycleID, attemptNo)

	// 恢复条件文案（根据是否启用二次进场动态调整）
	recoveryHint := "等领航员完全平掉旧 posId 后，下次他重新开仓时跟单系统自动恢复跟随"
	if ti.engine != nil && ti.engine.config != nil && ti.engine.config.RiskReentryEnabled {
		if ti.engine.config.RiskReentryDecisionMode == "ai_guarded" {
			recoveryHint = "仓位确认完全平仓后冷却 5 分钟，系统进入持久 AI 观察；\n" +
				"  AI 只判断趋势反转与入场价值，下单前仍由确定性风控复核价格、仓位、风险预算和保护能力；\n" +
				"  也会在领航员平仓、反向或观察到期时自动结束。"
		} else {
			recoveryHint = fmt.Sprintf(
				"已启用旧规则重入：价格回归重入边界并连续确认、冷却期结束且重入后仓位可保护 → 自动按 %.0f%% 名义上限重入；\n"+
					"  或等领航员完全平掉旧 posId，下次他新开仓时自动恢复跟随",
				ti.engine.config.RiskReentryRatio*100)
		}
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
		Time:       event.Timestamp,
		Category:   "copy_trade",
		TraderID:   ti.traderID,
		TraderName: traderName,
		Title:      fmt.Sprintf("%s | 账户保护止损触发 %s %s", traderName, event.Symbol, event.Side),
		Body:       body,
		RateKey:    alertKey,
		DedupKey:   alertKey,
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
			"CycleID":     fmt.Sprint(cycleID),
			"AttemptNo":   fmt.Sprint(attemptNo),
		},
	})

	logger.Infof("📧 [%s] 已发送 SL 触发告警邮件 | posId=%s", ti.traderID, event.LeaderPosID)
}

// sendReentryInitiatedAlert 发送二次进场决策已生成邮件
//
// legacy_rule 仍保留旧的“决策已生成”通知。ai_guarded 不发审批/建议邮件，
// 只在成交并验证保护覆盖后由 enforceAIReentryProtection 发一封结果邮件。
//
// 注意：这是"决策已生成"告警。实际执行成功/失败的告警分别走：
//   - 成功：executeFullDecision 成功分支（识别 reentry decision 后单独发 sendReentryExecutedAlert）
//   - 失败：execFailureDedupKey 路径（普通跟单失败告警）
func (ti *TraderIntegration) sendReentryInitiatedAlert(event *RiskEvent) {
	if ti.engine != nil && ti.engine.config != nil && ti.engine.config.RiskReentryDecisionMode == "ai_guarded" {
		return
	}
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

	cycleID, attemptNo := event.CycleID, event.ReentryCount+1
	if cycleID == 0 {
		if cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, event.LeaderPosID); err == nil {
			cycleID, attemptNo = cycle.ID, cycle.ReentryCount+1
		}
	}
	alertKey := fmt.Sprintf("REENTRY_REQUESTED|%s|%d|%d|0", ti.traderID, cycleID, attemptNo)

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
		Time:       event.Timestamp,
		Category:   "copy_trade",
		TraderID:   ti.traderID,
		TraderName: traderName,
		Title:      fmt.Sprintf("%s | 二次进场触发 %s %s", traderName, event.Symbol, event.Side),
		Body:       body,
		RateKey:    alertKey,
		DedupKey:   alertKey,
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
			"CycleID":           fmt.Sprint(cycleID),
			"AttemptNo":         fmt.Sprint(attemptNo),
		},
	})

	logger.Infof("📧 [%s] 已发送二次进场触发告警邮件 | posId=%s", ti.traderID, event.LeaderPosID)
}

// ============================================================================
// 人工重入（v5.1）已于 v7 退役：不再生成新信号、不再执行确认下单。
// 本段仅保留兼容边界（ConfirmManualReentry 恒返回 ErrManualReentryRetired）
// 与历史 EXECUTING 信号的结果回写（markManualReentryOutcome）。
// ============================================================================

// markManualReentryOutcome 重入执行成败后回写人工信号状态。
// 周期没有 EXECUTING 信号时（自动重入路径）静默跳过，零影响。
func (ti *TraderIntegration) markManualReentryOutcome(cycleID int64, status, errMsg string) {
	sig, err := ti.store.CopyTrade().GetExecutingManualReentrySignalByCycle(cycleID)
	if err != nil || sig == nil {
		return // 无 EXECUTING 信号 = 自动重入，无需回写
	}
	if err := ti.store.CopyTrade().MarkManualReentrySignalOutcome(sig.ID, status, errMsg); err != nil {
		logger.Warnf("⚠️ [%s] 人工重入信号结果回写失败: %v | signal=%d status=%s", ti.traderID, err, sig.ID, status)
		return
	}
	logger.Infof("✅ [%s] 人工重入信号结果已回写 | signal=%d cycle=%d status=%s", ti.traderID, sig.ID, cycleID, status)
}

// ErrManualReentryRetired is returned by every compatibility entry point for
// the removed per-order approval workflow. Keeping a typed error lets API and
// older callers fail explicitly instead of ever reaching the legacy executor.
var ErrManualReentryRetired = errors.New("manual reentry confirmation was retired in Copy Guard v7; use ai_guarded candidates")

// ConfirmManualReentry is retained as a binary/source compatibility boundary;
// v7 never permits it to execute a trade.
func (ti *TraderIntegration) ConfirmManualReentry(_ int64, _ string, _ float64) error {
	return ErrManualReentryRetired
}

// ExecuteAIReentry 执行 ai_guarded 候选。AI 只能缩小 max_notional；本函数
// 在提交决策前重新检查行情、仓位、领航员、保护可行性和三层风险预算。
func (ti *TraderIntegration) ExecuteAIReentry(candidateID, analysisID int64) error {
	if ti.engine == nil || ti.engine.config == nil {
		return fmt.Errorf("跟单引擎未运行")
	}
	cfg := ti.engine.config
	if cfg.RiskReentryDecisionMode != "ai_guarded" || !cfg.RiskReentryEnabled {
		return fmt.Errorf("AI guarded reentry is not enabled")
	}
	rs := ti.store.ReentryAI()
	candidate, err := rs.GetReentryCandidate(candidateID)
	if err != nil {
		return fmt.Errorf("候选不存在: %w", err)
	}
	if candidate.TraderID != ti.traderID || candidate.Status != store.ReentryCandidateEntryPending {
		return fmt.Errorf("候选状态不允许执行: %s", candidate.Status)
	}
	analysis, err := rs.GetReentryAnalysis(analysisID)
	if err != nil || analysis.CandidateID != candidate.ID || candidate.LastAnalysisID != analysis.ID {
		return fmt.Errorf("AI 分析与候选不匹配")
	}
	ttl := candidate.DecisionTTLSeconds
	if expired, age := aiDecisionExpired(analysis, ttl, time.Now()); expired {
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: candidate.CycleID, TraderID: ti.traderID, Type: "AI_RESULT_STALE", Price: candidate.TriggerPrice, Metadata: map[string]interface{}{"candidate_id": candidate.ID, "analysis_id": analysis.ID, "attempt_no": candidate.ReentryCount + 1, "age_seconds": age.Seconds(), "ttl_seconds": ttl, "reason_code": "SNAPSHOT_STALE"}})
		return fmt.Errorf("AI 结果已过期: %s", age.Truncate(time.Second))
	}
	if !candidate.Protectable {
		return fmt.Errorf("保护止损预检未通过")
	}
	cycle, err := ti.store.CopyTrade().GetOpenCopyGuardCycle(ti.traderID, candidate.LeaderPosID)
	if err != nil || cycle.ID != candidate.CycleID {
		return fmt.Errorf("保护周期已结束")
	}
	if candidate.ReentryCount != cycle.ReentryCount {
		return fmt.Errorf("候选尝试次数已过期")
	}
	if cycle.ReentryCount >= cfg.RiskMaxReentries {
		return fmt.Errorf("已达到最大重入次数")
	}
	switch cycle.Status {
	case store.CopyGuardAIWatching, store.CopyGuardAIWaiting, store.CopyGuardAIReviewing, store.CopyGuardStoppedWatching:
	default:
		return fmt.Errorf("保护周期状态不允许重入: %s", cycle.Status)
	}
	leader := ti.engine.buildLeaderPosMap()[candidate.LeaderPosID]
	if leader == nil || leader.Size <= 0 || (leader.Side != "" && string(leader.Side) != candidate.Side) {
		return fmt.Errorf("领航员已平仓或反向")
	}
	positions, err := ti.getFreshPositions()
	if err != nil {
		return fmt.Errorf("查询跟随仓位失败: %w", err)
	}
	for _, pos := range positions {
		if getStringField(pos, "symbol") == candidate.Symbol && getStringField(pos, "side") == candidate.Side && math.Abs(getFloatField(pos, "positionAmt", "quantity")) > 0 {
			if candidate.MarginMode == "" || getStringField(pos, "marginMode", "mgnMode") == "" || getStringField(pos, "marginMode", "mgnMode") == candidate.MarginMode {
				return fmt.Errorf("跟随账户已有同向仓位")
			}
		}
	}
	price := leader.MarkPrice
	if mgr, ok := ti.executor.(StopLossManager); ok {
		if p, e := mgr.GetMarketPrice(candidate.Symbol); e == nil && p > 0 {
			price = p
		}
	}
	if price <= 0 {
		return fmt.Errorf("实时价格不可用")
	}
	if candidate.EntryPriceLow <= 0 || candidate.EntryPriceHigh < candidate.EntryPriceLow || price < candidate.EntryPriceLow || price > candidate.EntryPriceHigh {
		return fmt.Errorf("实时价 %.8f 已离开 AI 入场区间 [%.8f, %.8f]", price, candidate.EntryPriceLow, candidate.EntryPriceHigh)
	}
	snapshotPrice := analysis.SnapshotPrice
	if snapshotPrice <= 0 {
		snapshotPrice = (candidate.EntryPriceLow + candidate.EntryPriceHigh) / 2
	}
	if candidate.ATR <= 0 || math.Abs(price-snapshotPrice) > 0.25*candidate.ATR {
		return fmt.Errorf("价格相对 AI 快照漂移超过 0.25 ATR")
	}
	nominalCap := candidate.MaxNotional * candidate.SizeFactor
	minNotional := minTradeNotionalOrDefault(cfg.MinTradeWarn)
	equity := ti.getEquityFunc()()
	if equity <= 0 {
		equity = cycle.AccountEquity
	}
	if equity <= 0 {
		return fmt.Errorf("账户权益不可用")
	}
	side := SideLong
	if candidate.Side == string(SideShort) {
		side = SideShort
	}
	structure := structuralInvalidationPrice(candidate.Symbol, side, price)
	usage, err := rs.GetCopyGuardRiskUsageExcludingAttempt(ti.traderID, cycle.ID, cycle.ReentryCount+1)
	if err != nil {
		return fmt.Errorf("风险占用查询失败: %w", err)
	}
	availableRisk, err := AvailableCopyGuardRiskUSD(cfg, equity, usage)
	if err != nil {
		return fmt.Errorf("风险预算不足: %w", err)
	}
	plan, err := BuildProtectionPlan(cfg, side, price, candidate.ATR, structure, equity, availableRisk/equity, nominalCap)
	if err != nil {
		return fmt.Errorf("保护计划失败: %w", err)
	}
	notional := plan.MaxNotional
	if notional < minNotional {
		return fmt.Errorf("风险缩量后名义 %.2f 低于最小下单额 %.2f", notional, minNotional)
	}
	attemptNo := cycle.ReentryCount + 1
	if err := rs.ReserveCopyGuardRisk(ti.traderID, cycle.ID, attemptNo, plan.ExpectedLossUSD, equity, cfg.RiskAccountPct, cfg.RiskCycleLossBudgetPct, cfg.RiskPortfolioLossBudgetPct); err != nil {
		return fmt.Errorf("风险预算拒绝: %w", err)
	}
	mapping, err := ti.store.CopyTrade().GetMapping(ti.traderID, candidate.LeaderPosID)
	if err != nil || mapping.Status != "stopped_by_risk" {
		_ = rs.ReleaseCopyGuardRisk(cycle.ID, attemptNo)
		return fmt.Errorf("仓位映射不是止损观察态")
	}
	_ = ti.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardReentryPending, leader.EntryPrice, price, candidate.ATR)
	_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: ti.traderID, Type: "REENTRY_REQUESTED", Price: price, Notional: notional, Metadata: map[string]interface{}{"candidate_id": candidate.ID, "analysis_id": analysis.ID, "attempt_no": attemptNo, "decision_generation": candidate.DecisionGeneration, "size_factor": candidate.SizeFactor, "risk_budget": plan.ExpectedLossUSD, "max_risk_usd": plan.MaxRiskUSD, "stop_price": plan.StopPrice, "stop_distance": plan.StopDistance, "structure_invalidation": plan.StructureInvalidation}})
	if !ti.engine.emitReentryDecision(mapping, leader, notional, price) {
		_ = rs.ReleaseCopyGuardRisk(cycle.ID, attemptNo)
		_ = ti.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardAIWaiting, leader.EntryPrice, price, candidate.ATR)
		return fmt.Errorf("执行通道繁忙")
	}
	return nil
}

// ExecuteAIReentryForTrader 供 ReentryAdvisor 调用运行中的标准执行链。
func ExecuteAIReentryForTrader(traderID string, candidateID, analysisID int64) error {
	ti, ok := integrations[traderID]
	if !ok || !ti.IsRunning() {
		return fmt.Errorf("该交易员跟单未运行")
	}
	return ti.ExecuteAIReentry(candidateID, analysisID)
}

// GetExecutionMarketPriceForTrader exposes only a public market price from the
// running follower exchange adapter. It never exposes account credentials or
// private position data and lets the AI datapack label the execution venue as
// the primary price source.
func GetExecutionMarketPriceForTrader(traderID, symbol string) (float64, error) {
	ti, ok := integrations[traderID]
	if !ok || !ti.IsRunning() {
		return 0, fmt.Errorf("copy trader is not running")
	}
	mgr, ok := ti.executor.(StopLossManager)
	if !ok {
		return 0, fmt.Errorf("executor does not provide market price")
	}
	price, err := mgr.GetMarketPrice(symbol)
	if err != nil {
		return 0, fmt.Errorf("execution market price unavailable: %w", err)
	}
	if price <= 0 {
		return 0, fmt.Errorf("execution market returned a non-positive price")
	}
	return price, nil
}

type ExecutionRiskPosition struct {
	Symbol      string  `json:"symbol"`
	Side        string  `json:"side"`
	MarginMode  string  `json:"margin_mode"`
	Quantity    float64 `json:"quantity"`
	EntryPrice  float64 `json:"entry_price"`
	MarkPrice   float64 `json:"mark_price"`
	NotionalUSD float64 `json:"notional_usd"`
}

type ExecutionRiskSnapshot struct {
	EquityUSD             float64                 `json:"equity_usd"`
	AttemptBudgetUSD      float64                 `json:"attempt_budget_usd"`
	CycleRemainingUSD     float64                 `json:"cycle_remaining_usd"`
	PortfolioRemainingUSD float64                 `json:"portfolio_remaining_usd"`
	Positions             []ExecutionRiskPosition `json:"positions"`
}

// GetExecutionRiskSnapshotForTrader returns a credential-free private account
// snapshot for the AI datapack. It is deliberately not an HTTP endpoint.
func GetExecutionRiskSnapshotForTrader(traderID string, cycleID int64) (*ExecutionRiskSnapshot, error) {
	ti, ok := integrations[traderID]
	if !ok || !ti.IsRunning() || ti.engine == nil || ti.engine.config == nil {
		return nil, fmt.Errorf("copy trader is not running")
	}
	equity := ti.getEquityFunc()()
	if equity <= 0 {
		return nil, fmt.Errorf("account equity unavailable")
	}
	cfg := ti.engine.config
	usage, err := ti.store.ReentryAI().GetCopyGuardRiskUsage(traderID, cycleID)
	if err != nil {
		return nil, err
	}
	snapshot := &ExecutionRiskSnapshot{
		EquityUSD: equity, AttemptBudgetUSD: equity * cfg.RiskAccountPct,
		CycleRemainingUSD:     equity*cfg.RiskCycleLossBudgetPct - usage.CycleUsedUSD,
		PortfolioRemainingUSD: equity*cfg.RiskPortfolioLossBudgetPct - usage.PortfolioUsedUSD,
		Positions:             []ExecutionRiskPosition{},
	}
	if snapshot.CycleRemainingUSD < 0 {
		snapshot.CycleRemainingUSD = 0
	}
	if snapshot.PortfolioRemainingUSD < 0 {
		snapshot.PortfolioRemainingUSD = 0
	}
	positions, err := ti.getFreshPositions()
	if err != nil {
		return nil, err
	}
	for _, pos := range positions {
		quantity := math.Abs(getFloatField(pos, "positionAmt", "quantity"))
		if quantity <= 0 {
			continue
		}
		entry := getFloatField(pos, "entryPrice", "entry_price")
		mark := getFloatField(pos, "markPrice", "mark_price")
		if mark <= 0 {
			mark = entry
		}
		snapshot.Positions = append(snapshot.Positions, ExecutionRiskPosition{Symbol: getStringField(pos, "symbol"), Side: getStringField(pos, "side"), MarginMode: getStringField(pos, "marginMode", "mgnMode"), Quantity: quantity, EntryPrice: entry, MarkPrice: mark, NotionalUSD: quantity * mark})
	}
	return snapshot, nil
}

// ConfirmManualReentryForTrader 包级导出：API 层确认人工重入信号。
// 需要运行中的跟单引擎（实时复核领航员持仓 + 复用决策执行链）。
// overrideNotional <=0 表示用信号建议金额（见 ConfirmManualReentry）。
func ConfirmManualReentryForTrader(traderID string, signalID int64, operator string, overrideNotional float64) error {
	integration, exists := integrations[traderID]
	if !exists || !integration.IsRunning() {
		return fmt.Errorf("该交易员跟单未运行，无法执行人工重入（请先启动跟单）")
	}
	return integration.ConfirmManualReentry(signalID, operator, overrideNotional)
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
	if err := integration.StartCopyTrading(); err != nil {
		return err
	}
	integrations[traderID] = integration
	return nil
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
