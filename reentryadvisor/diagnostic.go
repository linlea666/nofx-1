package reentryadvisor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"nofx/logger"
	"nofx/mcp"
	"nofx/store"
)

const connectionSelfTestPrompt = `这是一次零交易的连接与 JSON Schema 自检，不包含真实候选，也绝不应建议入场。请忽略市场判断，严格只返回以下 JSON 对象且不要输出其他文本：
{"decision":"WAIT","regime":"CHOP","confidence":0.0,"size_factor":0.0,"entry_price_low":0.0,"entry_price_high":0.0,"attention_price_low":0.0,"attention_price_high":0.0,"ttl_seconds":30,"next_review_seconds":900,"reasons":["connection and schema self-test"],"risk_notes":["no trade candidate was evaluated"]}`

// runCandidateSchemaSelfTest is deliberately shared by the production client
// factory and unit tests. Passing the same strict parser here catches provider,
// model, Prompt and JSON-contract failures without creating a candidate or
// entering the execution path.
func runCandidateSchemaSelfTest(client mcp.AIClient, systemPrompt string) (string, *parsedVerdict, error) {
	if client == nil {
		return "", nil, fmt.Errorf("AI client is nil")
	}
	raw, err := client.CallWithMessages(systemPrompt, connectionSelfTestPrompt)
	if err != nil {
		return "", nil, err
	}
	pv, err := parseAICandidateVerdict(raw)
	if err != nil {
		return raw, nil, err
	}
	if pv.Verdict != store.ReentryVerdictWait {
		return raw, pv, fmt.Errorf("自检要求 WAIT，模型实际返回 %s", pv.Verdict)
	}
	return raw, pv, nil
}

// RunConnectionSelfTest performs one paid but zero-trade model call. The
// result is written to a dedicated diagnostics table and is intentionally not
// counted in candidate quotas, AI trading statistics, or cycle exports.
func RunConnectionSelfTest(st *store.Store, userID string) (*store.ReentryAIDiagnostic, error) {
	if st == nil || strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("invalid self-test context")
	}
	diagnostic := &store.ReentryAIDiagnostic{UserID: userID, PromptVersion: candidatePromptVersion}
	started := time.Now()
	cfg, err := st.ReentryAI().GetReentryAIConfig()
	if err == nil {
		diagnostic.Model = cfg.Model
		diagnostic.Provider = cfg.Provider
	}
	if err != nil {
		diagnostic.Error = "读取 AI 配置失败: " + err.Error()
		return saveConnectionDiagnostic(st, diagnostic)
	}
	model, err := resolveAIModel(st, cfg)
	if err != nil {
		diagnostic.Error = err.Error()
		return saveConnectionDiagnostic(st, diagnostic)
	}
	diagnostic.Model = model.ID
	diagnostic.Provider = model.Provider
	client, err := newAIClientForModel(model, time.Duration(cfg.TimeoutSeconds)*time.Second)
	if err != nil {
		diagnostic.Error = err.Error()
		return saveConnectionDiagnostic(st, diagnostic)
	}
	raw, pv, callErr := runCandidateSchemaSelfTest(client, candidateSystemPrompt(cfg.AnalysisFocus))
	diagnostic.LatencyMS = time.Since(started).Milliseconds()
	diagnostic.RawResponse = raw
	if pv != nil {
		parsedPayload := map[string]interface{}{
			"decision":             pv.Verdict,
			"regime":               pv.Regime,
			"confidence":           pv.Confidence,
			"size_factor":          pv.SizeFactor,
			"entry_price_low":      pv.EntryPriceLow,
			"entry_price_high":     pv.EntryPriceHigh,
			"attention_price_low":  pv.AttentionPriceLow,
			"attention_price_high": pv.AttentionPriceHigh,
			"ttl_seconds":          pv.TTLSeconds,
			"next_review_seconds":  pv.NextReviewSeconds,
		}
		var evidence map[string]interface{}
		if json.Unmarshal([]byte(pv.ReasonsJSON), &evidence) == nil {
			parsedPayload["reasons"] = evidence["reasons"]
			parsedPayload["risk_notes"] = evidence["risk_notes"]
		}
		parsed, _ := json.Marshal(parsedPayload)
		diagnostic.ParsedJSON = string(parsed)
	}
	if callErr != nil {
		diagnostic.Error = callErr.Error()
	} else {
		diagnostic.Success = true
	}
	return saveConnectionDiagnostic(st, diagnostic)
}

func saveConnectionDiagnostic(st *store.Store, diagnostic *store.ReentryAIDiagnostic) (*store.ReentryAIDiagnostic, error) {
	saved, err := st.ReentryAI().SaveReentryAIDiagnostic(diagnostic)
	if err != nil {
		return nil, fmt.Errorf("保存 AI 自检审计失败: %w", err)
	}
	if saved.Success {
		logger.Infof("[ReentryAdvisor] AI 连接自检成功 user=%s model=%s/%s latency_ms=%d prompt=%s", saved.UserID, saved.Provider, saved.Model, saved.LatencyMS, saved.PromptVersion)
	} else {
		logger.Warnf("[ReentryAdvisor] AI 连接自检失败 user=%s model=%s/%s latency_ms=%d prompt=%s error=%s", saved.UserID, saved.Provider, saved.Model, saved.LatencyMS, saved.PromptVersion, saved.Error)
	}
	return saved, nil
}
