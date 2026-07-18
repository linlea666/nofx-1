package copytrade

import (
	"errors"
	"path/filepath"
	"testing"

	"nofx/store"
	"nofx/trader"
)

type replacementLifecycleStopManager struct {
	orders    map[string]*trader.ProtectiveStopOrder
	cancelErr error
	canceled  []string
}

func (m *replacementLifecycleStopManager) PlaceProtectiveStop(trader.ProtectiveStopRequest) (*trader.ProtectiveStopOrder, error) {
	return nil, errors.New("not used")
}
func (m *replacementLifecycleStopManager) AmendProtectiveStop(string, trader.ProtectiveStopRequest) error {
	return errors.New("not used")
}
func (m *replacementLifecycleStopManager) GetProtectiveStop(algoID, _ string) (*trader.ProtectiveStopOrder, error) {
	if order := m.orders[algoID]; order != nil {
		copy := *order
		return &copy, nil
	}
	return nil, trader.ErrProtectiveStopNotFound
}
func (m *replacementLifecycleStopManager) GetProtectiveStopByClientID(clientID, _ string) (*trader.ProtectiveStopOrder, error) {
	for _, order := range m.orders {
		if order.ClientID == clientID {
			copy := *order
			return &copy, nil
		}
	}
	return nil, trader.ErrProtectiveStopNotFound
}
func (m *replacementLifecycleStopManager) CancelProtectiveStop(algoID, _ string) error {
	m.canceled = append(m.canceled, algoID)
	if m.cancelErr != nil {
		return m.cancelErr
	}
	if order := m.orders[algoID]; order != nil {
		order.State = "canceled"
	}
	return nil
}

func TestRetryRetiringProtectiveStopCompletesOnlyAfterTerminationConfirmed(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "replacement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle := newTestCopyGuardCycle(t, st, "trader-replacement")
	pending := &store.CopyGuardProtectiveOrder{
		CycleID: cycle.ID, TraderID: "trader-replacement", AlgoID: "new", AlgoClientID: "new-client",
		PreviousAlgoID: "old", PreviousAlgoClientID: "old-client", ReplacementPending: true,
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 1, TriggerPrice: 95, Status: "live",
	}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(pending); err != nil {
		t.Fatal(err)
	}
	mgr := &replacementLifecycleStopManager{orders: map[string]*trader.ProtectiveStopOrder{
		"new": {AlgoID: "new", ClientID: "new-client", State: "live"},
		"old": {AlgoID: "old", ClientID: "old-client", State: "live"},
	}}
	ti := NewTraderIntegration("trader-replacement", flatExecutor{}, st)
	if !ti.retryRetiringProtectiveStop(mgr, cycle, pending) {
		t.Fatal("confirmed cancellation should complete replacement")
	}
	if len(mgr.canceled) != 1 || mgr.canceled[0] != "old" {
		t.Fatalf("must cancel only retiring order: %v", mgr.canceled)
	}
	stored, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil || stored.ReplacementPending || stored.PreviousAlgoID != "" || stored.AlgoID != "new" {
		t.Fatalf("replacement state not completed safely: stored=%+v err=%v", stored, err)
	}
}

func TestRetryRetiringProtectiveStopKeepsBothIDsOnCancelFailure(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "replacement-fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle := newTestCopyGuardCycle(t, st, "trader-replacement-fail")
	pending := &store.CopyGuardProtectiveOrder{
		CycleID: cycle.ID, TraderID: "trader-replacement-fail", AlgoID: "new", AlgoClientID: "new-client",
		PreviousAlgoID: "old", PreviousAlgoClientID: "old-client", ReplacementPending: true,
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 1, TriggerPrice: 95, Status: "live",
	}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(pending); err != nil {
		t.Fatal(err)
	}
	mgr := &replacementLifecycleStopManager{orders: map[string]*trader.ProtectiveStopOrder{
		"new": {AlgoID: "new", ClientID: "new-client", State: "live"},
		"old": {AlgoID: "old", ClientID: "old-client", State: "live"},
	}, cancelErr: errors.New("temporary cancel failure")}
	ti := NewTraderIntegration("trader-replacement-fail", flatExecutor{}, st)
	if ti.retryRetiringProtectiveStop(mgr, cycle, pending) {
		t.Fatal("failed cancellation must remain pending")
	}
	stored, err := st.CopyTrade().GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil || !stored.ReplacementPending || stored.PreviousAlgoID != "old" || stored.AlgoID != "new" {
		t.Fatalf("both recoverable ids must remain durable: stored=%+v err=%v", stored, err)
	}
}

func TestCancelTerminalReplacementStillCleansRetiringOrder(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "replacement-orphan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle := newTestCopyGuardCycle(t, st, "trader-orphan")
	order := &store.CopyGuardProtectiveOrder{
		CycleID: cycle.ID, TraderID: "trader-orphan", AlgoID: "new", AlgoClientID: "new-client",
		PreviousAlgoID: "old", PreviousAlgoClientID: "old-client", ReplacementPending: true,
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: "effective",
	}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(order); err != nil {
		t.Fatal(err)
	}
	mgr := &replacementLifecycleStopManager{orders: map[string]*trader.ProtectiveStopOrder{"old": {AlgoID: "old", State: "live"}}}
	ti := NewTraderIntegration("trader-orphan", flatExecutor{}, st)
	if err := ti.cancelProtectiveOrderForCycle(mgr, cycle, order); err != nil {
		t.Fatal(err)
	}
	if len(mgr.canceled) != 1 || mgr.canceled[0] != "old" {
		t.Fatalf("terminal current must not hide retiring orphan: %v", mgr.canceled)
	}
}
