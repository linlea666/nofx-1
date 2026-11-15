package copytrade

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

// hlPollTestProvider simulates the production HyperliquidProvider capability
// set: authoritative snapshot hot path, no raw fills consumed.
type hlPollTestProvider struct {
	state         *AccountState
	stateErr      error
	fillsCalls    int
	snapshotCalls int
}

func (p *hlPollTestProvider) GetFills(string, time.Time) ([]Fill, error) {
	p.fillsCalls++
	return nil, nil
}

func (p *hlPollTestProvider) GetAccountState(leader string) (*AccountState, error) {
	return p.GetPositionSnapshot(leader)
}

func (p *hlPollTestProvider) GetPositionSnapshot(string) (*AccountState, error) {
	p.snapshotCalls++
	if p.stateErr != nil {
		return nil, p.stateErr
	}
	return cloneLeaderSnapshot(p.state), nil
}

func (p *hlPollTestProvider) Type() ProviderType                    { return ProviderHyperliquid }
func (p *hlPollTestProvider) UseAuthoritativeSnapshotHotPath() bool { return true }

func hlTestPosition(symbol string, side SideType, size, mark float64) *Position {
	return &Position{
		PosID: PositionKey(symbol, side), Symbol: symbol, Side: side, Size: size,
		EntryPrice: mark, MarkPrice: mark, Leverage: 10, MarginMode: "cross",
		PositionValue: size * mark,
	}
}

func TestHyperliquidProductionProviderUsesSnapshotPollingPath(t *testing.T) {
	provider, err := NewProvider(&CopyConfig{ProviderType: ProviderHyperliquid})
	if err != nil {
		t.Fatal(err)
	}
	if hot, ok := provider.(AuthoritativeSnapshotHotPathProvider); !ok || !hot.UseAuthoritativeSnapshotHotPath() {
		t.Fatal("Hyperliquid provider must use the authoritative snapshot hot path")
	}
	if _, ok := provider.(SourcePollScheduleProvider); !ok {
		t.Fatal("Hyperliquid provider must schedule its own poll cadence")
	}
	if _, ok := provider.(interface {
		GetPositionSnapshot(string) (*AccountState, error)
	}); !ok {
		t.Fatal("Hyperliquid provider must expose GetPositionSnapshot")
	}
	if _, ok := provider.(interface{ GetLeaderEquity(string) (float64, error) }); ok {
		t.Fatal("Hyperliquid sizing equity must come atomically from the same snapshot, not a second fetch")
	}
	if SupportsCopyGuard(ProviderHyperliquid) {
		t.Fatal("Hyperliquid Copy Guard support is out of scope")
	}
}

func TestHyperliquidPollOpensFromSnapshotWithoutRawFills(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderHyperliquid)
	e.config.MinTradeWarn = 1
	pos := hlTestPosition("BTCUSDT", SideLong, 0.03, 2000)
	provider := &hlPollTestProvider{state: &AccountState{
		TotalEquity: 1000, Timestamp: time.Now(),
		Positions: map[string]*Position{pos.PosID: pos},
	}}
	e.provider = provider

	e.poll()
	if provider.fillsCalls != 0 {
		t.Fatalf("snapshot hot path must never read raw userFills: calls=%d", provider.fillsCalls)
	}
	select {
	case full := <-e.decisionCh:
		if len(full.Decisions) != 1 {
			t.Fatalf("expected one decision: %+v", full.Decisions)
		}
		d := full.Decisions[0]
		if d.Action != "open_long" || d.LeaderPosID != "BTCUSDT_long" || d.LeaderPosSize != 0.03 {
			t.Fatalf("unexpected decision: %+v", d)
		}
		if !strings.HasPrefix(d.SourceFillID, "hyperliquid_snapshot|r0|BTCUSDT_long|open|") {
			t.Fatalf("snapshot identity must carry revision: %s", d.SourceFillID)
		}
		// 跟单比例不变：0.03 × 2000 / 1000 × followerEquity(100) × ratio(1) = 6
		if math.Abs(d.PositionSizeUSD-6) > 1e-9 {
			t.Fatalf("proportional sizing changed: got %.8f want 6", d.PositionSizeUSD)
		}
	default:
		t.Fatal("expected Hyperliquid snapshot open decision")
	}

	e.poll()
	select {
	case extra := <-e.decisionCh:
		t.Fatalf("unchanged snapshot produced duplicate decision: %+v", extra.Decisions)
	default:
	}
}

func TestHyperliquidPollFailsClosedWhenSnapshotUnavailable(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderHyperliquid)
	const posID = "ETHUSDT_long"
	saveActiveMapping(t, st, posID, 0.02)
	cached := hlTestPosition("ETHUSDT", SideLong, 0.02, 2000)
	e.leaderState.Positions[posID] = cached
	provider := &hlPollTestProvider{stateErr: errors.New("HTTP 500")}
	e.provider = provider

	e.poll()
	if provider.fillsCalls != 0 || provider.snapshotCalls == 0 {
		t.Fatalf("expected snapshot attempt only: fills=%d snapshots=%d", provider.fillsCalls, provider.snapshotCalls)
	}
	select {
	case got := <-e.decisionCh:
		t.Fatalf("failed snapshot must not drive decisions from stale cache: %+v", got.Decisions)
	default:
	}
	if len(e.seenFills) != 0 {
		t.Fatalf("failed round must not acknowledge any source transition: %+v", e.seenFills)
	}
}

func TestHyperliquidSnapshotDetectsAddReduceAndClose(t *testing.T) {
	const posID = "ETHUSDT_long"

	t.Run("add", func(t *testing.T) {
		e, st := newTestCopyTradeEngine(t, ProviderHyperliquid)
		saveActiveMapping(t, st, posID, 0.01)
		e.leaderState.Positions[posID] = hlTestPosition("ETHUSDT", SideLong, 0.03, 2000)
		fills := e.detectBinancePositionSnapshotFills()
		if len(fills) != 1 || fills[0].Action != ActionAdd || math.Abs(fills[0].Size-0.02) > 1e-12 {
			t.Fatalf("expected add of 0.02: %+v", fills)
		}
		if math.Abs(fills[0].Value-40) > 1e-9 || !fills[0].ValueUSDValid || fills[0].ValueCurrency != "USD" {
			t.Fatalf("add value must scale positionValue by size delta: %+v", fills[0])
		}
	})

	t.Run("reduce matches and sizes by ratio", func(t *testing.T) {
		e, st := newTestCopyTradeEngine(t, ProviderHyperliquid)
		saveActiveMapping(t, st, posID, 0.02)
		e.leaderState.Positions[posID] = hlTestPosition("ETHUSDT", SideLong, 0.01, 2000)
		fills := e.detectBinancePositionSnapshotFills()
		if len(fills) != 1 || fills[0].Action != ActionReduce {
			t.Fatalf("expected reduce: %+v", fills)
		}
		signal := e.buildSignal(&fills[0])
		match := e.matchSignalWithMapping(signal)
		if !match.ShouldFollow || match.Action != ActionReduce || match.PosID != posID {
			t.Fatalf("reduce must match active mapping: %+v", match)
		}
		if ratio := e.calculateReduceRatioV2(signal, match); math.Abs(ratio-0.5) > 1e-9 {
			t.Fatalf("reduce ratio = (last-current)/last must be 0.5, got %f", ratio)
		}
	})

	t.Run("missing position closes", func(t *testing.T) {
		e, st := newTestCopyTradeEngine(t, ProviderHyperliquid)
		saveActiveMapping(t, st, posID, 0.02)
		fills := e.detectBinancePositionSnapshotFills()
		if len(fills) != 1 || fills[0].Action != ActionClose || fills[0].LeaderPosID != posID {
			t.Fatalf("expected close for vanished position: %+v", fills)
		}
		match := e.matchSignalWithMapping(e.buildSignal(&fills[0]))
		if !match.ShouldFollow || match.Action != ActionClose {
			t.Fatalf("close must match active mapping: %+v", match)
		}
	})
}

func TestHyperliquidReversalClosesOldSideBeforeOpeningNewSide(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderHyperliquid)
	saveActiveMapping(t, st, "ETHUSDT_long", 0.02)
	short := hlTestPosition("ETHUSDT", SideShort, 0.05, 2000)
	e.leaderState.Positions[short.PosID] = short

	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 2 {
		t.Fatalf("reversal must yield close + open: %+v", fills)
	}
	if fills[0].Action != ActionClose || fills[0].LeaderPosID != "ETHUSDT_long" {
		t.Fatalf("old long must close first: %+v", fills[0])
	}
	if fills[1].Action != ActionOpen || fills[1].LeaderPosID != "ETHUSDT_short" || fills[1].Size != 0.05 {
		t.Fatalf("new short must open after close: %+v", fills[1])
	}
}

func TestHyperliquidSnapshotRevisionDistinguishesCloseReopen(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderHyperliquid)
	pos := hlTestPosition("ETHUSDT", SideLong, 0.01, 2000)
	e.leaderState.Positions[pos.PosID] = pos

	first := e.detectBinancePositionSnapshotFills()
	retry := e.detectBinancePositionSnapshotFills()
	if len(first) != 1 || len(retry) != 1 || first[0].ID != retry[0].ID {
		t.Fatalf("unacknowledged snapshot must retain one identity: first=%+v retry=%+v", first, retry)
	}
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: e.traderID, LeaderPosID: pos.PosID, LeaderID: e.config.LeaderID,
		Symbol: pos.Symbol, Side: string(pos.Side), MarginMode: pos.MarginMode,
		OpenedAt: time.Now(), OpenPrice: pos.EntryPrice, LastKnownSize: pos.Size,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().CloseMapping(e.traderID, pos.PosID, pos.MarkPrice); err != nil {
		t.Fatal(err)
	}

	reopened := e.detectBinancePositionSnapshotFills()
	if len(reopened) != 1 || reopened[0].Action != ActionOpen {
		t.Fatalf("expected reopen snapshot: %+v", reopened)
	}
	if reopened[0].ID == first[0].ID {
		t.Fatalf("reused symbol_side posId and size must not reuse prior lifecycle identity: %s", reopened[0].ID)
	}
}

func TestHyperliquidIgnoredPositionFollowedOnlyAfterLeaderCloses(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderHyperliquid)
	pos := hlTestPosition("ETHUSDT", SideLong, 0.02, 2000)
	if err := st.CopyTrade().SaveIgnoredPosition(e.traderID, e.config.LeaderID, pos.PosID,
		pos.Symbol, string(pos.Side), pos.MarginMode); err != nil {
		t.Fatal(err)
	}

	e.leaderState.Positions[pos.PosID] = pos
	if fills := e.detectBinancePositionSnapshotFills(); len(fills) != 0 {
		t.Fatalf("pre-existing position must not be followed while held: %+v", fills)
	}

	delete(e.leaderState.Positions, pos.PosID)
	e.checkIgnoredPositionsClosed()
	mapping, err := st.CopyTrade().GetMappingForReconciliation(e.traderID, pos.PosID)
	if err != nil || mapping == nil || mapping.Status != store.MappingStatusClosed {
		t.Fatalf("ignored position must close once the leader exits: %+v err=%v", mapping, err)
	}

	e.leaderState.Positions[pos.PosID] = pos
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].Action != ActionOpen {
		t.Fatalf("leader reopen after ignored close must be followed: %+v", fills)
	}
}

func TestHyperliquidSnapshotMissingValueFailsClosedForRiskIncrease(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderHyperliquid)
	pos := &Position{PosID: "TESTUSDT_long", Symbol: "TESTUSDT", Side: SideLong, Size: 100, MarginMode: "cross"}
	fill := e.buildBinanceSnapshotFillForPosition(pos, pos.PosID, ActionOpen, pos.Size, 0, pos.Size, 0)
	match := &SignalMatchResult{Action: ActionOpen, PosID: pos.PosID, LeaderPosition: pos}
	if fill.ValueUSDValid || !e.blockInvalidSourceRiskIncrease(&TradeSignal{Fill: &fill}, match) {
		t.Fatalf("unvalued Hyperliquid open must be blocked: %+v", fill)
	}
	closeMatch := &SignalMatchResult{Action: ActionClose, PosID: pos.PosID}
	if e.blockInvalidSourceRiskIncrease(&TradeSignal{Fill: &fill}, closeMatch) {
		t.Fatal("risk-reducing Hyperliquid transitions must never be blocked by value checks")
	}
}
