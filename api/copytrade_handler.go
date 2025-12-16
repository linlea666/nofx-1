package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"nofx/copytrade"
	"nofx/logger"
	"nofx/manager"
	"nofx/store"
)

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
	}
}

// CopyTradeConfigRequest 跟单配置请求
type CopyTradeConfigRequest struct {
	ProviderType   string  `json:"provider_type" binding:"required,oneof=hyperliquid okx"`
	LeaderID       string  `json:"leader_id" binding:"required"`
	CopyRatio      float64 `json:"copy_ratio" binding:"required,gt=0"`
	SyncLeverage   bool    `json:"sync_leverage"`
	SyncMarginMode bool    `json:"sync_margin_mode"`
	MinTradeWarn   float64 `json:"min_trade_warn"`
	MaxTradeWarn   float64 `json:"max_trade_warn"`
	Enabled        bool    `json:"enabled"`
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

	c.JSON(http.StatusOK, gin.H{
		"config": config,
		"status": copytrade.IsCopyTradingRunning(traderID),
	})
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
	}

	// 保存配置
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
		"config":  config,
	})
}

// DeleteConfig 删除跟单配置
// @Summary 删除跟单配置
// @Tags CopyTrade
// @Param trader_id path string true "Trader ID"
// @Success 200 {object} map[string]interface{}
// @Router /api/copytrade/config/{trader_id} [delete]
func (h *CopyTradeHandler) DeleteConfig(c *gin.Context) {
	traderID := c.Param("trader_id")

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

