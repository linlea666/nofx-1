package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"nofx/store"
)

type copyRuntimeHealthView struct {
	TraderID         string                               `json:"trader_id"`
	TraderName       string                               `json:"trader_name"`
	Running          bool                                 `json:"running"`
	Source           *store.CopyRuntimeSource             `json:"source"`
	ExecutionIssues  []*store.LeaderTransitionFacts       `json:"execution_issues"`
	RuntimeIssues    []store.CopyRuntimeIssue             `json:"runtime_issues"`
	SettlementIssues []store.PositionSettlementDiagnostic `json:"settlement_issues"`
}

// RuntimeHealth is read-only and scoped to the authenticated user's traders.
// Execution blockers, protection work and historical fees are separate so an
// old accounting incident cannot imply every live trade is blocked.
func (h *CopyTradeHandler) RuntimeHealth(c *gin.Context) {
	traderID := c.Query("trader_id")
	traders, err := h.store.Trader().List(c.GetString("user_id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read trader health"})
		return
	}
	views := []copyRuntimeHealthView{}
	found := traderID == ""
	for _, t := range traders {
		if traderID != "" && t.ID != traderID {
			continue
		}
		if t.ID == traderID {
			found = true
		}
		cfg, configErr := h.store.CopyTrade().GetByTraderID(t.ID)
		if errors.Is(configErr, sql.ErrNoRows) {
			continue
		}
		if configErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read copy configuration"})
			return
		}
		if !cfg.Enabled || (traderID == "" && !t.IsRunning) {
			continue
		}
		view := copyRuntimeHealthView{TraderID: t.ID, TraderName: t.Name, Running: t.IsRunning, ExecutionIssues: []*store.LeaderTransitionFacts{}, RuntimeIssues: []store.CopyRuntimeIssue{}, SettlementIssues: []store.PositionSettlementDiagnostic{}}
		view.Source, err = h.store.CopyTrade().GetRuntimeSource(t.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read source health"})
			return
		}
		if view.Source != nil {
			view.Source.LastError = sanitizeSourceHealthError(view.Source.LastError)
		}
		view.ExecutionIssues, err = h.store.CopyTrade().ListLeaderTransitionIssues(t.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to classify execution health"})
			return
		}
		if view.ExecutionIssues == nil {
			view.ExecutionIssues = []*store.LeaderTransitionFacts{}
		}
		for _, issue := range view.ExecutionIssues {
			issue.Fingerprint = ""
		}
		view.RuntimeIssues, err = h.store.CopyTrade().ListRuntimeIssues(t.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read protection follow-ups"})
			return
		}
		for i := range view.RuntimeIssues {
			view.RuntimeIssues[i].Detail = sanitizeSourceHealthError(view.RuntimeIssues[i].Detail)
		}
		cycles, cycleErr := h.store.CopyTrade().ListOpenCopyGuardCycles(t.ID)
		if cycleErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read live protection health"})
			return
		}
		for _, cycle := range cycles {
			switch cycle.ProtectionStatus {
			case store.CopyGuardProtectionPending, store.CopyGuardProtectionUnknown, store.CopyGuardProtectionDegraded,
				store.CopyGuardProtectionUnprotectedWarning, store.CopyGuardProtectionForcedExitPending, store.CopyGuardProtectionUnprotectable:
			default:
				continue
			}
			issue := store.CopyRuntimeIssue{TraderID: t.ID, Area: "protection", ResourceID: fmt.Sprintf("cycle:%d", cycle.ID), LeaderPosID: cycle.LeaderPosID, Symbol: cycle.Symbol, Side: cycle.Side, Code: "PROTECTION_" + cycle.ProtectionStatus, Detail: sanitizeSourceHealthError(cycle.ProtectionError), LastSeen: cycle.UpdatedAt}
			if cycle.ProtectionMissingAt != nil {
				issue.FirstSeen = *cycle.ProtectionMissingAt
			}
			view.RuntimeIssues = append(view.RuntimeIssues, issue)
		}
		if t.ExchangeID != "" {
			view.SettlementIssues, err = h.store.Position().ListSettlementDiagnostics(t.ExchangeID, false, 50)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read settlement diagnostics"})
				return
			}
			if view.SettlementIssues == nil {
				view.SettlementIssues = []store.PositionSettlementDiagnostic{}
			}
			for i := range view.SettlementIssues {
				view.SettlementIssues[i].Detail = sanitizeSourceHealthError(view.SettlementIssues[i].Detail)
			}
		}
		views = append(views, view)
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "trader not found or access denied"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"traders": views})
}
