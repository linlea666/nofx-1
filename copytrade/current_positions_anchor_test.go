package copytrade

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"nofx/store"
)

// hlAnchorTestProvider serves a Hyperliquid-shaped snapshot: positions carry no
// venue open time (OpenedMS=0) and PosID is reused across lifecycles.
type hlAnchorTestProvider struct{ fcPollTestProvider }

func (p *hlAnchorTestProvider) Type() ProviderType { return ProviderHyperliquid }

func hlAnchorPosition(symbol string, side SideType, size, entry float64) *Position {
	return &Position{
		PosID: PositionKey(symbol, side), Symbol: symbol, Side: side, Size: size,
		EntryPrice: entry, MarkPrice: entry, Leverage: 10, MarginMode: "cross",
		PositionValue: size * entry,
	}
}

// anchoredCurrentCopyEngine registers and seals a one-shot copy of pos for a
// Hyperliquid leader, the way the operator's preview selection would (the
// preview clears anchors, so the selection carries opened_ms=0).
func anchoredCurrentCopyEngine(t *testing.T, pos *Position) (*Engine, *store.Store, *hlAnchorTestProvider) {
	t.Helper()
	e, s := newTestCopyTradeEngine(t, ProviderHyperliquid)
	e.config.LeaderID = "0xleader"
	e.config.SourceGeneration = 1
	e.config.CopyCatchupWindowSeconds = 60
	if _, err := s.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,lifecycle_status,lifecycle_generation,is_running,decision_mode) VALUES(?,'u','copy','','account',100,'STOPPED',0,0,'copy_trade')`, e.traderID); err != nil {
		t.Fatal(err)
	}
	on := true
	cfg := &store.CopyTradeConfig{TraderID: e.traderID, ProviderType: "hyperliquid", LeaderID: "0xleader", CopyRatio: 1, SourceGeneration: 1,
		CopyCurrentPositionsOnce: &on, CopyCurrentPositionsRequestID: uuid.NewString(),
		CopyCurrentPositionsSelection: []store.CurrentPositionCopySelection{{LeaderPosID: pos.PosID}}}
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
	provider := &hlAnchorTestProvider{fcPollTestProvider{state: &AccountState{TotalEquity: 1000, Timestamp: time.Now(),
		Positions: map[string]*Position{pos.PosID: pos}}}}
	e.provider = provider
	// Mirrors the production closure in integration.go.
	e.currentCopyPreview = func(state *AccountState) ([]store.CurrentPositionCopyTask, error) {
		anchored := anchorCurrentCopyLifecycle(ProviderHyperliquid, state, time.Now().UnixMilli())
		return BuildCurrentPositionPreview(s, e.traderID, "0xleader", 1, anchored, 100, nil, func(string) (float64, error) { return pos.EntryPrice, nil }, &positionMarginLifecycleExecutor{})
	}
	if err = e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if err = e.syncLeaderState(); err != nil {
		t.Fatal(err)
	}
	return e, s, provider
}

func TestAnchoredCurrentPositionCopyFullChain(t *testing.T) {
	pos := hlAnchorPosition("ETHUSDT", SideLong, 2, 2000)
	e, s, _ := anchoredCurrentCopyEngine(t, pos)
	r, err := s.CopyTrade().CurrentPositionCopy(e.traderID)
	if err != nil || r == nil || r.Status != "SEALED" || len(r.Tasks) != 1 || r.Tasks[0].Status != "READY" || r.Tasks[0].OpenedMS <= 0 {
		t.Fatalf("hyperliquid request not sealed with an anchor: %+v %v", r, err)
	}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].CurrentPositionTaskID == 0 {
		t.Fatalf("anchored copy not produced: %+v", fills)
	}
	d := copyDecision(t, e, fills[0])
	if d.CurrentPositionTaskID == 0 || d.Action != "open_long" || d.SourceOpenedMS != r.Tasks[0].OpenedMS {
		t.Fatalf("decision must carry the sealed anchor: %+v task=%+v", d, r.Tasks[0])
	}
	ti := &TraderIntegration{traderID: e.traderID, store: s, engine: e, executor: &positionMarginLifecycleExecutor{marketPrice: 2000}}
	if err = ti.validateCurrentPositionCopy(d); err != nil {
		t.Fatalf("pre-submit recheck rejected an unchanged anchored position: %v", err)
	}
	// The anchor identifies the authorization only; it is not a venue open time.
	var lifecycles int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_source_lifecycles WHERE trader_id=? AND leader_pos_id=?`, e.traderID, pos.PosID).Scan(&lifecycles); err != nil || lifecycles != 0 {
		t.Fatalf("seal-time anchor recorded as a source lifecycle: %d %v", lifecycles, err)
	}
}

// A same-ID position whose entry price changed after the seal (closed and
// reopened, or added to) is no longer the authorized one.
func TestAnchoredCurrentPositionCopyRejectsChangedFingerprint(t *testing.T) {
	pos := hlAnchorPosition("ETHUSDT", SideLong, 2, 2000)
	e, _, provider := anchoredCurrentCopyEngine(t, pos)
	reopened := hlAnchorPosition("ETHUSDT", SideLong, 2, 2100)
	provider.state = &AccountState{TotalEquity: 1000, Timestamp: time.Now(), Positions: map[string]*Position{reopened.PosID: reopened}}
	e.leaderState = cloneLeaderSnapshot(provider.state)
	if fills := e.detectBinancePositionSnapshotFills(); len(fills) != 0 {
		t.Fatalf("changed position copied under the old authorization: %+v", fills)
	}
	if e.currentCopyOpenedMS(reopened) != 0 || e.currentCopyOpenedMS(pos) == 0 {
		t.Fatal("fingerprint must accept only the sealed position")
	}
}

// Anchors live in memory: after a restart a sealed anchored task can no longer
// be re-proven and must end without a fill.
func TestAnchoredCurrentPositionCopyEndsWhenAnchorLost(t *testing.T) {
	pos := hlAnchorPosition("ETHUSDT", SideLong, 2, 2000)
	e, s, _ := anchoredCurrentCopyEngine(t, pos)
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 {
		t.Fatalf("anchored copy not produced: %+v", fills)
	}
	d := copyDecision(t, e, fills[0])
	e.setCurrentCopyAnchors(nil)
	ti := &TraderIntegration{traderID: e.traderID, store: s, engine: e, executor: &positionMarginLifecycleExecutor{marketPrice: 2000}}
	if err := ti.validateCurrentPositionCopy(d); err == nil {
		t.Fatal("anchored copy submitted after its anchor was lost")
	}
}

func TestAnchorCurrentCopyLifecycleLeavesNativeSourcesUntouched(t *testing.T) {
	native := &Position{PosID: "okx-1", OpenedMS: 0}
	state := &AccountState{Positions: map[string]*Position{"okx-1": native}}
	if got := anchorCurrentCopyLifecycle(ProviderOKX, state, 42); got != state || native.OpenedMS != 0 {
		t.Fatal("native source must keep its own (missing) open time")
	}
	anchored := anchorCurrentCopyLifecycle(ProviderBinance, state, 42)
	if anchored.Positions["okx-1"].OpenedMS != 42 || native.OpenedMS != 0 {
		t.Fatal("anchoring must stamp a copy, never the live snapshot")
	}
}

// Binance copy-management positions are the follower's mirrored copy portfolio;
// the one-shot copy must size them against that portfolio's margin balance,
// exactly like live execution, and fail closed without it.
func TestBinanceCopyManagementCurrentCopySizesAgainstCopyPortfolio(t *testing.T) {
	const leadID = "4959394188752686849"
	body := `{"code":"000000","data":[{"leadPortfolioId":"` + leadID + `","copyPortfolioId":"copy-target","marginBalance":200,"copyMode":"FIXED_RATIO","leadStatus":"ACTIVE"}]}`
	bp := newBinanceProviderWithTransport(detailListMockTransport(t, body))
	pos := &Position{PosID: "1_ETHUSDT_LONG", Symbol: "ETHUSDT", Side: SideLong, Size: 0.1, EntryPrice: 2000, MarginMode: "cross", PositionValue: 200}
	state := &AccountState{TotalEquity: 5000, Positions: map[string]*Position{pos.PosID: pos}}
	equity, err := currentCopySizingEquity(bp, leadID, state)
	if err != nil || equity != 200 {
		t.Fatalf("sizing equity=%v err=%v, want the copy portfolio margin balance 200", equity, err)
	}
	failing := newBinanceProviderWithTransport(detailListMockTransport(t, `{"code":"000000","data":[]}`))
	if _, err = currentCopySizingEquity(failing, leadID, state); err == nil {
		t.Fatal("missing copy portfolio must fail closed")
	}
	if equity, err = currentCopySizingEquity(bp, leadID, &AccountState{TotalEquity: 5000, Positions: map[string]*Position{}}); err != nil || equity != 5000 {
		t.Fatalf("flat leader keeps snapshot equity: %v %v", equity, err)
	}
}
