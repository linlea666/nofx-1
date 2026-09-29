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

// PreviewCurrentPositions accepts draft settings, and never saves configuration,
// initializes a trading runtime, or sends any exchange mutation.
func (h *CopyTradeHandler) PreviewCurrentPositions(c *gin.Context) {
	var req struct {
		TraderID   string  `json:"trader_id"`
		ExchangeID string  `json:"exchange_id"`
		Provider   string  `json:"provider_type"`
		LeaderID   string  `json:"leader_id"`
		Ratio      float64 `json:"copy_ratio"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Provider != "okx" || strings.TrimSpace(req.LeaderID) == "" || req.Ratio <= 0 || req.Ratio > maxCopyRatio || math.IsNaN(req.Ratio) || math.IsInf(req.Ratio, 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 OKX 领航员、执行账户及有效跟单系数"})
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
		source := copytrade.NewOKXProvider()
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
		tasks, err := copytrade.BuildCurrentPositionPreview(h.store, req.TraderID, req.LeaderID, req.Ratio, state, equity, positions, executor.GetMarketPrice, resolver)
		if err != nil {
			return nil, err
		}
		return gin.H{"positions": tasks, "snapshot_at": state.Timestamp, "leader_equity": state.TotalEquity, "follower_equity": equity}, nil
	}()
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "复制仓位预览失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// Validate before the general trader endpoint saves unrelated trader fields.
// The config transaction repeats the authoritative generation/identity check.
func validateCurrentCopyRegistration(st *store.Store, traderID, accountID, mode, provider, leader, requestID string, enabled *bool) error {
	if enabled == nil || !*enabled {
		return nil
	}
	if mode != "copy_trade" || provider != "okx" || strings.TrimSpace(leader) == "" {
		return fmt.Errorf("复制当前仓位需要 OKX 领航员及跟单模式")
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
