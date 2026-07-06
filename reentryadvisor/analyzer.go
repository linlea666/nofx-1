package reentryadvisor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"nofx/logger"
	"nofx/mcp"
	"nofx/notifier"
	"nofx/store"
)

// ============================================================================
// Phase 2：内置 AI 分析
//
// 对已落库的分析快照（reentry_ai_analyses）调用内置 LLM，得到与外部 AI 同源
// 同 Prompt 的结论（verdict/confidence/reasons），写回同一条记录，供前端
// 展示与内外对比。密钥复用 ai_models 表（与自动交易/辩论模块同一套配置）。
//
// 失败降级：模型未配置、调用超时、JSON 解析失败都只记日志 + 保留 raw 回复，
// 不影响数据包透明化与人工确认主流程。
// ============================================================================

// aiVerdict 模型输出的严格 JSON 结论（schema 见 buildSystemPrompt 输出格式段）
type aiVerdict struct {
	Decision          string   `json:"decision"`
	Confidence        float64  `json:"confidence"`
	SuggestedNotional float64  `json:"suggested_notional"`
	Reasons           []string `json:"reasons"`
	RiskNotes         []string `json:"risk_notes"`
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
	switch m.Provider {
	case "deepseek":
		client = mcp.NewDeepSeekClient()
	case "qwen":
		client = mcp.NewQwenClient()
	case "openai":
		client = mcp.NewOpenAIClient()
	case "claude":
		client = mcp.NewClaudeClient()
	case "gemini":
		client = mcp.NewGeminiClient()
	case "grok":
		client = mcp.NewGrokClient()
	case "kimi":
		client = mcp.NewKimiClient()
	case "custom":
		client = mcp.New()
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

// parseAIVerdict 解析并校验模型回复。返回归一化 verdict、置信度与
// reasons JSON（含 risk_notes 与建议金额，前端整体展示）。
func parseAIVerdict(raw string) (verdict string, confidence float64, reasonsJSON string, err error) {
	obj, ok := extractJSONObject(raw)
	if !ok {
		return "", 0, "", fmt.Errorf("回复中未找到 JSON 对象")
	}
	var v aiVerdict
	if err := json.Unmarshal([]byte(obj), &v); err != nil {
		return "", 0, "", fmt.Errorf("JSON 解析失败: %w", err)
	}
	verdict = strings.ToUpper(strings.TrimSpace(v.Decision))
	switch verdict {
	case store.ReentryVerdictEnter, store.ReentryVerdictWait, store.ReentryVerdictSkip:
	default:
		return "", 0, "", fmt.Errorf("decision 字段无效: %q", v.Decision)
	}
	confidence = v.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	reasonsPayload := map[string]interface{}{
		"reasons":    v.Reasons,
		"risk_notes": v.RiskNotes,
	}
	if verdict == store.ReentryVerdictEnter && v.SuggestedNotional > 0 {
		reasonsPayload["suggested_notional"] = v.SuggestedNotional
	}
	b, err := json.Marshal(reasonsPayload)
	if err != nil {
		return "", 0, "", err
	}
	return verdict, confidence, string(b), nil
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

// AnalysisRunning 该分析记录是否正在内置 AI 分析中（API 状态回显用）
func (a *Advisor) AnalysisRunning(id int64) bool {
	a.inflightMu.Lock()
	defer a.inflightMu.Unlock()
	return a.inflight[id]
}

// runAnalysis 对一条分析快照执行内置 AI 分析并写回结果。
// sendEmail=true 时（自动触发路径）完成后追加一封结论邮件，与既有信号邮件
// 互补：信号邮件即时发出，AI 结论稍后跟进，不阻塞不修改 copytrade 邮件流程。
// 解析失败自动重试一次（重新调用模型）；两次都失败则保留 raw 供人工查看。
func (a *Advisor) runAnalysis(analysisID int64, sendEmail bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("[ReentryAdvisor] AI 分析 panic 已恢复 (analysis=%d): %v", analysisID, r)
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
	cfg, err := a.st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		logger.Warnf("[ReentryAdvisor] AI 分析读取配置失败: %v", err)
		return
	}
	model, err := resolveAIModel(a.st, cfg)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] AI 分析模型解析失败 (analysis=%d): %v", analysisID, err)
		return
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	client, err := newAIClientForModel(model, timeout)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] AI 客户端创建失败 (analysis=%d): %v", analysisID, err)
		return
	}

	logger.Infof("[ReentryAdvisor] 开始内置 AI 分析 (analysis=%d, signal=%d, %s %s, model=%s/%s)",
		analysis.ID, analysis.SignalID, analysis.Symbol, analysis.Side, model.Provider, model.ID)

	var raw, verdict, reasonsJSON string
	var confidence float64
	for attempt := 1; attempt <= 2; attempt++ {
		raw, err = client.CallWithMessages(analysis.SystemPrompt, analysis.UserPrompt)
		if err != nil {
			logger.Warnf("[ReentryAdvisor] AI 调用失败 (analysis=%d, 第 %d 次): %v", analysisID, attempt, err)
			continue
		}
		verdict, confidence, reasonsJSON, err = parseAIVerdict(raw)
		if err == nil {
			break
		}
		logger.Warnf("[ReentryAdvisor] AI 回复解析失败 (analysis=%d, 第 %d 次): %v", analysisID, attempt, err)
	}
	if raw == "" {
		// 两次调用都没拿到回复，无可落库内容
		return
	}
	if saveErr := a.st.ReentryAI().UpdateReentryInternalResult(analysis.ID, raw, verdict, confidence, reasonsJSON); saveErr != nil {
		logger.Warnf("[ReentryAdvisor] AI 结果落库失败 (analysis=%d): %v", analysisID, saveErr)
		return
	}
	if verdict == "" {
		logger.Warnf("[ReentryAdvisor] AI 分析完成但结论不可解析，已存原始回复 (analysis=%d)", analysisID)
		return
	}
	logger.Infof("[ReentryAdvisor] 内置 AI 结论: %s (confidence=%.2f) (analysis=%d, signal=%d, %s %s)",
		verdict, confidence, analysis.ID, analysis.SignalID, analysis.Symbol, analysis.Side)

	if sendEmail {
		a.notifyVerdict(analysis, verdict, confidence, reasonsJSON)
	}
}

// notifyVerdict AI 结论邮件（自动分析路径）。信号本身的告警邮件由 copytrade
// 即时发出；这封是补充结论，限流键独立，notifier 未启用时零副作用。
func (a *Advisor) notifyVerdict(analysis *store.ReentryAIAnalysis, verdict string, confidence float64, reasonsJSON string) {
	verdictText := map[string]string{
		store.ReentryVerdictEnter: "ENTER（建议确认重入）",
		store.ReentryVerdictWait:  "WAIT（建议继续观察）",
		store.ReentryVerdictSkip:  "SKIP（建议忽略本信号）",
	}[verdict]
	var payload struct {
		Reasons           []string `json:"reasons"`
		RiskNotes         []string `json:"risk_notes"`
		SuggestedNotional float64  `json:"suggested_notional"`
	}
	_ = json.Unmarshal([]byte(reasonsJSON), &payload)
	var b strings.Builder
	fmt.Fprintf(&b, "人工重入信号 #%d（%s %s）的内置 AI 分析已完成。\n\n", analysis.SignalID, analysis.Symbol, strings.ToUpper(analysis.Side))
	fmt.Fprintf(&b, "结论: %s\n置信度: %.0f%%\n", verdictText, confidence*100)
	if payload.SuggestedNotional > 0 {
		fmt.Fprintf(&b, "建议金额: %.2f USDT\n", payload.SuggestedNotional)
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
	b.WriteString("\n请在 Copy Guard 页面查看完整数据包并人工确认。AI 结论仅供参考，不会自动入场。")
	notifier.Notify(notifier.Alert{
		Category: "copy_trade",
		TraderID: analysis.TraderID,
		Title:    fmt.Sprintf("重入 AI 结论 %s %s → %s", analysis.Symbol, strings.ToUpper(analysis.Side), verdict),
		Body:     b.String(),
		RateKey:  fmt.Sprintf("reentry_ai_verdict|%d", analysis.ID),
	})
}

// AnalyzeAnalysis 手动触发内置 AI 分析（API 层调用，异步执行）。
// 不受 ai_enabled（自动分析开关）限制：只要模型可解析即可手动分析。
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
	if _, err := a.st.ReentryAI().GetReentryAnalysis(analysisID); err != nil {
		return fmt.Errorf("分析记录不存在: %d", analysisID)
	}
	if _, err := resolveAIModel(a.st, cfg); err != nil {
		return err
	}
	if a.AnalysisRunning(analysisID) {
		return fmt.Errorf("该记录的 AI 分析正在进行中，请稍候")
	}
	go a.runAnalysis(analysisID, false)
	return nil
}
