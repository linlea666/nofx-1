package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"nofx/copyguardmetrics"
	"nofx/copytrade"
	"nofx/logger"
	"nofx/manager"
	"nofx/store"
)

// maxCopyRatio 跟单系数硬上限（前端滑杆上限 3，后端留出直连 API 余量）。
// 两个保存入口（SaveConfig / server.go 创建与更新交易员）共用。
const maxCopyRatio = 10.0

// CopyTradeHandler 跟单 API Handler
type CopyTradeHandler struct {
	store         *store.Store
	traderManager *manager.TraderManager
}

// NewCopyTradeHandler 创建跟单 Handler
func NewCopyTradeHandler(st *store.Store, tm *manager.TraderManager) *CopyTradeHandler {
	return &CopyTradeHandler{
		store:         st,
		traderManager: tm,
	}
}

func validateAIGuardedPrerequisites(st *store.Store, cfg *store.CopyTradeConfig) error {
	if cfg == nil || cfg.RiskReentryDecisionMode != "ai_guarded" {
		return nil
	}
	aiCfg, err := st.ReentryAI().GetReentryAIConfig()
	if err != nil || !aiCfg.Enabled || !aiCfg.AIEnabled {
		return fmt.Errorf("ai_guarded requires enabled Reentry AI analysis")
	}
	if aiCfg.Model != "" {
		model, modelErr := st.AIModel().GetByID(aiCfg.Model)
		if modelErr != nil || !model.Enabled || strings.TrimSpace(model.APIKey) == "" {
			return fmt.Errorf("ai_guarded model is missing, disabled, or has no API key")
		}
		return nil
	}
	models, err := st.AIModel().List("default")
	if err != nil {
		return fmt.Errorf("cannot validate AI model: %w", err)
	}
	for _, model := range models {
		if model.Enabled && strings.TrimSpace(model.APIKey) != "" {
			return nil
		}
	}
	return fmt.Errorf("ai_guarded requires at least one enabled AI model with an API key")
}

// validateLegacyReentrySelection makes legacy_rule a one-way compatibility
// state: an existing legacy trader may remain unchanged or migrate out, but no
// new/non-legacy trader can opt back into the retired execution engine.
func validateLegacyReentrySelection(existing *store.CopyTradeConfig, requested string) error {
	if requested != "legacy_rule" {
		return nil
	}
	if existing != nil && existing.RiskReentryDecisionMode == "legacy_rule" {
		return nil
	}
	return fmt.Errorf("legacy_rule is retired for new selections; use ai_guarded or disabled")
}

// validateRiskConfirmation keeps every configuration entry point on the same
// high-risk confirmation contract. Above 8%, a boolean is insufficient: the
// caller must echo the exact percentage value the operator typed.
func validateRiskConfirmation(accountRisk float64, confirmed bool, typedPercent *float64) error {
	if accountRisk <= 0.04 {
		return nil
	}
	if !confirmed {
		return fmt.Errorf("risk_account_pct > 4%% requires risk_high_risk_confirmed")
	}
	if accountRisk > 0.08 {
		expected := accountRisk * 100
		if typedPercent == nil || math.IsNaN(*typedPercent) || math.IsInf(*typedPercent, 0) || math.Abs(*typedPercent-expected) > 1e-9 {
			return fmt.Errorf("risk_account_pct > 8%% requires matching risk_extreme_risk_confirm_value")
		}
	}
	return nil
}

// RegisterRoutes 注册路由
func (h *CopyTradeHandler) RegisterRoutes(group *gin.RouterGroup) {
	copyTrade := group.Group("/copytrade")
	{
		copyTrade.GET("/config/:trader_id", h.GetConfig)
		copyTrade.POST("/config/:trader_id", h.SaveConfig)
		copyTrade.DELETE("/config/:trader_id", h.DeleteConfig)
		copyTrade.POST("/start/:trader_id", h.Start)
		copyTrade.POST("/stop/:trader_id", h.Stop)
		copyTrade.GET("/stats/:trader_id", h.GetStats)
		copyTrade.GET("/logs/:trader_id", h.GetLogs)
		copyTrade.GET("/risk/summary", h.GetRiskSummary)
		copyTrade.GET("/risk/defaults", h.GetRiskDefaults)
		copyTrade.GET("/risk/ai-candidates", h.ListAICandidates)
		copyTrade.POST("/risk/ai-candidates/:id/pause", h.PauseAICandidate)
		copyTrade.POST("/risk/ai-candidates/:id/resume", h.ResumeAICandidate)
		copyTrade.POST("/risk/ai-candidates/:id/request-review", h.RequestAICandidateReview)
		copyTrade.POST("/risk/ai-candidates/:id/terminate", h.TerminateAICandidate)
		copyTrade.GET("/risk/cycles", h.GetRiskCycles)
		copyTrade.GET("/risk/cycles/:id", h.GetRiskCycle)
		copyTrade.GET("/risk/cycles/:id/export", h.ExportRiskCycle)
		copyTrade.GET("/risk/export", h.ExportRiskCycles)

		// 统一跟单事件日志（开仓/加仓/减仓/平仓 + 止损/二次入场/接手/保护/对账）
		copyTrade.GET("/events", h.GetCopyEvents)
		copyTrade.GET("/events/export", h.ExportCopyEvents)

		// v5.1 人工重入信号：列表 / 确认（系统代执行）/ 忽略
		copyTrade.GET("/risk/manual-signals", h.ListManualSignals)
		copyTrade.POST("/risk/manual-signals/:id/confirm", h.ConfirmManualSignal)
		copyTrade.POST("/risk/manual-signals/:id/dismiss", h.DismissManualSignal)

		// Binance 全局共享凭证管理（v2 凭证全局化）
		// 所有 Binance 跟单交易员共享同一份凭证（p20t / csrftoken），
		// 一处更新全局生效，无需逐个交易员维护。
		creds := copyTrade.Group("/binance-credentials")
		{
			creds.GET("", h.ListBinanceCredentials)
			creds.POST("", h.SetBinanceCredentials)
			creds.POST("/test", h.TestBinanceCredentials)
			creds.GET("/affected", h.ListBinanceCredentialsAffectedTraders)
			creds.DELETE("/:label", h.DeleteBinanceCredentials)
		}
	}
}

func (h *CopyTradeHandler) ListAICandidates(c *gin.Context) {
	ids, _, names, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var statuses []string
	for _, status := range strings.Split(strings.TrimSpace(c.Query("status")), ",") {
		if status = strings.ToUpper(strings.TrimSpace(status)); status != "" {
			statuses = append(statuses, status)
		}
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	candidates, err := h.store.ReentryAI().ListReentryCandidatesByTraders(ids, statuses, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for _, candidate := range candidates {
		if policy, policyErr := h.store.CopyTrade().GetByTraderID(candidate.TraderID); policyErr == nil {
			candidate.AIConfidenceThreshold = policy.RiskAIConfidenceThreshold
			candidate.AIMinReviewSeconds = policy.RiskAIMinReviewSeconds
			candidate.AIDailyCallLimit = policy.RiskAIDailyCallLimit
			candidate.AILifecycleCallLimit = policy.RiskAILifecycleCallLimit
		}
		if calls, callsErr := h.store.ReentryAI().CountReentryCandidateCalls24h(candidate.ID); callsErr == nil {
			candidate.AIDailyCallsUsed = calls
		}
	}
	c.JSON(http.StatusOK, gin.H{"candidates": candidates, "trader_names": names, "count": len(candidates)})
}

func (h *CopyTradeHandler) getOwnedAICandidate(c *gin.Context) *store.CopyGuardReentryCandidate {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid candidate id"})
		return nil
	}
	_, owned, _, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return nil
	}
	candidate, err := h.store.ReentryAI().GetReentryCandidate(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "candidate not found"})
		return nil
	}
	if !owned[candidate.TraderID] {
		c.JSON(http.StatusForbidden, gin.H{"error": "candidate not owned by current user"})
		return nil
	}
	return candidate
}

func (h *CopyTradeHandler) PauseAICandidate(c *gin.Context) {
	candidate := h.getOwnedAICandidate(c)
	if candidate == nil {
		return
	}
	if err := h.store.ReentryAI().PauseReentryCandidate(candidate.ID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	_ = h.store.CopyTrade().UpdateCopyGuardObservation(candidate.CycleID, store.CopyGuardAIWaiting, candidate.LeaderEntryPrice, candidate.TriggerPrice, candidate.ATR)
	c.JSON(http.StatusOK, gin.H{"message": "AI candidate paused"})
}

func (h *CopyTradeHandler) ResumeAICandidate(c *gin.Context) {
	candidate := h.getOwnedAICandidate(c)
	if candidate == nil {
		return
	}
	if err := h.store.ReentryAI().ResumeReentryCandidate(candidate.ID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	_ = h.store.CopyTrade().UpdateCopyGuardObservation(candidate.CycleID, store.CopyGuardAIWaiting, candidate.LeaderEntryPrice, candidate.TriggerPrice, candidate.ATR)
	c.JSON(http.StatusOK, gin.H{"message": "AI candidate resumed"})
}

func (h *CopyTradeHandler) RequestAICandidateReview(c *gin.Context) {
	candidate := h.getOwnedAICandidate(c)
	if candidate == nil {
		return
	}
	policy, err := h.store.CopyTrade().GetByTraderID(candidate.TraderID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "trader risk policy unavailable"})
		return
	}
	if !policy.RiskReentryEnabled || policy.RiskReentryDecisionMode != "ai_guarded" {
		c.JSON(http.StatusConflict, gin.H{"error": "candidate trader is not enabled for ai_guarded reentry"})
		return
	}
	if err := validateAIGuardedPrerequisites(h.store, policy); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	fresh, err := h.store.ReentryAI().RequestImmediateReentryCandidateReview(candidate.ID, time.Duration(policy.RiskAIMinReviewSeconds)*time.Second)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	_ = h.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
		CycleID: candidate.CycleID, TraderID: candidate.TraderID, Type: "AI_REVIEW_REQUESTED",
		Metadata: map[string]interface{}{
			"candidate_id": candidate.ID, "attempt_no": candidate.ReentryCount + 1,
			"decision_generation": candidate.DecisionGeneration, "operator": c.GetString("user_id"),
			"eligible_at": fresh.NextReviewAt, "min_review_seconds": policy.RiskAIMinReviewSeconds,
		},
	})
	c.JSON(http.StatusOK, gin.H{
		"message":                "AI review requested through the guarded scheduler",
		"candidate":              fresh,
		"eligible_at":            fresh.NextReviewAt,
		"may_execute_real_order": true,
	})
}

func (h *CopyTradeHandler) TerminateAICandidate(c *gin.Context) {
	candidate := h.getOwnedAICandidate(c)
	if candidate == nil {
		return
	}
	if err := h.store.ReentryAI().TerminateReentryCandidate(candidate.ID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	_ = h.store.CopyTrade().UpdateCopyGuardObservation(candidate.CycleID, store.CopyGuardAIAbandoned, candidate.LeaderEntryPrice, candidate.TriggerPrice, candidate.ATR)
	_ = h.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: candidate.CycleID, TraderID: candidate.TraderID, Type: "AI_CANDIDATE_TERMINATED", Metadata: map[string]interface{}{"candidate_id": candidate.ID, "operator": c.GetString("user_id")}})
	c.JSON(http.StatusOK, gin.H{"message": "AI candidate terminated"})
}

func (h *CopyTradeHandler) ownedTraderIDs(c *gin.Context) ([]string, map[string]bool, map[string]string, error) {
	list, err := h.store.Trader().List(c.GetString("user_id"))
	if err != nil {
		return nil, nil, nil, err
	}
	ids := make([]string, 0, len(list))
	set := map[string]bool{}
	names := map[string]string{}
	filter := strings.TrimSpace(c.Query("trader_id"))
	for _, t := range list {
		if filter != "" && t.ID != filter {
			continue
		}
		ids = append(ids, t.ID)
		set[t.ID] = true
		names[t.ID] = t.Name
	}
	return ids, set, names, nil
}

// allTraderIDs 系统内全部交易员（跨用户），仅供跟单事件日志的只读查询/导出使用：
// 全部账户由管理员操作，AI 分析需要所有交易员的完整事件时间线。
// 注意：写操作与其他归属敏感接口必须继续走 ownedTraderIDs，不得改用本函数。
func (h *CopyTradeHandler) allTraderIDs(c *gin.Context) ([]string, map[string]string, error) {
	list, err := h.store.Trader().ListAll()
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(list))
	names := map[string]string{}
	filter := strings.TrimSpace(c.Query("trader_id"))
	for _, t := range list {
		// names 覆盖全部交易员，便于回填历史事件的名称（即使按 trader_id 收窄查询）
		names[t.ID] = t.Name
		if filter != "" && t.ID != filter {
			continue
		}
		ids = append(ids, t.ID)
	}
	return ids, names, nil
}

func riskTimeRange(c *gin.Context) (time.Time, time.Time) {
	to := time.Now().UTC().Add(time.Second)
	from := to.AddDate(0, 0, -30)
	if v := c.Query("from"); v != "" {
		if x, e := time.Parse(time.RFC3339, v); e == nil {
			from = x
		}
	}
	if v := c.Query("to"); v != "" {
		if x, e := time.Parse(time.RFC3339, v); e == nil {
			to = x
		}
	}
	return from, to
}
func riskFilter(c *gin.Context) store.CopyGuardFilter {
	return store.CopyGuardFilter{LeaderID: strings.TrimSpace(c.Query("leader_id")), Symbol: strings.TrimSpace(c.Query("symbol")), Status: strings.TrimSpace(c.Query("status")), ResultType: strings.TrimSpace(c.Query("result_type"))}
}

type copyGuardCycleArtifacts struct {
	Attempts      []*store.CopyGuardAttempt            `json:"attempts"`
	Events        []*store.CopyGuardEvent              `json:"events"`
	Protection    *store.CopyGuardProtectiveOrder      `json:"protection,omitempty"`
	WatchSamples  []*store.CopyGuardWatchSample        `json:"watch_samples"`
	Candidates    []*store.CopyGuardReentryCandidate   `json:"ai_candidates"`
	AIAnalyses    []*store.ReentryAIAnalysis           `json:"ai_analyses"`
	AIEvaluations []*store.ReentryAIDecisionEvaluation `json:"ai_decision_evaluations"`
}

type copyGuardAttemptAttribution struct {
	AttemptNo      int      `json:"attempt_no"`
	PnL            float64  `json:"pnl"`
	Fee            float64  `json:"fee"`
	Funding        float64  `json:"funding_fee"`
	StopOnly       bool     `json:"stop_only_path"`
	RecoverySec    *float64 `json:"first_recovery_seconds,omitempty"`
	PostStopMFEUSD *float64 `json:"post_stop_mfe_usd,omitempty"`
	PostStopMAEUSD *float64 `json:"post_stop_mae_usd,omitempty"`
}

type copyGuardAttribution struct {
	Final                      bool                          `json:"final"`
	LeaderDirectionReturn      float64                       `json:"leader_direction_return"`
	BaselineNoGuardPnL         float64                       `json:"baseline_no_guard_pnl"`
	StopOnlyPnL                float64                       `json:"stop_only_pnl"`
	ActualCopyGuardPnL         float64                       `json:"actual_copy_guard_pnl"`
	StopSavings                float64                       `json:"stop_savings"`
	MissedProfit               float64                       `json:"missed_profit"`
	ReentryContribution        float64                       `json:"reentry_contribution"`
	FirstReentryPnL            float64                       `json:"first_reentry_pnl"`
	SecondReentryPnL           float64                       `json:"second_reentry_pnl"`
	Fees                       float64                       `json:"fees"`
	Slippage                   float64                       `json:"slippage"`
	RealizedPathMaxDrawdownUSD float64                       `json:"realized_path_max_drawdown_usd"`
	WorstAttemptPnL            float64                       `json:"worst_attempt_pnl"`
	MaxPostStopMFEUSD          float64                       `json:"max_post_stop_mfe_usd"`
	MaxPostStopMAEUSD          float64                       `json:"max_post_stop_mae_usd"`
	Attempts                   []copyGuardAttemptAttribution `json:"attempts"`
}

func copyGuardNumber(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		x, err := v.Float64()
		return x, err == nil
	default:
		return 0, false
	}
}

func copyGuardAttemptRecovery(events []*store.CopyGuardEvent) map[int]map[string]float64 {
	out := make(map[int]map[string]float64)
	for _, event := range events {
		if event == nil || event.Type != "WATCH_SUMMARY" {
			continue
		}
		items, ok := event.Metadata["attempt_recovery"].([]interface{})
		if !ok {
			continue
		}
		for _, item := range items {
			values, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			attemptValue, ok := copyGuardNumber(values["attempt_no"])
			if !ok {
				continue
			}
			metrics := make(map[string]float64)
			for _, key := range []string{"first_recovery_seconds", "max_favorable_excursion_usd", "max_adverse_excursion_usd"} {
				if value, exists := copyGuardNumber(values[key]); exists {
					metrics[key] = value
				}
			}
			out[int(attemptValue)] = metrics
		}
	}
	return out
}

func buildCopyGuardAttribution(cycle *store.CopyGuardCycle, attempts []*store.CopyGuardAttempt, events []*store.CopyGuardEvent) copyGuardAttribution {
	a := copyGuardAttribution{BaselineNoGuardPnL: cycle.BaselinePnL, ActualCopyGuardPnL: cycle.ActualPnL, Fees: cycle.Fees, Slippage: cycle.Slippage, Attempts: make([]copyGuardAttemptAttribution, 0, len(attempts))}
	a.Final = cycle.ClosedAt != nil && cycle.AccountingStatus == store.CopyGuardAccountingReconciled
	recovery := copyGuardAttemptRecovery(events)
	peak, cumulative := float64(0), float64(0)
	if cycle.LeaderEntryPrice > 0 && cycle.LastObservedPrice > 0 {
		a.LeaderDirectionReturn = (cycle.LastObservedPrice - cycle.LeaderEntryPrice) / cycle.LeaderEntryPrice
		if strings.EqualFold(cycle.Side, "short") {
			a.LeaderDirectionReturn = -a.LeaderDirectionReturn
		}
	}
	for _, attempt := range attempts {
		if attempt == nil {
			continue
		}
		item := copyGuardAttemptAttribution{AttemptNo: attempt.AttemptNo, PnL: attempt.PnL, Fee: attempt.Fee, Funding: attempt.FundingFee, StopOnly: attempt.AttemptNo == 0}
		if metrics, ok := recovery[attempt.AttemptNo]; ok {
			if value, exists := metrics["first_recovery_seconds"]; exists && value >= 0 {
				item.RecoverySec = &value
			}
			if value, exists := metrics["max_favorable_excursion_usd"]; exists {
				item.PostStopMFEUSD = &value
				if value > a.MaxPostStopMFEUSD {
					a.MaxPostStopMFEUSD = value
				}
			}
			if value, exists := metrics["max_adverse_excursion_usd"]; exists {
				item.PostStopMAEUSD = &value
				if value > a.MaxPostStopMAEUSD {
					a.MaxPostStopMAEUSD = value
				}
			}
		}
		a.Attempts = append(a.Attempts, item)
		if attempt.PnL < a.WorstAttemptPnL {
			a.WorstAttemptPnL = attempt.PnL
		}
		cumulative += attempt.PnL
		if cumulative > peak {
			peak = cumulative
		}
		if drawdown := peak - cumulative; drawdown > a.RealizedPathMaxDrawdownUSD {
			a.RealizedPathMaxDrawdownUSD = drawdown
		}
		switch attempt.AttemptNo {
		case 0:
			a.StopOnlyPnL = attempt.PnL
		case 1:
			a.FirstReentryPnL = attempt.PnL
			a.ReentryContribution += attempt.PnL
		case 2:
			a.SecondReentryPnL = attempt.PnL
			a.ReentryContribution += attempt.PnL
		default:
			a.ReentryContribution += attempt.PnL
		}
	}
	if a.Final {
		delta := a.StopOnlyPnL - a.BaselineNoGuardPnL
		if delta >= 0 {
			a.StopSavings = delta
		} else {
			a.MissedProfit = -delta
		}
	}
	return a
}

func (h *CopyTradeHandler) loadCopyGuardCycleArtifacts(cycleID int64) (*copyGuardCycleArtifacts, error) {
	artifacts := &copyGuardCycleArtifacts{}
	var err error
	if artifacts.Events, err = h.store.CopyTrade().ListCopyGuardEvents(cycleID); err != nil {
		return nil, err
	}
	if artifacts.Attempts, err = h.store.CopyTrade().ListCopyGuardAttempts(cycleID); err != nil {
		return nil, err
	}
	if artifacts.Protection, err = h.store.CopyTrade().GetCopyGuardProtectiveOrder(cycleID); err != nil {
		artifacts.Protection = nil
	}
	if artifacts.WatchSamples, err = h.store.CopyTrade().ListCopyGuardWatchSamples(cycleID); err != nil {
		return nil, err
	}
	if artifacts.Candidates, err = h.store.ReentryAI().ListReentryCandidatesByCycle(cycleID); err != nil {
		return nil, err
	}
	if artifacts.AIAnalyses, err = h.store.ReentryAI().ListReentryAnalysesByCycle(cycleID, 500); err != nil {
		return nil, err
	}
	if artifacts.AIEvaluations, err = h.store.ReentryAI().ListReentryDecisionEvaluationsByCycle(cycleID); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func copyGuardCycleDocument(cycle *store.CopyGuardCycle, artifacts *copyGuardCycleArtifacts) gin.H {
	return gin.H{
		"schema_version":          5,
		"defaults_version":        store.CopyGuardDefaultsVersion(),
		"cycle":                   cycle,
		"attempts":                artifacts.Attempts,
		"events":                  artifacts.Events,
		"protection":              artifacts.Protection,
		"watch_samples":           artifacts.WatchSamples,
		"ai_candidates":           artifacts.Candidates,
		"ai_analyses":             artifacts.AIAnalyses,
		"ai_decision_evaluations": artifacts.AIEvaluations,
		"ai_effect_summary":       copyguardmetrics.SummarizeCycleAIEffects(cycle, artifacts.Attempts, artifacts.AIEvaluations),
		"attribution":             buildCopyGuardAttribution(cycle, artifacts.Attempts, artifacts.Events),
	}
}

func (h *CopyTradeHandler) GetRiskSummary(c *gin.Context) {
	ids, _, _, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	from, to := riskTimeRange(c)
	x, err := h.store.CopyTrade().CopyGuardSummary(ids, from, to, riskFilter(c))
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"summary": x, "from": from, "to": to})
}
func (h *CopyTradeHandler) GetRiskCycles(c *gin.Context) {
	ids, _, names, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	from, to := riskTimeRange(c)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	rows, err := h.store.CopyTrade().ListCopyGuardCycles(ids, from, to, limit, offset, riskFilter(c))
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	for _, cycle := range rows {
		cycle.TraderName = names[cycle.TraderID]
	}
	c.JSON(200, gin.H{"cycles": rows, "count": len(rows)})
}
func (h *CopyTradeHandler) GetRiskCycle(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid cycle id"})
		return
	}
	_, owned, names, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	cycle, err := h.store.CopyTrade().GetCopyGuardCycle(id)
	if err != nil {
		c.JSON(404, gin.H{"error": "cycle not found"})
		return
	}
	if !owned[cycle.TraderID] {
		c.JSON(403, gin.H{"error": "forbidden"})
		return
	}
	cycle.TraderName = names[cycle.TraderID]
	artifacts, err := h.loadCopyGuardCycleArtifacts(id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, copyGuardCycleDocument(cycle, artifacts))
}

func (h *CopyTradeHandler) ExportRiskCycle(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cycle id"})
		return
	}
	_, owned, names, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	cycle, err := h.store.CopyTrade().GetCopyGuardCycle(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "cycle not found"})
		return
	}
	if !owned[cycle.TraderID] {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	cycle.TraderName = names[cycle.TraderID]
	artifacts, err := h.loadCopyGuardCycleArtifacts(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=copy-guard-cycle-%d.jsonl", id))
	doc := copyGuardCycleDocument(cycle, artifacts)
	doc["exported_at"] = time.Now().UTC()
	_ = json.NewEncoder(c.Writer).Encode(doc)
}
func (h *CopyTradeHandler) ExportRiskCycles(c *gin.Context) {
	ids, _, names, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	from, to := riskTimeRange(c)
	format := strings.ToLower(c.DefaultQuery("format", "csv"))
	if format != "csv" && format != "jsonl" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "format must be csv or jsonl"})
		return
	}
	if format == "jsonl" {
		c.Header("Content-Type", "application/x-ndjson")
		c.Header("Content-Disposition", "attachment; filename=copy-guard.jsonl")
		enc := json.NewEncoder(c.Writer)
		for offset := 0; ; offset += 500 {
			rows, err := h.store.CopyTrade().ListCopyGuardCycles(ids, from, to, 500, offset, riskFilter(c))
			if err != nil {
				return
			}
			for _, cycle := range rows {
				cycle.TraderName = names[cycle.TraderID]
				artifacts, loadErr := h.loadCopyGuardCycleArtifacts(cycle.ID)
				if loadErr != nil {
					logger.Warnf("Copy Guard schema v5 export skipped cycle=%d: %v", cycle.ID, loadErr)
					continue
				}
				doc := copyGuardCycleDocument(cycle, artifacts)
				doc["exported_at"] = time.Now().UTC()
				_ = enc.Encode(doc)
			}
			if len(rows) < 500 {
				break
			}
		}
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=copy-guard.csv")
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"cycle_id", "trader_id", "trader_name", "leader_id", "leader_pos_id", "symbol", "side", "margin_mode", "status", "accounting_status", "accounting_error", "tracking_difference", "protection_status", "protection_coverage", "protection_retries", "protection_missing_seconds", "protection_error", "stop_count", "reentry_count", "actual_pnl", "baseline_no_guard_pnl", "stop_only_pnl", "first_reentry_pnl", "second_reentry_pnl", "reentry_contribution", "realized_path_max_drawdown_usd", "worst_attempt_pnl", "max_post_stop_mfe_usd", "max_post_stop_mae_usd", "net_guard_effect", "fees", "funding_fee", "liquidation_penalty", "slippage", "ai_decisions", "ai_scorable", "ai_unscorable", "ai_missed_reversals", "ai_correct_abandons", "ai_risk_gate_saved_losses", "ai_final_decision", "ai_final_outcome", "ai_actual_reentry_pnl", "ai_evaluation_version", "opened_at", "closed_at"})
	for offset := 0; ; offset += 500 {
		rows, err := h.store.CopyTrade().ListCopyGuardCycles(ids, from, to, 500, offset, riskFilter(c))
		if err != nil {
			return
		}
		for _, x := range rows {
			x.TraderName = names[x.TraderID]
			closed := ""
			if x.ClosedAt != nil {
				closed = x.ClosedAt.Format(time.RFC3339)
			}
			attempts, _ := h.store.CopyTrade().ListCopyGuardAttempts(x.ID)
			events, _ := h.store.CopyTrade().ListCopyGuardEvents(x.ID)
			attribution := buildCopyGuardAttribution(x, attempts, events)
			evaluations, _ := h.store.ReentryAI().ListReentryDecisionEvaluationsByCycle(x.ID)
			aiEffect := copyguardmetrics.SummarizeCycleAIEffects(x, attempts, evaluations)
			_ = w.Write([]string{strconv.FormatInt(x.ID, 10), x.TraderID, x.TraderName, x.LeaderID, x.LeaderPosID, x.Symbol, x.Side, x.MarginMode, x.Status, x.AccountingStatus, x.AccountingError, fmt.Sprint(x.TrackingDifference), x.ProtectionStatus, fmt.Sprint(x.ProtectionCoverage), strconv.Itoa(x.ProtectionRetries), fmt.Sprint(x.ProtectionMissingSeconds), x.ProtectionError, strconv.Itoa(x.StopCount), strconv.Itoa(x.ReentryCount), fmt.Sprint(x.ActualPnL), fmt.Sprint(x.BaselinePnL), fmt.Sprint(attribution.StopOnlyPnL), fmt.Sprint(attribution.FirstReentryPnL), fmt.Sprint(attribution.SecondReentryPnL), fmt.Sprint(attribution.ReentryContribution), fmt.Sprint(attribution.RealizedPathMaxDrawdownUSD), fmt.Sprint(attribution.WorstAttemptPnL), fmt.Sprint(attribution.MaxPostStopMFEUSD), fmt.Sprint(attribution.MaxPostStopMAEUSD), fmt.Sprint(x.NetGuardEffect), fmt.Sprint(x.Fees), fmt.Sprint(x.FundingFee), fmt.Sprint(x.LiquidationPenalty), fmt.Sprint(x.Slippage), strconv.Itoa(aiEffect.TotalDecisions), strconv.Itoa(aiEffect.ScorableDecisions), strconv.Itoa(aiEffect.UnscorableDecisions), strconv.Itoa(aiEffect.MissedReversals), strconv.Itoa(aiEffect.CorrectAbandons), strconv.Itoa(aiEffect.RiskGateSavedLosses), aiEffect.FinalDecision, aiEffect.FinalDecisionOutcome, fmt.Sprint(aiEffect.ActualReentryPnL), strconv.Itoa(aiEffect.EvaluationVersion), x.OpenedAt.Format(time.RFC3339), closed})
		}
		w.Flush()
		if len(rows) < 500 {
			break
		}
	}
	w.Flush()
}

// ============================================================================
// 统一跟单事件日志 API
// ============================================================================

// copyEventWindows 导出/查询支持的时间窗（用于给 AI 分析）。
var copyEventWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"3h":  3 * time.Hour,
	"5h":  5 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"15d": 15 * 24 * time.Hour,
}

// copyEventTimeRange 解析时间范围：优先 window（1h/3h/5h/24h/7d/15d），
// 否则回退 from/to（RFC3339），默认最近 24 小时。
func copyEventTimeRange(c *gin.Context) (time.Time, time.Time) {
	to := time.Now().UTC().Add(time.Second)
	if w := strings.TrimSpace(c.Query("window")); w != "" {
		if d, ok := copyEventWindows[w]; ok {
			return to.Add(-d), to
		}
	}
	from := to.AddDate(0, 0, -1)
	if v := c.Query("from"); v != "" {
		if x, e := time.Parse(time.RFC3339, v); e == nil {
			from = x
		}
	}
	if v := c.Query("to"); v != "" {
		if x, e := time.Parse(time.RFC3339, v); e == nil {
			to = x
		}
	}
	return from, to
}

func copyEventFilter(c *gin.Context) store.CopyEventFilter {
	return store.CopyEventFilter{
		Provider:  strings.TrimSpace(c.Query("provider")),
		Category:  strings.TrimSpace(c.Query("category")),
		Severity:  strings.TrimSpace(c.Query("severity")),
		Symbol:    strings.TrimSpace(strings.ToUpper(c.Query("symbol"))),
		EventType: strings.TrimSpace(c.Query("event_type")),
	}
}

// GetCopyEvents 查询系统内全部交易员的跟单事件（时间倒序，分页）。
// 作用域为全局（跨用户）：见 allTraderIDs 说明。响应额外携带 traders（id->名称）供前端筛选下拉使用。
func (h *CopyTradeHandler) GetCopyEvents(c *gin.Context) {
	ids, names, err := h.allTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	from, to := copyEventTimeRange(c)
	filter := copyEventFilter(c)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	events, err := h.store.CopyTrade().QueryCopyEvents(ids, from, to, filter, limit, offset)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	for _, e := range events {
		e.TraderName = names[e.TraderID]
	}
	total, err := h.store.CopyTrade().CountCopyEvents(ids, from, to, filter)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"events": events, "count": len(events), "total": total, "from": from, "to": to, "traders": names})
}

// ExportCopyEvents 导出跟单事件（csv|jsonl），时间戳为 ISO UTC，便于喂 AI 分析。
// 作用域为全局（跨用户）：见 allTraderIDs 说明。
func (h *CopyTradeHandler) ExportCopyEvents(c *gin.Context) {
	ids, names, err := h.allTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	from, to := copyEventTimeRange(c)
	filter := copyEventFilter(c)
	format := strings.ToLower(c.DefaultQuery("format", "jsonl"))
	if format != "csv" && format != "jsonl" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "format must be csv or jsonl"})
		return
	}
	window := strings.TrimSpace(c.Query("window"))
	if window == "" {
		window = "range"
	}

	if format == "jsonl" {
		c.Header("Content-Type", "application/x-ndjson")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=copy-events-%s.jsonl", window))
		enc := json.NewEncoder(c.Writer)
		_ = h.store.CopyTrade().StreamCopyEvents(ids, from, to, filter, func(e *store.CopyTradeEvent) error {
			e.TraderName = names[e.TraderID]
			return enc.Encode(e)
		})
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=copy-events-%s.csv", window))
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"created_at_utc", "trader_id", "trader_name", "leader_id", "provider", "category", "event_type", "severity", "symbol", "side", "margin_mode", "status", "leader_pos_id", "cycle_id", "signal_id", "price", "quantity", "notional", "pnl", "operator", "summary"})
	_ = h.store.CopyTrade().StreamCopyEvents(ids, from, to, filter, func(e *store.CopyTradeEvent) error {
		cycleID := ""
		if e.CycleID > 0 {
			cycleID = strconv.FormatInt(e.CycleID, 10)
		}
		_ = w.Write([]string{
			e.CreatedAt.UTC().Format(time.RFC3339), e.TraderID, names[e.TraderID], e.LeaderID, e.ProviderType,
			e.Category, e.EventType, e.Severity, e.Symbol, e.Side, e.MarginMode, e.Status,
			e.LeaderPosID, cycleID, e.SignalID,
			fmt.Sprint(e.Price), fmt.Sprint(e.Quantity), fmt.Sprint(e.Notional), fmt.Sprint(e.PnL),
			e.Operator, e.Summary,
		})
		return nil
	})
	w.Flush()
}

// ============================================================================
// v5.1 人工重入信号 API
// ============================================================================

// ListManualSignals 列出当前用户所属交易员的人工重入信号
// @Summary 人工重入信号列表
// @Tags CopyTrade
// @Param trader_id query string false "只看某个交易员"
// @Param status query string false "状态过滤，逗号分隔（如 PENDING,EXECUTED）；空=全部"
// @Router /api/copytrade/risk/manual-signals [get]
func (h *CopyTradeHandler) ListManualSignals(c *gin.Context) {
	ids, _, names, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	var statuses []string
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(strings.ToUpper(s)); s != "" {
				statuses = append(statuses, s)
			}
		}
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	signals, err := h.store.CopyTrade().ListManualReentrySignals(ids, statuses, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	for _, sig := range signals {
		sig.TraderName = names[sig.TraderID]
	}
	c.JSON(200, gin.H{"signals": signals, "count": len(signals)})
}

// getOwnedManualSignal 解析 :id 并校验信号归属当前用户，失败时已写响应
func (h *CopyTradeHandler) getOwnedManualSignal(c *gin.Context) *store.CopyGuardManualReentrySignal {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid signal id"})
		return nil
	}
	_, owned, _, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return nil
	}
	sig, err := h.store.CopyTrade().GetManualReentrySignal(id)
	if err != nil {
		c.JSON(404, gin.H{"error": "signal not found"})
		return nil
	}
	if !owned[sig.TraderID] {
		c.JSON(403, gin.H{"error": "signal not owned by current user"})
		return nil
	}
	return sig
}

// ConfirmManualSignal 确认人工重入信号（系统代执行）
// @Summary 确认人工重入
// @Tags CopyTrade
// @Param id path int true "Signal ID"
// @Router /api/copytrade/risk/manual-signals/{id}/confirm [post]
func (h *CopyTradeHandler) ConfirmManualSignal(c *gin.Context) {
	sig := h.getOwnedManualSignal(c)
	if sig == nil {
		return
	}
	c.JSON(http.StatusGone, gin.H{
		"error":         "manual reentry confirmation was retired in Copy Guard v7",
		"deprecated":    true,
		"candidate_api": "/api/copytrade/risk/ai-candidates",
	})
}

// DismissManualSignal 忽略人工重入信号
// @Summary 忽略人工重入信号
// @Tags CopyTrade
// @Param id path int true "Signal ID"
// @Router /api/copytrade/risk/manual-signals/{id}/dismiss [post]
func (h *CopyTradeHandler) DismissManualSignal(c *gin.Context) {
	sig := h.getOwnedManualSignal(c)
	if sig == nil {
		return
	}
	c.JSON(http.StatusGone, gin.H{
		"error":         "manual reentry dismissal was retired in Copy Guard v7; terminate the AI candidate instead",
		"deprecated":    true,
		"candidate_api": "/api/copytrade/risk/ai-candidates",
	})
}

// CopyTradeConfigRequest 跟单配置请求
type CopyTradeConfigRequest struct {
	ProviderType   string  `json:"provider_type" binding:"required,oneof=hyperliquid okx binance"`
	LeaderID       string  `json:"leader_id"`
	CopyRatio      float64 `json:"copy_ratio" binding:"required,gt=0"`
	SyncLeverage   bool    `json:"sync_leverage"`
	SyncMarginMode bool    `json:"sync_margin_mode"`
	MinTradeWarn   float64 `json:"min_trade_warn"`
	MaxTradeWarn   float64 `json:"max_trade_warn"`
	Enabled        bool    `json:"enabled"`

	// Binance Web 凭证（仅 ProviderType=binance 时使用）
	BinanceP20T        string `json:"binance_p20t"`
	BinanceCSRFToken   string `json:"binance_csrf_token"`
	BinanceSourceMode  string `json:"binance_source_mode"`
	BinanceTopTraderID string `json:"binance_top_trader_id"`

	// ============================================================
	// Copy Guard v7 风控与 AI guarded 重入配置
	// 所有字段都是可选的（前端不传走 store 默认值，详见 store.CopyTradeConfig.FillRiskDefaults）
	// v3 遗留字段（risk_atr_enabled / risk_reentry_tolerance / 反加仓铁律 /
	// risk_stop_noise_floor_atr / risk_cycle_max_loss_pct）已随 v5 下线，
	// 前端传入将被忽略。
	// ============================================================
	RiskStopLossEnabled        *bool    `json:"risk_stop_loss_enabled,omitempty"`
	RiskAccountPct             *float64 `json:"risk_account_pct,omitempty"`
	RiskATRMultiplier          *float64 `json:"risk_atr_multiplier,omitempty"`
	RiskATRTimeframe           *string  `json:"risk_atr_timeframe,omitempty"`
	RiskLeverageFallback       *bool    `json:"risk_leverage_fallback,omitempty"`
	RiskLeverageMaxLoss        *float64 `json:"risk_leverage_max_loss,omitempty"`
	RiskReentryEnabled         *bool    `json:"risk_reentry_enabled,omitempty"`
	RiskReentryRatio           *float64 `json:"risk_reentry_ratio,omitempty"`
	RiskReentryDecisionMode    *string  `json:"risk_reentry_decision_mode,omitempty"`
	RiskCycleLossBudgetPct     *float64 `json:"risk_cycle_loss_budget_pct,omitempty"`
	RiskPortfolioLossBudgetPct *float64 `json:"risk_portfolio_loss_budget_pct,omitempty"`
	RiskRoundTripFeeBPS        *float64 `json:"risk_round_trip_fee_bps,omitempty"`
	RiskAIConfidenceThreshold  *float64 `json:"risk_ai_confidence_threshold,omitempty"`
	RiskAIMinReviewSeconds     *int     `json:"risk_ai_min_review_seconds,omitempty"`
	RiskAIDailyCallLimit       *int     `json:"risk_ai_daily_call_limit,omitempty"`
	RiskAILifecycleCallLimit   *int     `json:"risk_ai_lifecycle_call_limit,omitempty"`
	RiskNotificationLevel      *string  `json:"risk_notification_level,omitempty"`
	// 历史人工重入兼容字段；v7 固定 false
	RiskManualReentryEnabled *bool `json:"risk_manual_reentry_enabled,omitempty"`

	RiskPolicyVersion          *int     `json:"risk_policy_version,omitempty"`
	RiskStopMode               *string  `json:"risk_stop_mode,omitempty"`
	RiskATRPeriod              *int     `json:"risk_atr_period,omitempty"`
	RiskATRCacheMaxAgeMinutes  *int     `json:"risk_atr_cache_max_age_minutes,omitempty"`
	RiskATRFallbackPct         *float64 `json:"risk_atr_fallback_pct,omitempty"`
	RiskTriggerPriceType       *string  `json:"risk_trigger_price_type,omitempty"`
	RiskSlippageBufferBPS      *float64 `json:"risk_slippage_buffer_bps,omitempty"`
	RiskLiquidationBufferATR   *float64 `json:"risk_liquidation_buffer_atr,omitempty"`
	RiskMaxReentries           *int     `json:"risk_max_reentries,omitempty"`
	RiskReentryBandATR         *float64 `json:"risk_reentry_band_atr,omitempty"`
	RiskReentryCooldownSeconds *int     `json:"risk_reentry_cooldown_seconds,omitempty"`
	RiskReentryMaxChaseATR     *float64 `json:"risk_reentry_max_chase_atr,omitempty"`
	RiskReentryMaxATRExpansion *float64 `json:"risk_reentry_max_atr_expansion,omitempty"`
	RiskWatchTimeoutMinutes    *int     `json:"risk_watch_timeout_minutes,omitempty"`
	RiskMigrationConfirmed     *bool    `json:"risk_migration_confirmed,omitempty"`
	RiskAddonBudgetPct         *float64 `json:"risk_addon_budget_pct,omitempty"`
	RiskHighRiskConfirmed      bool     `json:"risk_high_risk_confirmed,omitempty"`
	RiskExtremeConfirmValue    *float64 `json:"risk_extreme_risk_confirm_value,omitempty"`

	// v4.1 重入加严（字段含义见 store.CopyTradeConfig 注释）
	RiskReentryMinRecoveryATR     *float64 `json:"risk_reentry_min_recovery_atr,omitempty"`
	RiskReentryCooldownEscalation *float64 `json:"risk_reentry_cooldown_escalation,omitempty"`
	RiskReentryRecoveryEscalation *float64 `json:"risk_reentry_recovery_escalation,omitempty"`

	// v5 可保护性状态机 / 噪音档重入
	RiskUnprotectableAction  *string `json:"risk_unprotectable_action,omitempty"`
	RiskReentryNoiseOverride *bool   `json:"risk_reentry_noise_override,omitempty"`
}

// GetConfig 获取跟单配置
// @Summary 获取跟单配置
// @Tags CopyTrade
// @Param trader_id path string true "Trader ID"
// @Success 200 {object} store.CopyTradeConfig
// @Router /api/copytrade/config/{trader_id} [get]
func (h *CopyTradeHandler) GetConfig(c *gin.Context) {
	traderID := c.Param("trader_id")

	config, err := h.store.CopyTrade().GetByTraderID(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "copy trade config not found",
		})
		return
	}

	var sourceHealth interface{}
	if health, healthErr := h.store.CopyTrade().GetSourceHealth(traderID); healthErr == nil && health != nil &&
		health.SourceGeneration == config.SourceGeneration && health.LeaderID == config.LeaderID &&
		health.SourceMode == config.BinanceSourceMode {
		copy := *health
		copy.LastError = sanitizeSourceHealthError(copy.LastError)
		if unsupported, listErr := h.store.CopyTrade().ListUnsupportedExecutionInstruments(traderID); listErr == nil {
			copy.UnsupportedContracts = unsupported
		}
		sourceHealth = &copy
	}
	c.JSON(http.StatusOK, gin.H{
		"config":        maskedCopyTradeConfig(config),
		"status":        copytrade.IsCopyTradingRunning(traderID),
		"source_health": sourceHealth,
	})
}

func sanitizeSourceHealthError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 300 {
		value = value[:300] + "..."
	}
	return value
}

// GetRiskDefaults 返回新交易员 Copy Guard 推荐默认值。前端不得再复制一份常量。
func (h *CopyTradeHandler) GetRiskDefaults(c *gin.Context) {
	d := store.NewCopyGuardDefaults()
	c.JSON(http.StatusOK, gin.H{
		"defaults_version": store.CopyGuardDefaultsVersion(),
		"defaults":         d,
	})
}

// maskedCopyTradeConfig 返回凭证已脱敏的配置副本（API 响应专用，不落库）。
// 前端编辑时掩码值原样回传，保存路径通过 resolveCredentialUpdate 识别为"未修改"。
func maskedCopyTradeConfig(config *store.CopyTradeConfig) *store.CopyTradeConfig {
	if config == nil {
		return nil
	}
	masked := *config
	masked.BinanceP20T = store.MaskSecret(masked.BinanceP20T)
	masked.BinanceCSRFToken = store.MaskSecret(masked.BinanceCSRFToken)
	return &masked
}

// SaveConfig 保存跟单配置
// @Summary 保存跟单配置
// @Tags CopyTrade
// @Param trader_id path string true "Trader ID"
// @Param config body CopyTradeConfigRequest true "Config"
// @Success 200 {object} map[string]interface{}
// @Router /api/copytrade/config/{trader_id} [post]
func (h *CopyTradeHandler) SaveConfig(c *gin.Context) {
	traderID := c.Param("trader_id")

	var req CopyTradeConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// 跟单系数上限校验（与前端滑杆上限 3 保持数量级一致，留出直连 API 余量）：
	// 无上限时误传 100 会以百倍杠杆化仓位跟单，属于资金安全隐患
	if req.CopyRatio > maxCopyRatio {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("copy_ratio %.2f exceeds max %.0f", req.CopyRatio, maxCopyRatio)})
		return
	}

	// 读取已有配置，作为风控字段与凭证字段的旧值兜底
	// 设计：风控字段全部可选；前端不传字段保持原值；旧库读出来的默认值由 FillRiskDefaults 兜底
	existing, _ := h.store.CopyTrade().GetByTraderID(traderID)

	existingP20T, existingCSRF := "", ""
	if existing != nil {
		existingP20T, existingCSRF = existing.BinanceP20T, existing.BinanceCSRFToken
	}

	// 构造配置
	config := &store.CopyTradeConfig{
		TraderID:       traderID,
		ProviderType:   req.ProviderType,
		LeaderID:       req.LeaderID,
		CopyRatio:      req.CopyRatio,
		SyncLeverage:   req.SyncLeverage,
		SyncMarginMode: req.SyncMarginMode,
		MinTradeWarn:   req.MinTradeWarn,
		MaxTradeWarn:   req.MaxTradeWarn,
		Enabled:        req.Enabled,
		// GET 接口已脱敏返回凭证；编辑回传的掩码值/空值不覆盖已存明文
		BinanceP20T:        resolveCredentialUpdate(req.BinanceP20T, existingP20T),
		BinanceCSRFToken:   resolveCredentialUpdate(req.BinanceCSRFToken, existingCSRF),
		BinanceSourceMode:  req.BinanceSourceMode,
		BinanceTopTraderID: req.BinanceTopTraderID,
	}

	// 风控字段透传（指针类型，未传则使用旧值或默认值）
	if req.RiskStopLossEnabled != nil {
		config.RiskStopLossEnabled = *req.RiskStopLossEnabled
	} else if existing != nil {
		config.RiskStopLossEnabled = existing.RiskStopLossEnabled
	} else {
		config.RiskStopLossEnabled = true // 默认 on
	}
	if req.RiskAccountPct != nil {
		config.RiskAccountPct = *req.RiskAccountPct
	} else if existing != nil {
		config.RiskAccountPct = existing.RiskAccountPct
	}
	if req.RiskATRMultiplier != nil {
		config.RiskATRMultiplier = *req.RiskATRMultiplier
	} else if existing != nil {
		config.RiskATRMultiplier = existing.RiskATRMultiplier
	}
	if req.RiskATRTimeframe != nil {
		config.RiskATRTimeframe = *req.RiskATRTimeframe
	} else if existing != nil {
		config.RiskATRTimeframe = existing.RiskATRTimeframe
	}
	if req.RiskLeverageFallback != nil {
		config.RiskLeverageFallback = *req.RiskLeverageFallback
	} else if existing != nil {
		config.RiskLeverageFallback = existing.RiskLeverageFallback
	} else {
		// v5.2 默认 off（抗噪默认态，与 api/server.go、store 层 FillRiskDefaults
		// 保持一致）：margin_cap 在高杠杆下会把止损压进噪音区
		config.RiskLeverageFallback = false
	}
	if req.RiskLeverageMaxLoss != nil {
		config.RiskLeverageMaxLoss = *req.RiskLeverageMaxLoss
	} else if existing != nil {
		config.RiskLeverageMaxLoss = existing.RiskLeverageMaxLoss
	}
	if req.RiskReentryEnabled != nil {
		config.RiskReentryEnabled = *req.RiskReentryEnabled
	} else if existing != nil {
		config.RiskReentryEnabled = existing.RiskReentryEnabled
	}
	if req.RiskReentryRatio != nil {
		config.RiskReentryRatio = *req.RiskReentryRatio
	} else if existing != nil {
		config.RiskReentryRatio = existing.RiskReentryRatio
	}
	if req.RiskReentryDecisionMode != nil {
		config.RiskReentryDecisionMode = *req.RiskReentryDecisionMode
	} else if existing != nil {
		config.RiskReentryDecisionMode = existing.RiskReentryDecisionMode
	} else {
		config.RiskReentryDecisionMode = "ai_guarded"
	}
	if req.RiskCycleLossBudgetPct != nil {
		config.RiskCycleLossBudgetPct = *req.RiskCycleLossBudgetPct
	} else if existing != nil {
		config.RiskCycleLossBudgetPct = existing.RiskCycleLossBudgetPct
	}
	if req.RiskPortfolioLossBudgetPct != nil {
		config.RiskPortfolioLossBudgetPct = *req.RiskPortfolioLossBudgetPct
	} else if existing != nil {
		config.RiskPortfolioLossBudgetPct = existing.RiskPortfolioLossBudgetPct
	}
	if req.RiskRoundTripFeeBPS != nil {
		config.RiskRoundTripFeeBPS = *req.RiskRoundTripFeeBPS
	} else if existing != nil {
		config.RiskRoundTripFeeBPS = existing.RiskRoundTripFeeBPS
	}
	if req.RiskAIConfidenceThreshold != nil {
		config.RiskAIConfidenceThreshold = *req.RiskAIConfidenceThreshold
	} else if existing != nil {
		config.RiskAIConfidenceThreshold = existing.RiskAIConfidenceThreshold
	}
	if req.RiskAIMinReviewSeconds != nil {
		config.RiskAIMinReviewSeconds = *req.RiskAIMinReviewSeconds
	} else if existing != nil {
		config.RiskAIMinReviewSeconds = existing.RiskAIMinReviewSeconds
	}
	if req.RiskAIDailyCallLimit != nil {
		config.RiskAIDailyCallLimit = *req.RiskAIDailyCallLimit
	} else if existing != nil {
		config.RiskAIDailyCallLimit = existing.RiskAIDailyCallLimit
	}
	if req.RiskAILifecycleCallLimit != nil {
		config.RiskAILifecycleCallLimit = *req.RiskAILifecycleCallLimit
	} else if existing != nil {
		config.RiskAILifecycleCallLimit = existing.RiskAILifecycleCallLimit
	}
	if req.RiskNotificationLevel != nil {
		config.RiskNotificationLevel = *req.RiskNotificationLevel
	} else if existing != nil {
		config.RiskNotificationLevel = existing.RiskNotificationLevel
	}
	if req.RiskManualReentryEnabled != nil {
		config.RiskManualReentryEnabled = *req.RiskManualReentryEnabled
	} else if existing != nil {
		config.RiskManualReentryEnabled = existing.RiskManualReentryEnabled
	} else {
		config.RiskManualReentryEnabled = false // v7 已废弃逐笔人工确认
	}
	applyCopyGuardV4Request(config, existing, &req)
	if config.RiskPolicyVersion >= 4 && !copytrade.SupportsCopyGuard(copytrade.ProviderType(config.ProviderType)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "copy guard is only supported for OKX or Binance leader sources"})
		return
	}
	if config.RiskPolicyVersion >= 4 {
		if err := validateRiskConfirmation(config.RiskAccountPct, req.RiskHighRiskConfirmed, req.RiskExtremeConfirmValue); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if err := validateLegacyReentrySelection(existing, config.RiskReentryDecisionMode); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateAIGuardedPrerequisites(h.store, config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := copytrade.ValidateStoredRiskPolicy(config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := prepareCopyTradeSource(h.store, config, existing); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if copyTradeSourceIdentityChanged(existing, config) && copytrade.IsCopyTradingRunning(traderID) {
		c.JSON(http.StatusConflict, gin.H{"error": "跟单引擎仍在运行；切换领航员数据源前请先停止跟单，保存后再重新启动"})
		return
	}

	// 保存配置（store.Upsert 内部会调 FillRiskDefaults 做最后兜底）
	if err := h.store.CopyTrade().Upsert(config); err != nil {
		logger.Errorf("Failed to save copy trade config: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config"})
		return
	}

	// 更新 trader 的决策模式
	if req.Enabled {
		h.store.CopyTrade().UpdateDecisionMode(traderID, "copy_trade")
	} else {
		h.store.CopyTrade().UpdateDecisionMode(traderID, "ai")
	}

	logger.Infof("✓ Saved copy trade config for trader %s: provider=%s leader=%s ratio=%.0f%%",
		traderID, req.ProviderType, req.LeaderID, req.CopyRatio*100)

	c.JSON(http.StatusOK, gin.H{
		"message": "config saved",
		"config":  maskedCopyTradeConfig(config),
	})
}

func applyCopyGuardV4Request(c, old *store.CopyTradeConfig, r *CopyTradeConfigRequest) {
	if r.RiskPolicyVersion != nil {
		c.RiskPolicyVersion = *r.RiskPolicyVersion
	} else if old != nil {
		c.RiskPolicyVersion = old.RiskPolicyVersion
	}
	if r.RiskStopMode != nil {
		c.RiskStopMode = *r.RiskStopMode
	} else if old != nil {
		c.RiskStopMode = old.RiskStopMode
	}
	if r.RiskATRPeriod != nil {
		c.RiskATRPeriod = *r.RiskATRPeriod
	} else if old != nil {
		c.RiskATRPeriod = old.RiskATRPeriod
	}
	if r.RiskATRCacheMaxAgeMinutes != nil {
		c.RiskATRCacheMaxAgeMinutes = *r.RiskATRCacheMaxAgeMinutes
	} else if old != nil {
		c.RiskATRCacheMaxAgeMinutes = old.RiskATRCacheMaxAgeMinutes
	}
	if r.RiskATRFallbackPct != nil {
		c.RiskATRFallbackPct = *r.RiskATRFallbackPct
	} else if old != nil {
		c.RiskATRFallbackPct = old.RiskATRFallbackPct
	}
	if r.RiskTriggerPriceType != nil {
		c.RiskTriggerPriceType = *r.RiskTriggerPriceType
	} else if old != nil {
		c.RiskTriggerPriceType = old.RiskTriggerPriceType
	}
	if r.RiskSlippageBufferBPS != nil {
		c.RiskSlippageBufferBPS = *r.RiskSlippageBufferBPS
	} else if old != nil {
		c.RiskSlippageBufferBPS = old.RiskSlippageBufferBPS
	}
	if r.RiskLiquidationBufferATR != nil {
		c.RiskLiquidationBufferATR = *r.RiskLiquidationBufferATR
	} else if old != nil {
		c.RiskLiquidationBufferATR = old.RiskLiquidationBufferATR
	}
	if r.RiskMaxReentries != nil {
		c.RiskMaxReentries = *r.RiskMaxReentries
	} else if old != nil {
		c.RiskMaxReentries = old.RiskMaxReentries
	}
	if r.RiskReentryBandATR != nil {
		c.RiskReentryBandATR = *r.RiskReentryBandATR
	} else if old != nil {
		c.RiskReentryBandATR = old.RiskReentryBandATR
	}
	if r.RiskReentryCooldownSeconds != nil {
		c.RiskReentryCooldownSeconds = *r.RiskReentryCooldownSeconds
	} else if old != nil {
		c.RiskReentryCooldownSeconds = old.RiskReentryCooldownSeconds
	}
	if r.RiskReentryMaxChaseATR != nil {
		c.RiskReentryMaxChaseATR = *r.RiskReentryMaxChaseATR
	} else if old != nil {
		c.RiskReentryMaxChaseATR = old.RiskReentryMaxChaseATR
	}
	if r.RiskReentryMaxATRExpansion != nil {
		c.RiskReentryMaxATRExpansion = *r.RiskReentryMaxATRExpansion
	} else if old != nil {
		c.RiskReentryMaxATRExpansion = old.RiskReentryMaxATRExpansion
	}
	if r.RiskWatchTimeoutMinutes != nil {
		c.RiskWatchTimeoutMinutes = *r.RiskWatchTimeoutMinutes
	} else if old != nil {
		c.RiskWatchTimeoutMinutes = old.RiskWatchTimeoutMinutes
	}
	if r.RiskMigrationConfirmed != nil {
		c.RiskMigrationConfirmed = *r.RiskMigrationConfirmed
	} else if old != nil {
		c.RiskMigrationConfirmed = old.RiskMigrationConfirmed
	}
	if r.RiskAddonBudgetPct != nil {
		c.RiskAddonBudgetPct = *r.RiskAddonBudgetPct
	} else if old != nil {
		c.RiskAddonBudgetPct = old.RiskAddonBudgetPct
	}
	if r.RiskReentryMinRecoveryATR != nil {
		c.RiskReentryMinRecoveryATR = *r.RiskReentryMinRecoveryATR
	} else if old != nil {
		c.RiskReentryMinRecoveryATR = old.RiskReentryMinRecoveryATR
	}
	if r.RiskReentryCooldownEscalation != nil {
		c.RiskReentryCooldownEscalation = *r.RiskReentryCooldownEscalation
	} else if old != nil {
		c.RiskReentryCooldownEscalation = old.RiskReentryCooldownEscalation
	}
	if r.RiskReentryRecoveryEscalation != nil {
		c.RiskReentryRecoveryEscalation = *r.RiskReentryRecoveryEscalation
	} else if old != nil {
		c.RiskReentryRecoveryEscalation = old.RiskReentryRecoveryEscalation
	}
	if r.RiskUnprotectableAction != nil {
		c.RiskUnprotectableAction = *r.RiskUnprotectableAction
	} else if old != nil {
		c.RiskUnprotectableAction = old.RiskUnprotectableAction
	}
	if r.RiskReentryNoiseOverride != nil {
		c.RiskReentryNoiseOverride = *r.RiskReentryNoiseOverride
	} else if old != nil {
		c.RiskReentryNoiseOverride = old.RiskReentryNoiseOverride
	}
	if c.RiskPolicyVersion >= 4 {
		// Only missing fields on a brand-new v4 config receive balanced defaults.
		// Existing values, including valid explicit zero values, are preserved.
		if old == nil {
			if r.RiskSlippageBufferBPS == nil {
				c.RiskSlippageBufferBPS = 10
			}
			if r.RiskLiquidationBufferATR == nil {
				c.RiskLiquidationBufferATR = 0.5
			}
			if r.RiskMaxReentries == nil {
				// v5：默认单周期最多重入 1 次（whipsaw 磨损收敛）
				c.RiskMaxReentries = 1
			}
			if r.RiskReentryBandATR == nil {
				c.RiskReentryBandATR = 0.5
			}
			if r.RiskReentryCooldownSeconds == nil {
				// v4.1：默认冷却 300s（旧默认 60s 在高杠杆震荡下重入过快）
				c.RiskReentryCooldownSeconds = 300
			}
			if r.RiskReentryMaxChaseATR == nil {
				c.RiskReentryMaxChaseATR = 0.5
			}
			if r.RiskReentryMaxATRExpansion == nil {
				c.RiskReentryMaxATRExpansion = 2
			}
			if r.RiskReentryEnabled == nil {
				c.RiskReentryEnabled = true
			}
		}
		c.FillRiskDefaults()
	}
}

// DeleteConfig 删除跟单配置
// @Summary 删除跟单配置
// @Tags CopyTrade
// @Param trader_id path string true "Trader ID"
// @Success 200 {object} map[string]interface{}
// @Router /api/copytrade/config/{trader_id} [delete]
func (h *CopyTradeHandler) DeleteConfig(c *gin.Context) {
	traderID := c.Param("trader_id")
	live, err := h.store.CopyTrade().HasLiveSourceState(traderID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify active copy-trade state"})
		return
	}
	if live {
		c.JSON(http.StatusConflict, gin.H{"error": "当前仍有活动跟单映射、持仓保护或 Copy Guard 周期，不能删除配置；请先安全清理旧来源"})
		return
	}

	// 先停止跟单
	if copytrade.IsCopyTradingRunning(traderID) {
		copytrade.StopCopyTradingForTrader(traderID)
	}

	// 删除配置
	if err := h.store.CopyTrade().Delete(traderID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete config"})
		return
	}

	// 恢复为 AI 模式
	h.store.CopyTrade().UpdateDecisionMode(traderID, "ai")

	c.JSON(http.StatusOK, gin.H{"message": "config deleted"})
}

// Start 启动跟单
// @Summary 启动跟单
// @Tags CopyTrade
// @Param trader_id path string true "Trader ID"
// @Success 200 {object} map[string]interface{}
// @Router /api/copytrade/start/{trader_id} [post]
func (h *CopyTradeHandler) Start(c *gin.Context) {
	traderID := c.Param("trader_id")

	// 检查是否已在运行
	if copytrade.IsCopyTradingRunning(traderID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "copy trading already running"})
		return
	}

	// 获取 AutoTrader
	autoTrader, err := h.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "trader not found"})
		return
	}

	// 启动跟单
	if err := copytrade.StartCopyTradingForTrader(traderID, autoTrader, h.store); err != nil {
		logger.Errorf("Failed to start copy trading: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 更新配置状态
	h.store.CopyTrade().SetEnabled(traderID, true)
	h.store.CopyTrade().UpdateDecisionMode(traderID, "copy_trade")

	c.JSON(http.StatusOK, gin.H{
		"message": "copy trading started",
		"status":  "running",
	})
}

// Stop 停止跟单
// @Summary 停止跟单
// @Tags CopyTrade
// @Param trader_id path string true "Trader ID"
// @Success 200 {object} map[string]interface{}
// @Router /api/copytrade/stop/{trader_id} [post]
func (h *CopyTradeHandler) Stop(c *gin.Context) {
	traderID := c.Param("trader_id")

	// 检查是否在运行
	if !copytrade.IsCopyTradingRunning(traderID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "copy trading not running"})
		return
	}

	// 停止跟单
	if err := copytrade.StopCopyTradingForTrader(traderID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 更新配置状态
	h.store.CopyTrade().SetEnabled(traderID, false)
	h.store.CopyTrade().UpdateDecisionMode(traderID, "ai")

	c.JSON(http.StatusOK, gin.H{
		"message": "copy trading stopped",
		"status":  "stopped",
	})
}

// GetStats 获取跟单统计
// @Summary 获取跟单统计
// @Tags CopyTrade
// @Param trader_id path string true "Trader ID"
// @Success 200 {object} copytrade.EngineStats
// @Router /api/copytrade/stats/{trader_id} [get]
func (h *CopyTradeHandler) GetStats(c *gin.Context) {
	traderID := c.Param("trader_id")

	stats := copytrade.GetCopyTradingStats(traderID)
	if stats == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no stats available"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"stats":   stats,
		"running": copytrade.IsCopyTradingRunning(traderID),
	})
}

// GetLogs 获取跟单日志
// @Summary 获取跟单日志
// @Tags CopyTrade
// @Param trader_id path string true "Trader ID"
// @Param limit query int false "Limit" default(50)
// @Success 200 {array} store.CopyTradeSignalLog
// @Router /api/copytrade/logs/{trader_id} [get]
func (h *CopyTradeHandler) GetLogs(c *gin.Context) {
	traderID := c.Param("trader_id")
	limit := 50 // 默认值

	if l := c.Query("limit"); l != "" {
		// 简单转换
		if parsed, ok := parseInt(l); ok {
			limit = parsed
		}
	}

	logs, err := h.store.CopyTrade().GetRecentSignalLogs(traderID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get logs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"logs":  logs,
		"count": len(logs),
	})
}

// parseInt 简单整数解析
func parseInt(s string) (int, bool) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// ============================================================================
// Binance 全局凭证管理（v2）
// ============================================================================

// BinanceCredentialsView 凭证列表项响应（已脱敏）
type BinanceCredentialsView struct {
	Label           string    `json:"label"`
	BinanceUserID   string    `json:"binance_user_id"`
	MaskedP20T      string    `json:"masked_p20t"`
	MaskedCSRFToken string    `json:"masked_csrf_token"`
	LastValidatedAt time.Time `json:"last_validated_at"`
	LastStatus      string    `json:"last_status"`
	LastError       string    `json:"last_error"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func toBinanceCredentialsView(c *store.BinanceCredentials) *BinanceCredentialsView {
	if c == nil {
		return nil
	}
	return &BinanceCredentialsView{
		Label:           c.Label,
		BinanceUserID:   c.BinanceUserID,
		MaskedP20T:      c.MaskedP20T(),
		MaskedCSRFToken: c.MaskedCSRFToken(),
		LastValidatedAt: c.LastValidatedAt,
		LastStatus:      c.LastStatus,
		LastError:       c.LastError,
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
	}
}

// ListBinanceCredentials 列出所有 label 的凭证（脱敏）
// @Router /api/copytrade/binance-credentials [get]
func (h *CopyTradeHandler) ListBinanceCredentials(c *gin.Context) {
	creds, err := h.store.BinanceCreds().List()
	if err != nil {
		logger.Errorf("Failed to list binance credentials: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list credentials"})
		return
	}
	views := make([]*BinanceCredentialsView, 0, len(creds))
	for _, item := range creds {
		views = append(views, toBinanceCredentialsView(item))
	}
	c.JSON(http.StatusOK, gin.H{
		"credentials": views,
		"count":       len(views),
	})
}

// BinanceCredentialsSetRequest 设置凭证请求
//
// 支持两种填法（任选其一）：
//  1. 直接填 p20t / csrftoken
//  2. 粘贴完整 cURL 字符串到 curl 字段，由后端自动提取
//
// label 默认 "default"（v1 单账号）。
type BinanceCredentialsSetRequest struct {
	Label     string `json:"label"`
	P20T      string `json:"p20t"`
	CSRFToken string `json:"csrftoken"`
	Curl      string `json:"curl"`
}

// SetBinanceCredentials 保存或更新凭证（保存后立即探活并回写状态）
// @Router /api/copytrade/binance-credentials [post]
func (h *CopyTradeHandler) SetBinanceCredentials(c *gin.Context) {
	var req BinanceCredentialsSetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	p20t := strings.TrimSpace(req.P20T)
	csrf := strings.TrimSpace(req.CSRFToken)

	// 自动从 cURL 提取（用户从浏览器开发者工具复制 cURL 时方便）
	if (p20t == "" || csrf == "") && strings.TrimSpace(req.Curl) != "" {
		extP20T, extCSRF := extractBinanceCredentialsFromCurl(req.Curl)
		if p20t == "" {
			p20t = extP20T
		}
		if csrf == "" {
			csrf = extCSRF
		}
	}

	if p20t == "" || csrf == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "p20t 与 csrftoken 不能为空（可直接填字段或粘贴完整 cURL）",
		})
		return
	}

	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = store.BinanceCredsLabelDefault
	}

	if err := h.store.BinanceCreds().Set(label, p20t, csrf); err != nil {
		logger.Errorf("Failed to save binance credentials: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save credentials"})
		return
	}

	// 保存后立即探活并写状态（同步执行，给前端即时反馈）
	status, errMsg, userID := h.validateBinanceCredsByLabel(label)
	if err := h.store.BinanceCreds().UpdateStatus(label, status, errMsg, userID); err != nil {
		logger.Warnf("Failed to update credentials status: %v", err)
	}

	creds, _ := h.store.BinanceCreds().Get(label)
	c.JSON(http.StatusOK, gin.H{
		"message":     "credentials saved",
		"credentials": toBinanceCredentialsView(creds),
	})
}

// TestBinanceCredentials 测试当前 label 凭证（不写入新值，仅探活）
// @Router /api/copytrade/binance-credentials/test [post]
func (h *CopyTradeHandler) TestBinanceCredentials(c *gin.Context) {
	label := strings.TrimSpace(c.Query("label"))
	if label == "" {
		label = store.BinanceCredsLabelDefault
	}

	status, errMsg, userID := h.validateBinanceCredsByLabel(label)
	if err := h.store.BinanceCreds().UpdateStatus(label, status, errMsg, userID); err != nil {
		logger.Warnf("Failed to update credentials status: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"label":           label,
		"status":          status,
		"binance_user_id": userID,
		"error":           errMsg,
	})
}

// DeleteBinanceCredentials 删除凭证
// @Router /api/copytrade/binance-credentials/{label} [delete]
func (h *CopyTradeHandler) DeleteBinanceCredentials(c *gin.Context) {
	label := strings.TrimSpace(c.Param("label"))
	if label == "" {
		label = store.BinanceCredsLabelDefault
	}
	if err := h.store.BinanceCreds().Delete(label); err != nil {
		logger.Errorf("Failed to delete binance credentials: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted", "label": label})
}

// ListBinanceCredentialsAffectedTraders 列出哪些 trader 在用 Binance 凭证
// （前端展示"该凭证影响 N 个交易员"用）
// @Router /api/copytrade/binance-credentials/affected [get]
func (h *CopyTradeHandler) ListBinanceCredentialsAffectedTraders(c *gin.Context) {
	ids, err := h.store.BinanceCreds().CountBinanceCopyTraderIDs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"trader_ids": ids,
		"count":      len(ids),
	})
}

// validateBinanceCredsByLabel 临时构造一个 Provider 调用 ValidateCredentials 探活
//
// 注意：临时实例与运行中的 Provider 不共享缓存，仅用于一次性探活。
// 不会影响在跑的 trader（它们仍按自己 Provider 的缓存运行）。
func (h *CopyTradeHandler) validateBinanceCredsByLabel(label string) (status, errMsg, userID string) {
	creds, err := h.store.BinanceCreds().Get(label)
	if err != nil {
		return store.BinanceCredsStatusError, err.Error(), ""
	}
	if creds == nil || strings.TrimSpace(creds.P20T) == "" || strings.TrimSpace(creds.CSRFToken) == "" {
		return store.BinanceCredsStatusUnknown, "credentials not configured", ""
	}

	// 用临时 BinanceProvider（带本地凭证）调用 ValidateCredentials
	probe := copytrade.NewBinanceProvider(creds.P20T, creds.CSRFToken)
	if verr := probe.ValidateCredentials(); verr != nil {
		if errors.Is(verr, copytrade.ErrBinanceCredentialsExpired) {
			return store.BinanceCredsStatusExpired, verr.Error(), ""
		}
		return store.BinanceCredsStatusError, verr.Error(), ""
	}

	// 探活成功：再调一次 get-user-base-info 拿 userId（用于显示绑定的币安账号）
	uid := probe.FetchedBinanceUserID()
	return store.BinanceCredsStatusValid, "", uid
}

// extractBinanceCredentialsFromCurl 从浏览器复制的 cURL 文本中提取 p20t / csrftoken
//
// 容忍多种格式：
//   - -H 'csrftoken: xxx'
//   - --header 'csrftoken: xxx'
//   - cookie: ...; p20t=xxx; ...
//   - -b 'p20t=xxx'
//
// 提取失败时返回空字符串，由调用方报错。
func extractBinanceCredentialsFromCurl(curl string) (p20t, csrfToken string) {
	// csrftoken 匹配（header 形式）
	csrfRe := regexp.MustCompile(`(?i)csrftoken['":\s]*([0-9a-f]{16,})`)
	if m := csrfRe.FindStringSubmatch(curl); len(m) >= 2 {
		csrfToken = m[1]
	}

	// p20t 匹配（cookie 形式）
	p20tRe := regexp.MustCompile(`(?i)p20t=([^\s;'"]+)`)
	if m := p20tRe.FindStringSubmatch(curl); len(m) >= 2 {
		p20t = m[1]
	}
	return p20t, csrfToken
}
