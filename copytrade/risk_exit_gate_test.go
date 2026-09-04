package copytrade

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

type blockingRiskExitExecutor struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (e *blockingRiskExitExecutor) ExecuteDecision(*decision.Decision) error {
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.mu.Unlock()
	if call == 1 && e.entered != nil {
		close(e.entered)
		<-e.release
	}
	return nil
}

func (e *blockingRiskExitExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{"total_equity": 1000.0}, nil
}

func (e *blockingRiskExitExecutor) GetPositions() ([]map[string]interface{}, error) {
	return nil, nil
}

func (e *blockingRiskExitExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func seedRiskExitGateLifecycle(t *testing.T, st *store.Store, traderID string) *store.CopyGuardCycle {
	t.Helper()
	cs := st.CopyTrade()
	if err := cs.SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: traderID, LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", OpenPrice: 100,
		OpenSizeUSD: 100, LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	policy := store.NewCopyGuardDefaults()
	policy.TraderID, policy.ProviderType = traderID, "okx"
	policySnapshot, err := store.EncodeCopyGuardPolicySnapshot(policy)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := cs.EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: traderID, LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing,
		PolicySnapshot: policySnapshot, FollowerEntryPrice: 100, FollowerNotional: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.OpenCopyGuardAttempt(cycle.ID, 0, 100, 100, 1, 2); err != nil {
		t.Fatal(err)
	}
	return cycle
}

func TestRiskExitGateSerializesTriggerAgainstVenueSubmission(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "risk-exit-gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle := seedRiskExitGateLifecycle(t, st, "gate-trader")
	executor := &blockingRiskExitExecutor{entered: make(chan struct{}), release: make(chan struct{})}
	ti := NewTraderIntegration("gate-trader", executor, st)
	dec := &decision.Decision{LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Action: "open_long", CopyTradeAction: "add"}
	firstDone := make(chan error, 1)
	go func() { firstDone <- ti.executeDecisionUnderRiskExitGate(dec, dec) }()
	select {
	case <-executor.entered:
	case <-time.After(time.Second):
		t.Fatal("first venue call did not start")
	}
	begin := store.CopyGuardRiskExitBegin{
		CycleID: cycle.ID, TraderID: "gate-trader", LeaderPosID: "leader-pos", AttemptNo: 0,
		TriggerPrice: 92, Quantity: 1, TriggerSource: "test_trigger",
	}
	gateDone := make(chan error, 1)
	go func() { gateDone <- ti.establishRiskExitGate(begin) }()
	select {
	case err := <-gateDone:
		t.Fatalf("risk gate crossed an already-running venue call: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(executor.release)
	if err = <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err = <-gateDone; err != nil {
		t.Fatal(err)
	}
	if err = ti.executeDecisionUnderRiskExitGate(dec, dec); !errors.Is(err, errCopyGuardRiskExitGate) {
		t.Fatalf("post-trigger leader action reached venue: %v", err)
	}
	if executor.callCount() != 1 {
		t.Fatalf("post-trigger venue calls=%d want=1", executor.callCount())
	}
	mapping, err := st.CopyTrade().GetMapping("gate-trader", "leader-pos")
	if err != nil || mapping.Status != store.MappingStatusStoppedByRisk {
		t.Fatalf("risk gate was not persisted with mapping: mapping=%+v err=%v", mapping, err)
	}
}

func TestRiskExitGateFailsClosedWhenPersistenceIsUnavailable(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "risk-exit-gate-db-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	cycle := seedRiskExitGateLifecycle(t, st, "gate-failure-trader")
	executor := &blockingRiskExitExecutor{}
	ti := NewTraderIntegration("gate-failure-trader", executor, st)
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	err = ti.establishRiskExitGate(store.CopyGuardRiskExitBegin{
		CycleID: cycle.ID, TraderID: "gate-failure-trader", LeaderPosID: "leader-pos",
		AttemptNo: 0, TriggerPrice: 92, Quantity: 1, TriggerSource: "test_db_failure",
	})
	if err == nil {
		t.Fatal("closed database unexpectedly persisted risk gate")
	}
	dec := &decision.Decision{LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Action: "reduce_long", CopyTradeAction: "reduce"}
	if execErr := ti.executeDecisionUnderRiskExitGate(dec, dec); !errors.Is(execErr, errCopyGuardRiskExitGate) {
		t.Fatalf("database failure reopened leader execution: %v", execErr)
	}
	if executor.callCount() != 0 {
		t.Fatalf("database failure allowed %d venue calls", executor.callCount())
	}
	reentry := &decision.Decision{
		LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Action: "open_long",
		CopyTradeAction: "ai_reentry", Reasoning: "Copy Guard reentry",
	}
	if execErr := ti.executeDecisionUnderRiskExitGate(reentry, reentry); !errors.Is(execErr, errCopyGuardRiskExitGate) {
		t.Fatalf("database failure allowed a queued reentry through the in-memory gate: %v", execErr)
	}
	if executor.callCount() != 0 {
		t.Fatalf("database failure allowed %d venue calls after queued reentry", executor.callCount())
	}
}

func TestRiskExitGateAllowsOnlyDurablyPendingReentry(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "risk-exit-gate-reentry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cycle := seedRiskExitGateLifecycle(t, st, "gate-reentry-trader")
	executor := &blockingRiskExitExecutor{}
	ti := NewTraderIntegration("gate-reentry-trader", executor, st)
	if err = ti.establishRiskExitGate(store.CopyGuardRiskExitBegin{
		CycleID: cycle.ID, TraderID: "gate-reentry-trader", LeaderPosID: "leader-pos",
		AttemptNo: 0, TriggerPrice: 92, Quantity: 1, TriggerSource: "test_trigger",
	}); err != nil {
		t.Fatal(err)
	}
	reentry := &decision.Decision{
		LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Action: "open_long",
		CopyTradeAction: "ai_reentry", Reasoning: "Copy Guard reentry",
	}
	if err = ti.executeDecisionUnderRiskExitGate(reentry, reentry); !errors.Is(err, errCopyGuardRiskExitGate) {
		t.Fatalf("reentry escaped while the durable lifecycle was still exiting: %v", err)
	}
	if executor.callCount() != 0 {
		t.Fatalf("non-pending reentry reached venue %d times", executor.callCount())
	}
	if err = st.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardReentryPending, 100, 95, 1); err != nil {
		t.Fatal(err)
	}
	if ti.riskExitGateActive(reentry) {
		t.Fatal("durably pending reentry remained blocked by the old risk-exit gate")
	}
	if err = ti.executeDecisionUnderRiskExitGate(reentry, reentry); err != nil {
		t.Fatalf("durably pending reentry was not admitted: %v", err)
	}
	if executor.callCount() != 1 {
		t.Fatalf("durably pending reentry venue calls=%d want=1", executor.callCount())
	}
}

func TestFixedPositionMarginQueuedReentryIsRejectedBeforeVenueCall(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "fixed-reentry-runtime-gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	policy := store.NewCopyGuardDefaults()
	policy.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	policy.RiskReentryEnabled = false
	policy.RiskReentryDecisionMode = "disabled"
	policy.RiskMaxReentries = 0
	snapshot, err := store.EncodeCopyGuardPolicySnapshot(policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "fixed-trader", LeaderID: "leader", LeaderPosID: "fixed-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardStoppedWatching,
		PolicySnapshot: snapshot,
	}); err != nil {
		t.Fatal(err)
	}
	executor := &blockingRiskExitExecutor{}
	ti := NewTraderIntegration("fixed-trader", executor, st)
	ti.engine = &Engine{config: &CopyConfig{RiskPolicyVersion: 4}}
	dec := &decision.Decision{
		LeaderPosID: "fixed-pos", Symbol: "ETHUSDT", Action: "open_long",
		CopyTradeAction: "ai_reentry", Reasoning: "Copy Guard AI reentry",
	}
	if err = ti.rejectPositionMarginReentryBeforeOrder(dec); ReasonCodeOf(err) != "POSITION_MARGIN_REENTRY_BLOCKED" {
		t.Fatalf("fixed queued reentry was not blocked: code=%q err=%v", ReasonCodeOf(err), err)
	}
	if executor.callCount() != 0 {
		t.Fatalf("fixed queued reentry reached venue %d times", executor.callCount())
	}
}
