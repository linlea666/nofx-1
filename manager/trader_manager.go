package manager

import (
	"context"
	"fmt"
	"nofx/copytrade"
	"nofx/debate"
	"nofx/decision"
	"nofx/logger"
	"nofx/notifier"
	"nofx/store"
	"nofx/trader"
	"sort"
	"sync"
	"time"
)

// TraderExecutorAdapter wraps AutoTrader to implement debate.TraderExecutor
type TraderExecutorAdapter struct {
	autoTrader *trader.AutoTrader
}

// ExecuteDecision executes a trading decision
func (a *TraderExecutorAdapter) ExecuteDecision(d *decision.Decision) error {
	return a.autoTrader.ExecuteDecision(d)
}

// GetBalance returns account balance
func (a *TraderExecutorAdapter) GetBalance() (map[string]interface{}, error) {
	info, err := a.autoTrader.GetAccountInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get account info: %w", err)
	}
	// Log the balance for debugging
	logger.Infof("[Debate] GetBalance for trader, result: %+v", info)
	return info, nil
}

// CompetitionCache competition data cache
type CompetitionCache struct {
	data      map[string]interface{}
	timestamp time.Time
	mu        sync.RWMutex
}

func cloneCompetitionData(data map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(data))
	for key, value := range data {
		switch typed := value.(type) {
		case []map[string]interface{}:
			traders := make([]map[string]interface{}, len(typed))
			for i, traderData := range typed {
				copyData := make(map[string]interface{}, len(traderData))
				for field, fieldValue := range traderData {
					copyData[field] = fieldValue
				}
				traders[i] = copyData
			}
			cloned[key] = traders
		default:
			cloned[key] = value
		}
	}
	return cloned
}

// accountSnapshot is the last account read that actually succeeded for a trader.
//
// It exists so a failed read can be reported as a failure. Writing 0.0 on failure
// made an outage indistinguishable from an empty account: in production a trader
// whose copy trading had been stalled for hours by rejected API timestamps simply
// showed a 0.00 balance and a RUNNING badge.
type accountSnapshot struct {
	equity        float64
	pnl           float64
	pnlPct        float64
	positionCount interface{}
	marginUsedPct float64
	at            time.Time
}

// accountUnavailableAlertAfter: how long a running trader may fail to produce any
// account read before it is escalated. Silence here means proportional copy
// trading is suspended, so it must not stay invisible.
const accountUnavailableAlertAfter = 10 * time.Minute

// TraderManager manages multiple trader instances
type TraderManager struct {
	traders          map[string]*trader.AutoTrader // key: trader ID
	loadErrors       map[string]error              // key: trader ID, stores last load error
	competitionCache *CompetitionCache
	mu               sync.RWMutex

	accountMu        sync.Mutex
	lastAccount      map[string]accountSnapshot // key: trader ID, last successful read
	accountFailedAt  map[string]time.Time       // key: trader ID, start of the current failure streak
	accountAlertedAt map[string]time.Time       // key: trader ID, last escalation
}

// NewTraderManager creates a trader manager
func NewTraderManager() *TraderManager {
	return &TraderManager{
		traders:    make(map[string]*trader.AutoTrader),
		loadErrors: make(map[string]error),
		competitionCache: &CompetitionCache{
			data: make(map[string]interface{}),
		},
		lastAccount:      make(map[string]accountSnapshot),
		accountFailedAt:  make(map[string]time.Time),
		accountAlertedAt: make(map[string]time.Time),
	}
}

// recordAccountSuccess stores the reading and clears the failure streak.
func (tm *TraderManager) recordAccountSuccess(traderID string, snapshot accountSnapshot) {
	tm.accountMu.Lock()
	defer tm.accountMu.Unlock()
	tm.lastAccount[traderID] = snapshot
	delete(tm.accountFailedAt, traderID)
	delete(tm.accountAlertedAt, traderID)
}

// recordAccountFailure returns the last successful reading (if any) and how long
// the current failure streak has lasted.
func (tm *TraderManager) recordAccountFailure(traderID string) (accountSnapshot, bool, time.Duration) {
	tm.accountMu.Lock()
	defer tm.accountMu.Unlock()
	now := time.Now()
	if _, streaking := tm.accountFailedAt[traderID]; !streaking {
		tm.accountFailedAt[traderID] = now
	}
	snapshot, known := tm.lastAccount[traderID]
	return snapshot, known, now.Sub(tm.accountFailedAt[traderID])
}

// shouldAlertAccountUnavailable reports whether the failure streak has lasted long
// enough to escalate, at most once per streak per alert window.
func (tm *TraderManager) shouldAlertAccountUnavailable(traderID string, streak time.Duration) bool {
	if streak < accountUnavailableAlertAfter {
		return false
	}
	tm.accountMu.Lock()
	defer tm.accountMu.Unlock()
	if last, alerted := tm.accountAlertedAt[traderID]; alerted && time.Since(last) < time.Hour {
		return false
	}
	tm.accountAlertedAt[traderID] = time.Now()
	return true
}

// GetLoadError returns the last load error for a trader
func (tm *TraderManager) GetLoadError(traderID string) error {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.loadErrors[traderID]
}

// GetTrader retrieves a trader by ID
func (tm *TraderManager) GetTrader(id string) (*trader.AutoTrader, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	t, exists := tm.traders[id]
	if !exists {
		return nil, fmt.Errorf("trader ID '%s' does not exist", id)
	}
	return t, nil
}

// GetAllTraders retrieves all traders
func (tm *TraderManager) GetAllTraders() map[string]*trader.AutoTrader {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	result := make(map[string]*trader.AutoTrader)
	for id, t := range tm.traders {
		result[id] = t
	}
	return result
}

// GetTraderIDs retrieves all trader IDs
func (tm *TraderManager) GetTraderIDs() []string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	ids := make([]string, 0, len(tm.traders))
	for id := range tm.traders {
		ids = append(ids, id)
	}
	return ids
}

// StartAll starts all traders
func (tm *TraderManager) StartAll() {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	logger.Info("🚀 Starting all traders...")
	for id, t := range tm.traders {
		go func(traderID string, at *trader.AutoTrader) {
			logger.Infof("▶️  Starting %s...", at.GetName())
			if err := at.Run(); err != nil {
				logger.Infof("❌ %s runtime error: %v", at.GetName(), err)
			}
		}(id, t)
	}
}

// StartTraderWithMode starts a trader in the appropriate mode (AI or Copy Trade)
// This is the unified entry point for starting traders with decision mode awareness
func (tm *TraderManager) StartTraderWithMode(traderID string, st *store.Store) error {
	at, err := tm.GetTrader(traderID)
	if err != nil {
		return err
	}

	// Check decision mode
	decisionMode, _ := st.CopyTrade().GetDecisionMode(traderID)

	if decisionMode == "copy_trade" {
		// Start in copy trade mode
		logger.Infof("🎯 [%s] Copy trade mode, starting copy trading engine...", at.GetName())
		go func() {
			if err := copytrade.StartCopyTradingForTrader(traderID, at, st); err != nil {
				logger.Warnf("⚠️ Trader '%s' copy trade stopped with error: %v", at.GetName(), err)
			}
		}()
		logger.Infof("✅ Trader '%s' started in copy trade mode", at.GetName())
	} else {
		// Start in AI mode (default)
		logger.Infof("🤖 [%s] AI mode, starting AI trading...", at.GetName())
		go func() {
			if err := at.Run(); err != nil {
				logger.Warnf("⚠️ Trader '%s' stopped with error: %v", at.GetName(), err)
			}
		}()
		logger.Infof("✅ Trader '%s' started in AI mode", at.GetName())
	}

	return nil
}

// StopAll stops all traders
func (tm *TraderManager) StopAll() {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	logger.Info("⏹  Stopping all traders...")
	for _, t := range tm.traders {
		t.Stop()
	}
}

// AutoStartRunningTraders automatically starts traders marked as running in the database
func (tm *TraderManager) AutoStartRunningTraders(st *store.Store) {
	// Get all trader configurations (single query)
	traderList, err := st.Trader().ListAll()
	if err != nil {
		logger.Infof("⚠️ Failed to get trader list: %v", err)
		return
	}

	// Build set of running trader IDs
	runningTraderIDs := make(map[string]bool)
	for _, traderCfg := range traderList {
		if traderCfg.IsRunning {
			runningTraderIDs[traderCfg.ID] = true
		}
	}

	if len(runningTraderIDs) == 0 {
		logger.Info("📋 No traders to auto-restore")
		return
	}

	tm.mu.RLock()
	defer tm.mu.RUnlock()

	startedCount := 0
	for id, t := range tm.traders {
		if runningTraderIDs[id] {
			// Check decision mode for each trader
			decisionMode, _ := st.CopyTrade().GetDecisionMode(id)

			if decisionMode == "copy_trade" {
				// Start in copy trade mode
				go func(traderID string, at *trader.AutoTrader) {
					logger.Infof("▶️  Auto-restoring %s (copy trade mode)...", at.GetName())
					if err := copytrade.StartCopyTradingForTrader(traderID, at, st); err != nil {
						logger.Infof("❌ %s copy trade runtime error: %v", at.GetName(), err)
					}
				}(id, t)
			} else {
				// Start in AI mode (default)
				go func(traderID string, at *trader.AutoTrader) {
					logger.Infof("▶️  Auto-restoring %s (AI mode)...", at.GetName())
					if err := at.Run(); err != nil {
						logger.Infof("❌ %s runtime error: %v", at.GetName(), err)
					}
				}(id, t)
			}
			startedCount++
		}
	}

	if startedCount > 0 {
		logger.Infof("✓ Auto-restored %d traders", startedCount)
	}
}

// GetComparisonData retrieves comparison data
func (tm *TraderManager) GetComparisonData() (map[string]interface{}, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	comparison := make(map[string]interface{})
	traders := make([]map[string]interface{}, 0, len(tm.traders))

	for _, t := range tm.traders {
		account, err := t.GetAccountInfo()
		if err != nil {
			continue
		}

		status := t.GetStatus()

		traders = append(traders, map[string]interface{}{
			"trader_id":       t.GetID(),
			"trader_name":     t.GetName(),
			"ai_model":        t.GetAIModel(),
			"exchange":        t.GetExchange(),
			"total_equity":    account["total_equity"],
			"total_pnl":       account["total_pnl"],
			"total_pnl_pct":   account["total_pnl_pct"],
			"position_count":  account["position_count"],
			"margin_used_pct": account["margin_used_pct"],
			"call_count":      status["call_count"],
			"is_running":      status["is_running"],
		})
	}

	comparison["traders"] = traders
	comparison["count"] = len(traders)

	return comparison, nil
}

// GetCompetitionData retrieves competition data (all traders across platform)
func (tm *TraderManager) GetCompetitionData() (map[string]interface{}, error) {
	// Check if cache is valid (within 30 seconds)
	if cachedData, ok := tm.GetCachedCompetitionData(30 * time.Second); ok {
		logger.Infof("📋 Returning competition data cache")
		return cachedData, nil
	}

	tm.mu.RLock()

	// Get all trader list (only those with ShowInCompetition = true)
	allTraders := make([]*trader.AutoTrader, 0, len(tm.traders))
	for id, t := range tm.traders {
		if t.GetShowInCompetition() {
			allTraders = append(allTraders, t)
			logger.Infof("📋 Competition data includes trader: %s (%s)", t.GetName(), id)
		} else {
			logger.Infof("📋 Competition data excludes trader (hidden): %s (%s)", t.GetName(), id)
		}
	}
	tm.mu.RUnlock()

	logger.Infof("🔄 Refreshing competition data, trader count: %d", len(allTraders))

	// Concurrently fetch trader data
	traders := tm.getConcurrentTraderData(allTraders)

	// Sort by profit rate (descending)
	sort.Slice(traders, func(i, j int) bool {
		pnlPctI, okI := traders[i]["total_pnl_pct"].(float64)
		pnlPctJ, okJ := traders[j]["total_pnl_pct"].(float64)
		if !okI {
			pnlPctI = 0
		}
		if !okJ {
			pnlPctJ = 0
		}
		return pnlPctI > pnlPctJ
	})

	// Limit to top 50
	totalCount := len(traders)
	limit := 50
	if len(traders) > limit {
		traders = traders[:limit]
	}

	comparison := make(map[string]interface{})
	comparison["traders"] = traders
	comparison["count"] = len(traders)
	comparison["total_count"] = totalCount // Total number of traders

	// Update cache
	tm.competitionCache.mu.Lock()
	tm.competitionCache.data = comparison
	tm.competitionCache.timestamp = time.Now()
	tm.competitionCache.mu.Unlock()

	return comparison, nil
}

// GetCachedCompetitionData returns a read-only snapshot without refreshing any
// trader account. Consumers such as historical charts must never turn a cache
// miss into exchange API traffic.
func (tm *TraderManager) GetCachedCompetitionData(maxAge time.Duration) (map[string]interface{}, bool) {
	if tm == nil || tm.competitionCache == nil || maxAge <= 0 {
		return nil, false
	}
	tm.competitionCache.mu.RLock()
	defer tm.competitionCache.mu.RUnlock()
	age := time.Since(tm.competitionCache.timestamp)
	if tm.competitionCache.timestamp.IsZero() || age < 0 || age > maxAge || len(tm.competitionCache.data) == 0 {
		return nil, false
	}
	return cloneCompetitionData(tm.competitionCache.data), true
}

// getConcurrentTraderData concurrently fetches data for multiple traders
func (tm *TraderManager) getConcurrentTraderData(traders []*trader.AutoTrader) []map[string]interface{} {
	type traderResult struct {
		index int
		data  map[string]interface{}
	}

	// Create result channel
	resultChan := make(chan traderResult, len(traders))

	// Concurrently fetch data for each trader
	for i, t := range traders {
		go func(index int, trader *trader.AutoTrader) {
			// Set timeout to 3 seconds for single trader
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			// Use channel for timeout control
			accountChan := make(chan map[string]interface{}, 1)
			errorChan := make(chan error, 1)

			go func() {
				account, err := trader.GetAccountInfo()
				if err != nil {
					errorChan <- err
				} else {
					accountChan <- account
				}
			}()

			status := trader.GetStatus()
			var traderData map[string]interface{}

			select {
			case account := <-accountChan:
				// Successfully got account info
				traderData = map[string]interface{}{
					"trader_id":              trader.GetID(),
					"trader_name":            trader.GetName(),
					"ai_model":               trader.GetAIModel(),
					"exchange":               trader.GetExchange(),
					"total_equity":           account["total_equity"],
					"total_pnl":              account["total_pnl"],
					"total_pnl_pct":          account["total_pnl_pct"],
					"position_count":         account["position_count"],
					"margin_used_pct":        account["margin_used_pct"],
					"is_running":             status["is_running"],
					"system_prompt_template": trader.GetSystemPromptTemplate(),
				}
				tm.recordAccountSuccess(trader.GetID(), accountSnapshot{
					equity:        toFloat(account["total_equity"]),
					pnl:           toFloat(account["total_pnl"]),
					pnlPct:        toFloat(account["total_pnl_pct"]),
					positionCount: account["position_count"],
					marginUsedPct: toFloat(account["margin_used_pct"]),
					at:            time.Now(),
				})
			case err := <-errorChan:
				logger.Warnf("⚠️ Failed to get account info for trader %s: %v", trader.GetID(), err)
				traderData = tm.unavailableTraderData(trader, status, "Failed to get account data", err.Error())
			case <-ctx.Done():
				logger.Warnf("⏰ Timeout getting account info for trader %s", trader.GetID())
				traderData = tm.unavailableTraderData(trader, status, "Request timeout", "account read exceeded 3s")
			}

			resultChan <- traderResult{index: index, data: traderData}
		}(i, t)
	}

	// Collect all results
	results := make([]map[string]interface{}, len(traders))
	for i := 0; i < len(traders); i++ {
		result := <-resultChan
		results[result.index] = result.data
	}

	return results
}

func toFloat(value interface{}) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	}
	return 0
}

// unavailableTraderData builds the leaderboard row for a trader whose account
// could not be read. It falls back to the last successful reading and marks the
// row stale, so the UI can say "读取失败" instead of showing a fabricated 0.00.
func (tm *TraderManager) unavailableTraderData(
	t *trader.AutoTrader, status map[string]interface{}, reason, detail string,
) map[string]interface{} {
	snapshot, known, streak := tm.recordAccountFailure(t.GetID())
	data := map[string]interface{}{
		"trader_id":              t.GetID(),
		"trader_name":            t.GetName(),
		"ai_model":               t.GetAIModel(),
		"exchange":               t.GetExchange(),
		"total_equity":           0.0,
		"total_pnl":              0.0,
		"total_pnl_pct":          0.0,
		"position_count":         0,
		"margin_used_pct":        0.0,
		"is_running":             status["is_running"],
		"system_prompt_template": t.GetSystemPromptTemplate(),
		"error":                  reason,
		"stale":                  true,
	}
	if known {
		data["total_equity"] = snapshot.equity
		data["total_pnl"] = snapshot.pnl
		data["total_pnl_pct"] = snapshot.pnlPct
		data["margin_used_pct"] = snapshot.marginUsedPct
		if snapshot.positionCount != nil {
			data["position_count"] = snapshot.positionCount
		}
		data["stale_age_seconds"] = int64(time.Since(snapshot.at).Seconds())
	}
	running, _ := status["is_running"].(bool)
	if running && tm.shouldAlertAccountUnavailable(t.GetID(), streak) {
		traderName := t.GetName()
		// Hour-bucketed dedupe key, matching the pattern used elsewhere: the
		// notifier dedupes once per process, so a bare key would silence every
		// recurrence for the rest of the process lifetime.
		key := fmt.Sprintf("account_unavailable|%s", t.GetID())
		notifier.Notify(notifier.Alert{
			Category: "trader", TraderID: t.GetID(), TraderName: traderName,
			Title: fmt.Sprintf("%s | 账户数据持续读取失败", traderName),
			Body: fmt.Sprintf("交易员仍处于运行中，但已连续 %.0f 分钟无法读取账户数据。\n交易所: %s\n原因: %s\n详情: %s\n\n影响: 权威权益不可得时按比例跟单会暂停，界面权益为最近一次成功读取的陈旧值。\n建议: 检查交易所 API 密钥、网络与系统时间同步（Binance 签名对时钟偏移敏感）。",
				streak.Minutes(), t.GetExchange(), reason, detail),
			RateKey: key, DedupKey: fmt.Sprintf("%s|%d", key, time.Now().Unix()/3600),
		})
	}
	return data
}

// GetTopTradersData retrieves top 5 traders data (for performance comparison)
func (tm *TraderManager) GetTopTradersData() (map[string]interface{}, error) {
	// Reuse competition data cache, as top 5 is filtered from all data
	competitionData, err := tm.GetCompetitionData()
	if err != nil {
		return nil, err
	}

	// Extract top 5 from competition data
	allTraders, ok := competitionData["traders"].([]map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid competition data format")
	}

	// Limit to top 5
	limit := 5
	topTraders := allTraders
	if len(allTraders) > limit {
		topTraders = allTraders[:limit]
	}

	result := map[string]interface{}{
		"traders": topTraders,
		"count":   len(topTraders),
	}

	return result, nil
}

// RemoveTrader removes a trader from memory (does not affect database)
// Used to force reload when updating trader configuration
func (tm *TraderManager) RemoveTrader(traderID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if _, exists := tm.traders[traderID]; exists {
		delete(tm.traders, traderID)
		logger.Infof("✓ Trader %s removed from memory", traderID)
	}
}

// LoadUserTradersFromStore loads traders from store for a specific user to memory
func (tm *TraderManager) LoadUserTradersFromStore(st *store.Store, userID string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Get all traders for the specified user
	traders, err := st.Trader().List(userID)
	if err != nil {
		return fmt.Errorf("failed to get trader list for user %s: %w", userID, err)
	}

	logger.Infof("📋 Loading trader configurations for user %s: %d traders", userID, len(traders))

	// Get AI model and exchange lists (query only once outside loop)
	aiModels, err := st.AIModel().List(userID)
	if err != nil {
		logger.Infof("⚠️ Failed to get AI model config for user %s: %v", userID, err)
		return fmt.Errorf("failed to get AI model config: %w", err)
	}

	exchanges, err := st.Exchange().List(userID)
	if err != nil {
		logger.Infof("⚠️ Failed to get exchange config for user %s: %v", userID, err)
		return fmt.Errorf("failed to get exchange config: %w", err)
	}

	// Load configuration for each trader
	for _, traderCfg := range traders {
		// Check if this trader is already loaded
		if _, exists := tm.traders[traderCfg.ID]; exists {
			logger.Infof("⚠️ Trader %s already loaded, skipping", traderCfg.Name)
			continue
		}

		// Find AI model config from already queried list
		var aiModelCfg *store.AIModel
		for _, model := range aiModels {
			if model.ID == traderCfg.AIModelID {
				aiModelCfg = model
				break
			}
		}
		if aiModelCfg == nil {
			for _, model := range aiModels {
				if model.Provider == traderCfg.AIModelID {
					aiModelCfg = model
					break
				}
			}
		}

		if aiModelCfg == nil {
			logger.Infof("⚠️ AI model %s for trader %s does not exist, skipping", traderCfg.AIModelID, traderCfg.Name)
			continue
		}

		if !aiModelCfg.Enabled {
			logger.Infof("⚠️ AI model %s for trader %s is not enabled, skipping", traderCfg.AIModelID, traderCfg.Name)
			continue
		}

		// Find exchange config from already queried list
		var exchangeCfg *store.Exchange
		for _, exchange := range exchanges {
			if exchange.ID == traderCfg.ExchangeID {
				exchangeCfg = exchange
				break
			}
		}

		if exchangeCfg == nil {
			logger.Infof("⚠️ Exchange %s for trader %s does not exist, skipping", traderCfg.ExchangeID, traderCfg.Name)
			continue
		}

		if !exchangeCfg.Enabled {
			logger.Infof("⚠️ Exchange %s for trader %s is not enabled, skipping", traderCfg.ExchangeID, traderCfg.Name)
			continue
		}

		// Use existing method to load trader
		logger.Infof("📦 Loading trader %s (AI Model: %s, Exchange: %s/%s, Strategy ID: %s)", traderCfg.Name, aiModelCfg.Provider, exchangeCfg.ExchangeType, exchangeCfg.AccountName, traderCfg.StrategyID)
		err = tm.addTraderFromStore(traderCfg, aiModelCfg, exchangeCfg, st, false)
		if err != nil {
			logger.Infof("❌ Failed to load trader %s: %v", traderCfg.Name, err)
			// Save error for later retrieval
			tm.loadErrors[traderCfg.ID] = err
		} else {
			// Clear any previous error on success
			delete(tm.loadErrors, traderCfg.ID)
		}
	}

	return nil
}

// LoadTradersFromStore loads all traders from store to memory (new API)
func (tm *TraderManager) LoadTradersFromStore(st *store.Store) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Get all users
	userIDs, err := st.User().GetAllIDs()
	if err != nil {
		return fmt.Errorf("failed to get user list: %w", err)
	}

	logger.Infof("📋 Found %d users, loading all trader configurations...", len(userIDs))

	var allTraders []*store.Trader
	for _, userID := range userIDs {
		// Get traders for each user
		traders, err := st.Trader().List(userID)
		if err != nil {
			logger.Infof("⚠️ Failed to get traders for user %s: %v", userID, err)
			continue
		}
		logger.Infof("📋 User %s: %d traders", userID, len(traders))
		allTraders = append(allTraders, traders...)
	}

	logger.Infof("📋 Total loaded trader configurations: %d", len(allTraders))

	// Get AI model and exchange configs for each trader
	for _, traderCfg := range allTraders {
		// Get AI model config
		aiModels, err := st.AIModel().List(traderCfg.UserID)
		if err != nil {
			logger.Infof("⚠️  Failed to get AI model config: %v", err)
			continue
		}

		var aiModelCfg *store.AIModel
		// Prioritize exact match on model.ID
		for _, model := range aiModels {
			if model.ID == traderCfg.AIModelID {
				aiModelCfg = model
				break
			}
		}
		// If no exact match, try matching provider (for backward compatibility)
		if aiModelCfg == nil {
			for _, model := range aiModels {
				if model.Provider == traderCfg.AIModelID {
					aiModelCfg = model
					logger.Infof("⚠️  Trader %s using legacy provider match: %s -> %s", traderCfg.Name, traderCfg.AIModelID, model.ID)
					break
				}
			}
		}

		if aiModelCfg == nil {
			logger.Infof("⚠️  AI model %s for trader %s does not exist, skipping", traderCfg.AIModelID, traderCfg.Name)
			continue
		}

		if !aiModelCfg.Enabled {
			logger.Infof("⚠️  AI model %s for trader %s is not enabled, skipping", traderCfg.AIModelID, traderCfg.Name)
			continue
		}

		// Get exchange config
		exchanges, err := st.Exchange().List(traderCfg.UserID)
		if err != nil {
			logger.Infof("⚠️  Failed to get exchange config: %v", err)
			continue
		}

		var exchangeCfg *store.Exchange
		for _, exchange := range exchanges {
			if exchange.ID == traderCfg.ExchangeID {
				exchangeCfg = exchange
				break
			}
		}

		if exchangeCfg == nil {
			logger.Infof("⚠️  Exchange %s for trader %s does not exist, skipping", traderCfg.ExchangeID, traderCfg.Name)
			continue
		}

		if !exchangeCfg.Enabled {
			logger.Infof("⚠️  Exchange %s for trader %s is not enabled, skipping", traderCfg.ExchangeID, traderCfg.Name)
			continue
		}

		// Add to TraderManager (coinPoolURL/oiTopURL already obtained from strategy config)
		err = tm.addTraderFromStore(traderCfg, aiModelCfg, exchangeCfg, st, true)
		if err != nil {
			logger.Infof("❌ Failed to add trader %s: %v", traderCfg.Name, err)
			continue
		}
	}

	logger.Infof("✓ Successfully loaded %d traders to memory", len(tm.traders))
	return nil
}

// addTraderFromStore internal method: adds trader from store configuration
func (tm *TraderManager) addTraderFromStore(traderCfg *store.Trader, aiModelCfg *store.AIModel, exchangeCfg *store.Exchange, st *store.Store, restoreRuntime bool) error {
	if _, exists := tm.traders[traderCfg.ID]; exists {
		return fmt.Errorf("trader ID '%s' already exists", traderCfg.ID)
	}

	// Load strategy config (must have strategy)
	var strategyConfig *store.StrategyConfig
	if traderCfg.StrategyID != "" {
		strategy, err := st.Strategy().Get(traderCfg.UserID, traderCfg.StrategyID)
		if err != nil {
			return fmt.Errorf("failed to load strategy %s for trader %s: %w", traderCfg.StrategyID, traderCfg.Name, err)
		}
		// Parse JSON config
		strategyConfig, err = strategy.ParseConfig()
		if err != nil {
			return fmt.Errorf("failed to parse strategy config for trader %s: %w", traderCfg.Name, err)
		}
		logger.Infof("✓ Trader %s loaded strategy config: %s", traderCfg.Name, strategy.Name)
	} else {
		return fmt.Errorf("trader %s has no strategy configured", traderCfg.Name)
	}

	// Build AutoTraderConfig (coinPoolURL/oiTopURL obtained from strategy config, used in StrategyEngine)
	traderConfig := trader.AutoTraderConfig{
		ID:                    traderCfg.ID,
		Name:                  traderCfg.Name,
		AIModel:               aiModelCfg.Provider,
		Exchange:              exchangeCfg.ExchangeType, // Exchange type: binance/bybit/okx/etc
		ExchangeID:            exchangeCfg.ID,           // Exchange account UUID (for multi-account)
		BinanceAPIKey:         "",
		BinanceSecretKey:      "",
		HyperliquidPrivateKey: "",
		HyperliquidTestnet:    exchangeCfg.Testnet,
		UseQwen:               aiModelCfg.Provider == "qwen",
		DeepSeekKey:           "",
		QwenKey:               "",
		CustomAPIURL:          aiModelCfg.CustomAPIURL,
		CustomModelName:       aiModelCfg.CustomModelName,
		ScanInterval:          time.Duration(traderCfg.ScanIntervalMinutes) * time.Minute,
		InitialBalance:        traderCfg.InitialBalance,
		IsCrossMargin:         traderCfg.IsCrossMargin,
		ShowInCompetition:     traderCfg.ShowInCompetition,
		StrategyConfig:        strategyConfig,
	}

	// Set API keys based on exchange type
	switch exchangeCfg.ExchangeType {
	case "binance":
		traderConfig.BinanceAPIKey = exchangeCfg.APIKey
		traderConfig.BinanceSecretKey = exchangeCfg.SecretKey
	case "bybit":
		traderConfig.BybitAPIKey = exchangeCfg.APIKey
		traderConfig.BybitSecretKey = exchangeCfg.SecretKey
	case "okx":
		traderConfig.OKXAPIKey = exchangeCfg.APIKey
		traderConfig.OKXSecretKey = exchangeCfg.SecretKey
		traderConfig.OKXPassphrase = exchangeCfg.Passphrase
	case "bitget":
		traderConfig.BitgetAPIKey = exchangeCfg.APIKey
		traderConfig.BitgetSecretKey = exchangeCfg.SecretKey
		traderConfig.BitgetPassphrase = exchangeCfg.Passphrase
	case "hyperliquid":
		traderConfig.HyperliquidPrivateKey = exchangeCfg.APIKey
		traderConfig.HyperliquidWalletAddr = exchangeCfg.HyperliquidWalletAddr
	case "aster":
		traderConfig.AsterUser = exchangeCfg.AsterUser
		traderConfig.AsterSigner = exchangeCfg.AsterSigner
		traderConfig.AsterPrivateKey = exchangeCfg.AsterPrivateKey
	case "lighter":
		traderConfig.LighterPrivateKey = exchangeCfg.LighterPrivateKey
		traderConfig.LighterWalletAddr = exchangeCfg.LighterWalletAddr
		traderConfig.LighterAPIKeyPrivateKey = exchangeCfg.LighterAPIKeyPrivateKey
		traderConfig.LighterAPIKeyIndex = exchangeCfg.LighterAPIKeyIndex
		traderConfig.LighterTestnet = exchangeCfg.Testnet
	}

	// Set API keys based on AI model
	switch aiModelCfg.Provider {
	case "qwen":
		traderConfig.QwenKey = aiModelCfg.APIKey
	case "deepseek":
		traderConfig.DeepSeekKey = aiModelCfg.APIKey
	default:
		// For other providers (grok, openai, claude, gemini, kimi, etc.), use CustomAPIKey
		traderConfig.CustomAPIKey = aiModelCfg.APIKey
	}

	// Create trader instance
	at, err := trader.NewAutoTrader(traderConfig, st, traderCfg.UserID)
	if err != nil {
		return fmt.Errorf("failed to create trader: %w", err)
	}

	// Set custom prompt (if exists)
	if traderCfg.CustomPrompt != "" {
		at.SetCustomPrompt(traderCfg.CustomPrompt)
		at.SetOverrideBasePrompt(traderCfg.OverrideBasePrompt)
		if traderCfg.OverrideBasePrompt {
			logger.Infof("✓ Set custom trading strategy prompt (overriding base prompt)")
		} else {
			logger.Infof("✓ Set custom trading strategy prompt (supplementing base prompt)")
		}
	}

	tm.traders[traderCfg.ID] = at
	at.SetLifecycleGeneration(traderCfg.LifecycleGeneration)
	logger.Infof("✓ Trader '%s' (%s + %s/%s) loaded to memory", traderCfg.Name, aiModelCfg.Provider, exchangeCfg.ExchangeType, exchangeCfg.AccountName)

	// Only the process-start loader restores RUNNING runtimes. User-scoped
	// refreshes are configuration loads and must never start a second engine or
	// mark the authoritative lifecycle stopped because a runtime already exists.
	if restoreRuntime && traderCfg.IsRunning {
		logger.Infof("🔄 Auto-starting trader '%s' (was running before shutdown)...", traderCfg.Name)

		// Check decision mode to determine startup type
		decisionMode, _ := st.CopyTrade().GetDecisionMode(traderCfg.ID)

		if decisionMode == "copy_trade" {
			// Start in copy trade mode
			logger.Infof("🎯 [%s] Copy trade mode detected, starting copy trading engine...", traderCfg.Name)
			// Process startup must finish deterministic order/ownership recovery
			// before the global reentry advisor is started by main. Running this
			// synchronously closes the former window in which a WAITING candidate
			// could be claimed while its exchange intent was still recovering.
			if err := copytrade.StartCopyTradingForTrader(traderCfg.ID, at, st); err != nil {
				logger.Warnf("⚠️ Trader '%s' copy trade stopped with error: %v", traderCfg.Name, err)
				if st != nil {
					_ = st.Trader().UpdateStatus(traderCfg.UserID, traderCfg.ID, false)
				}
				return fmt.Errorf("restore copy trader %s: %w", traderCfg.Name, err)
			}
			logger.Infof("✅ Trader '%s' started in copy trade mode", traderCfg.Name)
		} else {
			// Start in AI mode (default)
			go func(autoTrader *trader.AutoTrader, traderName, traderID, userID string) {
				if err := autoTrader.Run(); err != nil {
					logger.Warnf("⚠️ Trader '%s' stopped with error: %v", traderName, err)
					if st != nil {
						_ = st.Trader().UpdateStatus(userID, traderID, false)
					}
				}
			}(at, traderCfg.Name, traderCfg.ID, traderCfg.UserID)
			logger.Infof("✅ Trader '%s' auto-started in AI mode", traderCfg.Name)
		}
	}

	return nil
}

// GetTraderExecutor returns a TraderExecutor for the given trader ID
// This is used by the debate module to execute consensus trades
func (tm *TraderManager) GetTraderExecutor(traderID string) (debate.TraderExecutor, error) {
	at, err := tm.GetTrader(traderID)
	if err != nil {
		return nil, err
	}
	return &TraderExecutorAdapter{autoTrader: at}, nil
}
