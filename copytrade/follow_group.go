package copytrade

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"nofx/decision"
	"nofx/store"
)

func (e *Engine) usesFollowGroups() bool {
	return e != nil && e.store != nil && e.config != nil && e.config.FollowExitPolicyVersion >= 2 && SupportsCopyGuard(e.config.ProviderType)
}

// Membership is installed from a COMPLETE snapshot before action generation.
// Source member changes never create a transient empty group between legs.
func (e *Engine) synchronizeFollowGroups(state *AccountState) error {
	if !e.usesFollowGroups() {
		return nil
	}
	cs := e.store.CopyTrade()
	mappings, err := cs.ListAllMappings(e.traderID, 0)
	if err != nil {
		return err
	}
	byID := map[string]*store.CopyTradePositionMapping{}
	for _, m := range mappings {
		byID[m.LeaderPosID] = m
		if m.Status == store.MappingStatusClosed {
			continue
		}
		opened, err := cs.SourceLifecycleOpenedMS(e.traderID, m.LeaderPosID)
		if err != nil {
			return err
		}
		if _, err = cs.EnsureFollowGroupMember(e.traderID, string(e.config.ProviderType), e.config.LeaderID, m.Symbol, m.Side, m.LeaderPosID, opened, m.Status); err != nil {
			return err
		}
	}
	var observations []store.FollowGroupObservation
	groups := map[int64]*store.FollowGroup{}
	keys := make([]string, 0, len(state.Positions))
	for k := range state.Positions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p := state.Positions[key]
		if p == nil || p.Size <= 0 {
			continue
		}
		id := p.PosID
		if id == "" {
			id = key
		}
		if e.sourceLifecycleStale(id, p) {
			_, _ = cs.RecordRuntimeIssue(store.CopyRuntimeIssue{TraderID: e.traderID, Area: "source", ResourceID: "source_epoch:" + id, LeaderPosID: id, Symbol: p.Symbol, Side: string(p.Side), Code: "SOURCE_OLDER_INCARNATION", Detail: "当前快照周期早于已确认源周期，等待新的完整源快照"})
		} else {
			_ = cs.ResolveRuntimeIssue(e.traderID, "source", "source_epoch:"+id)
		}
		symbol, status, opened := p.Symbol, "", p.OpenedMS
		if m := byID[id]; m != nil && m.Status != store.MappingStatusClosed {
			// The old member must acknowledge its end before attaching the
			// replacement identity, even when the venue reuses its position ID.
			if m.Side != string(p.Side) || e.sourceLifecycleChanged(id, p) {
				continue
			}
			symbol, status = m.Symbol, m.Status
		}
		g, err := cs.EnsureFollowGroupMember(e.traderID, string(e.config.ProviderType), e.config.LeaderID, symbol, string(p.Side), id, opened, status)
		if err != nil {
			return err
		}
		if g.ConflictReason != "" {
			_, _ = cs.RecordRuntimeIssue(store.CopyRuntimeIssue{TraderID: e.traderID, Area: "source", ResourceID: fmt.Sprintf("group:%d", g.ID), LeaderPosID: id, Symbol: g.Symbol, Side: g.Side, Code: g.ConflictReason, Detail: "历史同方向成员的跟随/暂停/整轮跳过状态不一致，需要核实本轮参与边界"})
		}
		groups[g.ID] = g
		observations = append(observations, store.FollowGroupObservation{GroupID: g.ID, LeaderPosID: id, MarginMode: p.MarginMode, Size: p.Size})
	}
	for groupID, g := range groups {
		ids, err := cs.FollowGroupMemberIDs(groupID)
		if err != nil {
			return err
		}
		oldCount, survivors := 0, 0
		for _, id := range ids {
			m := byID[id]
			if m == nil || m.Status == store.MappingStatusClosed {
				continue
			}
			oldCount++
			p := state.Positions[id]
			if p == nil {
				for _, candidate := range state.Positions {
					if candidate != nil && candidate.PosID == id {
						p = candidate
						break
					}
				}
			}
			if p != nil && p.Size > 0 && string(p.Side) == m.Side && !e.sourceLifecycleChanged(id, p) {
				survivors++
			}
		}
		if oldCount > 0 && survivors == 0 {
			if err = cs.BlockFollowGroupEntry(groupID, "SOURCE_GROUP_BOUNDARY_UNCONFIRMED"); err != nil {
				return err
			}
			_, _ = cs.RecordRuntimeIssue(store.CopyRuntimeIssue{TraderID: e.traderID, Area: "source", ResourceID: fmt.Sprintf("group_boundary:%d", groupID), Symbol: g.Symbol, Side: g.Side, Code: "SOURCE_GROUP_BOUNDARY_UNCONFIRMED", Detail: "完整快照中所有旧源成员已被新身份替换，无法证明期间是否空仓；本轮暂停新开加仓，原退出与保护继续"})
		}
	}
	return cs.PublishFollowGroupSnapshot(e.traderID, observations)
}

// followGroupReduction fixes one combined budget from the same source image.
// Only the canonical first reducing member reserves an execution intent.
func (e *Engine) followGroupReduction(posID string) (*store.FollowGroupExitBatch, error) {
	if !e.usesFollowGroups() {
		return nil, nil
	}
	cs := e.store.CopyTrade()
	g, err := cs.GetFollowGroupForPosition(e.traderID, posID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if g.ConflictReason != "" {
		return nil, fmt.Errorf("%s", g.ConflictReason)
	}
	if g.Paused || g.Ignored || g.SourceEnded {
		return nil, store.ErrFollowControlChanged
	}
	ids, err := cs.FollowGroupMemberIDs(g.ID)
	if err != nil {
		return nil, err
	}
	positions := e.buildLeaderPosMap()
	b := &store.FollowGroupExitBatch{GroupID: g.ID, FullGroupExit: true}
	previous, decrease := 0.0, 0.0
	for _, id := range ids {
		m, err := cs.GetMappingForReconciliation(e.traderID, id)
		if err != nil {
			return nil, err
		}
		p := positions[id]
		if e.sourceLifecycleStale(id, p) {
			return nil, fmt.Errorf("source member %s has an older incarnation than acknowledged", id)
		}
		// A newly joined child contributes to source continuity, but not to
		// the PREVIOUS quantity denominator of this snapshot's reductions.
		if p != nil && p.Size > 0 && strings.EqualFold(string(p.Side), g.Side) {
			if m == nil || m.Status == store.MappingStatusClosed || !e.sourceLifecycleChanged(id, p) {
				b.FullGroupExit = false
			}
		}
		if m == nil || m.Status == store.MappingStatusClosed || m.LastKnownSize <= 0 {
			continue
		}
		if m.Status == store.MappingStatusIgnored || m.Status == store.MappingStatusManualStopped {
			return nil, fmt.Errorf("follow group participation conflict for %s", id)
		}
		previous += m.LastKnownSize
		closed := p == nil || string(p.Side) != m.Side || e.sourceLifecycleChanged(id, p)
		target := 0.0
		if !closed {
			target = p.Size
		}
		if target < m.LastKnownSize-binancePositionSizeEpsilon {
			decrease += m.LastKnownSize - target
			b.Members = append(b.Members, store.FollowGroupExitMember{LeaderPosID: id, Revision: m.SourceRevision, TargetSize: target, SourceClosed: closed})
		}
	}
	if previous <= 0 || decrease <= 0 || len(b.Members) == 0 {
		return nil, nil
	}
	sort.Slice(b.Members, func(i, j int) bool { return b.Members[i].LeaderPosID < b.Members[j].LeaderPosID })
	b.MainPosID = b.Members[0].LeaderPosID
	b.Ratio = math.Min(1, decrease/previous)
	return b, nil
}

func (ti *TraderIntegration) queueFollowGroupProtections(groupID int64) error {
	cycles, err := ti.store.CopyTrade().FollowGroupGuardCycles(groupID)
	if err != nil {
		return err
	}
	for _, c := range cycles {
		if c.ClosedAt != nil {
			continue
		}
		ti.queueProtectionRefresh(&decision.Decision{IsCopyTrade: true, LeaderPosID: c.LeaderPosID, Symbol: c.Symbol, Action: "reduce_" + c.Side, CopyTradeAction: "reduce", MarginMode: c.MarginMode})
	}
	return nil
}

func (ti *TraderIntegration) verifyManagedFollowGroupPeer(dec *decision.Decision) (bool, error) {
	g, err := ti.store.CopyTrade().GetFollowGroupForPosition(ti.traderID, dec.LeaderPosID)
	if err != nil {
		return false, err
	}
	ids, err := ti.store.CopyTrade().FollowGroupMemberIDs(g.ID)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == dec.LeaderPosID {
			continue
		}
		p, err := ti.store.CopyTrade().GetPositionCustody(ti.traderID, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		if p.State != "MANAGED" {
			continue
		}
		m, err := ti.store.CopyTrade().GetMappingForReconciliation(ti.traderID, id)
		if err != nil {
			return false, err
		}
		if m == nil {
			continue
		}
		if _, err = ti.verifyFollowingPosition(m, p); err == nil {
			return true, nil
		} else if !errors.Is(err, errCopyPositionEnded) {
			return false, err
		}
	}
	return false, nil
}

func (ti *TraderIntegration) resumeFollowGroup(g *store.FollowGroup) error {
	if g.SourceEnded || g.Ignored || g.ConflictReason != "" || g.EntryBlockReason != "" {
		return fmt.Errorf("原方向组已结束或本轮跳过")
	}
	ids, err := ti.store.CopyTrade().FollowGroupMemberIDs(g.ID)
	if err != nil {
		return err
	}
	continuous := false
	for _, id := range ids {
		p, err := ti.store.CopyTrade().GetPositionCustody(ti.traderID, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if p.State != "MANAGED" {
			continue
		}
		m, err := ti.store.CopyTrade().GetMappingForReconciliation(ti.traderID, id)
		if err != nil {
			return err
		}
		if m == nil {
			continue
		}
		if _, err = ti.verifyFollowingPosition(m, p); err != nil {
			return err
		}
		continuous = true
	}
	if !continuous {
		return fmt.Errorf("原方向组跟随仓已结束，不能恢复后来手动仓")
	}
	if err = ti.engine.syncLeaderState(); err != nil {
		return err
	}
	current, err := ti.store.CopyTrade().GetFollowGroup(g.ID)
	if err != nil {
		return err
	}
	if current.SourceEnded {
		return fmt.Errorf("领航员原方向组已经结束")
	}
	// A fresh snapshot can add members while paused. Absorb their baselines too.
	ids, err = ti.store.CopyTrade().FollowGroupMemberIDs(g.ID)
	if err != nil {
		return err
	}
	positions := ti.engine.buildLeaderPosMap()
	targets := map[string]float64{}
	total := 0.0
	for _, id := range ids {
		targets[id] = 0
		p := positions[id]
		if p == nil || string(p.Side) != g.Side {
			continue
		}
		if ti.engine.sourceLifecycleChanged(id, p) {
			return fmt.Errorf("源成员周期身份已经变化")
		}
		targets[id] = p.Size
		total += p.Size
	}
	if total <= 0 {
		return fmt.Errorf("领航员原方向组已经结束")
	}
	return ti.store.CopyTrade().ResumeFollowGroup(ti.traderID, g.ID, g.Version, targets)
}

func (ti *TraderIntegration) retryCompletedLeaderExitProtections() {
	plans, err := ti.store.CopyTrade().CompletedLeaderExitsPendingProtection(ti.traderID)
	if err != nil {
		return
	}
	for _, plan := range plans {
		dec := &decision.Decision{IsCopyTrade: true, LeaderExitScope: leaderAccountExitScope, ExecutionIntentID: plan.IntentID, LeaderPosID: plan.LeaderPosID, Symbol: plan.Symbol, Action: "close_" + plan.Side, CopyTradeAction: "close"}
		_ = ti.finishLeaderExit(dec, plan)
	}
}
