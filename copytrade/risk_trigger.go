package copytrade

import (
	"nofx/logger"
	"nofx/store"
	"nofx/trader"
)

// Every authoritative hosted trigger crosses this boundary before enrichment.
// establishRiskExitGate latches in memory even if the transaction fails.
func (ti *TraderIntegration) observeTrustedProtectiveTrigger(cycle *store.CopyGuardCycle, order *store.CopyGuardProtectiveOrder, actual *trader.ProtectiveStopOrder) {
	if cycle == nil || order == nil || actual == nil || !isProtectiveStopFired(actual.State) {
		return
	}
	begin := store.CopyGuardRiskExitBegin{
		CycleID: cycle.ID, TraderID: ti.traderID, LeaderPosID: cycle.LeaderPosID, AttemptNo: cycle.ReentryCount,
		TriggerPrice: actual.TriggerPrice, Quantity: order.Quantity, AlgoID: actual.AlgoID, TriggerSource: "exchange_hosted",
		Metadata: map[string]interface{}{"state": actual.State, "actual_order_id": actual.ActualOrderID},
	}
	if begin.TriggerPrice <= 0 {
		begin.TriggerPrice = order.TriggerPrice
	}
	if err := ti.establishRiskExitGate(begin); err != nil {
		logger.Errorf("[CopyGuard] trusted trigger persistence pending cycle=%d: %v", cycle.ID, err)
	}
}
