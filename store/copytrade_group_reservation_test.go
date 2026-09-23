package store

import (
	"errors"
	"testing"
)

func TestDeferSourceReservationNeverOverwritesExchangeEvidence(t *testing.T) {
	st := resolutionStore(t)
	i := resolutionIntent(t, st, "t", "p", "open_long", ExecutionIntentReserved, 1, 10)
	resolutionExec(t, st, `UPDATE copy_trade_execution_intents SET submitted_at=CURRENT_TIMESTAMP,exchange_order_id='unknown-ack' WHERE id=?`, i.ID)
	if err := st.CopyTrade().DeferUnsubmittedSourceReservation(i.ID, "t", "local binding failed"); !errors.Is(err, ErrSourceTransitionUnresolved) {
		t.Fatalf("unresolved exchange evidence overwritten: %v", err)
	}
	got, err := st.CopyTrade().GetExecutionIntentByID(i.ID)
	if err != nil || got.Status != ExecutionIntentReserved || got.ExchangeOrderID != "unknown-ack" {
		t.Fatalf("exchange identity changed: %+v %v", got, err)
	}
}
