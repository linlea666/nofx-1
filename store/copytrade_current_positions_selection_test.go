package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func currentCopySelect(t *testing.T, s *Store, c *CopyTradeConfig, filter bool, pct float64, ids ...string) {
	t.Helper()
	c.CopyCurrentPositionsSelection = nil
	for _, id := range ids {
		c.CopyCurrentPositionsSelection = append(c.CopyCurrentPositionsSelection, CurrentPositionCopySelection{LeaderPosID: id, OpenedMS: 1790000000000})
	}
	c.CopyCurrentPriceFilterEnabled, c.CopyCurrentMinAdvantagePct = &filter, &pct
	if err := s.CopyTrade().Update(c); err != nil {
		t.Fatal(err)
	}
}

func currentCopyTaskByID(t *testing.T, s *Store) map[string]CurrentPositionCopyTask {
	t.Helper()
	r, err := s.CopyTrade().CurrentPositionCopy("t")
	if err != nil || r == nil {
		t.Fatalf("request=%v %v", r, err)
	}
	out := map[string]CurrentPositionCopyTask{}
	for _, task := range r.Tasks {
		out[task.LeaderPosID] = task
	}
	return out
}

func TestCurrentPositionCopyOptionsArePersistedAndEditableWhilePending(t *testing.T) {
	s, c := currentCopyFixture(t)
	r, _ := s.CopyTrade().CurrentPositionCopy("t")
	if r.SelectionMode != CurrentCopySelectionAll || r.PriceFilterEnabled {
		t.Fatalf("legacy request must default to copy-all without filter: %+v", r)
	}
	currentCopySelect(t, s, c, true, 0.5, "p")
	currentCopySelect(t, s, c, true, 1.5, "p", "x")
	r, _ = s.CopyTrade().CurrentPositionCopy("t")
	if r.SelectionMode != CurrentCopySelectionSelected || !r.PriceFilterEnabled || r.MinAdvantagePct != 1.5 || len(r.Selection) != 2 {
		t.Fatalf("options not replaced: %+v", r)
	}
	// Omitted options from older clients leave the stored choice untouched.
	c.CopyCurrentPositionsSelection, c.CopyCurrentPriceFilterEnabled, c.CopyCurrentMinAdvantagePct = nil, nil, nil
	if err := s.CopyTrade().Update(c); err != nil {
		t.Fatal(err)
	}
	r, _ = s.CopyTrade().CurrentPositionCopy("t")
	if len(r.Selection) != 2 || r.MinAdvantagePct != 1.5 {
		t.Fatalf("omitted options overwrote the request: %+v", r)
	}
	for name, bad := range map[string]func(){
		"empty selection": func() { c.CopyCurrentPositionsSelection = []CurrentPositionCopySelection{} },
		"duplicate": func() {
			c.CopyCurrentPositionsSelection = []CurrentPositionCopySelection{{LeaderPosID: "p"}, {LeaderPosID: "p"}}
		},
		"pct too high": func() { v := 51.0; c.CopyCurrentMinAdvantagePct = &v },
		"pct too low":  func() { v := -21.0; c.CopyCurrentMinAdvantagePct = &v },
	} {
		c.CopyCurrentPositionsSelection, c.CopyCurrentMinAdvantagePct = nil, nil
		bad()
		if err := s.CopyTrade().Update(c); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestCurrentPositionCopyProviderScope(t *testing.T) {
	for provider, ok := range map[string]bool{"okx": true, "followcoin": true, "hyperliquid": false, "binance": false} {
		if CurrentPositionCopySupported(provider) != ok {
			t.Fatalf("%s supported=%v", provider, !ok)
		}
	}
	s, c := currentCopyFixture(t)
	c.ProviderType = "hyperliquid"
	c.CopyCurrentPositionsRequestID = uuid.NewString()
	if err := s.CopyTrade().Update(c); err == nil {
		t.Fatal("hyperliquid registration accepted")
	}
	c.ProviderType = "followcoin"
	c.LeaderID = "31535"
	if err := s.CopyTrade().Update(c); err != nil {
		t.Fatalf("followcoin registration rejected: %v", err)
	}
	currentCopyStart(t, s)
	// FollowCoin snapshots carry a platform time minutes in the past; sealing
	// uses the local observation time so the catch-up window is meaningful.
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "31535", 1, []CurrentPositionCopyTask{currentCopyCandidate("p")}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if tasks := currentCopyTaskByID(t, s); tasks["p"].Status != "READY" {
		t.Fatalf("followcoin task not sealed: %+v", tasks)
	}
}

func TestCurrentPositionCopySelectionSkipsAndIgnoresUnselected(t *testing.T) {
	s, c := currentCopyFixture(t)
	currentCopySelect(t, s, c, false, 0, "p")
	currentCopyStart(t, s)
	p, x, reopened := currentCopyCandidate("p"), currentCopyCandidate("x"), currentCopyCandidate("r")
	x.Symbol = "BTCUSDT"
	reopened.Symbol = "SOLUSDT"
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{p, x, reopened}, time.Now()); err != nil {
		t.Fatal(err)
	}
	tasks := currentCopyTaskByID(t, s)
	if tasks["p"].Status != "READY" || tasks["x"].Reason != "NOT_SELECTED" || tasks["r"].Reason != "NOT_SELECTED" {
		t.Fatalf("selection not applied: %+v", tasks)
	}
	// An unselected position with no mapping must not be followed later as a
	// fresh open; it is ignored but stays selectable for a future request.
	for _, candidate := range []CurrentPositionCopyTask{x, reopened} {
		id := candidate.LeaderPosID
		m, err := s.CopyTrade().GetMapping("t", id)
		if err != nil || m == nil || m.Status != MappingStatusIgnored {
			t.Fatalf("%s mapping=%+v %v", id, m, err)
		}
		reason, err := s.CopyTrade().CurrentCopyEligibility("t", "leader", candidate)
		if err != nil || reason != "" {
			t.Fatalf("%s not re-selectable: %q %v", id, reason, err)
		}
	}
	if m, _ := s.CopyTrade().GetMapping("t", "p"); m.Status != MappingStatusCopyPending {
		t.Fatalf("selected mapping=%+v", m)
	}
}

func TestCurrentPositionCopySelectionReopenedLifecycleIsNotSelected(t *testing.T) {
	s, c := currentCopyFixture(t)
	currentCopySelect(t, s, c, false, 0, "p")
	currentCopyStart(t, s)
	p := currentCopyCandidate("p")
	p.OpenedMS++
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{p}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if tasks := currentCopyTaskByID(t, s); tasks["p"].Reason != "NOT_SELECTED" {
		t.Fatalf("reopened position copied under an old choice: %+v", tasks)
	}
}

func TestCurrentPositionCopyPartialDirectionSelectionConflicts(t *testing.T) {
	s, c := currentCopyFixture(t)
	currentCopySelect(t, s, c, false, 0, "p")
	currentCopyStart(t, s)
	p, q := currentCopyCandidate("p"), currentCopyCandidate("q")
	q.MarginMode = "isolated"
	if err := s.CopyTrade().SealCurrentPositionCopy("t", "leader", 1, []CurrentPositionCopyTask{p, q}, time.Now()); err != nil {
		t.Fatal(err)
	}
	tasks := currentCopyTaskByID(t, s)
	if tasks["p"].Reason != "GROUP_PARTICIPATION_CONFLICT" || tasks["q"].Reason != "NOT_SELECTED" {
		t.Fatalf("half a direction group was copied: %+v", tasks)
	}
}

func TestCurrentPositionCopyNoFillStaysSelectableButOtherSkipsDoNot(t *testing.T) {
	s, _ := currentCopyFixture(t)
	for id, reason := range map[string]string{"p": "CURRENT_POSITION_COPY_NO_FILL", "q": "SOME_OTHER_SKIP"} {
		if _, err := s.DB().Exec(`INSERT INTO copy_trade_position_mappings(trader_id,leader_pos_id,leader_id,symbol,side,margin_mode,status,last_failure_reason,opened_at,updated_at)
 VALUES('t',?,'leader','ETHUSDT','long','cross','ignored',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, id, reason); err != nil {
			t.Fatal(err)
		}
	}
	if reason, err := s.CopyTrade().CurrentCopyEligibility("t", "leader", currentCopyCandidate("p")); err != nil || reason != "" {
		t.Fatalf("no-fill position blacklisted: %q %v", reason, err)
	}
	if reason, _ := s.CopyTrade().CurrentCopyEligibility("t", "leader", currentCopyCandidate("q")); reason != "SOURCE_PREVIOUSLY_SKIPPED" {
		t.Fatalf("other skip reasons must stay blocked: %q", reason)
	}
}
