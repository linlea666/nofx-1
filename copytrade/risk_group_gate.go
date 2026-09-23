package copytrade

import (
	"database/sql"
	"errors"
	"strings"

	"nofx/decision"
)

// Called with riskExitMu held. A trusted trigger blocks the whole participating
// direction before its database transaction succeeds. Group identity, rather
// than reusable source position IDs, permits a genuinely new round later.
func (ti *TraderIntegration) matchingRiskExitGateLocked(dec *decision.Decision) *riskExitGateState {
	if dec == nil {
		return nil
	}
	if ti.engine == nil || !ti.engine.usesFollowGroups() {
		return ti.riskExitGates[dec.LeaderPosID]
	}
	if !strings.HasPrefix(dec.Action, "open_") {
		return nil
	}
	if len(ti.riskExitGates) == 0 {
		return nil
	}
	current, currentErr := ti.store.CopyTrade().GetFollowGroupForPosition(ti.traderID, dec.LeaderPosID)
	for _, gate := range ti.riskExitGates {
		if gate == nil {
			continue
		}
		group, groupErr := ti.store.CopyTrade().GetFollowGroupForCycle(gate.begin.CycleID)
		if (currentErr != nil && !errors.Is(currentErr, sql.ErrNoRows)) || (groupErr != nil && !errors.Is(groupErr, sql.ErrNoRows)) {
			return gate // Once triggered, unavailable identity cannot authorize more risk.
		}
		if currentErr == nil && groupErr == nil {
			if current.ID == group.ID && !group.SourceEnded {
				return gate
			}
			continue
		}
		cycle, err := ti.store.CopyTrade().GetCopyGuardCycle(gate.begin.CycleID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return gate
		}
		if cycle.ClosedAt == nil && cycle.Symbol == dec.Symbol && strings.HasSuffix(dec.Action, cycle.Side) {
			return gate
		}
	}
	return nil
}
