package copytrade

import (
	"fmt"
	"strings"
	"time"

	"nofx/notifier"
	"nofx/store"
)

func protectionIncidentKind(kind string) (bool, int, bool) {
	switch kind {
	case "unprotected_warning", "rearm_throttled":
		return true, 1, false
	case "missing_escalation":
		return true, 2, false
	case "recovered":
		return true, 0, true
	}
	return false, 0, false
}

// Preserve the existing notifier, recipients and email-level policy. Only the
// repeated protection-failure family gets a shared durable incident identity.
func (ti *TraderIntegration) attachProtectionIncident(a *notifier.Alert, cycle *store.CopyGuardCycle, kind string) bool {
	managed, level, recovery := protectionIncidentKind(kind)
	if !managed {
		// A mark-price outage matters only while this cycle still protects an
		// exposure. Keep accounting/exit-receipt notices independent: those can
		// remain important after flatness or after the lifecycle has closed.
		if strings.HasPrefix(kind, "position_margin_mark_unavailable_") {
			a.BeforeSend = func() bool {
				if ti.store == nil {
					return false
				}
				current, err := ti.store.CopyTrade().GetCopyGuardCycle(cycle.ID)
				return err == nil && current.ClosedAt == nil && current.ReentryCount == cycle.ReentryCount &&
					(current.Status == store.CopyGuardFollowing || current.Status == store.CopyGuardFollowingReentry) &&
					current.ProtectionStatus != store.CopyGuardProtectionFlatReconciling &&
					current.ProtectionStatus != store.CopyGuardProtectionPositionAbsent && ti.copyGuardOwnsPosition(current)
			}
			return a.BeforeSend()
		}
		return true
	}
	if ti.store == nil {
		return false
	}
	if !recovery && ti.confirmCopyGuardFollowerAbsent(cycle) {
		return false
	}
	claim, err := ti.store.CopyTrade().ClaimProtectionMail(cycle.ID, cycle.ReentryCount, level, recovery, time.Now())
	if err != nil || claim == nil {
		return false
	}
	a.RateKey = fmt.Sprintf("guard-incident|%d|%t", claim.ID, recovery)
	a.DedupKey = fmt.Sprintf("%s|%d", a.RateKey, claim.Sequence)
	a.BeforeSend = func() bool { return ti.store.CopyTrade().ProtectionMailStillRelevant(claim, time.Now()) }
	a.StatusHook = func(status notifier.DeliveryStatus, err error) {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		_ = ti.store.CopyTrade().RecordProtectionMailDelivery(claim, string(status), detail, time.Now())
	}
	return true
}
