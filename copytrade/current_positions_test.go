package copytrade

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"nofx/decision"
	"nofx/store"
	"nofx/trader"
)

func currentCopyEngine(t *testing.T, enabled bool) (*Engine, *store.Store, *okxPollTestProvider) {
	t.Helper()
	e, s := newTestCopyTradeEngine(t, ProviderOKX)
	e.config.SourceGeneration = 1
	e.config.FollowExitPolicyVersion = 2
	e.config.SyncMarginMode = true
	e.config.CopyCatchupWindowSeconds = 60
	if _, err := s.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,lifecycle_status,lifecycle_generation,is_running,decision_mode) VALUES(?,'u','copy','','account',100,'STOPPED',0,0,'copy_trade')`, e.traderID); err != nil {
		t.Fatal(err)
	}
	cfg := &store.CopyTradeConfig{TraderID: e.traderID, ProviderType: "okx", LeaderID: "leader", CopyRatio: 1, SourceGeneration: 1}
	if enabled {
		cfg.CopyCurrentPositionsOnce = &enabled
		cfg.CopyCurrentPositionsRequestID = uuid.NewString()
	}
	if err := s.CopyTrade().Create(cfg); err != nil {
		t.Fatal(err)
	}
	start, err := s.Trader().BeginStart("u", e.traderID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Trader().CompleteCopyGuardStart("u", e.traderID, start.Generation, "account"); err != nil {
		t.Fatal(err)
	}
	state := &AccountState{TotalEquity: 1000, Timestamp: time.Now(), Positions: map[string]*Position{}}
	for n, mode := range []string{"cross", "isolated"} {
		id := fmt.Sprintf("p%d", n)
		state.Positions[id] = &Position{PosID: id, Symbol: "ETHUSDT", Side: SideLong, MarginMode: mode, Size: 10, PositionValue: 1000, EntryPrice: 110, MarkPrice: 100, Leverage: 10, OpenedMS: 1790000000000}
	}
	provider := &okxPollTestProvider{&binancePollTestProvider{state: state}}
	e.provider = provider
	e.currentCopyPreview = func(state *AccountState) ([]store.CurrentPositionCopyTask, error) {
		return BuildCurrentPositionPreview(s, e.traderID, "leader", 1, state, 100, nil, func(string) (float64, error) { return 100, nil }, &positionMarginLifecycleExecutor{})
	}
	return e, s, provider
}
func copyDecision(t *testing.T, e *Engine, fill Fill) *decision.Decision {
	t.Helper()
	signal := e.buildSignal(&fill)
	signal.AuthoritativeSnapshot = true
	e.processSignal(signal)
	select {
	case d := <-e.decisionCh:
		return &d.Decisions[0]
	default:
		t.Fatalf("no decision for %+v", fill)
		return nil
	}
}
func commitCurrentCopyFill(t *testing.T, e *Engine, d *decision.Decision) {
	t.Helper()
	if _, err := e.store.CopyTrade().PrepareExecutionOrderAttempt(d.ExecutionIntentID, d.ClientOrderID, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CopyTrade().CommitLeaderExecutionFill(store.LeaderExecutionCommit{IntentID: d.ExecutionIntentID, TraderID: e.traderID, LeaderID: "leader", LeaderPosID: d.LeaderPosID, SourceRevision: d.SourceRevision, Action: d.Action, Symbol: d.Symbol, Side: "long", MarginMode: d.MarginMode, LeaderTargetSize: d.LeaderPosSize, FillPrice: 100, FilledQuantity: 1, FilledNotional: 100, ClientOrderID: d.ClientOrderID, ExchangeOrderID: "venue-" + d.LeaderPosID, ExchangeState: "FILLED", OrderTerminal: true}); err != nil {
		t.Fatal(err)
	}
}
func TestCurrentPositionCopySurvivesAllBaselinesAndMatchesExactSource(t *testing.T) {
	e, s, _ := currentCopyEngine(t, true)
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 2 {
		t.Fatalf("baselines swallowed copy: %+v", fills)
	}
	first := copyDecision(t, e, fills[0])
	if first.LeaderPosID != fills[0].LeaderPosID || first.CurrentPositionTaskID == 0 || math.Abs(first.PositionSizeUSD-100) > 1e-9 {
		t.Fatalf("source identity/native contracts sizing wrong: %+v", first)
	}
	// No mapping/custody is acknowledged until the normal fill transaction.
	custody, _ := s.CopyTrade().GetPositionCustody(e.traderID, first.LeaderPosID)
	if custody != nil {
		t.Fatal("reservation fabricated custody")
	}
	commitCurrentCopyFill(t, e, first)
	second := copyDecision(t, e, fills[1])
	if second.LeaderPosID == first.LeaderPosID || second.MarginMode == first.MarginMode {
		t.Fatalf("same-direction source identity collapsed: first=%+v second=%+v", first, second)
	}
	commitCurrentCopyFill(t, e, second)
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	if got := e.detectBinancePositionSnapshotFills(); len(got) != 0 {
		t.Fatalf("duplicate import: %+v", got)
	}
	r, _ := s.CopyTrade().CurrentPositionCopy(e.traderID)
	if r.Status != "DONE" {
		t.Fatalf("batch not finished: %+v", r)
	}
}
func TestCurrentPositionCopyDefaultOffAndSourceChangeDoNotChase(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			e, s, p := currentCopyEngine(t, enabled)
			if err := e.InitIgnoredPositions(); err != nil {
				t.Fatal(err)
			}
			if err := e.syncLeaderState(); err != nil {
				t.Fatal(err)
			}
			if !enabled {
				if fills := e.detectBinancePositionSnapshotFills(); len(fills) != 0 {
					t.Fatal("default chased history")
				}
				return
			}
			delete(p.state.Positions, "p0")
			p.state.Positions["p1"].OpenedMS++
			if err := e.syncLeaderState(); err != nil {
				t.Fatal(err)
			}
			r, _ := s.CopyTrade().CurrentPositionCopy(e.traderID)
			for _, task := range r.Tasks {
				if task.IntentID != 0 || task.Status != "SKIPPED" {
					t.Fatalf("old source task survived: %+v", task)
				}
			}
		})
	}
}
func TestCurrentPositionCopyRestartKeepsOneIntentAndPreservesPendingSourceRevision(t *testing.T) {
	e, s, _ := currentCopyEngine(t, true)
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	d := copyDecision(t, e, fills[0])
	// Crash after reservation before adapter invocation: recovery requests source
	// revalidation, then the same task/canonical order is reclaimed.
	if err := s.CopyTrade().DeferUnsubmittedSourceReservation(d.ExecutionIntentID, e.traderID, "simulated crash before send"); err != nil {
		t.Fatal(err)
	}
	e.seenFills = map[string]time.Time{}
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	replay := copyDecision(t, e, fills[0])
	if replay.ExecutionIntentID != d.ExecutionIntentID || replay.ClientOrderID != d.ClientOrderID {
		t.Fatalf("restarted new order: %v %v", d, replay)
	}
	// Stop discards every unsent part and cannot reopen it on another generation.
	if _, err := s.Trader().BeginStop("u", e.traderID); err != nil {
		t.Fatal(err)
	}
	task, err := s.CopyTrade().CurrentPositionCopyTask(e.traderID, d.LeaderPosID)
	if err != nil || task != nil {
		t.Fatalf("stopped authorization still executable: %+v %v", task, err)
	}
}
func TestCurrentPositionCopyUsesOrdinaryAddsAndGroupedReductions(t *testing.T) {
	e, _, p := currentCopyEngine(t, true)
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	for _, f := range e.detectBinancePositionSnapshotFills() {
		commitCurrentCopyFill(t, e, copyDecision(t, e, f))
	}
	p.state.Positions["p0"].Size = 5
	p.state.Positions["p0"].PositionValue = 500
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].Action != ActionReduce {
		t.Fatalf("ordinary reduction missing: %+v", fills)
	}
	d := copyDecision(t, e, fills[0])
	if math.Abs(d.CloseRatio-.25) > 1e-9 || d.LeaderExitScope != leaderAccountExitScope || d.CurrentPositionTaskID != 0 {
		t.Fatalf("incorrect group denominator/exit path: %+v", d)
	}
}
func TestCurrentPositionPreviewNeverClaimsIndependentPositionsOrRiskExit(t *testing.T) {
	e, s, p := currentCopyEngine(t, false)
	rows, err := BuildCurrentPositionPreview(s, e.traderID, "leader", 1, p.state, 100, []map[string]interface{}{{"symbol": "ETHUSDT", "side": "long", "positionAmt": 1.0}}, func(string) (float64, error) { return 120, nil }, &positionMarginLifecycleExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Reason != "INDEPENDENT_SAME_SIDE_POSITION" {
			t.Fatalf("manual position was imported: %+v", row)
		}
	}
	if err = s.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{TraderID: e.traderID, LeaderID: "leader", LeaderPosID: "p0", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`UPDATE copy_trade_position_mappings SET status='stopped_by_risk'`); err != nil {
		t.Fatal(err)
	}
	rows, err = BuildCurrentPositionPreview(s, e.traderID, "leader", 1, p.state, 100, nil, func(string) (float64, error) { return 120, nil }, &positionMarginLifecycleExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != "SKIPPED" || rows[1].Status != "READY" {
		t.Fatalf("eligibility/price gate incorrect: %+v", rows)
	}
}

func TestCurrentPositionCopySubmissionBoundaryRevalidatesSourceAndExpiry(t *testing.T) {
	for _, change := range []string{"unchanged", "close", "reverse", "cycle", "size", "expired", "generation", "source-error"} {
		t.Run(change, func(t *testing.T) {
			e, s, p := currentCopyEngine(t, true)
			e.config.SyncMarginMode = false
			if err := e.InitIgnoredPositions(); err != nil {
				t.Fatal(err)
			}
			if err := e.syncLeaderState(); err != nil {
				t.Fatal(err)
			}
			d := copyDecision(t, e, e.detectBinancePositionSnapshotFills()[0])
			// The follower's margin mode may differ from the source authorization.
			d.MarginMode = "cross"
			ti := &TraderIntegration{traderID: e.traderID, store: s, engine: e}
			if err := ti.validateCurrentPositionCopy(d); err != nil {
				t.Fatal(err)
			}
			ti.bindExecutionAttemptRecorder(d)
			if err := d.BeforeOrderSubmit(d.ClientOrderID, 1); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "close":
				delete(p.state.Positions, d.LeaderPosID)
			case "reverse":
				p.state.Positions[d.LeaderPosID].Side = SideShort
			case "cycle":
				p.state.Positions[d.LeaderPosID].OpenedMS++
			case "size":
				p.state.Positions[d.LeaderPosID].Size = 5
			case "expired":
				if _, err := s.DB().Exec(`UPDATE copy_trade_current_position_requests SET snapshot_at=?`, time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
					t.Fatal(err)
				}
			case "generation":
				if _, err := s.Trader().BeginStop("u", e.traderID); err != nil {
					t.Fatal(err)
				}
			case "source-error":
				e.provider = &failedCurrentCopyProvider{LeaderProvider: p}
			}
			err := d.BeforeExchangeSubmit(d.ClientOrderID)
			if change == "unchanged" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid source crossed submission boundary")
			}
			attempts, err := s.CopyTrade().ListExecutionOrderAttempts(d.ExecutionIntentID)
			if err != nil || len(attempts) != 1 || attempts[0].SubmittedAt != nil {
				t.Fatalf("invalid copy submitted: %+v %v", attempts, err)
			}
		})
	}
}

type failedCurrentCopyProvider struct{ LeaderProvider }

func (p *failedCurrentCopyProvider) GetAccountState(string) (*AccountState, error) {
	return nil, fmt.Errorf("source timeout")
}

func TestCurrentPositionCopyIncompleteStartupKeepsRequestAndExpirationReleasesRevision(t *testing.T) {
	e, s, p := currentCopyEngine(t, true)
	original := e.currentCopyPreview
	e.currentCopyPreview = func(*AccountState) ([]store.CurrentPositionCopyTask, error) {
		return nil, fmt.Errorf("incomplete follower snapshot")
	}
	if err := e.Start(context.Background()); err == nil {
		t.Fatal("started with incomplete copy snapshot")
	}
	r, _ := s.CopyTrade().CurrentPositionCopy(e.traderID)
	if r.Status != "PENDING" || e.running {
		t.Fatalf("failed startup consumed authorization: %+v", r)
	}
	// Both HTTP start endpoints roll back the already committed RUNNING state.
	// Exercise that path too, then retry through a new manual-start generation.
	runtime, err := s.Trader().GetLifecycle(e.traderID)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := s.Trader().BeginFailedStartStop("u", e.traderID, runtime.Generation, "incomplete follower snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Trader().CompleteStop("u", e.traderID, stop.Generation); err != nil {
		t.Fatal(err)
	}
	start, err := s.Trader().BeginStart("u", e.traderID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Trader().CompleteCopyGuardStart("u", e.traderID, start.Generation, "account"); err != nil {
		t.Fatal(err)
	}
	e.currentCopyPreview = original
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	d := copyDecision(t, e, e.detectBinancePositionSnapshotFills()[0])
	if err := s.CopyTrade().UpdateExecutionIntent(d.ExecutionIntentID, store.ExecutionIntentFailed, "PRE_SUBMIT", "local error", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE copy_trade_current_position_requests SET snapshot_at=?`, time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	p.state.Timestamp = time.Now()
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	r, _ = s.CopyTrade().CurrentPositionCopy(e.traderID)
	if r.Status != "DONE" {
		t.Fatalf("expired failed pre-submit reservation stuck: %+v", r)
	}
	facts, err := s.CopyTrade().InspectLeaderTransition(d.ExecutionIntentID)
	if err != nil || facts.MappingRevision < facts.Revision || facts.Resolution != store.SourceSupersedeNoFill {
		t.Fatalf("source revision stranded: %+v %v", facts, err)
	}
}

func TestCurrentPositionCopyPartialGroupUsesUnfilledMemberForExitOnly(t *testing.T) {
	e, s, p := currentCopyEngine(t, true)
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	first := copyDecision(t, e, fills[0])
	commitCurrentCopyFill(t, e, first)
	second := copyDecision(t, e, fills[1])
	if err := s.CopyTrade().CommitIgnoredLeaderTransition(store.IgnoredLeaderTransition{IntentID: second.ExecutionIntentID, TraderID: e.traderID, LeaderID: "leader", LeaderPosID: second.LeaderPosID, SourceRevision: second.SourceRevision, Symbol: second.Symbol, Side: "long", MarginMode: second.MarginMode, LeaderTargetSize: second.LeaderPosSize, ReasonCode: "MIN_NOTIONAL"}); err != nil {
		t.Fatal(err)
	}
	p.state.Positions[second.LeaderPosID].Size = 5
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills = e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].Action != ActionReduce {
		t.Fatalf("unfilled group leg lost exit: %+v", fills)
	}
	d := copyDecision(t, e, fills[0])
	if math.Abs(d.CloseRatio-.25) > 1e-9 {
		t.Fatalf("wrong mixed group ratio: %+v", d)
	}
}

func TestCurrentPositionCopyManagedMergeNeverAdoptsAnotherMarginScope(t *testing.T) {
	for _, manualIsolated := range []bool{false, true} {
		t.Run(fmt.Sprint(manualIsolated), func(t *testing.T) {
			e, s, _ := currentCopyEngine(t, true)
			e.config.SyncMarginMode = false
			if err := e.InitIgnoredPositions(); err != nil {
				t.Fatal(err)
			}
			if err := e.syncLeaderState(); err != nil {
				t.Fatal(err)
			}
			fills := e.detectBinancePositionSnapshotFills()
			first := copyDecision(t, e, fills[0])
			first.MarginMode = "cross"
			commitCurrentCopyFill(t, e, first)
			second := copyDecision(t, e, fills[1])
			second.MarginMode = "cross"
			ex := &continuityExecutor{positionMarginLifecycleExecutor: &positionMarginLifecycleExecutor{}, fills: []trader.TradeRecord{continuityFill("entry", "venue-"+first.LeaderPosID, "BUY", 1, time.Now())}}
			ex.setPosition(100, 1, 10, 100, 80)
			if manualIsolated {
				ex.positions = append(ex.positions, map[string]interface{}{"symbol": "ETHUSDT", "side": "long", "marginMode": "isolated", "positionAmt": 1.0})
			}
			ti := NewTraderIntegration(e.traderID, ex, s)
			ti.engine = e
			err := ti.preflightCopyPositionOwnership(second)
			if manualIsolated {
				if ReasonCodeOf(err) != "INDEPENDENT_POSITION_CONFLICT" {
					t.Fatalf("manual scope accepted: %v", err)
				}
				group, readErr := s.CopyTrade().GetFollowGroupForPosition(e.traderID, first.LeaderPosID)
				if readErr != nil || group.Ignored {
					t.Fatalf("existing managed peer was disabled: %+v %v", group, readErr)
				}
			} else if err != nil || !second.AllowManagedPositionMerge {
				t.Fatalf("managed peer not reused: %v %+v", err, second)
			}
		})
	}
}

func TestCurrentPositionCopyStoppedBaselineCanBeExplicitlyAuthorized(t *testing.T) {
	e, s, p := currentCopyEngine(t, false)
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	stop, err := s.Trader().BeginStop("u", e.traderID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Trader().CompleteStop("u", e.traderID, stop.Generation); err != nil {
		t.Fatal(err)
	}
	p.state.Positions["new-while-stopped"] = &Position{PosID: "new-while-stopped", Symbol: "BTCUSDT", Side: SideShort, MarginMode: "cross", Size: 2, PositionValue: 200, EntryPrice: 90, MarkPrice: 100, Leverage: 5, OpenedMS: 1790000001000}
	c, err := s.CopyTrade().GetByTraderID(e.traderID)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	c.CopyCurrentPositionsOnce = &on
	c.CopyCurrentPositionsRequestID = uuid.NewString()
	if err = s.CopyTrade().Update(c); err != nil {
		t.Fatal(err)
	}
	start, err := s.Trader().BeginStart("u", e.traderID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Trader().CompleteCopyGuardStart("u", e.traderID, start.Generation, "account"); err != nil {
		t.Fatal(err)
	}
	if err = e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 3 {
		t.Fatalf("stopped baseline swallowed explicit authorization: %+v", fills)
	}
	for _, f := range fills {
		if f.LeaderPosID == "new-while-stopped" {
			d := copyDecision(t, e, f)
			if d.Action != "open_short" || math.Abs(d.PositionSizeUSD-20) > 1e-8 || d.CurrentPositionTaskID == 0 {
				t.Fatalf("short copy does not use ordinary sizing: %+v", d)
			}
		}
	}
}

func TestCurrentPositionCopyLaterAddsRemainOrdinary(t *testing.T) {
	e, _, p := currentCopyEngine(t, true)
	delete(p.state.Positions, "p1")
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	d := copyDecision(t, e, e.detectBinancePositionSnapshotFills()[0])
	commitCurrentCopyFill(t, e, d)
	p.state.Positions["p0"].Size = 15
	p.state.Positions["p0"].PositionValue = 1500
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].Action != ActionAdd {
		t.Fatalf("missing ordinary add: %+v", fills)
	}
	add := copyDecision(t, e, fills[0])
	if add.CurrentPositionTaskID != 0 || add.CopyTradeAction != "add" || math.Abs(add.PositionSizeUSD-50) > 1e-9 {
		t.Fatalf("copied lifecycle changed ordinary add: %+v", add)
	}
}

func TestCurrentPositionCopyHealthySizeChangeRetargetsOnlyProvenUnsentIntent(t *testing.T) {
	for _, submitted := range []bool{false, true} {
		t.Run(fmt.Sprint(submitted), func(t *testing.T) {
			e, s, p := currentCopyEngine(t, true)
			delete(p.state.Positions, "p1")
			if err := e.InitIgnoredPositions(); err != nil {
				t.Fatal(err)
			}
			if err := e.syncLeaderState(); err != nil {
				t.Fatal(err)
			}
			first := copyDecision(t, e, e.detectBinancePositionSnapshotFills()[0])
			if submitted {
				if _, err := s.CopyTrade().PrepareExecutionOrderAttempt(first.ExecutionIntentID, first.ClientOrderID, 1); err != nil {
					t.Fatal(err)
				}
			}
			p.state.Positions["p0"].Size = 5
			p.state.Positions["p0"].PositionValue = 500
			if err := e.syncLeaderState(); err != nil {
				t.Fatal(err)
			}
			r, _ := s.CopyTrade().CurrentPositionCopy(e.traderID)
			if len(r.Tasks) != 1 || r.Status != "SEALED" {
				t.Fatalf("target change rescanned batch: %+v", r)
			}
			if submitted {
				if r.Tasks[0].IntentID != first.ExecutionIntentID {
					t.Fatal("unknown order replaced")
				}
				return
			}
			next := copyDecision(t, e, e.detectBinancePositionSnapshotFills()[0])
			if next.CurrentPositionTaskID != first.CurrentPositionTaskID || next.ExecutionIntentID == first.ExecutionIntentID || next.SourceRevision <= first.SourceRevision || math.Abs(next.PositionSizeUSD-50) > 1e-8 {
				t.Fatalf("target did not follow remaining size: before=%+v after=%+v", first, next)
			}
			if err := s.CopyTrade().CheckFollowSubmission(first.ExecutionIntentID); err == nil {
				t.Fatal("superseded queued intent can still submit")
			}
		})
	}
}
