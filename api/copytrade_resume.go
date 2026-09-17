package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"nofx/copytrade"
)

func (h *CopyTradeHandler) ResumeFollowPosition(c *gin.Context) {
	traderID := c.Param("trader_id")
	if _, err := h.store.Trader().GetLifecycleForUser(c.GetString("user_id"), traderID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "trader not found or access denied"})
		return
	}
	var req StopFollowPositionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "leader_pos_id is required"})
		return
	}
	if err := copytrade.ResumeFollowingPosition(traderID, req.LeaderPosID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "active", "message": "已恢复跟单，只跟随后续动作，不补做暂停期间交易"})
}
