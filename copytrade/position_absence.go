package copytrade

import (
	"database/sql"
	"errors"
	"nofx/store"
	"time"
)

// A successful fresh read is required; an empty cached/error result cannot
// detach a lifecycle. Wait for the original/in-flight fill to settle first.
func (ti *TraderIntegration) confirmCopyGuardFollowerAbsent(cycle *store.CopyGuardCycle) bool {
	if cycle == nil || cycle.ProtectionStatus == store.CopyGuardProtectionForcedExitPending || time.Since(cycle.OpenedAt) < 30*time.Second {
		return false
	}
	expected, err := ti.store.CopyTrade().GetCopyGuardPositionOwnershipExpectation(cycle.ID)
	if err != nil || expected.InFlight || (!expected.LastIntentUpdated.IsZero() && time.Since(expected.LastIntentUpdated) < 15*time.Second) {
		return false
	}
	quantity, known := ti.followerPositionQuantity(cycle.Symbol, cycle.Side, cycle.MarginMode, cycle.FollowerPosID, true)
	if !known || quantity > 1e-12 {
		return false
	}
	if order, err := ti.store.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return false
		}
	} else if isProtectiveStopFired(order.Status) {
		return false
	}
	if err = ti.store.CopyTrade().MarkCopyGuardFollowerAbsent(cycle.ID, ti.traderID, cycle.LeaderPosID); err != nil {
		return false
	}
	return true
}
