// Package reentryadvisor 实现 Copy Guard 持久化 AI 重入候选调度。
//
// AI 只判断趋势反转与建议缩放比例；仓位、风险预算、止损可保护性和
// 订单幂等由 copytrade 确定性执行层最终裁决。插件每 5 秒领取到期候选，
// 使用数据哈希、最小间隔、事件合并与故障退避控制调用。原日/生命周期额度只保留
// 费用软预警，不再中断分析。历史人工信号只保留
// 查询兼容，不再由后台产生分析或执行。
package reentryadvisor

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nofx/copyguardmetrics"
	"nofx/copytrade"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
)

const (
	pollInterval = 5 * time.Second
	// regenerateCooldown 手动"重新生成"冷却：防止高频刷新打爆 Binance 公共接口
	regenerateCooldown = 60 * time.Second
	// backfillEvery 结局盈亏回填频率：每 12 轮（约 60s）扫一次已执行信号
	backfillEvery = 12
	// manualAnalyzeCooldown 手动"内置 AI 分析"按钮冷却，防误触连击烧 token
	manualAnalyzeCooldown = 30 * time.Second
	// marketEventScanInterval bounds public-market polling. The scan only
	// schedules a review; feature-hash dedupe still decides whether a model
	// call is needed.
	marketEventScanInterval = 5 * time.Minute
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
	inflightMu     sync.Mutex
	inflight       map[int64]bool
	inflightTrader map[string]bool
	// analyzeLast 手动 analyze 冷却（analysis ID → 上次触发）
	analyzeLast map[int64]time.Time

	marketEventMu       sync.Mutex
	marketEventLastScan map[int64]time.Time
	marketEventState    map[int64]marketEventSnapshot
	marketEventScanning atomic.Bool

	// Phase 2：结局回填节流计数（每 backfillEvery 轮跑一次）
	pollCount int
}

// lifecycleReentryConfig applies the immutable Copy Guard policy carried by
// an AI candidate's lifecycle. Editing a trader to fixed position-margin mode
// is a template change for later positions; it must neither kill nor reprice
// an ATR candidate that was already stopped and entered the review pipeline.
func (a *Advisor) lifecycleReentryConfig(candidate *store.CopyGuardReentryCandidate, fallback *store.CopyTradeConfig) (*store.CopyTradeConfig, error) {
	if candidate == nil || fallback == nil {
		return nil, fmt.Errorf("candidate lifecycle configuration unavailable")
	}
	cycle, err := a.st.CopyTrade().GetCopyGuardCycle(candidate.CycleID)
	if err != nil {
		return nil, err
	}
	cfg := *fallback
	policy, err := store.DecodeCopyGuardPolicySnapshot(cycle.PolicySnapshot)
	if err != nil {
		return nil, err
	}
	if policy.ProtectionMode == store.RiskProtectionModePositionMarginPct {
		cfg.RiskProtectionMode = policy.ProtectionMode
		cfg.RiskReentryEnabled = false
		cfg.RiskReentryDecisionMode = "disabled"
		cfg.RiskMaxReentries = 0
		return &cfg, nil
	}
	if policy.ReentryEnabled != nil {
		cfg.RiskReentryEnabled = *policy.ReentryEnabled
	} else if policy.ReentryDecisionMode != "" {
		cfg.RiskReentryEnabled = policy.ReentryDecisionMode != "disabled" && policy.MaxReentries > 0
	}
	if policy.ReentryDecisionMode != "" {
		cfg.RiskReentryDecisionMode = policy.ReentryDecisionMode
	}
	hasPolicy := policy.Version >= 4 || policy.DefaultsVersion > 0 ||
		policy.ReentryDecisionMode != "" || policy.ProtectionMode != ""
	if hasPolicy {
		cfg.RiskMaxReentries = policy.MaxReentries
		cfg.RiskReentryMinNotional = policy.ReentryMinNotional
	}
	if policy.AIMinReviewSeconds > 0 {
		cfg.RiskAIMinReviewSeconds = policy.AIMinReviewSeconds
	}
	if policy.AIDailyCallLimit > 0 {
		cfg.RiskAIDailyCallLimit = policy.AIDailyCallLimit
	}
	if policy.AILifecycleCallLimit > 0 {
		cfg.RiskAILifecycleCallLimit = policy.AILifecycleCallLimit
	}
	return &cfg, nil
}

var (
	defaultAdvisor   *Advisor
	defaultAdvisorMu sync.RWMutex
)

// Start 创建并启动插件（main.go 调用一次）。返回实例供 Stop。
func Start(st *store.Store) *Advisor {
	a := &Advisor{
		st:                  st,
		bn:                  newBinanceClient(),
		stopCh:              make(chan struct{}),
		inflight:            map[int64]bool{},
		inflightTrader:      map[string]bool{},
		analyzeLast:         map[int64]time.Time{},
		marketEventLastScan: map[int64]time.Time{},
		marketEventState:    map[int64]marketEventSnapshot{},
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
		if cfg.AIEnabled && a.marketEventScanning.CompareAndSwap(false, true) {
			a.wg.Add(1)
			go func() {
				defer a.wg.Done()
				defer a.marketEventScanning.Store(false)
				a.pollMarketEventReviews()
			}()
		}
	}
	if !cfg.Enabled {
		return
	}
	if cfg.AIEnabled {
		a.pollAICandidates(cfg)
	}

}

type marketEventSnapshot struct {
	FundingState string
	OIState      string
	CVDState     string
	ATRState     string
}

func changedMarketEventTriggers(before, after marketEventSnapshot) []string {
	var changed []string
	for _, field := range []struct {
		name          string
		before, after string
	}{
		{"FUNDING_STATE_FLIP", before.FundingState, after.FundingState},
		{"OI_STATE_FLIP", before.OIState, after.OIState},
		{"CONTRACT_CVD_STATE_FLIP", before.CVDState, after.CVDState},
		{"ATR_REGIME_CHANGE", before.ATRState, after.ATRState},
	} {
		// Missing data becoming available is evidence-quality recovery, not a
		// market reversal by itself.
		if field.before != "" && field.after != "" && field.before != field.after {
			changed = append(changed, field.name)
		}
	}
	sort.Strings(changed)
	return changed
}

func atrFromClosedKlines(klines []market.Kline, period int) float64 {
	if period <= 0 || len(klines) < period+1 {
		return 0
	}
	start := len(klines) - period
	var total float64
	for i := start; i < len(klines); i++ {
		prevClose := klines[i-1].Close
		tr := math.Max(klines[i].High-klines[i].Low,
			math.Max(math.Abs(klines[i].High-prevClose), math.Abs(klines[i].Low-prevClose)))
		total += tr
	}
	return total / float64(period)
}

func oiState(points []oiPoint) string {
	if len(points) < 2 {
		return ""
	}
	change := pctChange(points[0].Value, points[len(points)-1].Value)
	switch {
	case change >= 1:
		return "RISING"
	case change <= -1:
		return "FALLING"
	default:
		return "STABLE"
	}
}

func atrRegime(current, baseline float64) string {
	if current <= 0 || baseline <= 0 {
		return ""
	}
	ratio := current / baseline
	switch {
	case ratio >= 1.5:
		return "EXPANDED"
	case ratio <= 0.67:
		return "CONTRACTED"
	default:
		return "NORMAL"
	}
}

func (a *Advisor) loadMarketEventSnapshot(c *store.CopyGuardReentryCandidate) marketEventSnapshot {
	var snapshot marketEventSnapshot
	if a == nil || a.bn == nil || c == nil {
		return snapshot
	}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		if premium, err := a.bn.premiumIndex(c.Symbol); err == nil {
			snapshot.FundingState = fundingState(premium.LastFundingRate)
		}
	}()
	go func() {
		defer wg.Done()
		if points, err := a.bn.openInterestHist(c.Symbol, "5m", 13); err == nil {
			snapshot.OIState = oiState(points)
		}
	}()
	go func() {
		defer wg.Done()
		if klines, err := a.bn.futuresKlines(c.Symbol, "5m", 60); err == nil {
			if cvd := summarizeCVD(closedKlinesAt(klines, time.Now())); cvd != nil {
				snapshot.CVDState = cvd.SlopeSign + "|" + cvd.Divergence
			}
		}
	}()
	go func() {
		defer wg.Done()
		if klines, err := a.bn.futuresKlines(c.Symbol, "1h", 30); err == nil {
			snapshot.ATRState = atrRegime(atrFromClosedKlines(closedKlinesAt(klines, time.Now()), 14), c.ATR)
		}
	}()
	wg.Wait()
	return snapshot
}

// pollMarketEventReviews turns material derivative-market state flips into
// coalesced candidate reviews. It never calls the model or mutates execution
// state directly.
func (a *Advisor) pollMarketEventReviews() {
	traders, err := a.st.Trader().ListAll()
	if err != nil {
		logger.Warnf("[ReentryAdvisor] 读取市场事件交易员失败: %v", err)
		return
	}
	var runningIDs []string
	for _, trader := range traders {
		if trader != nil && trader.LifecycleStatus == store.TraderLifecycleRunning && trader.IsRunning {
			runningIDs = append(runningIDs, trader.ID)
		}
	}
	candidates, err := a.st.ReentryAI().ListReentryCandidatesByTraders(runningIDs,
		[]string{store.ReentryCandidateWatching, store.ReentryCandidateWaiting, store.ReentryCandidateDormantRearm}, 25)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] 读取市场事件候选失败: %v", err)
		return
	}
	now := time.Now().UTC()
	active := make(map[int64]struct{}, len(candidates))
	symbolSnapshots := make(map[string]marketEventSnapshot)
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		active[candidate.ID] = struct{}{}
		a.marketEventMu.Lock()
		lastScan := a.marketEventLastScan[candidate.ID]
		a.marketEventMu.Unlock()
		if !lastScan.IsZero() && now.Sub(lastScan) < marketEventScanInterval {
			continue
		}
		snapshotKey := fmt.Sprintf("%s|%.8f", strings.ToUpper(candidate.Symbol), candidate.ATR)
		snapshot, cached := symbolSnapshots[snapshotKey]
		if !cached {
			snapshot = a.loadMarketEventSnapshot(candidate)
			symbolSnapshots[snapshotKey] = snapshot
		}
		a.marketEventMu.Lock()
		before, hadBefore := a.marketEventState[candidate.ID]
		a.marketEventState[candidate.ID] = snapshot
		a.marketEventLastScan[candidate.ID] = now
		a.marketEventMu.Unlock()
		if !hadBefore {
			continue
		}
		triggers := changedMarketEventTriggers(before, snapshot)
		if len(triggers) == 0 {
			continue
		}
		traderCfg, cfgErr := a.st.CopyTrade().GetByTraderID(candidate.TraderID)
		if cfgErr == nil {
			traderCfg, cfgErr = a.lifecycleReentryConfig(candidate, traderCfg)
		}
		if cfgErr != nil || !traderCfg.RiskStopLossEnabled ||
			traderCfg.RiskReentryDecisionMode != "ai_guarded" || !traderCfg.RiskReentryEnabled {
			continue
		}
		minInterval := time.Duration(traderCfg.RiskAIMinReviewSeconds) * time.Second
		trigger := strings.Join(triggers, "+")
		if _, scheduleErr := a.st.ReentryAI().ScheduleReentryCandidateEventReview(
			candidate.ID, trigger, minInterval); scheduleErr != nil {
			logger.Warnf("[ReentryAdvisor] 市场状态事件复审调度失败 candidate=%d trigger=%s: %v",
				candidate.ID, trigger, scheduleErr)
		}
	}
	a.marketEventMu.Lock()
	for id := range a.marketEventState {
		if _, exists := active[id]; !exists {
			delete(a.marketEventState, id)
			delete(a.marketEventLastScan, id)
		}
	}
	a.marketEventMu.Unlock()
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
		lifecycle, lifecycleErr := a.st.Trader().GetLifecycle(candidate.TraderID)
		if lifecycleErr != nil || lifecycle.Status != store.TraderLifecycleRunning || !lifecycle.IsRunning {
			continue
		}
		a.inflightMu.Lock()
		traderBusy := a.inflightTrader[candidate.TraderID]
		a.inflightMu.Unlock()
		if traderBusy {
			continue
		}
		traderCfg, err := a.st.CopyTrade().GetByTraderID(candidate.TraderID)
		if err == nil {
			traderCfg, err = a.lifecycleReentryConfig(candidate, traderCfg)
		}
		if err != nil || !traderCfg.RiskStopLossEnabled || traderCfg.RiskReentryDecisionMode != "ai_guarded" || !traderCfg.RiskReentryEnabled {
			continue
		}
		effectiveMinimum := traderCfg.RiskReentryMinNotional
		stoppedNotionalCeiling := 0.0
		unactionableCode := ""
		unactionableMessage := ""
		riskSnapshot, snapshotErr := copytrade.GetExecutionRiskSnapshotForTrader(candidate.TraderID, candidate.CycleID)
		if snapshotErr == nil && riskSnapshot != nil {
			if riskSnapshot.EffectiveMinNotional > effectiveMinimum {
				effectiveMinimum = riskSnapshot.EffectiveMinNotional
			}
			stoppedNotionalCeiling = riskSnapshot.StoppedPositionNotional
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
		case effectiveMinimum > 0 && stoppedNotionalCeiling > 0 && effectiveMinimum > stoppedNotionalCeiling+0.01:
			unactionableCode, unactionableMessage = "INELIGIBLE_PROMOTION_CEILING", fmt.Sprintf("effective minimum %.2f exceeds stopped position ceiling %.2f", effectiveMinimum, stoppedNotionalCeiling)
		}
		if unactionableCode != "" {
			if unactionableCode == "INELIGIBLE_PROMOTION_CEILING" {
				_ = a.st.ReentryAI().MarkReentryCandidateStatus(candidate.ID, store.ReentryCandidateInvalidated, unactionableMessage)
				a.updateCandidateCycleStatus(candidate, store.CopyGuardAIAbandoned)
				a.recordCandidateEvent(candidate, "INELIGIBLE_PROMOTION_CEILING", candidate.TriggerPrice, stoppedNotionalCeiling, map[string]interface{}{"reason_code": unactionableCode, "reason": unactionableMessage, "configured_minimum": traderCfg.RiskReentryMinNotional, "exchange_minimum": riskSnapshot.MinExecutableNotional, "effective_minimum": effectiveMinimum, "stopped_notional": stoppedNotionalCeiling})
				continue
			}
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
			continue
		}
		if claimed == nil {
			continue
		}
		a.updateCandidateCycleStatus(claimed, store.CopyGuardAIReviewing)
		analysis, err := a.generateForCandidate(claimed, cfg)
		if err != nil {
			failed, _ := a.st.ReentryAI().SaveReentryAnalysis(&store.ReentryAIAnalysis{CandidateID: claimed.ID, TraderID: claimed.TraderID, CycleID: claimed.CycleID, Symbol: claimed.Symbol, Side: claimed.Side, AttemptNo: claimed.ReentryCount + 1, DecisionGeneration: claimed.DecisionGeneration, CallStatus: "PENDING", PromptVersion: activeCandidatePromptVersion(), SnapshotPrice: claimed.TriggerPrice})
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
		duplicate, hashErr := a.st.ReentryAI().HasCompletedCandidateDataHash(claimed.ID, analysis.ID, analysis.DataHash, analysis.PromptVersion)
		if hashErr != nil {
			_ = a.st.ReentryAI().FailReentryCandidateBeforeModel(claimed.ID, analysis.ID, "data hash dedupe failed: "+hashErr.Error(), candidateFailureBackoff(claimed.FailureCount+1))
			a.updateCandidateCycleStatus(claimed, store.CopyGuardAIWaiting)
			continue
		}
		if duplicate {
			next := a.nextCandidateReviewAt(claimed.ID, time.Now().UTC())
			if err := a.st.ReentryAI().SkipDuplicateCandidateReview(claimed.ID, analysis.ID, next); err != nil {
				logger.Warnf("[ReentryAdvisor] 候选 %d 跳过重复数据失败: %v", claimed.ID, err)
			} else {
				a.updateCandidateCycleStatus(claimed, store.CopyGuardAIWaiting)
			}
			continue
		}
		if !a.spawnAnalysis(analysis.ID, true, claimed.TraderID) {
			_ = a.st.ReentryAI().FailReentryCandidateBeforeModel(claimed.ID, analysis.ID, "advisor stopped before model call", candidateFailureBackoff(claimed.FailureCount+1))
			a.updateCandidateCycleStatus(claimed, store.CopyGuardAIWaiting)
		}
	}
}

var errCandidateUnactionable = errors.New("AI candidate is deterministically unactionable")

func (a *Advisor) updateCandidateCycleStatus(c *store.CopyGuardReentryCandidate, status string) {
	if a == nil || a.st == nil || c == nil || !a.traderLifecycleRunning(c.TraderID) {
		return
	}
	_ = a.st.CopyTrade().UpdateCopyGuardObservation(c.CycleID, status, c.LeaderEntryPrice, c.TriggerPrice, c.ATR)
}

func (a *Advisor) traderLifecycleRunning(traderID string) bool {
	if a == nil || a.st == nil {
		return false
	}
	state, err := a.st.Trader().GetLifecycle(traderID)
	return err == nil && state.Status == store.TraderLifecycleRunning && state.IsRunning
}

func candidateFailureBackoff(n int) time.Duration {
	if n <= 1 {
		return 5 * time.Minute
	}
	if n == 2 {
		return 15 * time.Minute
	}
	backoff := time.Hour
	for attempt := 3; attempt < n && backoff < 6*time.Hour; attempt++ {
		backoff *= 2
	}
	if backoff > 6*time.Hour {
		backoff = 6 * time.Hour
	}
	return backoff
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
	analysis := &store.ReentryAIAnalysis{CandidateID: c.ID, TraderID: c.TraderID, CycleID: c.CycleID, Symbol: c.Symbol, Side: c.Side, AttemptNo: c.ReentryCount + 1, DecisionGeneration: c.DecisionGeneration, DataHash: dataHash, SystemPrompt: candidateSystemPrompt(cfg.AnalysisFocus), UserPrompt: buildCandidateUserPrompt(&promptCandidate, string(b)), DatapackJSON: string(b), MarketDataAvailable: pack.Meta.FuturesAvailable, MissingFields: joinMissing(pack.Meta.MissingFields), PromptVersion: activeCandidatePromptVersion(), SnapshotPrice: snapshotPrice}
	return a.st.ReentryAI().SaveReentryAnalysis(analysis)
}

func (a *Advisor) recordCandidateEvent(c *store.CopyGuardReentryCandidate, event string, price, notional float64, detail map[string]interface{}) {
	if c == nil || !a.traderLifecycleRunning(c.TraderID) {
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

// spawnAnalysis 以受管 goroutine 启动内置 AI 分析：纳入 wg（Stop 会等待
// 收尾，避免对已关闭资源写入），插件已停止时不再启动。
func (a *Advisor) spawnAnalysis(analysisID int64, autoTriggered bool, traderIDs ...string) bool {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return false
	}
	a.wg.Add(1)
	a.mu.Unlock()
	traderID := ""
	if len(traderIDs) > 0 {
		traderID = traderIDs[0]
	} else if analysis, err := a.st.ReentryAI().GetReentryAnalysis(analysisID); err == nil {
		traderID = analysis.TraderID
	}
	if traderID != "" {
		a.inflightMu.Lock()
		if a.inflightTrader == nil {
			a.inflightTrader = map[string]bool{}
		}
		if a.inflightTrader[traderID] {
			a.inflightMu.Unlock()
			a.wg.Done()
			return false
		}
		a.inflightTrader[traderID] = true
		a.inflightMu.Unlock()
	}
	go func() {
		defer a.wg.Done()
		if traderID != "" {
			defer func() {
				a.inflightMu.Lock()
				delete(a.inflightTrader, traderID)
				a.inflightMu.Unlock()
			}()
		}
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
