package copytrade

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"nofx/store"
)

const leaderAccountExitScope = "ACCOUNT_SYMBOL_SIDE"

func (e *Engine) sourceLifecycleChanged(posID string, pos *Position) bool {
	if pos == nil || pos.OpenedMS <= 0 || e.store == nil {
		return false
	}
	previous, err := e.store.CopyTrade().SourceLifecycleOpenedMS(e.traderID, posID)
	return err == nil && previous > 0 && pos.OpenedMS > previous
}

func (e *Engine) baselineRepairedSources(state *AccountState) error {
	if e.store == nil || state == nil {
		return fmt.Errorf("missing source snapshot")
	}
	if _, err := e.store.CopyTrade().RepairReleasedCloseBlockers(e.traderID); err != nil {
		return err
	}
	current := make(map[string]*store.CopyTradePositionMapping)
	opened := make(map[string]int64)
	for key, pos := range state.Positions {
		id := pos.PosID
		if id == "" {
			id = key
		}
		current[id] = &store.CopyTradePositionMapping{LeaderID: e.config.LeaderID, Symbol: pos.Symbol, Side: string(pos.Side), MarginMode: pos.MarginMode, LastKnownSize: pos.Size}
		opened[id] = pos.OpenedMS
	}
	if err := e.store.CopyTrade().BaselineRepairedSource(e.traderID, current, opened, e.config.FollowExitPolicyVersion >= 2); err != nil {
		return err
	}
	for id, ms := range opened {
		old, err := e.store.CopyTrade().SourceLifecycleOpenedMS(e.traderID, id)
		if err != nil {
			return err
		}
		if old == 0 {
			if err = e.store.CopyTrade().BindSourceLifecycle(e.traderID, id, ms); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) matchAccountLeaderReduction(signal *TradeSignal, positions map[string]*Position) *SignalMatchResult {
	fill := signal.Fill
	no := func(reason string) *SignalMatchResult { return &SignalMatchResult{Reason: reason} }
	mappings, err := e.store.CopyTrade().ListExitFollowingMappings(e.traderID)
	if err != nil {
		return no("源仓状态暂不可用")
	}
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].LeaderPosID < mappings[j].LeaderPosID })
	for _, m := range mappings {
		if (sourceMappingSymbol(m) != fill.Symbol && m.Symbol != fill.Symbol) || !strings.EqualFold(m.Side, string(fill.PositionSide)) || (fill.LeaderPosID != "" && fill.LeaderPosID != m.LeaderPosID) {
			continue
		}
		p := positions[m.LeaderPosID]
		closed := p == nil || !strings.EqualFold(string(p.Side), m.Side) || e.sourceLifecycleChanged(m.LeaderPosID, p)
		if !closed && p.Size >= m.LastKnownSize-binancePositionSizeEpsilon {
			continue
		}
		action := ActionReduce
		if closed {
			action = ActionClose
			p = nil
		}
		return &SignalMatchResult{ShouldFollow: true, Reason: fmt.Sprintf("领航员%s，同向总仓跟随(posId=%s)", action, m.LeaderPosID), Action: action, PosID: m.LeaderPosID, MarginMode: m.MarginMode, LeaderPosition: p, SourceSymbol: m.SourceSymbol, ExecutionSymbol: m.ExecutionSymbol, SourceQuoteAsset: m.SourceQuoteAsset, ExecutionSettleAsset: m.ExecutionSettleAsset, LeaderReversed: closed && positions[m.LeaderPosID] != nil && !strings.EqualFold(string(positions[m.LeaderPosID].Side), m.Side)}
	}
	return no("无新的领航员减仓变化，或该周期已暂停/整轮跳过")
}

// Source transitions for one symbol/side are reserved serially. Already
// acknowledged peers therefore supply the correct denominator, including a
// close of only one of several leader margin-mode positions.
func (e *Engine) accountLeaderReductionRatio(signal *TradeSignal, match *SignalMatchResult) float64 {
	m, err := e.store.CopyTrade().GetMapping(e.traderID, match.PosID)
	if err != nil || m == nil || m.LastKnownSize <= 0 {
		return 0
	}
	current := 0.0
	if match.LeaderPosition != nil {
		current = match.LeaderPosition.Size
	}
	delta := m.LastKnownSize - current
	if delta <= 0 {
		return 0
	}
	previous := m.LastKnownSize
	peers, err := e.store.CopyTrade().ListExitFollowingMappings(e.traderID)
	if err != nil {
		return 0
	}
	counted := map[string]bool{m.LeaderPosID: true}
	for _, p := range peers {
		if p.LeaderPosID != m.LeaderPosID && p.Symbol == m.Symbol && strings.EqualFold(p.Side, m.Side) {
			previous += p.LastKnownSize
			counted[p.LeaderPosID] = true
		}
	}
	// A still-present, ignored source leg is not permission to flatten all.
	for id, p := range e.buildLeaderPosMap() {
		if !counted[id] && p.Symbol == sourceMappingSymbol(m) && strings.EqualFold(string(p.Side), m.Side) {
			previous += p.Size
		}
	}
	return math.Min(1, delta/previous)
}

func sourceMappingSymbol(m *store.CopyTradePositionMapping) string {
	if m.SourceSymbol != "" {
		return m.SourceSymbol
	}
	return m.Symbol
}
