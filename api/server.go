package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"nofx/auth"
	"nofx/backtest"
	"nofx/config"
	"nofx/copytrade"
	"nofx/crypto"
	"nofx/logger"
	"nofx/manager"
	"nofx/store"
	"nofx/trader"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// isTraderRunning checks if a trader is running (unified for both AI and copy trade modes)
// This is the single source of truth for trader running status
func (s *Server) isTraderRunning(traderID string) bool {
	state, err := s.store.Trader().GetLifecycle(traderID)
	return err == nil && state.Status == store.TraderLifecycleRunning && state.IsRunning
}

func (s *Server) getExchangeArchiveBlockers(traderID string) []store.TraderLifecycleBlocker {
	traderConfig, err := s.store.Trader().GetByID(traderID)
	if err != nil {
		return []store.TraderLifecycleBlocker{{
			Code: "EXCHANGE_POSITION_VERIFICATION_UNAVAILABLE", ResourceID: traderID,
			Status: err.Error(),
		}}
	}
	var exchangeTrader interface{}
	if s.positionSyncManager != nil {
		exchangeTrader, err = s.positionSyncManager.GetTraderForReconciliation(traderID)
	} else {
		exchangeTrader, err = s.traderManager.GetTrader(traderID)
	}
	if err != nil {
		return []store.TraderLifecycleBlocker{{
			Code: "EXCHANGE_POSITION_VERIFICATION_UNAVAILABLE", ResourceID: traderConfig.ExchangeID,
			Status: err.Error(),
		}}
	}
	freshProvider, ok := exchangeTrader.(trader.FreshPositionProvider)
	if !ok {
		return []store.TraderLifecycleBlocker{{
			Code: "EXCHANGE_POSITION_VERIFICATION_UNAVAILABLE", ResourceID: traderConfig.ExchangeID,
			Status: "exchange does not support authoritative fresh position reads",
		}}
	}
	positions, err := freshProvider.GetPositionsFresh()
	if err != nil {
		return []store.TraderLifecycleBlocker{{
			Code: "EXCHANGE_POSITION_VERIFICATION_FAILED", ResourceID: traderConfig.ExchangeID,
			Status: err.Error(),
		}}
	}
	var blockers []store.TraderLifecycleBlocker
	if pendingProvider, ok := exchangeTrader.(trader.PendingOrderProvider); ok {
		pendingOrders, pendingErr := pendingProvider.GetPendingOrdersFresh()
		if pendingErr != nil {
			return []store.TraderLifecycleBlocker{{
				Code: "EXCHANGE_ORDER_VERIFICATION_FAILED", ResourceID: traderConfig.ExchangeID,
				Status: pendingErr.Error(),
			}}
		}
		for _, order := range pendingOrders {
			code := "EXCHANGE_ORDER_PENDING"
			if order.Protective {
				code = "EXCHANGE_PROTECTIVE_ORDER_PENDING"
			}
			blockers = append(blockers, store.TraderLifecycleBlocker{
				Code: code, ResourceID: order.ID, Symbol: order.Symbol, Status: order.Status,
			})
		}
	}
	if len(positions) == 0 {
		return blockers
	}
	accountUsers, err := s.store.Trader().CountNonArchivedByExchange(traderConfig.ExchangeID)
	if err != nil {
		return []store.TraderLifecycleBlocker{{
			Code: "EXCHANGE_ACCOUNT_OWNERSHIP_UNAVAILABLE", ResourceID: traderConfig.ExchangeID,
			Status: err.Error(),
		}}
	}
	if accountUsers > 1 {
		// A live position on a shared account cannot be assigned to the trader
		// being archived merely from symbol/side. Fail closed and require an
		// explicit ownership reconciliation instead of risking orphaned funds.
		return []store.TraderLifecycleBlocker{{
			Code:       "SHARED_ACCOUNT_POSITION_OWNERSHIP_AMBIGUOUS",
			ResourceID: traderConfig.ExchangeID,
			Status:     fmt.Sprintf("%d open exchange positions across %d non-archived traders", len(positions), accountUsers),
		}}
	}
	for index, position := range positions {
		symbol, _ := position["symbol"].(string)
		blockers = append(blockers, store.TraderLifecycleBlocker{
			Code: "EXCHANGE_POSITION_PRESENT", ResourceID: fmt.Sprintf("%s:%d", traderConfig.ExchangeID, index),
			Symbol: symbol, Status: "OPEN",
		})
	}
	return blockers
}

// Server HTTP API server
type Server struct {
	router              *gin.Engine
	traderManager       *manager.TraderManager
	positionSyncManager *trader.PositionSyncManager
	store               *store.Store
	cryptoHandler       *CryptoHandler
	backtestManager     *backtest.Manager
	debateHandler       *DebateHandler
	httpServer          *http.Server
	port                int
}

// NewServer Creates API server
func NewServer(traderManager *manager.TraderManager, st *store.Store, cryptoService *crypto.CryptoService, backtestManager *backtest.Manager, port int) *Server {
	// Set to Release mode (reduce log output)
	gin.SetMode(gin.ReleaseMode)

	router := gin.Default()

	// Enable CORS
	router.Use(corsMiddleware())

	// Create crypto handler
	cryptoHandler := NewCryptoHandler(cryptoService)

	// Create debate store and handler
	debateStore := store.NewDebateStore(st.DB())
	if err := debateStore.InitSchema(); err != nil {
		logger.Errorf("Failed to initialize debate schema: %v", err)
	}
	debateHandler := NewDebateHandler(debateStore, st.Strategy(), st.AIModel())
	debateHandler.SetTraderManager(traderManager)

	s := &Server{
		router:          router,
		traderManager:   traderManager,
		store:           st,
		cryptoHandler:   cryptoHandler,
		backtestManager: backtestManager,
		debateHandler:   debateHandler,
		port:            port,
	}

	// Setup routes
	s.setupRoutes()

	return s
}

func (s *Server) SetPositionSyncManager(positionSyncManager *trader.PositionSyncManager) {
	s.positionSyncManager = positionSyncManager
}

// corsMiddleware CORS middleware
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusOK)
			return
		}

		c.Next()
	}
}

// setupRoutes Setup routes
func (s *Server) setupRoutes() {
	// API route group
	api := s.router.Group("/api")
	{
		// Health check
		api.Any("/health", s.handleHealth)

		// Admin login (used in admin mode, public)

		// System supported models and exchanges (no authentication required)
		api.GET("/supported-models", s.handleGetSupportedModels)
		api.GET("/supported-exchanges", s.handleGetSupportedExchanges)

		// System config (no authentication required, for frontend to determine admin mode/registration status)
		api.GET("/config", s.handleGetSystemConfig)

		// Crypto related endpoints (no authentication required)
		api.GET("/crypto/config", s.cryptoHandler.HandleGetCryptoConfig)
		api.GET("/crypto/public-key", s.cryptoHandler.HandleGetPublicKey)
		api.POST("/crypto/decrypt", s.cryptoHandler.HandleDecryptSensitiveData)

		// Public competition data (no authentication required)
		api.GET("/traders", s.handlePublicTraderList)
		api.GET("/competition", s.handlePublicCompetition)
		api.GET("/top-traders", s.handleTopTraders)
		api.GET("/equity-history", s.handleEquityHistory)
		api.POST("/equity-history-batch", s.handleEquityHistoryBatch)
		api.GET("/traders/:id/public-config", s.handleGetPublicTraderConfig)

		// Dashboard 数据大屏 API (无需认证)
		s.RegisterDashboardRoutes(api)

		// Authentication related routes (no authentication required)
		api.POST("/register", s.handleRegister)
		api.POST("/login", s.handleLogin)
		api.POST("/verify-otp", s.handleVerifyOTP)
		api.POST("/complete-registration", s.handleCompleteRegistration)

		// Routes requiring authentication
		protected := api.Group("/", s.authMiddleware())
		{
			// Logout (add to blacklist)
			protected.POST("/logout", s.handleLogout)

			// Server IP query (requires authentication, for whitelist configuration)
			protected.GET("/server-ip", s.handleGetServerIP)

			// AI trader management
			protected.GET("/my-traders", s.handleTraderList)
			protected.GET("/traders/:id/config", s.handleGetTraderConfig)
			protected.POST("/traders", s.handleCreateTrader)
			protected.PUT("/traders/:id", s.handleUpdateTrader)
			protected.DELETE("/traders/:id", s.handleDeleteTrader)
			protected.POST("/traders/:id/start", s.handleStartTrader)
			protected.POST("/traders/:id/stop", s.handleStopTrader)
			protected.POST("/traders/:id/reconcile", s.handleReconcileTrader)
			protected.PUT("/traders/:id/prompt", s.handleUpdateTraderPrompt)
			protected.POST("/traders/:id/sync-balance", s.handleSyncBalance)
			protected.POST("/traders/:id/close-position", s.handleClosePosition)
			protected.PUT("/traders/:id/competition", s.handleToggleCompetition)

			// AI model configuration
			protected.GET("/models", s.handleGetModelConfigs)
			protected.PUT("/models", s.handleUpdateModelConfigs)

			// Exchange configuration
			protected.GET("/exchanges", s.handleGetExchangeConfigs)
			protected.POST("/exchanges", s.handleCreateExchange)
			protected.PUT("/exchanges", s.handleUpdateExchangeConfigs)
			protected.DELETE("/exchanges/:id", s.handleDeleteExchange)

			// Strategy management
			protected.GET("/strategies", s.handleGetStrategies)
			protected.GET("/strategies/active", s.handleGetActiveStrategy)
			protected.GET("/strategies/default-config", s.handleGetDefaultStrategyConfig)
			protected.POST("/strategies/preview-prompt", s.handlePreviewPrompt)
			protected.POST("/strategies/test-run", s.handleStrategyTestRun)
			protected.GET("/strategies/:id", s.handleGetStrategy)
			protected.POST("/strategies", s.handleCreateStrategy)
			protected.PUT("/strategies/:id", s.handleUpdateStrategy)
			protected.DELETE("/strategies/:id", s.handleDeleteStrategy)
			protected.POST("/strategies/:id/activate", s.handleActivateStrategy)
			protected.POST("/strategies/:id/duplicate", s.handleDuplicateStrategy)

			// Debate Arena
			protected.GET("/debates", s.debateHandler.HandleListDebates)
			protected.GET("/debates/personalities", s.debateHandler.HandleGetPersonalities)
			protected.GET("/debates/:id", s.debateHandler.HandleGetDebate)
			protected.POST("/debates", s.debateHandler.HandleCreateDebate)
			protected.POST("/debates/:id/start", s.debateHandler.HandleStartDebate)
			protected.POST("/debates/:id/cancel", s.debateHandler.HandleCancelDebate)
			protected.POST("/debates/:id/execute", s.debateHandler.HandleExecuteDebate)
			protected.DELETE("/debates/:id", s.debateHandler.HandleDeleteDebate)
			protected.GET("/debates/:id/messages", s.debateHandler.HandleGetMessages)
			protected.GET("/debates/:id/votes", s.debateHandler.HandleGetVotes)
			protected.GET("/debates/:id/stream", s.debateHandler.HandleDebateStream)

			// Data for specified trader (using query parameter ?trader_id=xxx)
			protected.GET("/status", s.handleStatus)
			protected.GET("/account", s.handleAccount)
			protected.GET("/positions", s.handlePositions)
			protected.GET("/decisions", s.handleDecisions)
			protected.GET("/decisions/latest", s.handleLatestDecisions)
			protected.GET("/statistics", s.handleStatistics)

			// Backtest routes
			backtest := protected.Group("/backtest")
			s.registerBacktestRoutes(backtest)

			// Copy Trade routes
			copyTradeHandler := NewCopyTradeHandler(s.store, s.traderManager)
			copyTradeHandler.RegisterRoutes(protected)

			// Reentry Advisor routes（重入 AI 助手插件）
			reentryAIHandler := NewReentryAIHandler(s.store)
			reentryAIHandler.RegisterRoutes(protected)
		}
	}
}

// handleHealth Health check
func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"time":   c.Request.Context().Value("time"),
	})
}

// handleGetSystemConfig Get system configuration (configuration that client needs to know)
func (s *Server) handleGetSystemConfig(c *gin.Context) {
	cfg := config.Get()

	c.JSON(http.StatusOK, gin.H{
		"registration_enabled": cfg.RegistrationEnabled,
		"btc_eth_leverage":     10, // Default value
		"altcoin_leverage":     5,  // Default value
	})
}

// handleGetServerIP Get server IP address (for whitelist configuration)
func (s *Server) handleGetServerIP(c *gin.Context) {
	// Try to get public IP via third-party API
	publicIP := getPublicIPFromAPI()

	// If third-party API fails, get first public IP from network interface
	if publicIP == "" {
		publicIP = getPublicIPFromInterface()
	}

	// If still cannot get it, return error
	if publicIP == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unable to get public IP address"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"public_ip": publicIP,
		"message":   "Please add this IP address to the whitelist",
	})
}

// getPublicIPFromAPI Get public IP via third-party API
func getPublicIPFromAPI() string {
	// Try multiple public IP query services
	services := []string{
		"https://api.ipify.org?format=text",
		"https://icanhazip.com",
		"https://ifconfig.me",
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	for _, service := range services {
		resp, err := client.Get(service)
		if err != nil {
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			body := make([]byte, 128)
			n, err := resp.Body.Read(body)
			if err != nil && err.Error() != "EOF" {
				continue
			}

			ip := strings.TrimSpace(string(body[:n]))
			// Verify if it's a valid IP address
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
	}

	return ""
}

// getPublicIPFromInterface Get first public IP from network interface
func getPublicIPFromInterface() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range interfaces {
		// Skip disabled interfaces and loopback interfaces
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			// Only consider IPv4 addresses
			if ip.To4() != nil {
				ipStr := ip.String()
				// Exclude private IP address ranges
				if !isPrivateIP(ip) {
					return ipStr
				}
			}
		}
	}

	return ""
}

// isPrivateIP Determine if it's a private IP address
func isPrivateIP(ip net.IP) bool {
	// Private IP address ranges:
	// 10.0.0.0/8
	// 172.16.0.0/12
	// 192.168.0.0/16
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
	}

	for _, cidr := range privateRanges {
		_, subnet, _ := net.ParseCIDR(cidr)
		if subnet.Contains(ip) {
			return true
		}
	}

	return false
}

// getTraderFromQuery Get trader from query parameter
func (s *Server) getTraderFromQuery(c *gin.Context) (*manager.TraderManager, string, error) {
	userID := c.GetString("user_id")
	traderID := c.Query("trader_id")

	// Ensure user's traders are loaded into memory
	err := s.traderManager.LoadUserTradersFromStore(s.store, userID)
	if err != nil {
		logger.Infof("⚠️ Failed to load traders for user %s: %v", userID, err)
	}

	if traderID == "" {
		// If no trader_id specified, return first trader for this user
		ids := s.traderManager.GetTraderIDs()
		if len(ids) == 0 {
			return nil, "", fmt.Errorf("No available traders")
		}

		// Get user's trader list, prioritize returning user's own traders
		userTraders, err := s.store.Trader().List(userID)
		if err == nil && len(userTraders) > 0 {
			traderID = userTraders[0].ID
		} else {
			traderID = ids[0]
		}
	}

	return s.traderManager, traderID, nil
}

// AI trader management related structures
type CreateTraderRequest struct {
	Name                string  `json:"name" binding:"required"`
	AIModelID           string  `json:"ai_model_id" binding:"required"`
	ExchangeID          string  `json:"exchange_id" binding:"required"`
	StrategyID          string  `json:"strategy_id"` // Strategy ID (new version)
	InitialBalance      float64 `json:"initial_balance"`
	ScanIntervalMinutes int     `json:"scan_interval_minutes"`
	IsCrossMargin       *bool   `json:"is_cross_margin"`     // Pointer type, nil means use default value true
	ShowInCompetition   *bool   `json:"show_in_competition"` // Pointer type, nil means use default value true
	// The following fields are kept for backward compatibility, new version uses strategy config
	BTCETHLeverage       int    `json:"btc_eth_leverage"`
	AltcoinLeverage      int    `json:"altcoin_leverage"`
	TradingSymbols       string `json:"trading_symbols"`
	CustomPrompt         string `json:"custom_prompt"`
	OverrideBasePrompt   bool   `json:"override_base_prompt"`
	SystemPromptTemplate string `json:"system_prompt_template"` // System prompt template name
	UseCoinPool          bool   `json:"use_coin_pool"`
	UseOITop             bool   `json:"use_oi_top"`
	// Copy trading configuration
	DecisionMode string         `json:"decision_mode"` // "ai" or "copy_trade"
	CopyConfig   *CopyConfigReq `json:"copy_config"`   // Copy trade config (required when decision_mode is "copy_trade")
}

// CopyConfigReq Copy trading configuration request
type CopyConfigReq struct {
	ProviderType   string  `json:"provider_type"`    // "hyperliquid" | "okx" | "binance"
	LeaderID       string  `json:"leader_id"`        // Leader wallet address or uniqueName / portfolioId (Binance)
	CopyRatio      float64 `json:"copy_ratio"`       // Copy ratio (1.0 = 100%)
	SyncLeverage   bool    `json:"sync_leverage"`    // Whether to sync leverage
	SyncMarginMode *bool   `json:"sync_margin_mode"` // Whether to sync margin mode (OKX: cross/isolated)
	// 最小/最大跟单金额阈值（USDT）。指针区分"未传"（保留存量/默认值）与
	// 显式 0（max=0 表示不预警）。此前请求结构缺这两个字段，前端传值被
	// JSON unmarshal 静默丢弃，配置形同虚设（M18）。
	MinTradeWarn             *float64 `json:"min_trade_warn,omitempty"`
	MaxTradeWarn             *float64 `json:"max_trade_warn,omitempty"`
	CopyCatchupWindowSeconds *int     `json:"copy_catchup_window_seconds,omitempty"`
	CopyCatchupMaxAdverseBPS *float64 `json:"copy_catchup_max_adverse_bps,omitempty"`

	// Binance Web 凭证（仅 ProviderType=binance 时使用，明文存储）
	BinanceP20T        string `json:"binance_p20t,omitempty"`       // 登录 cookie p20t
	BinanceCSRFToken   string `json:"binance_csrf_token,omitempty"` // CSRF header csrftoken
	BinanceSourceMode  string `json:"binance_source_mode,omitempty"`
	BinanceTopTraderID string `json:"binance_top_trader_id,omitempty"`

	// Copy Guard v7 风控与 AI guarded 重入配置
	// 字段含义详见 store.CopyTradeConfig
	// 开关字段用 *bool 区分"未传"(nil) 和"显式 false"（避免 Go JSON 零值歧义）
	// 可合法取 0 的数值字段使用指针，区分“未传”和“显式 0”；其余正数
	// 参数保留值类型并由统一默认工厂兜底。
	// v3 遗留字段（risk_atr_enabled / risk_reentry_tolerance / 反加仓铁律 /
	// risk_stop_noise_floor_atr / risk_cycle_max_loss_pct）已随 v5 下线
	RiskStopLossEnabled        *bool    `json:"risk_stop_loss_enabled,omitempty"`
	RiskStopMaxAccountLossPct  *float64 `json:"risk_stop_max_account_loss_pct,omitempty"`
	RiskAccountPct             float64  `json:"risk_account_pct,omitempty"`
	RiskATRMultiplier          float64  `json:"risk_atr_multiplier,omitempty"`
	RiskATRTimeframe           string   `json:"risk_atr_timeframe,omitempty"`
	RiskLeverageFallback       *bool    `json:"risk_leverage_fallback,omitempty"`
	RiskLeverageMaxLoss        float64  `json:"risk_leverage_max_loss,omitempty"`
	RiskReentryEnabled         *bool    `json:"risk_reentry_enabled,omitempty"`
	RiskReentryRatio           float64  `json:"risk_reentry_ratio,omitempty"`
	RiskReentryDecisionMode    *string  `json:"risk_reentry_decision_mode,omitempty"`
	RiskReentryMinNotional     *float64 `json:"risk_reentry_min_notional,omitempty"`
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

	RiskPolicyVersion           int      `json:"risk_policy_version,omitempty"`
	RiskStopMode                string   `json:"risk_stop_mode,omitempty"`
	RiskStopPriority            string   `json:"risk_stop_priority,omitempty"`
	RiskATRPeriod               int      `json:"risk_atr_period,omitempty"`
	RiskATRCacheMaxAgeMinutes   int      `json:"risk_atr_cache_max_age_minutes,omitempty"`
	RiskATRFallbackPct          float64  `json:"risk_atr_fallback_pct,omitempty"`
	RiskTriggerPriceType        string   `json:"risk_trigger_price_type,omitempty"`
	RiskSlippageBufferBPS       *float64 `json:"risk_slippage_buffer_bps,omitempty"`
	RiskLiquidationBufferATR    *float64 `json:"risk_liquidation_buffer_atr,omitempty"`
	RiskMaxReentries            *int     `json:"risk_max_reentries,omitempty"`
	RiskReentryBandATR          *float64 `json:"risk_reentry_band_atr,omitempty"`
	RiskReentryCooldownSeconds  *int     `json:"risk_reentry_cooldown_seconds,omitempty"`
	RiskReentryMaxChaseATR      *float64 `json:"risk_reentry_max_chase_atr,omitempty"`
	RiskReentryMaxATRExpansion  float64  `json:"risk_reentry_max_atr_expansion,omitempty"`
	RiskWatchTimeoutMinutes     *int     `json:"risk_watch_timeout_minutes,omitempty"`
	RiskMigrationConfirmed      *bool    `json:"risk_migration_confirmed,omitempty"`
	RiskAddonBudgetPct          *float64 `json:"risk_addon_budget_pct,omitempty"`
	RiskHighRiskConfirmed       bool     `json:"risk_high_risk_confirmed,omitempty"`
	RiskExtremeConfirmValue     *float64 `json:"risk_extreme_risk_confirm_value,omitempty"`
	RiskStopExtremeConfirmValue *float64 `json:"risk_stop_extreme_confirm_value,omitempty"`

	// v4.1 重入加严（缺失时兼容旧默认；显式零值可关闭恢复幅度门槛）
	RiskReentryMinRecoveryATR     *float64 `json:"risk_reentry_min_recovery_atr,omitempty"`
	RiskReentryCooldownEscalation float64  `json:"risk_reentry_cooldown_escalation,omitempty"`
	RiskReentryRecoveryEscalation float64  `json:"risk_reentry_recovery_escalation,omitempty"`

	// v5 可保护性状态机 / 噪音档重入
	RiskUnprotectableDisposition string `json:"risk_unprotectable_disposition,omitempty"`
	RiskUnprotectableAction      string `json:"risk_unprotectable_action,omitempty"`
	RiskReentryNoiseOverride     *bool  `json:"risk_reentry_noise_override,omitempty"`
}

// applyCopyConfigRiskFields 把 CopyConfigReq 中的 Copy Guard 风控字段透传到 store.CopyTradeConfig
//
// 设计：开关字段未传时(nil)保留统一默认；允许显式 false。可合法为 0 的
// 数值字段使用指针，允许显式 0，其余字段由 FillRiskDefaults 兜底。
//
// 调用点：Create handler / Update handler 内部，构造 copyConfig 后调用一次。
// 复用理由：两个 handler 透传逻辑完全一致，提取避免重复
func applyCopyConfigRiskFields(copyConfig *store.CopyTradeConfig, req *CopyConfigReq) bool {
	if copyConfig == nil || req == nil {
		return false
	}
	// Retired fields remain readable for old clients, but every write path
	// normalizes them before provider-specific early returns.
	manualDeprecated := applyRetiredCopyGuardCompatibility(copyConfig, req.RiskManualReentryEnabled)
	applyUnprotectableCompatibility(copyConfig, req.RiskUnprotectableDisposition, req.RiskUnprotectableAction, req.RiskUnprotectableAction != "")
	// Copy Guard（risk_policy_version >= 4）支持 OKX 与 Binance 领航员数据源。
	// 不支持的数据源（Hyperliquid）对所有 Copy Guard 风控字段无意义，若不剥离
	// risk_policy_version，后续 "v4 only for OKX/Binance" 校验会把其正常跟单
	// 保存整体 400 拒绝。语义：不支持的数据源忽略全部风控字段，只保留基础跟单配置。
	if !copytrade.SupportsCopyGuard(copytrade.ProviderType(copyConfig.ProviderType)) {
		copyConfig.RiskPolicyVersion = 0
		return manualDeprecated
	}
	// 开关：nil 用合理默认；非 nil 用 *值
	copyConfig.RiskStopLossEnabled = derefBoolDefault(req.RiskStopLossEnabled, copyConfig.RiskStopLossEnabled)
	if req.RiskStopMaxAccountLossPct != nil {
		copyConfig.RiskStopMaxAccountLossPct = *req.RiskStopMaxAccountLossPct
	}
	// 仓位初始保证金亏损上限由统一默认工厂初始化为开启 50%；指针仍允许
	// 用户显式关闭，历史配置则由带版本的数据库迁移保持原有选择。
	copyConfig.RiskLeverageFallback = derefBoolDefault(req.RiskLeverageFallback, copyConfig.RiskLeverageFallback)
	copyConfig.RiskReentryEnabled = derefBoolDefault(req.RiskReentryEnabled, copyConfig.RiskReentryEnabled)
	copyConfig.RiskReentryNoiseOverride = derefBoolDefault(req.RiskReentryNoiseOverride, copyConfig.RiskReentryNoiseOverride)
	// 旧请求结构中部分数值不是指针；零值只能表示“未传”，因此仅用非零值
	// 覆盖已经由统一默认工厂或存量配置提供的值。
	if req.RiskAccountPct != 0 {
		copyConfig.RiskAccountPct = req.RiskAccountPct
	}
	if req.RiskATRMultiplier != 0 {
		copyConfig.RiskATRMultiplier = req.RiskATRMultiplier
	}
	if req.RiskATRTimeframe != "" {
		copyConfig.RiskATRTimeframe = req.RiskATRTimeframe
	}
	if req.RiskLeverageMaxLoss != 0 {
		copyConfig.RiskLeverageMaxLoss = req.RiskLeverageMaxLoss
	}
	if req.RiskReentryRatio != 0 {
		copyConfig.RiskReentryRatio = req.RiskReentryRatio
	}
	if req.RiskPolicyVersion > 0 {
		copyConfig.RiskPolicyVersion = req.RiskPolicyVersion
	}
	if req.RiskStopMode != "" {
		copyConfig.RiskStopMode = req.RiskStopMode
	}
	if req.RiskStopPriority != "" {
		copyConfig.RiskStopPriority = req.RiskStopPriority
	}
	if req.RiskATRPeriod != 0 {
		copyConfig.RiskATRPeriod = req.RiskATRPeriod
	}
	if req.RiskATRCacheMaxAgeMinutes != 0 {
		copyConfig.RiskATRCacheMaxAgeMinutes = req.RiskATRCacheMaxAgeMinutes
	}
	if req.RiskATRFallbackPct != 0 {
		copyConfig.RiskATRFallbackPct = req.RiskATRFallbackPct
	}
	if req.RiskTriggerPriceType != "" {
		copyConfig.RiskTriggerPriceType = req.RiskTriggerPriceType
	}
	copyConfig.RiskSlippageBufferBPS = derefFloatDefault(req.RiskSlippageBufferBPS, copyConfig.RiskSlippageBufferBPS)
	copyConfig.RiskLiquidationBufferATR = derefFloatDefault(req.RiskLiquidationBufferATR, copyConfig.RiskLiquidationBufferATR)
	// v5 代次 5：默认单周期最多重入 2 次（确认式重入的结构性门槛——连续确认/
	// 恢复幅度与冷却逐次加严/噪音档禁入/可保护性预检——已足够约束坏重入，
	// 名义按 ratio 几何衰减保证累计风险有界）
	copyConfig.RiskMaxReentries = derefIntDefault(req.RiskMaxReentries, copyConfig.RiskMaxReentries)
	copyConfig.RiskReentryBandATR = derefFloatDefault(req.RiskReentryBandATR, copyConfig.RiskReentryBandATR)
	// v4.1：默认冷却 300s（旧默认 60s 在高杠杆震荡下重入过快）
	copyConfig.RiskReentryCooldownSeconds = derefIntDefault(req.RiskReentryCooldownSeconds, copyConfig.RiskReentryCooldownSeconds)
	copyConfig.RiskReentryMaxChaseATR = derefFloatDefault(req.RiskReentryMaxChaseATR, copyConfig.RiskReentryMaxChaseATR)
	if req.RiskReentryMaxATRExpansion != 0 {
		copyConfig.RiskReentryMaxATRExpansion = req.RiskReentryMaxATRExpansion
	}
	copyConfig.RiskWatchTimeoutMinutes = derefIntDefault(req.RiskWatchTimeoutMinutes, copyConfig.RiskWatchTimeoutMinutes)
	copyConfig.RiskMigrationConfirmed = derefBoolDefault(req.RiskMigrationConfirmed, copyConfig.RiskMigrationConfirmed)
	if req.RiskAddonBudgetPct != nil {
		copyConfig.RiskAddonBudgetPct = *req.RiskAddonBudgetPct
	}
	// v4.1 重入加严：字段缺失沿用旧值/默认值；显式 0 表示关闭该恢复幅度门槛。
	if req.RiskReentryMinRecoveryATR != nil {
		copyConfig.RiskReentryMinRecoveryATR = *req.RiskReentryMinRecoveryATR
		copyConfig.RiskReentryMinRecoveryATRExplicit = true
	}
	if req.RiskReentryCooldownEscalation != 0 {
		copyConfig.RiskReentryCooldownEscalation = req.RiskReentryCooldownEscalation
	}
	if req.RiskReentryRecoveryEscalation != 0 {
		copyConfig.RiskReentryRecoveryEscalation = req.RiskReentryRecoveryEscalation
	}
	if req.RiskReentryDecisionMode != nil {
		copyConfig.RiskReentryDecisionMode = *req.RiskReentryDecisionMode
	}
	if req.RiskReentryMinNotional != nil {
		copyConfig.RiskReentryMinNotional = *req.RiskReentryMinNotional
	}
	if req.RiskCycleLossBudgetPct != nil {
		copyConfig.RiskCycleLossBudgetPct = *req.RiskCycleLossBudgetPct
	}
	if req.RiskPortfolioLossBudgetPct != nil {
		copyConfig.RiskPortfolioLossBudgetPct = *req.RiskPortfolioLossBudgetPct
	}
	if req.RiskRoundTripFeeBPS != nil {
		copyConfig.RiskRoundTripFeeBPS = *req.RiskRoundTripFeeBPS
	}
	if req.RiskAIConfidenceThreshold != nil {
		copyConfig.RiskAIConfidenceThreshold = *req.RiskAIConfidenceThreshold
	}
	if req.RiskAIMinReviewSeconds != nil {
		copyConfig.RiskAIMinReviewSeconds = *req.RiskAIMinReviewSeconds
	}
	if req.RiskAIDailyCallLimit != nil {
		copyConfig.RiskAIDailyCallLimit = *req.RiskAIDailyCallLimit
	}
	if req.RiskAILifecycleCallLimit != nil {
		copyConfig.RiskAILifecycleCallLimit = *req.RiskAILifecycleCallLimit
	}
	if req.RiskNotificationLevel != nil {
		copyConfig.RiskNotificationLevel = *req.RiskNotificationLevel
	}
	copyConfig.FillRiskDefaults()
	return manualDeprecated
}

// derefBoolDefault 安全解引用 *bool：nil 返回 def，非 nil 返回 *p
// 用于 JSON 可选字段：nil 表示"未传"，*p=true/false 表示"显式传值"
func derefBoolDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func derefFloatDefault(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

func derefIntDefault(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// resolveCredentialUpdate 凭证字段保存语义：
// 传空或掩码值（含 "***"，即 GET 接口脱敏后原样回传）表示"不修改"，保留旧值。
// 注意：该语义下无法通过传空字符串清除凭证（改用新值覆盖即可，全局凭证是主路径）。
func resolveCredentialUpdate(incoming, existing string) string {
	incoming = strings.TrimSpace(incoming)
	if incoming == "" || strings.Contains(incoming, "***") {
		return existing
	}
	return incoming
}

type ModelConfig struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	Enabled      bool   `json:"enabled"`
	APIKey       string `json:"apiKey,omitempty"`
	CustomAPIURL string `json:"customApiUrl,omitempty"`
}

// SafeModelConfig Safe model configuration structure (does not contain sensitive information)
type SafeModelConfig struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	Enabled         bool   `json:"enabled"`
	CustomAPIURL    string `json:"customApiUrl"`    // Custom API URL (usually not sensitive)
	CustomModelName string `json:"customModelName"` // Custom model name (not sensitive)
}

type ExchangeConfig struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"` // "cex" or "dex"
	Enabled   bool   `json:"enabled"`
	APIKey    string `json:"apiKey,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
	Testnet   bool   `json:"testnet,omitempty"`
}

// SafeExchangeConfig Safe exchange configuration structure (does not contain sensitive information)
type SafeExchangeConfig struct {
	ID                    string `json:"id"`            // UUID
	ExchangeType          string `json:"exchange_type"` // "binance", "bybit", "okx", "hyperliquid", "aster", "lighter"
	AccountName           string `json:"account_name"`  // User-defined account name
	Name                  string `json:"name"`          // Display name
	Type                  string `json:"type"`          // "cex" or "dex"
	Enabled               bool   `json:"enabled"`
	Testnet               bool   `json:"testnet,omitempty"`
	HyperliquidWalletAddr string `json:"hyperliquidWalletAddr"` // Hyperliquid wallet address (not sensitive)
	AsterUser             string `json:"asterUser"`             // Aster username (not sensitive)
	AsterSigner           string `json:"asterSigner"`           // Aster signer (not sensitive)
	LighterWalletAddr     string `json:"lighterWalletAddr"`     // LIGHTER wallet address (not sensitive)
}

type UpdateModelConfigRequest struct {
	Models map[string]struct {
		Enabled         bool   `json:"enabled"`
		APIKey          string `json:"api_key"`
		CustomAPIURL    string `json:"custom_api_url"`
		CustomModelName string `json:"custom_model_name"`
	} `json:"models"`
}

type UpdateExchangeConfigRequest struct {
	Exchanges map[string]struct {
		Enabled                 bool   `json:"enabled"`
		APIKey                  string `json:"api_key"`
		SecretKey               string `json:"secret_key"`
		Passphrase              string `json:"passphrase"` // OKX specific
		Testnet                 bool   `json:"testnet"`
		HyperliquidWalletAddr   string `json:"hyperliquid_wallet_addr"`
		AsterUser               string `json:"aster_user"`
		AsterSigner             string `json:"aster_signer"`
		AsterPrivateKey         string `json:"aster_private_key"`
		LighterWalletAddr       string `json:"lighter_wallet_addr"`
		LighterPrivateKey       string `json:"lighter_private_key"`
		LighterAPIKeyPrivateKey string `json:"lighter_api_key_private_key"`
		LighterAPIKeyIndex      int    `json:"lighter_api_key_index"`
	} `json:"exchanges"`
}

// handleCreateTrader Create new AI trader
func (s *Server) handleCreateTrader(c *gin.Context) {
	userID := c.GetString("user_id")
	var req CreateTraderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.DecisionMode == "copy_trade" && req.CopyConfig != nil && req.CopyConfig.RiskReentryDecisionMode != nil && *req.CopyConfig.RiskReentryDecisionMode == "legacy_rule" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "legacy_rule is retired for new traders; use ai_guarded or disabled"})
		return
	}
	if req.DecisionMode == "copy_trade" && req.CopyConfig != nil {
		candidate := &store.CopyTradeConfig{
			ProviderType: req.CopyConfig.ProviderType, LeaderID: req.CopyConfig.LeaderID,
			BinanceSourceMode: req.CopyConfig.BinanceSourceMode, BinanceTopTraderID: req.CopyConfig.BinanceTopTraderID,
		}
		if err := prepareCopyTradeSource(nil, candidate, nil); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.CopyConfig.LeaderID = candidate.LeaderID
		req.CopyConfig.BinanceSourceMode = candidate.BinanceSourceMode
		req.CopyConfig.BinanceTopTraderID = candidate.BinanceTopTraderID
	}

	// Validate leverage values
	if req.BTCETHLeverage < 0 || req.BTCETHLeverage > 50 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "BTC/ETH leverage must be between 1-50x"})
		return
	}
	if req.AltcoinLeverage < 0 || req.AltcoinLeverage > 20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Altcoin leverage must be between 1-20x"})
		return
	}

	// Validate trading symbol format
	if req.TradingSymbols != "" {
		symbols := strings.Split(req.TradingSymbols, ",")
		for _, symbol := range symbols {
			symbol = strings.TrimSpace(symbol)
			if symbol != "" && !strings.HasSuffix(strings.ToUpper(symbol), "USDT") {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid symbol format: %s, must end with USDT", symbol)})
				return
			}
		}
	}

	// Generate trader ID (use short UUID prefix for readability)
	exchangeIDShort := req.ExchangeID
	if len(exchangeIDShort) > 8 {
		exchangeIDShort = exchangeIDShort[:8]
	}
	traderID := fmt.Sprintf("%s_%s_%d", exchangeIDShort, req.AIModelID, time.Now().Unix())

	// Set default values
	isCrossMargin := true // Default to cross margin mode
	if req.IsCrossMargin != nil {
		isCrossMargin = *req.IsCrossMargin
	}

	showInCompetition := true // Default to show in competition
	if req.ShowInCompetition != nil {
		showInCompetition = *req.ShowInCompetition
	}

	// Set leverage default values
	btcEthLeverage := 10 // Default value
	altcoinLeverage := 5 // Default value
	if req.BTCETHLeverage > 0 {
		btcEthLeverage = req.BTCETHLeverage
	}
	if req.AltcoinLeverage > 0 {
		altcoinLeverage = req.AltcoinLeverage
	}

	// Set system prompt template default value
	systemPromptTemplate := "default"
	if req.SystemPromptTemplate != "" {
		systemPromptTemplate = req.SystemPromptTemplate
	}

	// Set scan interval default value
	scanIntervalMinutes := req.ScanIntervalMinutes
	if scanIntervalMinutes < 3 {
		scanIntervalMinutes = 3 // Default 3 minutes, not allowed to be less than 3
	}

	// Query exchange actual balance, override user input
	actualBalance := req.InitialBalance // Default to use user input
	exchanges, err := s.store.Exchange().List(userID)
	if err != nil {
		logger.Infof("⚠️ Failed to get exchange config, using user input for initial balance: %v", err)
	}

	// Find matching exchange configuration
	var exchangeCfg *store.Exchange
	for _, ex := range exchanges {
		if ex.ID == req.ExchangeID {
			exchangeCfg = ex
			break
		}
	}

	if exchangeCfg == nil {
		logger.Infof("⚠️ Exchange %s configuration not found, using user input for initial balance", req.ExchangeID)
	} else if !exchangeCfg.Enabled {
		logger.Infof("⚠️ Exchange %s not enabled, using user input for initial balance", req.ExchangeID)
	} else {
		// Create temporary trader based on exchange type to query balance
		var tempTrader trader.Trader
		var createErr error

		// Use ExchangeType (e.g., "binance") instead of ID (UUID)
		switch exchangeCfg.ExchangeType {
		case "binance":
			tempTrader = trader.NewFuturesTrader(exchangeCfg.APIKey, exchangeCfg.SecretKey, userID)
		case "hyperliquid":
			tempTrader, createErr = trader.NewHyperliquidTrader(
				exchangeCfg.APIKey, // private key
				exchangeCfg.HyperliquidWalletAddr,
				exchangeCfg.Testnet,
			)
		case "aster":
			tempTrader, createErr = trader.NewAsterTrader(
				exchangeCfg.AsterUser,
				exchangeCfg.AsterSigner,
				exchangeCfg.AsterPrivateKey,
			)
		case "bybit":
			tempTrader = trader.NewBybitTrader(
				exchangeCfg.APIKey,
				exchangeCfg.SecretKey,
			)
		case "okx":
			tempTrader = trader.NewOKXTrader(
				exchangeCfg.APIKey,
				exchangeCfg.SecretKey,
				exchangeCfg.Passphrase,
			)
		case "bitget":
			tempTrader = trader.NewBitgetTrader(
				exchangeCfg.APIKey,
				exchangeCfg.SecretKey,
				exchangeCfg.Passphrase,
			)
		case "lighter":
			if exchangeCfg.LighterWalletAddr != "" && exchangeCfg.LighterAPIKeyPrivateKey != "" {
				// Lighter only supports mainnet
				tempTrader, createErr = trader.NewLighterTraderV2(
					exchangeCfg.LighterWalletAddr,
					exchangeCfg.LighterAPIKeyPrivateKey,
					exchangeCfg.LighterAPIKeyIndex,
					false, // Always use mainnet for Lighter
				)
			} else {
				createErr = fmt.Errorf("Lighter requires wallet address and API Key private key")
			}
		default:
			logger.Infof("⚠️ Unsupported exchange type: %s, using user input for initial balance", exchangeCfg.ExchangeType)
		}

		if createErr != nil {
			logger.Infof("⚠️ Failed to create temporary trader, using user input for initial balance: %v", createErr)
		} else if tempTrader != nil {
			// Query actual balance
			balanceInfo, balanceErr := tempTrader.GetBalance()
			if balanceErr != nil {
				logger.Infof("⚠️ Failed to query exchange balance, using user input for initial balance: %v", balanceErr)
			} else {
				// Extract total equity (account total value = wallet balance + unrealized PnL)
				// Priority: total_equity > totalWalletBalance > wallet_balance > totalEq > balance
				// Note: Must use total_equity (not availableBalance) for accurate P&L calculation
				balanceKeys := []string{"total_equity", "totalWalletBalance", "wallet_balance", "totalEq", "balance"}
				for _, key := range balanceKeys {
					if balance, ok := balanceInfo[key].(float64); ok && balance > 0 {
						actualBalance = balance
						logger.Infof("✓ Queried exchange total equity (%s): %.2f USDT (user input: %.2f USDT)", key, actualBalance, req.InitialBalance)
						break
					}
				}
				if actualBalance <= 0 {
					logger.Infof("⚠️ Unable to extract total equity from balance info, balanceInfo=%v, using user input for initial balance", balanceInfo)
				}
			}
		}
	}

	// Create trader configuration (database entity)
	logger.Infof("🔧 DEBUG: Starting to create trader config, ID=%s, Name=%s, AIModel=%s, Exchange=%s, StrategyID=%s", traderID, req.Name, req.AIModelID, req.ExchangeID, req.StrategyID)
	traderRecord := &store.Trader{
		ID:                   traderID,
		UserID:               userID,
		Name:                 req.Name,
		AIModelID:            req.AIModelID,
		ExchangeID:           req.ExchangeID,
		StrategyID:           req.StrategyID, // Associated strategy ID (new version)
		InitialBalance:       actualBalance,  // Use actual queried balance
		BTCETHLeverage:       btcEthLeverage,
		AltcoinLeverage:      altcoinLeverage,
		TradingSymbols:       req.TradingSymbols,
		UseCoinPool:          req.UseCoinPool,
		UseOITop:             req.UseOITop,
		CustomPrompt:         req.CustomPrompt,
		OverrideBasePrompt:   req.OverrideBasePrompt,
		SystemPromptTemplate: systemPromptTemplate,
		IsCrossMargin:        isCrossMargin,
		ShowInCompetition:    showInCompetition,
		ScanIntervalMinutes:  scanIntervalMinutes,
		IsRunning:            false,
	}

	// Save to database
	logger.Infof("🔧 DEBUG: Preparing to call CreateTrader")
	err = s.store.Trader().Create(traderRecord)
	if err != nil {
		logger.Infof("❌ Failed to create trader: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to create trader: %v", err)})
		return
	}
	logger.Infof("🔧 DEBUG: CreateTrader succeeded")

	// Handle copy trading configuration
	decisionMode := req.DecisionMode
	if decisionMode == "" {
		decisionMode = "ai" // Default to AI mode
	}
	manualReentryDeprecated := false

	// Update decision_mode in database
	if err := s.store.CopyTrade().UpdateDecisionMode(traderID, decisionMode); err != nil {
		logger.Infof("⚠️ Failed to update decision mode: %v", err)
	}

	// If copy_trade mode, create copy trade config
	if decisionMode == "copy_trade" && req.CopyConfig != nil {
		// SyncMarginMode 默认为 true（同步领航员的全仓/逐仓模式）
		syncMarginMode := true
		if req.CopyConfig.SyncMarginMode != nil {
			syncMarginMode = *req.CopyConfig.SyncMarginMode
		}

		copyConfig := store.NewCopyGuardDefaults()
		copyConfig.TraderID = traderID
		copyConfig.ProviderType = req.CopyConfig.ProviderType
		copyConfig.LeaderID = req.CopyConfig.LeaderID
		copyConfig.CopyRatio = req.CopyConfig.CopyRatio
		copyConfig.SyncLeverage = req.CopyConfig.SyncLeverage
		copyConfig.SyncMarginMode = syncMarginMode
		if req.CopyConfig.MinTradeWarn != nil {
			copyConfig.MinTradeWarn = *req.CopyConfig.MinTradeWarn
		}
		if req.CopyConfig.MaxTradeWarn != nil {
			copyConfig.MaxTradeWarn = *req.CopyConfig.MaxTradeWarn
		}
		if req.CopyConfig.CopyCatchupWindowSeconds != nil {
			copyConfig.CopyCatchupWindowSeconds = *req.CopyConfig.CopyCatchupWindowSeconds
		}
		if req.CopyConfig.CopyCatchupMaxAdverseBPS != nil {
			copyConfig.CopyCatchupMaxAdverseBPS = *req.CopyConfig.CopyCatchupMaxAdverseBPS
		}
		copyConfig.Enabled = false
		copyConfig.BinanceP20T = req.CopyConfig.BinanceP20T
		copyConfig.BinanceCSRFToken = req.CopyConfig.BinanceCSRFToken
		copyConfig.BinanceSourceMode = req.CopyConfig.BinanceSourceMode
		copyConfig.BinanceTopTraderID = req.CopyConfig.BinanceTopTraderID
		if err := prepareCopyTradeSource(s.store, copyConfig, nil); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// v3 风控字段透传（修复 bug：之前 CopyConfigReq 无 risk_* 字段，前端传值被 JSON unmarshal 静默丢弃）
		manualReentryDeprecated = applyCopyConfigRiskFields(copyConfig, req.CopyConfig)
		if err := copytrade.ValidateStoredRiskPolicy(copyConfig); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if copyConfig.RiskPolicyVersion >= 4 && !copytrade.SupportsCopyGuard(copytrade.ProviderType(copyConfig.ProviderType)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "copy guard v4 is only supported for OKX or Binance leader sources"})
			return
		}
		if copyConfig.RiskPolicyVersion >= 4 {
			if err := validateRiskConfirmation(copyConfig.RiskAccountPct, req.CopyConfig.RiskHighRiskConfirmed, req.CopyConfig.RiskExtremeConfirmValue); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if copyConfig.RiskStopPriority == "account_cap" {
				if err := validateStopLossPctConfirmation(copyConfig.RiskStopMaxAccountLossPct, req.CopyConfig.RiskHighRiskConfirmed, req.CopyConfig.RiskStopExtremeConfirmValue); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
					return
				}
			}
		}
		if err := validateAIGuardedPrerequisites(s.store, copyConfig); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Validate required fields
		if copyConfig.ProviderType == "" || copyConfig.LeaderID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Copy trade config requires provider_type and leader_id"})
			return
		}

		// Default copy ratio to 1.0 (100%)
		if copyConfig.CopyRatio <= 0 {
			copyConfig.CopyRatio = 1.0
		}
		if copyConfig.CopyRatio > maxCopyRatio {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("copy_ratio %.2f exceeds max %.0f", copyConfig.CopyRatio, maxCopyRatio)})
			return
		}

		if err := s.store.CopyTrade().Upsert(copyConfig); err != nil {
			// 必须显式失败：静默吞掉会造成"交易员已创建但跟单配置缺失"的
			// 半成品状态，启动跟单时才暴露且难以定位。Upsert 幂等，用户重试即可。
			logger.Errorf("❌ Failed to create copy trade config: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "trader created but copy trade config save failed, please save again: " + err.Error()})
			return
		}
		disableReentryCandidatesAfterConfigSave(s.store, nil, copyConfig)
		logger.Infof("✓ Copy trade config created: provider=%s, leader=%s, ratio=%.0f%% (risk SL=%v reentry=%v)",
			copyConfig.ProviderType, copyConfig.LeaderID, copyConfig.CopyRatio*100,
			copyConfig.RiskStopLossEnabled, copyConfig.RiskReentryEnabled)
	}

	// Immediately load new trader into TraderManager
	logger.Infof("🔧 DEBUG: Preparing to call LoadUserTraders")
	err = s.traderManager.LoadUserTradersFromStore(s.store, userID)
	if err != nil {
		logger.Infof("⚠️ Failed to load user traders into memory: %v", err)
		// Don't return error here since trader was successfully created in database
	}
	logger.Infof("🔧 DEBUG: LoadUserTraders completed")

	logger.Infof("✓ Trader created successfully: %s (model: %s, exchange: %s, mode: %s)", req.Name, req.AIModelID, req.ExchangeID, decisionMode)

	response := gin.H{
		"trader_id":     traderID,
		"trader_name":   req.Name,
		"ai_model":      req.AIModelID,
		"is_running":    false,
		"decision_mode": decisionMode,
	}
	if manualReentryDeprecated {
		response["deprecated"] = manualReentryDeprecatedMessage
	}
	c.JSON(http.StatusCreated, response)
}

// UpdateTraderRequest Update trader request
type UpdateTraderRequest struct {
	Name                string  `json:"name" binding:"required"`
	AIModelID           string  `json:"ai_model_id" binding:"required"`
	ExchangeID          string  `json:"exchange_id" binding:"required"`
	StrategyID          string  `json:"strategy_id"` // Strategy ID (new version)
	InitialBalance      float64 `json:"initial_balance"`
	ScanIntervalMinutes int     `json:"scan_interval_minutes"`
	IsCrossMargin       *bool   `json:"is_cross_margin"`
	ShowInCompetition   *bool   `json:"show_in_competition"`
	// The following fields are kept for backward compatibility, new version uses strategy config
	BTCETHLeverage       int    `json:"btc_eth_leverage"`
	AltcoinLeverage      int    `json:"altcoin_leverage"`
	TradingSymbols       string `json:"trading_symbols"`
	CustomPrompt         string `json:"custom_prompt"`
	OverrideBasePrompt   bool   `json:"override_base_prompt"`
	SystemPromptTemplate string `json:"system_prompt_template"`
	// Copy trading configuration
	DecisionMode string         `json:"decision_mode"` // "ai" or "copy_trade"
	CopyConfig   *CopyConfigReq `json:"copy_config"`   // Copy trade config
}

// handleUpdateTrader Update trader configuration
func (s *Server) handleUpdateTrader(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("id")

	var req UpdateTraderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check if trader exists and belongs to current user
	traders, err := s.store.Trader().List(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get trader list"})
		return
	}

	var existingTrader *store.Trader
	for _, t := range traders {
		if t.ID == traderID {
			existingTrader = t
			break
		}
	}

	if existingTrader == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trader does not exist"})
		return
	}
	if req.DecisionMode == "copy_trade" && req.CopyConfig != nil {
		existingCopyCfg, _ := s.store.CopyTrade().GetByTraderID(traderID)
		candidate := store.NewCopyGuardDefaults()
		if existingCopyCfg != nil {
			clone := *existingCopyCfg
			candidate = &clone
		}
		candidate.TraderID = traderID
		candidate.ProviderType = req.CopyConfig.ProviderType
		candidate.LeaderID = req.CopyConfig.LeaderID
		candidate.BinanceSourceMode = req.CopyConfig.BinanceSourceMode
		candidate.BinanceTopTraderID = req.CopyConfig.BinanceTopTraderID
		if err := prepareCopyTradeSource(s.store, candidate, existingCopyCfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.CopyConfig.LeaderID = candidate.LeaderID
		req.CopyConfig.BinanceSourceMode = candidate.BinanceSourceMode
		req.CopyConfig.BinanceTopTraderID = candidate.BinanceTopTraderID
	}

	// Set default values
	isCrossMargin := existingTrader.IsCrossMargin // Keep original value
	if req.IsCrossMargin != nil {
		isCrossMargin = *req.IsCrossMargin
	}

	showInCompetition := existingTrader.ShowInCompetition // Keep original value
	if req.ShowInCompetition != nil {
		showInCompetition = *req.ShowInCompetition
	}

	// Set leverage default values
	btcEthLeverage := req.BTCETHLeverage
	altcoinLeverage := req.AltcoinLeverage
	if btcEthLeverage <= 0 {
		btcEthLeverage = existingTrader.BTCETHLeverage // Keep original value
	}
	if altcoinLeverage <= 0 {
		altcoinLeverage = existingTrader.AltcoinLeverage // Keep original value
	}

	// Set scan interval, allow updates
	scanIntervalMinutes := req.ScanIntervalMinutes
	if scanIntervalMinutes <= 0 {
		scanIntervalMinutes = existingTrader.ScanIntervalMinutes // Keep original value
	} else if scanIntervalMinutes < 3 {
		scanIntervalMinutes = 3
	}

	// Set system prompt template
	systemPromptTemplate := req.SystemPromptTemplate
	if systemPromptTemplate == "" {
		systemPromptTemplate = existingTrader.SystemPromptTemplate // Keep original value
	}

	// Handle strategy ID (if not provided, keep original value)
	strategyID := req.StrategyID
	if strategyID == "" {
		strategyID = existingTrader.StrategyID
	}

	// Update trader configuration
	traderRecord := &store.Trader{
		ID:                   traderID,
		UserID:               userID,
		Name:                 req.Name,
		AIModelID:            req.AIModelID,
		ExchangeID:           req.ExchangeID,
		StrategyID:           strategyID, // Associated strategy ID
		InitialBalance:       req.InitialBalance,
		BTCETHLeverage:       btcEthLeverage,
		AltcoinLeverage:      altcoinLeverage,
		TradingSymbols:       req.TradingSymbols,
		CustomPrompt:         req.CustomPrompt,
		OverrideBasePrompt:   req.OverrideBasePrompt,
		SystemPromptTemplate: systemPromptTemplate,
		IsCrossMargin:        isCrossMargin,
		ShowInCompetition:    showInCompetition,
		ScanIntervalMinutes:  scanIntervalMinutes,
		IsRunning:            existingTrader.IsRunning, // Keep original value
	}

	// Update database
	logger.Infof("🔄 Updating trader: ID=%s, Name=%s, AIModelID=%s, StrategyID=%s, req.StrategyID=%s",
		traderRecord.ID, traderRecord.Name, traderRecord.AIModelID, traderRecord.StrategyID, req.StrategyID)
	err = s.store.Trader().Update(traderRecord)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to update trader: %v", err)})
		return
	}

	// Handle copy trading configuration update
	decisionMode := req.DecisionMode
	manualReentryDeprecated := false
	if decisionMode != "" {
		// Update decision_mode in database
		if err := s.store.CopyTrade().UpdateDecisionMode(traderID, decisionMode); err != nil {
			logger.Infof("⚠️ Failed to update decision mode: %v", err)
		}

		// If copy_trade mode, update copy trade config
		if decisionMode == "copy_trade" && req.CopyConfig != nil {
			// SyncMarginMode 默认为 true（同步领航员的全仓/逐仓模式）
			syncMarginMode := true
			if req.CopyConfig.SyncMarginMode != nil {
				syncMarginMode = *req.CopyConfig.SyncMarginMode
			}

			// 读取已有配置：凭证"空/掩码=不修改"语义 + 保留 Enabled 状态都需要旧值
			existingCopyCfg, _ := s.store.CopyTrade().GetByTraderID(traderID)
			existingP20T, existingCSRF := "", ""
			if existingCopyCfg != nil {
				existingP20T, existingCSRF = existingCopyCfg.BinanceP20T, existingCopyCfg.BinanceCSRFToken
			}

			copyConfig := store.NewCopyGuardDefaults()
			if existingCopyCfg != nil {
				clone := *existingCopyCfg
				copyConfig = &clone
			}
			copyConfig.TraderID = traderID
			copyConfig.ProviderType = req.CopyConfig.ProviderType
			copyConfig.LeaderID = req.CopyConfig.LeaderID
			copyConfig.CopyRatio = req.CopyConfig.CopyRatio
			copyConfig.SyncLeverage = req.CopyConfig.SyncLeverage
			copyConfig.SyncMarginMode = syncMarginMode
			if req.CopyConfig.MinTradeWarn != nil {
				copyConfig.MinTradeWarn = *req.CopyConfig.MinTradeWarn
			}
			if req.CopyConfig.MaxTradeWarn != nil {
				copyConfig.MaxTradeWarn = *req.CopyConfig.MaxTradeWarn
			}
			if req.CopyConfig.CopyCatchupWindowSeconds != nil {
				copyConfig.CopyCatchupWindowSeconds = *req.CopyConfig.CopyCatchupWindowSeconds
			}
			if req.CopyConfig.CopyCatchupMaxAdverseBPS != nil {
				copyConfig.CopyCatchupMaxAdverseBPS = *req.CopyConfig.CopyCatchupMaxAdverseBPS
			}
			copyConfig.BinanceP20T = resolveCredentialUpdate(req.CopyConfig.BinanceP20T, existingP20T)
			copyConfig.BinanceCSRFToken = resolveCredentialUpdate(req.CopyConfig.BinanceCSRFToken, existingCSRF)
			copyConfig.BinanceSourceMode = req.CopyConfig.BinanceSourceMode
			copyConfig.BinanceTopTraderID = req.CopyConfig.BinanceTopTraderID

			// v3 风控字段透传（修复 bug：之前 CopyConfigReq 无 risk_* 字段，前端传值被 JSON unmarshal 静默丢弃）
			manualReentryDeprecated = applyCopyConfigRiskFields(copyConfig, req.CopyConfig)
			if err := validateLegacyReentrySelection(existingCopyCfg, copyConfig.RiskReentryDecisionMode); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if err := copytrade.ValidateStoredRiskPolicy(copyConfig); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if copyConfig.RiskPolicyVersion >= 4 && !copytrade.SupportsCopyGuard(copytrade.ProviderType(copyConfig.ProviderType)) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "copy guard v4 is only supported for OKX or Binance leader sources"})
				return
			}
			if copyConfig.RiskPolicyVersion >= 4 {
				if err := validateRiskConfirmation(copyConfig.RiskAccountPct, req.CopyConfig.RiskHighRiskConfirmed, req.CopyConfig.RiskExtremeConfirmValue); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
					return
				}
				if copyConfig.RiskStopPriority == "account_cap" {
					if err := validateStopLossPctConfirmation(copyConfig.RiskStopMaxAccountLossPct, req.CopyConfig.RiskHighRiskConfirmed, req.CopyConfig.RiskStopExtremeConfirmValue); err != nil {
						c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
						return
					}
				}
			}
			if err := validateAIGuardedPrerequisites(s.store, copyConfig); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if err := prepareCopyTradeSource(s.store, copyConfig, existingCopyCfg); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			// Update 路径需要保留 Enabled 状态（与 Create 不同：Create 总是 false 等显式启动）
			// 复用前面读出的已有配置，避免被 Upsert 误覆盖为 false
			if existingCopyCfg != nil {
				copyConfig.Enabled = existingCopyCfg.Enabled
			}

			// Default copy ratio to 1.0 (100%)
			if copyConfig.CopyRatio <= 0 {
				copyConfig.CopyRatio = 1.0
			}
			if copyConfig.CopyRatio > maxCopyRatio {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("copy_ratio %.2f exceeds max %.0f", copyConfig.CopyRatio, maxCopyRatio)})
				return
			}

			if err := s.store.CopyTrade().Upsert(copyConfig); err != nil {
				// 同 Create 路径：静默吞掉会让用户以为已保存，实际引擎仍
				// 在用旧配置运行。Upsert 幂等，返回 500 让用户重试。
				logger.Errorf("❌ Failed to update copy trade config: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "copy trade config save failed, please save again: " + err.Error()})
				return
			}
			disableReentryCandidatesAfterConfigSave(s.store, existingCopyCfg, copyConfig)
			if err := copytrade.ReloadCopyTradingForTrader(traderID); err != nil {
				logger.Errorf("❌ Copy trade config persisted but runtime reload failed: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "persisted": true})
				return
			}
			logger.Infof("✓ Copy trade config updated: provider=%s, leader=%s, ratio=%.0f%% (risk SL=%v reentry=%v)",
				copyConfig.ProviderType, copyConfig.LeaderID, copyConfig.CopyRatio*100,
				copyConfig.RiskStopLossEnabled, copyConfig.RiskReentryEnabled)
		} else if decisionMode == "ai" {
			// If switching to AI mode, disable copy trade
			s.store.CopyTrade().SetEnabled(traderID, false)
		}
	}

	// Remove old trader from memory first to ensure fresh config is loaded
	s.traderManager.RemoveTrader(traderID)
	if s.positionSyncManager != nil {
		s.positionSyncManager.InvalidateCache(traderID)
	}

	// Reload traders into memory with fresh config
	err = s.traderManager.LoadUserTradersFromStore(s.store, userID)
	if err != nil {
		logger.Infof("⚠️ Failed to reload user traders into memory: %v", err)
	}

	logger.Infof("✓ Trader updated successfully: %s (model: %s, exchange: %s, strategy: %s)", req.Name, req.AIModelID, req.ExchangeID, strategyID)

	response := gin.H{
		"trader_id":     traderID,
		"trader_name":   req.Name,
		"ai_model":      req.AIModelID,
		"decision_mode": decisionMode,
		"message":       "Trader updated successfully",
	}
	if manualReentryDeprecated {
		response["deprecated"] = manualReentryDeprecatedMessage
	}
	c.JSON(http.StatusOK, response)
}

// handleDeleteTrader Delete trader
func (s *Server) handleDeleteTrader(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("id")

	state, err := s.store.Trader().GetLifecycleForUser(userID, traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trader does not exist or no access permission"})
		return
	}
	if state.Status == store.TraderLifecycleArchived {
		c.JSON(http.StatusOK, gin.H{"status": store.TraderLifecycleArchived, "message": "Trader already archived"})
		return
	}
	if state.Status != store.TraderLifecycleStopped {
		c.JSON(http.StatusConflict, gin.H{
			"error":            "Trader must be fully stopped before archive",
			"lifecycle_status": state.Status,
			"pending_blockers": []store.TraderLifecycleBlocker{{
				Code: "TRADER_NOT_STOPPED", ResourceID: traderID, Status: state.Status,
			}},
		})
		return
	}
	// Archive is one operator action: first refresh authoritative exchange
	// positions, regular/algo orders and local Copy Guard retirement, then repeat
	// both local and exchange checks before the irreversible archive transition.
	if s.positionSyncManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Position reconciliation service unavailable"})
		return
	}
	s.positionSyncManager.InvalidateCache(traderID)
	if exchangeBlockers, reconcileErr := s.positionSyncManager.ReconcileStoppedTrader(traderID); reconcileErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":            "Authoritative exchange reconciliation failed: " + reconcileErr.Error(),
			"lifecycle_status": state.Status,
		})
		return
	} else if len(exchangeBlockers) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":            "Trader has unresolved exchange risk and cannot be archived",
			"lifecycle_status": state.Status,
			"pending_blockers": exchangeBlockers,
		})
		return
	}
	archiveBlockers, err := s.store.Trader().GetArchiveBlockers(traderID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to verify archive safety: %v", err)})
		return
	}
	archiveBlockers = append(archiveBlockers, s.getExchangeArchiveBlockers(traderID)...)
	if len(archiveBlockers) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":            "Trader has unresolved exchange risk and cannot be archived",
			"lifecycle_status": state.Status,
			"pending_blockers": archiveBlockers,
		})
		return
	}
	blockers, err := s.store.Trader().Archive(userID, traderID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to archive trader: %v", err)})
		return
	}
	if len(blockers) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":            "Trader has unresolved exchange risk and cannot be archived",
			"lifecycle_status": state.Status,
			"pending_blockers": blockers,
		})
		return
	}
	s.traderManager.RemoveTrader(traderID)
	logger.Infof("✓ Trader archived with audit history retained: %s", traderID)
	c.JSON(http.StatusOK, gin.H{"status": store.TraderLifecycleArchived, "message": "Trader archived"})
}

// handleStartTrader Start trader
func (s *Server) handleStartTrader(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("id")

	// Verify trader belongs to current user
	fullConfig, err := s.store.Trader().GetFullConfig(userID, traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trader does not exist or no access permission"})
		return
	}

	if fullConfig.Trader.LifecycleStatus == store.TraderLifecycleArchived {
		c.JSON(http.StatusConflict, gin.H{"error": "Archived trader cannot be started"})
		return
	}
	lifecycle, err := s.store.Trader().BeginStart(userID, traderID)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error(), "lifecycle_status": fullConfig.Trader.LifecycleStatus})
		return
	}
	if lifecycle.Status == store.TraderLifecycleRunning {
		c.JSON(http.StatusOK, gin.H{
			"status": store.TraderLifecycleRunning, "lifecycle_generation": lifecycle.Generation,
			"message": "Trader already running",
		})
		return
	}
	failStart := func(cause error) {
		_ = s.store.Trader().FailStart(userID, traderID, lifecycle.Generation, cause.Error())
	}

	// Remove from memory to reload fresh config
	if _, err := s.traderManager.GetTrader(traderID); err == nil {
		logger.Infof("🔄 Removing stopped trader %s from memory to reload config...", traderID)
		s.traderManager.RemoveTrader(traderID)
	}

	// Load trader from database (always reload to get latest config)
	logger.Infof("🔄 Loading trader %s from database...", traderID)
	if loadErr := s.traderManager.LoadUserTradersFromStore(s.store, userID); loadErr != nil {
		failStart(loadErr)
		logger.Infof("❌ Failed to load user traders: %v", loadErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load trader: " + loadErr.Error()})
		return
	}

	trader, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		failStart(fmt.Errorf("trader runtime configuration could not be loaded: %w", err))
		// Check detailed reason
		fullCfg, _ := s.store.Trader().GetFullConfig(userID, traderID)
		if fullCfg != nil && fullCfg.Trader != nil {
			// Check strategy
			if fullCfg.Strategy == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Trader has no strategy configured, please create a strategy in Strategy Studio and associate it with the trader"})
				return
			}
			// Check AI model
			if fullCfg.AIModel == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Trader's AI model does not exist, please check AI model configuration"})
				return
			}
			if !fullCfg.AIModel.Enabled {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Trader's AI model is not enabled, please enable the AI model first"})
				return
			}
			// Check exchange
			if fullCfg.Exchange == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Trader's exchange does not exist, please check exchange configuration"})
				return
			}
			if !fullCfg.Exchange.Enabled {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Trader's exchange is not enabled, please enable the exchange first"})
				return
			}
		}
		// Check if there's a specific load error
		if loadErr := s.traderManager.GetLoadError(traderID); loadErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load trader: " + loadErr.Error()})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "Failed to load trader, please check AI model, exchange and strategy configuration"})
		return
	}
	trader.SetLifecycleGeneration(lifecycle.Generation)

	// Check decision mode to determine startup type
	decisionMode, _ := s.store.CopyTrade().GetDecisionMode(traderID)

	if err = s.store.Trader().CompleteStart(userID, traderID, lifecycle.Generation); err != nil {
		trader.Stop()
		failStart(err)
		c.JSON(http.StatusConflict, gin.H{"error": "Failed to commit trader start: " + err.Error()})
		return
	}
	rollbackCommittedStart := func(cause error) {
		if stopped, stopErr := s.store.Trader().BeginStop(userID, traderID); stopErr == nil {
			_ = s.store.Trader().CompleteStop(userID, traderID, stopped.Generation)
		}
		trader.Stop()
		logger.Errorf("❌ Trader %s startup rolled back after RUNNING commit: %v", traderID, cause)
	}

	if decisionMode == "copy_trade" {
		// Start in copy trade mode
		logger.Infof("🎯 [%s] Starting in copy trade mode...", trader.GetName())

		// Enable copy trade config before starting
		if err := s.store.CopyTrade().SetEnabled(traderID, true); err != nil {
			logger.Warnf("⚠️ Failed to enable copy trade config: %v", err)
		}

		if err := copytrade.StartCopyTradingForTraderWithGeneration(traderID, lifecycle.Generation, trader, s.store); err != nil {
			_ = s.store.CopyTrade().SetEnabled(traderID, false)
			rollbackCommittedStart(err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start copy trader: " + err.Error()})
			return
		}
	} else {
		// Start in AI mode (default)
		logger.Infof("🤖 [%s] Starting in AI mode...", trader.GetName())
	}
	// A stopped Copy Guard candidate becomes eligible only after unfinished
	// exchange work and authoritative leader/follower state have been restored.
	// Keeping it PAUSED_BY_TRADER until this point prevents the global advisor
	// scheduler from racing the copy-trading startup reconciliation.
	if err = s.store.ReentryAI().ResumeTraderCandidatesForStart(traderID); err != nil {
		if decisionMode == "copy_trade" {
			_ = copytrade.StopCopyTradingForTrader(traderID)
			_ = s.store.CopyTrade().SetEnabled(traderID, false)
		}
		rollbackCommittedStart(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to restore paused AI candidates: " + err.Error()})
		return
	}
	if decisionMode != "copy_trade" {
		go func(generation int64) {
			if runErr := trader.Run(); runErr != nil {
				logger.Infof("❌ Trader %s runtime error: %v", trader.GetName(), runErr)
				state, stateErr := s.store.Trader().GetLifecycle(traderID)
				if stateErr == nil && state.Generation == generation && state.Status == store.TraderLifecycleRunning {
					_ = s.store.Trader().UpdateStatus(userID, traderID, false)
				}
			}
		}(lifecycle.Generation)
	}

	logger.Infof("✓ Trader %s started", trader.GetName())
	c.JSON(http.StatusOK, gin.H{
		"status": store.TraderLifecycleRunning, "lifecycle_generation": lifecycle.Generation,
		"message": "Trader started",
	})
}

// handleStopTrader Stop trader
func (s *Server) handleStopTrader(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("id")

	// Stop and reconciliation remain available even if runtime dependencies
	// were removed; only ownership and the durable lifecycle are required.
	_, err := s.store.Trader().GetLifecycleForUser(userID, traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trader does not exist or no access permission"})
		return
	}

	lifecycle, err := s.store.Trader().BeginStop(userID, traderID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if lifecycle.Status == store.TraderLifecycleStopped {
		c.JSON(http.StatusOK, gin.H{
			"status": store.TraderLifecycleStopped, "lifecycle_generation": lifecycle.Generation,
			"pending_blockers": []store.TraderLifecycleBlocker{}, "message": "Trader already stopped",
		})
		return
	}

	// Stop every possible runtime path. Decision-mode configuration is not an
	// authority: it may be missing or stale, while a copy-trading integration
	// and submitted exchange work still exist.
	var executor copytrade.DecisionExecutor
	if runtimeTrader, getErr := s.traderManager.GetTrader(traderID); getErr == nil {
		executor = runtimeTrader
	}
	_ = copytrade.PrepareCopyTradingStopWithExecutor(traderID, executor, s.store)
	if err := copytrade.StopCopyTradingForTrader(traderID); err != nil {
		logger.Warnf("⚠️ Failed to stop copy trade engine: %v", err)
	}
	_ = s.store.CopyTrade().SetEnabled(traderID, false)

	// Stop AutoTrader (also stop for copy trade mode in case it was partially started)
	if trader, err := s.traderManager.GetTrader(traderID); err == nil {
		trader.Stop()
	}

	blockers, blockerErr := s.store.Trader().GetStopBlockers(traderID)
	if blockerErr != nil {
		blockers = []store.TraderLifecycleBlocker{{
			Code: "STOP_BLOCKER_QUERY_FAILED", ResourceID: traderID, Status: blockerErr.Error(),
		}}
	}
	if len(blockers) > 0 {
		detail := store.FormatLifecycleBlockers(blockers)
		_ = s.store.Trader().MarkStopReconcileRequired(userID, traderID, lifecycle.Generation, detail)
		c.JSON(http.StatusAccepted, gin.H{
			"status": store.TraderLifecycleStoppingReconcileRequired, "lifecycle_generation": lifecycle.Generation,
			"pending_blockers": blockers, "message": "Trader runtime stopped; exchange reconciliation is still required",
		})
		return
	}
	if err := s.store.Trader().CompleteStop(userID, traderID, lifecycle.Generation); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	logger.Infof("⏹  Trader %s stopped", traderID)
	c.JSON(http.StatusOK, gin.H{
		"status": store.TraderLifecycleStopped, "lifecycle_generation": lifecycle.Generation,
		"pending_blockers": []store.TraderLifecycleBlocker{}, "message": "Trader stopped",
	})
}

// handleReconcileTrader explicitly prepares a stopped trader for archive.
// Stop completion and archive preparation are intentionally separate: STOPPED
// may retain real positions, while archive requires authoritative flat/order-
// free evidence and auditable local accounting.
func (s *Server) handleReconcileTrader(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("id")
	lifecycle, err := s.store.Trader().GetLifecycleForUser(userID, traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trader does not exist or no access permission"})
		return
	}
	switch lifecycle.Status {
	case store.TraderLifecycleStopping, store.TraderLifecycleStoppingReconcileRequired:
		var executor copytrade.DecisionExecutor
		if runtimeTrader, getErr := s.traderManager.GetTrader(traderID); getErr == nil {
			executor = runtimeTrader
		}
		_ = copytrade.PrepareCopyTradingStopWithExecutor(traderID, executor, s.store)
		_ = copytrade.StopCopyTradingForTrader(traderID)
		_ = s.store.CopyTrade().SetEnabled(traderID, false)
		blockers, blockerErr := s.store.Trader().GetStopBlockers(traderID)
		if blockerErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": blockerErr.Error()})
			return
		}
		if len(blockers) > 0 {
			_ = s.store.Trader().MarkStopReconcileRequired(
				userID, traderID, lifecycle.Generation, store.FormatLifecycleBlockers(blockers),
			)
			c.JSON(http.StatusAccepted, gin.H{
				"status":               store.TraderLifecycleStoppingReconcileRequired,
				"lifecycle_generation": lifecycle.Generation,
				"pending_blockers":     blockers,
				"message":              "Submitted exchange work still requires reconciliation",
			})
			return
		}
		if err = s.store.Trader().CompleteStop(userID, traderID, lifecycle.Generation); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
	case store.TraderLifecycleStopped:
	default:
		c.JSON(http.StatusConflict, gin.H{
			"error":            "Trader must be stopping or stopped before reconciliation",
			"lifecycle_status": lifecycle.Status,
		})
		return
	}
	if s.positionSyncManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Position reconciliation service unavailable"})
		return
	}
	s.positionSyncManager.InvalidateCache(traderID)
	exchangeBlockers, err := s.positionSyncManager.ReconcileStoppedTrader(traderID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":            "Authoritative exchange reconciliation failed: " + err.Error(),
			"lifecycle_status": store.TraderLifecycleStopped,
		})
		return
	}
	archiveBlockers, err := s.store.Trader().GetArchiveBlockers(traderID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	archiveBlockers = append(archiveBlockers, exchangeBlockers...)
	c.JSON(http.StatusOK, gin.H{
		"status":               store.TraderLifecycleStopped,
		"lifecycle_generation": lifecycle.Generation,
		"pending_blockers":     archiveBlockers,
		"message":              "Stopped trader reconciliation completed",
	})
}

// handleUpdateTraderPrompt Update trader custom prompt
func (s *Server) handleUpdateTraderPrompt(c *gin.Context) {
	traderID := c.Param("id")
	userID := c.GetString("user_id")

	var req struct {
		CustomPrompt       string `json:"custom_prompt"`
		OverrideBasePrompt bool   `json:"override_base_prompt"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Update database
	err := s.store.Trader().UpdateCustomPrompt(userID, traderID, req.CustomPrompt, req.OverrideBasePrompt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to update custom prompt: %v", err)})
		return
	}

	// If trader is in memory, update its custom prompt and override settings
	trader, err := s.traderManager.GetTrader(traderID)
	if err == nil {
		trader.SetCustomPrompt(req.CustomPrompt)
		trader.SetOverrideBasePrompt(req.OverrideBasePrompt)
		logger.Infof("✓ Updated trader %s custom prompt (override base=%v)", trader.GetName(), req.OverrideBasePrompt)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Custom prompt updated"})
}

// handleToggleCompetition Toggle trader competition visibility
func (s *Server) handleToggleCompetition(c *gin.Context) {
	traderID := c.Param("id")
	userID := c.GetString("user_id")

	var req struct {
		ShowInCompetition bool `json:"show_in_competition"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Update database
	err := s.store.Trader().UpdateShowInCompetition(userID, traderID, req.ShowInCompetition)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to update competition visibility: %v", err)})
		return
	}

	// Update in-memory trader if it exists
	if trader, err := s.traderManager.GetTrader(traderID); err == nil {
		trader.SetShowInCompetition(req.ShowInCompetition)
	}

	status := "shown"
	if !req.ShowInCompetition {
		status = "hidden"
	}
	logger.Infof("✓ Trader %s competition visibility updated: %s", traderID, status)
	c.JSON(http.StatusOK, gin.H{
		"message":             "Competition visibility updated",
		"show_in_competition": req.ShowInCompetition,
	})
}

// handleSyncBalance Sync exchange balance to initial_balance (Option B: Manual Sync + Option C: Smart Detection)
func (s *Server) handleSyncBalance(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("id")

	logger.Infof("🔄 User %s requested balance sync for trader %s", userID, traderID)

	// Get trader configuration from database (including exchange info)
	fullConfig, err := s.store.Trader().GetFullConfig(userID, traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trader does not exist"})
		return
	}

	traderConfig := fullConfig.Trader
	exchangeCfg := fullConfig.Exchange

	if exchangeCfg == nil || !exchangeCfg.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Exchange not configured or not enabled"})
		return
	}

	// Create temporary trader to query balance
	var tempTrader trader.Trader
	var createErr error

	// Use ExchangeType (e.g., "binance") instead of ExchangeID (which is now UUID)
	switch exchangeCfg.ExchangeType {
	case "binance":
		tempTrader = trader.NewFuturesTrader(exchangeCfg.APIKey, exchangeCfg.SecretKey, userID)
	case "hyperliquid":
		tempTrader, createErr = trader.NewHyperliquidTrader(
			exchangeCfg.APIKey,
			exchangeCfg.HyperliquidWalletAddr,
			exchangeCfg.Testnet,
		)
	case "aster":
		tempTrader, createErr = trader.NewAsterTrader(
			exchangeCfg.AsterUser,
			exchangeCfg.AsterSigner,
			exchangeCfg.AsterPrivateKey,
		)
	case "bybit":
		tempTrader = trader.NewBybitTrader(
			exchangeCfg.APIKey,
			exchangeCfg.SecretKey,
		)
	case "okx":
		tempTrader = trader.NewOKXTrader(
			exchangeCfg.APIKey,
			exchangeCfg.SecretKey,
			exchangeCfg.Passphrase,
		)
	case "bitget":
		tempTrader = trader.NewBitgetTrader(
			exchangeCfg.APIKey,
			exchangeCfg.SecretKey,
			exchangeCfg.Passphrase,
		)
	case "lighter":
		if exchangeCfg.LighterWalletAddr != "" && exchangeCfg.LighterAPIKeyPrivateKey != "" {
			// Lighter only supports mainnet
			tempTrader, createErr = trader.NewLighterTraderV2(
				exchangeCfg.LighterWalletAddr,
				exchangeCfg.LighterAPIKeyPrivateKey,
				exchangeCfg.LighterAPIKeyIndex,
				false, // Always use mainnet for Lighter
			)
		} else {
			createErr = fmt.Errorf("Lighter requires wallet address and API Key private key")
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported exchange type"})
		return
	}

	if createErr != nil {
		logger.Infof("⚠️ Failed to create temporary trader: %v", createErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to connect to exchange: %v", createErr)})
		return
	}

	// Query actual balance
	balanceInfo, balanceErr := tempTrader.GetBalance()
	if balanceErr != nil {
		logger.Infof("⚠️ Failed to query exchange balance: %v", balanceErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to query balance: %v", balanceErr)})
		return
	}

	// Extract total equity (for P&L calculation, we need total account value, not available balance)
	var actualBalance float64
	// Priority: total_equity > totalWalletBalance > wallet_balance > totalEq > balance
	balanceKeys := []string{"total_equity", "totalWalletBalance", "wallet_balance", "totalEq", "balance"}
	for _, key := range balanceKeys {
		if balance, ok := balanceInfo[key].(float64); ok && balance > 0 {
			actualBalance = balance
			break
		}
	}
	if actualBalance <= 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unable to get total equity"})
		return
	}

	oldBalance := traderConfig.InitialBalance

	// ✅ Option C: Smart balance change detection
	changePercent := ((actualBalance - oldBalance) / oldBalance) * 100
	changeType := "increase"
	if changePercent < 0 {
		changeType = "decrease"
	}

	logger.Infof("✓ Queried actual exchange balance: %.2f USDT (current config: %.2f USDT, change: %.2f%%)",
		actualBalance, oldBalance, changePercent)

	// Update initial_balance in database
	err = s.store.Trader().UpdateInitialBalance(userID, traderID, actualBalance)
	if err != nil {
		logger.Infof("❌ Failed to update initial_balance: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update balance"})
		return
	}

	// Reload traders into memory
	err = s.traderManager.LoadUserTradersFromStore(s.store, userID)
	if err != nil {
		logger.Infof("⚠️ Failed to reload user traders into memory: %v", err)
	}

	logger.Infof("✅ Synced balance: %.2f → %.2f USDT (%s %.2f%%)", oldBalance, actualBalance, changeType, changePercent)

	c.JSON(http.StatusOK, gin.H{
		"message":        "Balance synced successfully",
		"old_balance":    oldBalance,
		"new_balance":    actualBalance,
		"change_percent": changePercent,
		"change_type":    changeType,
	})
}

// handleClosePosition One-click close position
func (s *Server) handleClosePosition(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("id")

	var req struct {
		Symbol string `json:"symbol" binding:"required"`
		Side   string `json:"side" binding:"required"` // "LONG" or "SHORT"
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Parameter error: symbol and side are required"})
		return
	}

	logger.Infof("🔻 User %s requested position close: trader=%s, symbol=%s, side=%s", userID, traderID, req.Symbol, req.Side)

	// Get trader configuration from database (including exchange info)
	fullConfig, err := s.store.Trader().GetFullConfig(userID, traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trader does not exist"})
		return
	}

	exchangeCfg := fullConfig.Exchange

	if exchangeCfg == nil || !exchangeCfg.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Exchange not configured or not enabled"})
		return
	}

	// Create temporary trader to execute close position
	var tempTrader trader.Trader
	var createErr error

	// Use ExchangeType (e.g., "binance") instead of ExchangeID (which is now UUID)
	switch exchangeCfg.ExchangeType {
	case "binance":
		tempTrader = trader.NewFuturesTrader(exchangeCfg.APIKey, exchangeCfg.SecretKey, userID)
	case "hyperliquid":
		tempTrader, createErr = trader.NewHyperliquidTrader(
			exchangeCfg.APIKey,
			exchangeCfg.HyperliquidWalletAddr,
			exchangeCfg.Testnet,
		)
	case "aster":
		tempTrader, createErr = trader.NewAsterTrader(
			exchangeCfg.AsterUser,
			exchangeCfg.AsterSigner,
			exchangeCfg.AsterPrivateKey,
		)
	case "bybit":
		tempTrader = trader.NewBybitTrader(
			exchangeCfg.APIKey,
			exchangeCfg.SecretKey,
		)
	case "okx":
		tempTrader = trader.NewOKXTrader(
			exchangeCfg.APIKey,
			exchangeCfg.SecretKey,
			exchangeCfg.Passphrase,
		)
	case "bitget":
		tempTrader = trader.NewBitgetTrader(
			exchangeCfg.APIKey,
			exchangeCfg.SecretKey,
			exchangeCfg.Passphrase,
		)
	case "lighter":
		if exchangeCfg.LighterWalletAddr != "" && exchangeCfg.LighterAPIKeyPrivateKey != "" {
			// Lighter only supports mainnet
			tempTrader, createErr = trader.NewLighterTraderV2(
				exchangeCfg.LighterWalletAddr,
				exchangeCfg.LighterAPIKeyPrivateKey,
				exchangeCfg.LighterAPIKeyIndex,
				false, // Always use mainnet for Lighter
			)
		} else {
			createErr = fmt.Errorf("Lighter requires wallet address and API Key private key")
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported exchange type"})
		return
	}

	if createErr != nil {
		logger.Infof("⚠️ Failed to create temporary trader: %v", createErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to connect to exchange: %v", createErr)})
		return
	}

	// Execute close position operation
	var result map[string]interface{}
	var closeErr error

	if req.Side == "LONG" {
		result, closeErr = tempTrader.CloseLong(req.Symbol, 0) // 0 means close all
	} else if req.Side == "SHORT" {
		result, closeErr = tempTrader.CloseShort(req.Symbol, 0) // 0 means close all
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "side must be LONG or SHORT"})
		return
	}

	if closeErr != nil {
		logger.Infof("❌ Close position failed: symbol=%s, side=%s, error=%v", req.Symbol, req.Side, closeErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to close position: %v", closeErr)})
		return
	}

	logger.Infof("✅ Position closed successfully: symbol=%s, side=%s, result=%v", req.Symbol, req.Side, result)
	c.JSON(http.StatusOK, gin.H{
		"message": "Position closed successfully",
		"symbol":  req.Symbol,
		"side":    req.Side,
		"result":  result,
	})
}

// handleGetModelConfigs Get AI model configurations
func (s *Server) handleGetModelConfigs(c *gin.Context) {
	userID := c.GetString("user_id")
	logger.Infof("🔍 Querying AI model configs for user %s", userID)
	models, err := s.store.AIModel().List(userID)
	if err != nil {
		logger.Infof("❌ Failed to get AI model configs: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to get AI model configs: %v", err)})
		return
	}

	// If no models in database, return default models
	if len(models) == 0 {
		logger.Infof("⚠️ No AI models in database, returning defaults")
		defaultModels := []SafeModelConfig{
			{ID: "deepseek", Name: "DeepSeek AI", Provider: "deepseek", Enabled: false},
			{ID: "qwen", Name: "Qwen AI", Provider: "qwen", Enabled: false},
			{ID: "openai", Name: "OpenAI", Provider: "openai", Enabled: false},
			{ID: "claude", Name: "Claude AI", Provider: "claude", Enabled: false},
			{ID: "gemini", Name: "Gemini AI", Provider: "gemini", Enabled: false},
			{ID: "grok", Name: "Grok AI", Provider: "grok", Enabled: false},
			{ID: "kimi", Name: "Kimi AI", Provider: "kimi", Enabled: false},
		}
		c.JSON(http.StatusOK, defaultModels)
		return
	}

	logger.Infof("✅ Found %d AI model configs", len(models))

	// Convert to safe response structure, remove sensitive information
	safeModels := make([]SafeModelConfig, len(models))
	for i, model := range models {
		safeModels[i] = SafeModelConfig{
			ID:              model.ID,
			Name:            model.Name,
			Provider:        model.Provider,
			Enabled:         model.Enabled,
			CustomAPIURL:    model.CustomAPIURL,
			CustomModelName: model.CustomModelName,
		}
	}

	c.JSON(http.StatusOK, safeModels)
}

// handleUpdateModelConfigs Update AI model configurations (supports both encrypted and plain text based on config)
func (s *Server) handleUpdateModelConfigs(c *gin.Context) {
	userID := c.GetString("user_id")
	cfg := config.Get()

	// Read raw request body
	bodyBytes, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	var req UpdateModelConfigRequest

	// Check if transport encryption is enabled
	if !cfg.TransportEncryption {
		// Transport encryption disabled, accept plain JSON
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			logger.Infof("❌ Failed to parse plain JSON request: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
			return
		}
		logger.Infof("📝 Received plain text model config (UserID: %s)", userID)
	} else {
		// Transport encryption enabled, require encrypted payload
		var encryptedPayload crypto.EncryptedPayload
		if err := json.Unmarshal(bodyBytes, &encryptedPayload); err != nil {
			logger.Infof("❌ Failed to parse encrypted payload: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format, encrypted transmission required"})
			return
		}

		// Verify encrypted data
		if encryptedPayload.WrappedKey == "" {
			logger.Infof("❌ Detected unencrypted request (UserID: %s)", userID)
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "This endpoint only supports encrypted transmission, please use encrypted client",
				"code":    "ENCRYPTION_REQUIRED",
				"message": "Encrypted transmission is required for security reasons",
			})
			return
		}

		// Decrypt data
		decrypted, err := s.cryptoHandler.cryptoService.DecryptSensitiveData(&encryptedPayload)
		if err != nil {
			logger.Infof("❌ Failed to decrypt model config (UserID: %s): %v", userID, err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to decrypt data"})
			return
		}

		// Parse decrypted data
		if err := json.Unmarshal([]byte(decrypted), &req); err != nil {
			logger.Infof("❌ Failed to parse decrypted data: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse decrypted data"})
			return
		}
		logger.Infof("🔓 Decrypted model config data (UserID: %s)", userID)
	}

	// Update each model's configuration
	for modelID, modelData := range req.Models {
		err := s.store.AIModel().Update(userID, modelID, modelData.Enabled, modelData.APIKey, modelData.CustomAPIURL, modelData.CustomModelName)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to update model %s: %v", modelID, err)})
			return
		}
	}

	// Reload all traders for this user to make new config take effect immediately
	err = s.traderManager.LoadUserTradersFromStore(s.store, userID)
	if err != nil {
		logger.Infof("⚠️ Failed to reload user traders into memory: %v", err)
		// Don't return error here since model config was successfully updated to database
	}

	logger.Infof("✓ AI model config updated: %+v", req.Models)
	c.JSON(http.StatusOK, gin.H{"message": "Model configuration updated"})
}

// handleGetExchangeConfigs Get exchange configurations
func (s *Server) handleGetExchangeConfigs(c *gin.Context) {
	userID := c.GetString("user_id")
	logger.Infof("🔍 Querying exchange configs for user %s", userID)
	exchanges, err := s.store.Exchange().List(userID)
	if err != nil {
		logger.Infof("❌ Failed to get exchange configs: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to get exchange configs: %v", err)})
		return
	}

	// If no exchanges in database, return empty array (user needs to create accounts)
	if len(exchanges) == 0 {
		logger.Infof("⚠️ No exchanges in database for user %s", userID)
		c.JSON(http.StatusOK, []SafeExchangeConfig{})
		return
	}

	logger.Infof("✅ Found %d exchange configs", len(exchanges))

	// Convert to safe response structure, remove sensitive information
	safeExchanges := make([]SafeExchangeConfig, len(exchanges))
	for i, exchange := range exchanges {
		safeExchanges[i] = SafeExchangeConfig{
			ID:                    exchange.ID,
			ExchangeType:          exchange.ExchangeType,
			AccountName:           exchange.AccountName,
			Name:                  exchange.Name,
			Type:                  exchange.Type,
			Enabled:               exchange.Enabled,
			Testnet:               exchange.Testnet,
			HyperliquidWalletAddr: exchange.HyperliquidWalletAddr,
			AsterUser:             exchange.AsterUser,
			AsterSigner:           exchange.AsterSigner,
			LighterWalletAddr:     exchange.LighterWalletAddr,
		}
	}

	c.JSON(http.StatusOK, safeExchanges)
}

// handleUpdateExchangeConfigs Update exchange configurations (supports both encrypted and plain text based on config)
func (s *Server) handleUpdateExchangeConfigs(c *gin.Context) {
	userID := c.GetString("user_id")
	cfg := config.Get()

	// Read raw request body
	bodyBytes, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	var req UpdateExchangeConfigRequest

	// Check if transport encryption is enabled
	if !cfg.TransportEncryption {
		// Transport encryption disabled, accept plain JSON
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			logger.Infof("❌ Failed to parse plain JSON request: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
			return
		}
		logger.Infof("📝 Received plain text exchange config (UserID: %s)", userID)
	} else {
		// Transport encryption enabled, require encrypted payload
		var encryptedPayload crypto.EncryptedPayload
		if err := json.Unmarshal(bodyBytes, &encryptedPayload); err != nil {
			logger.Infof("❌ Failed to parse encrypted payload: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format, encrypted transmission required"})
			return
		}

		// Verify encrypted data
		if encryptedPayload.WrappedKey == "" {
			logger.Infof("❌ Detected unencrypted request (UserID: %s)", userID)
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "This endpoint only supports encrypted transmission, please use encrypted client",
				"code":    "ENCRYPTION_REQUIRED",
				"message": "Encrypted transmission is required for security reasons",
			})
			return
		}

		// Decrypt data
		decrypted, err := s.cryptoHandler.cryptoService.DecryptSensitiveData(&encryptedPayload)
		if err != nil {
			logger.Infof("❌ Failed to decrypt exchange config (UserID: %s): %v", userID, err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to decrypt data"})
			return
		}

		// Parse decrypted data
		if err := json.Unmarshal([]byte(decrypted), &req); err != nil {
			logger.Infof("❌ Failed to parse decrypted data: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse decrypted data"})
			return
		}
		logger.Infof("🔓 Decrypted exchange config data (UserID: %s)", userID)
	}

	// Update each exchange's configuration
	for exchangeID, exchangeData := range req.Exchanges {
		err := s.store.Exchange().Update(userID, exchangeID, exchangeData.Enabled, exchangeData.APIKey, exchangeData.SecretKey, exchangeData.Passphrase, exchangeData.Testnet, exchangeData.HyperliquidWalletAddr, exchangeData.AsterUser, exchangeData.AsterSigner, exchangeData.AsterPrivateKey, exchangeData.LighterWalletAddr, exchangeData.LighterPrivateKey, exchangeData.LighterAPIKeyPrivateKey, exchangeData.LighterAPIKeyIndex)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to update exchange %s: %v", exchangeID, err)})
			return
		}
	}

	// Reload all traders for this user to make new config take effect immediately
	err = s.traderManager.LoadUserTradersFromStore(s.store, userID)
	if err != nil {
		logger.Infof("⚠️ Failed to reload user traders into memory: %v", err)
		// Don't return error here since exchange config was successfully updated to database
	}

	logger.Infof("✓ Exchange config updated: %+v", req.Exchanges)
	c.JSON(http.StatusOK, gin.H{"message": "Exchange configuration updated"})
}

// CreateExchangeRequest request structure for creating a new exchange account
type CreateExchangeRequest struct {
	ExchangeType            string `json:"exchange_type" binding:"required"` // "binance", "bybit", "okx", "hyperliquid", "aster", "lighter"
	AccountName             string `json:"account_name"`                     // User-defined account name
	Enabled                 bool   `json:"enabled"`
	APIKey                  string `json:"api_key"`
	SecretKey               string `json:"secret_key"`
	Passphrase              string `json:"passphrase"`
	Testnet                 bool   `json:"testnet"`
	HyperliquidWalletAddr   string `json:"hyperliquid_wallet_addr"`
	AsterUser               string `json:"aster_user"`
	AsterSigner             string `json:"aster_signer"`
	AsterPrivateKey         string `json:"aster_private_key"`
	LighterWalletAddr       string `json:"lighter_wallet_addr"`
	LighterPrivateKey       string `json:"lighter_private_key"`
	LighterAPIKeyPrivateKey string `json:"lighter_api_key_private_key"`
	LighterAPIKeyIndex      int    `json:"lighter_api_key_index"`
}

// handleCreateExchange Create a new exchange account
func (s *Server) handleCreateExchange(c *gin.Context) {
	userID := c.GetString("user_id")
	cfg := config.Get()

	// Read raw request body
	bodyBytes, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	var req CreateExchangeRequest

	// Check if transport encryption is enabled
	if !cfg.TransportEncryption {
		// Transport encryption disabled, accept plain JSON
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			logger.Infof("❌ Failed to parse plain JSON request: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
			return
		}
	} else {
		// Transport encryption enabled, require encrypted payload
		var encryptedPayload crypto.EncryptedPayload
		if err := json.Unmarshal(bodyBytes, &encryptedPayload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format, encrypted transmission required"})
			return
		}

		if encryptedPayload.WrappedKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "This endpoint only supports encrypted transmission",
				"code":    "ENCRYPTION_REQUIRED",
				"message": "Encrypted transmission is required for security reasons",
			})
			return
		}

		decrypted, err := s.cryptoHandler.cryptoService.DecryptSensitiveData(&encryptedPayload)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to decrypt data"})
			return
		}

		if err := json.Unmarshal([]byte(decrypted), &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse decrypted data"})
			return
		}
	}

	// Validate exchange type
	validTypes := map[string]bool{
		"binance": true, "bybit": true, "okx": true, "bitget": true,
		"hyperliquid": true, "aster": true, "lighter": true,
	}
	if !validTypes[req.ExchangeType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid exchange type: %s", req.ExchangeType)})
		return
	}

	// Create new exchange account
	id, err := s.store.Exchange().Create(
		userID, req.ExchangeType, req.AccountName, req.Enabled,
		req.APIKey, req.SecretKey, req.Passphrase, req.Testnet,
		req.HyperliquidWalletAddr, req.AsterUser, req.AsterSigner, req.AsterPrivateKey,
		req.LighterWalletAddr, req.LighterPrivateKey, req.LighterAPIKeyPrivateKey, req.LighterAPIKeyIndex,
	)
	if err != nil {
		logger.Infof("❌ Failed to create exchange account: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to create exchange account: %v", err)})
		return
	}

	logger.Infof("✓ Created exchange account: type=%s, name=%s, id=%s", req.ExchangeType, req.AccountName, id)
	c.JSON(http.StatusOK, gin.H{
		"message": "Exchange account created",
		"id":      id,
	})
}

// handleDeleteExchange Delete an exchange account
func (s *Server) handleDeleteExchange(c *gin.Context) {
	userID := c.GetString("user_id")
	exchangeID := c.Param("id")

	if exchangeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Exchange ID is required"})
		return
	}

	// Check if any traders are using this exchange
	traders, err := s.store.Trader().List(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check traders"})
		return
	}

	for _, trader := range traders {
		if trader.ExchangeID == exchangeID {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":       "Cannot delete exchange account that is in use by traders",
				"trader_id":   trader.ID,
				"trader_name": trader.Name,
			})
			return
		}
	}

	// Delete exchange account
	err = s.store.Exchange().Delete(userID, exchangeID)
	if err != nil {
		logger.Infof("❌ Failed to delete exchange account: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to delete exchange account: %v", err)})
		return
	}

	logger.Infof("✓ Deleted exchange account: id=%s", exchangeID)
	c.JSON(http.StatusOK, gin.H{"message": "Exchange account deleted"})
}

// handleTraderList Trader list
func (s *Server) handleTraderList(c *gin.Context) {
	userID := c.GetString("user_id")
	traders, err := s.store.Trader().List(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to get trader list: %v", err)})
		return
	}

	result := make([]map[string]interface{}, 0, len(traders))
	for _, trader := range traders {
		// Get decision mode
		decisionMode, _ := s.store.CopyTrade().GetDecisionMode(trader.ID)
		if decisionMode == "" {
			decisionMode = "ai"
		}

		// Get real-time running status (unified check)
		isRunning := s.isTraderRunning(trader.ID)
		pendingBlockers := []store.TraderLifecycleBlocker{}
		if trader.LifecycleStatus == store.TraderLifecycleStopping ||
			trader.LifecycleStatus == store.TraderLifecycleStoppingReconcileRequired {
			if found, blockerErr := s.store.Trader().GetStopBlockers(trader.ID); blockerErr == nil {
				pendingBlockers = found
			}
		} else if trader.LifecycleStatus == store.TraderLifecycleStopped {
			if found, blockerErr := s.store.Trader().GetArchiveBlockers(trader.ID); blockerErr == nil {
				pendingBlockers = found
			}
		}

		// Get strategy name if strategy_id is set
		var strategyName string
		if trader.StrategyID != "" {
			if strategy, err := s.store.Strategy().Get(userID, trader.StrategyID); err == nil {
				strategyName = strategy.Name
			}
		}

		// Return complete AIModelID (e.g. "admin_deepseek"), don't truncate
		// Frontend needs complete ID to verify model exists (consistent with handleGetTraderConfig)
		result = append(result, map[string]interface{}{
			"trader_id":            trader.ID,
			"trader_name":          trader.Name,
			"ai_model":             trader.AIModelID, // Use complete ID
			"exchange_id":          trader.ExchangeID,
			"is_running":           isRunning,
			"lifecycle_status":     trader.LifecycleStatus,
			"lifecycle_generation": trader.LifecycleGeneration,
			"pending_blockers":     pendingBlockers,
			"stopped_at":           trader.StoppedAt,
			"archived_at":          trader.ArchivedAt,
			"show_in_competition":  trader.ShowInCompetition,
			"initial_balance":      trader.InitialBalance,
			"strategy_id":          trader.StrategyID,
			"strategy_name":        strategyName,
			"decision_mode":        decisionMode,
		})
	}

	c.JSON(http.StatusOK, result)
}

// handleGetTraderConfig Get trader detailed configuration
func (s *Server) handleGetTraderConfig(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("id")

	if traderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Trader ID cannot be empty"})
		return
	}

	fullCfg, err := s.store.Trader().GetFullConfig(userID, traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Failed to get trader config: %v", err)})
		return
	}
	traderConfig := fullCfg.Trader

	// Get decision mode
	decisionMode, _ := s.store.CopyTrade().GetDecisionMode(traderID)
	if decisionMode == "" {
		decisionMode = "ai"
	}

	// Get real-time running status (unified check)
	isRunning := s.isTraderRunning(traderID)
	pendingBlockers := []store.TraderLifecycleBlocker{}
	if traderConfig.LifecycleStatus == store.TraderLifecycleStopping ||
		traderConfig.LifecycleStatus == store.TraderLifecycleStoppingReconcileRequired {
		pendingBlockers, _ = s.store.Trader().GetStopBlockers(traderID)
	} else if traderConfig.LifecycleStatus == store.TraderLifecycleStopped {
		pendingBlockers, _ = s.store.Trader().GetArchiveBlockers(traderID)
	}

	// Return complete model ID without conversion, consistent with frontend model list
	aiModelID := traderConfig.AIModelID

	// Get copy trade config if in copy trade mode
	var copyConfig map[string]interface{}
	if decisionMode == "copy_trade" {
		if cfg, err := s.store.CopyTrade().GetByTraderID(traderID); err == nil && cfg != nil {
			copyConfig = map[string]interface{}{
				"enabled":          cfg.Enabled,
				"provider_type":    cfg.ProviderType,
				"leader_id":        cfg.LeaderID,
				"copy_ratio":       cfg.CopyRatio,
				"sync_leverage":    cfg.SyncLeverage,
				"sync_margin_mode": cfg.SyncMarginMode,
				"min_trade_warn":   cfg.MinTradeWarn,
				"max_trade_warn":   cfg.MaxTradeWarn,
				// 凭证脱敏返回（保存路径识别掩码值为"未修改"，见 resolveCredentialUpdate）
				"binance_p20t":          store.MaskSecret(cfg.BinanceP20T),
				"binance_csrf_token":    store.MaskSecret(cfg.BinanceCSRFToken),
				"binance_source_mode":   cfg.BinanceSourceMode,
				"binance_top_trader_id": cfg.BinanceTopTraderID,
				"source_generation":     cfg.SourceGeneration,
			}
			if health, healthErr := s.store.CopyTrade().GetSourceHealth(traderID); healthErr == nil && health != nil &&
				health.SourceGeneration == cfg.SourceGeneration && health.LeaderID == cfg.LeaderID &&
				health.SourceMode == cfg.BinanceSourceMode {
				healthCopy := *health
				healthCopy.LastError = sanitizeSourceHealthError(healthCopy.LastError)
				if unsupported, listErr := s.store.CopyTrade().ListUnsupportedExecutionInstruments(traderID); listErr == nil {
					healthCopy.UnsupportedContracts = unsupported
				}
				copyConfig["source_health"] = &healthCopy
			}
		}
	}

	result := map[string]interface{}{
		"trader_id":             traderConfig.ID,
		"trader_name":           traderConfig.Name,
		"ai_model":              aiModelID,
		"exchange_id":           traderConfig.ExchangeID,
		"strategy_id":           traderConfig.StrategyID,
		"initial_balance":       traderConfig.InitialBalance,
		"scan_interval_minutes": traderConfig.ScanIntervalMinutes,
		"btc_eth_leverage":      traderConfig.BTCETHLeverage,
		"altcoin_leverage":      traderConfig.AltcoinLeverage,
		"trading_symbols":       traderConfig.TradingSymbols,
		"custom_prompt":         traderConfig.CustomPrompt,
		"override_base_prompt":  traderConfig.OverrideBasePrompt,
		"is_cross_margin":       traderConfig.IsCrossMargin,
		"use_coin_pool":         traderConfig.UseCoinPool,
		"use_oi_top":            traderConfig.UseOITop,
		"is_running":            isRunning,
		"lifecycle_status":      traderConfig.LifecycleStatus,
		"lifecycle_generation":  traderConfig.LifecycleGeneration,
		"pending_blockers":      pendingBlockers,
		"stopped_at":            traderConfig.StoppedAt,
		"archived_at":           traderConfig.ArchivedAt,
		"decision_mode":         decisionMode,
		"copy_config":           copyConfig,
	}

	c.JSON(http.StatusOK, result)
}

// handleStatus System status
func (s *Server) handleStatus(c *gin.Context) {
	_, traderID, err := s.getTraderFromQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	trader, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	status := trader.GetStatus()
	c.JSON(http.StatusOK, status)
}

// handleAccount Account information
func (s *Server) handleAccount(c *gin.Context) {
	_, traderID, err := s.getTraderFromQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	trader, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	logger.Infof("📊 Received account info request [%s]", trader.GetName())
	account, err := trader.GetAccountInfo()
	if err != nil {
		logger.Infof("❌ Failed to get account info [%s]: %v", trader.GetName(), err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get account info: %v", err),
		})
		return
	}

	logger.Infof("✓ Returning account info [%s]: equity=%.2f, available=%.2f, pnl=%.2f (%.2f%%)",
		trader.GetName(),
		account["total_equity"],
		account["available_balance"],
		account["total_pnl"],
		account["total_pnl_pct"])
	c.JSON(http.StatusOK, account)
}

// handlePositions Position list
func (s *Server) handlePositions(c *gin.Context) {
	_, traderID, err := s.getTraderFromQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	trader, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	positions, err := trader.GetPositions()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get position list: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, positions)
}

// handleDecisions Decision log list
func (s *Server) handleDecisions(c *gin.Context) {
	_, traderID, err := s.getTraderFromQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	trader, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	// Get all historical decision records (unlimited)
	records, err := trader.GetStore().Decision().GetLatestRecords(trader.GetID(), 10000)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get decision log: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, records)
}

// handleLatestDecisions Latest decision logs (newest first, supports limit parameter)
func (s *Server) handleLatestDecisions(c *gin.Context) {
	_, traderID, err := s.getTraderFromQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	trader, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	// Get limit from query parameter, default to 5
	limit := 5
	if limitStr := c.Query("limit"); limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
			if limit > 100 {
				limit = 100 // Max 100 to prevent abuse
			}
		}
	}

	records, err := trader.GetStore().Decision().GetLatestRecords(trader.GetID(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get decision log: %v", err),
		})
		return
	}

	// Reverse array to put newest first (for list display)
	// GetLatestRecords returns oldest to newest (for charts), here we need newest to oldest
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}

	c.JSON(http.StatusOK, records)
}

// handleStatistics Statistics information
func (s *Server) handleStatistics(c *gin.Context) {
	_, traderID, err := s.getTraderFromQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	trader, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	stats, err := trader.GetStore().Decision().GetStatistics(trader.GetID())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get statistics: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, stats)
}

// handleCompetition Competition overview (compare all traders)
func (s *Server) handleCompetition(c *gin.Context) {
	userID := c.GetString("user_id")

	// Ensure user's traders are loaded into memory
	err := s.traderManager.LoadUserTradersFromStore(s.store, userID)
	if err != nil {
		logger.Infof("⚠️ Failed to load traders for user %s: %v", userID, err)
	}

	competition, err := s.traderManager.GetCompetitionData()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get competition data: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, competition)
}

// handleEquityHistory Return rate historical data
// Query directly from database, not dependent on trader in memory (so historical data can be retrieved after restart)
func (s *Server) handleEquityHistory(c *gin.Context) {
	_, traderID, err := s.getTraderFromQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get equity historical data from new equity table
	// Every 3 minutes per cycle: 10000 records = about 20 days of data
	snapshots, err := s.store.Equity().GetLatest(traderID, 10000)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get historical data: %v", err),
		})
		return
	}

	if len(snapshots) == 0 {
		c.JSON(http.StatusOK, []interface{}{})
		return
	}

	// Build return rate historical data points
	type EquityPoint struct {
		Timestamp        string  `json:"timestamp"`
		TotalEquity      float64 `json:"total_equity"`      // Account equity (wallet + unrealized)
		AvailableBalance float64 `json:"available_balance"` // Available balance
		TotalPnL         float64 `json:"total_pnl"`         // Total PnL (unrealized PnL)
		TotalPnLPct      float64 `json:"total_pnl_pct"`     // Total PnL percentage
		PositionCount    int     `json:"position_count"`    // Position count
		MarginUsedPct    float64 `json:"margin_used_pct"`   // Margin used percentage
	}

	// Use the balance of the first record as initial balance to calculate return rate
	initialBalance := snapshots[0].Balance
	if initialBalance == 0 {
		initialBalance = 1 // Avoid division by zero
	}

	var history []EquityPoint
	for _, snap := range snapshots {
		// Calculate PnL percentage
		totalPnLPct := 0.0
		if initialBalance > 0 {
			totalPnLPct = (snap.UnrealizedPnL / initialBalance) * 100
		}

		history = append(history, EquityPoint{
			Timestamp:        snap.Timestamp.Format("2006-01-02 15:04:05"),
			TotalEquity:      snap.TotalEquity,
			AvailableBalance: snap.Balance,
			TotalPnL:         snap.UnrealizedPnL,
			TotalPnLPct:      totalPnLPct,
			PositionCount:    snap.PositionCount,
			MarginUsedPct:    snap.MarginUsedPct,
		})
	}

	c.JSON(http.StatusOK, history)
}

// authMiddleware JWT authentication middleware
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing Authorization header"})
			c.Abort()
			return
		}

		// Check Bearer token format
		tokenParts := strings.Split(authHeader, " ")
		if len(tokenParts) != 2 || tokenParts[0] != "Bearer" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid Authorization format"})
			c.Abort()
			return
		}

		tokenString := tokenParts[1]

		// Blacklist check
		if auth.IsTokenBlacklisted(tokenString) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Token expired, please login again"})
			c.Abort()
			return
		}

		// Validate JWT token
		claims, err := auth.ValidateJWT(tokenString)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token: " + err.Error()})
			c.Abort()
			return
		}

		// Store user information in context
		c.Set("user_id", claims.UserID)
		c.Set("email", claims.Email)
		c.Next()
	}
}

// handleLogout Add current token to blacklist
func (s *Server) handleLogout(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing Authorization header"})
		return
	}
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid Authorization format"})
		return
	}
	tokenString := parts[1]
	claims, err := auth.ValidateJWT(tokenString)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
		return
	}
	var exp time.Time
	if claims.ExpiresAt != nil {
		exp = claims.ExpiresAt.Time
	} else {
		exp = time.Now().Add(24 * time.Hour)
	}
	auth.BlacklistToken(tokenString, exp)
	c.JSON(http.StatusOK, gin.H{"message": "Logged out"})
}

// handleRegister Handle user registration request
func (s *Server) handleRegister(c *gin.Context) {
	// Check if registration is allowed
	if !config.Get().RegistrationEnabled {
		c.JSON(http.StatusForbidden, gin.H{"error": "Registration is disabled"})
		return
	}

	// Check max users limit
	maxUsers := config.Get().MaxUsers
	if maxUsers > 0 {
		userCount, err := s.store.User().Count()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check user count"})
			return
		}
		if userCount >= maxUsers {
			c.JSON(http.StatusForbidden, gin.H{"error": "Not on whitelist"})
			return
		}
	}

	var req struct {
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required,min=6"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check if email already exists
	_, err := s.store.User().GetByEmail(req.Email)
	if err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Email already registered"})
		return
	}

	// Generate password hash
	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Password processing failed"})
		return
	}

	// Generate OTP secret
	otpSecret, err := auth.GenerateOTPSecret()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "OTP secret generation failed"})
		return
	}

	// Create user (unverified OTP status)
	userID := uuid.New().String()
	user := &store.User{
		ID:           userID,
		Email:        req.Email,
		PasswordHash: passwordHash,
		OTPSecret:    otpSecret,
		OTPVerified:  false,
	}

	err = s.store.User().Create(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user: " + err.Error()})
		return
	}

	// Return OTP setup information
	qrCodeURL := auth.GetOTPQRCodeURL(otpSecret, req.Email)
	c.JSON(http.StatusOK, gin.H{
		"user_id":     userID,
		"email":       req.Email,
		"otp_secret":  otpSecret,
		"qr_code_url": qrCodeURL,
		"message":     "Please scan the QR code with Google Authenticator and verify OTP",
	})
}

// handleCompleteRegistration Complete registration (verify OTP)
func (s *Server) handleCompleteRegistration(c *gin.Context) {
	var req struct {
		UserID  string `json:"user_id" binding:"required"`
		OTPCode string `json:"otp_code" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get user information
	user, err := s.store.User().GetByID(req.UserID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User does not exist"})
		return
	}

	// Verify OTP
	if !auth.VerifyOTP(user.OTPSecret, req.OTPCode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OTP code error"})
		return
	}

	// Update user OTP verified status
	err = s.store.User().UpdateOTPVerified(req.UserID, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update user status"})
		return
	}

	// Generate JWT token
	token, err := auth.GenerateJWT(user.ID, user.Email)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	// Initialize default model and exchange configs for user
	err = s.initUserDefaultConfigs(user.ID)
	if err != nil {
		logger.Infof("Failed to initialize user default configs: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"token":   token,
		"user_id": user.ID,
		"email":   user.Email,
		"message": "Registration completed",
	})
}

// handleLogin Handle user login request
func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get user information
	user, err := s.store.User().GetByEmail(req.Email)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Email or password incorrect"})
		return
	}

	// Verify password
	if !auth.CheckPassword(req.Password, user.PasswordHash) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Email or password incorrect"})
		return
	}

	// Check if OTP is verified
	if !user.OTPVerified {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":              "Account has not completed OTP setup",
			"user_id":            user.ID,
			"requires_otp_setup": true,
		})
		return
	}

	// Return status requiring OTP verification
	c.JSON(http.StatusOK, gin.H{
		"user_id":      user.ID,
		"email":        user.Email,
		"message":      "Please enter Google Authenticator code",
		"requires_otp": true,
	})
}

// handleVerifyOTP Verify OTP and complete login
func (s *Server) handleVerifyOTP(c *gin.Context) {
	var req struct {
		UserID  string `json:"user_id" binding:"required"`
		OTPCode string `json:"otp_code" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get user information
	user, err := s.store.User().GetByID(req.UserID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User does not exist"})
		return
	}

	// Verify OTP
	if !auth.VerifyOTP(user.OTPSecret, req.OTPCode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Verification code error"})
		return
	}

	// Generate JWT token
	token, err := auth.GenerateJWT(user.ID, user.Email)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":   token,
		"user_id": user.ID,
		"email":   user.Email,
		"message": "Login successful",
	})
}

// handleResetPassword Reset password (via email + OTP verification)
func (s *Server) handleResetPassword(c *gin.Context) {
	var req struct {
		Email       string `json:"email" binding:"required,email"`
		NewPassword string `json:"new_password" binding:"required,min=6"`
		OTPCode     string `json:"otp_code" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Query user
	user, err := s.store.User().GetByEmail(req.Email)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Email does not exist"})
		return
	}

	// Verify OTP
	if !auth.VerifyOTP(user.OTPSecret, req.OTPCode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Google Authenticator code error"})
		return
	}

	// Generate new password hash
	newPasswordHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Password processing failed"})
		return
	}

	// Update password
	err = s.store.User().UpdatePassword(user.ID, newPasswordHash)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Password update failed"})
		return
	}

	logger.Infof("✓ User %s password has been reset", user.Email)
	c.JSON(http.StatusOK, gin.H{"message": "Password reset successful, please login with new password"})
}

// initUserDefaultConfigs Initialize default model and exchange configs for new user
func (s *Server) initUserDefaultConfigs(userID string) error {
	// Commented out auto-creation of default configs, let users add manually
	// This way new users won't have config items automatically after registration
	logger.Infof("User %s registration completed, waiting for manual AI model and exchange configuration", userID)
	return nil
}

// handleGetSupportedModels Get list of AI models supported by the system
func (s *Server) handleGetSupportedModels(c *gin.Context) {
	// Return static list of supported AI models with default versions
	supportedModels := []map[string]interface{}{
		{"id": "deepseek", "name": "DeepSeek", "provider": "deepseek", "defaultModel": "deepseek-chat"},
		{"id": "qwen", "name": "Qwen", "provider": "qwen", "defaultModel": "qwen3-max"},
		{"id": "openai", "name": "OpenAI", "provider": "openai", "defaultModel": "gpt-5.1"},
		{"id": "claude", "name": "Claude", "provider": "claude", "defaultModel": "claude-opus-4-5-20251101"},
		{"id": "gemini", "name": "Google Gemini", "provider": "gemini", "defaultModel": "gemini-3-pro-preview"},
		{"id": "grok", "name": "Grok (xAI)", "provider": "grok", "defaultModel": "grok-3-latest"},
		{"id": "kimi", "name": "Kimi (Moonshot)", "provider": "kimi", "defaultModel": "moonshot-v1-auto"},
	}

	c.JSON(http.StatusOK, supportedModels)
}

// handleGetSupportedExchanges Get list of exchanges supported by the system
func (s *Server) handleGetSupportedExchanges(c *gin.Context) {
	// Return static list of supported exchange types
	// Note: ID is empty for supported exchanges (they are templates, not actual accounts)
	supportedExchanges := []SafeExchangeConfig{
		{ExchangeType: "binance", Name: "Binance Futures", Type: "cex"},
		{ExchangeType: "bybit", Name: "Bybit Futures", Type: "cex"},
		{ExchangeType: "okx", Name: "OKX Futures", Type: "cex"},
		{ExchangeType: "hyperliquid", Name: "Hyperliquid", Type: "dex"},
		{ExchangeType: "aster", Name: "Aster DEX", Type: "dex"},
		{ExchangeType: "lighter", Name: "LIGHTER DEX", Type: "dex"},
	}

	c.JSON(http.StatusOK, supportedExchanges)
}

// Start Start server
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.port)
	logger.Infof("🌐 API server starting at http://localhost%s", addr)
	logger.Infof("📊 API Documentation:")
	logger.Infof("  • GET  /api/health           - Health check")
	logger.Infof("  • GET  /api/traders          - Public AI trader leaderboard top 50 (no auth required)")
	logger.Infof("  • GET  /api/competition      - Public competition data (no auth required)")
	logger.Infof("  • GET  /api/top-traders      - Top 5 trader data (no auth required, for performance comparison)")
	logger.Infof("  • GET  /api/equity-history?trader_id=xxx - Public return rate historical data (no auth required, for competition)")
	logger.Infof("  • GET  /api/equity-history-batch?trader_ids=a,b,c - Batch get historical data (no auth required, performance comparison optimization)")
	logger.Infof("  • GET  /api/traders/:id/public-config - Public trader config (no auth required, no sensitive info)")
	logger.Infof("  • POST /api/traders          - Create new AI trader")
	logger.Infof("  • DELETE /api/traders/:id    - Delete AI trader")
	logger.Infof("  • POST /api/traders/:id/start - Start AI trader")
	logger.Infof("  • POST /api/traders/:id/stop  - Stop AI trader")
	logger.Infof("  • GET  /api/models           - Get AI model config")
	logger.Infof("  • PUT  /api/models           - Update AI model config")
	logger.Infof("  • GET  /api/exchanges        - Get exchange config")
	logger.Infof("  • PUT  /api/exchanges        - Update exchange config")
	logger.Infof("  • GET  /api/status?trader_id=xxx     - Specified trader's system status")
	logger.Infof("  • GET  /api/account?trader_id=xxx    - Specified trader's account info")
	logger.Infof("  • GET  /api/positions?trader_id=xxx  - Specified trader's position list")
	logger.Infof("  • GET  /api/decisions?trader_id=xxx  - Specified trader's decision log")
	logger.Infof("  • GET  /api/decisions/latest?trader_id=xxx - Specified trader's latest decisions")
	logger.Infof("  • GET  /api/statistics?trader_id=xxx - Specified trader's statistics")
	logger.Infof("  • GET  /api/performance?trader_id=xxx - Specified trader's AI learning performance analysis")
	logger.Info()

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: s.router,
	}
	return s.httpServer.ListenAndServe()
}

// Shutdown Gracefully shutdown server
func (s *Server) Shutdown() error {
	if s.httpServer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

// handlePublicTraderList Get public trader list (no authentication required)
func (s *Server) handlePublicTraderList(c *gin.Context) {
	// Get trader information from all users
	competition, err := s.traderManager.GetCompetitionData()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get trader list: %v", err),
		})
		return
	}

	// Get traders array
	tradersData, exists := competition["traders"]
	if !exists {
		c.JSON(http.StatusOK, []map[string]interface{}{})
		return
	}

	traders, ok := tradersData.([]map[string]interface{})
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Trader data format error",
		})
		return
	}

	// Return trader basic information, filter sensitive information
	result := make([]map[string]interface{}, 0, len(traders))
	for _, trader := range traders {
		result = append(result, map[string]interface{}{
			"trader_id":       trader["trader_id"],
			"trader_name":     trader["trader_name"],
			"ai_model":        trader["ai_model"],
			"exchange":        trader["exchange"],
			"is_running":      trader["is_running"],
			"total_equity":    trader["total_equity"],
			"total_pnl":       trader["total_pnl"],
			"total_pnl_pct":   trader["total_pnl_pct"],
			"position_count":  trader["position_count"],
			"margin_used_pct": trader["margin_used_pct"],
		})
	}

	c.JSON(http.StatusOK, result)
}

// handlePublicCompetition Get public competition data (no authentication required)
func (s *Server) handlePublicCompetition(c *gin.Context) {
	competition, err := s.traderManager.GetCompetitionData()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get competition data: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, competition)
}

// handleTopTraders Get top 5 trader data (no authentication required, for performance comparison)
func (s *Server) handleTopTraders(c *gin.Context) {
	topTraders, err := s.traderManager.GetTopTradersData()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get top 10 trader data: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, topTraders)
}

// handleEquityHistoryBatch Batch get return rate historical data for multiple traders (no authentication required, for performance comparison)
func (s *Server) handleEquityHistoryBatch(c *gin.Context) {
	var requestBody struct {
		TraderIDs []string `json:"trader_ids"`
	}

	// Try to parse POST request JSON body
	if err := c.ShouldBindJSON(&requestBody); err != nil {
		// If JSON parse fails, try to get from query parameters (compatible with GET request)
		traderIDsParam := c.Query("trader_ids")
		if traderIDsParam == "" {
			// If no trader_ids specified, return historical data for top 5
			topTraders, err := s.traderManager.GetTopTradersData()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": fmt.Sprintf("Failed to get top 5 traders: %v", err),
				})
				return
			}

			traders, ok := topTraders["traders"].([]map[string]interface{})
			if !ok {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Trader data format error"})
				return
			}

			// Extract trader IDs
			traderIDs := make([]string, 0, len(traders))
			for _, trader := range traders {
				if traderID, ok := trader["trader_id"].(string); ok {
					traderIDs = append(traderIDs, traderID)
				}
			}

			result := s.getEquityHistoryForTraders(traderIDs)
			c.JSON(http.StatusOK, result)
			return
		}

		// Parse comma-separated trader IDs
		requestBody.TraderIDs = strings.Split(traderIDsParam, ",")
		for i := range requestBody.TraderIDs {
			requestBody.TraderIDs[i] = strings.TrimSpace(requestBody.TraderIDs[i])
		}
	}

	// Limit to maximum 20 traders to prevent oversized requests
	if len(requestBody.TraderIDs) > 20 {
		requestBody.TraderIDs = requestBody.TraderIDs[:20]
	}

	result := s.getEquityHistoryForTraders(requestBody.TraderIDs)
	c.JSON(http.StatusOK, result)
}

// getEquityHistoryForTraders Get historical data for multiple traders
// Query directly from database, not dependent on trader in memory (so historical data can be retrieved after restart)
// Also appends current real-time data point to ensure chart matches leaderboard
func (s *Server) getEquityHistoryForTraders(traderIDs []string) map[string]interface{} {
	result := make(map[string]interface{})
	histories := make(map[string]interface{})
	errors := make(map[string]string)

	// Use a single consistent timestamp for all real-time data points
	now := time.Now()

	// Pre-fetch initial balances for all traders
	initialBalances := make(map[string]float64)
	for _, traderID := range traderIDs {
		if traderID == "" {
			continue
		}
		// Get trader's initial balance from database (use GetByID which doesn't require userID)
		trader, err := s.store.Trader().GetByID(traderID)
		if err == nil && trader != nil && trader.InitialBalance > 0 {
			initialBalances[traderID] = trader.InitialBalance
		}
	}

	for _, traderID := range traderIDs {
		if traderID == "" {
			continue
		}

		// Get equity historical data from new equity table
		snapshots, err := s.store.Equity().GetLatest(traderID, 500)
		if err != nil {
			errors[traderID] = fmt.Sprintf("Failed to get historical data: %v", err)
			continue
		}

		// Get initial balance for calculating PnL percentage
		initialBalance := initialBalances[traderID]
		if initialBalance <= 0 && len(snapshots) > 0 {
			// If no initial balance configured, use the first snapshot's equity as baseline
			initialBalance = snapshots[0].TotalEquity
		}

		// Build return rate historical data with PnL percentage
		history := make([]map[string]interface{}, 0, len(snapshots)+1)
		var lastSnapshotTime time.Time
		for _, snap := range snapshots {
			// Calculate PnL percentage: (current_equity - initial_balance) / initial_balance * 100
			pnlPct := 0.0
			if initialBalance > 0 {
				pnlPct = (snap.TotalEquity - initialBalance) / initialBalance * 100
			}

			history = append(history, map[string]interface{}{
				"timestamp":     snap.Timestamp,
				"total_equity":  snap.TotalEquity,
				"total_pnl":     snap.UnrealizedPnL,
				"total_pnl_pct": pnlPct,
				"balance":       snap.Balance,
			})
			if snap.Timestamp.After(lastSnapshotTime) {
				lastSnapshotTime = snap.Timestamp
			}
		}

		// Append current real-time data point to ensure chart matches leaderboard
		// This ensures the latest point is always current, not from a potentially stale snapshot
		if trader, err := s.traderManager.GetTrader(traderID); err == nil {
			if accountInfo, err := trader.GetAccountInfo(); err == nil {
				// Only append if it's been more than 30 seconds since last snapshot
				if now.Sub(lastSnapshotTime) > 30*time.Second {
					totalEquity := 0.0
					if v, ok := accountInfo["total_equity"].(float64); ok {
						totalEquity = v
					}
					totalPnL := 0.0
					if v, ok := accountInfo["total_pnl"].(float64); ok {
						totalPnL = v
					}
					walletBalance := 0.0
					if v, ok := accountInfo["wallet_balance"].(float64); ok {
						walletBalance = v
					}
					pnlPct := 0.0
					if initialBalance > 0 {
						pnlPct = (totalEquity - initialBalance) / initialBalance * 100
					}

					history = append(history, map[string]interface{}{
						"timestamp":     now,
						"total_equity":  totalEquity,
						"total_pnl":     totalPnL,
						"total_pnl_pct": pnlPct,
						"balance":       walletBalance,
					})
				}
			}
		}

		histories[traderID] = history
	}

	result["histories"] = histories
	result["count"] = len(histories)
	if len(errors) > 0 {
		result["errors"] = errors
	}

	return result
}

// handleGetPublicTraderConfig Get public trader configuration information (no authentication required, does not include sensitive information)
func (s *Server) handleGetPublicTraderConfig(c *gin.Context) {
	traderID := c.Param("id")
	if traderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Trader ID cannot be empty"})
		return
	}

	trader, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trader does not exist"})
		return
	}

	// Get trader status information
	status := trader.GetStatus()

	// Only return public configuration information, not including sensitive data like API keys
	result := map[string]interface{}{
		"trader_id":   trader.GetID(),
		"trader_name": trader.GetName(),
		"ai_model":    trader.GetAIModel(),
		"exchange":    trader.GetExchange(),
		"is_running":  status["is_running"],
		"ai_provider": status["ai_provider"],
		"start_time":  status["start_time"],
	}

	c.JSON(http.StatusOK, result)
}
