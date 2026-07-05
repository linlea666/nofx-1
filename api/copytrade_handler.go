package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

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
		copyTrade.GET("/risk/summary", h.GetRiskSummary)
		copyTrade.GET("/risk/cycles", h.GetRiskCycles)
		copyTrade.GET("/risk/cycles/:id", h.GetRiskCycle)
		copyTrade.GET("/risk/cycles/:id/export", h.ExportRiskCycle)
		copyTrade.GET("/risk/export", h.ExportRiskCycles)

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
	events, err := h.store.CopyTrade().ListCopyGuardEvents(id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	attempts, err := h.store.CopyTrade().ListCopyGuardAttempts(id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	protection, protectionErr := h.store.CopyTrade().GetCopyGuardProtectiveOrder(id)
	if protectionErr != nil {
		protection = nil
	}
	// 观察期采样时间线（v4.1）：出局后每个 tick 的价格/边界/门控轨迹
	watchSamples, samplesErr := h.store.CopyTrade().ListCopyGuardWatchSamples(id)
	if samplesErr != nil {
		watchSamples = nil
	}
	c.JSON(200, gin.H{"cycle": cycle, "events": events, "attempts": attempts, "protection": protection, "watch_samples": watchSamples})
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
	events, err := h.store.CopyTrade().ListCopyGuardEvents(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	attempts, err := h.store.CopyTrade().ListCopyGuardAttempts(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	protection, protectionErr := h.store.CopyTrade().GetCopyGuardProtectiveOrder(id)
	if protectionErr != nil {
		protection = nil
	}
	watchSamples, samplesErr := h.store.CopyTrade().ListCopyGuardWatchSamples(id)
	if samplesErr != nil {
		watchSamples = nil
	}
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=copy-guard-cycle-%d.jsonl", id))
	// schema_version 3：新增 watch_samples（观察期采样时间线，离线回测输入）
	_ = json.NewEncoder(c.Writer).Encode(gin.H{"schema_version": 3, "exported_at": time.Now().UTC(), "cycle": cycle, "attempts": attempts, "events": events, "protection": protection, "watch_samples": watchSamples})
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
				events, _ := h.store.CopyTrade().ListCopyGuardEvents(cycle.ID)
				attempts, _ := h.store.CopyTrade().ListCopyGuardAttempts(cycle.ID)
				protection, protectionErr := h.store.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
				if protectionErr != nil {
					protection = nil
				}
				watchSamples, samplesErr := h.store.CopyTrade().ListCopyGuardWatchSamples(cycle.ID)
				if samplesErr != nil {
					watchSamples = nil
				}
				_ = enc.Encode(gin.H{"cycle": cycle, "attempts": attempts, "events": events, "protection": protection, "watch_samples": watchSamples})
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
	_ = w.Write([]string{"cycle_id", "trader_id", "trader_name", "leader_id", "leader_pos_id", "symbol", "side", "margin_mode", "status", "accounting_status", "accounting_error", "tracking_difference", "protection_status", "protection_coverage", "protection_retries", "protection_missing_seconds", "protection_error", "stop_count", "reentry_count", "actual_pnl", "baseline_no_guard_pnl", "net_guard_effect", "fees", "funding_fee", "liquidation_penalty", "slippage", "opened_at", "closed_at"})
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
			_ = w.Write([]string{strconv.FormatInt(x.ID, 10), x.TraderID, x.TraderName, x.LeaderID, x.LeaderPosID, x.Symbol, x.Side, x.MarginMode, x.Status, x.AccountingStatus, x.AccountingError, fmt.Sprint(x.TrackingDifference), x.ProtectionStatus, fmt.Sprint(x.ProtectionCoverage), strconv.Itoa(x.ProtectionRetries), fmt.Sprint(x.ProtectionMissingSeconds), x.ProtectionError, strconv.Itoa(x.StopCount), strconv.Itoa(x.ReentryCount), fmt.Sprint(x.ActualPnL), fmt.Sprint(x.BaselinePnL), fmt.Sprint(x.NetGuardEffect), fmt.Sprint(x.Fees), fmt.Sprint(x.FundingFee), fmt.Sprint(x.LiquidationPenalty), fmt.Sprint(x.Slippage), x.OpenedAt.Format(time.RFC3339), closed})
		}
		w.Flush()
		if len(rows) < 500 {
			break
		}
	}
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
	operator := c.GetString("user_id")
	if err := copytrade.ConfirmManualReentryForTrader(sig.TraderID, sig.ID, operator); err != nil {
		// 硬校验失败/状态冲突等都是用户可读错误，直接透传
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	latest, err := h.store.CopyTrade().GetManualReentrySignal(sig.ID)
	if err != nil {
		latest = sig
	}
	c.JSON(200, gin.H{"message": "重入已确认，系统正在执行", "signal": latest})
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
	operator := c.GetString("user_id")
	ok, err := h.store.CopyTrade().DismissManualReentrySignal(sig.ID, operator)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(409, gin.H{"error": fmt.Sprintf("信号当前状态为 %s，无法忽略", sig.Status)})
		return
	}
	// 审计事件：忽略动作落周期事件流
	_ = h.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: sig.CycleID, TraderID: sig.TraderID, Type: "GUARD_MANUAL_REENTRY_DISMISSED", Price: sig.TriggerPrice, Notional: sig.RecommendedNotional, Metadata: map[string]interface{}{
		"signal_id": sig.ID,
		"operator":  operator,
	}})
	c.JSON(200, gin.H{"message": "信号已忽略"})
}

// CopyTradeConfigRequest 跟单配置请求
type CopyTradeConfigRequest struct {
	ProviderType   string  `json:"provider_type" binding:"required,oneof=hyperliquid okx binance"`
	LeaderID       string  `json:"leader_id" binding:"required"`
	CopyRatio      float64 `json:"copy_ratio" binding:"required,gt=0"`
	SyncLeverage   bool    `json:"sync_leverage"`
	SyncMarginMode bool    `json:"sync_margin_mode"`
	MinTradeWarn   float64 `json:"min_trade_warn"`
	MaxTradeWarn   float64 `json:"max_trade_warn"`
	Enabled        bool    `json:"enabled"`

	// Binance Web 凭证（仅 ProviderType=binance 时使用）
	BinanceP20T      string `json:"binance_p20t"`
	BinanceCSRFToken string `json:"binance_csrf_token"`

	// ============================================================
	// Copy Guard 账户保护 / 止损兜底（v5）—— 仅 OKX 路径生效
	// 所有字段都是可选的（前端不传走 store 默认值，详见 store.CopyTradeConfig.FillRiskDefaults）
	// v3 遗留字段（risk_atr_enabled / risk_reentry_tolerance / 反加仓铁律 /
	// risk_stop_noise_floor_atr / risk_cycle_max_loss_pct）已随 v5 下线，
	// 前端传入将被忽略。
	// ============================================================
	RiskStopLossEnabled  *bool    `json:"risk_stop_loss_enabled,omitempty"`
	RiskAccountPct       *float64 `json:"risk_account_pct,omitempty"`
	RiskATRMultiplier    *float64 `json:"risk_atr_multiplier,omitempty"`
	RiskATRTimeframe     *string  `json:"risk_atr_timeframe,omitempty"`
	RiskLeverageFallback *bool    `json:"risk_leverage_fallback,omitempty"`
	RiskLeverageMaxLoss  *float64 `json:"risk_leverage_max_loss,omitempty"`
	RiskReentryEnabled   *bool    `json:"risk_reentry_enabled,omitempty"`
	RiskReentryRatio     *float64 `json:"risk_reentry_ratio,omitempty"`
	// v5.1 人工重入（自动重入用尽后邮件提醒+人工确认代执行），默认 true
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
		TraderID:         traderID,
		ProviderType:     req.ProviderType,
		LeaderID:         req.LeaderID,
		CopyRatio:        req.CopyRatio,
		SyncLeverage:     req.SyncLeverage,
		SyncMarginMode:   req.SyncMarginMode,
		MinTradeWarn:     req.MinTradeWarn,
		MaxTradeWarn:     req.MaxTradeWarn,
		Enabled:          req.Enabled,
		BinanceP20T:      req.BinanceP20T,
		BinanceCSRFToken: req.BinanceCSRFToken,
	}

	// 读取已有配置，作为风控字段的旧值兜底
	// 设计：风控字段全部可选；前端不传字段保持原值；旧库读出来的默认值由 FillRiskDefaults 兜底
	existing, _ := h.store.CopyTrade().GetByTraderID(traderID)

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
		config.RiskLeverageFallback = true // 默认 on
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
	if req.RiskManualReentryEnabled != nil {
		config.RiskManualReentryEnabled = *req.RiskManualReentryEnabled
	} else if existing != nil {
		config.RiskManualReentryEnabled = existing.RiskManualReentryEnabled
	} else {
		config.RiskManualReentryEnabled = true // 默认 on
	}
	applyCopyGuardV4Request(config, existing, &req)
	if config.RiskPolicyVersion >= 4 && config.ProviderType != "okx" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "copy guard is only supported for OKX"})
		return
	}
	// v5：账户线是硬兜底（默认 10%），>= 50% 属于极端配置需二次确认。
	// v3→v4 迁移确认已下线：存量配置由 store 层在加载时强制升级。
	if config.RiskPolicyVersion >= 4 && config.RiskAccountPct >= 0.50 && !req.RiskHighRiskConfirmed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "risk_account_pct >= 50% requires risk_high_risk_confirmed"})
		return
	}
	if err := copytrade.ValidateStoredRiskPolicy(config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
		"config":  config,
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
