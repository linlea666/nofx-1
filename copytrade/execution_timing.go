package copytrade

import (
	"nofx/decision"
	"strings"
)

func (ti *TraderIntegration) observeExecutionFillTime(intentID int64, order map[string]interface{}) {
	if ti.store == nil || intentID <= 0 || !strings.EqualFold(getStringField(order, "status", "state"), "FILLED") || getFloatField(order, "executedQty", "filled_quantity") <= 0 {
		return
	}
	// For a terminal filled order, venue updateTime is the final fill update;
	// canceled partial orders need their immutable fill history instead.
	ms := int64(getFloatField(order, "fillTime", "fill_time", "updateTime"))
	if ms > 0 {
		_ = ti.store.CopyTrade().RecordExchangeFillTime(intentID, ms)
	}
}

func (ti *TraderIntegration) executionEquityEvidence(dec *decision.Decision) float64 {
	if dec.CopyFollowerEquity > 0 {
		return dec.CopyFollowerEquity
	}
	if intent, err := ti.store.CopyTrade().GetExecutionIntentByID(dec.ExecutionIntentID); err == nil && intent.FollowerEquityAtTarget > 0 {
		return intent.FollowerEquityAtTarget
	}
	// Missing startup evidence is filled asynchronously by protection maintenance.
	return 0
}
