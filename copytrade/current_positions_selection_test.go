package copytrade

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"nofx/store"
)

func currentCopyPriceRule(minPct float64, ids ...string) func(*store.CopyTradeConfig) {
	return func(cfg *store.CopyTradeConfig) {
		on := true
		cfg.CopyCurrentPriceFilterEnabled, cfg.CopyCurrentMinAdvantagePct = &on, &minPct
		for _, id := range ids {
			cfg.CopyCurrentPositionsSelection = append(cfg.CopyCurrentPositionsSelection, store.CurrentPositionCopySelection{LeaderPosID: id, OpenedMS: 1790000000000})
		}
	}
}

func TestSupportedCurrentCopyProvidersMatchStore(t *testing.T) {
	for _, p := range []ProviderType{ProviderOKX, ProviderFollowCoin, ProviderHyperliquid, ProviderBinance} {
		if supportsCurrentPositionCopy(p) != store.CurrentPositionCopySupported(string(p)) {
			t.Fatalf("engine and store disagree on %s", p)
		}
	}
	for p, native := range map[ProviderType]bool{ProviderOKX: true, ProviderFollowCoin: true, ProviderHyperliquid: false, ProviderBinance: false} {
		if nativeSourceLifecycle(p) != native || !supportsCurrentPositionCopy(p) {
			t.Fatalf("%s native lifecycle=%v supported=%v", p, nativeSourceLifecycle(p), supportsCurrentPositionCopy(p))
		}
	}
}

func TestCurrentCopyAdvantageDirection(t *testing.T) {
	cases := []struct {
		side         string
		entry, price float64
		want         float64
	}{{"long", 100, 95, 5}, {"long", 100, 105, -5}, {"short", 100, 105, 5}, {"short", 100, 95, -5}}
	for _, c := range cases {
		got, ok := currentCopyAdvantagePct(c.side, c.entry, c.price)
		if !ok || got < c.want-1e-9 || got > c.want+1e-9 {
			t.Fatalf("%+v got %v %v", c, got, ok)
		}
	}
	if _, ok := currentCopyAdvantagePct("long", 0, 100); ok {
		t.Fatal("missing leader entry price must not count as an advantage")
	}
}

// Leader entry 110, follower price 100: a long is ~9.09% better than the leader.
func TestCurrentPositionCopyPriceRuleSkipsAtSealAndStaysSelectable(t *testing.T) {
	e, s, _ := currentCopyEngineWith(t, true, currentCopyPriceRule(10, "p0", "p1"))
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	if fills := e.detectBinancePositionSnapshotFills(); len(fills) != 0 {
		t.Fatalf("unfavourable prices were copied: %+v", fills)
	}
	r, _ := s.CopyTrade().CurrentPositionCopy(e.traderID)
	if r.Status != "DONE" || len(r.Tasks) != 2 {
		t.Fatalf("request=%+v", r)
	}
	for _, task := range r.Tasks {
		if task.Reason != "PRICE_NOT_FAVORABLE" {
			t.Fatalf("task=%+v", task)
		}
		task.Reason = ""
		if reason, err := s.CopyTrade().CurrentCopyEligibility(e.traderID, "leader", task); err != nil || reason != "" {
			t.Fatalf("price skip must stay selectable next time: %q %v", reason, err)
		}
	}
}

func TestCurrentPositionCopyPriceRuleAllowsFavourableSelection(t *testing.T) {
	e, s, _ := currentCopyEngineWith(t, true, currentCopyPriceRule(5, "p0", "p1"))
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 2 {
		t.Fatalf("favourable selection not copied: %+v", fills)
	}
	d := copyDecision(t, e, fills[0])
	exec := &positionMarginLifecycleExecutor{marketPrice: 100}
	ti := &TraderIntegration{traderID: e.traderID, store: s, engine: e, executor: exec}
	d.MarginMode = "cross"
	if err := ti.validateCurrentPositionCopy(d); err != nil {
		t.Fatalf("favourable price rejected: %v", err)
	}
	// The price moved against the follower inside the catch-up window.
	exec.marketPrice = 108
	err := ti.validateCurrentPositionCopy(d)
	if err == nil || !strings.Contains(err.Error(), "价格优势") {
		t.Fatalf("worse price crossed the submission boundary: %v", err)
	}
}

func TestCurrentPositionCopyUnselectedLegIsNotFollowedAsNewOpen(t *testing.T) {
	e, s, p := currentCopyEngineWith(t, true, func(cfg *store.CopyTradeConfig) {
		cfg.CopyCurrentPositionsSelection = []store.CurrentPositionCopySelection{{LeaderPosID: "p0"}}
	})
	p.state.Positions["p1"].Symbol = "BTCUSDT"
	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].LeaderPosID != "p0" {
		t.Fatalf("selection not honoured: %+v", fills)
	}
	commitCurrentCopyFill(t, e, copyDecision(t, e, fills[0]))
	if err := e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	if got := e.detectBinancePositionSnapshotFills(); len(got) != 0 {
		t.Fatalf("unselected position chased later: %+v", got)
	}
	if m, _ := s.CopyTrade().GetMapping(e.traderID, "p1"); m == nil || m.Status != store.MappingStatusIgnored {
		t.Fatalf("unselected mapping=%+v", m)
	}
}

// FollowCoin snapshots are stamped with the platform's update time, minutes in
// the past; the request must still seal and copy within the local window.
func TestFollowCoinCurrentPositionCopyFullChain(t *testing.T) {
	e, s := newTestCopyTradeEngine(t, ProviderFollowCoin)
	e.config.LeaderID = "31535"
	e.config.SourceGeneration = 1
	e.config.FollowExitPolicyVersion = 2
	e.config.CopyCatchupWindowSeconds = 60
	if _, err := s.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,lifecycle_status,lifecycle_generation,is_running,decision_mode) VALUES(?,'u','copy','','account',100,'STOPPED',0,0,'copy_trade')`, e.traderID); err != nil {
		t.Fatal(err)
	}
	on, pct := true, 0.0
	pos := fcTestPosition("BTCUSDT", SideLong, 0.03, 2000, 1794694989778)
	other := fcTestPosition("SOLUSDT", SideShort, 1, 150, 1794694989000)
	cfg := &store.CopyTradeConfig{TraderID: e.traderID, ProviderType: "followcoin", LeaderID: "31535", CopyRatio: 1, SourceGeneration: 1,
		CopyCurrentPositionsOnce: &on, CopyCurrentPositionsRequestID: uuid.NewString(), CopyCurrentPriceFilterEnabled: &on, CopyCurrentMinAdvantagePct: &pct,
		CopyCurrentPositionsSelection: []store.CurrentPositionCopySelection{{LeaderPosID: pos.PosID, OpenedMS: pos.OpenedMS}}}
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
	e.provider = &fcPollTestProvider{state: &AccountState{TotalEquity: 1000, Timestamp: time.Now().Add(-8 * time.Minute),
		Positions: map[string]*Position{pos.PosID: pos, other.PosID: other}}}
	e.currentCopyPreview = func(state *AccountState) ([]store.CurrentPositionCopyTask, error) {
		return BuildCurrentPositionPreview(s, e.traderID, "31535", 1, state, 100, nil, func(string) (float64, error) { return 1990, nil }, &positionMarginLifecycleExecutor{})
	}
	if err = e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err = e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].LeaderPosID != pos.PosID {
		r, _ := s.CopyTrade().CurrentPositionCopy(e.traderID)
		t.Fatalf("followcoin copy not produced: fills=%+v request=%+v", fills, r)
	}
	d := copyDecision(t, e, fills[0])
	if d.CurrentPositionTaskID == 0 || d.Action != "open_long" {
		t.Fatalf("decision=%+v", d)
	}
	ti := &TraderIntegration{traderID: e.traderID, store: s, engine: e, executor: &positionMarginLifecycleExecutor{marketPrice: 1995}}
	if err = ti.validateCurrentPositionCopy(d); err != nil {
		t.Fatalf("pre-submit recheck rejected a valid followcoin copy: %v", err)
	}
	r, _ := s.CopyTrade().CurrentPositionCopy(e.traderID)
	for _, task := range r.Tasks {
		if task.LeaderPosID == other.PosID && task.Reason != "NOT_SELECTED" {
			t.Fatalf("unselected followcoin position=%+v", task)
		}
	}
}

// Leader entry 110, follower price 100 (+9.09%), minimum 10%, tolerance 1%:
// the rows miss the minimum but sit inside the manual band. The preview keeps
// them selectable without preselecting; only an explicit pick copies them, and
// the pre-submit recheck then allows at most the tolerated disadvantage.
func TestCurrentPositionCopyManualPickWithinTolerance(t *testing.T) {
	manualRule := func(accept bool) func(*store.CopyTradeConfig) {
		return func(cfg *store.CopyTradeConfig) {
			currentCopyPriceRule(10, "p0", "p1")(cfg)
			tolerance := 1.0
			cfg.CopyCurrentManualMaxDisadvantagePct = &tolerance
			for i := range cfg.CopyCurrentPositionsSelection {
				cfg.CopyCurrentPositionsSelection[i].AcceptUnfavorable = accept
			}
		}
	}
	t.Run("preview", func(t *testing.T) {
		e, s, p := currentCopyEngineWith(t, false, nil)
		tasks, err := e.currentCopyPreview(p.state)
		if err != nil {
			t.Fatal(err)
		}
		tasks, err = ApplyCurrentCopyPriceFilter(s, e.traderID, "leader", tasks, true, 10, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range tasks {
			if task.Status != "READY" || task.Reason != "" || !task.PriceManualOnly {
				t.Fatalf("manual-band row must stay selectable and flagged: %+v", task)
			}
		}
		tasks, _ = e.currentCopyPreview(p.state)
		tasks, _ = ApplyCurrentCopyPriceFilter(s, e.traderID, "leader", tasks, true, 10, 0, nil)
		for _, task := range tasks {
			if task.Reason != "PRICE_NOT_FAVORABLE" || task.PriceManualOnly {
				t.Fatalf("zero tolerance keeps the strict rule: %+v", task)
			}
		}
	})
	for _, accept := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-accepted", true: "accepted"}[accept], func(t *testing.T) {
			e, s, _ := currentCopyEngineWith(t, true, manualRule(accept))
			if err := e.InitIgnoredPositions(); err != nil {
				t.Fatal(err)
			}
			if err := e.syncLeaderState(); err != nil {
				t.Fatal(err)
			}
			fills := e.detectBinancePositionSnapshotFills()
			if !accept {
				if len(fills) != 0 {
					t.Fatalf("selection without acceptance copied an unfavourable price: %+v", fills)
				}
				return
			}
			if len(fills) != 2 {
				r, _ := s.CopyTrade().CurrentPositionCopy(e.traderID)
				t.Fatalf("accepted manual picks not copied: %+v %+v", fills, r)
			}
			d := copyDecision(t, e, fills[0])
			d.MarginMode = "cross"
			exec := &positionMarginLifecycleExecutor{marketPrice: 110.5} // -0.45%
			ti := &TraderIntegration{traderID: e.traderID, store: s, engine: e, executor: exec}
			if err := ti.validateCurrentPositionCopy(d); err != nil {
				t.Fatalf("tolerated disadvantage rejected: %v", err)
			}
			exec.marketPrice = 112 // -1.82%
			if err := ti.validateCurrentPositionCopy(d); err == nil || !strings.Contains(err.Error(), "价格优势") {
				t.Fatalf("disadvantage beyond tolerance crossed the boundary: %v", err)
			}
		})
	}
}
