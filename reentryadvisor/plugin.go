// Package reentryadvisor 实现 Copy Guard 持久化 AI 重入候选调度。
//
// AI 只判断趋势反转与建议缩放比例；仓位、风险预算、止损可保护性和
// 订单幂等由 copytrade 确定性执行层最终裁决。插件每 5 秒领取到期候选，
// 使用数据哈希、最小间隔、退避与日/生命周期额度控制调用。历史人工信号只保留
// 查询兼容，不再由后台产生分析或执行。
package reentryadvisor

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"nofx/copyguardmetrics"
	"nofx/copytrade"
	"nofx/logger"
	"nofx/store"
)

const (
	pollInterval = 5 * time.Second
	// regenerateCooldown 手动"重新生成"冷却：防止高频刷新打爆 Binance 公共接口
	regenerateCooldown = 60 * time.Second
	// backfillEvery 结局盈亏回填频率：每 12 轮（约 60s）扫一次已执行信号
	backfillEvery = 12
	// maxAutoAnalysisRetries 自动 AI 分析失败后的补跑上限（首跑之外），
	// 防止模型持续故障时无限烧 API
	maxAutoAnalysisRetries = 2
	// manualAnalyzeCooldown 手动"内置 AI 分析"按钮冷却，防误触连击烧 token
	manualAnalyzeCooldown = 30 * time.Second
)

// Advisor 插件实例（进程内单例）
type Advisor struct {
	st *store.Store
	bn *binanceClient

	stopCh chan struct{}
	wg     sync.WaitGroup

	mu      sync.Mutex
	started bool

	// Phase 2：内置 AI 分析进行中标记（防止同一记录并发分析）
	inflightMu sync.Mutex
	inflight   map[int64]bool
	// aiRetries 自动分析失败补跑计数（analysis ID → 已补跑次数，内存态，
	// 重启清零重新给额度）；analyzeLast 手动 analyze 冷却（analysis ID → 上次触发）
	aiRetries   map[int64]int
	analyzeLast map[int64]time.Time

	// Phase 2：结局回填节流计数（每 backfillEvery 轮跑一次）
	pollCount int
}

var (
	defaultAdvisor   *Advisor
	defaultAdvisorMu sync.RWMutex
)

// Start 创建并启动插件（main.go 调用一次）。返回实例供 Stop。
func Start(st *store.Store) *Advisor {
	a := &Advisor{
		st:          st,
		bn:          newBinanceClient(),
		stopCh:      make(chan struct{}),
		inflight:    map[int64]bool{},
		aiRetries:   map[int64]int{},
		analyzeLast: map[int64]time.Time{},
	}
	defaultAdvisorMu.Lock()
	defaultAdvisor = a
	defaultAdvisorMu.Unlock()

	a.mu.Lock()
	a.started = true
	a.mu.Unlock()
	a.wg.Add(1)
	go a.pollLoop()
	logger.Info("[ReentryAdvisor] AI 重入候选调度已启动（5s 间隔）")
	return a
}

// Stop 停止轮询并等待在途 AI 分析协程收尾（幂等；started=false 先挡住
// 新的 spawnAnalysis，再关 stopCh，wg.Wait 覆盖轮询与分析协程）
func (a *Advisor) Stop() {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return
	}
	a.started = false
	a.mu.Unlock()
	close(a.stopCh)
	a.wg.Wait()
	logger.Info("[ReentryAdvisor] 插件已停止")
}

func (a *Advisor) pollLoop() {
	defer a.wg.Done()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-ticker.C:
			a.pollOnce()
		}
	}
}

// pollOnce 领取到期 AI 候选并回填历史分析结局。任何异常
// （含 panic）只影响本轮，绝不外泄影响宿主进程。
func (a *Advisor) pollOnce() {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("[ReentryAdvisor] 轮询 panic 已恢复: %v", r)
		}
	}()

	cfg, err := a.st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		logger.Warnf("[ReentryAdvisor] 读取配置失败: %v", err)
		return
	}
	// Outcome materialization is independent of whether new AI calls or live
	// execution are enabled. Turning AI off must not strand already completed
	// cycles without actual PnL/effect evaluation.
	a.pollCount++
	if a.pollCount%backfillEvery == 0 {
		a.backfillOutcomes()
		a.backfillDecisionEvaluations()
	}
	if !cfg.Enabled {
		return
	}
	if cfg.AIEnabled {
		a.pollAICandidates(cfg)
	}

}

func (a *Advisor) backfillDecisionEvaluations() {
	cycleIDs, err := a.st.ReentryAI().ListCyclesWithMatureAIEvaluationWindows(25)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] AI 后验评价待处理周期查询失败: %v", err)
		return
	}
	for _, cycleID := range cycleIDs {
		if _, err := copyguardmetrics.EvaluateCycleAIDecisions(a.st, cycleID); err != nil {
			logger.Warnf("[ReentryAdvisor] AI 后验评价失败 cycle=%d: %v", cycleID, err)
		}
	}
}

func (a *Advisor) pollAICandidates(cfg *store.ReentryAIConfig) {
	if recovered, err := a.st.ReentryAI().RecoverStaleReentryCandidateLeases(10 * time.Minute); err != nil {
		logger.Warnf("[ReentryAdvisor] 回收 AI 候选过期租约失败: %v", err)
	} else if recovered > 0 {
		logger.Warnf("[ReentryAdvisor] 已回收 %d 个 AI 候选过期租约", recovered)
	}
	candidates, err := a.st.ReentryAI().ListDueReentryCandidates(50)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] 读取 AI 候选失败: %v", err)
		return
	}
	for _, candidate := range candidates {
		traderCfg, err := a.st.CopyTrade().GetByTraderID(candidate.TraderID)
		if err != nil || !traderCfg.RiskStopLossEnabled || traderCfg.RiskReentryDecisionMode != "ai_guarded" || !traderCfg.RiskReentryEnabled {
			continue
		}
		minNotional := traderCfg.RiskReentryMinNotional
		maxExecutableNotional := candidate.MaxNotional
		unactionableCode := ""
		unactionableMessage := ""
		riskSnapshot, snapshotErr := copytrade.GetExecutionRiskSnapshotForTrader(candidate.TraderID, candidate.CycleID)
		if snapshotErr == nil && riskSnapshot != nil {
			if minNotional <= 0 {
				minNotional = riskSnapshot.MinExecutableNotional
			}
			if riskSnapshot.MaxExecutableNotional > 0 && (maxExecutableNotional <= 0 || riskSnapshot.MaxExecutableNotional < maxExecutableNotional) {
				maxExecutableNotional = riskSnapshot.MaxExecutableNotional
			}
		}
		switch {
		case !candidate.Protectable:
			unactionableCode, unactionableMessage = "PROTECTION_UNAVAILABLE", "deterministic protection precheck unavailable"
		case snapshotErr != nil || riskSnapshot == nil || !riskSnapshot.ExecutionConstraintsAvailable:
			reason := "execution constraints unavailable"
			if snapshotErr != nil {
				reason = snapshotErr.Error()
			} else if riskSnapshot != nil && riskSnapshot.ExecutionConstraintReason != "" {
				reason = riskSnapshot.ExecutionConstraintReason
			}
			unactionableCode, unactionableMessage = "EXECUTION_CONSTRAINTS_UNAVAILABLE", reason
		case minNotional > 0 && maxExecutableNotional < minNotional:
			unactionableCode, unactionableMessage = "MIN_NOTIONAL", fmt.Sprintf("maximum executable notional %.2f is below exchange minimum %.2f", maxExecutableNotional, minNotional)
		}
		if unactionableCode != "" {
			retry := time.Duration(traderCfg.RiskAIMinReviewSeconds) * time.Second
			if retry < 5*time.Minute {
				retry = 5 * time.Minute
			}
			_ = a.st.ReentryAI().DeferReentryCandidateUnactionable(candidate.ID, 0, unactionableMessage, retry)
			a.updateCandidateCycleStatus(candidate, store.CopyGuardAIWaiting)
			if record, recordErr := a.st.ReentryAI().ShouldRecordReentryCandidateUnactionable(candidate.ID, unactionableCode, time.Hour); recordErr != nil {
				logger.Warnf("[ReentryAdvisor] 候选 %d 无法执行事件去重失败: %v", candidate.ID, recordErr)
			} else if record {
				a.recordCandidateEvent(candidate, "AI_CANDIDATE_UNACTIONABLE", candidate.TriggerPrice, candidate.MaxNotional, map[string]interface{}{"reason_code": unactionableCode, "reason": unactionableMessage})
			}
			continue
		}
		claimed, ok, err := a.st.ReentryAI().ClaimReentryCandidateReview(candidate.ID, time.Duration(traderCfg.RiskAIMinReviewSeconds)*time.Second, traderCfg.RiskAIDailyCallLimit, traderCfg.RiskAILifecycleCallLimit)
		if err != nil {
			logger.Warnf("[ReentryAdvisor] 候选 %d 领取失败: %v", candidate.ID, err)
			continue
		}
		if !ok {
			if claimed != nil && claimed.Status == store.ReentryCandidateBudgetSuspended {
				a.updateCandidateCycleStatus(claimed, store.CopyGuardBudgetSuspended)
				a.recordCandidateEvent(claimed, "AI_BUDGET_SUSPENDED", claimed.TriggerPrice, 0, map[string]interface{}{"review_count": claimed.ReviewCount, "lifecycle_limit": traderCfg.RiskAILifecycleCallLimit})
				a.notifyCandidateImportant(claimed, "AI_BUDGET_SUSPENDED", "AI 重入观察额度已耗尽", "候选已暂停，不再继续消耗模型额度。")
			}
			continue
		}
		if claimed == nil {
			continue
		}
		a.updateCandidateCycleStatus(claimed, store.CopyGuardAIReviewing)
		analysis, err := a.generateForCandidate(claimed, cfg)
		if err != nil {
			failed, _ := a.st.ReentryAI().SaveReentryAnalysis(&store.ReentryAIAnalysis{CandidateID: claimed.ID, TraderID: claimed.TraderID, CycleID: claimed.CycleID, Symbol: claimed.Symbol, Side: claimed.Side, AttemptNo: claimed.ReentryCount + 1, DecisionGeneration: claimed.DecisionGeneration, CallStatus: "PENDING", PromptVersion: candidatePromptVersion, SnapshotPrice: claimed.TriggerPrice})
			analysisID := int64(0)
			if failed != nil {
				analysisID = failed.ID
			}
			recordEvent := true
			if errors.Is(err, errCandidateUnactionable) {
				_ = a.st.ReentryAI().DeferReentryCandidateUnactionable(claimed.ID, analysisID, err.Error(), time.Hour)
				recordEvent, _ = a.st.ReentryAI().ShouldRecordReentryCandidateUnactionable(claimed.ID, "DATA_UNAVAILABLE", time.Hour)
			} else {
				_ = a.st.ReentryAI().FailReentryCandidateBeforeModel(claimed.ID, analysisID, err.Error(), candidateFailureBackoff(claimed.FailureCount+1))
			}
			a.updateCandidateCycleStatus(claimed, store.CopyGuardAIWaiting)
			eventType := "AI_REVIEW_FAILED"
			detail := map[string]interface{}{"error": err.Error(), "stage": "datapack"}
			if errors.Is(err, errCandidateUnactionable) {
				eventType = "AI_CANDIDATE_UNACTIONABLE"
				detail["reason_code"] = "DATA_UNAVAILABLE"
			}
			if failed != nil {
				detail["analysis_id"] = failed.ID
			}
			if recordEvent {
				a.recordCandidateEvent(claimed, eventType, 0, 0, detail)
			}
			if !errors.Is(err, errCandidateUnactionable) && claimed.FailureCount+1 == 3 {
				a.notifyCandidateImportant(claimed, "AI_REVIEW_FAILED", "AI 候选数据连续生成失败", "行情或数据包连续不可用，候选将按退避策略继续等待。")
			}
			continue
		}
		duplicate, hashErr := a.st.ReentryAI().HasCompletedCandidateDataHash(claimed.ID, analysis.ID, analysis.DataHash)
		if hashErr != nil {
			_ = a.st.ReentryAI().FailReentryCandidateBeforeModel(claimed.ID, analysis.ID, "data hash dedupe failed: "+hashErr.Error(), candidateFailureBackoff(claimed.FailureCount+1))
			a.updateCandidateCycleStatus(claimed, store.CopyGuardAIWaiting)
			continue
		}
		if duplicate {
			if err := a.st.ReentryAI().SkipDuplicateCandidateReview(claimed.ID, analysis.ID, time.Now().Add(2*time.Hour)); err != nil {
				logger.Warnf("[ReentryAdvisor] 候选 %d 跳过重复数据失败: %v", claimed.ID, err)
			} else {
				a.updateCandidateCycleStatus(claimed, store.CopyGuardAIWaiting)
			}
			continue
		}
		if !a.spawnAnalysis(analysis.ID, true) {
			_ = a.st.ReentryAI().FailReentryCandidateBeforeModel(claimed.ID, analysis.ID, "advisor stopped before model call", candidateFailureBackoff(claimed.FailureCount+1))
			a.updateCandidateCycleStatus(claimed, store.CopyGuardAIWaiting)
		}
	}
}

var errCandidateUnactionable = errors.New("AI candidate is deterministically unactionable")

func (a *Advisor) updateCandidateCycleStatus(c *store.CopyGuardReentryCandidate, status string) {
	if a == nil || a.st == nil || c == nil {
		return
	}
	_ = a.st.CopyTrade().UpdateCopyGuardObservation(c.CycleID, status, c.LeaderEntryPrice, c.TriggerPrice, c.ATR)
}

func candidateFailureBackoff(n int) time.Duration {
	if n <= 1 {
		return 5 * time.Minute
	}
	if n == 2 {
		return 15 * time.Minute
	}
	return time.Hour
}

func (a *Advisor) generateForCandidate(c *store.CopyGuardReentryCandidate, cfg *store.ReentryAIConfig) (*store.ReentryAIAnalysis, error) {
	pack, err := buildDataPackForCandidate(a.st, a.bn, c)
	if err != nil {
		return nil, err
	}
	if !pack.CopyGuard.ReentryBoundaryAvailable || !pack.CopyGuard.ChaseLimitAvailable || !pack.CopyGuard.Leader.SizeVsCycleBaselineAvailable {
		return nil, fmt.Errorf("%w: critical baseline or reentry boundary is unavailable", errCandidateUnactionable)
	}
	b, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return nil, err
	}
	snapshotPrice := c.TriggerPrice
	if pack.Market != nil && pack.Market.CurrentPrice > 0 {
		snapshotPrice = pack.Market.CurrentPrice
	}
	promptCandidate := *c
	promptCandidate.TriggerPrice = snapshotPrice
	hashGuard := pack.CopyGuard
	hashGuard.PreviousAIDecisions = nil
	hashGuard.RecentEvents = nil
	hashGuard.LeaderTimeline = nil
	hashInput, err := json.Marshal(struct {
		FeatureHash string         `json:"feature_hash"`
		Guard       GuardSection   `json:"copy_guard"`
		Market      *MarketSection `json:"market"`
	}{FeatureHash: c.FeatureHash, Guard: hashGuard, Market: pack.Market})
	if err != nil {
		return nil, err
	}
	dataHash := fmt.Sprintf("%x", sha256.Sum256(hashInput))
	analysis := &store.ReentryAIAnalysis{CandidateID: c.ID, TraderID: c.TraderID, CycleID: c.CycleID, Symbol: c.Symbol, Side: c.Side, AttemptNo: c.ReentryCount + 1, DecisionGeneration: c.DecisionGeneration, DataHash: dataHash, SystemPrompt: candidateSystemPrompt(cfg.AnalysisFocus), UserPrompt: buildCandidateUserPrompt(&promptCandidate, string(b)), DatapackJSON: string(b), MarketDataAvailable: pack.Meta.FuturesAvailable, MissingFields: joinMissing(pack.Meta.MissingFields), PromptVersion: candidatePromptVersion, SnapshotPrice: snapshotPrice}
	return a.st.ReentryAI().SaveReentryAnalysis(analysis)
}

func (a *Advisor) recordCandidateEvent(c *store.CopyGuardReentryCandidate, event string, price, notional float64, detail map[string]interface{}) {
	if c == nil {
		return
	}
	if detail == nil {
		detail = map[string]interface{}{}
	}
	detail["candidate_id"] = c.ID
	detail["attempt_no"] = c.ReentryCount + 1
	detail["decision_generation"] = c.DecisionGeneration
	detail["trader_name_snapshot"] = a.st.Trader().ResolveDisplayName(c.TraderID)
	_ = a.st.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: c.CycleID, TraderID: c.TraderID, Type: event, Price: price, Notional: notional, Metadata: detail})
}

// maybeRetryAnalysis 对 PENDING 信号的最新快照补跑自动 AI 分析：
// 仅当该快照既无原始回复也无结论（首跑失败或生成时自动分析未开启），
// 且未在分析中、补跑次数未超上限时触发。修复"自动分析与生成事件一次性
// 绑定，瞬时故障后永不重试"的缺口。
func (a *Advisor) maybeRetryAnalysis(signalID int64) {
	latest, err := a.st.ReentryAI().LatestReentryAnalysisBySignal(signalID)
	if err != nil || latest == nil {
		return
	}
	if latest.RawResponse != "" || latest.Verdict != "" {
		return // 已有结果（含"已回复但不可解析"，那是模型输出问题，不自动重跑）
	}
	a.inflightMu.Lock()
	if a.inflight[latest.ID] || a.aiRetries[latest.ID] >= maxAutoAnalysisRetries {
		a.inflightMu.Unlock()
		return
	}
	a.aiRetries[latest.ID]++
	attempt := a.aiRetries[latest.ID]
	a.inflightMu.Unlock()
	logger.Infof("[ReentryAdvisor] 补跑自动 AI 分析 (analysis=%d, signal=%d, 第 %d/%d 次)",
		latest.ID, signalID, attempt, maxAutoAnalysisRetries)
	a.spawnAnalysis(latest.ID, true)
}

// spawnAnalysis 以受管 goroutine 启动内置 AI 分析：纳入 wg（Stop 会等待
// 收尾，避免对已关闭资源写入），插件已停止时不再启动。
func (a *Advisor) spawnAnalysis(analysisID int64, autoTriggered bool) bool {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return false
	}
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		select {
		case <-a.stopCh:
			return
		default:
		}
		a.runAnalysis(analysisID, autoTriggered)
	}()
	return true
}

// backfillOutcomes 为已执行（EXECUTED）信号回填重入尝试的真实结局盈亏。
// 归属规则：人工重入成交后引擎把周期 reentry_count 自增并开出新尝试，
// 故本信号对应 attempt_no = 信号快照时的 reentry_count + 1；等该尝试
// 闭合且对账完成（reconciled）后，以 pnl 作为结局净额回填。OKX 结算写入的
// attempt.PnL 已包含手续费；Fee 单独保留只用于归因展示，不能再次扣除。
func (a *Advisor) backfillOutcomes() {
	signalIDs, err := a.st.ReentryAI().ListExecutedSignalIDsPendingOutcome(50)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] 结局回填查询失败: %v", err)
		return
	}
	for _, sigID := range signalIDs {
		sig, err := a.st.CopyTrade().GetManualReentrySignal(sigID)
		if err != nil {
			continue
		}
		attempts, err := a.st.CopyTrade().ListCopyGuardAttempts(sig.CycleID)
		if err != nil {
			logger.Warnf("[ReentryAdvisor] 结局回填读取尝试失败 (signal=%d cycle=%d): %v", sigID, sig.CycleID, err)
			continue
		}
		targetNo := sig.ReentryCount + 1
		for _, at := range attempts {
			if at.AttemptNo != targetNo || at.ClosedAt == nil || !at.Reconciled {
				continue
			}
			outcome := at.PnL
			if err := a.st.ReentryAI().SetReentryOutcomeForSignal(sigID, outcome); err != nil {
				logger.Warnf("[ReentryAdvisor] 结局回填写入失败 (signal=%d): %v", sigID, err)
				break
			}
			logger.Infof("[ReentryAdvisor] 信号 %d (%s %s) 结局已回填: %.4f USDT (attempt_no=%d)",
				sigID, sig.Symbol, sig.Side, outcome, targetNo)
			break
		}
	}
	analyses, err := a.st.ReentryAI().ListCandidateAnalysesPendingOutcome(100)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] AI 候选结局回填查询失败: %v", err)
		return
	}
	for _, analysis := range analyses {
		attempts, err := a.st.CopyTrade().ListCopyGuardAttempts(analysis.CycleID)
		if err != nil {
			continue
		}
		for _, attempt := range attempts {
			if attempt.AttemptNo != analysis.AttemptNo || attempt.ClosedAt == nil || !attempt.Reconciled {
				continue
			}
			if err := a.st.ReentryAI().SetReentryOutcomeForAnalysis(analysis.ID, attempt.PnL); err == nil {
				logger.Infof("[ReentryAdvisor] AI 候选结局已回填 analysis=%d cycle=%d attempt=%d pnl=%.4f", analysis.ID, analysis.CycleID, analysis.AttemptNo, attempt.PnL)
			}
			break
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "无"
	}
	return s
}

// generateForSignal 组装数据包 → 生成 Prompt（含配置页自定义模板）→ 落库一条分析记录
func (a *Advisor) generateForSignal(sig *store.CopyGuardManualReentrySignal, cfg *store.ReentryAIConfig) (*store.ReentryAIAnalysis, error) {
	pack, err := buildDataPack(a.st, a.bn, sig)
	if err != nil {
		return nil, err
	}
	packJSON, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("数据包序列化失败: %w", err)
	}
	analysis := &store.ReentryAIAnalysis{
		SignalID:            sig.ID,
		TraderID:            sig.TraderID,
		CycleID:             sig.CycleID,
		Symbol:              sig.Symbol,
		Side:                sig.Side,
		AttemptNo:           sig.ReentryCount + 1,
		SystemPrompt:        buildSystemPrompt(cfg.PromptTemplate),
		UserPrompt:          buildUserPrompt(sig, string(packJSON)),
		DatapackJSON:        string(packJSON),
		MarketDataAvailable: pack.Meta.FuturesAvailable,
		MissingFields:       joinMissing(pack.Meta.MissingFields),
		PromptVersion:       promptVersion,
	}
	return a.st.ReentryAI().SaveReentryAnalysis(analysis)
}

func joinMissing(fields []string) string {
	out := ""
	for i, f := range fields {
		if i > 0 {
			out += ","
		}
		out += f
	}
	return out
}

// RegenerateForSignal 手动重新生成数据包（新快照新记录），API 层调用。
// 约束：插件已启动且启用；信号处于 PENDING/EXECUTING；距最近快照 ≥ 60s。
func RegenerateForSignal(signalID int64) (*store.ReentryAIAnalysis, error) {
	defaultAdvisorMu.RLock()
	a := defaultAdvisor
	defaultAdvisorMu.RUnlock()
	if a == nil {
		return nil, fmt.Errorf("重入 AI 助手插件未启动")
	}
	cfg, err := a.st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("重入 AI 助手已在配置中关闭")
	}
	sig, err := a.st.CopyTrade().GetManualReentrySignal(signalID)
	if err != nil {
		return nil, fmt.Errorf("信号不存在: %d", signalID)
	}
	if sig.Status != store.ManualReentryStatusPending && sig.Status != store.ManualReentryStatusExecuting {
		return nil, fmt.Errorf("信号当前状态为 %s，不支持重新生成分析数据", sig.Status)
	}
	if latest, err := a.st.ReentryAI().LatestReentryAnalysisBySignal(signalID); err == nil && latest != nil {
		if since := time.Since(latest.SnapshotAt); since < regenerateCooldown {
			return nil, fmt.Errorf("重新生成过于频繁，请 %d 秒后再试", int((regenerateCooldown-since).Seconds())+1)
		}
	}
	analysis, err := a.generateForSignal(sig, cfg)
	if err != nil {
		return nil, err
	}
	logger.Infof("[ReentryAdvisor] 信号 %d (%s %s) 数据包已手动重新生成 (analysis=%d)", sig.ID, sig.Symbol, sig.Side, analysis.ID)
	// 自动分析开启时对新快照顺带跑内置 AI（用户在界面上，不发邮件、不自动入场）
	if cfg.AIEnabled {
		a.spawnAnalysis(analysis.ID, false)
	}
	return analysis, nil
}
