package copytrade

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

func proportionalCopyNotional(sourceNotional, leaderEquity, followerEquity, ratio float64) float64 {
	return ratio * (sourceNotional / leaderEquity) * followerEquity
}

// BuildCurrentPositionPreview is a query-only view of the same source identity,
// proportional sizing and independent-position rules used at entry.
func BuildCurrentPositionPreview(st *store.Store, traderID, leaderID string, ratio float64, state *AccountState, followerEquity float64, positions []map[string]interface{}, price func(string) (float64, error), resolver trader.ExecutionInstrumentResolver) ([]store.CurrentPositionCopyTask, error) {
	if state == nil || state.Positions == nil {
		return nil, fmt.Errorf("完整领航员仓位快照不可用")
	}
	if len(state.Positions) > 0 && (!positiveFinite(state.TotalEquity) || !positiveFinite(followerEquity)) {
		return nil, fmt.Errorf("领航员或跟随账户总权益不可用，复制请求保留待执行")
	}
	out := make([]store.CurrentPositionCopyTask, 0, len(state.Positions))
	keys := make([]string, 0, len(state.Positions))
	for id := range state.Positions {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	for _, id := range keys {
		p := state.Positions[id]
		if p == nil || p.Size <= 0 {
			continue
		}
		t := store.CurrentPositionCopyTask{LeaderPosID: p.PosID, Symbol: p.Symbol, Side: string(p.Side), MarginMode: p.MarginMode, OpenedMS: p.OpenedMS, Size: p.Size, EntryPrice: p.EntryPrice, Leverage: p.Leverage, Status: "READY"}
		if t.LeaderPosID != "" && seen[t.LeaderPosID] {
			return nil, fmt.Errorf("领航员快照包含重复仓位标识，复制请求保留待执行")
		}
		seen[t.LeaderPosID] = true
		if t.LeaderPosID == "" {
			t.LeaderPosID = "unknown:" + id
			t.Reason = "SOURCE_IDENTITY_UNAVAILABLE"
		}
		if !positiveFinite(t.Size) {
			t.Size = 0
			t.Reason = "SOURCE_IDENTITY_UNAVAILABLE"
		}
		if !positiveFinite(t.EntryPrice) {
			t.EntryPrice = 0
		}
		reason, err := st.CopyTrade().CurrentCopyEligibility(traderID, leaderID, t)
		if err != nil {
			return nil, err
		}
		if t.Reason == "" {
			t.Reason = reason
		}

		if !positiveFinite(p.PositionValue) {
			t.Reason = "SOURCE_VALUE_UNAVAILABLE"
		} else {
			t.Notional = proportionalCopyNotional(p.PositionValue, state.TotalEquity, followerEquity, ratio)
			if !positiveFinite(t.Notional) {
				t.Notional = 0
				t.Reason = "SOURCE_VALUE_UNAVAILABLE"
			}
		}
		if resolver == nil {
			t.Reason = "EXECUTION_INSTRUMENT_UNAVAILABLE"
		} else if instrument, resolveErr := resolver.ResolveExecutionInstrument(p.Symbol); resolveErr != nil || instrument == nil {
			t.Reason = "EXECUTION_INSTRUMENT_UNAVAILABLE"
		}
		if price != nil {
			t.ReferencePrice, err = price(p.Symbol)
		}
		if err != nil || !positiveFinite(t.ReferencePrice) {
			t.ReferencePrice = 0
			t.Reason = "EXECUTION_PRICE_UNAVAILABLE"
		}
		for _, actual := range positions {
			if !strings.EqualFold(getStringField(actual, "symbol"), p.Symbol) || !strings.EqualFold(getStringField(actual, "side"), string(p.Side)) {
				continue
			}
			qty, _ := knownQuantityField(actual, "positionAmt", "quantity", "size")
			if math.Abs(qty) <= 0 {
				continue
			}
			var owned int
			if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_position_custody WHERE trader_id=? AND symbol=? AND side=? AND margin_mode=? AND state='MANAGED'`, traderID, p.Symbol, string(p.Side), getStringField(actual, "marginMode", "mgnMode")).Scan(&owned); err != nil {
				return nil, err
			}
			if owned == 0 {
				t.Reason = "INDEPENDENT_SAME_SIDE_POSITION"
			}
		}
		if t.Reason != "" {
			t.Status = "SKIPPED"
		}
		out = append(out, t)
	}
	if err := st.CopyTrade().ValidateCurrentCopyCandidates(traderID, leaderID, out); err != nil {
		return nil, err
	}
	return out, nil
}
func positiveFinite(n float64) bool { return n > 0 && !math.IsNaN(n) && !math.IsInf(n, 0) }

func (e *Engine) prepareCurrentPositionCopy(state *AccountState) error {
	if e.store == nil || e.config.ProviderType != ProviderOKX {
		return nil
	}
	request, err := e.store.CopyTrade().ActiveCurrentPositionCopy(e.traderID)
	if err != nil || request == nil {
		return err
	}
	if request.Status == "SEALED" {
		if err = e.store.CopyTrade().RefreshCurrentPositionCopy(e.traderID); err != nil {
			return err
		}
		request, err = e.store.CopyTrade().ActiveCurrentPositionCopy(e.traderID)
		if err != nil || request == nil {
			return err
		}
	}
	if request.Status == "PENDING" {
		if e.currentCopyPreview == nil {
			return fmt.Errorf("当前仓位复制的查询适配器不可用")
		}
		positions, err := e.currentCopyPreview(state)
		if err != nil {
			return err
		}
		generation := e.config.SourceGeneration
		if generation <= 0 {
			generation = 1
		}
		return e.store.CopyTrade().SealCurrentPositionCopy(e.traderID, e.config.LeaderID, generation, positions, state.Timestamp)
	}
	if request.Status != "SEALED" {
		return nil
	}
	positions := map[string]*Position{}
	for _, p := range state.Positions {
		if p != nil {
			positions[p.PosID] = p
		}
	}
	snapshotAt, _ := time.Parse(time.RFC3339Nano, request.SnapshotAt)
	expired := !snapshotAt.IsZero() && time.Since(snapshotAt) > time.Duration(e.config.CopyCatchupWindowSeconds)*time.Second
	for _, task := range request.Tasks {
		p := positions[task.LeaderPosID]
		changed := p == nil || !positiveFinite(p.Size) || p.OpenedMS != task.OpenedMS || string(p.Side) != task.Side || p.MarginMode != task.MarginMode
		if task.IntentID > 0 {
			intent, err := e.store.CopyTrade().GetExecutionIntentByID(task.IntentID)
			if err != nil {
				return err
			}
			sizeChanged := p != nil && math.Abs(p.Size-intent.LeaderTargetSize) > math.Max(1e-12, intent.LeaderTargetSize*1e-8)
			if !changed && !sizeChanged && !expired {
				continue
			}
			facts, err := e.store.CopyTrade().InspectLeaderTransition(task.IntentID)
			if err != nil {
				return err
			}
			if facts.Effect != store.SourceEffectUnsubmitted && facts.Effect != store.SourceEffectZeroFill {
				continue
			}
			if !changed && sizeChanged && !expired {
				if err = e.store.CopyTrade().RefreshUnsubmittedCurrentCopyTarget(task.ID, task.IntentID, p.Size); err != nil {
					return err
				}
				continue
			}
			if err = e.store.CopyTrade().FinishLeaderSourceTransition(store.FinishLeaderSourceTransitionRequest{IntentID: task.IntentID, TraderID: e.traderID, LeaderID: e.config.LeaderID, Disposition: store.SourceSupersedeNoFill, Reason: "CURRENT_POSITION_COPY_EXPIRED_OR_CHANGED", Evidence: "complete source snapshot invalidated the unsubmitted one-shot entry"}); err != nil {
				return err
			}
		} else if task.Status == "READY" && (changed || expired) {
			if err = e.store.CopyTrade().SkipCurrentPositionCopyTask(task.ID, "CURRENT_POSITION_COPY_EXPIRED_OR_CHANGED"); err != nil {
				return err
			}
		}
	}
	return e.store.CopyTrade().RefreshCurrentPositionCopy(e.traderID)
}

func (e *Engine) currentPositionCopyFill(pos *Position, m *store.CopyTradePositionMapping) *Fill {
	task, err := e.store.CopyTrade().CurrentPositionCopyTask(e.traderID, m.LeaderPosID)
	if err != nil || task == nil || pos.PosID != m.LeaderPosID || pos.OpenedMS != task.OpenedMS || string(pos.Side) != task.Side || pos.MarginMode != task.MarginMode {
		return nil
	}
	fill := e.buildBinanceSnapshotFillForPosition(pos, m.LeaderPosID, ActionOpen, pos.Size, 0, pos.Size, m.SourceRevision)
	fill.CurrentPositionTaskID = task.ID
	fill.ID = fmt.Sprintf("current-position-copy|%d|%d", task.ID, m.SourceRevision)
	return &fill
}

// Revalidation uses the public source query only. It runs inside the ordinary
// execution mutex immediately before the existing preflight/submission path.
func (ti *TraderIntegration) validateCurrentPositionCopy(dec *decision.Decision) error {
	if dec.ExecutionIntentID > 0 && dec.CurrentPositionTaskID == 0 {
		task, err := ti.store.CopyTrade().CurrentPositionTaskForIntent(dec.ExecutionIntentID)
		if err != nil {
			return err
		}
		if task != nil {
			dec.CurrentPositionTaskID = task.ID
			dec.SourceOpenedMS = task.OpenedMS
		}
	}
	if dec.CurrentPositionTaskID == 0 {
		return nil
	}
	task, lookupErr := ti.store.CopyTrade().CurrentPositionCopyTask(ti.traderID, dec.LeaderPosID)
	if lookupErr != nil {
		return leaderCopyPreflightError("SOURCE_DATA_UNAVAILABLE", "current-position authorization query: %v", lookupErr)
	}
	if task == nil || task.ID != dec.CurrentPositionTaskID {
		return leaderCopyPreflightError("SOURCE_SUPERSEDED", "current-position copy authorization changed")
	}
	if snapshot, err := time.Parse(time.RFC3339Nano, task.SnapshotAt); err != nil || time.Since(snapshot) > time.Duration(ti.engine.config.CopyCatchupWindowSeconds)*time.Second {
		return leaderCopyPreflightError("SOURCE_SUPERSEDED", "本次复制窗口已结束")
	}
	state, err := ti.engine.currentSourceSnapshot()
	if err != nil {
		return leaderCopyPreflightError("SOURCE_DATA_UNAVAILABLE", "current-position copy source query: %v", err)
	}
	for _, p := range state.Positions {
		if p != nil && p.PosID == dec.LeaderPosID && p.OpenedMS == dec.SourceOpenedMS && p.MarginMode == task.MarginMode && strings.EqualFold(string(p.Side), strings.TrimPrefix(dec.Action, "open_")) && math.Abs(p.Size-dec.LeaderPosSize) <= math.Max(1e-12, p.Size*1e-8) {
			return nil
		}
	}
	return leaderCopyPreflightError("SOURCE_SUPERSEDED", "领航员仓位已变化，本次复制开仓失效")
}
