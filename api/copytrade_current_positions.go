package api

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"nofx/copytrade"
	"nofx/store"
	"nofx/trader"
)

// newCurrentCopySource builds the read-only leader source for a preview.
// Replaceable in tests; production never mutates anything through it.
var newCurrentCopySource = func(st *store.Store, provider, binanceMode, leaderID string) (copytrade.LeaderProvider, error) {
	switch provider {
	case string(copytrade.ProviderOKX):
		return copytrade.NewOKXProvider(), nil
	case string(copytrade.ProviderHyperliquid):
		return copytrade.NewHyperliquidProvider(), nil
	case string(copytrade.ProviderBinance):
		// Same construction and global credentials as the running engine.
		return copytrade.NewProviderWithLoader(&copytrade.CopyConfig{ProviderType: copytrade.ProviderBinance, BinanceSourceMode: copytrade.BinanceSourceMode(binanceMode)}, st.BinanceCreds())
	case string(copytrade.ProviderFollowCoin):
		if _, err := copytrade.ParseFollowCoinAPIID(leaderID); err != nil {
			return nil, err
		}
		token, err := st.FollowCoinCreds().LoadFollowCoinToken(store.FollowCoinCredsLabelDefault)
		if err != nil {
			return nil, err
		}
		if token == "" {
			return nil, fmt.Errorf("请先在「FollowCoin 凭证」中配置 token")
		}
		return copytrade.NewFollowCoinProviderWithLoader(st.FollowCoinCreds()), nil
	default:
		return nil, fmt.Errorf("复制当前仓位不支持该数据源：%s", provider)
	}
}

// PreviewCurrentPositions accepts draft settings, and never saves configuration,
// initializes a trading runtime, or sends any exchange mutation.
func (h *CopyTradeHandler) PreviewCurrentPositions(c *gin.Context) {
	var req struct {
		TraderID           string  `json:"trader_id"`
		ExchangeID         string  `json:"exchange_id"`
		Provider           string  `json:"provider_type"`
		BinanceSourceMode  string  `json:"binance_source_mode"`
		LeaderID           string  `json:"leader_id"`
		Ratio              float64 `json:"copy_ratio"`
		PriceFilterEnabled bool    `json:"price_filter_enabled"`
		MinAdvantagePct    float64 `json:"min_advantage_pct"`
		// Manual tolerance: rows within it are selectable but not preselected.
		ManualMaxDisadvantagePct float64 `json:"manual_max_disadvantage_pct"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !store.CurrentPositionCopySupported(req.Provider) || strings.TrimSpace(req.LeaderID) == "" || req.Ratio <= 0 || req.Ratio > maxCopyRatio || math.IsNaN(req.Ratio) || math.IsInf(req.Ratio, 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要受支持的领航员数据源、执行账户及有效跟单系数"})
		return
	}
	if req.Provider == string(copytrade.ProviderBinance) {
		if req.BinanceSourceMode == "" {
			req.BinanceSourceMode = string(copytrade.BinanceSourceCopyManagement)
		}
		// The saved Smart Money leader is the normalized topTraderId; the
		// preview must check eligibility against the same identity.
		if req.BinanceSourceMode == string(copytrade.BinanceSourceSmartMoney) {
			id, err := copytrade.NormalizeTopTraderID(req.LeaderID)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			req.LeaderID = id
		}
	}
	if math.IsNaN(req.MinAdvantagePct) || math.IsInf(req.MinAdvantagePct, 0) || math.IsNaN(req.ManualMaxDisadvantagePct) || math.IsInf(req.ManualMaxDisadvantagePct, 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "价格优势阈值无效"})
		return
	}
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	if req.TraderID != "" {
		if _, err := h.store.Trader().GetLifecycleForUser(userID, req.TraderID); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "trader not found or access denied"})
			return
		}
	}
	exchange, err := h.store.Exchange().GetByID(userID, req.ExchangeID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "execution account not found or access denied"})
		return
	}
	executor, err := trader.NewAccountQueryTrader(exchange, userID)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	result, err := func() (gin.H, error) {
		source, err := newCurrentCopySource(h.store, req.Provider, req.BinanceSourceMode, req.LeaderID)
		if err != nil {
			return nil, err
		}
		state, err := source.GetAccountState(req.LeaderID)
		if err != nil {
			return nil, err
		}
		balance, err := executor.GetBalance()
		if err != nil {
			return nil, err
		}
		equity, _ := balance["total_equity"].(float64)
		if equity <= 0 {
			wallet, _ := balance["totalWalletBalance"].(float64)
			profit, _ := balance["totalUnrealizedProfit"].(float64)
			equity = wallet + profit
		}
		fresh, ok := executor.(trader.FreshPositionProvider)
		if !ok {
			return nil, fmt.Errorf("fresh position query unavailable")
		}
		positions, err := fresh.GetPositionsFresh()
		if err != nil {
			return nil, err
		}
		resolver, _ := executor.(trader.ExecutionInstrumentResolver)
		tasks, leaderEquity, err := copytrade.PreviewCurrentPositionCopy(h.store, copytrade.CurrentCopyPreviewRequest{
			Provider: copytrade.ProviderType(req.Provider), Source: source,
			TraderID: req.TraderID, LeaderID: req.LeaderID, Ratio: req.Ratio,
			State: state, FollowerEquity: equity, FollowerPositions: positions,
			Price: executor.GetMarketPrice, Resolver: resolver,
			PriceFilterEnabled: req.PriceFilterEnabled, MinAdvantagePct: req.MinAdvantagePct, ManualMaxDisadvantagePct: req.ManualMaxDisadvantagePct,
		})
		if err != nil {
			return nil, err
		}
		return gin.H{"positions": tasks, "snapshot_at": state.Timestamp, "leader_equity": leaderEquity, "follower_equity": equity}, nil
	}()
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "复制仓位预览失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// validateCurrentCopyOptionsRequest checks the selection and price rule of an
// enabled registration before the trader endpoint persists anything.
func validateCurrentCopyOptionsRequest(enabled *bool, selection []store.CurrentPositionCopySelection, minPct, manualPct *float64) error {
	if enabled == nil || !*enabled {
		return nil
	}
	return store.ValidateCurrentCopyOptions(&store.CopyTradeConfig{CopyCurrentPositionsSelection: selection, CopyCurrentMinAdvantagePct: minPct, CopyCurrentManualMaxDisadvantagePct: manualPct})
}

// Validate before the general trader endpoint saves unrelated trader fields.
// The config transaction repeats the authoritative generation/identity check.
func validateCurrentCopyRegistration(st *store.Store, traderID, accountID, mode, provider, leader, requestID string, enabled *bool) error {
	if enabled == nil || !*enabled {
		return nil
	}
	if mode != "copy_trade" || !store.CurrentPositionCopySupported(provider) || strings.TrimSpace(leader) == "" {
		return fmt.Errorf("复制当前仓位需要跟单模式及受支持的领航员数据源")
	}
	if _, err := uuid.Parse(requestID); err != nil {
		return fmt.Errorf("复制当前仓位需要有效的请求幂等编号")
	}
	var owner, priorAccount, priorLeader, priorProvider string
	err := st.DB().QueryRow(`SELECT trader_id,exchange_id,leader_id,provider FROM copy_trade_current_position_requests WHERE request_id=?`, requestID).Scan(&owner, &priorAccount, &priorLeader, &priorProvider)
	if err == nil {
		if owner != traderID || priorAccount != accountID || priorLeader != leader || priorProvider != provider {
			return fmt.Errorf("复制请求配置身份已改变，请重新开启复制开关")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if traderID != "" {
		lifecycle, err := st.Trader().GetLifecycle(traderID)
		if err != nil {
			return err
		}
		if lifecycle.Status != store.TraderLifecycleStopped {
			return fmt.Errorf("请先停止交易员，再登记下次启动复制仓位")
		}
	}
	return nil
}
