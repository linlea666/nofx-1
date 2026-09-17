package copytrade

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"nofx/store"
	"nofx/trader"
)

type FollowPositionView struct {
	*store.CopyTradePositionMapping
	CustodyState       string `json:"custody_state"`
	CurrentPositionKey string `json:"current_position_key,omitempty"`
	CanResume          bool   `json:"can_resume"`
	ResumeReason       string `json:"resume_reason,omitempty"`
}

func InspectFollowingPosition(st *store.Store, m *store.CopyTradePositionMapping) FollowPositionView {
	view := FollowPositionView{CopyTradePositionMapping: m, CustodyState: "UNKNOWN", ResumeReason: "仓位归属待核实"}
	p, err := st.CopyTrade().GetPositionCustody(m.TraderID, m.LeaderPosID)
	if err != nil {
		return view
	}
	view.CustodyState = p.State
	if p.State == "RELEASED" {
		view.ResumeReason = "原跟单仓已结束，不能恢复到后来新开的仓位"
		return view
	}
	ti, ok := runningIntegration(m.TraderID)
	if !ok || !ti.IsRunning() {
		view.CustodyState = "UNKNOWN"
		view.ResumeReason = "交易员未运行"
		return view
	}
	key, err := ti.verifyFollowingPosition(m, p)
	if err != nil {
		view.CustodyState = "UNKNOWN"
		view.ResumeReason = err.Error()
		return view
	}
	view.CurrentPositionKey = key
	view.ResumeReason = ""
	view.CanResume = m.Status == store.MappingStatusManualStopped
	return view
}

func (ti *TraderIntegration) verifyFollowingPosition(m *store.CopyTradePositionMapping, p *store.CopyPositionCustody) (string, error) {
	if p == nil || p.State != "MANAGED" {
		return "", fmt.Errorf("原跟单仓已结束")
	}
	if p.CycleID > 0 {
		c, err := ti.store.CopyTrade().GetCopyGuardCycle(p.CycleID)
		if err != nil {
			return "", err
		}
		err = ti.verifyCopyGuardContinuity(c)
		if err != nil {
			return "", fmt.Errorf("原仓连续性待核实: %w", err)
		}
		if c.Status != store.CopyGuardFollowing && c.Status != store.CopyGuardFollowingReentry {
			return "", fmt.Errorf("仓位已进入止损退出流程")
		}
	} else {
		history, ok := ti.executor.(trader.SymbolTradeHistoryProvider)
		if !ok {
			return "", fmt.Errorf("无法核实原仓成交记录")
		}
		intent, err := ti.store.CopyTrade().GetExecutionIntentByID(p.InitialIntentID)
		if err != nil {
			return "", err
		}
		start := intent.CreatedAt.Add(-2 * time.Second)
		entryID := intent.ExchangeOrderID
		orders, err := ti.store.CopyTrade().ListExecutionOrderAttempts(intent.ID)
		if err != nil {
			return "", err
		}
		for _, order := range orders {
			if order.FilledQuantity > 0 && order.ExchangeOrderID != "" {
				entryID = order.ExchangeOrderID
				break
			}
		}
		var fills []trader.TradeRecord
		if scoped, ok := ti.executor.(trader.ScopedTradeHistoryProvider); ok {
			fills, err = scoped.GetTradesForPosition(p.Symbol, p.MarginMode, start)
		} else {
			fills, err = history.GetTradesForSymbol(p.Symbol, start, 100)
		}
		if err != nil {
			return "", err
		}
		ended, err := positionEndedInFills(fills, entryID, p.Symbol, p.Side)
		if err != nil {
			return "", err
		}
		if ended {
			_ = ti.store.CopyTrade().ReleasePositionCustody(ti.traderID, m.LeaderPosID, p.InitialIntentID, "CONFIRMED_FLAT_IN_FILLS")
			return "", errCopyPositionEnded
		}
	}
	positions, err := ti.getFreshPositions()
	if err != nil {
		return "", err
	}
	var key string
	for _, pos := range positions {
		mode := getStringField(pos, "marginMode", "mgnMode")
		if getStringField(pos, "symbol") != p.Symbol || !strings.EqualFold(getStringField(pos, "side"), p.Side) || mode != p.MarginMode || absFloat(getFloatField(pos, "positionAmt", "quantity")) <= 0 {
			continue
		}
		if key != "" {
			return "", fmt.Errorf("存在多个无法区分的仓位")
		}
		key = trader.CopyPositionKey(p.Symbol, p.Side, mode, getStringField(pos, "posId", "positionId"))
	}
	if key == "" {
		return "", fmt.Errorf("原跟单仓当前不存在")
	}
	return key, nil
}

func StopFollowingPosition(traderID, leaderPosID string, st *store.Store) (bool, error) {
	if ti, ok := runningIntegration(traderID); ok {
		ti.riskExitMu.Lock()
		defer ti.riskExitMu.Unlock()
	}
	return st.CopyTrade().MarkManualStopped(traderID, leaderPosID)
}

func ResumeFollowingPosition(traderID, leaderPosID string) error {
	ti, ok := runningIntegration(traderID)
	if !ok || !ti.IsRunning() {
		return fmt.Errorf("交易员未运行")
	}
	ti.executionMu.Lock()
	defer ti.executionMu.Unlock()
	ti.engine.followControlMu.Lock()
	defer ti.engine.followControlMu.Unlock()
	m, err := ti.store.CopyTrade().GetMapping(traderID, leaderPosID)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("跟单映射不存在")
	}
	if m.Status == store.MappingStatusActive {
		return nil
	}
	if m.Status != store.MappingStatusManualStopped {
		return fmt.Errorf("只有手动暂停的原仓位可以恢复")
	}
	p, err := ti.store.CopyTrade().GetPositionCustody(traderID, leaderPosID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("原仓归属尚未核实")
	}
	if err != nil {
		return err
	}
	if _, err = ti.verifyFollowingPosition(m, p); err != nil {
		return err
	}
	if err = ti.engine.syncLeaderState(); err != nil {
		return fmt.Errorf("领航员当前仓位不可用: %w", err)
	}
	leader := ti.engine.buildLeaderPosMap()[leaderPosID]
	if leader == nil || leader.Size <= 0 || !strings.EqualFold(string(leader.Side), m.Side) || (ti.engine.config.SyncMarginMode && leader.MarginMode != m.MarginMode) {
		return fmt.Errorf("领航员原仓已经结束或身份变化")
	}
	ti.riskExitMu.Lock()
	defer ti.riskExitMu.Unlock()
	if ti.riskExitGates[leaderPosID] != nil {
		return fmt.Errorf("原仓止损退出尚未结束")
	}
	if err = ti.store.CopyTrade().ResumeFollowing(traderID, leaderPosID, m.SourceRevision, leader.Size); err != nil {
		return err
	}
	if p.CycleID > 0 {
		_ = ti.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: p.CycleID, TraderID: traderID, Type: "MANUAL_FOLLOW_RESUMED", Metadata: map[string]interface{}{"leader_size_baseline": leader.Size, "replay_paused_trades": false}})
	}
	return nil
}
