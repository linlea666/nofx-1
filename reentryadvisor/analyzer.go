package reentryadvisor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"nofx/copytrade"
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

// parsedVerdict 解析归一化后的结论
type parsedVerdict struct {
	Verdict           string
	Confidence        float64
	SuggestedNotional float64 // 仅 ENTER 且模型给出有效值时 >0
	ReasonsJSON       string  // reasons + risk_notes（+建议金额）的 JSON，前端整体展示
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
// autoTriggered=true（新信号自动分析路径）时：完成后追加一封结论邮件（与
// 既有信号邮件互补，不阻塞不修改 copytrade 邮件流程）；且在 Phase 3 自动
// 入场开启并达到置信度门槛时尝试自动确认重入。手动触发（按钮/重新生成）
// 永不自动入场。解析失败自动重试一次；两次都失败则保留 raw 供人工查看。
func (a *Advisor) runAnalysis(analysisID int64, autoTriggered bool) {
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

	var raw string
	var pv *parsedVerdict
	for attempt := 1; attempt <= 2; attempt++ {
		raw, err = client.CallWithMessages(analysis.SystemPrompt, analysis.UserPrompt)
		if err != nil {
			logger.Warnf("[ReentryAdvisor] AI 调用失败 (analysis=%d, 第 %d 次): %v", analysisID, attempt, err)
			continue
		}
		pv, err = parseAIVerdict(raw)
		if err == nil {
			break
		}
		logger.Warnf("[ReentryAdvisor] AI 回复解析失败 (analysis=%d, 第 %d 次): %v", analysisID, attempt, err)
	}
	if raw == "" {
		// 两次调用都没拿到回复，无可落库内容
		return
	}
	if pv == nil {
		pv = &parsedVerdict{} // 解析失败：verdict 留空，仅存 raw
	}
	if saveErr := a.st.ReentryAI().UpdateReentryInternalResult(analysis.ID, raw, pv.Verdict, pv.Confidence, pv.ReasonsJSON); saveErr != nil {
		logger.Warnf("[ReentryAdvisor] AI 结果落库失败 (analysis=%d): %v", analysisID, saveErr)
		return
	}
	if pv.Verdict == "" {
		logger.Warnf("[ReentryAdvisor] AI 分析完成但结论不可解析，已存原始回复 (analysis=%d)", analysisID)
		return
	}
	logger.Infof("[ReentryAdvisor] 内置 AI 结论: %s (confidence=%.2f) (analysis=%d, signal=%d, %s %s)",
		pv.Verdict, pv.Confidence, analysis.ID, analysis.SignalID, analysis.Symbol, analysis.Side)

	// Seam C：把 AI 二次入场分析结论记入统一跟单事件日志（best-effort），
	// 用于追踪 AI 接收/介入的执行情况。
	a.recordAIAnalysisEvent(analysis, pv, autoTriggered)

	// Phase 3：自动入场（仅自动分析路径；结果并入结论邮件）
	if autoTriggered {
		autoEntryNote := a.maybeAutoEnter(analysis, cfg, pv)
		a.notifyVerdict(analysis, cfg, pv, autoEntryNote)
	}
}

// recordAIAnalysisEvent 把一次内置 AI 二次入场分析结论落成统一跟单事件（best-effort）。
// Copy Guard / 二次入场仅 OKX 生效，故 provider 固定为 okx。
func (a *Advisor) recordAIAnalysisEvent(analysis *store.ReentryAIAnalysis, pv *parsedVerdict, autoTriggered bool) {
	if a.st == nil || analysis == nil || pv == nil {
		return
	}
	trigger := "manual"
	if autoTriggered {
		trigger = "auto"
	}
	summary := fmt.Sprintf("AI 二次入场分析结论: %s (置信度 %.0f%%)", pv.Verdict, pv.Confidence*100)
	ev := &store.CopyTradeEvent{
		TraderID:     analysis.TraderID,
		ProviderType: "okx",
		Category:     store.CopyEventCategoryTakeover,
		EventType:    "AI_ANALYSIS",
		Severity:     store.CopyEventSeverityInfo,
		Symbol:       analysis.Symbol,
		Side:         analysis.Side,
		CycleID:      analysis.CycleID,
		Operator:     "ai",
		Summary:      summary,
		Detail: map[string]interface{}{
			"verdict":     pv.Verdict,
			"confidence":  pv.Confidence,
			"trigger":     trigger,
			"analysis_id": analysis.ID,
			"signal_id":   analysis.SignalID,
		},
	}
	if err := a.st.CopyTrade().LogCopyEvent(ev); err != nil {
		logger.Warnf("[ReentryAdvisor] AI 分析事件落库失败 (analysis=%d): %v", analysis.ID, err)
	}
}

// autoEntryMaxSnapshotAge 自动入场的快照新鲜度护栏：数据包生成到 AI 结论
// 落地超过该时长（模型排队/超时重试导致）时放弃自动入场，转人工。
const autoEntryMaxSnapshotAge = 10 * time.Minute

// maybeAutoEnter Phase 3 自动确认重入。返回给结论邮件的执行说明（空=未尝试）。
//
// 安全设计：
//   - 双开关（ai_enabled + auto_entry_enabled）+ 置信度门槛，默认全关；
//   - 只在结论为 ENTER 时触发，金额取 min(模型建议, 信号建议)（上界仍由
//     ConfirmManualReentry 封死在首仓名义）；
//   - 完整复用人工确认链路 ConfirmManualReentryForTrader：领航员仍持仓、
//     方向一致、本地无同向仓位、金额下限、PENDING→EXECUTING 原子抢占等
//     全部硬校验与审计事件（operator="ai:auto"）原样生效；
//   - 任何失败只记日志+邮件说明，信号保持 PENDING 可人工处理。
func (a *Advisor) maybeAutoEnter(analysis *store.ReentryAIAnalysis, cfg *store.ReentryAIConfig, pv *parsedVerdict) string {
	if !cfg.AutoEntryEnabled {
		return ""
	}
	if pv.Verdict != store.ReentryVerdictEnter {
		return ""
	}
	if pv.Confidence < cfg.ConfidenceThreshold {
		return fmt.Sprintf("未自动入场：置信度 %.0f%% 低于门槛 %.0f%%，请人工决策。",
			pv.Confidence*100, cfg.ConfidenceThreshold*100)
	}
	if age := time.Since(analysis.SnapshotAt); age > autoEntryMaxSnapshotAge {
		return fmt.Sprintf("未自动入场：数据快照已过时（%s 前生成），请在页面重新生成后人工决策。",
			age.Truncate(time.Second))
	}
	sig, err := a.st.CopyTrade().GetManualReentrySignal(analysis.SignalID)
	if err != nil || sig.Status != store.ManualReentryStatusPending {
		return "未自动入场：信号已不在待确认状态。"
	}
	notional := sig.RecommendedNotional
	if pv.SuggestedNotional > 0 && pv.SuggestedNotional < notional {
		notional = pv.SuggestedNotional
	}
	logger.Infof("[ReentryAdvisor] 触发 AI 自动入场 (signal=%d, %s %s, confidence=%.2f≥%.2f, 金额=%.2f)",
		sig.ID, sig.Symbol, sig.Side, pv.Confidence, cfg.ConfidenceThreshold, notional)
	if err := copytrade.ConfirmManualReentryForTrader(sig.TraderID, sig.ID, "ai:auto", notional); err != nil {
		logger.Warnf("[ReentryAdvisor] AI 自动入场被拒 (signal=%d): %v", sig.ID, err)
		return fmt.Sprintf("自动入场未执行：%v。信号保持待确认，可人工处理。", err)
	}
	return fmt.Sprintf("已自动确认重入（金额 %.2f USDT），系统正在执行；执行结果见周期事件流。", notional)
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
		fmt.Fprintf(&b, "\n自动入场: %s\n", autoEntryNote)
	case cfg.AutoEntryEnabled:
		b.WriteString("\n自动入场已开启，但仅结论为 ENTER 时触发，本条未下单。请在 Copy Guard 页面查看完整数据包并人工决策。")
	default:
		b.WriteString("\n请在 Copy Guard 页面查看完整数据包并人工确认。AI 结论仅供参考，未开启自动入场时不会下单。")
	}
	notifier.Notify(notifier.Alert{
		Category: "copy_trade",
		TraderID: analysis.TraderID,
		Title:    fmt.Sprintf("重入 AI 结论 %s %s → %s", analysis.Symbol, strings.ToUpper(analysis.Side), pv.Verdict),
		Body:     b.String(),
		RateKey:  fmt.Sprintf("reentry_ai_verdict|%d", analysis.ID),
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
	if _, err := a.st.ReentryAI().GetReentryAnalysis(analysisID); err != nil {
		return fmt.Errorf("分析记录不存在: %d", analysisID)
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
