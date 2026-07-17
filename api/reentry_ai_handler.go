package api

import (
	"database/sql"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"nofx/reentryadvisor"
	"nofx/store"
)

// ============================================================================
// 重入 AI 助手（Reentry Advisor）API
//
// Phase 1（半自动）：
//   GET  /api/reentry-advisor/signals/:signal_id/analyses     信号的分析记录列表
//   POST /api/reentry-advisor/signals/:signal_id/regenerate   重新生成数据包（60s 冷却）
//   PUT  /api/reentry-advisor/analyses/:id/external           保存外部 AI 粘贴结论
//   GET  /api/reentry-advisor/config                          全局配置
//   PUT  /api/reentry-advisor/config                          保存全局配置
//
// Phase 2（内置 AI 分析）：
//   POST /api/reentry-advisor/analyses/:id/analyze            手动触发内置 AI 分析（异步）
//   GET  /api/reentry-advisor/stats                           结论分布与准确率统计
//   GET  /api/reentry-advisor/analyses                        分析历史列表（跨信号，最新在前）
//   GET  /api/reentry-advisor/market-preview?symbol=          市场指标实时预览（60s 缓存）
//
// 归属校验：分析记录 → 信号 → 交易员 → 当前用户（同人工重入信号 API 口径）
// ============================================================================

// ReentryAIHandler 重入 AI 助手 Handler
type ReentryAIHandler struct {
	store *store.Store
}

// NewReentryAIHandler 创建 Handler
func NewReentryAIHandler(st *store.Store) *ReentryAIHandler {
	return &ReentryAIHandler{store: st}
}

// RegisterRoutes 注册路由（挂 protected 组，已带 auth 中间件）
func (h *ReentryAIHandler) RegisterRoutes(group *gin.RouterGroup) {
	g := group.Group("/reentry-advisor")
	{
		g.GET("/signals/:signal_id/analyses", h.ListAnalyses)
		g.POST("/signals/:signal_id/regenerate", h.RegenerateAnalysis)
		g.PUT("/analyses/:id/external", h.SaveExternal)
		g.POST("/analyses/:id/analyze", h.AnalyzeInternal)
		g.GET("/analyses", h.ListHistory)
		g.GET("/market-preview", h.MarketPreview)
		g.GET("/stats", h.GetStats)
		g.GET("/config", h.GetConfig)
		g.PUT("/config", h.SaveConfig)
	}
}

// ownedTraderSet 当前用户名下交易员 ID 集合
func (h *ReentryAIHandler) ownedTraderSet(c *gin.Context) (map[string]bool, error) {
	list, err := h.store.Trader().List(c.GetString("user_id"))
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, t := range list {
		set[t.ID] = true
	}
	return set, nil
}

// ownedSignal 解析 :signal_id 并校验归属；失败时已写响应，返回 nil
func (h *ReentryAIHandler) ownedSignal(c *gin.Context) *store.CopyGuardManualReentrySignal {
	id, err := strconv.ParseInt(c.Param("signal_id"), 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid signal id"})
		return nil
	}
	owned, err := h.ownedTraderSet(c)
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

// ownedAnalysis 解析 :id 并校验分析记录归属；失败时已写响应，返回 nil
func (h *ReentryAIHandler) ownedAnalysis(c *gin.Context) *store.ReentryAIAnalysis {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid analysis id"})
		return nil
	}
	analysis, err := h.store.ReentryAI().GetReentryAnalysis(id)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(404, gin.H{"error": "analysis not found"})
		} else {
			c.JSON(500, gin.H{"error": err.Error()})
		}
		return nil
	}
	owned, err := h.ownedTraderSet(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return nil
	}
	if !owned[analysis.TraderID] {
		c.JSON(403, gin.H{"error": "analysis not owned by current user"})
		return nil
	}
	return analysis
}

// ListAnalyses 某信号的分析记录（最新在前）
// @Summary 重入分析记录列表
// @Tags ReentryAdvisor
// @Param signal_id path int true "Signal ID"
// @Router /api/reentry-advisor/signals/{signal_id}/analyses [get]
func (h *ReentryAIHandler) ListAnalyses(c *gin.Context) {
	sig := h.ownedSignal(c)
	if sig == nil {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	analyses, err := h.store.ReentryAI().ListReentryAnalysesBySignal(sig.ID, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"analyses": analyses, "count": len(analyses)})
}

// RegenerateAnalysis 重新生成数据包（新快照新记录，60s 冷却）
// @Summary 重新生成重入分析数据包
// @Tags ReentryAdvisor
// @Param signal_id path int true "Signal ID"
// @Router /api/reentry-advisor/signals/{signal_id}/regenerate [post]
func (h *ReentryAIHandler) RegenerateAnalysis(c *gin.Context) {
	sig := h.ownedSignal(c)
	if sig == nil {
		return
	}
	analysis, err := reentryadvisor.RegenerateForSignal(sig.ID)
	if err != nil {
		// 冷却/状态/插件关闭等都是用户可读错误
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "数据包已重新生成", "analysis": analysis})
}

// SaveExternal 保存外部 AI 粘贴结论（永久可编辑）
// @Summary 保存外部 AI 结论
// @Tags ReentryAdvisor
// @Param id path int true "Analysis ID"
// @Router /api/reentry-advisor/analyses/{id}/external [put]
func (h *ReentryAIHandler) SaveExternal(c *gin.Context) {
	analysis := h.ownedAnalysis(c)
	if analysis == nil {
		return
	}
	id := analysis.ID
	var req struct {
		ExternalResponse string `json:"external_response"`
		ExternalVerdict  string `json:"external_verdict"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "invalid request body"})
		return
	}
	req.ExternalVerdict = strings.ToUpper(strings.TrimSpace(req.ExternalVerdict))
	if err := h.store.ReentryAI().UpdateReentryExternal(id, req.ExternalResponse, req.ExternalVerdict); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	latest, err := h.store.ReentryAI().GetReentryAnalysis(id)
	if err != nil {
		latest = analysis
	}
	c.JSON(200, gin.H{"message": "外部 AI 结论已保存", "analysis": latest})
}

// AnalyzeInternal 手动触发内置 AI 分析（异步执行，前端轮询列表拿结果）
// @Summary 触发内置 AI 分析
// @Tags ReentryAdvisor
// @Param id path int true "Analysis ID"
// @Router /api/reentry-advisor/analyses/{id}/analyze [post]
func (h *ReentryAIHandler) AnalyzeInternal(c *gin.Context) {
	analysis := h.ownedAnalysis(c)
	if analysis == nil {
		return
	}
	if analysis.CandidateID > 0 {
		c.JSON(409, gin.H{"error": "AI 候选分析由持久化调度器管理，禁止手动触发"})
		return
	}
	if err := reentryadvisor.AnalyzeAnalysis(analysis.ID); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "AI 分析已开始，结果稍后自动写入本条记录"})
}

// ownedTraderIDs 当前用户名下交易员 ID 列表（统计/列表接口的归属过滤参数）
func (h *ReentryAIHandler) ownedTraderIDs(c *gin.Context) ([]string, error) {
	set, err := h.ownedTraderSet(c)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids, nil
}

// ListHistory 分析历史列表（跨信号，限当前用户名下交易员，最新在前）
// @Summary 重入分析历史
// @Tags ReentryAdvisor
// @Router /api/reentry-advisor/analyses [get]
func (h *ReentryAIHandler) ListHistory(c *gin.Context) {
	ids, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	analyses, err := h.store.ReentryAI().ListReentryAnalysesByTraders(ids, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"analyses": analyses, "count": len(analyses)})
}

// MarketPreview 市场指标实时预览（与信号无关，同数据包指标口径，60s 结果缓存）
// @Summary 市场指标预览
// @Tags ReentryAdvisor
// @Param symbol query string true "交易对，如 BTCUSDT"
// @Router /api/reentry-advisor/market-preview [get]
func (h *ReentryAIHandler) MarketPreview(c *gin.Context) {
	preview, err := reentryadvisor.GetMarketPreview(c.Query("symbol"))
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"preview": preview})
}

// GetStats 内外部 AI 结论分布与准确率统计（限当前用户名下交易员）
// @Summary 重入 AI 统计
// @Tags ReentryAdvisor
// @Router /api/reentry-advisor/stats [get]
func (h *ReentryAIHandler) GetStats(c *gin.Context) {
	ids, err := h.ownedTraderIDs(c)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	stats, err := h.store.ReentryAI().GetReentryAIStats(ids)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"stats": stats})
}

// GetConfig 全局配置（附内置默认 Prompt，供配置页占位与"恢复默认"）
// @Summary 重入 AI 助手配置
// @Tags ReentryAdvisor
// @Router /api/reentry-advisor/config [get]
func (h *ReentryAIHandler) GetConfig(c *gin.Context) {
	cfg, err := h.store.ReentryAI().GetReentryAIConfig()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"config": cfg, "default_prompt": reentryadvisor.DefaultSystemPrompt()})
}

// SaveConfig 保存全局配置
// @Summary 保存重入 AI 助手配置
// @Tags ReentryAdvisor
// @Router /api/reentry-advisor/config [put]
func (h *ReentryAIHandler) SaveConfig(c *gin.Context) {
	var cfg store.ReentryAIConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(400, gin.H{"error": "invalid request body"})
		return
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 60
	}
	if cfg.ConfidenceThreshold <= 0 || cfg.ConfidenceThreshold > 1 {
		cfg.ConfidenceThreshold = 0.7
	}
	if cfg.Provider == "" {
		cfg.Provider = "deepseek"
	}
	// Phase 3 依赖链：自动入场必须建立在自动分析之上（无分析则无结论可依据）
	if cfg.AutoEntryEnabled && !cfg.AIEnabled {
		c.JSON(400, gin.H{"error": "开启 AI 自动入场前，请先开启「自动内置 AI 分析」"})
		return
	}
	// model 非空时校验其存在并同步 provider（provider 列仅作展示冗余）
	if cfg.Model != "" {
		m, err := h.store.AIModel().GetByID(cfg.Model)
		if err != nil {
			c.JSON(400, gin.H{"error": "所选模型不存在: " + cfg.Model})
			return
		}
		cfg.Provider = m.Provider
	}
	if err := h.store.ReentryAI().SaveReentryAIConfig(&cfg); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "配置已保存", "config": cfg})
}
