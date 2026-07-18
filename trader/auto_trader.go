package trader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"nofx/decision"
	"nofx/logger"
	"nofx/market"
	"nofx/mcp"
	"nofx/store"
	"strings"
	"sync"
	"time"
)

// AutoTraderConfig auto trading configuration (simplified version - AI makes all decisions)
type AutoTraderConfig struct {
	// Trader identification
	ID      string // Trader unique identifier (for log directory, etc.)
	Name    string // Trader display name
	AIModel string // AI model: "qwen" or "deepseek"

	// Trading platform selection
	Exchange   string // Exchange type: "binance", "bybit", "okx", "bitget", "hyperliquid", "aster" or "lighter"
	ExchangeID string // Exchange account UUID (for multi-account support)

	// Binance API configuration
	BinanceAPIKey    string
	BinanceSecretKey string

	// Bybit API configuration
	BybitAPIKey    string
	BybitSecretKey string

	// OKX API configuration
	OKXAPIKey     string
	OKXSecretKey  string
	OKXPassphrase string

	// Bitget API configuration
	BitgetAPIKey     string
	BitgetSecretKey  string
	BitgetPassphrase string

	// Hyperliquid configuration
	HyperliquidPrivateKey string
	HyperliquidWalletAddr string
	HyperliquidTestnet    bool

	// Aster configuration
	AsterUser       string // Aster main wallet address
	AsterSigner     string // Aster API wallet address
	AsterPrivateKey string // Aster API wallet private key

	// LIGHTER configuration
	LighterWalletAddr       string // LIGHTER wallet address (L1 wallet)
	LighterPrivateKey       string // LIGHTER L1 private key (for account identification)
	LighterAPIKeyPrivateKey string // LIGHTER API Key private key (40 bytes, for transaction signing)
	LighterAPIKeyIndex      int    // LIGHTER API Key index (0-255)
	LighterTestnet          bool   // Whether to use testnet

	// AI configuration
	UseQwen     bool
	DeepSeekKey string
	QwenKey     string

	// Custom AI API configuration
	CustomAPIURL    string
	CustomAPIKey    string
	CustomModelName string

	// Scan configuration
	ScanInterval time.Duration // Scan interval (recommended 3 minutes)

	// Account configuration
	InitialBalance float64 // Initial balance (for P&L calculation, must be set manually)

	// Risk control (only as hints, AI can make autonomous decisions)
	MaxDailyLoss    float64       // Maximum daily loss percentage (hint)
	MaxDrawdown     float64       // Maximum drawdown percentage (hint)
	StopTradingTime time.Duration // Pause duration after risk control triggers

	// Position mode
	IsCrossMargin bool // true=cross margin mode, false=isolated margin mode

	// Competition visibility
	ShowInCompetition bool // Whether to show in competition page

	// Strategy configuration (use complete strategy config)
	StrategyConfig *store.StrategyConfig // Strategy configuration (includes coin sources, indicators, risk control, prompts, etc.)
}

// AutoTrader automatic trader
type AutoTrader struct {
	id                    string // Trader unique identifier
	name                  string // Trader display name
	aiModel               string // AI model name
	exchange              string // Trading platform type (binance/bybit/etc)
	exchangeID            string // Exchange account UUID
	showInCompetition     bool   // Whether to show in competition page
	config                AutoTraderConfig
	trader                Trader // Use Trader interface (supports multiple platforms)
	mcpClient             mcp.AIClient
	store                 *store.Store             // Data storage (decision records, etc.)
	strategyEngine        *decision.StrategyEngine // Strategy engine (uses strategy configuration)
	cycleNumber           int                      // Current cycle number
	initialBalance        float64
	dailyPnL              float64
	customPrompt          string // Custom trading strategy prompt
	overrideBasePrompt    bool   // Whether to override base prompt
	lastResetTime         time.Time
	stopUntil             time.Time
	isRunning             bool
	startTime             time.Time          // System start time
	callCount             int                // AI call count
	positionFirstSeenTime map[string]int64   // Position first seen time (symbol_side -> timestamp in milliseconds)
	stopMonitorCh         chan struct{}      // Used to stop monitoring goroutine
	monitorWg             sync.WaitGroup     // Used to wait for monitoring goroutine to finish
	peakPnLCache          map[string]float64 // Peak profit cache (symbol -> peak P&L percentage)
	peakPnLCacheMutex     sync.RWMutex       // Cache read-write lock
	lastBalanceSyncTime   time.Time          // Last balance sync time
	userID                string             // User ID
}

// NewAutoTrader creates an automatic trader
// st parameter is used to store decision records to database
func NewAutoTrader(config AutoTraderConfig, st *store.Store, userID string) (*AutoTrader, error) {
	// Set default values
	if config.ID == "" {
		config.ID = "default_trader"
	}
	if config.Name == "" {
		config.Name = "Default Trader"
	}
	if config.AIModel == "" {
		if config.UseQwen {
			config.AIModel = "qwen"
		} else {
			config.AIModel = "deepseek"
		}
	}

	// Initialize AI client based on provider
	var mcpClient mcp.AIClient
	aiModel := config.AIModel
	if config.UseQwen && aiModel == "" {
		aiModel = "qwen"
	}

	switch aiModel {
	case "claude":
		mcpClient = mcp.NewClaudeClient()
		mcpClient.SetAPIKey(config.CustomAPIKey, config.CustomAPIURL, config.CustomModelName)
		logger.Infof("🤖 [%s] Using Claude AI", config.Name)

	case "kimi":
		mcpClient = mcp.NewKimiClient()
		mcpClient.SetAPIKey(config.CustomAPIKey, config.CustomAPIURL, config.CustomModelName)
		logger.Infof("🤖 [%s] Using Kimi (Moonshot) AI", config.Name)

	case "gemini":
		mcpClient = mcp.NewGeminiClient()
		mcpClient.SetAPIKey(config.CustomAPIKey, config.CustomAPIURL, config.CustomModelName)
		logger.Infof("🤖 [%s] Using Google Gemini AI", config.Name)

	case "grok":
		mcpClient = mcp.NewGrokClient()
		mcpClient.SetAPIKey(config.CustomAPIKey, config.CustomAPIURL, config.CustomModelName)
		logger.Infof("🤖 [%s] Using xAI Grok AI", config.Name)

	case "openai":
		mcpClient = mcp.NewOpenAIClient()
		mcpClient.SetAPIKey(config.CustomAPIKey, config.CustomAPIURL, config.CustomModelName)
		logger.Infof("🤖 [%s] Using OpenAI", config.Name)

	case "qwen":
		mcpClient = mcp.NewQwenClient()
		apiKey := config.QwenKey
		if apiKey == "" {
			apiKey = config.CustomAPIKey
		}
		mcpClient.SetAPIKey(apiKey, config.CustomAPIURL, config.CustomModelName)
		logger.Infof("🤖 [%s] Using Alibaba Cloud Qwen AI", config.Name)

	case "custom":
		mcpClient = mcp.New()
		mcpClient.SetAPIKey(config.CustomAPIKey, config.CustomAPIURL, config.CustomModelName)
		logger.Infof("🤖 [%s] Using custom AI API: %s (model: %s)", config.Name, config.CustomAPIURL, config.CustomModelName)

	default: // deepseek or empty
		mcpClient = mcp.NewDeepSeekClient()
		apiKey := config.DeepSeekKey
		if apiKey == "" {
			apiKey = config.CustomAPIKey
		}
		mcpClient.SetAPIKey(apiKey, config.CustomAPIURL, config.CustomModelName)
		logger.Infof("🤖 [%s] Using DeepSeek AI", config.Name)
	}

	if config.CustomAPIURL != "" || config.CustomModelName != "" {
		logger.Infof("🔧 [%s] Custom config - URL: %s, Model: %s", config.Name, config.CustomAPIURL, config.CustomModelName)
	}

	// Set default trading platform
	if config.Exchange == "" {
		config.Exchange = "binance"
	}

	// Create corresponding trader based on configuration
	var trader Trader
	var err error

	// Record position mode (general)
	marginModeStr := "Cross Margin"
	if !config.IsCrossMargin {
		marginModeStr = "Isolated Margin"
	}
	logger.Infof("📊 [%s] Position mode: %s", config.Name, marginModeStr)

	switch config.Exchange {
	case "binance":
		logger.Infof("🏦 [%s] Using Binance Futures trading", config.Name)
		trader = NewFuturesTrader(config.BinanceAPIKey, config.BinanceSecretKey, userID)
	case "bybit":
		logger.Infof("🏦 [%s] Using Bybit Futures trading", config.Name)
		trader = NewBybitTrader(config.BybitAPIKey, config.BybitSecretKey)
	case "okx":
		logger.Infof("🏦 [%s] Using OKX Futures trading", config.Name)
		trader = NewOKXTrader(config.OKXAPIKey, config.OKXSecretKey, config.OKXPassphrase)
	case "bitget":
		logger.Infof("🏦 [%s] Using Bitget Futures trading", config.Name)
		trader = NewBitgetTrader(config.BitgetAPIKey, config.BitgetSecretKey, config.BitgetPassphrase)
	case "hyperliquid":
		logger.Infof("🏦 [%s] Using Hyperliquid trading", config.Name)
		trader, err = NewHyperliquidTrader(config.HyperliquidPrivateKey, config.HyperliquidWalletAddr, config.HyperliquidTestnet)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Hyperliquid trader: %w", err)
		}
	case "aster":
		logger.Infof("🏦 [%s] Using Aster trading", config.Name)
		trader, err = NewAsterTrader(config.AsterUser, config.AsterSigner, config.AsterPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Aster trader: %w", err)
		}
	case "lighter":
		logger.Infof("🏦 [%s] Using LIGHTER trading", config.Name)

		if config.LighterWalletAddr == "" || config.LighterAPIKeyPrivateKey == "" {
			return nil, fmt.Errorf("Lighter requires wallet address and API Key private key")
		}

		// Lighter only supports mainnet (testnet disabled)
		trader, err = NewLighterTraderV2(
			config.LighterWalletAddr,
			config.LighterAPIKeyPrivateKey,
			config.LighterAPIKeyIndex,
			false, // Always use mainnet for Lighter
		)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize LIGHTER trader: %w", err)
		}
		logger.Infof("✓ LIGHTER trader initialized successfully")
	default:
		return nil, fmt.Errorf("unsupported trading platform: %s", config.Exchange)
	}

	// Validate initial balance configuration, auto-fetch from exchange if 0
	if config.InitialBalance <= 0 {
		logger.Infof("📊 [%s] Initial balance not set, attempting to fetch current balance from exchange...", config.Name)
		account, err := trader.GetBalance()
		if err != nil {
			return nil, fmt.Errorf("initial balance not set and unable to fetch balance from exchange: %w", err)
		}
		// Try multiple balance field names (different exchanges return different formats)
		balanceKeys := []string{"total_equity", "totalWalletBalance", "wallet_balance", "totalEq", "balance"}
		var foundBalance float64
		for _, key := range balanceKeys {
			if balance, ok := account[key].(float64); ok && balance > 0 {
				foundBalance = balance
				break
			}
		}
		if foundBalance > 0 {
			config.InitialBalance = foundBalance
			logger.Infof("✓ [%s] Auto-fetched initial balance: %.2f USDT", config.Name, foundBalance)
			// Save to database so it persists across restarts
			if st != nil {
				if err := st.Trader().UpdateInitialBalance(userID, config.ID, foundBalance); err != nil {
					logger.Infof("⚠️  [%s] Failed to save initial balance to database: %v", config.Name, err)
				} else {
					logger.Infof("✓ [%s] Initial balance saved to database", config.Name)
				}
			}
		} else {
			return nil, fmt.Errorf("initial balance must be greater than 0, please set InitialBalance in config or ensure exchange account has balance")
		}
	}

	// Get last cycle number (for recovery)
	var cycleNumber int
	if st != nil {
		cycleNumber, _ = st.Decision().GetLastCycleNumber(config.ID)
		logger.Infof("📊 [%s] Decision records will be stored to database", config.Name)
	}

	// Create strategy engine (must have strategy config)
	if config.StrategyConfig == nil {
		return nil, fmt.Errorf("[%s] strategy not configured", config.Name)
	}
	strategyEngine := decision.NewStrategyEngine(config.StrategyConfig)
	logger.Infof("✓ [%s] Using strategy engine (strategy configuration loaded)", config.Name)

	return &AutoTrader{
		id:                    config.ID,
		name:                  config.Name,
		aiModel:               config.AIModel,
		exchange:              config.Exchange,
		exchangeID:            config.ExchangeID,
		showInCompetition:     config.ShowInCompetition,
		config:                config,
		trader:                trader,
		mcpClient:             mcpClient,
		store:                 st,
		strategyEngine:        strategyEngine,
		cycleNumber:           cycleNumber,
		initialBalance:        config.InitialBalance,
		lastResetTime:         time.Now(),
		startTime:             time.Now(),
		callCount:             0,
		isRunning:             false,
		positionFirstSeenTime: make(map[string]int64),
		stopMonitorCh:         make(chan struct{}),
		monitorWg:             sync.WaitGroup{},
		peakPnLCache:          make(map[string]float64),
		peakPnLCacheMutex:     sync.RWMutex{},
		lastBalanceSyncTime:   time.Now(),
		userID:                userID,
	}, nil
}

// Run runs the automatic trading main loop
func (at *AutoTrader) Run() error {
	at.isRunning = true
	at.stopMonitorCh = make(chan struct{})
	at.startTime = time.Now()

	logger.Info("🚀 AI-driven automatic trading system started")
	logger.Infof("💰 Initial balance: %.2f USDT", at.initialBalance)
	logger.Infof("⚙️  Scan interval: %v", at.config.ScanInterval)
	logger.Info("🤖 AI will make full decisions on leverage, position size, stop loss/take profit, etc.")
	at.monitorWg.Add(1)
	defer at.monitorWg.Done()

	// Start drawdown monitoring
	at.startDrawdownMonitor()

	ticker := time.NewTicker(at.config.ScanInterval)
	defer ticker.Stop()

	// Execute immediately on first run
	if err := at.runCycle(); err != nil {
		logger.Infof("❌ Execution failed: %v", err)
	}

	for at.isRunning {
		select {
		case <-ticker.C:
			if err := at.runCycle(); err != nil {
				logger.Infof("❌ Execution failed: %v", err)
			}
		case <-at.stopMonitorCh:
			logger.Infof("[%s] ⏹ Stop signal received, exiting automatic trading main loop", at.name)
			return nil
		}
	}

	return nil
}

// Stop stops the automatic trading
func (at *AutoTrader) Stop() {
	if !at.isRunning {
		return
	}
	at.isRunning = false
	close(at.stopMonitorCh) // Notify monitoring goroutine to stop
	at.monitorWg.Wait()     // Wait for monitoring goroutine to finish
	logger.Info("⏹ Automatic trading system stopped")
}

// runCycle runs one trading cycle (using AI full decision-making)
func (at *AutoTrader) runCycle() error {
	at.callCount++

	logger.Info("\n" + strings.Repeat("=", 70) + "\n")
	logger.Infof("⏰ %s - AI decision cycle #%d", time.Now().Format("2006-01-02 15:04:05"), at.callCount)
	logger.Info(strings.Repeat("=", 70))

	// Create decision record
	record := &store.DecisionRecord{
		ExecutionLog: []string{},
		Success:      true,
	}

	// 1. Check if trading needs to be stopped
	if time.Now().Before(at.stopUntil) {
		remaining := at.stopUntil.Sub(time.Now())
		logger.Infof("⏸ Risk control: Trading paused, remaining %.0f minutes", remaining.Minutes())
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("Risk control paused, remaining %.0f minutes", remaining.Minutes())
		at.saveDecision(record)
		return nil
	}

	// 2. Reset daily P&L (reset every day)
	if time.Since(at.lastResetTime) > 24*time.Hour {
		at.dailyPnL = 0
		at.lastResetTime = time.Now()
		logger.Info("📅 Daily P&L reset")
	}

	// 4. Collect trading context
	ctx, err := at.buildTradingContext()
	if err != nil {
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("Failed to build trading context: %v", err)
		at.saveDecision(record)
		return fmt.Errorf("failed to build trading context: %w", err)
	}

	// Save equity snapshot independently (decoupled from AI decision, used for drawing profit curve)
	at.saveEquitySnapshot(ctx)

	logger.Info(strings.Repeat("=", 70))
	for _, coin := range ctx.CandidateCoins {
		record.CandidateCoins = append(record.CandidateCoins, coin.Symbol)
	}

	logger.Infof("📊 Account equity: %.2f USDT | Available: %.2f USDT | Positions: %d",
		ctx.Account.TotalEquity, ctx.Account.AvailableBalance, ctx.Account.PositionCount)

	// 5. Use strategy engine to call AI for decision
	logger.Infof("🤖 Requesting AI analysis and decision... [Strategy Engine]")
	aiDecision, err := decision.GetFullDecisionWithStrategy(ctx, at.mcpClient, at.strategyEngine, "balanced")

	if aiDecision != nil && aiDecision.AIRequestDurationMs > 0 {
		record.AIRequestDurationMs = aiDecision.AIRequestDurationMs
		logger.Infof("⏱️ AI call duration: %.2f seconds", float64(record.AIRequestDurationMs)/1000)
		record.ExecutionLog = append(record.ExecutionLog,
			fmt.Sprintf("AI call duration: %d ms", record.AIRequestDurationMs))
	}

	// Save chain of thought, decisions, and input prompt even if there's an error (for debugging)
	if aiDecision != nil {
		record.SystemPrompt = aiDecision.SystemPrompt // Save system prompt
		record.InputPrompt = aiDecision.UserPrompt
		record.CoTTrace = aiDecision.CoTTrace
		record.RawResponse = aiDecision.RawResponse // Save raw AI response for debugging
		if len(aiDecision.Decisions) > 0 {
			decisionJSON, _ := json.MarshalIndent(aiDecision.Decisions, "", "  ")
			record.DecisionJSON = string(decisionJSON)
		}
	}

	if err != nil {
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("Failed to get AI decision: %v", err)

		// Print system prompt and AI chain of thought (output even with errors for debugging)
		if aiDecision != nil {
			logger.Info("\n" + strings.Repeat("=", 70) + "\n")
			logger.Infof("📋 System prompt (error case)")
			logger.Info(strings.Repeat("=", 70))
			logger.Info(aiDecision.SystemPrompt)
			logger.Info(strings.Repeat("=", 70))

			if aiDecision.CoTTrace != "" {
				logger.Info("\n" + strings.Repeat("-", 70) + "\n")
				logger.Info("💭 AI chain of thought analysis (error case):")
				logger.Info(strings.Repeat("-", 70))
				logger.Info(aiDecision.CoTTrace)
				logger.Info(strings.Repeat("-", 70))
			}
		}

		at.saveDecision(record)
		return fmt.Errorf("failed to get AI decision: %w", err)
	}

	// // 5. Print system prompt
	// logger.Infof("\n" + strings.Repeat("=", 70))
	// logger.Infof("📋 System prompt [template: %s]", at.systemPromptTemplate)
	// logger.Info(strings.Repeat("=", 70))
	// logger.Info(decision.SystemPrompt)
	// logger.Infof(strings.Repeat("=", 70) + "\n")

	// 6. Print AI chain of thought
	// logger.Infof("\n" + strings.Repeat("-", 70))
	// logger.Info("💭 AI chain of thought analysis:")
	// logger.Info(strings.Repeat("-", 70))
	// logger.Info(decision.CoTTrace)
	// logger.Infof(strings.Repeat("-", 70) + "\n")

	// 7. Print AI decisions
	// logger.Infof("📋 AI decision list (%d items):\n", len(decision.Decisions))
	// for i, d := range decision.Decisions {
	//     logger.Infof("  [%d] %s: %s - %s", i+1, d.Symbol, d.Action, d.Reasoning)
	//     if d.Action == "open_long" || d.Action == "open_short" {
	//        logger.Infof("      Leverage: %dx | Position: %.2f USDT | Stop loss: %.4f | Take profit: %.4f",
	//           d.Leverage, d.PositionSizeUSD, d.StopLoss, d.TakeProfit)
	//     }
	// }
	logger.Info()
	logger.Info(strings.Repeat("-", 70))
	// 8. Sort decisions: ensure close positions first, then open positions (prevent position stacking overflow)
	logger.Info(strings.Repeat("-", 70))

	// 8. Sort decisions: ensure close positions first, then open positions (prevent position stacking overflow)
	sortedDecisions := sortDecisionsByPriority(aiDecision.Decisions)

	logger.Info("🔄 Execution order (optimized): Close positions first → Open positions later")
	for i, d := range sortedDecisions {
		logger.Infof("  [%d] %s %s", i+1, d.Symbol, d.Action)
	}
	logger.Info()

	// Execute decisions and record results
	for _, d := range sortedDecisions {
		actionRecord := store.DecisionAction{
			Action:     d.Action,
			Symbol:     d.Symbol,
			Quantity:   0,
			Leverage:   d.Leverage,
			Price:      0,
			StopLoss:   d.StopLoss,
			TakeProfit: d.TakeProfit,
			Confidence: d.Confidence,
			Reasoning:  d.Reasoning,
			Timestamp:  time.Now(),
			Success:    false,
		}

		if err := at.executeDecisionWithRecord(&d, &actionRecord); err != nil {
			logger.Infof("❌ Failed to execute decision (%s %s): %v", d.Symbol, d.Action, err)
			actionRecord.Error = err.Error()
			record.ExecutionLog = append(record.ExecutionLog, fmt.Sprintf("❌ %s %s failed: %v", d.Symbol, d.Action, err))
		} else {
			actionRecord.Success = true
			record.ExecutionLog = append(record.ExecutionLog, fmt.Sprintf("✓ %s %s succeeded", d.Symbol, d.Action))
			// Brief delay after successful execution
			time.Sleep(1 * time.Second)
		}

		record.Decisions = append(record.Decisions, actionRecord)
	}

	// 9. Save decision record
	if err := at.saveDecision(record); err != nil {
		logger.Infof("⚠ Failed to save decision record: %v", err)
	}

	return nil
}

// buildTradingContext builds trading context
func (at *AutoTrader) buildTradingContext() (*decision.Context, error) {
	// 1. Get account information
	balance, err := at.trader.GetBalance()
	if err != nil {
		return nil, fmt.Errorf("failed to get account balance: %w", err)
	}

	// Get account fields
	totalWalletBalance := 0.0
	totalUnrealizedProfit := 0.0
	availableBalance := 0.0

	if wallet, ok := balance["totalWalletBalance"].(float64); ok {
		totalWalletBalance = wallet
	}
	if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
		totalUnrealizedProfit = unrealized
	}
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Total Equity = Wallet balance + Unrealized profit
	totalEquity := totalWalletBalance + totalUnrealizedProfit

	// 2. Get position information
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}

	var positionInfos []decision.PositionInfo
	totalMarginUsed := 0.0

	// Current position key set (for cleaning up closed position records)
	currentPositionKeys := make(map[string]bool)

	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity // Short position quantity is negative, convert to positive
		}

		// Skip closed positions (quantity = 0), prevent "ghost positions" from being passed to AI
		if quantity == 0 {
			continue
		}

		unrealizedPnl := pos["unRealizedProfit"].(float64)
		liquidationPrice := pos["liquidationPrice"].(float64)

		// Calculate margin used (estimated)
		leverage := 10 // Default value, should actually be fetched from position info
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}
		marginUsed := (quantity * markPrice) / float64(leverage)
		totalMarginUsed += marginUsed

		// Calculate P&L percentage (based on margin, considering leverage)
		pnlPct := calculatePnLPercentage(unrealizedPnl, marginUsed)

		// Get position open time from exchange (preferred) or fallback to local tracking
		posKey := symbol + "_" + side
		currentPositionKeys[posKey] = true

		var updateTime int64
		// Priority 1: Get from database (trader_positions table) - most accurate
		if at.store != nil {
			if dbPos, err := at.store.Position().GetOpenPositionBySymbol(at.id, symbol, side); err == nil && dbPos != nil {
				if !dbPos.EntryTime.IsZero() {
					updateTime = dbPos.EntryTime.UnixMilli()
				}
			}
		}
		// Priority 2: Get from exchange API (Bybit: createdTime, OKX: createdTime)
		if updateTime == 0 {
			if createdTime, ok := pos["createdTime"].(int64); ok && createdTime > 0 {
				updateTime = createdTime
			}
		}
		// Priority 3: Fallback to local tracking
		if updateTime == 0 {
			if _, exists := at.positionFirstSeenTime[posKey]; !exists {
				at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()
			}
			updateTime = at.positionFirstSeenTime[posKey]
		}

		// Get peak profit rate for this position
		at.peakPnLCacheMutex.RLock()
		peakPnlPct := at.peakPnLCache[posKey]
		at.peakPnLCacheMutex.RUnlock()

		positionInfos = append(positionInfos, decision.PositionInfo{
			Symbol:           symbol,
			Side:             side,
			EntryPrice:       entryPrice,
			MarkPrice:        markPrice,
			Quantity:         quantity,
			Leverage:         leverage,
			UnrealizedPnL:    unrealizedPnl,
			UnrealizedPnLPct: pnlPct,
			PeakPnLPct:       peakPnlPct,
			LiquidationPrice: liquidationPrice,
			MarginUsed:       marginUsed,
			UpdateTime:       updateTime,
		})
	}

	// Clean up closed position records
	for key := range at.positionFirstSeenTime {
		if !currentPositionKeys[key] {
			delete(at.positionFirstSeenTime, key)
		}
	}

	// 3. Use strategy engine to get candidate coins (must have strategy engine)
	if at.strategyEngine == nil {
		return nil, fmt.Errorf("trader has no strategy engine configured")
	}
	candidateCoins, err := at.strategyEngine.GetCandidateCoins()
	if err != nil {
		return nil, fmt.Errorf("failed to get candidate coins: %w", err)
	}
	logger.Infof("📋 [%s] Strategy engine fetched candidate coins: %d", at.name, len(candidateCoins))

	// 4. Calculate total P&L
	totalPnL := totalEquity - at.initialBalance
	totalPnLPct := 0.0
	if at.initialBalance > 0 {
		totalPnLPct = (totalPnL / at.initialBalance) * 100
	}

	marginUsedPct := 0.0
	if totalEquity > 0 {
		marginUsedPct = (totalMarginUsed / totalEquity) * 100
	}

	// 5. Get leverage from strategy config
	strategyConfig := at.strategyEngine.GetConfig()
	btcEthLeverage := strategyConfig.RiskControl.BTCETHMaxLeverage
	altcoinLeverage := strategyConfig.RiskControl.AltcoinMaxLeverage
	logger.Infof("📋 [%s] Strategy leverage config: BTC/ETH=%dx, Altcoin=%dx", at.name, btcEthLeverage, altcoinLeverage)

	// 6. Build context
	ctx := &decision.Context{
		CurrentTime:     time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		RuntimeMinutes:  int(time.Since(at.startTime).Minutes()),
		CallCount:       at.callCount,
		BTCETHLeverage:  btcEthLeverage,
		AltcoinLeverage: altcoinLeverage,
		Account: decision.AccountInfo{
			TotalEquity:      totalEquity,
			AvailableBalance: availableBalance,
			UnrealizedPnL:    totalUnrealizedProfit,
			TotalPnL:         totalPnL,
			TotalPnLPct:      totalPnLPct,
			MarginUsed:       totalMarginUsed,
			MarginUsedPct:    marginUsedPct,
			PositionCount:    len(positionInfos),
		},
		Positions:      positionInfos,
		CandidateCoins: candidateCoins,
	}

	// 7. Add recent closed trades (if store is available)
	if at.store != nil {
		// Get recent 10 closed trades for AI context
		recentTrades, err := at.store.Position().GetRecentTrades(at.id, 10)
		if err != nil {
			logger.Infof("⚠️ [%s] Failed to get recent trades: %v", at.name, err)
		} else {
			logger.Infof("📊 [%s] Found %d recent closed trades for AI context", at.name, len(recentTrades))
			for _, trade := range recentTrades {
				ctx.RecentOrders = append(ctx.RecentOrders, decision.RecentOrder{
					Symbol:       trade.Symbol,
					Side:         trade.Side,
					EntryPrice:   trade.EntryPrice,
					ExitPrice:    trade.ExitPrice,
					RealizedPnL:  trade.RealizedPnL,
					PnLPct:       trade.PnLPct,
					EntryTime:    trade.EntryTime,
					ExitTime:     trade.ExitTime,
					HoldDuration: trade.HoldDuration,
				})
			}
		}
	} else {
		logger.Infof("⚠️ [%s] Store is nil, cannot get recent trades", at.name)
	}

	// 8. Get quantitative data (if enabled in strategy config)
	if strategyConfig.Indicators.EnableQuantData && strategyConfig.Indicators.QuantDataAPIURL != "" {
		// Collect symbols to query (candidate coins + position coins)
		symbolsToQuery := make(map[string]bool)
		for _, coin := range candidateCoins {
			symbolsToQuery[coin.Symbol] = true
		}
		for _, pos := range positionInfos {
			symbolsToQuery[pos.Symbol] = true
		}

		symbols := make([]string, 0, len(symbolsToQuery))
		for sym := range symbolsToQuery {
			symbols = append(symbols, sym)
		}

		logger.Infof("📊 [%s] Fetching quantitative data for %d symbols...", at.name, len(symbols))
		ctx.QuantDataMap = at.strategyEngine.FetchQuantDataBatch(symbols)
		logger.Infof("📊 [%s] Successfully fetched quantitative data for %d symbols", at.name, len(ctx.QuantDataMap))
	}

	// 9. Get OI ranking data (market-wide position changes)
	if strategyConfig.Indicators.EnableOIRanking {
		logger.Infof("📊 [%s] Fetching OI ranking data...", at.name)
		ctx.OIRankingData = at.strategyEngine.FetchOIRankingData()
		if ctx.OIRankingData != nil {
			logger.Infof("📊 [%s] OI ranking data ready: %d top, %d low positions",
				at.name, len(ctx.OIRankingData.TopPositions), len(ctx.OIRankingData.LowPositions))
		}
	}

	return ctx, nil
}

// executeDecisionWithRecord executes AI decision and records detailed information
func (at *AutoTrader) executeDecisionWithRecord(decision *decision.Decision, actionRecord *store.DecisionAction) error {
	switch decision.Action {
	case "open_long":
		return at.executeOpenLongWithRecord(decision, actionRecord)
	case "open_short":
		return at.executeOpenShortWithRecord(decision, actionRecord)
	case "close_long":
		return at.executeCloseLongWithRecord(decision, actionRecord)
	case "close_short":
		return at.executeCloseShortWithRecord(decision, actionRecord)
	case "reduce_long":
		// 减仓：复用平仓逻辑（通过 CloseRatio 控制减仓比例）
		logger.Infof("  📉 Reduce long: %s (ratio=%.2f)", decision.Symbol, decision.CloseRatio)
		return at.executeCloseLongWithRecord(decision, actionRecord)
	case "reduce_short":
		// 减仓：复用平仓逻辑（通过 CloseRatio 控制减仓比例）
		logger.Infof("  📉 Reduce short: %s (ratio=%.2f)", decision.Symbol, decision.CloseRatio)
		return at.executeCloseShortWithRecord(decision, actionRecord)
	case "hold", "wait":
		// No execution needed, just record
		return nil
	default:
		return fmt.Errorf("unknown action: %s", decision.Action)
	}
}

// SetStopLoss exposes the underlying trader's stop-loss capability for copytrade SL management.
// 跟单 v3 风控通过 copytrade.StopLossManager 接口（type assertion）调用本方法，
// 在「执行后用实际成交均价精确重挂 SL」「加仓/减仓前撤旧 SL」等路径生效。
// 不影响 AI 决策模式的 SL 流程（AI 走 executeOpenLong/Short 内部自动挂 SL）。
func (at *AutoTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	return at.trader.SetStopLoss(symbol, positionSide, quantity, stopPrice)
}

// CancelStopLossOrders exposes the underlying trader's SL cancel capability for copytrade SL management.
// 跟单 v3 风控通过 copytrade.StopLossManager 接口调用本方法，
// 在每次开仓/加仓/减仓**前**清理可能残留的旧 algo SL 单，避免多挂导致状态污染。
func (at *AutoTrader) CancelStopLossOrders(symbol string) error {
	return at.trader.CancelStopLossOrders(symbol)
}

// GetMarketPrice exposes the underlying trader's market price query for copytrade SL management.
// 跟单 v3 风控用本方法在「拿不到实际成交价」时降级取市价作为入场价基准。
func (at *AutoTrader) GetMarketPrice(symbol string) (float64, error) {
	return at.trader.GetMarketPrice(symbol)
}

// SupportsProtectiveStops reports whether the underlying exchange trader can
// place exchange-managed protective stops (Copy Guard). It inspects the
// concrete trader rather than the AutoTrader wrapper: AutoTrader always
// satisfies ProtectiveStopManager at compile time (the methods exist) but they
// return errors at runtime when the wrapped exchange lacks support. Callers
// (Copy Guard startup validation) use this to fail fast on incapable execution
// venues instead of degrading into a protective-stop retry loop.
func (at *AutoTrader) SupportsProtectiveStops() bool {
	_, ok := at.trader.(ProtectiveStopManager)
	return ok
}

func (at *AutoTrader) ValidateCopyGuardCapabilities() error {
	checks := []struct {
		name string
		ok   bool
	}{
		{"protective stop management", implementsTraderCapability[ProtectiveStopManager](at.trader)},
		{"uncached position refresh", implementsTraderCapability[FreshPositionProvider](at.trader)},
		{"client order lookup", implementsTraderCapability[ClientOrderStatusProvider](at.trader)},
		{"idempotent preserving copy orders", implementsTraderCapability[CopyTradeIdempotentOrderExecutor](at.trader)},
		{"exact execution instrument resolution", implementsTraderCapability[ExecutionInstrumentResolver](at.trader)},
	}
	for _, check := range checks {
		if !check.ok {
			return fmt.Errorf("execution exchange does not support %s", check.name)
		}
	}
	_, byPosition := at.trader.(ClosedPnLByPositionProvider)
	_, bySymbol := at.trader.(ClosedPnLBySymbolProvider)
	if !byPosition && !bySymbol {
		return fmt.Errorf("execution exchange does not support precise closed PnL reconciliation")
	}
	return nil
}

func implementsTraderCapability[T any](value interface{}) bool {
	_, ok := value.(T)
	return ok
}

func (at *AutoTrader) ResolveExecutionInstrument(symbol string) (*ExecutionInstrument, error) {
	resolver, ok := at.trader.(ExecutionInstrumentResolver)
	if !ok {
		return nil, fmt.Errorf("%w: exchange has no exact instrument resolver", ErrExecutionInstrumentUnsupported)
	}
	return resolver.ResolveExecutionInstrument(symbol)
}

func (at *AutoTrader) PlaceProtectiveStop(req ProtectiveStopRequest) (*ProtectiveStopOrder, error) {
	mgr, ok := at.trader.(ProtectiveStopManager)
	if !ok {
		return nil, fmt.Errorf("exchange does not support precise protective stops")
	}
	return mgr.PlaceProtectiveStop(req)
}
func (at *AutoTrader) AmendProtectiveStop(algoID string, req ProtectiveStopRequest) error {
	mgr, ok := at.trader.(ProtectiveStopManager)
	if !ok {
		return fmt.Errorf("exchange does not support precise protective stops")
	}
	return mgr.AmendProtectiveStop(algoID, req)
}
func (at *AutoTrader) EnsureProtectiveStop(existing *ProtectiveStopOrder, req ProtectiveStopRequest) (*ProtectiveStopEnsureResult, error) {
	if ensurer, ok := at.trader.(ProtectiveStopEnsurer); ok {
		return ensurer.EnsureProtectiveStop(existing, req)
	}
	mgr, ok := at.trader.(ProtectiveStopManager)
	if !ok {
		return nil, fmt.Errorf("exchange does not support precise protective stops")
	}
	if existing == nil || existing.AlgoID == "" {
		placed, err := mgr.PlaceProtectiveStop(req)
		return &ProtectiveStopEnsureResult{Current: placed}, err
	}
	if err := mgr.AmendProtectiveStop(existing.AlgoID, req); err != nil {
		return nil, err
	}
	return &ProtectiveStopEnsureResult{Current: &ProtectiveStopOrder{AlgoID: existing.AlgoID, ClientID: existing.ClientID, Symbol: req.Symbol, PositionSide: req.PositionSide, MarginMode: req.MarginMode, Quantity: req.Quantity, TriggerPrice: req.TriggerPrice, TriggerType: req.TriggerType, State: "live"}}, nil
}
func (at *AutoTrader) GetProtectiveStop(algoID, symbol string) (*ProtectiveStopOrder, error) {
	mgr, ok := at.trader.(ProtectiveStopManager)
	if !ok {
		return nil, fmt.Errorf("exchange does not support precise protective stops")
	}
	return mgr.GetProtectiveStop(algoID, symbol)
}
func (at *AutoTrader) GetProtectiveStopByClientID(clientID, symbol string) (*ProtectiveStopOrder, error) {
	mgr, ok := at.trader.(ProtectiveStopManager)
	if !ok {
		return nil, fmt.Errorf("exchange does not support precise protective stops")
	}
	return mgr.GetProtectiveStopByClientID(clientID, symbol)
}
func (at *AutoTrader) GetPositionsFresh() ([]map[string]interface{}, error) {
	if provider, ok := at.trader.(FreshPositionProvider); ok {
		return provider.GetPositionsFresh()
	}
	return at.trader.GetPositions()
}
func (at *AutoTrader) CancelProtectiveStop(algoID, symbol string) error {
	mgr, ok := at.trader.(ProtectiveStopManager)
	if !ok {
		return fmt.Errorf("exchange does not support precise protective stops")
	}
	return mgr.CancelProtectiveStop(algoID, symbol)
}

func (at *AutoTrader) GetClosedPnL(start time.Time, limit int) ([]ClosedPnLRecord, error) {
	return at.trader.GetClosedPnL(start, limit)
}
func (at *AutoTrader) GetClosedPnLByPositionID(symbol, posID string, limit int) ([]ClosedPnLRecord, error) {
	p, ok := at.trader.(ClosedPnLByPositionProvider)
	if !ok {
		return nil, fmt.Errorf("exchange does not support closed PnL lookup by position id")
	}
	return p.GetClosedPnLByPositionID(symbol, posID, limit)
}
func (at *AutoTrader) GetClosedPnLBySymbol(symbol string, start time.Time, limit int) ([]ClosedPnLRecord, error) {
	p, ok := at.trader.(ClosedPnLBySymbolProvider)
	if ok {
		return p.GetClosedPnLBySymbol(symbol, start, limit)
	}
	records, err := at.trader.GetClosedPnL(start, limit)
	if err != nil {
		return nil, err
	}
	filtered := make([]ClosedPnLRecord, 0, len(records))
	for _, record := range records {
		if strings.EqualFold(record.Symbol, symbol) {
			filtered = append(filtered, record)
		}
	}
	return filtered, nil
}
func (at *AutoTrader) GetOrderStatus(symbol, orderID string) (map[string]interface{}, error) {
	return at.trader.GetOrderStatus(symbol, orderID)
}
func (at *AutoTrader) GetOrderStatusByClientID(symbol, clientOrderID string) (map[string]interface{}, error) {
	p, ok := at.trader.(ClientOrderStatusProvider)
	if !ok {
		return nil, fmt.Errorf("exchange does not support client order lookup")
	}
	return p.GetOrderStatusByClientID(symbol, clientOrderID)
}

// ExecuteDecision executes a trading decision from external sources (e.g., debate consensus)
// This is a public method that can be called by other modules
func (at *AutoTrader) ExecuteDecision(d *decision.Decision) error {
	logger.Infof("[%s] Executing external decision: %s %s", at.name, d.Action, d.Symbol)

	// Create a minimal action record for tracking
	actionRecord := &store.DecisionAction{
		Symbol:     d.Symbol,
		Action:     d.Action,
		Leverage:   d.Leverage,
		StopLoss:   d.StopLoss,
		TakeProfit: d.TakeProfit,
		Confidence: d.Confidence,
		Reasoning:  d.Reasoning,
	}

	// Execute the decision
	err := at.executeDecisionWithRecord(d, actionRecord)
	if err != nil {
		logger.Errorf("[%s] External decision execution failed: %v", at.name, err)
		return err
	}

	logger.Infof("[%s] External decision executed successfully: %s %s", at.name, d.Action, d.Symbol)
	return nil
}

// ErrPartialCloseSkipped 减仓因边界约束（仓位太小无法按比例减 / 减仓价值低于
// 交易所最小订单价值）被跳过，未向交易所下任何订单。
// 必须以错误形式向上传递：旧版直接 return nil 伪装成功，导致跟单 integration
// 误更新 lastKnownSize/减仓计数并按空执行重挂止损单。
// 调用方用 errors.Is 识别后按"跳过"处理（不更新 mapping、不告警、不熔断）。
var ErrPartialCloseSkipped = errors.New("partial close skipped (no order placed)")

// fillCopyTradeMarginMode 跟单开仓决策缺省 MarginMode 时回填交易员自身配置。
// 缺省来源：SyncMarginMode=false（引擎主动清空）或数据源未提供模式。
// 回填后重复仓位检查、SetMarginMode、mapping 记录、Copy Guard 保护单 tdMode
// 全链路统一使用真实模式；AI 决策（非跟单）不受影响。
func (at *AutoTrader) fillCopyTradeMarginMode(isCopyTrade bool, d *decision.Decision) {
	if !isCopyTrade || d.MarginMode != "" {
		return
	}
	if at.config.IsCrossMargin {
		d.MarginMode = "cross"
	} else {
		d.MarginMode = "isolated"
	}
	logger.Infof("  📊 跟单开仓使用交易员自身保证金模式: %s（未同步领航员模式）", d.MarginMode)
}

func (at *AutoTrader) openLongOrder(copyTrade bool, symbol string, quantity float64, leverage int, clientOrderID string) (map[string]interface{}, error) {
	if copyTrade {
		if clientOrderID != "" {
			if executor, ok := at.trader.(CopyTradeIdempotentOrderExecutor); ok {
				return executor.OpenLongPreservingOrdersWithClientID(symbol, quantity, leverage, clientOrderID)
			}
		}
		if executor, ok := at.trader.(CopyTradeOrderExecutor); ok {
			return executor.OpenLongPreservingOrders(symbol, quantity, leverage)
		}
	}
	return at.trader.OpenLong(symbol, quantity, leverage)
}

func (at *AutoTrader) openShortOrder(copyTrade bool, symbol string, quantity float64, leverage int, clientOrderID string) (map[string]interface{}, error) {
	if copyTrade {
		if clientOrderID != "" {
			if executor, ok := at.trader.(CopyTradeIdempotentOrderExecutor); ok {
				return executor.OpenShortPreservingOrdersWithClientID(symbol, quantity, leverage, clientOrderID)
			}
		}
		if executor, ok := at.trader.(CopyTradeOrderExecutor); ok {
			return executor.OpenShortPreservingOrders(symbol, quantity, leverage)
		}
	}
	return at.trader.OpenShort(symbol, quantity, leverage)
}

func (at *AutoTrader) closeLongOrder(copyTrade bool, symbol string, quantity float64, clientOrderID string) (map[string]interface{}, error) {
	if copyTrade {
		if clientOrderID != "" {
			if executor, ok := at.trader.(CopyTradeIdempotentOrderExecutor); ok {
				return executor.CloseLongPreservingOrdersWithClientID(symbol, quantity, clientOrderID)
			}
		}
		if executor, ok := at.trader.(CopyTradeOrderExecutor); ok {
			return executor.CloseLongPreservingOrders(symbol, quantity)
		}
	}
	return at.trader.CloseLong(symbol, quantity)
}

func (at *AutoTrader) closeShortOrder(copyTrade bool, symbol string, quantity float64, clientOrderID string) (map[string]interface{}, error) {
	if copyTrade {
		if clientOrderID != "" {
			if executor, ok := at.trader.(CopyTradeIdempotentOrderExecutor); ok {
				return executor.CloseShortPreservingOrdersWithClientID(symbol, quantity, clientOrderID)
			}
		}
		if executor, ok := at.trader.(CopyTradeOrderExecutor); ok {
			return executor.CloseShortPreservingOrders(symbol, quantity)
		}
	}
	return at.trader.CloseShort(symbol, quantity)
}

func captureDecisionOrderIDs(d *decision.Decision, order map[string]interface{}) {
	if d == nil || order == nil {
		return
	}
	if id, ok := order["orderId"].(string); ok {
		d.ExchangeOrderID = id
	}
	if id, ok := order["clOrdId"].(string); ok && id != "" {
		d.ClientOrderID = id
	}
}

// executeOpenLongWithRecord executes open long position and records detailed information
func (at *AutoTrader) executeOpenLongWithRecord(decision *decision.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  📈 Open long: %s", decision.Symbol)

	// ⚠️ Get current positions for multiple checks
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}

	// 检查是否为跟单操作（Reasoning 包含 "Copy trading"）
	isCopyTrade := strings.Contains(decision.Reasoning, "Copy trading")
	// 检查是否为加仓操作（跟单加仓时 Reasoning 包含 "add"）
	isAddPosition := isCopyTrade && strings.Contains(strings.ToLower(decision.Reasoning), "add")

	// 跟单开仓 MarginMode 为空（SyncMarginMode=false 或数据源未提供）：
	// 回填为交易员自身配置的模式，让后续的重复仓位检查、mapping 记录、
	// Copy Guard 保护单 tdMode 都基于真实模式，而不是残留空串。
	at.fillCopyTradeMarginMode(isCopyTrade, decision)

	// [CODE ENFORCED] Check max positions limit (跟单模式跳过，领航员已通过风控)
	if !isCopyTrade {
		if err := at.enforceMaxPositions(len(positions)); err != nil {
			return err
		}
	}

	// Check if there's already a position in the same symbol and direction
	// 跟单加仓时跳过此检查
	if !isAddPosition {
		for _, pos := range positions {
			if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
				// 跟单模式下，如果 mgnMode 不同（全仓 vs 逐仓），视为不同仓位，允许开仓
				if isCopyTrade && decision.MarginMode != "" {
					posMgnMode, _ := pos["mgnMode"].(string)
					if posMgnMode == "" {
						posMgnMode = "cross" // 默认全仓
					}
					if posMgnMode != decision.MarginMode {
						logger.Infof("  📊 跟单模式：本地已有 %s 仓位(模式=%s)，目标是 %s 模式，允许新开仓",
							decision.Symbol, posMgnMode, decision.MarginMode)
						continue // 不同模式，不视为重复仓位
					}
				}
				return fmt.Errorf("❌ %s already has long position, close it first", decision.Symbol)
			}
		}
	} else {
		logger.Infof("  📊 跟单加仓，跳过重复仓位检查")
	}

	// Get current price
	// 跟单模式：使用交易所自己的 API 获取价格（避免 Binance 交易对不兼容问题，如 PEPE vs 1000PEPE）
	var currentPrice float64
	if isCopyTrade {
		price, err := at.trader.GetMarketPrice(decision.Symbol)
		if err != nil {
			// Fallback: 使用信号中的入场价格
			if decision.EntryPrice > 0 {
				currentPrice = decision.EntryPrice
				logger.Infof("  📊 跟单模式：使用信号入场价 %.6f (GetMarketPrice failed: %v)", currentPrice, err)
			} else {
				return fmt.Errorf("failed to get market price: %w", err)
			}
		} else {
			currentPrice = price
			logger.Infof("  📊 跟单模式：使用交易所实时价格 %.6f", currentPrice)
		}
	} else {
		marketData, err := market.Get(decision.Symbol)
		if err != nil {
			return err
		}
		currentPrice = marketData.CurrentPrice
	}

	// Get balance (needed for multiple checks)
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Get equity for position value ratio check
	equity := 0.0
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		equity = eq
	} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
		equity = eq
	} else {
		equity = availableBalance // Fallback to available balance
	}

	// [CODE ENFORCED] Position Value Ratio Check (跟单模式只提示不封顶，保持领航员盈亏比)
	if isCopyTrade {
		// 跟单模式：只提示风险，不封顶（保持领航员盈亏比）
		_, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
		if wasCapped {
			logger.Infof("  📊 跟单模式，忽略仓位封顶限制（保持领航员盈亏比）")
		}
	} else {
		adjustedPositionSize, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
		if wasCapped {
			decision.PositionSizeUSD = adjustedPositionSize
		}
	}

	// ⚠️ Auto-adjust position size if insufficient margin
	// Formula: totalRequired = positionSize/leverage + positionSize*0.001 + positionSize/leverage*0.01
	//        = positionSize * (1.01/leverage + 0.001)
	marginFactor := 1.01/float64(decision.Leverage) + 0.001
	maxAffordablePositionSize := availableBalance / marginFactor

	actualPositionSize := decision.PositionSizeUSD
	if actualPositionSize > maxAffordablePositionSize {
		// Use 98% of max to leave buffer for price fluctuation
		adjustedSize := maxAffordablePositionSize * 0.98
		logger.Infof("  ⚠️ Position size %.2f exceeds max affordable %.2f, auto-reducing to %.2f",
			actualPositionSize, maxAffordablePositionSize, adjustedSize)
		actualPositionSize = adjustedSize
		decision.PositionSizeUSD = actualPositionSize
	}

	// [CODE ENFORCED] Minimum position size check (跟单模式跳过，领航员已通过风控)
	if !isCopyTrade {
		if err := at.enforceMinPositionSize(decision.PositionSizeUSD); err != nil {
			return err
		}
	} else {
		logger.Infof("  📊 跟单模式，跳过最小仓位和最大仓位检查")
	}

	// Calculate quantity with adjusted position size
	quantity := actualPositionSize / currentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = currentPrice

	// Set margin mode: 跟单模式下使用 decision 中的模式，否则使用配置
	isCrossMargin := at.config.IsCrossMargin
	if decision.MarginMode != "" {
		isCrossMargin = decision.MarginMode == "cross"
		logger.Infof("  📊 使用跟单指定的保证金模式: %s", decision.MarginMode)
	}
	if err := at.trader.SetMarginMode(decision.Symbol, isCrossMargin); err != nil {
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
		// Continue execution, doesn't affect trading
	}

	// Open position
	order, err := at.openLongOrder(isCopyTrade, decision.Symbol, quantity, decision.Leverage, decision.ClientOrderID)
	if err != nil {
		// 🔁 PR-4 / 修复 E'：跟单开仓遇到保证金不足，自动减半重试一次。
		//
		// 触发条件（同时满足）：
		//   1. isCopyTrade（仅跟单路径，AI 决策不受影响）
		//   2. isInsufficientMarginError(err)（精确识别保证金/可用余额不足）
		//
		// 重试动作：quantity / actualPositionSize 各减半，再次调用 OpenLong。
		// 重试失败：返回包装错误（包含原始错误 + 重试错误），便于熔断与告警识别。
		//
		// 不重试的情况：
		//   - 非跟单（AI 决策已有自己的余额预扣逻辑）
		//   - 非保证金错误（如 size below minSz、风控拒单等，减半通常无济于事）
		if isCopyTrade && isInsufficientMarginError(err) {
			retryQty := quantity * 0.5
			retrySize := actualPositionSize * 0.5
			logger.Warnf("  🔁 [%s] 跟单开多保证金不足，减半重试一次 | qty=%.4f→%.4f USD=%.2f→%.2f | 原err=%v",
				decision.Symbol, quantity, retryQty, actualPositionSize, retrySize, err)
			order2, err2 := at.openLongOrder(isCopyTrade, decision.Symbol, retryQty, decision.Leverage, decision.ClientOrderID)
			if err2 != nil {
				return fmt.Errorf("open long retry-halved failed: initial=%v retry=%v", err, err2)
			}
			order = order2
			quantity = retryQty
			actualPositionSize = retrySize
			actionRecord.Quantity = quantity
			decision.PositionSizeUSD = actualPositionSize
			logger.Infof("  ✓ [%s] 跟单开多减半重试成功 | qty=%.4f USD=%.2f", decision.Symbol, quantity, actualPositionSize)
		} else {
			return err
		}
	}
	captureDecisionOrderIDs(decision, order)

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	logger.Infof("  ✓ Position opened successfully, order ID: %v, quantity: %.4f", order["orderId"], quantity)

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "open_long", quantity, currentPrice, decision.Leverage, 0)

	// Record position opening time
	posKey := decision.Symbol + "_long"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// Set stop loss and take profit
	// 🔑 守卫：StopLoss/TakeProfit <= 0 时跳过挂单
	// - AI 决策模式：AI 总是会填正值，行为不变
	// - 跟单模式：buildDecisionV2 在用户禁用风控或开仓即触发风险时会填 0，应跳过避免挂出无效 algo 单
	if decision.StopLoss > 0 {
		if err := at.trader.SetStopLoss(decision.Symbol, "LONG", quantity, decision.StopLoss); err != nil {
			logger.Infof("  ⚠ Failed to set stop loss: %v", err)
		}
	}
	if decision.TakeProfit > 0 {
		if err := at.trader.SetTakeProfit(decision.Symbol, "LONG", quantity, decision.TakeProfit); err != nil {
			logger.Infof("  ⚠ Failed to set take profit: %v", err)
		}
	}

	return nil
}

// executeOpenShortWithRecord executes open short position and records detailed information
func (at *AutoTrader) executeOpenShortWithRecord(decision *decision.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  📉 Open short: %s", decision.Symbol)

	// ⚠️ Get current positions for multiple checks
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}

	// 检查是否为跟单操作（Reasoning 包含 "Copy trading"）
	isCopyTrade := strings.Contains(decision.Reasoning, "Copy trading")
	// 检查是否为加仓操作（跟单加仓时 Reasoning 包含 "add"）
	isAddPosition := isCopyTrade && strings.Contains(strings.ToLower(decision.Reasoning), "add")

	// 跟单开仓 MarginMode 回填（与 executeOpenLongWithRecord 对称，见其注释）
	at.fillCopyTradeMarginMode(isCopyTrade, decision)

	// [CODE ENFORCED] Check max positions limit (跟单模式跳过，领航员已通过风控)
	if !isCopyTrade {
		if err := at.enforceMaxPositions(len(positions)); err != nil {
			return err
		}
	}

	// Check if there's already a position in the same symbol and direction
	// 跟单加仓时跳过此检查
	if !isAddPosition {
		for _, pos := range positions {
			if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
				// 跟单模式下，如果 mgnMode 不同（全仓 vs 逐仓），视为不同仓位，允许开仓
				if isCopyTrade && decision.MarginMode != "" {
					posMgnMode, _ := pos["mgnMode"].(string)
					if posMgnMode == "" {
						posMgnMode = "cross" // 默认全仓
					}
					if posMgnMode != decision.MarginMode {
						logger.Infof("  📊 跟单模式：本地已有 %s 仓位(模式=%s)，目标是 %s 模式，允许新开仓",
							decision.Symbol, posMgnMode, decision.MarginMode)
						continue // 不同模式，不视为重复仓位
					}
				}
				return fmt.Errorf("❌ %s already has short position, close it first", decision.Symbol)
			}
		}
	} else {
		logger.Infof("  📊 跟单加仓，跳过重复仓位检查")
	}

	// Get current price
	// 跟单模式：使用交易所自己的 API 获取价格（避免 Binance 交易对不兼容问题，如 PEPE vs 1000PEPE）
	var currentPrice float64
	if isCopyTrade {
		price, err := at.trader.GetMarketPrice(decision.Symbol)
		if err != nil {
			// Fallback: 使用信号中的入场价格
			if decision.EntryPrice > 0 {
				currentPrice = decision.EntryPrice
				logger.Infof("  📊 跟单模式：使用信号入场价 %.6f (GetMarketPrice failed: %v)", currentPrice, err)
			} else {
				return fmt.Errorf("failed to get market price: %w", err)
			}
		} else {
			currentPrice = price
			logger.Infof("  📊 跟单模式：使用交易所实时价格 %.6f", currentPrice)
		}
	} else {
		marketData, err := market.Get(decision.Symbol)
		if err != nil {
			return err
		}
		currentPrice = marketData.CurrentPrice
	}

	// Get balance (needed for multiple checks)
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Get equity for position value ratio check
	equity := 0.0
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		equity = eq
	} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
		equity = eq
	} else {
		equity = availableBalance // Fallback to available balance
	}

	// [CODE ENFORCED] Position Value Ratio Check (跟单模式只提示不封顶，保持领航员盈亏比)
	if isCopyTrade {
		// 跟单模式：只提示风险，不封顶（保持领航员盈亏比）
		_, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
		if wasCapped {
			logger.Infof("  📊 跟单模式，忽略仓位封顶限制（保持领航员盈亏比）")
		}
	} else {
		adjustedPositionSize, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
		if wasCapped {
			decision.PositionSizeUSD = adjustedPositionSize
		}
	}

	// ⚠️ Auto-adjust position size if insufficient margin
	// Formula: totalRequired = positionSize/leverage + positionSize*0.001 + positionSize/leverage*0.01
	//        = positionSize * (1.01/leverage + 0.001)
	marginFactor := 1.01/float64(decision.Leverage) + 0.001
	maxAffordablePositionSize := availableBalance / marginFactor

	actualPositionSize := decision.PositionSizeUSD
	if actualPositionSize > maxAffordablePositionSize {
		// Use 98% of max to leave buffer for price fluctuation
		adjustedSize := maxAffordablePositionSize * 0.98
		logger.Infof("  ⚠️ Position size %.2f exceeds max affordable %.2f, auto-reducing to %.2f",
			actualPositionSize, maxAffordablePositionSize, adjustedSize)
		actualPositionSize = adjustedSize
		decision.PositionSizeUSD = actualPositionSize
	}

	// [CODE ENFORCED] Minimum position size check (跟单模式跳过，领航员已通过风控)
	if !isCopyTrade {
		if err := at.enforceMinPositionSize(decision.PositionSizeUSD); err != nil {
			return err
		}
	} else {
		logger.Infof("  📊 跟单模式，跳过最小仓位和最大仓位检查")
	}

	// Calculate quantity with adjusted position size
	quantity := actualPositionSize / currentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = currentPrice

	// Set margin mode: 跟单模式下使用 decision 中的模式，否则使用配置
	isCrossMargin := at.config.IsCrossMargin
	if decision.MarginMode != "" {
		isCrossMargin = decision.MarginMode == "cross"
		logger.Infof("  📊 使用跟单指定的保证金模式: %s", decision.MarginMode)
	}
	if err := at.trader.SetMarginMode(decision.Symbol, isCrossMargin); err != nil {
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
		// Continue execution, doesn't affect trading
	}

	// Open position
	order, err := at.openShortOrder(isCopyTrade, decision.Symbol, quantity, decision.Leverage, decision.ClientOrderID)
	if err != nil {
		// 🔁 PR-4 / 修复 E'：跟单开空保证金不足，自动减半重试一次。逻辑与 OpenLong 对称。
		// 详见 executeOpenLongWithRecord 中相同位置的注释。
		if isCopyTrade && isInsufficientMarginError(err) {
			retryQty := quantity * 0.5
			retrySize := actualPositionSize * 0.5
			logger.Warnf("  🔁 [%s] 跟单开空保证金不足，减半重试一次 | qty=%.4f→%.4f USD=%.2f→%.2f | 原err=%v",
				decision.Symbol, quantity, retryQty, actualPositionSize, retrySize, err)
			order2, err2 := at.openShortOrder(isCopyTrade, decision.Symbol, retryQty, decision.Leverage, decision.ClientOrderID)
			if err2 != nil {
				return fmt.Errorf("open short retry-halved failed: initial=%v retry=%v", err, err2)
			}
			order = order2
			quantity = retryQty
			actualPositionSize = retrySize
			actionRecord.Quantity = quantity
			decision.PositionSizeUSD = actualPositionSize
			logger.Infof("  ✓ [%s] 跟单开空减半重试成功 | qty=%.4f USD=%.2f", decision.Symbol, quantity, actualPositionSize)
		} else {
			return err
		}
	}
	captureDecisionOrderIDs(decision, order)

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	logger.Infof("  ✓ Position opened successfully, order ID: %v, quantity: %.4f", order["orderId"], quantity)

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "open_short", quantity, currentPrice, decision.Leverage, 0)

	// Record position opening time
	posKey := decision.Symbol + "_short"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// Set stop loss and take profit
	// 🔑 守卫：StopLoss/TakeProfit <= 0 时跳过挂单（同 executeOpenLongWithRecord 注释）
	if decision.StopLoss > 0 {
		if err := at.trader.SetStopLoss(decision.Symbol, "SHORT", quantity, decision.StopLoss); err != nil {
			logger.Infof("  ⚠ Failed to set stop loss: %v", err)
		}
	}
	if decision.TakeProfit > 0 {
		if err := at.trader.SetTakeProfit(decision.Symbol, "SHORT", quantity, decision.TakeProfit); err != nil {
			logger.Infof("  ⚠ Failed to set take profit: %v", err)
		}
	}

	return nil
}

// executeCloseLongWithRecord executes close long position and records detailed information
func (at *AutoTrader) executeCloseLongWithRecord(decision *decision.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  🔄 Close long: %s", decision.Symbol)

	// 检查是否为跟单操作
	isCopyTrade := strings.Contains(decision.Reasoning, "Copy trading")

	// Get current price
	// 跟单模式：使用交易所自己的 API 获取价格（避免 Binance 交易对不兼容问题）
	var currentPrice float64
	if isCopyTrade {
		price, err := at.trader.GetMarketPrice(decision.Symbol)
		if err != nil {
			if decision.EntryPrice > 0 {
				currentPrice = decision.EntryPrice
			} else {
				return fmt.Errorf("failed to get market price: %w", err)
			}
		} else {
			currentPrice = price
		}
	} else {
		marketData, err := market.Get(decision.Symbol)
		if err != nil {
			return err
		}
		currentPrice = marketData.CurrentPrice
	}
	actionRecord.Price = currentPrice

	// 🔑 posId 方案：设置正确的 marginMode（复用 SetMarginMode）
	if decision.MarginMode != "" {
		isCross := decision.MarginMode == "cross"
		if err := at.trader.SetMarginMode(decision.Symbol, isCross); err != nil {
			logger.Warnf("  ⚠️ SetMarginMode failed: %v (继续执行)", err)
		} else {
			logger.Infof("  📊 Set marginMode=%s for close long", decision.MarginMode)
		}
	}

	// Get entry price and quantity from exchange API (most accurate)
	var entryPrice float64
	var totalQuantity float64
	positions, err := at.trader.GetPositions()
	if err == nil {
		for _, pos := range positions {
			// 根据 marginMode 精确匹配仓位
			if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
				// 如果指定了 marginMode，需要精确匹配
				if decision.MarginMode != "" {
					posMgnMode, _ := pos["mgnMode"].(string)
					if posMgnMode != decision.MarginMode {
						continue // 跳过不匹配的仓位
					}
				}
				if ep, ok := pos["entryPrice"].(float64); ok {
					entryPrice = ep
				}
				if amt, ok := pos["positionAmt"].(float64); ok && amt > 0 {
					totalQuantity = amt
				}
				logger.Infof("  📊 Found long position: mgnMode=%v, quantity=%.4f", pos["mgnMode"], totalQuantity)
				break
			}
		}
	}

	// Calculate close quantity based on CloseRatio
	closeQuantity := float64(0) // 0 = close all
	if decision.CloseRatio > 0 && decision.CloseRatio < 1 && totalQuantity > 0 {
		closeQuantity = totalQuantity * decision.CloseRatio
		logger.Infof("  📊 Partial close: %.0f%% of %.4f = %.4f", decision.CloseRatio*100, totalQuantity, closeQuantity)

		// 🆕 边界检查 1：仓位太小无法按比例减仓
		// 当仓位很小时，由于最小下单量（1张合约）约束，减仓可能导致意外全平
		// 例如：仓位 0.01 BNB（1张），减仓50% = 0.005，但向上取整到1张 → 全平
		// 为避免这种情况，当仓位 < 阈值且不是接近全平时，跳过减仓
		const minPositionForPartialClose = 0.02 // 约2张合约的阈值
		if totalQuantity < minPositionForPartialClose && decision.CloseRatio < 0.9 {
			logger.Warnf("  ⚠️ 仓位太小 (%.4f < %.4f)，无法按 %.0f%% 比例减仓，跳过本次操作",
				totalQuantity, minPositionForPartialClose, decision.CloseRatio*100)
			// 返回 sentinel 错误而非 nil：伪装成功会让跟单侧误更新
			// lastKnownSize/减仓计数并按空执行重挂止损单
			return fmt.Errorf("%w: 仓位 %.4f 过小无法按 %.0f%% 减仓",
				ErrPartialCloseSkipped, totalQuantity, decision.CloseRatio*100)
		}

		// 🆕 边界检查 2：价值检查（仅 Hyperliquid：最小订单价值 $10 硬约束）
		// 其他交易所（OKX/Binance 等）无此限制，误伤会导致小仓减仓永远被跳过、
		// 与领航员仓位长期漂移
		// 如果接近全平（>90%），不跳过，让它尝试执行
		closeValue := closeQuantity * currentPrice
		const minOrderValue = 10.0 // Hyperliquid 最小 $10
		if at.exchange == "hyperliquid" && closeValue < minOrderValue && decision.CloseRatio < 0.9 {
			logger.Warnf("  ⚠️ 减仓价值太小 (%.2f < $%.0f)，跳过本次操作（等待后续全平）",
				closeValue, minOrderValue)
			return fmt.Errorf("%w: 减仓价值 %.2f 低于 Hyperliquid 最小订单价值 $%.0f",
				ErrPartialCloseSkipped, closeValue, minOrderValue)
		}
	}

	// Close position
	order, err := at.closeLongOrder(isCopyTrade, decision.Symbol, closeQuantity, decision.ClientOrderID)
	if err != nil {
		return err
	}
	captureDecisionOrderIDs(decision, order)

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	// Determine actual closed quantity for recording
	recordQuantity := totalQuantity
	if closeQuantity > 0 {
		recordQuantity = closeQuantity
	}

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "close_long", recordQuantity, currentPrice, 0, entryPrice)

	if closeQuantity > 0 {
		logger.Infof("  ✓ Position partially closed: %.4f (%.0f%%)", closeQuantity, decision.CloseRatio*100)
	} else {
		logger.Infof("  ✓ Position closed successfully")
	}
	return nil
}

// executeCloseShortWithRecord executes close short position and records detailed information
func (at *AutoTrader) executeCloseShortWithRecord(decision *decision.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  🔄 Close short: %s", decision.Symbol)

	// 检查是否为跟单操作
	isCopyTrade := strings.Contains(decision.Reasoning, "Copy trading")

	// Get current price
	// 跟单模式：使用交易所自己的 API 获取价格（避免 Binance 交易对不兼容问题）
	var currentPrice float64
	if isCopyTrade {
		price, err := at.trader.GetMarketPrice(decision.Symbol)
		if err != nil {
			if decision.EntryPrice > 0 {
				currentPrice = decision.EntryPrice
			} else {
				return fmt.Errorf("failed to get market price: %w", err)
			}
		} else {
			currentPrice = price
		}
	} else {
		marketData, err := market.Get(decision.Symbol)
		if err != nil {
			return err
		}
		currentPrice = marketData.CurrentPrice
	}
	actionRecord.Price = currentPrice

	// 🔑 posId 方案：设置正确的 marginMode（复用 SetMarginMode）
	if decision.MarginMode != "" {
		isCross := decision.MarginMode == "cross"
		if err := at.trader.SetMarginMode(decision.Symbol, isCross); err != nil {
			logger.Warnf("  ⚠️ SetMarginMode failed: %v (继续执行)", err)
		} else {
			logger.Infof("  📊 Set marginMode=%s for close short", decision.MarginMode)
		}
	}

	// Get entry price and quantity from exchange API (most accurate)
	var entryPrice float64
	var totalQuantity float64
	positions, err := at.trader.GetPositions()
	if err == nil {
		for _, pos := range positions {
			// 根据 marginMode 精确匹配仓位
			if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
				// 如果指定了 marginMode，需要精确匹配
				if decision.MarginMode != "" {
					posMgnMode, _ := pos["mgnMode"].(string)
					if posMgnMode != decision.MarginMode {
						continue // 跳过不匹配的仓位
					}
				}
				if ep, ok := pos["entryPrice"].(float64); ok {
					entryPrice = ep
				}
				if amt, ok := pos["positionAmt"].(float64); ok {
					if amt < 0 {
						totalQuantity = -amt
					} else {
						totalQuantity = amt
					}
				}
				logger.Infof("  📊 Found short position: mgnMode=%v, quantity=%.4f", pos["mgnMode"], totalQuantity)
				break
			}
		}
	}

	// Calculate close quantity based on CloseRatio
	closeQuantity := float64(0) // 0 = close all
	if decision.CloseRatio > 0 && decision.CloseRatio < 1 && totalQuantity > 0 {
		closeQuantity = totalQuantity * decision.CloseRatio
		logger.Infof("  📊 Partial close: %.0f%% of %.4f = %.4f", decision.CloseRatio*100, totalQuantity, closeQuantity)

		// 🆕 边界检查 1：仓位太小无法按比例减仓
		// 当仓位很小时，由于最小下单量（1张合约）约束，减仓可能导致意外全平
		// 例如：仓位 0.01 BNB（1张），减仓50% = 0.005，但向上取整到1张 → 全平
		// 为避免这种情况，当仓位 < 阈值且不是接近全平时，跳过减仓
		const minPositionForPartialClose = 0.02 // 约2张合约的阈值
		if totalQuantity < minPositionForPartialClose && decision.CloseRatio < 0.9 {
			logger.Warnf("  ⚠️ 仓位太小 (%.4f < %.4f)，无法按 %.0f%% 比例减仓，跳过本次操作",
				totalQuantity, minPositionForPartialClose, decision.CloseRatio*100)
			// 同 executeCloseLongWithRecord：sentinel 错误，禁止伪装成功
			return fmt.Errorf("%w: 仓位 %.4f 过小无法按 %.0f%% 减仓",
				ErrPartialCloseSkipped, totalQuantity, decision.CloseRatio*100)
		}

		// 🆕 边界检查 2：价值检查（仅 Hyperliquid：最小订单价值 $10 硬约束）
		// 如果接近全平（>90%），不跳过，让它尝试执行
		closeValue := closeQuantity * currentPrice
		const minOrderValue = 10.0 // Hyperliquid 最小 $10
		if at.exchange == "hyperliquid" && closeValue < minOrderValue && decision.CloseRatio < 0.9 {
			logger.Warnf("  ⚠️ 减仓价值太小 (%.2f < $%.0f)，跳过本次操作（等待后续全平）",
				closeValue, minOrderValue)
			return fmt.Errorf("%w: 减仓价值 %.2f 低于 Hyperliquid 最小订单价值 $%.0f",
				ErrPartialCloseSkipped, closeValue, minOrderValue)
		}
	}

	// Close position
	order, err := at.closeShortOrder(isCopyTrade, decision.Symbol, closeQuantity, decision.ClientOrderID)
	if err != nil {
		return err
	}
	captureDecisionOrderIDs(decision, order)

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	// Determine actual closed quantity for recording
	recordQuantity := totalQuantity
	if closeQuantity > 0 {
		recordQuantity = closeQuantity
	}

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "close_short", recordQuantity, currentPrice, 0, entryPrice)

	if closeQuantity > 0 {
		logger.Infof("  ✓ Position partially closed: %.4f (%.0f%%)", closeQuantity, decision.CloseRatio*100)
	} else {
		logger.Infof("  ✓ Position closed successfully")
	}
	return nil
}

// GetID gets trader ID
func (at *AutoTrader) GetID() string {
	return at.id
}

// GetName gets trader name
func (at *AutoTrader) GetName() string {
	return at.name
}

// GetAIModel gets AI model
func (at *AutoTrader) GetAIModel() string {
	return at.aiModel
}

// GetExchange gets exchange
func (at *AutoTrader) GetExchange() string {
	return at.exchange
}

// GetShowInCompetition returns whether trader should be shown in competition
func (at *AutoTrader) GetShowInCompetition() bool {
	return at.showInCompetition
}

// SetShowInCompetition sets whether trader should be shown in competition
func (at *AutoTrader) SetShowInCompetition(show bool) {
	at.showInCompetition = show
}

// SetCustomPrompt sets custom trading strategy prompt
func (at *AutoTrader) SetCustomPrompt(prompt string) {
	at.customPrompt = prompt
}

// SetOverrideBasePrompt sets whether to override base prompt
func (at *AutoTrader) SetOverrideBasePrompt(override bool) {
	at.overrideBasePrompt = override
}

// GetSystemPromptTemplate gets current system prompt template name (from strategy config)
func (at *AutoTrader) GetSystemPromptTemplate() string {
	if at.strategyEngine != nil {
		config := at.strategyEngine.GetConfig()
		if config.CustomPrompt != "" {
			return "custom"
		}
	}
	return "strategy"
}

// saveEquitySnapshot saves equity snapshot independently (for drawing profit curve, decoupled from AI decision)
func (at *AutoTrader) saveEquitySnapshot(ctx *decision.Context) {
	if at.store == nil || ctx == nil {
		return
	}

	snapshot := &store.EquitySnapshot{
		TraderID:      at.id,
		Timestamp:     time.Now().UTC(),
		TotalEquity:   ctx.Account.TotalEquity,
		Balance:       ctx.Account.TotalEquity - ctx.Account.UnrealizedPnL,
		UnrealizedPnL: ctx.Account.UnrealizedPnL,
		PositionCount: ctx.Account.PositionCount,
		MarginUsedPct: ctx.Account.MarginUsedPct,
	}

	if err := at.store.Equity().Save(snapshot); err != nil {
		logger.Infof("⚠️ Failed to save equity snapshot: %v", err)
	}
}

// saveDecision saves AI decision log to database (only records AI input/output, for debugging)
func (at *AutoTrader) saveDecision(record *store.DecisionRecord) error {
	if at.store == nil {
		return nil
	}

	at.cycleNumber++
	record.CycleNumber = at.cycleNumber
	record.TraderID = at.id

	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}

	if err := at.store.Decision().LogDecision(record); err != nil {
		logger.Infof("⚠️ Failed to save decision record: %v", err)
		return err
	}

	logger.Infof("📝 Decision record saved: trader=%s, cycle=%d", at.id, at.cycleNumber)
	return nil
}

// GetStore gets data store (for external access to decision records, etc.)
func (at *AutoTrader) GetStore() *store.Store {
	return at.store
}

// GetStatus gets system status (for API)
func (at *AutoTrader) GetStatus() map[string]interface{} {
	aiProvider := "DeepSeek"
	if at.config.UseQwen {
		aiProvider = "Qwen"
	}

	return map[string]interface{}{
		"trader_id":       at.id,
		"trader_name":     at.name,
		"ai_model":        at.aiModel,
		"exchange":        at.exchange,
		"is_running":      at.isRunning,
		"start_time":      at.startTime.Format(time.RFC3339),
		"runtime_minutes": int(time.Since(at.startTime).Minutes()),
		"call_count":      at.callCount,
		"initial_balance": at.initialBalance,
		"scan_interval":   at.config.ScanInterval.String(),
		"stop_until":      at.stopUntil.Format(time.RFC3339),
		"last_reset_time": at.lastResetTime.Format(time.RFC3339),
		"ai_provider":     aiProvider,
	}
}

// GetAccountInfo gets account information (for API)
func (at *AutoTrader) GetAccountInfo() (map[string]interface{}, error) {
	balance, err := at.trader.GetBalance()
	if err != nil {
		return nil, fmt.Errorf("failed to get balance: %w", err)
	}

	// Get account fields
	totalWalletBalance := 0.0
	totalUnrealizedProfit := 0.0
	availableBalance := 0.0

	if wallet, ok := balance["totalWalletBalance"].(float64); ok {
		totalWalletBalance = wallet
	}
	if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
		totalUnrealizedProfit = unrealized
	}
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Total Equity = Wallet balance + Unrealized profit
	totalEquity := totalWalletBalance + totalUnrealizedProfit

	// Get positions to calculate total margin
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}

	totalMarginUsed := 0.0
	totalUnrealizedPnLCalculated := 0.0
	for _, pos := range positions {
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity
		}
		unrealizedPnl := pos["unRealizedProfit"].(float64)
		totalUnrealizedPnLCalculated += unrealizedPnl

		leverage := 10
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}
		marginUsed := (quantity * markPrice) / float64(leverage)
		totalMarginUsed += marginUsed
	}

	// Verify unrealized P&L consistency (API value vs calculated from positions)
	diff := math.Abs(totalUnrealizedProfit - totalUnrealizedPnLCalculated)
	if diff > 0.1 { // Allow 0.01 USDT error margin
		logger.Infof("⚠️ Unrealized P&L inconsistency: API=%.4f, Calculated=%.4f, Diff=%.4f",
			totalUnrealizedProfit, totalUnrealizedPnLCalculated, diff)
	}

	totalPnL := totalEquity - at.initialBalance
	totalPnLPct := 0.0
	if at.initialBalance > 0 {
		totalPnLPct = (totalPnL / at.initialBalance) * 100
	} else {
		logger.Infof("⚠️ Initial Balance abnormal: %.2f, cannot calculate P&L percentage", at.initialBalance)
	}

	marginUsedPct := 0.0
	if totalEquity > 0 {
		marginUsedPct = (totalMarginUsed / totalEquity) * 100
	}

	return map[string]interface{}{
		// Core fields
		"total_equity":      totalEquity,           // Account equity = wallet + unrealized
		"wallet_balance":    totalWalletBalance,    // Wallet balance (excluding unrealized P&L)
		"unrealized_profit": totalUnrealizedProfit, // Unrealized P&L (official value from exchange API)
		"available_balance": availableBalance,      // Available balance

		// P&L statistics
		"total_pnl":       totalPnL,          // Total P&L = equity - initial
		"total_pnl_pct":   totalPnLPct,       // Total P&L percentage
		"initial_balance": at.initialBalance, // Initial balance
		"daily_pnl":       at.dailyPnL,       // Daily P&L

		// Position information
		"position_count":  len(positions),  // Position count
		"margin_used":     totalMarginUsed, // Margin used
		"margin_used_pct": marginUsedPct,   // Margin usage rate
	}, nil
}

// GetPositions gets position list (for API)
func (at *AutoTrader) GetPositions() ([]map[string]interface{}, error) {
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}

	var result []map[string]interface{}
	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity
		}
		unrealizedPnl := pos["unRealizedProfit"].(float64)
		liquidationPrice := pos["liquidationPrice"].(float64)

		leverage := 10
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}

		// Calculate margin used
		marginUsed := (quantity * markPrice) / float64(leverage)

		// Calculate P&L percentage (based on margin)
		pnlPct := calculatePnLPercentage(unrealizedPnl, marginUsed)

		result = append(result, map[string]interface{}{
			"symbol":             symbol,
			"side":               side,
			"entry_price":        entryPrice,
			"mark_price":         markPrice,
			"quantity":           quantity,
			"leverage":           leverage,
			"unrealized_pnl":     unrealizedPnl,
			"unrealized_pnl_pct": pnlPct,
			"liquidation_price":  liquidationPrice,
			"margin_used":        marginUsed,
		})
	}

	return result, nil
}

// calculatePnLPercentage calculates P&L percentage (based on margin, automatically considers leverage)
// Return rate = Unrealized P&L / Margin × 100%
func calculatePnLPercentage(unrealizedPnl, marginUsed float64) float64 {
	if marginUsed > 0 {
		return (unrealizedPnl / marginUsed) * 100
	}
	return 0.0
}

// sortDecisionsByPriority sorts decisions: close positions first, then open positions, finally hold/wait
// This avoids position stacking overflow when changing positions
func sortDecisionsByPriority(decisions []decision.Decision) []decision.Decision {
	if len(decisions) <= 1 {
		return decisions
	}

	// Define priority
	getActionPriority := func(action string) int {
		switch action {
		case "close_long", "close_short":
			return 1 // Highest priority: close positions first
		case "open_long", "open_short":
			return 2 // Second priority: open positions later
		case "hold", "wait":
			return 3 // Lowest priority: wait
		default:
			return 999 // Unknown actions at the end
		}
	}

	// Copy decision list
	sorted := make([]decision.Decision, len(decisions))
	copy(sorted, decisions)

	// Sort by priority
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if getActionPriority(sorted[i].Action) > getActionPriority(sorted[j].Action) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	return sorted
}

// startDrawdownMonitor starts drawdown monitoring
func (at *AutoTrader) startDrawdownMonitor() {
	at.monitorWg.Add(1)
	go func() {
		defer at.monitorWg.Done()

		ticker := time.NewTicker(1 * time.Minute) // Check every minute
		defer ticker.Stop()

		logger.Info("📊 Started position drawdown monitoring (check every minute)")

		for {
			select {
			case <-ticker.C:
				at.checkPositionDrawdown()
			case <-at.stopMonitorCh:
				logger.Info("⏹ Stopped position drawdown monitoring")
				return
			}
		}
	}()
}

// checkPositionDrawdown checks position drawdown situation
func (at *AutoTrader) checkPositionDrawdown() {
	// Get current positions
	positions, err := at.trader.GetPositions()
	if err != nil {
		logger.Infof("❌ Drawdown monitoring: failed to get positions: %v", err)
		return
	}

	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity // Short position quantity is negative, convert to positive
		}

		// Calculate current P&L percentage
		leverage := 10 // Default value
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}

		var currentPnLPct float64
		if side == "long" {
			currentPnLPct = ((markPrice - entryPrice) / entryPrice) * float64(leverage) * 100
		} else {
			currentPnLPct = ((entryPrice - markPrice) / entryPrice) * float64(leverage) * 100
		}

		// Construct unique position identifier (distinguish long/short)
		posKey := symbol + "_" + side

		// Get historical peak profit for this position
		at.peakPnLCacheMutex.RLock()
		peakPnLPct, exists := at.peakPnLCache[posKey]
		at.peakPnLCacheMutex.RUnlock()

		if !exists {
			// If no historical peak record, use current P&L as initial value
			peakPnLPct = currentPnLPct
			at.UpdatePeakPnL(symbol, side, currentPnLPct)
		} else {
			// Update peak cache
			at.UpdatePeakPnL(symbol, side, currentPnLPct)
		}

		// Calculate drawdown (magnitude of decline from peak)
		var drawdownPct float64
		if peakPnLPct > 0 && currentPnLPct < peakPnLPct {
			drawdownPct = ((peakPnLPct - currentPnLPct) / peakPnLPct) * 100
		}

		// Check close position condition: profit > 5% and drawdown >= 40%
		if currentPnLPct > 5.0 && drawdownPct >= 40.0 {
			logger.Infof("🚨 Drawdown close position condition triggered: %s %s | Current profit: %.2f%% | Peak profit: %.2f%% | Drawdown: %.2f%%",
				symbol, side, currentPnLPct, peakPnLPct, drawdownPct)

			// Execute close position
			if err := at.emergencyClosePosition(symbol, side); err != nil {
				logger.Infof("❌ Drawdown close position failed (%s %s): %v", symbol, side, err)
			} else {
				logger.Infof("✅ Drawdown close position succeeded: %s %s", symbol, side)
				// Clear cache for this position after closing
				at.ClearPeakPnLCache(symbol, side)
			}
		} else if currentPnLPct > 5.0 {
			// Record situations close to close position condition (for debugging)
			logger.Infof("📊 Drawdown monitoring: %s %s | Profit: %.2f%% | Peak: %.2f%% | Drawdown: %.2f%%",
				symbol, side, currentPnLPct, peakPnLPct, drawdownPct)
		}
	}
}

// emergencyClosePosition emergency close position function
func (at *AutoTrader) emergencyClosePosition(symbol, side string) error {
	switch side {
	case "long":
		order, err := at.trader.CloseLong(symbol, 0) // 0 = close all
		if err != nil {
			return err
		}
		logger.Infof("✅ Emergency close long position succeeded, order ID: %v", order["orderId"])
	case "short":
		order, err := at.trader.CloseShort(symbol, 0) // 0 = close all
		if err != nil {
			return err
		}
		logger.Infof("✅ Emergency close short position succeeded, order ID: %v", order["orderId"])
	default:
		return fmt.Errorf("unknown position direction: %s", side)
	}

	return nil
}

// ClosePositionMarket exposes the same reduce-only close-all primitive to
// Copy Guard for residual positions after a partially filled protective stop.
func (at *AutoTrader) ClosePositionMarket(symbol, side string) (string, error) {
	var order map[string]interface{}
	var err error
	// Copy Guard must not use the normal Binance close path because that path
	// intentionally clears every pending/algo order for the symbol. Prefer the
	// copy-trade primitive, which only submits the requested reduce order and
	// leaves unrelated user/strategy orders untouched.
	copyExecutor, preservesOrders := at.trader.(CopyTradeOrderExecutor)
	switch side {
	case "long":
		if preservesOrders {
			order, err = copyExecutor.CloseLongPreservingOrders(symbol, 0)
		} else {
			order, err = at.trader.CloseLong(symbol, 0)
		}
	case "short":
		if preservesOrders {
			order, err = copyExecutor.CloseShortPreservingOrders(symbol, 0)
		} else {
			order, err = at.trader.CloseShort(symbol, 0)
		}
	default:
		return "", fmt.Errorf("unknown position direction: %s", side)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprint(order["orderId"]), nil
}

// GetPeakPnLCache gets peak profit cache
func (at *AutoTrader) GetPeakPnLCache() map[string]float64 {
	at.peakPnLCacheMutex.RLock()
	defer at.peakPnLCacheMutex.RUnlock()

	// Return a copy of the cache
	cache := make(map[string]float64)
	for k, v := range at.peakPnLCache {
		cache[k] = v
	}
	return cache
}

// UpdatePeakPnL updates peak profit cache
func (at *AutoTrader) UpdatePeakPnL(symbol, side string, currentPnLPct float64) {
	at.peakPnLCacheMutex.Lock()
	defer at.peakPnLCacheMutex.Unlock()

	posKey := symbol + "_" + side
	if peak, exists := at.peakPnLCache[posKey]; exists {
		// Update peak (if long, take larger value; if short, currentPnLPct is negative, also compare)
		if currentPnLPct > peak {
			at.peakPnLCache[posKey] = currentPnLPct
		}
	} else {
		// First time recording
		at.peakPnLCache[posKey] = currentPnLPct
	}
}

// ClearPeakPnLCache clears peak cache for specified position
func (at *AutoTrader) ClearPeakPnLCache(symbol, side string) {
	at.peakPnLCacheMutex.Lock()
	defer at.peakPnLCacheMutex.Unlock()

	posKey := symbol + "_" + side
	delete(at.peakPnLCache, posKey)
}

// recordAndConfirmOrder polls order status for actual fill data and records position
// action: open_long, open_short, close_long, close_short
// entryPrice: entry price when closing (0 when opening)
func (at *AutoTrader) recordAndConfirmOrder(orderResult map[string]interface{}, symbol, action string, quantity float64, price float64, leverage int, entryPrice float64) {
	if at.store == nil {
		return
	}

	// Get order ID (supports multiple types)
	var orderID string
	switch v := orderResult["orderId"].(type) {
	case int64:
		orderID = fmt.Sprintf("%d", v)
	case float64:
		orderID = fmt.Sprintf("%.0f", v)
	case string:
		orderID = v
	default:
		orderID = fmt.Sprintf("%v", v)
	}

	// 修复：某些交易所（如 Hyperliquid）不返回 orderId，生成唯一 ID 确保交易记录不丢失
	if orderID == "" || orderID == "0" || orderID == "<nil>" {
		// 生成唯一 ID: 交易所_币种_动作_时间戳
		orderID = fmt.Sprintf("%s_%s_%s_%d", at.exchange, symbol, action, time.Now().UnixNano())
		logger.Infof("  📝 Order ID is empty, using auto-generated: %s", orderID)
	}

	// Determine positionSide
	var positionSide string
	switch action {
	case "open_long", "close_long":
		positionSide = "LONG"
	case "open_short", "close_short":
		positionSide = "SHORT"
	}

	// Poll order status to get actual fill price, quantity and fee
	var actualPrice = price  // fallback to market price
	var actualQty = quantity // fallback to requested quantity
	var fee float64

	// Wait for order to be filled and get actual fill data
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < 5; i++ {
		status, err := at.trader.GetOrderStatus(symbol, orderID)
		if err == nil {
			statusStr, _ := status["status"].(string)
			if statusStr == "FILLED" {
				// Get actual fill price
				if avgPrice, ok := status["avgPrice"].(float64); ok && avgPrice > 0 {
					actualPrice = avgPrice
				}
				// Get actual executed quantity
				if execQty, ok := status["executedQty"].(float64); ok && execQty > 0 {
					actualQty = execQty
				}
				// Get commission/fee
				if commission, ok := status["commission"].(float64); ok {
					fee = commission
				}
				logger.Infof("  ✅ Order filled: avgPrice=%.6f, qty=%.6f, fee=%.6f", actualPrice, actualQty, fee)
				break
			} else if statusStr == "CANCELED" || statusStr == "EXPIRED" || statusStr == "REJECTED" {
				logger.Infof("  ⚠️ Order %s, skipping position record", statusStr)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	logger.Infof("  📝 Recording position (ID: %s, action: %s, price: %.6f, qty: %.6f, fee: %.4f)",
		orderID, action, actualPrice, actualQty, fee)

	// Record position change with actual fill data
	at.recordPositionChange(orderID, symbol, positionSide, action, actualQty, actualPrice, leverage, entryPrice, fee)
}

// recordPositionChange records position change (create record on open, update record on close)
func (at *AutoTrader) recordPositionChange(orderID, symbol, side, action string, quantity, price float64, leverage int, entryPrice float64, fee float64) {
	if at.store == nil {
		return
	}

	switch action {
	case "open_long", "open_short":
		// Open position: create new position record
		pos := &store.TraderPosition{
			TraderID:     at.id,
			ExchangeID:   at.exchangeID, // Exchange account UUID
			ExchangeType: at.exchange,   // Exchange type: binance/bybit/okx/etc
			Symbol:       symbol,
			Side:         side, // LONG or SHORT
			Quantity:     quantity,
			EntryPrice:   price,
			EntryOrderID: orderID,
			EntryTime:    time.Now(),
			Leverage:     leverage,
			Status:       "OPEN",
		}
		if err := at.store.Position().Create(pos); err != nil {
			logger.Infof("  ⚠️ Failed to record position: %v", err)
		} else {
			logger.Infof("  📊 Position recorded [%s] %s %s @ %.4f", at.id[:8], symbol, side, price)
		}

	case "close_long", "close_short":
		// Close position: find corresponding open position record and update
		openPos, err := at.store.Position().GetOpenPositionBySymbol(at.id, symbol, side)
		if err != nil || openPos == nil {
			logger.Infof("  ⚠️ Cannot find corresponding open position record (%s %s)", symbol, side)
			return
		}

		// Calculate P&L
		var realizedPnL float64
		if side == "LONG" {
			realizedPnL = (price - openPos.EntryPrice) * openPos.Quantity
		} else {
			realizedPnL = (openPos.EntryPrice - price) * openPos.Quantity
		}

		// Update position record
		err = at.store.Position().ClosePosition(
			openPos.ID,
			price,   // exitPrice
			orderID, // exitOrderID
			realizedPnL,
			fee, // fee from exchange API
			"ai_decision",
		)
		if err != nil {
			logger.Infof("  ⚠️ Failed to update position: %v", err)
		} else {
			logger.Infof("  📊 Position closed [%s] %s %s @ %.4f → %.4f, P&L: %.2f, Fee: %.4f",
				at.id[:8], symbol, side, openPos.EntryPrice, price, realizedPnL, fee)
		}
	}
}

// ============================================================================
// Risk Control Helpers
// ============================================================================

// isBTCETH checks if a symbol is BTC or ETH
func isBTCETH(symbol string) bool {
	symbol = strings.ToUpper(symbol)
	return strings.HasPrefix(symbol, "BTC") || strings.HasPrefix(symbol, "ETH")
}

// enforcePositionValueRatio checks and enforces position value ratio limits (CODE ENFORCED)
// Returns the adjusted position size (capped if necessary) and whether the position was capped
// positionSizeUSD: the original position size in USD
// equity: the account equity
// symbol: the trading symbol
func (at *AutoTrader) enforcePositionValueRatio(positionSizeUSD float64, equity float64, symbol string) (float64, bool) {
	if at.config.StrategyConfig == nil {
		return positionSizeUSD, false
	}

	riskControl := at.config.StrategyConfig.RiskControl

	// Get the appropriate position value ratio limit
	var maxPositionValueRatio float64
	if isBTCETH(symbol) {
		maxPositionValueRatio = riskControl.BTCETHMaxPositionValueRatio
		if maxPositionValueRatio <= 0 {
			maxPositionValueRatio = 5.0 // Default: 5x for BTC/ETH
		}
	} else {
		maxPositionValueRatio = riskControl.AltcoinMaxPositionValueRatio
		if maxPositionValueRatio <= 0 {
			maxPositionValueRatio = 1.0 // Default: 1x for altcoins
		}
	}

	// Calculate max allowed position value = equity × ratio
	maxPositionValue := equity * maxPositionValueRatio

	// Check if position size exceeds limit
	if positionSizeUSD > maxPositionValue {
		logger.Infof("  ⚠️ [RISK CONTROL] Position %.2f USDT exceeds limit (equity %.2f × %.1fx = %.2f USDT max for %s), capping",
			positionSizeUSD, equity, maxPositionValueRatio, maxPositionValue, symbol)
		return maxPositionValue, true
	}

	return positionSizeUSD, false
}

// enforceMinPositionSize checks minimum position size (CODE ENFORCED)
func (at *AutoTrader) enforceMinPositionSize(positionSizeUSD float64) error {
	if at.config.StrategyConfig == nil {
		return nil
	}

	minSize := at.config.StrategyConfig.RiskControl.MinPositionSize
	if minSize <= 0 {
		minSize = 12 // Default: 12 USDT
	}

	if positionSizeUSD < minSize {
		return fmt.Errorf("❌ [RISK CONTROL] Position %.2f USDT below minimum (%.2f USDT)", positionSizeUSD, minSize)
	}
	return nil
}

// enforceMaxPositions checks maximum positions count (CODE ENFORCED)
func (at *AutoTrader) enforceMaxPositions(currentPositionCount int) error {
	if at.config.StrategyConfig == nil {
		return nil
	}

	maxPositions := at.config.StrategyConfig.RiskControl.MaxPositions
	if maxPositions <= 0 {
		maxPositions = 3 // Default: 3 positions
	}

	if currentPositionCount >= maxPositions {
		return fmt.Errorf("❌ [RISK CONTROL] Already at max positions (%d/%d)", currentPositionCount, maxPositions)
	}
	return nil
}
