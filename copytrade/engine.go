package copytrade

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"nofx/decision"
	"nofx/logger"
	"nofx/notifier"
	"nofx/store"
)

const binancePositionSizeEpsilon = 1e-10

type binancePositionHistoryProvider interface {
	GetPositionHistory(leadPortfolioID string) ([]BinancePositionHistoryRecord, error)
}

// Engine 跟单引擎
type Engine struct {
	traderID string
	config   *CopyConfig
	provider LeaderProvider

	// 流式 Provider（如果支持）
	streamingProvider StreamingProvider
	isStreamingMode   bool

	// Binance 全局凭证加载器（仅 ProviderType=binance 时生效；nil 时走旧的 config 字段路径）
	binanceCredLoader BinanceCredentialsLoader

	// 跟随者账户信息（由外部注入）
	getFollowerBalance         func() float64
	getFollowerEquity          func() float64
	getFollowerPositions       func() map[string]*Position
	getFollowerPositionsResult func() (map[string]*Position, error)

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
	// smartEmptySnapshotCandidateAt is an engine-level confirmation guard that
	// survives provider-cache uncertainty within a running process. It is used
	// when persisted follower state exists, including the first poll after a
	// process restart where provider memory has no previous position count.
	smartEmptySnapshotCandidateAt time.Time
	// lastBaselineRefresh 限频 refreshEstimatedBaselines（每分钟一次）
	lastBaselineRefresh time.Time

	// 决策输出
	decisionCh chan *decision.FullDecision

	// 风控事件输出（v3 风控邮件告警机制）
	// 设计：buffered channel，写入失败时 select-default 降级（丢事件不阻塞主流程）
	// 容量 32：考虑极端情况下批量 SL 触发，3 秒 poll 周期内 32 个告警足够
	riskEventCh chan *RiskEvent

	// 预警日志
	warnings   []Warning
	warningsMu sync.Mutex

	// 运行状态
	running bool
	stopCh  chan struct{}
	mu      sync.RWMutex

	// 统计
	stats *EngineStats

	// v3 风控：账户保护止损疑似计数（防 GetPositions API 抖动误判）
	// key = leaderPosID，value = 连续疑似 SL 触发的次数
	// 达到 stopRiskSuspectThreshold 后才正式标 stopped_by_risk
	// SupportsCopyGuard（OKX/Binance）数据源使用；checkStoppedByRisk 内部访问，与 poll 串行执行无需额外锁
	stopRiskSuspectCount map[string]int

	// v5 确认式重入：REENTRY_CANDIDATE 连续确认计数（key = leaderPosID）。
	// 条件持续满足时逐 tick 递增，任一 tick 破坏即清零；达到确认阈值才重入。
	// 内存态：引擎重启后从 0 重新累计（保守方向，不会导致提前重入）。
	// 与 poll 串行执行，无需额外锁。
	reentryCandidateTicks map[string]int
	// AI 模式只需按已收盘 5m K 线检查结构事件；该内存限频不承担正确性，
	// 重启后持久化的 last_review_at/feature_hash 仍可防重复模型调用。
	lastAICandleFeatureCheck map[int64]time.Time
}

// EngineOption 引擎配置选项
type EngineOption func(*Engine)

func WithFollowerEquity(getEquity func() float64) EngineOption {
	return func(e *Engine) { e.getFollowerEquity = getEquity }
}

func WithFollowerPositionsResult(getPositions func() (map[string]*Position, error)) EngineOption {
	return func(e *Engine) { e.getFollowerPositionsResult = getPositions }
}

// WithStreamingMode 启用流式模式（WebSocket 事件驱动）
func WithStreamingMode() EngineOption {
	return func(e *Engine) {
		e.isStreamingMode = true
	}
}

// WithBinanceCredentialsLoader 注入 Binance 全局凭证加载器（启用热加载）
//
// 仅 ProviderType=binance 生效；OKX/HL 完全不读 loader。
// loader 为 nil 时等同未启用全局凭证（行为与旧版本一致）。
func WithBinanceCredentialsLoader(loader BinanceCredentialsLoader) EngineOption {
	return func(e *Engine) {
		e.binanceCredLoader = loader
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
		traderID:                 traderID,
		config:                   config,
		getFollowerBalance:       getBalance,
		getFollowerPositions:     getPositions,
		seenFills:                make(map[string]time.Time),
		seenTTL:                  1 * time.Hour,
		stateSyncInterval:        20 * time.Second,
		decisionCh:               make(chan *decision.FullDecision, 10),
		riskEventCh:              make(chan *RiskEvent, 32),
		stopCh:                   make(chan struct{}),
		stats:                    &EngineStats{StartTime: time.Now()},
		stopRiskSuspectCount:     make(map[string]int),
		reentryCandidateTicks:    make(map[string]int),
		lastAICandleFeatureCheck: make(map[int64]time.Time),
	}
	// Keep legacy constructor callers compatible while ensuring later balance
	// callback replacement (common in tests/adapters) is reflected. Production
	// injects an explicit total-equity callback via WithFollowerEquity.
	e.getFollowerEquity = func() float64 {
		if e.getFollowerBalance == nil {
			return 0
		}
		return e.getFollowerBalance()
	}

	// 应用选项
	for _, opt := range opts {
		opt(e)
	}

	// 根据配置选择 Provider 类型
	if e.isStreamingMode {
		// 尝试创建流式 Provider（目前只有 Hyperliquid 支持）
		streamingProvider, err := NewStreamingProvider(config)
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
	// 仅 Binance 路径会消费 binanceCredLoader；OKX/HL 完全不读，零影响。
	provider, err := NewProviderWithLoader(config, e.binanceCredLoader)
	if err != nil {
		return nil, err
	}
	e.provider = provider
	logger.Infof("✅ [%s] 使用轮询模式 (REST)", traderID)

	return e, nil
}

// reportBinanceCredentialsExpired 检测错误是否为 Binance 凭证过期
// 若是则发送邮件告警，返回 true。仅 ProviderType=binance 时生效；其他 provider 永远返回 false。
//
// 全局化设计：
//   - RateKey 按 label 维度限流（v1 单 label "default"），无论多少个 trader 触发只发一封
//   - Body 列出所有受影响的 Binance trader（从 store 查询，让用户一目了然）
//   - 标题与 body 中引导用户去"系统设置 → Binance 凭证"页面（v2 全局凭证位置）
func (e *Engine) reportBinanceCredentialsExpired(err error, where string) bool {
	if err == nil || e.config == nil || e.config.ProviderType != ProviderBinance {
		return false
	}
	if !errors.Is(err, ErrBinanceCredentialsExpired) {
		return false
	}

	// 查询全部受影响的 Binance trader（用于告警 body；查询失败不阻断告警）
	var affectedTraders []string
	if e.store != nil {
		if ids, qerr := e.store.BinanceCreds().CountBinanceCopyTraderIDs(); qerr == nil {
			affectedTraders = ids
		}
	}

	logger.Warnf("🔐 [%s] Binance 跟单凭证未配置或已过期 | portfolioId=%s where=%s affected_traders=%d",
		e.traderID, e.config.LeaderID, where, len(affectedTraders))

	// label 在 v1 单账号场景下固定为 default；future-proof: 引擎层有 binanceCredLoader 时
	// 实际 label 由 Provider 决定，这里仅用于 RateKey 与 Body 文案。
	label := DefaultBinanceCredentialsLabel
	traderName := e.traderID
	if e.store != nil {
		traderName = e.store.Trader().ResolveDisplayName(e.traderID)
	}

	notifier.Notify(notifier.Alert{
		Time:       time.Now(),
		Category:   "copy_trade",
		TraderID:   e.traderID,
		TraderName: traderName,
		Title:      "Binance 跟单凭证未配置或已过期，请粘贴 cURL",
		Body:       buildBinanceCredsExpiredAlertBody(label, e.traderID, e.config.LeaderID, where, affectedTraders),
		// 🔑 全局唯一限流键：无论多少 trader 触发，60s 内只发一封
		RateKey: "binance_creds_expired|" + label,
	})

	// Seam D：凭证失效会让 Binance 跟单静默停跟，写入统一日志便于排查。
	// best-effort + 小时分桶去重（凭证检查可能每轮触发，避免刷屏）。
	if e.store != nil {
		if lerr := e.store.CopyTrade().LogCopyEvent(&store.CopyTradeEvent{
			TraderID:     e.traderID,
			LeaderID:     e.config.LeaderID,
			ProviderType: string(e.config.ProviderType),
			Category:     store.CopyEventCategoryError,
			EventType:    "BINANCE_CREDENTIALS_EXPIRED",
			Severity:     store.CopyEventSeverityError,
			Status:       "failed",
			Summary:      "Binance 跟单凭证未配置或已过期，跟单已停跟",
			Detail:       map[string]interface{}{"where": where, "affected_traders": len(affectedTraders)},
			DedupKey:     fmt.Sprintf("err|%s|binance_creds_expired|%d", e.traderID, time.Now().Unix()/3600),
		}); lerr != nil {
			logger.Warnf("⚠️ [%s] 保存凭证失效事件到跟单日志失败: %v", e.traderID, lerr)
		}
	}
	return true
}

// buildBinanceCredsExpiredAlertBody 构造凭证过期告警 body
// 抽到独立函数便于单元测试 + 多触发点复用
func buildBinanceCredsExpiredAlertBody(label, currentTraderID, leaderID, where string, affectedTraders []string) string {
	affectedSection := ""
	if len(affectedTraders) > 0 {
		affectedSection = fmt.Sprintf("\n受影响的 Binance 跟单交易员（共 %d 个）:\n", len(affectedTraders))
		for _, id := range affectedTraders {
			affectedSection += "  - " + id + "\n"
		}
		affectedSection += "\n"
	}
	return fmt.Sprintf(
		"Binance Web 跟单凭证 (p20t / csrftoken) 未配置或已过期失效。\n\n"+
			"凭证 Label: %s\n"+
			"首次检测: traderID=%s portfolioId=%s 位置=%s\n"+
			"%s"+
			"修复方法：\n"+
			"  1. 登录 https://www.binance.com 跟单管理页面（保持登录状态）\n"+
			"  2. 打开 NOFX 前端 → AI 交易员管理 → 顶部 \"Binance 凭证\" 按钮\n"+
			"  3. 粘贴 cURL（自动提取 p20t / csrftoken）\n"+
			"  4. 点击保存后所有 Binance 交易员将自动恢复，无需重启\n\n"+
			"在此期间所有 Binance trader 的数据源将持续失败，OKX / Hyperliquid 不受影响。",
		label, currentTraderID, leaderID, where, affectedSection)
}

// GetDecisionChannel 获取决策输出通道
func (e *Engine) GetDecisionChannel() <-chan *decision.FullDecision {
	return e.decisionCh
}

// GetRiskEventChannel 获取风控事件输出通道（v3 风控邮件告警）
// 由 integration 层消费，转发为邮件告警
func (e *Engine) GetRiskEventChannel() <-chan *RiskEvent {
	return e.riskEventCh
}

// emitRiskEvent 推送风控事件（非阻塞）
// 失败时（channel 满）记 Debug 日志降级，不影响主流程
func (e *Engine) emitRiskEvent(event *RiskEvent) {
	if event == nil {
		return
	}
	select {
	case e.riskEventCh <- event:
		// 推送成功
	default:
		logger.Debugf("⚠️ [%s] 风控事件 channel 已满，丢弃 | type=%s posId=%s",
			e.traderID, event.Type, event.LeaderPosID)
	}
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
//
// 仅在首次启用（该 trader×leader 无任何 mapping 记录）时执行全量标记：
// 重启/配置变更时跳过——否则停机窗口期领航员新开的仓位（本应跟随，只是
// 错过了开仓信号）会被误标 ignored 而永久失跟。真正的历史仓位在首次启用
// 时已持久化为 ignored，重启后依然生效，跳过重写不会削弱保护。
func (e *Engine) InitIgnoredPositions() error {
	if e.store == nil {
		return fmt.Errorf("store not initialized")
	}

	if e.isSmartMoneyMode() {
		generation := e.config.SourceGeneration
		if generation <= 0 {
			generation = 1
		}
		complete, err := e.store.CopyTrade().IsSourceBaselineComplete(e.traderID, e.config.LeaderID, string(e.config.BinanceSourceMode), generation)
		if err != nil {
			return fmt.Errorf("查询 Smart Money 来源基线失败: %w", err)
		}
		if complete {
			logger.Infof("📊 [%s] Smart Money 来源代次 %d 已完成首次基线，跳过重复初始化", e.traderID, generation)
			return nil
		}
		state, err := e.provider.GetAccountState(e.config.LeaderID)
		if err != nil {
			e.recordSmartMoneySourceHealth()
			e.reportBinanceCredentialsExpired(err, "InitIgnoredPositions")
			return fmt.Errorf("获取 Smart Money 首次完整仓位失败: %w", err)
		}
		e.recordSmartMoneySourceHealth()
		positions := sourceBaselinePositions(state)
		if err := e.store.CopyTrade().InitializeSourceBaseline(e.traderID, e.config.LeaderID, string(e.config.BinanceSourceMode), generation, positions); err != nil {
			return fmt.Errorf("原子写入 Smart Money 首次基线失败: %w", err)
		}
		logger.Infof("✅ [%s] Smart Money 来源代次 %d 首次基线完成 | 持仓=%d", e.traderID, generation, len(positions))
		return nil
	}

	hasMappings, err := e.store.CopyTrade().HasMappingsForLeader(e.traderID, e.config.LeaderID)
	if err != nil {
		return fmt.Errorf("查询已有映射失败: %w", err)
	}
	if hasMappings {
		logger.Infof("📊 [%s] 已有跟单映射记录（重启/配置变更），跳过历史仓位全量 ignored 标记", e.traderID)
		return nil
	}

	// 获取领航员当前所有持仓
	state, err := e.provider.GetAccountState(e.config.LeaderID)
	if err != nil {
		e.recordSmartMoneySourceHealth()
		e.reportBinanceCredentialsExpired(err, "InitIgnoredPositions")
		return fmt.Errorf("获取领航员持仓失败: %w", err)
	}
	e.recordSmartMoneySourceHealth()

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
		pending, pendingErr := e.store.CopyTrade().HasUnfinishedLeaderExecutionIntent(e.traderID, posID)
		if pendingErr != nil {
			return fmt.Errorf("查询未完成执行意图失败 posId=%s: %w", posID, pendingErr)
		}
		if pending {
			// A crash between reservation/submission and mapping commit is not a
			// historical startup position. Leaving it unmapped lets the first
			// healthy authoritative snapshot reclaim and reconcile the same
			// canonical revision instead of permanently suppressing it.
			logger.Warnf("🟡 [%s] 启动持仓存在未完成执行意图，等待健康快照重验而不标记 ignored | posId=%s", e.traderID, posID)
			continue
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

func sourceBaselinePositions(state *AccountState) []store.CopyTradeBaselinePosition {
	if state == nil {
		return nil
	}
	positions := make([]store.CopyTradeBaselinePosition, 0, len(state.Positions))
	for key, position := range state.Positions {
		if position == nil || position.Size <= 0 {
			continue
		}
		posID := position.PosID
		if posID == "" {
			posID = key
		}
		positions = append(positions, store.CopyTradeBaselinePosition{
			LeaderPosID: posID, Symbol: position.Symbol, Side: string(position.Side), MarginMode: position.MarginMode, Size: position.Size,
		})
	}
	return positions
}

func (e *Engine) isSmartMoneyMode() bool {
	return e.config != nil &&
		e.config.ProviderType == ProviderBinance &&
		e.config.BinanceSourceMode == BinanceSourceSmartMoney
}

// recordSmartMoneySourceHealth persists the provider observation and owns the
// notification edge. It returns true only when a previously unhealthy source
// recovered with a complete snapshot.
func (e *Engine) recordSmartMoneySourceHealth() bool {
	if !e.isSmartMoneyMode() || e.store == nil {
		return false
	}
	provider, ok := e.provider.(SourceHealthObservationProvider)
	if !ok {
		return false
	}
	obs := provider.LastSourceHealthObservation()
	if obs.CheckedAt.IsZero() {
		return false
	}
	generation := e.config.SourceGeneration
	if generation <= 0 {
		generation = 1
	}
	health, transitioned, err := e.store.CopyTrade().RecordSourceHealthObservation(
		e.traderID,
		e.config.LeaderID,
		string(e.config.BinanceSourceMode),
		generation,
		store.SourceHealthObservation{
			Status:           obs.Status,
			TraderName:       obs.TraderName,
			Error:            obs.Error,
			CompleteSnapshot: obs.CompleteSnapshot,
			CheckedAt:        obs.CheckedAt,
		},
	)
	if err != nil {
		logger.Warnf("⚠️ [%s] 保存 Smart Money 数据源健康状态失败: %v", e.traderID, err)
		return false
	}

	recovered := transitioned && health.Status == store.SourceHealthHealthy && health.PreviousStatus != ""
	now := time.Now()
	shouldNotify := recovered || health.Status != store.SourceHealthHealthy &&
		(transitioned || health.LastNotifiedAt == nil || now.Sub(*health.LastNotifiedAt) >= time.Hour)
	// AUTH_FAILED already uses the global Binance credential notification, which
	// lists every affected trader. Persist it here but avoid sending a duplicate.
	if shouldNotify && health.Status != store.SourceHealthAuthFailed {
		e.notifySmartMoneySourceHealth(health, recovered, now)
		if err := e.store.CopyTrade().MarkSourceHealthNotified(e.traderID, now); err != nil {
			logger.Warnf("⚠️ [%s] 更新 Smart Money 健康状态通知时间失败: %v", e.traderID, err)
		}
	}
	return recovered
}

func (e *Engine) notifySmartMoneySourceHealth(health *store.CopyTradeSourceHealth, recovered bool, now time.Time) {
	if health == nil {
		return
	}
	traderName := e.traderID
	executionExchange := "unknown"
	activeMappings := "none"
	protectionSummary := "none"
	if e.store != nil {
		traderName = e.store.Trader().ResolveDisplayName(e.traderID)
		if configuredTrader, err := e.store.Trader().GetByID(e.traderID); err == nil && configuredTrader.ExchangeID != "" {
			executionExchange = configuredTrader.ExchangeID
		}
		if mappings, err := e.store.CopyTrade().ListActiveMappings(e.traderID); err == nil && len(mappings) > 0 {
			parts := make([]string, 0, len(mappings))
			for _, mapping := range mappings {
				if mapping == nil {
					continue
				}
				sourceSymbol := mapping.SourceSymbol
				if sourceSymbol == "" {
					sourceSymbol = mapping.Symbol
				}
				executionSymbol := mapping.ExecutionSymbol
				if executionSymbol == "" {
					executionSymbol = mapping.Symbol
				}
				parts = append(parts, fmt.Sprintf("%s/%s->%s", sourceSymbol, mapping.Side, executionSymbol))
			}
			if len(parts) > 0 {
				activeMappings = strings.Join(parts, ", ")
			}
		}
		if orders, err := e.store.CopyTrade().ListActiveCopyGuardProtectiveOrders(e.traderID); err == nil && len(orders) > 0 {
			pending := 0
			for _, order := range orders {
				if order.ReplacementPending {
					pending++
				}
			}
			protectionSummary = fmt.Sprintf("%d active, %d replacement pending", len(orders), pending)
		}
	}
	title := fmt.Sprintf("Binance 公开领航员数据源已冻结（%s）", health.Status)
	severity := store.CopyEventSeverityError
	status := "failed"
	if recovered {
		title = "Binance 公开领航员数据源已恢复"
		severity = store.CopyEventSeverityInfo
		status = "success"
	}
	lastComplete := "从未完整同步"
	if health.LastCompleteSnapshotAt != nil {
		lastComplete = health.LastCompleteSnapshotAt.Format(time.RFC3339)
	}
	body := fmt.Sprintf("领航员: %s (%s)\n交易员: %s\n执行交易所: %s\n状态: %s\n最后完整快照: %s\n活动映射: %s\n保护状态: %s\n连续失败: %d\n原因: %s\n处理: 冻结新开/加仓/减仓/平仓同步，已有仓位 Copy Guard 继续运行，不会把数据不可见视为全平。",
		health.TraderName, health.LeaderID, traderName, executionExchange, health.Status, lastComplete, activeMappings, protectionSummary, health.ConsecutiveFailures, health.LastError)
	if recovered {
		body = fmt.Sprintf("领航员: %s (%s)\n交易员: %s\n执行交易所: %s\n状态: HEALTHY\n完整快照: %s\n活动映射: %s\n保护状态: %s\n处理: 私密期间的减仓/平仓会立即同步；新仓/加仓只重建基线，不追单。",
			health.TraderName, health.LeaderID, traderName, executionExchange, lastComplete, activeMappings, protectionSummary)
	}
	bucket := now.Unix() / 3600
	dedup := fmt.Sprintf("source_health|%s|%s|%d", e.traderID, health.Status, bucket)
	if recovered {
		dedup = fmt.Sprintf("source_health|%s|recovered|%d", e.traderID, now.Unix())
	}
	notifier.Notify(notifier.Alert{
		Time: now, Category: "copy_trade", TraderID: e.traderID, TraderName: traderName,
		Title: title, Body: body, RateKey: dedup, DedupKey: dedup,
		Fields: map[string]string{"LeaderID": health.LeaderID, "ExecutionExchange": executionExchange, "SourceStatus": health.Status, "LastCompleteSnapshot": lastComplete, "ActiveMappings": activeMappings, "Protection": protectionSummary},
	})
	if err := e.store.CopyTrade().LogCopyEvent(&store.CopyTradeEvent{
		TraderID: e.traderID, LeaderID: health.LeaderID, ProviderType: string(ProviderBinance),
		Category: store.CopyEventCategoryError, EventType: "SOURCE_HEALTH_" + health.Status,
		Severity: severity, Status: status, Summary: title,
		Detail:   map[string]interface{}{"source_mode": health.SourceMode, "last_complete_snapshot": lastComplete, "consecutive_failures": health.ConsecutiveFailures, "error": health.LastError},
		DedupKey: dedup, CreatedAt: now,
	}); err != nil {
		logger.Warnf("⚠️ [%s] 保存 Smart Money 健康状态事件失败: %v", e.traderID, err)
	}
}

// rebaselineSmartMoneyRecovery deliberately absorbs only risk-increasing
// changes that happened while the source was hidden. Existing mappings keep
// their old size when the leader reduced or closed, so the next snapshot emits
// the required risk-reducing action.
func (e *Engine) rebaselineSmartMoneyRecovery(state *AccountState) error {
	if !e.isSmartMoneyMode() || e.store == nil || state == nil {
		return nil
	}
	if err := e.store.CopyTrade().RebaselineSourceRecovery(e.traderID, e.config.LeaderID, sourceBaselinePositions(state)); err != nil {
		return fmt.Errorf("Smart Money 恢复基线事务失败: %w", err)
	}
	return nil
}

func (e *Engine) smartMoneyRecoveryRequired(checkedAt time.Time) (bool, error) {
	if !e.isSmartMoneyMode() || e.store == nil {
		return false, nil
	}
	health, err := e.store.CopyTrade().GetSourceHealth(e.traderID)
	if err != nil {
		return false, err
	}
	if health == nil {
		return false, nil
	}
	if health.Status != store.SourceHealthHealthy || health.LastCompleteSnapshotAt == nil {
		return true, nil
	}
	return checkedAt.Sub(*health.LastCompleteSnapshotAt) > time.Minute, nil
}

func (e *Engine) confirmSmartMoneyEmptySnapshot(state *AccountState, now time.Time) error {
	if !e.isSmartMoneyMode() || e.store == nil || state == nil || len(state.Positions) > 0 {
		e.smartEmptySnapshotCandidateAt = time.Time{}
		return nil
	}
	if state.EmptySnapshotConfirmed {
		e.smartEmptySnapshotCandidateAt = time.Time{}
		return nil
	}
	live, err := e.store.CopyTrade().HasLiveSourceState(e.traderID)
	if err != nil {
		return fmt.Errorf("check persisted follower state before accepting empty Smart Money snapshot: %w", err)
	}
	if !live {
		e.smartEmptySnapshotCandidateAt = time.Time{}
		return nil
	}
	if e.smartEmptySnapshotCandidateAt.IsZero() {
		e.smartEmptySnapshotCandidateAt = now
		return ErrBinanceSmartMoneyEmptyUnconfirmed
	}
	if now.Sub(e.smartEmptySnapshotCandidateAt) < smartMoneyEmptyConfirmDelay {
		return ErrBinanceSmartMoneyEmptyUnconfirmed
	}
	e.smartEmptySnapshotCandidateAt = time.Time{}
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

// pollLookbackWindow poll 拉取成交的回看窗口。
// 必须 ≥ initSeenFills 的基线窗口（5 分钟），否则重启瞬间会漏掉基线外的成交；
// 也是 UnmarkSeen 瞬态失败重放的最长恢复期。
const pollLookbackWindow = 5 * time.Minute

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
	// 回看最近 5 分钟的成交（与 initSeenFills 的去重基线窗口一致）。
	// 旧值 1 分钟：API 连续失败超过 1 分钟后恢复时，窗口外的成交永久漏单；
	// 且 UnmarkSeen 重放机制依赖 fill 在后续 poll 中仍可拉到。
	// 扩大窗口的重复处理风险由 seenFills 去重 + 状态化匹配（持仓 size 对比）双重兜底。
	since := time.Now().Add(-pollLookbackWindow)
	fills, err := e.provider.GetFills(e.config.LeaderID, since)
	if err != nil {
		e.reportBinanceCredentialsExpired(err, "poll/GetFills")
		logger.Warnf("⚠️ [%s] 获取成交记录失败: %v", e.traderID, err)
		if e.config.ProviderType != ProviderBinance && e.config.ProviderType != ProviderOKX {
			return
		}
		fills = nil
	}

	// 按时间排序（确保反向开仓按顺序处理）
	sort.Slice(fills, func(i, j int) bool {
		return fills[i].Timestamp.Before(fills[j].Timestamp)
	})

	// 🔑 第一步：过滤出新成交（未处理的）
	var newFills []Fill
	for _, fill := range fills {
		if !e.isSeen(fill.ID) {
			newFills = append(newFills, fill)
		}
	}

	// 🔑 第二步：同步领航员持仓（确保用最新数据判断）
	if e.config.ProviderType == ProviderBinance {
		// Binance 成交历史会延迟数分钟；实时持仓快照才是开仓/加仓/减仓/平仓的主信号。
		// 成交历史仍保留为兜底，但只在快照没有检测到变化时处理，避免迟到成交重复触发。
		if err := e.syncLeaderState(); err != nil {
			// fail-closed：同步失败时快照对账无法进行，本轮不得继续消费
			// trade-history 兜底成交——否则会在过期 leaderState 上做决策，
			// 且第三步的 markSeen 会让这批成交永久丢失。信号下轮重取。
			logger.Warnf("⚠️ [%s] Binance 实时持仓同步失败，本轮跳过信号处理: %v", e.traderID, err)
			return
		}
		snapshotFills := e.detectBinancePositionSnapshotFills()
		// 🔑 关键去重：snapshot fill.ID 形如
		//   "binance_snapshot|<posId>|<action>|<previousSize>|<currentSize>"
		// 在"领航员已平 / 跟随者本地无对应仓位 / 执行失败 mapping 未回收"
		// 的死锁场景下，每轮 poll 都会生成完全相同 ID 的 close 信号。
		// 第三步循环已经 markSeen，但此处直接 `newFills = snapshotFills`
		// 跳过了第一步 isSeen 过滤，导致每 3s 重复处理。
		// 这里显式过滤一次，保证去重链路完整。
		var freshSnapshotFills []Fill
		for _, fill := range snapshotFills {
			if !e.isSeen(fill.ID) {
				freshSnapshotFills = append(freshSnapshotFills, fill)
			}
		}
		if len(freshSnapshotFills) > 0 {
			logger.Infof("📡 [%s] Binance 实时持仓发现 %d 条信号（原始 %d 条，去重保留 %d 条）",
				e.traderID, len(freshSnapshotFills), len(snapshotFills), len(freshSnapshotFills))
			for _, fill := range newFills {
				e.markSeen(fill.ID)
			}
			attachRawFillSources(freshSnapshotFills, newFills)
			newFills = freshSnapshotFills
		} else if len(snapshotFills) > 0 {
			logger.Debugf("📡 [%s] Binance 快照检测到 %d 条信号但全部为重复（已 seen），跳过",
				e.traderID, len(snapshotFills))
			// 同样把 trade-history 路径迟到的 fills 标记 seen，避免后续路径重复处理
			for _, fill := range newFills {
				e.markSeen(fill.ID)
			}
			newFills = nil
		} else if len(newFills) > 0 {
			logger.Debugf("📡 [%s] Binance 快照无变化，处理 %d 条成交历史信号", e.traderID, len(newFills))
		}
	} else if len(newFills) > 0 {
		if err := e.syncLeaderState(); err != nil {
			if e.config.ProviderType == ProviderOKX {
				logger.Warnf("⚠️ [%s] OKX 权威持仓同步失败，本轮保留成交等待重放: %v", e.traderID, err)
				return
			}
			logger.Warnf("⚠️ [%s] 处理信号前同步状态失败: %v（使用缓存）", e.traderID, err)
		} else {
			logger.Debugf("📡 [%s] 收到 %d 条新成交，已同步领航员持仓", e.traderID, len(newFills))
		}
	} else {
		// 无新成交时，保持原有的定时同步作为兜底（防止长时间无交易时数据过旧）
		if (e.config.ProviderType == ProviderOKX && e.config.RiskPolicyVersion >= 4) || time.Since(e.lastStateSync) > e.stateSyncInterval {
			if err := e.syncLeaderState(); err != nil {
				logger.Warnf("⚠️ [%s] 定时状态同步失败: %v", e.traderID, err)
				if e.config.ProviderType == ProviderOKX {
					return
				}
			}
		}
	}

	// OKX also needs the authoritative position-snapshot path. Previously only
	// Binance synthesized state deltas; OKX startup then marked the last five
	// minutes of fills as seen and permanently lost changes made while NOFX was
	// down. The current snapshot collapses every same-position fill in this poll
	// into one final target transition.
	if e.config.ProviderType == ProviderOKX {
		snapshotFills := e.detectBinancePositionSnapshotFills()
		freshSnapshotFills := make([]Fill, 0, len(snapshotFills))
		for _, fill := range snapshotFills {
			if !e.isSeen(fill.ID) {
				freshSnapshotFills = append(freshSnapshotFills, fill)
			}
		}
		if len(freshSnapshotFills) > 0 {
			attachRawFillSources(freshSnapshotFills, newFills)
			for _, fill := range newFills {
				e.markSeen(fill.ID)
			}
			newFills = freshSnapshotFills
			logger.Infof("📡 [%s] OKX 持仓快照合并为 %d 条最终状态变化", e.traderID, len(newFills))
		} else if len(snapshotFills) > 0 {
			for _, fill := range newFills {
				e.markSeen(fill.ID)
			}
			newFills = nil
		}
	}

	// 🔑 第三步：处理所有新成交（共用同一份最新 leaderState）
	for i := range newFills {
		fill := &newFills[i]
		e.markSeen(fill.ID)

		e.stats.SignalsReceived++
		e.stats.LastSignalTime = time.Now()

		// 单向持仓模式标准化：将 net 模式的 fill 转换为与双向模式等价的语义
		// 必须在 buildSignal/processSignal 之前执行，使后续全链路无需区分模式
		e.normalizeNetModeFill(fill)

		signal := e.buildSignal(fill)

		logger.Infof("📡 [%s] 收到信号 | %s %s %s | 价格=%.4f 数量=%.4f 价值=%.2f",
			e.traderID, fill.Symbol, fill.Action, fill.PositionSide,
			fill.Price, fill.Size, fill.Value)

		e.processSignal(signal)
	}
}

// attachRawFillSources retains the complete source evidence when authoritative
// position snapshots collapse several fills into one transition. Exact posId
// is not exposed by every history endpoint, so symbol+position side is the
// narrowest safe association; the authoritative size/value still comes from
// the healthy position snapshot.
func attachRawFillSources(snapshotFills, rawFills []Fill) {
	for i := range snapshotFills {
		seen := make(map[string]struct{})
		for _, raw := range rawFills {
			if raw.ID == "" || !strings.EqualFold(raw.Symbol, snapshotFills[i].Symbol) || raw.PositionSide != snapshotFills[i].PositionSide {
				continue
			}
			if _, ok := seen[raw.ID]; ok {
				continue
			}
			seen[raw.ID] = struct{}{}
			snapshotFills[i].SourceFillIDs = append(snapshotFills[i].SourceFillIDs, raw.ID)
		}
		if len(snapshotFills[i].SourceFillIDs) == 0 {
			snapshotFills[i].SourceFillIDs = []string{snapshotFills[i].ID}
		}
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

// detectBinancePositionSnapshotFills 基于当前领航员持仓和本地 mapping 生成
// 权威状态变化。函数名为历史兼容保留；当前支持 Binance 和 OKX。
func (e *Engine) detectBinancePositionSnapshotFills() []Fill {
	if e.store == nil || e.config == nil || (e.config.ProviderType != ProviderBinance && e.config.ProviderType != ProviderOKX) {
		return nil
	}

	leaderPosMap := e.buildLeaderPosMap()
	var fills []Fill

	for posID, pos := range leaderPosMap {
		if pos == nil || pos.Size <= 0 {
			continue
		}
		if posID == "" {
			posID = pos.PosID
		}
		if posID == "" {
			posID = fmt.Sprintf("%s_%s", pos.Symbol, pos.Side)
		}

		mapping, err := e.store.CopyTrade().GetMapping(e.traderID, posID)
		if err != nil {
			logger.Warnf("⚠️ [%s] %s 持仓兜底查询映射失败: %v (posId=%s)", e.traderID, e.config.ProviderType, err, posID)
			continue
		}
		if mapping == nil {
			revision, revisionErr := e.store.CopyTrade().GetSourceSnapshotRevision(e.traderID, posID)
			if revisionErr != nil {
				logger.Warnf("⚠️ [%s] %s 查询快照修订号失败: %v (posId=%s)", e.traderID, e.config.ProviderType, revisionErr, posID)
				continue
			}
			fills = append(fills, e.buildBinanceSnapshotFillForPosition(pos, posID, ActionOpen, pos.Size, 0, pos.Size, revision))
			continue
		}
		if mapping.Status == "ignored" {
			if SideType(mapping.Side) != pos.Side {
				// ignored 代表旧方向从未在跟随账户执行。领航员复用同一
				// posId 原地反向时，旧生命周期已经确定结束，可以原子地
				// 关闭 ignored 记录并在本轮生成新方向开仓；无需制造虚假
				// 的跟随者平仓动作。
				if err := e.store.CopyTrade().MarkIgnoredAsClosed(e.traderID, posID); err != nil {
					logger.Warnf("⚠️ [%s] %s 历史仓位反向但关闭 ignored 生命周期失败: %v (posId=%s)", e.traderID, e.config.ProviderType, err, posID)
					continue
				}
				nextRevision := mapping.SourceRevision + 1
				fills = append(fills, e.buildBinanceSnapshotFillForPosition(pos, posID, ActionOpen, pos.Size, 0, pos.Size, nextRevision))
				logger.Infof("🔄 [%s] %s 历史仓位原地反向 | posId=%s %s→%s，旧 ignored 生命周期已关闭并跟随新方向",
					e.traderID, e.config.ProviderType, posID, mapping.Side, pos.Side)
				continue
			}
			logger.Debugf("📊 [%s] %s 历史仓位仍在持仓中 | posId=%s → 持仓兜底跳过", e.traderID, e.config.ProviderType, posID)
			continue
		}
		if mapping.Status != "active" {
			continue
		}

		lastKnownSize := mapping.LastKnownSize
		if SideType(mapping.Side) != pos.Side {
			// 相同 leader_pos_id 的方向变化不是加减仓，而是旧生命周期
			// 全平后建立新生命周期。这里只生成旧方向 close，等待交易所
			// 成交、保护撤销、cycle/mapping 提交完成；下一轮 mapping 已
			// closed 后才会生成新方向 open，从而保证严格串行。
			fills = append(fills, e.buildBinanceSnapshotFill(
				posID,
				mapping.Symbol,
				SideType(mapping.Side),
				ActionClose,
				lastKnownSize,
				e.positionSignalPrice(pos),
				lastKnownSize,
				0,
				mapping.SourceRevision,
			))
			continue
		}
		if pos.Size > lastKnownSize+binancePositionSizeEpsilon {
			sizeDelta := pos.Size - lastKnownSize
			fills = append(fills, e.buildBinanceSnapshotFillForPosition(pos, posID, ActionAdd, sizeDelta, lastKnownSize, pos.Size, mapping.SourceRevision))
		} else if lastKnownSize > pos.Size+binancePositionSizeEpsilon {
			sizeDelta := lastKnownSize - pos.Size
			action := ActionReduce
			if pos.Size < lastKnownSize*NearZeroThreshold {
				action = ActionClose
			}
			fills = append(fills, e.buildBinanceSnapshotFillForPosition(pos, posID, action, sizeDelta, lastKnownSize, pos.Size, mapping.SourceRevision))
		}
	}

	activeMappings, err := e.store.CopyTrade().ListActiveMappings(e.traderID)
	if err != nil {
		logger.Warnf("⚠️ [%s] %s 持仓兜底读取 active 映射失败: %v", e.traderID, e.config.ProviderType, err)
		return fills
	}
	for _, mapping := range activeMappings {
		if mapping == nil {
			continue
		}
		if _, exists := leaderPosMap[mapping.LeaderPosID]; exists {
			continue
		}
		fills = append(fills, e.buildBinanceSnapshotFill(
			mapping.LeaderPosID,
			mapping.Symbol,
			SideType(mapping.Side),
			ActionClose,
			mapping.LastKnownSize,
			e.binanceCloseSignalPrice(mapping),
			mapping.LastKnownSize,
			0,
			mapping.SourceRevision,
		))
	}

	sort.Slice(fills, func(i, j int) bool {
		leftPriority := binanceSnapshotActionPriority(fills[i].Action)
		rightPriority := binanceSnapshotActionPriority(fills[j].Action)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if fills[i].Symbol != fills[j].Symbol {
			return fills[i].Symbol < fills[j].Symbol
		}
		if fills[i].PositionSide != fills[j].PositionSide {
			return fills[i].PositionSide < fills[j].PositionSide
		}
		return fills[i].ID < fills[j].ID
	})
	return fills
}

func binanceSnapshotActionPriority(action ActionType) int {
	switch action {
	case ActionClose:
		return 0
	case ActionReduce:
		return 1
	case ActionOpen:
		return 2
	case ActionAdd:
		return 3
	default:
		return 4
	}
}

func (e *Engine) buildBinanceSnapshotFillForPosition(pos *Position, posID string, action ActionType, size, previousSize, currentSize float64, sourceRevision int64) Fill {
	fill := e.buildBinanceSnapshotFill(posID, pos.Symbol, pos.Side, action, size, e.positionSignalPrice(pos), previousSize, currentSize, sourceRevision)
	if e.config != nil && e.config.ProviderType == ProviderOKX {
		fill.RawValue = math.Abs(size * fill.Price)
		fill.ValueCurrency = "USD"
		if pos != nil && pos.Size > 0 && pos.PositionValue > 0 {
			// OKX public position size is native contract count. notionalUsd is
			// already ctVal-aware, so scale the authoritative total notional by
			// the native size delta instead of guessing contracts*price.
			fill.Value = math.Abs(size/pos.Size) * math.Abs(pos.PositionValue)
			fill.ValueUSDValid = fill.Value > 0
		} else {
			fill.Value = 0
			fill.ValueUSDValid = false
			fill.ValueError = "OKX position notionalUsd is unavailable for native contract conversion"
		}
		return fill
	}
	if !e.isSmartMoneyMode() || pos == nil {
		return fill
	}
	fill.RawValue = math.Abs(size * fill.Price)
	fill.ValueCurrency = pos.ValueCurrency
	fill.ValueUSDValid = pos.ValueUSDValid
	fill.ValueError = pos.ValueError
	if pos.ValueUSDValid && pos.RawPositionValue > 0 && pos.PositionValue > 0 {
		fill.Value = fill.RawValue * (pos.PositionValue / pos.RawPositionValue)
	} else {
		fill.Value = 0
	}
	return fill
}

func (e *Engine) buildBinanceSnapshotFill(posID, symbol string, side SideType, action ActionType, size, price, previousSize, currentSize float64, sourceRevision int64) Fill {
	if size < 0 {
		size = -size
	}
	value := size * price
	if value < 0 {
		value = -value
	}

	tradeSide := "buy"
	if (action == ActionOpen || action == ActionAdd) && side == SideShort {
		tradeSide = "sell"
	}
	if (action == ActionReduce || action == ActionClose) && side == SideLong {
		tradeSide = "sell"
	}

	sourcePrefix := strings.ToLower(string(e.config.ProviderType)) + "_snapshot"
	fillID := fmt.Sprintf("%s|%s|%s|%.8f|%.8f", sourcePrefix, posID, action, previousSize, currentSize)
	if e.isSmartMoneyMode() {
		// Smart Money synthesizes signals from current-position snapshots, so
		// the same numeric transition can legitimately recur. The persisted
		// acknowledgement revision stays stable across a crash/retry and only
		// advances after the prior transition is accepted. Source generation
		// also prevents identity reuse after a configured source change.
		fillID = fmt.Sprintf("binance_snapshot|smart|g%d|r%d|%s|%s|%.8f|%.8f",
			e.config.SourceGeneration, sourceRevision, posID, action, previousSize, currentSize)
	} else if e.config != nil && e.config.ProviderType == ProviderOKX {
		// OKX can reuse a posId after a complete close. Persisted source
		// revision therefore belongs to the canonical identity just as it does
		// for Smart Money snapshots; otherwise 0->same-size reopens inside the
		// seen-fill TTL are indistinguishable from the previous lifecycle.
		fillID = fmt.Sprintf("okx_snapshot|r%d|%s|%s|%.8f|%.8f",
			sourceRevision, posID, action, previousSize, currentSize)
	} else if e.config != nil && e.config.ProviderType == ProviderBinance &&
		action == ActionOpen && sourceRevision > 0 {
		// Preserve the historical Copy Management identity for all active
		// lifecycle transitions. Only a reopened canonical position needs the
		// persisted revision suffix; otherwise a same-size reopen/reversal can
		// collide with the old open still held in the in-memory seen set.
		fillID = fmt.Sprintf("binance_snapshot|r%d|%s|%s|%.8f|%.8f",
			sourceRevision, posID, action, previousSize, currentSize)
	}

	return Fill{
		ID:           fillID,
		Symbol:       symbol,
		Side:         tradeSide,
		PositionSide: side,
		Action:       action,
		Price:        price,
		Size:         size,
		Value:        value,
		Timestamp:    time.Now(),
	}
}

func (e *Engine) positionSignalPrice(pos *Position) float64 {
	if pos == nil {
		return 0
	}
	if pos.MarkPrice > 0 {
		return pos.MarkPrice
	}
	return pos.EntryPrice
}

func (e *Engine) binanceCloseSignalPrice(mapping *store.CopyTradePositionMapping) float64 {
	if mapping == nil {
		return 0
	}
	if price := e.latestBinanceClosedPositionPrice(mapping.Symbol, SideType(mapping.Side)); price > 0 {
		return price
	}
	return mapping.ClosePrice
}

func (e *Engine) latestBinanceClosedPositionPrice(symbol string, side SideType) float64 {
	historyProvider, ok := e.provider.(binancePositionHistoryProvider)
	if !ok || e.config == nil {
		return 0
	}

	records, err := historyProvider.GetPositionHistory(e.config.LeaderID)
	if err != nil {
		e.reportBinanceCredentialsExpired(err, "binance/position-history")
		logger.Debugf("📊 [%s] Binance 仓位历史查询失败，平仓价格回退到映射缓存: %v", e.traderID, err)
		return 0
	}

	normalizedSymbol := strings.ToUpper(strings.TrimSpace(symbol))
	var best *BinancePositionHistoryRecord
	var bestTime int64
	for i := range records {
		rec := &records[i]
		if strings.ToUpper(strings.TrimSpace(rec.Symbol)) != normalizedSymbol {
			continue
		}
		if !binanceHistorySideMatches(rec.Side, side) {
			continue
		}
		if rec.AvgClosePrice <= 0 {
			continue
		}
		if !strings.Contains(strings.ToLower(rec.Status), "closed") && rec.ClosedVolume <= 0 {
			continue
		}
		closedAt := rec.Closed
		if rec.UpdateTime > closedAt {
			closedAt = rec.UpdateTime
		}
		if best == nil || closedAt > bestTime {
			best = rec
			bestTime = closedAt
		}
	}
	if best == nil {
		return 0
	}

	logger.Debugf("📊 [%s] Binance 仓位历史补充平仓价 | %s %s avgClosePrice=%.4f closed=%d",
		e.traderID, normalizedSymbol, side, best.AvgClosePrice, best.Closed)
	return best.AvgClosePrice
}

func binanceHistorySideMatches(raw string, side SideType) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "long":
		return side == SideLong
	case "short":
		return side == SideShort
	default:
		return false
	}
}

// ============================================================================
// 统一信号匹配（核心逻辑）
// ============================================================================

// SignalMatchResult 信号匹配结果
type SignalMatchResult struct {
	ShouldFollow         bool       // 是否跟随
	Reason               string     // 原因
	Action               ActionType // 实际动作类型
	PosID                string     // 领航员仓位 ID
	MarginMode           string     // 保证金模式
	LeaderPosition       *Position  // 领航员仓位（可能为 nil，表示已平仓）
	SourceSymbol         string     // 活动映射保存的源合约
	ExecutionSymbol      string     // 活动映射保存的精确执行合约
	SourceQuoteAsset     string     // 源合约报价币种
	ExecutionSettleAsset string     // 执行合约结算币种
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
	result := e.matchCloseReduceSignal(signal, leaderPosMap)
	if result.ShouldFollow && result.PosID != "" {
		mapping, err := e.store.CopyTrade().GetMapping(e.traderID, result.PosID)
		if err != nil {
			logger.Warnf("⚠️ [%s] 查询风险降低执行合约映射失败: %v", e.traderID, err)
		} else if mapping != nil && mapping.Status == store.MappingStatusActive {
			result.SourceSymbol = mapping.SourceSymbol
			if result.SourceSymbol == "" {
				result.SourceSymbol = mapping.Symbol
			}
			result.ExecutionSymbol = mapping.ExecutionSymbol
			result.SourceQuoteAsset = mapping.SourceQuoteAsset
			result.ExecutionSettleAsset = mapping.ExecutionSettleAsset
		}
	}
	return result
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

		// 🛑 风控止损熔断：该 posId 已被账户保护止损触发，等领航员完全平掉旧 posId 才能恢复
		// 用户如果启用了二次进场（RiskReentryEnabled），由 reentryMonitor 异步推决策，不走这里
		if mapping.Status == store.MappingStatusStoppedByRisk {
			logger.Infof("🛑 [%s] 账户保护止损熔断中 | posId=%s → 忽略开仓/加仓信号（等领航员平掉旧 posId 或触发二次进场）",
				e.traderID, posID)
			continue
		}
		if mapping.Status == store.MappingStatusDetached {
			logger.Infof("🟠 [%s] 跟随仓位已脱离 | posId=%s → 仅跟踪领航员终态，不再开仓/加仓或触发 AI",
				e.traderID, posID)
			continue
		}

		if mapping.Status == "ignored" {
			// 🔑 关键区分：根据数据源（ProviderType）使用不同的判断逻辑
			if e.config.ProviderType == ProviderOKX || e.config.ProviderType == ProviderBinance {
				// OKX/Binance: ignored 表示启动前已存在的历史仓位，当前仍在持仓中时不跟随。
				// Binance 如果该仓位先消失，checkIgnoredPositionsClosed 会标记 closed，后续重新开仓可按新仓处理。
				logger.Infof("📊 [%s] 历史仓位 | posId=%s status=ignored → 不跟随（%s）",
					e.traderID, posID, e.config.ProviderType)
				continue
			}

			// Hyperliquid: 需要判断是否是真正的重新开仓
			// 原因：Hyperliquid 的 posId 是虚拟的（symbol_side），平仓后重开会复用同一个 posId
			// 通过 ActionOpen 判断是否是全新开仓
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
			ShouldFollow:         true,
			Reason:               fmt.Sprintf("已跟随仓位(posId=%s)，加仓", posID),
			Action:               ActionAdd,
			PosID:                posID,
			MarginMode:           addMapping.MarginMode,
			LeaderPosition:       addPosition,
			SourceSymbol:         addMapping.SourceSymbol,
			ExecutionSymbol:      addMapping.ExecutionSymbol,
			SourceQuoteAsset:     addMapping.SourceQuoteAsset,
			ExecutionSettleAsset: addMapping.ExecutionSettleAsset,
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
		// size 增量守卫（OKX/Binance 共用）：到达兜底分支说明第二轮没找到
		// size 增加的仓位，lastKnownSize 有值时几乎必然是迟到/重复/部分成交
		// 的残余信号，盲目加仓会造成重复建仓。lastKnownSize=0（旧数据）时
		// 保留原兜底加仓行为。
		if (e.config.ProviderType == ProviderBinance || e.config.ProviderType == ProviderOKX) &&
			singleActiveMapping != nil &&
			singleActiveMapping.LastKnownSize > 0 &&
			singleActivePos.Size <= singleActiveMapping.LastKnownSize+binancePositionSizeEpsilon {
			logger.Infof("📊 [%s] %s 未检测到 size 增加 | posId=%s 上次=%.4f 当前=%.4f → 跳过迟到/重复成交",
				e.traderID, e.config.ProviderType, posID, singleActiveMapping.LastKnownSize, singleActivePos.Size)
			return &SignalMatchResult{
				ShouldFollow: false,
				Reason:       fmt.Sprintf("%s 持仓 size 未增加，可能是迟到/重复成交", e.config.ProviderType),
			}
		}
		logger.Infof("📊 [%s] 唯一 active 仓位 | posId=%s status=active → 加仓",
			e.traderID, posID)
		return &SignalMatchResult{
			ShouldFollow:         true,
			Reason:               fmt.Sprintf("已跟随仓位(posId=%s)，加仓", posID),
			Action:               ActionAdd,
			PosID:                posID,
			MarginMode:           singleActiveMapping.MarginMode,
			LeaderPosition:       singleActivePos,
			SourceSymbol:         singleActiveMapping.SourceSymbol,
			ExecutionSymbol:      singleActiveMapping.ExecutionSymbol,
			SourceQuoteAsset:     singleActiveMapping.SourceQuoteAsset,
			ExecutionSettleAsset: singleActiveMapping.ExecutionSettleAsset,
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
			if leaderPos.Size < mapping.LastKnownSize*NearZeroThreshold {
				// 剩余不足阈值 = 视为全平
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

	if e.config.ProviderType == ProviderBinance && len(activeMappings) == 1 {
		mapping := activeMappings[0]
		leaderPos := leaderPosMap[mapping.LeaderPosID]
		if leaderPos != nil &&
			mapping.LastKnownSize > 0 &&
			mapping.LastKnownSize <= leaderPos.Size+binancePositionSizeEpsilon {
			logger.Infof("📊 [%s] Binance 未检测到 size 减少 | posId=%s 上次=%.4f 当前=%.4f → 跳过迟到/重复成交",
				e.traderID, mapping.LeaderPosID, mapping.LastKnownSize, leaderPos.Size)
			return &SignalMatchResult{
				ShouldFollow: false,
				Reason:       "Binance 持仓 size 未减少，可能是迟到/重复成交",
			}
		}
	}

	// 兜底：如果只有一个映射且 lastKnownSize 为 0（旧数据），使用 fill.Size 判断
	if len(activeMappings) == 1 {
		mapping := activeMappings[0]
		leaderPos := leaderPosMap[mapping.LeaderPosID]

		if leaderPos != nil {
			// 用 fill.Size vs leaderPos.Size 判断是否是全平
			if fill.Size >= leaderPos.Size*FullCloseThreshold {
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
		if e.isSmartMoneyMode() {
			// The snapshot may have been generated immediately before the leader
			// hid positions or the credential expired. Never process it against
			// cached state; release the dedup key so a complete healthy snapshot
			// can replay the risk-reducing change after recovery.
			e.unmarkFillSources(fill)
			e.stats.SignalsSkipped++
			return
		}
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
	invalidSourceRiskIncrease := e.blockInvalidSourceRiskIncrease(signal, matchResult)

	// ========================================
	// Step 3: 计算跟单仓位（基于持仓变化量）
	// ========================================
	copySize := 0.0
	var warnings []Warning
	if !invalidSourceRiskIncrease && (matchResult.Action == ActionOpen || matchResult.Action == ActionAdd) {
		copySize, warnings = e.calculateCopySizeByPositionChange(signal, matchResult)
	} else {
		logger.Debugf("📊 [%s] %s 不需要计算开仓金额，跳过金额锚定", e.traderID, matchResult.Action)
	}

	// 记录所有预警（不阻止交易）
	for _, w := range warnings {
		e.logWarning(w)
	}

	// Reserve the normalized source transition before any deterministic
	// execution filter. This guarantees that every recognized/followed signal
	// has an auditable terminal intent, including zero/minimum/risk skips.
	dec := e.buildDecisionV2(signal, matchResult, copySize)
	if !e.reserveExecutionIntent(&dec) {
		return
	}

	if invalidSourceRiskIncrease {
		e.commitSkippedReservedIntent(&dec, "SOURCE_VALUE_UNAVAILABLE")
		logger.Infof("🎯 [%s] ⏭️ %s跳过 | %s | USD 价值换算不可用", e.traderID, matchResult.Action, fill.Symbol)
		e.stats.SignalsSkipped++
		return
	}

	if (matchResult.Action == ActionOpen || matchResult.Action == ActionAdd) && copySize == 0 {
		reasonCode := "MIN_NOTIONAL"
		for _, w := range warnings {
			if w.Type == WarnLeaderEquityUnavailable {
				reasonCode = "SOURCE_DATA_UNAVAILABLE"
				break
			}
		}
		e.commitSkippedReservedIntent(&dec, reasonCode)
		logger.Infof("🎯 [%s] ⏭️ %s跳过 | %s | 金额为零（低于阈值或余额不足）", e.traderID, matchResult.Action, fill.Symbol)
		e.stats.SignalsSkipped++
		return
	}

	// Ordinary leader opens/adds preserve the proportional source transition.
	// Exchange minimums and action-specific rounding are resolved immediately
	// before submission by the execution venue. Copy Guard only manages the
	// post-fill stop; its AI/cycle/portfolio budgets must never resize a normal
	// leader transition.
	if matchResult.Action == ActionOpen || matchResult.Action == ActionAdd {
		dec.PositionSizeUSD = copySize
	}

	// ========================================
	// Step 4: 构造 Decision
	// ========================================
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
		// 通道满时旧逻辑直接丢弃，但 fill 已 markSeen → 该信号永久丢失。
		// 释放去重标记让下轮 poll 重放（fill 仍在回看窗口内时可恢复），
		// 并升级为 ERROR：这是会造成漏单的严重拥塞信号。
		e.UnmarkDecisionSources(&dec)
		if dec.ExecutionIntentID > 0 && e.store != nil {
			_ = e.store.CopyTrade().UpdateExecutionIntent(dec.ExecutionIntentID, store.ExecutionIntentFailed, "DECISION_CHANNEL_BUSY", "decision channel is full before execution", "", 0, 0, 0)
		}
		logger.Errorf("❌ [%s] 决策通道已满，丢弃决策并释放去重标记等待重放 | %s %s fillID=%s",
			e.traderID, dec.Action, dec.Symbol, fill.ID)
	}
}

func (e *Engine) commitSkippedReservedIntent(dec *decision.Decision, reasonCode string) {
	if dec == nil || dec.ExecutionIntentID <= 0 || e.store == nil {
		return
	}
	side := "long"
	if strings.HasSuffix(dec.Action, "short") {
		side = "short"
	}
	var err error
	if strings.HasPrefix(dec.Action, "open_") {
		err = e.store.CopyTrade().CommitIgnoredLeaderTransition(store.IgnoredLeaderTransition{
			IntentID: dec.ExecutionIntentID, TraderID: e.traderID, LeaderID: e.config.LeaderID,
			LeaderPosID: dec.LeaderPosID, SourceRevision: dec.SourceRevision, Symbol: dec.Symbol,
			Side: side, MarginMode: dec.MarginMode, LeaderTargetSize: dec.LeaderPosSize, ReasonCode: reasonCode,
		})
	} else {
		err = e.store.CopyTrade().CommitSkippedLeaderTransition(dec.ExecutionIntentID, e.traderID, dec.LeaderPosID,
			dec.SourceRevision, dec.LeaderPosSize, reasonCode)
	}
	if err != nil {
		logger.Errorf("❌ [%s] 提交跳过执行意图失败 | intent=%d reason=%s: %v", e.traderID, dec.ExecutionIntentID, reasonCode, err)
		_ = e.store.CopyTrade().UpdateExecutionIntent(dec.ExecutionIntentID, store.ExecutionIntentReconciling,
			"SOURCE_REVALIDATION_REQUIRED", reasonCode+" local commit failed: "+err.Error(), "", 0, 0, 0)
		e.UnmarkDecisionSources(dec)
	}
}

// reserveExecutionIntent closes the gap between source matching and exchange
// acknowledgement. Until the mapping advances, every fill for the same leader
// position observes the same next revision and therefore collapses to one
// durable intent instead of queuing duplicate opens/reduces.
func (e *Engine) reserveExecutionIntent(dec *decision.Decision) bool {
	if dec == nil || e.store == nil || dec.LeaderPosID == "" || dec.Action == "hold" {
		return true
	}
	lastRevision, err := e.store.CopyTrade().GetSourceSnapshotRevision(e.traderID, dec.LeaderPosID)
	if err != nil {
		logger.Errorf("❌ [%s] 查询执行意图修订号失败 | posId=%s: %v", e.traderID, dec.LeaderPosID, err)
		e.UnmarkDecisionSources(dec)
		return false
	}
	revision := lastRevision + 1
	if revision <= 0 {
		revision = 1
	}
	side := "long"
	if strings.HasSuffix(dec.Action, "short") {
		side = "short"
	}
	canonicalKey := fmt.Sprintf("leader|%s|%s|%d", e.traderID, dec.LeaderPosID, revision)
	dec.ClientOrderID = stableClientOrderID(e.traderID, canonicalKey, dec.Action)
	intent, claimed, err := e.store.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: e.traderID, LeaderPosID: dec.LeaderPosID, SourceRevision: revision,
		SourceFillID: dec.SourceFillID, SourceKind: "LEADER_TRANSITION", CanonicalKey: canonicalKey,
		Action: dec.Action, Symbol: dec.Symbol, Side: side,
		MarginMode: dec.MarginMode, LeaderTargetSize: dec.LeaderPosSize,
		RequestedNotional: dec.PositionSizeUSD, ClientOrderID: dec.ClientOrderID,
		SourceFillIDs: dec.SourceFillIDs,
	})
	if err != nil {
		logger.Errorf("❌ [%s] 预留执行意图失败 | posId=%s action=%s: %v", e.traderID, dec.LeaderPosID, dec.Action, err)
		e.UnmarkDecisionSources(dec)
		return false
	}
	if !claimed {
		if intent.Status == store.ExecutionIntentFailed && intent.ReasonCode == "MANUAL_REVIEW_REQUIRED" {
			logger.Errorf("🚨 [%s] 源过渡被人工复核意图阻塞，不自动重放或重复下单 | intent=%d posId=%s rev=%d target=%.8f",
				e.traderID, intent.ID, dec.LeaderPosID, revision, dec.LeaderPosSize)
			return false
		}
		replayRequired := intent.Action != dec.Action || intent.LeaderTargetSize != dec.LeaderPosSize ||
			intent.Status == store.ExecutionIntentFilled || intent.Status == store.ExecutionIntentProtected ||
			intent.Status == store.ExecutionIntentSkipped || intent.Status == store.ExecutionIntentFailed
		if replayRequired {
			e.UnmarkDecisionSources(dec)
			logger.Infof("⏳ [%s] 后续源过渡等待前一意图推进，已释放去重键供下轮重放 | intent=%d posId=%s rev=%d current_target=%.8f next_target=%.8f",
				e.traderID, intent.ID, dec.LeaderPosID, revision, intent.LeaderTargetSize, dec.LeaderPosSize)
		} else {
			logger.Infof("⏭️ [%s] 同仓位同目标过渡已有执行意图，合并重复 fill | intent=%d posId=%s rev=%d action=%s fill=%s",
				e.traderID, intent.ID, dec.LeaderPosID, revision, dec.Action, dec.SourceFillID)
		}
		return false
	}
	dec.ExecutionIntentID = intent.ID
	dec.SourceRevision = revision
	dec.ExecutionStatus = store.ExecutionIntentReserved
	if intent.ClientOrderID != "" {
		dec.ClientOrderID = intent.ClientOrderID
	}
	return true
}

// blockInvalidSourceRiskIncrease enforces the value/unit conversion fail-closed
// boundary for position-snapshot sources that require explicit USD
// normalization. A missing OKX contract notional, catalog entry or FX rate can
// never authorize an open/add; reduce/close remains available from the exact
// persisted mapping and live follower position.
func (e *Engine) blockInvalidSourceRiskIncrease(signal *TradeSignal, match *SignalMatchResult) bool {
	if signal == nil || signal.Fill == nil || match == nil {
		return false
	}
	fill := signal.Fill
	requiresValidatedValue := e.isSmartMoneyMode() ||
		(e.config != nil && e.config.ProviderType == ProviderOKX && strings.HasPrefix(fill.ID, "okx_snapshot|"))
	if !requiresValidatedValue {
		return false
	}
	if match.Action != ActionOpen && match.Action != ActionAdd {
		return false
	}
	if fill.ValueUSDValid && fill.Value > 0 {
		return false
	}
	reason := strings.TrimSpace(fill.ValueError)
	if reason == "" {
		reason = "USD value conversion is unavailable"
	}
	logger.Warnf("🚫 [%s] 领航员快照禁止风险增加 | provider=%s %s %s | %s", e.traderID, e.config.ProviderType, match.Action, fill.Symbol, reason)

	traderName := e.traderID
	if e.store != nil {
		traderName = e.store.Trader().ResolveDisplayName(e.traderID)
	}
	dedup := fmt.Sprintf("source_value|%s|%s|%s|%s|%d", e.traderID, e.config.ProviderType, fill.Symbol, match.Action, time.Now().Unix()/3600)
	notifier.Notify(notifier.Alert{
		Time: time.Now(), Category: "copy_trade", TraderID: e.traderID, TraderName: traderName,
		Title:   fmt.Sprintf("跟单已跳过 %s 风险增加", fill.Symbol),
		Body:    fmt.Sprintf("领航员: %s\n合约: %s\n动作: %s\n原因: %s\n处理: 本次不开仓/不加仓并重建基线；后续减仓、平仓仍允许。", e.config.LeaderID, fill.Symbol, match.Action, reason),
		RateKey: dedup, DedupKey: dedup,
	})
	if e.store != nil {
		if err := e.store.CopyTrade().LogCopyEvent(&store.CopyTradeEvent{
			TraderID: e.traderID, LeaderID: e.config.LeaderID, ProviderType: string(e.config.ProviderType),
			Category: store.CopyEventCategoryError, EventType: "SOURCE_VALUE_UNAVAILABLE",
			Severity: store.CopyEventSeverityWarn, Symbol: fill.Symbol, Side: string(fill.PositionSide), LeaderPosID: match.PosID,
			SignalID: fill.ID, Status: "skipped", Summary: "领航员风险增加因合约单位或美元名义不可验证被拒绝",
			Detail: map[string]interface{}{"action": match.Action, "value_currency": fill.ValueCurrency, "reason": reason}, DedupKey: dedup,
		}); err != nil {
			logger.Warnf("⚠️ [%s] 保存领航员价值异常事件失败: %v", e.traderID, err)
		}
	}
	return true
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
		Symbol:          fill.Symbol,
		Action:          e.mapAction(match.Action, fill.PositionSide),
		IsCopyTrade:     true,
		CopyTradeAction: string(match.Action),
		Reasoning:       fmt.Sprintf("Copy trading: %s following %s leader %s", match.Action, e.config.ProviderType, e.config.LeaderID),
		EntryPrice:      fill.Price,
		LeaderPosID:     match.PosID,
		LeaderPosSize:   leaderPosSize,    // 传递领航员当前持仓数量
		MarginMode:      match.MarginMode, // 直接使用匹配结果中的 marginMode
		SourceFillID:    fill.ID,          // 执行瞬态失败时用于释放去重标记重放
		SourceFillIDs:   append([]string(nil), fill.SourceFillIDs...),
		SourceSymbol:    fill.Symbol,
		ValueCurrency:   fill.ValueCurrency,
	}
	if match.SourceSymbol != "" {
		dec.SourceSymbol = match.SourceSymbol
	}
	dec.ExecutionSymbol = match.ExecutionSymbol
	dec.ExecutionSettleAsset = match.ExecutionSettleAsset
	if dec.ValueCurrency == "" {
		dec.ValueCurrency = match.SourceQuoteAsset
	}

	// SyncMarginMode=false：新开仓不同步领航员的全仓/逐仓模式，
	// 清空后由 auto_trader 回落到跟随者自己的 is_cross_margin 配置。
	// 仅限 ActionOpen——加仓/减仓/平仓必须沿用 mapping 记录的实际模式
	// 操作已有仓位，否则会在另一模式下开出第二个仓位（OKX 双仓风险）。
	if !e.config.SyncMarginMode && match.Action == ActionOpen {
		dec.MarginMode = ""
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
	// 减仓：只按本次权威仓位变化计算比例。
	// ============================================================
	if match.Action == ActionReduce {
		ratio := e.calculateReduceRatioV2(signal, match)

		if e.config.ProviderType == ProviderBinance {
			dec.CloseRatio = ratio
			dec.Reasoning = fmt.Sprintf("Copy trading: reduce %.0f%% following %s leader %s",
				ratio*100, e.config.ProviderType, e.config.LeaderID)
			logger.Infof("📊 [%s] Binance 部分平仓 %.1f%% marginMode=%s（以实时持仓快照为准，不使用累计减仓触发全平）",
				e.traderID, ratio*100, dec.MarginMode)
			return dec
		}

		// 边界保护：单次减仓超过阈值时，直接全量平仓。
		// 不再在交易所确认前累加 accumulated_reduce_ratio：微量减仓按
		// 用户策略明确忽略，可执行减仓由 mapping 的源修订号确认。
		if ratio >= FullCloseThreshold {
			logger.Infof("📊 [%s] 减仓比例 %.1f%% ≥ %.0f%%，转为全量平仓", e.traderID, ratio*100, FullCloseThreshold*100)
			dec.CloseRatio = 0
			// 🔑 action 同步改为 close_*：executor 对 reduce/close 走同一平仓链路
			//（CloseRatio 决定量），但 integration 的 mapping 关闭、Copy Guard 周期
			// 收尾、保护单撤销都按 action 前缀分发——保持 reduce_* 会导致跟随者
			// 已空仓而交易所残留 live 止损单（孤儿单误触发风险）。
			dec.Action = e.mapAction(ActionClose, fill.PositionSide)
			dec.Reasoning = fmt.Sprintf("Copy trading: close (reduce %.0f%% → full close) following %s leader %s",
				ratio*100, e.config.ProviderType, e.config.LeaderID)
		} else {
			dec.CloseRatio = ratio
			dec.Reasoning = fmt.Sprintf("Copy trading: reduce %.0f%% following %s leader %s",
				ratio*100, e.config.ProviderType, e.config.LeaderID)
			logger.Infof("📊 [%s] 部分平仓 %.3f%% marginMode=%s（不累积微量减仓）",
				e.traderID, ratio*100, dec.MarginMode)
		}
	}

	// ============================================================
	// 平仓：全量平仓
	// ============================================================
	if match.Action == ActionClose {
		dec.CloseRatio = 0 // 0 = 全量平仓
		logger.Infof("📊 [%s] 全量平仓 marginMode=%s", e.traderID, dec.MarginMode)
	}

	// 🔐 决策级稳定 clOrdId（重试/重放幂等的最后一道硬闸）：
	// 同一 fill + 同一最终 action 恒定生成同一 ID。若某次下单请求实际已被
	// 交易所接受但本地误判失败（限流误判/响应丢失），重试或 UnmarkSeen
	// 重放的第二单会被交易所以 duplicated clOrdId 拒绝，杜绝重复建仓。
	// 放在 action 终态确定之后（reduce 可能已升级为 close）。
	// 重入决策不走此处（integration 层有独立的 cgr 周期级 ID）。
	dec.ClientOrderID = stableClientOrderID(e.traderID, fill.ID, dec.Action)

	return dec
}

// stableClientOrderID 生成决策级稳定的客户端订单 ID。
// 格式 "ct"+16位十六进制（FNV-1a 64），满足 OKX clOrdId 的
// 字母数字、≤32 字符约束；其他交易所执行器未实现幂等接口时忽略该值。
func stableClientOrderID(traderID, fillID, action string) string {
	h := fnv.New64a()
	h.Write([]byte(traderID))
	h.Write([]byte{'|'})
	h.Write([]byte(fillID))
	h.Write([]byte{'|'})
	h.Write([]byte(action))
	return fmt.Sprintf("ct%016x", h.Sum64())
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

// calculateCopySizeByPositionChange 基于持仓变化量计算跟单仓位大小
// 🔑 核心改进：用 (当前持仓size - 上次记录size) × 价格 作为交易价值
// 解决问题：Hyperliquid 大订单被拆成多个 fills，用 fill.Value 只能捕获第一个 fill 的价值
// 改进后：不管拆成多少个 fills，只要最终持仓变化正确，跟单金额就准确
func (e *Engine) calculateCopySizeByPositionChange(signal *TradeSignal, match *SignalMatchResult) (float64, []Warning) {
	var warnings []Warning
	fill := signal.Fill

	// 领航员的账户权益（用作比例计算分母）
	//
	// ⚠️ Binance 专属锚定：fill.Value 是跟随者镜像价值（已被币安按定比缩放），
	// 与 signal.LeaderEquity（领航员真实权益）量纲不一致——直接相除会让
	// 跟单金额偏小约 (leader_marginBalance / follower_invest) 倍。
	// 正确锚定：copy-portfolio/detail-list.marginBalance（跟随者实时权益），
	// 与 fill.Value 严格同量纲；Provider 内部已实现 stale cache 兜底，
	// 让网络抖动等临时失败不会回退到错误量纲。
	leaderEquity := signal.LeaderEquity
	anchorSource := "leader_equity"
	if e.config.ProviderType == ProviderBinance && e.config.BinanceSourceMode != BinanceSourceSmartMoney {
		anchorEquity, src, anchorErr := e.resolveBinanceAnchorEquity()
		if anchorErr == nil && anchorEquity > 0 {
			leaderEquity = anchorEquity
			anchorSource = src
		} else if anchorErr != nil {
			// 凭证类错误兜底告警（避免用户错过过期通知）
			e.reportBinanceCredentialsExpired(anchorErr, "calculateCopySize/anchor")
			// fill.Value belongs to the Binance copy-portfolio scale. Falling
			// back to leader equity mixes units and silently produces a wrong
			// proportional target. Fail closed and let the same source revision
			// retry after the authoritative anchor recovers.
			logger.Warnf("⚠️ [%s] ANCHOR_UNAVAILABLE Binance 权威权益锚点不可用，暂停本次比例跟单: %v",
				e.traderID, anchorErr)
			if _, productionProvider := e.provider.(*BinanceProvider); productionProvider {
				warnings = append(warnings, Warning{
					Timestamp: time.Now(),
					Symbol:    fill.Symbol,
					Type:      WarnLeaderEquityUnavailable,
					Message:   fmt.Sprintf("Binance 权威权益锚点不可用，暂停并等待重试: %v", anchorErr),
					Executed:  false,
				})
				return 0, warnings
			}
			// Test/in-process providers put their authoritative equity directly
			// on the signal and have no private detail endpoint.
		}
	}
	if leaderEquity <= 0 {
		// 领航员权益不可用（API 失败/限频/数据异常）。旧逻辑置 1 防除零，
		// 但 leaderTradeValue/1 × followerEquity 会产生远超账户承受力的
		// 跟单金额（资金安全事故）。正确做法：拒绝本次跟单并告警，
		// 由 processSignal 释放去重标记，等下轮 poll 状态恢复后重放。
		logger.Errorf("❌ [%s] 领航员权益不可用(=%.4f)，拒绝本次跟单 | %s %s",
			e.traderID, leaderEquity, fill.Symbol, match.Action)
		warnings = append(warnings, Warning{
			Timestamp: time.Now(),
			Symbol:    fill.Symbol,
			Type:      WarnLeaderEquityUnavailable,
			Message:   "领航员账户权益不可用，跳过本次跟单（等待数据恢复后重试）",
			Executed:  false,
		})
		return 0, warnings
	}

	// Proportional copy always targets the same percentage of total account
	// equity as the leader. Available balance is strictly an execution
	// constraint and must never change the ordinary target.
	followerEquity := float64(0)
	if e.getFollowerEquity != nil {
		followerEquity = e.getFollowerEquity()
	} else if e.getFollowerBalance != nil {
		// Compatibility for directly constructed engines. TraderIntegration
		// always injects the authoritative total-equity callback.
		followerEquity = e.getFollowerBalance()
	}
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

	// ========================================
	// 计算领航员实际交易价值
	// ========================================
	var leaderTradeValue float64

	// 🔑 OKX/Binance: 直接使用 fill.Value（API 返回完整订单价值，不存在拆分问题）
	// 🔑 Hyperliquid: 使用持仓变化量计算（解决大订单拆分导致金额偏小的问题）
	if e.config.ProviderType == ProviderOKX || e.config.ProviderType == ProviderBinance {
		leaderTradeValue = fill.Value
		logger.Infof("📊 [%s] %s计算 | 使用 fill.Value=%.2f", e.traderID, e.config.ProviderType, fill.Value)
	} else if match.Action == ActionOpen {
		// Hyperliquid 新开仓：用当前持仓的 size × price 作为交易价值
		if match.LeaderPosition != nil {
			leaderTradeValue = match.LeaderPosition.Size * fill.Price
			logger.Infof("📊 [%s] 开仓计算 | 持仓数量=%.4f 价格=%.4f → 交易价值=%.2f",
				e.traderID, match.LeaderPosition.Size, fill.Price, leaderTradeValue)
		} else {
			// 回退：无持仓信息时使用 fill.Value
			leaderTradeValue = fill.Value
			logger.Warnf("⚠️ [%s] 开仓无持仓信息，使用 fill.Value=%.2f", e.traderID, fill.Value)
		}
	} else if match.Action == ActionAdd {
		// Hyperliquid 加仓：用 (当前size - lastKnownSize) × price 作为交易价值
		if match.LeaderPosition != nil && e.store != nil {
			// 获取上次记录的持仓数量
			mapping, err := e.store.CopyTrade().GetMapping(e.traderID, match.PosID)
			lastKnownSize := float64(0)
			if err == nil && mapping != nil {
				lastKnownSize = mapping.LastKnownSize
			}

			// 计算持仓变化量
			sizeChange := match.LeaderPosition.Size - lastKnownSize
			if sizeChange > 0 {
				leaderTradeValue = sizeChange * fill.Price
				logger.Infof("📊 [%s] 加仓计算 | 当前=%.4f 上次=%.4f 变化=%.4f 价格=%.4f → 交易价值=%.2f",
					e.traderID, match.LeaderPosition.Size, lastKnownSize, sizeChange, fill.Price, leaderTradeValue)
			} else {
				// 异常：size 没有增加，使用 fill.Value 作为回退
				leaderTradeValue = fill.Value
				logger.Warnf("⚠️ [%s] 加仓但 size 未增加 (当前=%.4f 上次=%.4f)，使用 fill.Value=%.2f",
					e.traderID, match.LeaderPosition.Size, lastKnownSize, fill.Value)
			}
		} else {
			// 回退：无持仓信息时使用 fill.Value
			leaderTradeValue = fill.Value
			logger.Warnf("⚠️ [%s] 加仓无持仓信息或store，使用 fill.Value=%.2f", e.traderID, fill.Value)
		}
	} else {
		// 减仓/平仓：不需要计算金额（使用比例），这里返回 0
		// 实际上 reduce/close 不走这个分支（在 buildDecisionV2 中直接计算比例）
		leaderTradeValue = fill.Value
	}

	// 领航员该笔交易占其账户的比例
	leaderTradeRatio := leaderTradeValue / leaderEquity

	// 计算跟单金额
	copySize := e.config.CopyRatio * leaderTradeRatio * followerEquity

	logger.Infof("📊 [%s] 比例计算 | %s | 领航员: 交易=%.2f 权益=%.2f(锚定=%s) 占比=%.2f%% | 跟随者: 权益=%.2f 系数=%.0f%% → 跟单=%.2f",
		e.traderID, fill.Symbol,
		leaderTradeValue, leaderEquity, anchorSource, leaderTradeRatio*100,
		followerEquity, e.config.CopyRatio*100, copySize)

	// MinTradeWarn is operational observability only. The exact venue minimum
	// depends on symbol price, minQty/minNotional and native step (OKX ctVal);
	// it is resolved at the execution boundary. In particular this layer must
	// not promote an add or use the historical fixed 10/12 USDT threshold as an
	// exchange rule.
	minTradeThreshold := minTradeNotionalOrDefault(e.config.MinTradeWarn)
	if copySize > 0 && copySize < minTradeThreshold {
		logger.Infof("📊 [%s] %s比例金额 %.2f < 运营预警阈值 %.2f；执行层将按交易所真实约束判定",
			e.traderID, match.Action, copySize, minTradeThreshold)
		warnings = append(warnings, Warning{
			Timestamp:   time.Now(),
			Symbol:      fill.Symbol,
			Type:        "below_operational_warning_threshold",
			Message:     fmt.Sprintf("%s比例金额 %.2f 低于运营预警阈值 %.2f，未改变订单金额", match.Action, copySize, minTradeThreshold),
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

// resolveBinanceAnchorEquity 解析 Binance 跟单金额比例计算的锚定权益。
//
// 量纲背景：
//   - Binance copytrade fill.Value 来自 user-position（COPY 模式），
//     是跟随者镜像仓位的价值，已被币安按定比缩放。
//   - signal.LeaderEquity 来自 lead-portfolio/detail.marginBalance，
//     是领航员真实权益，与镜像价值不同尺度。
//   - 二者直接相除 → ratio 偏小约 (leader_marginBalance / follower_invest) 倍。
//
// 修复策略：
//   - 调 copy-portfolio/detail-list 取跟随者实时权益（marginBalance）
//   - 该值与镜像价值严格同量纲，比例计算正确
//
// 返回 (anchorEquity, source, error)：
//   - source 用于日志标注："follower_margin"（成功）/ ""（失败 → 调用方降级）
//   - 错误时上层会自动降级到 signal.LeaderEquity（保留量纲错配但不会崩）
func (e *Engine) resolveBinanceAnchorEquity() (float64, string, error) {
	if e.config == nil || e.config.ProviderType != ProviderBinance {
		return 0, "", fmt.Errorf("not binance provider")
	}

	bp, ok := e.provider.(*BinanceProvider)
	if !ok || bp == nil {
		return 0, "", fmt.Errorf("provider is not *BinanceProvider")
	}

	detail, err := bp.GetCopyPortfolioDetail(e.config.LeaderID)
	if err != nil {
		return 0, "", err
	}
	if detail == nil || detail.MarginBalance <= 0 {
		return 0, "", fmt.Errorf("follower marginBalance non-positive")
	}

	// 非 FIXED_RATIO 模式下两侧 ratio 不严格相等，但跟随者权益仍是更合适的锚定
	// （比领航员 marginBalance 量纲偏差小得多）。仅打日志告警，不阻断。
	if detail.CopyMode != BinanceCopyModeFixedRatio {
		logger.Warnf("⚠️ [%s] Binance copyMode=%s 非定比，比例计算可能不严格精确（仍优于 leader_equity）",
			e.traderID, detail.CopyMode)
	}
	if detail.IsPaused {
		logger.Warnf("⚠️ [%s] Binance 领航员已暂停带单 leadStatus=%s", e.traderID, detail.LeadStatus)
	}

	return detail.MarginBalance, "follower_margin", nil
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
	logger.Warnf("⚠️ [%s] 同步杠杆已开启，但未获取到领航员 %s %s 的有效杠杆，使用默认 10x",
		e.traderID, signal.Fill.Symbol, signal.Fill.PositionSide)
	return 10
}

// ============================================================================
// 单向持仓模式标准化（Net Mode Normalization）
// ============================================================================
// OKX 领航员可能使用单向持仓模式（posSide="net"），此时 fill 的 PositionSide 和 Action
// 无法从成交记录本身推断。此函数在信号进入匹配引擎前，将 net 模式的 fill 标准化为
// 与双向持仓模式等价的语义，使后续全链路（匹配/决策/执行/映射）无需区分模式。
//
// 标准化规则：
//   - PositionSide: 从领航员持仓的 Size 符号推断（已在 GetAccountState 中标准化为 long/short）
//   - Action: 交易方向与持仓方向同向 → Open（引擎进一步区分 Open/Add）
//     交易方向与持仓方向反向 → Close（引擎进一步区分 Close/Reduce）
//   - 持仓消失时: 从数据库映射恢复方向，标记为 Close

func (e *Engine) normalizeNetModeFill(fill *Fill) {
	if fill.PositionSide != SideNet {
		return
	}

	// 查找领航员当前持仓（已在 GetAccountState 中标准化：Side=long/short, Size=正数）
	leaderPos := e.findLeaderNetPosition(fill.Symbol)

	if leaderPos != nil && leaderPos.Size > 0 {
		// 领航员有持仓：从持仓方向设置 fill 的 PositionSide
		fill.PositionSide = leaderPos.Side

		// 通过交易方向与持仓方向的关系推断动作类型
		// 同向（买入+多头 或 卖出+空头）→ 开仓/加仓
		// 反向（买入+空头 或 卖出+多头）→ 减仓/平仓
		tradeIsBuy := fill.Side == "buy"
		posIsLong := leaderPos.Side == SideLong
		if tradeIsBuy == posIsLong {
			fill.Action = ActionOpen
		} else {
			fill.Action = ActionClose
		}

		logger.Infof("📊 [%s] 单向持仓标准化 | %s | side=%s → %s %s",
			e.traderID, fill.Symbol, SideNet, fill.Action, fill.PositionSide)
		return
	}

	// 领航员持仓消失：从数据库映射恢复方向，标记为平仓
	if e.store != nil {
		mappings, err := e.store.CopyTrade().FindActiveBySymbol(e.traderID, fill.Symbol)
		if err == nil && len(mappings) > 0 {
			fill.PositionSide = SideType(mappings[0].Side)
			fill.Action = ActionClose
			logger.Infof("📊 [%s] 单向持仓标准化(平仓) | %s | 从映射恢复 side=%s",
				e.traderID, fill.Symbol, fill.PositionSide)
			return
		}
	}

	logger.Warnf("⚠️ [%s] 单向持仓标准化失败 | %s | 无持仓且无映射，无法确定方向",
		e.traderID, fill.Symbol)
}

// findLeaderNetPosition 按 symbol 查找领航员持仓（用于单向持仓模式）
// 单向模式下同一 symbol 同一 marginMode 只有一个仓位
func (e *Engine) findLeaderNetPosition(symbol string) *Position {
	e.leaderStateMu.RLock()
	defer e.leaderStateMu.RUnlock()

	if e.leaderState == nil {
		return nil
	}

	for _, pos := range e.leaderState.Positions {
		if pos.Symbol == symbol {
			return pos
		}
	}
	return nil
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
		e.recordSmartMoneySourceHealth()
		e.reportBinanceCredentialsExpired(err, "syncLeaderState")
		return err
	}
	checkedAt := time.Now()
	if state != nil && !state.Timestamp.IsZero() {
		checkedAt = state.Timestamp
	}
	if err := e.confirmSmartMoneyEmptySnapshot(state, checkedAt); err != nil {
		return err
	}
	recoveryRequired, err := e.smartMoneyRecoveryRequired(checkedAt)
	if err != nil {
		return fmt.Errorf("check Smart Money recovery state: %w", err)
	}
	if recoveryRequired {
		if err := e.rebaselineSmartMoneyRecovery(state); err != nil {
			// Do not persist the provider's HEALTHY observation or install this
			// snapshot until every no-chase baseline write succeeds atomically.
			return err
		}
	}
	e.recordSmartMoneySourceHealth()

	e.leaderStateMu.Lock()
	e.leaderState = state
	e.lastStateSync = time.Now()
	e.leaderStateMu.Unlock()

	logger.Debugf("👁️ [%s] 领航员状态同步 | 权益=%.2f 持仓数=%d",
		e.traderID, state.TotalEquity, len(state.Positions))

	// 🔍 调试日志：打印 API 返回的所有持仓详情（方便排查 posId 问题）
	for key, pos := range state.Positions {
		logger.Infof("🔍 [%s] 领航员持仓 | key=%s posId=%s | %s %s %s | size=%.4f",
			e.traderID, key, pos.PosID, pos.Symbol, pos.Side, pos.MarginMode, pos.Size)
	}

	// A restart can leave a source transition reserved before any exchange
	// submission. Only a successfully installed healthy snapshot may decide
	// whether that reservation is still actionable. Existing positions are
	// left for the normal transition detector to reclaim and re-run preflight;
	// missing positions are terminally superseded without advancing a mapping.
	e.supersedeMissingUnsubmittedIntents()

	// 🔑 检查 ignored 仓位是否已被领航员平仓
	// 如果是，标记为 closed，这样后续重新开仓可以跟随
	// 同时处理 stopped_by_risk → closed（v3 风控恢复机制）
	e.checkIgnoredPositionsClosed()

	// 🔑 风控：SL 触发对账 + 二次进场监控（SupportsCopyGuard 数据源）
	// 调用时机：领航员状态刚同步完，本地持仓由 getFollowerPositions() 实时取
	// 设计原则：放在 syncLeaderState 末尾，保证所有 active mapping 的对账用最新数据
	// Copy Guard risk reconciliation runs for supported data sources on v4+
	// configs. The version gate keeps legacy configs (e.g. a Binance follower
	// whose stored risk_stop_loss_enabled defaults to true but
	// risk_policy_version is 0) out of the stop/reentry state machine; OKX with
	// stop loss enabled is always v4, so behavior there is unchanged.
	if e.config != nil && SupportsCopyGuard(e.config.ProviderType) && e.config.RiskPolicyVersion >= 4 {
		e.checkStoppedByRisk()
		e.checkReentryConditions()
		e.refreshEstimatedBaselines()
	}

	return nil
}

func (e *Engine) supersedeMissingUnsubmittedIntents() {
	if e.store == nil {
		return
	}
	intents, err := e.store.CopyTrade().ListUnfinishedExecutionIntents(e.traderID)
	if err != nil {
		logger.Warnf("⚠️ [%s] 获取待恢复执行意图失败: %v", e.traderID, err)
		return
	}
	leaderPosMap := e.buildLeaderPosMap()
	for _, intent := range intents {
		if intent == nil ||
			intent.SourceKind != "LEADER_TRANSITION" ||
			intent.Status != store.ExecutionIntentReconciling ||
			intent.ReasonCode != "SOURCE_REVALIDATION_REQUIRED" {
			continue
		}
		if _, stillExists := leaderPosMap[intent.LeaderPosID]; stillExists {
			continue
		}
		if err := e.store.CopyTrade().SupersedeUnsubmittedExecutionIntent(
			intent.ID, e.traderID, intent.LeaderPosID,
		); err != nil {
			logger.Warnf("⚠️ [%s] 淘汰已失效的未提交执行意图失败: intent=%d posId=%s err=%v",
				e.traderID, intent.ID, intent.LeaderPosID, err)
			continue
		}
		logger.Infof("⏭️ [%s] 未提交执行意图已被健康快照淘汰 | intent=%d posId=%s reason=SOURCE_SUPERSEDED",
			e.traderID, intent.ID, intent.LeaderPosID)
	}
}

// lookupLeaderHistory 从领航员公共历史仓位里找 leaderPosId 对应的平仓记录。
// 只读补强：查询失败或未命中都返回 nil，调用方必须有降级路径。
func (e *Engine) lookupLeaderHistory(leaderPosID, symbol, side string) *OKXLeaderPositionHistoryRecord {
	hp, ok := e.provider.(LeaderPositionHistoryProvider)
	if !ok || e.config == nil {
		return nil
	}
	records, err := hp.GetPositionHistory(e.config.LeaderID, 100)
	if err != nil {
		logger.Debugf("[%s] 领航员公共历史仓位查询失败（降级为最后观测价）: %v", e.traderID, err)
		return nil
	}
	return matchLeaderHistoryRecord(records, leaderPosID, symbol, side)
}

// closeCopyGuardCycleAtLeaderExit is the single attribution close path for a
// source position that has disappeared. It is shared by verified protective
// stops and protection-establishment exits: both need the leader's final price
// before no-Guard baseline and net protection effect can be scored.
func (e *Engine) closeCopyGuardCycleAtLeaderExit(mapping *store.CopyTradePositionMapping) bool {
	if mapping == nil || e.config == nil || e.config.RiskPolicyVersion < 4 {
		return true
	}
	cycle, err := e.store.CopyTrade().GetOpenCopyGuardCycle(e.traderID, mapping.LeaderPosID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true
		}
		logger.Warnf("⚠️ [%s] 查询待关闭 Copy Guard 周期失败: %v | posId=%s", e.traderID, err, mapping.LeaderPosID)
		return false
	}

	// Prefer authoritative public leader history. When it is delayed, close
	// with the latest healthy observation and let refreshEstimatedBaselines
	// calibrate the outcome later.
	closePrice, baselineSource := cycle.LastObservedPrice, "last_observed"
	if rec := e.lookupLeaderHistory(mapping.LeaderPosID, cycle.Symbol, cycle.Side); rec != nil && rec.ExitPrice > 0 {
		closePrice, baselineSource = rec.ExitPrice, "leader_history"
	}
	attempts, _ := e.store.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	baseline, baselineOK := ComputeOwnPathBaseline(cycle, attempts, closePrice)
	if !baselineOK {
		baseline = cycle.BaselineRealizedPnL
		if cycle.LeaderEntryPrice > 0 && closePrice > 0 {
			move := (closePrice - cycle.LeaderEntryPrice) / cycle.LeaderEntryPrice
			if cycle.Side == "short" {
				move = -move
			}
			baseline += cycle.BaselineNotional * move
		}
	}

	// The observation summary must be persisted before CloseCopyGuardCycle
	// invalidates all still-open evaluation state.
	emitWatchSummary(e.store.CopyTrade(), e.traderID, cycle, closePrice)
	// CloseCopyGuardCycle may synchronously emit the final reconciled summary.
	// Persist evidence quality first so every downstream reader observes the
	// same attribution source.
	if err := e.store.CopyTrade().SetCopyGuardBaselineSource(cycle.ID, baselineSource); err != nil {
		logger.Warnf("⚠️ [%s] Copy Guard 基线来源保存失败: %v | cycle=%d", e.traderID, err, cycle.ID)
		return false
	}
	if err := e.store.CopyTrade().CloseCopyGuardCycle(cycle.ID, store.CopyGuardLeaderClosed, cycle.ActualPnL, baseline, cycle.Fees, cycle.FundingFee, cycle.LiquidationPenalty, cycle.Slippage); err != nil {
		logger.Warnf("⚠️ [%s] Copy Guard 周期关闭失败: %v | cycle=%d posId=%s", e.traderID, err, cycle.ID, mapping.LeaderPosID)
		return false
	}
	_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
		CycleID: cycle.ID, TraderID: e.traderID, Type: "LEADER_CLOSED", Price: closePrice,
		Metadata: map[string]interface{}{
			"baseline_estimated": baselineSource != "leader_history",
			"baseline_source":    baselineSource,
		},
	})
	return true
}

func matchLeaderHistoryRecord(records []OKXLeaderPositionHistoryRecord, leaderPosID, symbol, side string) *OKXLeaderPositionHistoryRecord {
	for i := range records {
		r := &records[i]
		if r.PosID != leaderPosID {
			continue
		}
		if symbol != "" && r.Symbol != "" && r.Symbol != symbol {
			continue
		}
		// posSide=net（单向模式）无法直接比对方向，只按 posId+symbol 匹配
		if side != "" && r.Side != "" && !strings.EqualFold(r.Side, "net") && !strings.EqualFold(r.Side, side) {
			continue
		}
		return r
	}
	return nil
}

// refreshEstimatedBaselines 用领航员公共历史仓位补全"最后观测价估算"的基线。
// 周期关闭时如果公共历史还没出记录，先按估算关闭；这里每分钟重试一次，
// 命中后校准 baseline_pnl 并重算保护效果（最长回看 24 小时，覆盖公共
// 历史接口延迟出记录/程序重启错过窗口的场景）。
func (e *Engine) refreshEstimatedBaselines() {
	if time.Since(e.lastBaselineRefresh) < time.Minute {
		return
	}
	e.lastBaselineRefresh = time.Now()
	hp, ok := e.provider.(LeaderPositionHistoryProvider)
	if !ok || e.store == nil {
		return
	}
	cycles, err := e.store.CopyTrade().ListCopyGuardCyclesWithEstimatedBaseline(e.traderID, 24*time.Hour)
	if err != nil || len(cycles) == 0 {
		return
	}
	records, err := hp.GetPositionHistory(e.config.LeaderID, 100)
	if err != nil {
		return
	}
	for _, cycle := range cycles {
		rec := matchLeaderHistoryRecord(records, cycle.LeaderPosID, cycle.Symbol, cycle.Side)
		if rec == nil || rec.ExitPrice <= 0 {
			continue
		}
		// own-path 口径；attempt 数据不完整时回退旧口径（影子名义）
		attempts, _ := e.store.CopyTrade().ListCopyGuardAttempts(cycle.ID)
		baseline, ok := ComputeOwnPathBaseline(cycle, attempts, rec.ExitPrice)
		if !ok {
			baseline = cycle.BaselineRealizedPnL
			if cycle.LeaderEntryPrice > 0 {
				move := (rec.ExitPrice - cycle.LeaderEntryPrice) / cycle.LeaderEntryPrice
				if cycle.Side == "short" {
					move = -move
				}
				baseline += cycle.BaselineNotional * move
			}
		}
		if err := e.store.CopyTrade().UpdateCopyGuardBaselineOutcome(cycle.ID, baseline, "leader_history"); err != nil {
			continue
		}
		_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: e.traderID, Type: "BASELINE_CALIBRATED", Price: rec.ExitPrice, PnL: baseline, Metadata: map[string]interface{}{"leader_close_price": rec.ExitPrice, "leader_closed_at": rec.ClosedAt.UTC().Format(time.RFC3339), "close_type": rec.CloseType, "baseline_source": "leader_history"}})
		logger.Infof("📐 [%s] Copy Guard 基线已用领航员历史校准 | cycle=%d closeAvgPx=%.6f baseline=%.4f", e.traderID, cycle.ID, rec.ExitPrice, baseline)
	}
}

// checkIgnoredPositionsClosed 检查 ignored 仓位是否已被领航员平仓
// 当历史仓位被领航员平仓后，将状态从 ignored 改为 closed
// 这样如果领航员重新开仓（即使 posId 被复用），也能正确跟随
//
// 同时处理 stopped_by_risk 状态（v3 风控）：
// 当账户保护止损触发后，等领航员完全平掉旧 posId → stopped_by_risk → closed
// 下次他开同币种同方向时会用新 posId（OKX）或同一 posId 但走 closed → new 路径（理论 fallback）
func (e *Engine) checkIgnoredPositionsClosed() {
	if e.store == nil {
		return
	}

	// 获取领航员当前持仓的 posId 集合（一次构建，两个分支共用）
	leaderPosMap := e.buildLeaderPosMap()
	leaderPosIds := make(map[string]bool)
	for posId := range leaderPosMap {
		leaderPosIds[posId] = true
	}

	// ============================================================
	// 处理 ignored 状态
	// ============================================================
	ignoredMappings, err := e.store.CopyTrade().ListIgnoredMappings(e.traderID)
	if err != nil {
		logger.Warnf("⚠️ [%s] 获取 ignored 映射失败: %v", e.traderID, err)
	} else {
		for _, mapping := range ignoredMappings {
			if _, exists := leaderPosIds[mapping.LeaderPosID]; !exists {
				// ignored 仓位不在领航员持仓中 → 说明已被平仓 → 改为 closed
				if err := e.store.CopyTrade().MarkIgnoredAsClosed(e.traderID, mapping.LeaderPosID); err != nil {
					logger.Warnf("⚠️ [%s] 更新 ignored→closed 失败: %v (posId=%s)",
						e.traderID, err, mapping.LeaderPosID)
				} else {
					logger.Infof("📊 [%s] 历史仓位已平仓 | posId=%s %s %s → ignored→closed",
						e.traderID, mapping.LeaderPosID, mapping.Symbol, mapping.Side)
				}
			}
		}
	}

	// ============================================================
	// 处理 stopped_by_risk 状态（v3 风控恢复机制）
	// 当领航员完全平掉旧 posId 后，把熔断映射也置为 closed，下次新 posId 可以正常跟随
	// ============================================================
	stoppedMappings, err := e.store.CopyTrade().ListStoppedByRiskMappings(e.traderID)
	if err != nil {
		logger.Warnf("⚠️ [%s] 获取 stopped_by_risk 映射失败: %v", e.traderID, err)
		return
	}
	for _, mapping := range stoppedMappings {
		if _, exists := leaderPosIds[mapping.LeaderPosID]; !exists {
			if !e.closeCopyGuardCycleAtLeaderExit(mapping) {
				continue
			}
			// 领航员完全平掉了该 posId → 熔断解除（标 closed）
			if err := e.store.CopyTrade().MarkStoppedByRiskAsClosed(e.traderID, mapping.LeaderPosID); err != nil {
				logger.Warnf("⚠️ [%s] 更新 stopped_by_risk→closed 失败: %v (posId=%s)",
					e.traderID, err, mapping.LeaderPosID)
			} else {
				logger.Infof("✅ [%s] 风控熔断解除 | posId=%s %s %s → stopped_by_risk→closed（领航员已平掉旧仓）",
					e.traderID, mapping.LeaderPosID, mapping.Symbol, mapping.Side)
			}
		}
	}

	// Detached mappings have no follower position and no stop-loss evidence.
	// They remain source-tracked only until the leader's position disappears.
	detachedMappings, err := e.store.CopyTrade().ListDetachedMappings(e.traderID)
	if err != nil {
		logger.Warnf("⚠️ [%s] 获取 detached 映射失败: %v", e.traderID, err)
		return
	}
	for _, mapping := range detachedMappings {
		if _, exists := leaderPosIds[mapping.LeaderPosID]; exists {
			continue
		}
		if !e.closeCopyGuardCycleAtLeaderExit(mapping) {
			continue
		}
		if err := e.store.CopyTrade().MarkDetachedAsClosed(e.traderID, mapping.LeaderPosID); err != nil {
			logger.Warnf("⚠️ [%s] 更新 detached→closed 失败: %v (posId=%s)", e.traderID, err, mapping.LeaderPosID)
		} else {
			logger.Infof("📊 [%s] 脱离仓位的领航员生命周期结束 | posId=%s → detached→closed",
				e.traderID, mapping.LeaderPosID)
		}
	}
}

func (e *Engine) initSeenFills() {
	since := time.Now().Add(-5 * time.Minute)
	fills, err := e.provider.GetFills(e.config.LeaderID, since)
	if err != nil {
		e.reportBinanceCredentialsExpired(err, "initSeenFills")
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

// UnmarkSeen 释放 fill 的去重标记，使其可被下轮 poll 重放。
// 用途：瞬态失败（领航员权益不可用/决策通道满/执行阶段限流耗尽重试）后
// 恢复信号——前提是 fill 仍在 GetFills 回看窗口内。
// 导出方法：integration 层执行失败路径也需要调用。
func (e *Engine) UnmarkSeen(id string) {
	if id == "" {
		return
	}
	e.seenMu.Lock()
	defer e.seenMu.Unlock()
	delete(e.seenFills, id)
}

// UnmarkDecisionSources releases every raw fill collapsed into one normalized
// leader transition, plus the synthetic snapshot id. Releasing only one id can
// leave the remaining evidence permanently seen and make a failed transition
// impossible to reconstruct on the next healthy poll.
func (e *Engine) UnmarkDecisionSources(dec *decision.Decision) {
	if dec == nil {
		return
	}
	unique := make(map[string]struct{}, len(dec.SourceFillIDs)+1)
	for _, id := range append(append([]string(nil), dec.SourceFillIDs...), dec.SourceFillID) {
		if id == "" {
			continue
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		e.UnmarkSeen(id)
	}
}

func (e *Engine) unmarkFillSources(fill *Fill) {
	if fill == nil {
		return
	}
	unique := make(map[string]struct{}, len(fill.SourceFillIDs)+1)
	for _, id := range append(append([]string(nil), fill.SourceFillIDs...), fill.ID) {
		if id == "" {
			continue
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		e.UnmarkSeen(id)
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
