package reentryadvisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"nofx/copytrade"
	"nofx/logger"
	"nofx/mcp"
	"nofx/notifier"
	"nofx/store"
)

// ============================================================================
// 内置 AI 分析与 ai_guarded 候选决策
//
// 对已落库的分析快照（reentry_ai_analyses）调用内置 LLM，得到与外部 AI 同源
// 同 Prompt 的结论（verdict/confidence/reasons），写回同一条记录，供前端
// 展示与内外对比。密钥复用 ai_models 表（与自动交易/辩论模块同一套配置）。
//
// 失败时保留分析审计行并按 5/15/60 分钟退避，不回退硬规则下单。
// ============================================================================

// aiVerdict 模型输出的严格 JSON 结论（schema 见 buildSystemPrompt 输出格式段）
type aiVerdict struct {
	Decision           string   `json:"decision"`
	Regime             string   `json:"regime"`
	Confidence         float64  `json:"confidence"`
	SuggestedNotional  float64  `json:"suggested_notional"`
	SizeFactor         float64  `json:"size_factor"`
	EntryPriceLow      float64  `json:"entry_price_low"`
	EntryPriceHigh     float64  `json:"entry_price_high"`
	AttentionPriceLow  float64  `json:"attention_price_low"`
	AttentionPriceHigh float64  `json:"attention_price_high"`
	TTLSeconds         int      `json:"ttl_seconds"`
	NextReviewSeconds  int      `json:"next_review_seconds"`
	Reasons            []string `json:"reasons"`
	RiskNotes          []string `json:"risk_notes"`
}

// parsedVerdict 解析归一化后的结论
type parsedVerdict struct {
	Verdict                               string
	Confidence                            float64
	SuggestedNotional                     float64 // 仅 ENTER 且模型给出有效值时 >0
	ReasonsJSON                           string  // reasons + risk_notes（+建议金额）的 JSON，前端整体展示
	Regime                                string
	SizeFactor                            float64
	EntryPriceLow, EntryPriceHigh         float64
	AttentionPriceLow, AttentionPriceHigh float64
	TTLSeconds, NextReviewSeconds         int
}

func parseAICandidateVerdict(raw string) (*parsedVerdict, error) {
	obj, ok := extractJSONObject(raw)
	if !ok {
		return nil, fmt.Errorf("回复中未找到 JSON 对象")
	}
	if strings.TrimSpace(raw) != obj {
		return nil, fmt.Errorf("候选回复必须只包含一个 JSON 对象")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(obj), &keys); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	allowedKeys := map[string]struct{}{}
	for _, key := range []string{"decision", "regime", "confidence", "size_factor", "entry_price_low", "entry_price_high", "attention_price_low", "attention_price_high", "ttl_seconds", "next_review_seconds", "reasons", "risk_notes"} {
		allowedKeys[key] = struct{}{}
		if _, exists := keys[key]; !exists {
			return nil, fmt.Errorf("缺少必填字段 %s", key)
		}
	}
	for key := range keys {
		if _, allowed := allowedKeys[key]; !allowed {
			return nil, fmt.Errorf("候选回复包含未定义字段 %s", key)
		}
	}
	var v aiVerdict
	decoder := json.NewDecoder(strings.NewReader(obj))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&v); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("JSON 对象后存在额外内容")
	}
	decision := strings.ToUpper(strings.TrimSpace(v.Decision))
	if decision != "ENTER_NOW" && decision != store.ReentryVerdictWait && decision != store.ReentryVerdictAbandon {
		return nil, fmt.Errorf("decision 字段无效: %q", v.Decision)
	}
	regime := strings.ToUpper(strings.TrimSpace(v.Regime))
	switch regime {
	case "FALSE_BREAK", "REVERSAL", "CONTINUATION", "CHOP":
	default:
		return nil, fmt.Errorf("regime 字段无效: %q", v.Regime)
	}
	if v.Confidence < 0 || v.Confidence > 1 {
		return nil, fmt.Errorf("confidence 越界")
	}
	if decision == "ENTER_NOW" {
		if v.SizeFactor <= 0 || v.SizeFactor > 1 {
			return nil, fmt.Errorf("ENTER_NOW size_factor 必须在 (0,1]")
		}
		if v.EntryPriceLow <= 0 || v.EntryPriceHigh < v.EntryPriceLow {
			return nil, fmt.Errorf("入场价格区间无效")
		}
	} else if v.SizeFactor != 0 {
		return nil, fmt.Errorf("非入场决策 size_factor 必须为 0")
	}
	if v.AttentionPriceLow < 0 || v.AttentionPriceHigh < v.AttentionPriceLow {
		return nil, fmt.Errorf("关注价格区间无效")
	}
	if v.TTLSeconds < 15 || v.TTLSeconds > 60 {
		return nil, fmt.Errorf("ttl_seconds 必须为 15..60")
	}
	if v.NextReviewSeconds < 300 || v.NextReviewSeconds > 21600 {
		return nil, fmt.Errorf("next_review_seconds 必须为 300..21600")
	}
	payload, _ := json.Marshal(map[string]interface{}{"reasons": v.Reasons, "risk_notes": v.RiskNotes, "regime": regime, "size_factor": v.SizeFactor, "entry_price_low": v.EntryPriceLow, "entry_price_high": v.EntryPriceHigh, "attention_price_low": v.AttentionPriceLow, "attention_price_high": v.AttentionPriceHigh, "ttl_seconds": v.TTLSeconds, "next_review_seconds": v.NextReviewSeconds})
	normalized := decision
	if decision == "ENTER_NOW" {
		normalized = store.ReentryVerdictEnter
	}
	return &parsedVerdict{Verdict: normalized, Confidence: v.Confidence, ReasonsJSON: string(payload), Regime: regime, SizeFactor: v.SizeFactor, EntryPriceLow: v.EntryPriceLow, EntryPriceHigh: v.EntryPriceHigh, AttentionPriceLow: v.AttentionPriceLow, AttentionPriceHigh: v.AttentionPriceHigh, TTLSeconds: v.TTLSeconds, NextReviewSeconds: v.NextReviewSeconds}, nil
}

// resolveAIModel 解析配置指向的 ai_models 行：
// cfg.Model 非空时按 ID 精确取（须已启用）；为空时优先 default 用户下已启用
// 的 deepseek，退化为任一已启用模型。
func resolveAIModel(st *store.Store, cfg *store.ReentryAIConfig) (*store.AIModel, error) {
	if cfg.Model != "" {
		m, err := st.AIModel().GetByID(cfg.Model)
		if err != nil {
			return nil, fmt.Errorf("配置的模型 %s 不存在: %w", cfg.Model, err)
		}
		if !m.Enabled {
			return nil, fmt.Errorf("配置的模型 %s 未启用", cfg.Model)
		}
		return m, nil
	}
	models, err := st.AIModel().List("default")
	if err != nil {
		return nil, err
	}
	var fallback *store.AIModel
	for _, m := range models {
		if !m.Enabled || m.APIKey == "" {
			continue
		}
		if m.Provider == "deepseek" {
			return m, nil
		}
		if fallback == nil {
			fallback = m
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("没有已启用且配置了密钥的 AI 模型，请先在系统模型配置中添加")
}

// newAIClientForModel provider → mcp 客户端映射（与 debate/api-strategy 同一套口径）
func newAIClientForModel(m *store.AIModel, timeout time.Duration) (mcp.AIClient, error) {
	var client mcp.AIClient
	// Fixed low temperature makes repeated reviews comparable. Disable the
	// transport client's hidden retries so one claimed candidate review always
	// equals exactly one billable API call.
	opts := []mcp.ClientOption{mcp.WithTemperature(0.1), mcp.WithMaxRetries(1)}
	switch m.Provider {
	case "deepseek":
		client = mcp.NewDeepSeekClientWithOptions(opts...)
	case "qwen":
		client = mcp.NewQwenClientWithOptions(opts...)
	case "openai":
		client = mcp.NewOpenAIClientWithOptions(opts...)
	case "claude":
		client = mcp.NewClaudeClientWithOptions(opts...)
	case "gemini":
		client = mcp.NewGeminiClientWithOptions(opts...)
	case "grok":
		client = mcp.NewGrokClientWithOptions(opts...)
	case "kimi":
		client = mcp.NewKimiClientWithOptions(opts...)
	case "custom":
		client = mcp.NewClient(opts...)
	default:
		return nil, fmt.Errorf("不支持的模型 provider: %s", m.Provider)
	}
	if m.APIKey == "" {
		return nil, fmt.Errorf("模型 %s 未配置 API 密钥", m.ID)
	}
	client.SetAPIKey(m.APIKey, m.CustomAPIURL, m.CustomModelName)
	if timeout > 0 {
		client.SetTimeout(timeout)
	}
	return client, nil
}

// extractJSONObject 从模型回复中抽出第一个平衡的顶层 {...}（容忍 ```json 围栏
// 与前后闲聊文本；字符串内的花括号按 JSON 转义规则跳过）。
func extractJSONObject(raw string) (string, bool) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return "", false
	}
	depth, inStr, escaped := 0, false, false
	for i := start; i < len(raw); i++ {
		c := raw[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return raw[start : i+1], true
			}
		}
	}
	return "", false
}

// parseAIVerdict 解析并校验模型回复，返回归一化结论。
func parseAIVerdict(raw string) (*parsedVerdict, error) {
	obj, ok := extractJSONObject(raw)
	if !ok {
		return nil, fmt.Errorf("回复中未找到 JSON 对象")
	}
	var v aiVerdict
	if err := json.Unmarshal([]byte(obj), &v); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	verdict := strings.ToUpper(strings.TrimSpace(v.Decision))
	switch verdict {
	case store.ReentryVerdictEnter, store.ReentryVerdictWait, store.ReentryVerdictSkip:
	default:
		return nil, fmt.Errorf("decision 字段无效: %q", v.Decision)
	}
	confidence := v.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	suggested := float64(0)
	if verdict == store.ReentryVerdictEnter && v.SuggestedNotional > 0 {
		suggested = v.SuggestedNotional
	}
	reasonsPayload := map[string]interface{}{
		"reasons":    v.Reasons,
		"risk_notes": v.RiskNotes,
	}
	if suggested > 0 {
		reasonsPayload["suggested_notional"] = suggested
	}
	b, err := json.Marshal(reasonsPayload)
	if err != nil {
		return nil, err
	}
	return &parsedVerdict{
		Verdict:           verdict,
		Confidence:        confidence,
		SuggestedNotional: suggested,
		ReasonsJSON:       string(b),
	}, nil
}

// markAnalysisRunning / markAnalysisDone 进行中去重（同一条分析记录不并发跑两份）
func (a *Advisor) markAnalysisRunning(id int64) bool {
	a.inflightMu.Lock()
	defer a.inflightMu.Unlock()
	if a.inflight[id] {
		return false
	}
	a.inflight[id] = true
	return true
}

func (a *Advisor) markAnalysisDone(id int64) {
	a.inflightMu.Lock()
	delete(a.inflight, id)
	a.inflightMu.Unlock()
}

// runAnalysis 对一条分析快照执行内置 AI 分析并写回结果。
// candidate_id>0 的记录只允许由持久化候选调度器领取和执行；历史人工信号
// 的自动分析仅补充结论邮件，绝不再调用旧人工确认链路下单。解析失败时，
// 历史分析自动重试一次，候选分析则按一次调用计费并交调度器退避。
func (a *Advisor) runAnalysis(analysisID int64, autoTriggered bool) {
	candidateID := int64(0)
	candidateFinished := false
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("[ReentryAdvisor] AI 分析 panic 已恢复 (analysis=%d): %v", analysisID, r)
			if candidateID > 0 && !candidateFinished {
				a.failCandidateAnalysis(candidateID, analysisID, fmt.Sprintf("AI analysis panic: %v", r), 0)
			}
		}
	}()
	if !a.markAnalysisRunning(analysisID) {
		return
	}
	defer a.markAnalysisDone(analysisID)

	analysis, err := a.st.ReentryAI().GetReentryAnalysis(analysisID)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] AI 分析读取记录失败 (analysis=%d): %v", analysisID, err)
		return
	}
	candidateID = analysis.CandidateID
	cfg, err := a.st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		logger.Warnf("[ReentryAdvisor] AI 分析读取配置失败: %v", err)
		a.failCandidateBeforeModel(candidateID, analysisID, "AI config unavailable: "+err.Error())
		return
	}
	model, err := resolveAIModel(a.st, cfg)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] AI 分析模型解析失败 (analysis=%d): %v", analysisID, err)
		a.failCandidateBeforeModel(candidateID, analysisID, "AI model unavailable: "+err.Error())
		return
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	client, err := newAIClientForModel(model, timeout)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] AI 客户端创建失败 (analysis=%d): %v", analysisID, err)
		a.failCandidateBeforeModel(candidateID, analysisID, "AI client unavailable: "+err.Error())
		return
	}
	if candidateID > 0 {
		if err := a.st.ReentryAI().MarkReentryAnalysisRunning(analysis.ID); err != nil {
			a.failCandidateBeforeModel(candidateID, analysisID, "AI call lease unavailable: "+err.Error())
			return
		}
	}

	logger.Infof("[ReentryAdvisor] 开始内置 AI 分析 (analysis=%d, signal=%d, %s %s, model=%s/%s)",
		analysis.ID, analysis.SignalID, analysis.Symbol, analysis.Side, model.Provider, model.ID)

	var raw string
	var pv *parsedVerdict
	maxAttempts := 2
	if analysis.CandidateID > 0 {
		// Candidate review_count is the API-call budget. Retrying inside one
		// review would make 12 stored reviews consume up to 24 paid calls.
		maxAttempts = 1
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err = client.CallWithMessages(analysis.SystemPrompt, analysis.UserPrompt)
		if err != nil {
			logger.Warnf("[ReentryAdvisor] AI 调用失败 (analysis=%d, 第 %d 次): %v", analysisID, attempt, err)
			continue
		}
		if analysis.CandidateID > 0 {
			pv, err = parseAICandidateVerdict(raw)
		} else {
			pv, err = parseAIVerdict(raw)
		}
		if err == nil {
			break
		}
		logger.Warnf("[ReentryAdvisor] AI 回复解析失败 (analysis=%d, 第 %d 次): %v", analysisID, attempt, err)
	}
	if raw == "" {
		// 两次调用都没拿到回复，无可落库内容
		if analysis.CandidateID > 0 {
			_ = a.st.ReentryAI().MarkReentryAnalysisFailed(analysis.ID, "AI call failed")
			if c, e := a.st.ReentryAI().GetReentryCandidate(analysis.CandidateID); e == nil {
				_ = a.st.ReentryAI().FailReentryCandidateReview(c.ID, "AI call failed", candidateFailureBackoff(c.FailureCount+1))
				a.updateCandidateCycleStatus(c, store.CopyGuardAIWaiting)
				a.recordCandidateEvent(c, "AI_REVIEW_FAILED", 0, 0, map[string]interface{}{"reason": "ai_call_failed"})
				if c.FailureCount+1 == 3 {
					a.notifyCandidateImportant(c, "AI_REVIEW_FAILED", "AI 连续调用失败", "模型连续失败，候选将按 60 分钟退避继续观察。")
				}
			}
		}
		return
	}
	if pv == nil {
		pv = &parsedVerdict{} // 解析失败：verdict 留空，仅存 raw
	}
	if saveErr := a.st.ReentryAI().CompleteReentryInternalResult(analysis.ID, raw, pv.Verdict, pv.Confidence, pv.ReasonsJSON, pv.TTLSeconds); saveErr != nil {
		logger.Warnf("[ReentryAdvisor] AI 结果落库失败 (analysis=%d): %v", analysisID, saveErr)
		a.failCandidateAnalysis(candidateID, analysisID, "AI result persistence failed: "+saveErr.Error(), 0)
		return
	}
	if pv.Verdict == "" {
		logger.Warnf("[ReentryAdvisor] AI 分析完成但结论不可解析，已存原始回复 (analysis=%d)", analysisID)
		if analysis.CandidateID > 0 {
			if c, e := a.st.ReentryAI().GetReentryCandidate(analysis.CandidateID); e == nil {
				_ = a.st.ReentryAI().FailReentryCandidateReview(c.ID, "invalid AI response", candidateFailureBackoff(c.FailureCount+1))
				a.updateCandidateCycleStatus(c, store.CopyGuardAIWaiting)
				a.recordCandidateEvent(c, "AI_REVIEW_FAILED", 0, 0, map[string]interface{}{"reason": "invalid_response", "analysis_id": analysis.ID})
				if c.FailureCount+1 == 3 {
					a.notifyCandidateImportant(c, "AI_REVIEW_FAILED", "AI 连续返回无效结果", "模型输出连续不符合严格 JSON 契约，候选将退避后复查。")
				}
			}
		}
		return
	}
	logger.Infof("[ReentryAdvisor] 内置 AI 结论: %s (confidence=%.2f) (analysis=%d, signal=%d, %s %s)",
		pv.Verdict, pv.Confidence, analysis.ID, analysis.SignalID, analysis.Symbol, analysis.Side)

	// Seam C：把 AI 二次入场分析结论记入统一跟单事件日志（best-effort），
	// 用于追踪 AI 接收/介入的执行情况。
	a.recordAIAnalysisEvent(analysis, pv, autoTriggered)
	if analysis.CandidateID > 0 {
		if err := a.finishCandidateAnalysis(analysis, cfg, pv); err != nil {
			a.failCandidateAnalysis(analysis.CandidateID, analysis.ID, err.Error(), 0)
			return
		}
		candidateFinished = true
		return
	}

	// 历史人工信号只保留分析和审计，不再具有任何执行能力。真实 AI 重入
	// 只能经过上面的 candidate_id 路径和 ExecuteAIReentryForTrader。
	if autoTriggered {
		a.notifyVerdict(analysis, cfg, pv, legacyAnalysisExecutionNote(cfg, pv))
	}
}

func (a *Advisor) finishCandidateAnalysis(analysis *store.ReentryAIAnalysis, cfg *store.ReentryAIConfig, pv *parsedVerdict) error {
	c, err := a.st.ReentryAI().GetReentryCandidate(analysis.CandidateID)
	if err != nil {
		return fmt.Errorf("candidate unavailable after analysis: %w", err)
	}
	traderCfg, err := a.st.CopyTrade().GetByTraderID(c.TraderID)
	if err != nil {
		return fmt.Errorf("trader risk policy unavailable after analysis: %w", err)
	}
	abandonThreshold := math.Max(0.80, traderCfg.RiskAIConfidenceThreshold)
	nextSeconds := pv.NextReviewSeconds
	if pv.Verdict == store.ReentryVerdictWait || pv.Verdict == store.ReentryVerdictAbandon {
		// Paid heartbeat is owned by deterministic code: 15m → 30m → 60m →
		// 2h. Market/leader/attention-zone events may still pull the review
		// forward, subject to the 5m minimum interval and feature-hash dedupe.
		switch {
		case c.ReviewCount <= 1:
			nextSeconds = 15 * 60
		case c.ReviewCount == 2:
			nextSeconds = 30 * 60
		case c.ReviewCount == 3:
			nextSeconds = 60 * 60
		default:
			nextSeconds = 2 * 60 * 60
		}
	}
	next := time.Now().Add(time.Duration(nextSeconds) * time.Second)
	candle := candidateClosed5mCandleKey(analysis)
	enterApproved := pv.Verdict == store.ReentryVerdictEnter && pv.Confidence >= traderCfg.RiskAIConfidenceThreshold
	d := store.ReentryCandidateDecision{Decision: pv.Verdict, Regime: pv.Regime, Confidence: pv.Confidence, SizeFactor: pv.SizeFactor, EntryPriceLow: pv.EntryPriceLow, EntryPriceHigh: pv.EntryPriceHigh, AttentionPriceLow: pv.AttentionPriceLow, AttentionPriceHigh: pv.AttentionPriceHigh, NextReview: next, AnalysisID: analysis.ID, TTLSeconds: pv.TTLSeconds, CandleKey: candle, ConfirmAbandon: pv.Verdict == store.ReentryVerdictAbandon && pv.Confidence >= abandonThreshold && candle != "", EnterApproved: enterApproved}
	if err := a.st.ReentryAI().FinishReentryCandidateReview(c.ID, d); err != nil {
		return err
	}
	event := "AI_REVIEW_WAIT"
	if pv.Verdict == store.ReentryVerdictEnter {
		event = "AI_REVIEW_ENTER"
	}
	if pv.Verdict == store.ReentryVerdictAbandon {
		event = "AI_REVIEW_ABANDON"
	}
	a.recordCandidateEvent(c, event, c.TriggerPrice, c.MaxNotional*pv.SizeFactor, map[string]interface{}{"analysis_id": analysis.ID, "data_hash": analysis.DataHash, "model": cfg.Model, "prompt_version": analysis.PromptVersion, "confidence": pv.Confidence, "regime": pv.Regime, "size_factor": pv.SizeFactor, "entry_price_low": pv.EntryPriceLow, "entry_price_high": pv.EntryPriceHigh, "next_review_at": next, "ai_next_review_seconds": pv.NextReviewSeconds, "scheduled_next_review_seconds": nextSeconds})
	if pv.Verdict == store.ReentryVerdictWait {
		_ = a.st.CopyTrade().UpdateCopyGuardObservation(c.CycleID, store.CopyGuardAIWaiting, c.LeaderEntryPrice, c.TriggerPrice, c.ATR)
		return nil
	}
	if pv.Verdict == store.ReentryVerdictAbandon {
		fresh, _ := a.st.ReentryAI().GetReentryCandidate(c.ID)
		if fresh != nil && fresh.ConsecutiveAbandons >= 2 && pv.Confidence >= abandonThreshold {
			_ = a.st.ReentryAI().MarkReentryCandidateStatus(c.ID, store.ReentryCandidateAbandoned, "two confirmed ABANDON decisions")
			_ = a.st.CopyTrade().UpdateCopyGuardObservation(c.CycleID, store.CopyGuardAIAbandoned, c.LeaderEntryPrice, c.TriggerPrice, c.ATR)
			a.notifyCandidateImportant(fresh, "AI_REVIEW_ABANDON", "AI 已确认放弃重入候选", "两个不同的已收盘 5m K 线均给出高置信度 ABANDON，候选已结束。")
		} else {
			a.updateCandidateCycleStatus(c, store.CopyGuardAIWaiting)
		}
		return nil
	}
	if pv.Confidence < traderCfg.RiskAIConfidenceThreshold {
		a.recordCandidateEvent(c, "REENTRY_PREFLIGHT_REJECTED", c.TriggerPrice, 0, map[string]interface{}{"analysis_id": analysis.ID, "reason_code": "AI_CONFIDENCE_LOW", "reason": "confidence_below_threshold", "confidence": pv.Confidence, "required_confidence": traderCfg.RiskAIConfidenceThreshold})
		a.updateCandidateCycleStatus(c, store.CopyGuardAIWaiting)
		return nil
	}
	if !cfg.AutoEntryEnabled {
		reviewAfter := time.Duration(traderCfg.RiskAIMinReviewSeconds) * time.Second
		if reviewAfter <= 0 {
			reviewAfter = 5 * time.Minute
		}
		_ = a.st.ReentryAI().RejectReentryCandidatePreflight(c.ID, "global AI execution disabled", reviewAfter)
		a.updateCandidateCycleStatus(c, store.CopyGuardAIWaiting)
		a.recordCandidateEvent(c, "ENTER_APPROVED_EXECUTION_DISABLED", c.TriggerPrice, 0, map[string]interface{}{"analysis_id": analysis.ID, "reason_code": "EXECUTION_DISABLED", "reason": "global AI execution disabled"})
		return nil
	}
	if err := copytrade.ExecuteAIReentryForTrader(c.TraderID, c.ID, analysis.ID); err != nil {
		logger.Warnf("[ReentryAdvisor] AI 重入预检拒绝 candidate=%d: %v", c.ID, err)
		if copytrade.ReasonCodeOf(err) == "PRICE_OUT_OF_RANGE" {
			a.updateCandidateCycleStatus(c, store.CopyGuardAIWaiting)
			a.recordCandidateEvent(c, "ENTER_LEASE_WAITING_PRICE", c.TriggerPrice, 0, map[string]interface{}{"analysis_id": analysis.ID, "reason_code": "PRICE_OUT_OF_RANGE", "error": err.Error(), "ttl_seconds": d.TTLSeconds})
			return nil
		}
		if errors.Is(err, copytrade.ErrAIReentryAlreadyReserved) {
			a.recordCandidateEvent(c, "REENTRY_RECONCILIATION_PENDING", c.TriggerPrice, 0, map[string]interface{}{"analysis_id": analysis.ID, "reason_code": "INTENT_ALREADY_RESERVED", "error": err.Error()})
			return nil
		}
		_ = a.st.ReentryAI().RejectReentryCandidatePreflight(c.ID, err.Error(), time.Duration(traderCfg.RiskAIMinReviewSeconds)*time.Second)
		a.updateCandidateCycleStatus(c, store.CopyGuardAIWaiting)
		a.recordCandidateEvent(c, "REENTRY_PREFLIGHT_REJECTED", c.TriggerPrice, 0, map[string]interface{}{"analysis_id": analysis.ID, "reason_code": classifyAIReentryPreflightError(err), "error": err.Error()})
	}
	return nil
}

func classifyAIReentryPreflightError(err error) string {
	if err == nil {
		return ""
	}
	if code := copytrade.ReasonCodeOf(err); code != "" {
		return code
	}
	message := strings.ToLower(err.Error())
	checks := []struct {
		code   string
		tokens []string
	}{
		{"SNAPSHOT_STALE", []string{"过期", "expired", "stale"}},
		{"PRICE_OUT_OF_RANGE", []string{"入场区间", "approved range", "0.25 atr", "漂移"}},
		{"MIN_NOTIONAL", []string{"最小下单额", "minimum notional", "below minimum"}},
		{"RISK_CAP", []string{"风险预算", "risk budget", "risk cap", "exceeds ai deterministic cap"}},
		{"POSITION_EXISTS", []string{"已有同向仓位", "already has", "position exists"}},
		{"PROTECTION_UNAVAILABLE", []string{"保护止损", "保护计划", "protection"}},
		{"LEADER_CLOSED", []string{"领航员已平仓", "leader position closed", "reversed"}},
		{"CYCLE_STATE_CHANGED", []string{"周期已结束", "状态不允许", "尝试次数已过期", "cycle"}},
		{"EXECUTION_BUSY", []string{"执行通道繁忙", "channel"}},
	}
	for _, check := range checks {
		for _, token := range check.tokens {
			if strings.Contains(message, token) {
				return check.code
			}
		}
	}
	return "PREFLIGHT_REJECTED"
}

// candidateClosed5mCandleKey returns the actual last completed 5m candle from
// the persisted point-in-time datapack. Never fall back to analysis wall time:
// two reviews in different clock buckets over the same closed candle must not
// satisfy the two-candle ABANDON rule.
func candidateClosed5mCandleKey(analysis *store.ReentryAIAnalysis) string {
	if analysis == nil || analysis.DatapackJSON == "" {
		return ""
	}
	var pack struct {
		Market *struct {
			Klines map[string]*KlineSummary `json:"klines"`
		} `json:"market"`
	}
	if err := json.Unmarshal([]byte(analysis.DatapackJSON), &pack); err != nil || pack.Market == nil {
		return ""
	}
	summary := pack.Market.Klines["5m"]
	if summary == nil || len(summary.Bars) == 0 {
		return ""
	}
	bar := summary.Bars[len(summary.Bars)-1]
	if len(bar) == 0 || bar[0] <= 0 {
		return ""
	}
	return time.UnixMilli(int64(bar[0])).UTC().Format(time.RFC3339)
}

// failCandidateAnalysis closes every pre-decision failure path. The store
// condition prevents a late model result from resurrecting an operator-paused
// or terminated candidate.
func (a *Advisor) failCandidateAnalysis(candidateID, analysisID int64, message string, failureCount int) {
	if candidateID <= 0 {
		return
	}
	c, err := a.st.ReentryAI().GetReentryCandidate(candidateID)
	if err != nil || (c.Status != store.ReentryCandidateReviewing && c.Status != store.ReentryCandidateEntryPending) {
		return
	}
	if analysisID > 0 {
		_ = a.st.ReentryAI().MarkReentryAnalysisFailed(analysisID, message)
	}
	if failureCount <= 0 {
		failureCount = c.FailureCount + 1
	}
	if err := a.st.ReentryAI().FailReentryCandidateReview(c.ID, message, candidateFailureBackoff(failureCount)); err != nil {
		return
	}
	a.updateCandidateCycleStatus(c, store.CopyGuardAIWaiting)
	a.recordCandidateEvent(c, "AI_REVIEW_FAILED", 0, 0, map[string]interface{}{"reason": message, "failure_count": failureCount})
}

func (a *Advisor) failCandidateBeforeModel(candidateID, analysisID int64, message string) {
	if candidateID <= 0 {
		return
	}
	c, err := a.st.ReentryAI().GetReentryCandidate(candidateID)
	if err != nil || c.Status != store.ReentryCandidateReviewing {
		return
	}
	failureCount := c.FailureCount + 1
	if err := a.st.ReentryAI().FailReentryCandidateBeforeModel(c.ID, analysisID, message, candidateFailureBackoff(failureCount)); err != nil {
		return
	}
	a.updateCandidateCycleStatus(c, store.CopyGuardAIWaiting)
	a.recordCandidateEvent(c, "AI_REVIEW_FAILED", 0, 0, map[string]interface{}{"reason": message, "failure_count": failureCount, "before_model_call": true})
	if failureCount == 3 {
		a.notifyCandidateImportant(c, "AI_REVIEW_FAILED", "AI 重入分析连续准备失败", "模型尚未被调用，候选不会消耗调用额度，并将按退避策略继续等待。")
	}
}

func (a *Advisor) notifyCandidateImportant(c *store.CopyGuardReentryCandidate, eventType, title, body string) {
	if c == nil {
		return
	}
	traderCfg, err := a.st.CopyTrade().GetByTraderID(c.TraderID)
	if err == nil && !store.ShouldSendCopyGuardEmail(traderCfg.RiskNotificationLevel, eventType) {
		return
	}
	attemptNo := c.ReentryCount + 1
	key := fmt.Sprintf("%s|%s|%d|%d|%d", eventType, c.TraderID, c.CycleID, attemptNo, c.DecisionGeneration)
	traderName := a.st.Trader().ResolveDisplayName(c.TraderID)
	notifier.Notify(notifier.Alert{
		Category: "copy_trade", TraderID: c.TraderID, TraderName: traderName,
		Title:   fmt.Sprintf("%s | %s | %s %s", traderName, title, c.Symbol, strings.ToUpper(c.Side)),
		Body:    fmt.Sprintf("%s\n\nTrader Name: %s\nTrader ID: %s\nCycle: %d\nAttempt: %d\nCandidate: %d\nGeneration: %d\nLast decision: %s\nConfidence: %.0f%%\n\n本次决策效果将在后续行情与周期闭合后由确定性评价器回填。", body, traderName, c.TraderID, c.CycleID, attemptNo, c.ID, c.DecisionGeneration, c.LastDecision, c.Confidence*100),
		RateKey: key, DedupKey: key,
		Fields: map[string]string{"TraderName": traderName, "CycleID": fmt.Sprint(c.CycleID), "AttemptNo": fmt.Sprint(attemptNo), "CandidateID": fmt.Sprint(c.ID), "Generation": fmt.Sprint(c.DecisionGeneration)},
	})
}

// recordAIAnalysisEvent 把一次内置 AI 二次入场分析结论落成统一跟单事件（best-effort）。
// provider 从交易员跟单配置解析，避免非 OKX 账户在统一日志中被错误归类。
func (a *Advisor) recordAIAnalysisEvent(analysis *store.ReentryAIAnalysis, pv *parsedVerdict, autoTriggered bool) {
	if a.st == nil || analysis == nil || pv == nil {
		return
	}
	trigger := "manual"
	if autoTriggered {
		trigger = "auto"
	}
	providerType := ""
	if cfg, err := a.st.CopyTrade().GetByTraderID(analysis.TraderID); err == nil {
		providerType = cfg.ProviderType
	}
	traderName := a.st.Trader().ResolveDisplayName(analysis.TraderID)
	summary := fmt.Sprintf("AI 二次入场分析结论: %s (置信度 %.0f%%)", pv.Verdict, pv.Confidence*100)
	ev := &store.CopyTradeEvent{
		TraderID:     analysis.TraderID,
		ProviderType: providerType,
		Category:     store.CopyEventCategoryTakeover,
		EventType:    "AI_ANALYSIS",
		Severity:     store.CopyEventSeverityInfo,
		Symbol:       analysis.Symbol,
		Side:         analysis.Side,
		CycleID:      analysis.CycleID,
		Operator:     "ai",
		Summary:      summary,
		Detail: map[string]interface{}{
			"verdict":              pv.Verdict,
			"confidence":           pv.Confidence,
			"regime":               pv.Regime,
			"size_factor":          pv.SizeFactor,
			"entry_price_low":      pv.EntryPriceLow,
			"entry_price_high":     pv.EntryPriceHigh,
			"attention_price_low":  pv.AttentionPriceLow,
			"attention_price_high": pv.AttentionPriceHigh,
			"ttl_seconds":          pv.TTLSeconds,
			"next_review_seconds":  pv.NextReviewSeconds,
			"reasons_json":         pv.ReasonsJSON,
			"trigger":              trigger,
			"analysis_id":          analysis.ID,
			"candidate_id":         analysis.CandidateID,
			"signal_id":            analysis.SignalID,
			"attempt_no":           analysis.AttemptNo,
			"decision_generation":  analysis.DecisionGeneration,
			"data_hash":            analysis.DataHash,
			"prompt_version":       analysis.PromptVersion,
			"snapshot_at":          analysis.SnapshotAt,
			"trader_name_snapshot": traderName,
		},
	}
	if err := a.st.CopyTrade().LogCopyEvent(ev); err != nil {
		logger.Warnf("[ReentryAdvisor] AI 分析事件落库失败 (analysis=%d): %v", analysis.ID, err)
	}
}

// legacyAnalysisExecutionNote 明确封死旧人工信号的 AI 自动确认旁路。
// auto_entry_enabled 现在仅代表 ai_guarded 候选执行的全局安全开关；即使
// 历史信号分析得到 ENTER，也只能用于审计，不能下单。
func legacyAnalysisExecutionNote(cfg *store.ReentryAIConfig, pv *parsedVerdict) string {
	if cfg.AutoEntryEnabled && pv.Verdict == store.ReentryVerdictEnter {
		return "未执行：旧人工重入执行链已废弃；本结论仅用于历史审计。真实重入必须由持久化 AI 候选调度器重新预检后执行。"
	}
	return ""
}

// notifyVerdict AI 结论邮件（自动分析路径）。信号本身的告警邮件由 copytrade
// 即时发出；这封是补充结论（含自动入场执行说明），限流键独立，notifier 未
// 启用时零副作用。
func (a *Advisor) notifyVerdict(analysis *store.ReentryAIAnalysis, cfg *store.ReentryAIConfig, pv *parsedVerdict, autoEntryNote string) {
	verdictText := map[string]string{
		store.ReentryVerdictEnter: "ENTER（建议确认重入）",
		store.ReentryVerdictWait:  "WAIT（建议继续观察）",
		store.ReentryVerdictSkip:  "SKIP（建议忽略本信号）",
	}[pv.Verdict]
	var payload struct {
		Reasons   []string `json:"reasons"`
		RiskNotes []string `json:"risk_notes"`
	}
	_ = json.Unmarshal([]byte(pv.ReasonsJSON), &payload)
	var b strings.Builder
	fmt.Fprintf(&b, "人工重入信号 #%d（%s %s）的内置 AI 分析已完成。\n\n", analysis.SignalID, analysis.Symbol, strings.ToUpper(analysis.Side))
	fmt.Fprintf(&b, "结论: %s\n置信度: %.0f%%\n", verdictText, pv.Confidence*100)
	if pv.SuggestedNotional > 0 {
		fmt.Fprintf(&b, "建议金额: %.2f USDT\n", pv.SuggestedNotional)
	}
	if len(payload.Reasons) > 0 {
		b.WriteString("\n主要依据:\n")
		for _, r := range payload.Reasons {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}
	if len(payload.RiskNotes) > 0 {
		b.WriteString("\n风险提示:\n")
		for _, r := range payload.RiskNotes {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}
	switch {
	case autoEntryNote != "":
		fmt.Fprintf(&b, "\n执行状态: %s\n", autoEntryNote)
	default:
		b.WriteString("\n本条为旧信号的历史分析，不具备下单能力；真实重入仅由 ai_guarded 候选调度器执行。")
	}
	traderName := a.st.Trader().ResolveDisplayName(analysis.TraderID)
	notifier.Notify(notifier.Alert{
		Category:   "copy_trade",
		TraderID:   analysis.TraderID,
		TraderName: traderName,
		Title:      fmt.Sprintf("%s | 重入 AI 结论 %s %s → %s", traderName, analysis.Symbol, strings.ToUpper(analysis.Side), pv.Verdict),
		Body:       b.String(),
		RateKey:    fmt.Sprintf("reentry_ai_verdict|%d", analysis.ID),
	})
}

// AnalyzeAnalysis 手动触发内置 AI 分析（API 层调用，异步执行）。
// 不受 ai_enabled（自动分析开关）限制：只要模型可解析即可手动分析。
// 30s 冷却防误触连击烧 token；手动路径永不发邮件、永不自动入场。
func AnalyzeAnalysis(analysisID int64) error {
	defaultAdvisorMu.RLock()
	a := defaultAdvisor
	defaultAdvisorMu.RUnlock()
	if a == nil {
		return fmt.Errorf("重入 AI 助手插件未启动")
	}
	cfg, err := a.st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		return err
	}
	analysis, err := a.st.ReentryAI().GetReentryAnalysis(analysisID)
	if err != nil {
		return fmt.Errorf("分析记录不存在: %d", analysisID)
	}
	if analysis.CandidateID > 0 {
		return fmt.Errorf("AI 候选分析由持久化调度器管理，禁止手动触发")
	}
	if _, err := resolveAIModel(a.st, cfg); err != nil {
		return err
	}
	a.inflightMu.Lock()
	if a.inflight[analysisID] {
		a.inflightMu.Unlock()
		return fmt.Errorf("该记录的 AI 分析正在进行中，请稍候")
	}
	if last, ok := a.analyzeLast[analysisID]; ok {
		if since := time.Since(last); since < manualAnalyzeCooldown {
			a.inflightMu.Unlock()
			return fmt.Errorf("操作过于频繁，请 %d 秒后再试", int((manualAnalyzeCooldown-since).Seconds())+1)
		}
	}
	a.analyzeLast[analysisID] = time.Now()
	a.inflightMu.Unlock()
	if !a.spawnAnalysis(analysisID, false) {
		return fmt.Errorf("重入 AI 助手插件已停止")
	}
	return nil
}
