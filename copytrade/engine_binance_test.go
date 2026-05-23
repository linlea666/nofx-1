package copytrade

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

func newTestCopyTradeEngine(t *testing.T, providerType ProviderType) (*Engine, *store.Store) {
	t.Helper()

	st, err := store.New(filepath.Join(t.TempDir(), "nofx-test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	e := &Engine{
		traderID: "test-trader",
		config: &CopyConfig{
			ProviderType: providerType,
			LeaderID:     "leader",
			CopyRatio:    1,
			SyncLeverage: true,
		},
		store:         st,
		seenFills:     make(map[string]time.Time),
		seenTTL:       time.Hour,
		leaderState:   &AccountState{TotalEquity: 1000, Positions: map[string]*Position{}},
		decisionCh:    make(chan *decision.FullDecision, 10),
		stats:         &EngineStats{},
		lastStateSync: time.Now(),
		getFollowerBalance: func() float64 {
			return 100
		},
		getFollowerPositions: func() map[string]*Position {
			return map[string]*Position{}
		},
	}
	return e, st
}

type binancePollTestProvider struct {
	fills       []Fill
	fillsErr    error
	state       *AccountState
	history     []BinancePositionHistoryRecord
	fillsCalls  int
	stateCalls  int
	historyCall int
}

func (p *binancePollTestProvider) GetFills(_ string, _ time.Time) ([]Fill, error) {
	p.fillsCalls++
	if p.fillsErr != nil {
		return nil, p.fillsErr
	}
	return p.fills, nil
}

func (p *binancePollTestProvider) GetAccountState(_ string) (*AccountState, error) {
	p.stateCalls++
	if p.state == nil {
		return &AccountState{TotalEquity: 1000, Positions: map[string]*Position{}}, nil
	}
	return p.state, nil
}

func (p *binancePollTestProvider) Type() ProviderType {
	return ProviderBinance
}

func (p *binancePollTestProvider) GetPositionHistory(_ string) ([]BinancePositionHistoryRecord, error) {
	p.historyCall++
	return p.history, nil
}

func binanceTestPosition(posID string, size float64) *Position {
	return &Position{
		Symbol:        "ETHUSDT",
		Side:          SideLong,
		Size:          size,
		EntryPrice:    2096.58,
		MarkPrice:     2092.40,
		Leverage:      40,
		MarginMode:    "cross",
		PositionValue: size * 2092.40,
		PosID:         posID,
	}
}

func saveActiveMapping(t *testing.T, st *store.Store, posID string, lastKnownSize float64) {
	t.Helper()
	err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:      "test-trader",
		LeaderPosID:   posID,
		LeaderID:      "leader",
		Symbol:        "ETHUSDT",
		Side:          string(SideLong),
		MarginMode:    "cross",
		OpenedAt:      time.Now(),
		OpenPrice:     2096.58,
		OpenSizeUSD:   lastKnownSize * 2096.58,
		LastKnownSize: lastKnownSize,
	})
	if err != nil {
		t.Fatalf("save active mapping: %v", err)
	}
}

func TestBinancePositionSnapshotDetectsOpenAddReduceCloseAndIgnoresHistorical(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"

	t.Run("open without mapping", func(t *testing.T) {
		e, _ := newTestCopyTradeEngine(t, ProviderBinance)
		e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.01)

		fills := e.detectBinancePositionSnapshotFills()
		if len(fills) != 1 {
			t.Fatalf("fills len=%d want 1", len(fills))
		}
		if fills[0].Action != ActionOpen || fills[0].PositionSide != SideLong || fills[0].Size != 0.01 {
			t.Fatalf("unexpected fill: %+v", fills[0])
		}
	})

	t.Run("ignored historical position is skipped", func(t *testing.T) {
		e, st := newTestCopyTradeEngine(t, ProviderBinance)
		if err := st.CopyTrade().SaveIgnoredPosition("test-trader", "leader", posID, "ETHUSDT", string(SideLong), "cross"); err != nil {
			t.Fatalf("save ignored mapping: %v", err)
		}
		e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.01)

		fills := e.detectBinancePositionSnapshotFills()
		if len(fills) != 0 {
			t.Fatalf("fills len=%d want 0: %+v", len(fills), fills)
		}
	})

	t.Run("add when size increased", func(t *testing.T) {
		e, st := newTestCopyTradeEngine(t, ProviderBinance)
		saveActiveMapping(t, st, posID, 0.01)
		e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.02)

		fills := e.detectBinancePositionSnapshotFills()
		if len(fills) != 1 {
			t.Fatalf("fills len=%d want 1", len(fills))
		}
		if fills[0].Action != ActionAdd || fills[0].Size != 0.01 {
			t.Fatalf("unexpected add fill: %+v", fills[0])
		}
	})

	t.Run("reduce when size decreased", func(t *testing.T) {
		e, st := newTestCopyTradeEngine(t, ProviderBinance)
		saveActiveMapping(t, st, posID, 0.02)
		e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.01)

		fills := e.detectBinancePositionSnapshotFills()
		if len(fills) != 1 {
			t.Fatalf("fills len=%d want 1", len(fills))
		}
		if fills[0].Action != ActionReduce || fills[0].Size != 0.01 {
			t.Fatalf("unexpected reduce fill: %+v", fills[0])
		}
	})

	t.Run("close when active mapping disappeared", func(t *testing.T) {
		e, st := newTestCopyTradeEngine(t, ProviderBinance)
		saveActiveMapping(t, st, posID, 0.02)

		fills := e.detectBinancePositionSnapshotFills()
		if len(fills) != 1 {
			t.Fatalf("fills len=%d want 1", len(fills))
		}
		if fills[0].Action != ActionClose || fills[0].PositionSide != SideLong {
			t.Fatalf("unexpected close fill: %+v", fills[0])
		}
	})
}

func TestBinancePollUsesRealtimeSnapshotBeforeDelayedTradeHistory(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	saveActiveMapping(t, st, posID, 0.01)

	provider := &binancePollTestProvider{
		fills: []Fill{{
			ID:           "delayed-trade-history-fill",
			Symbol:       "ETHUSDT",
			Side:         "buy",
			PositionSide: SideLong,
			Action:       ActionAdd,
			Price:        2096.58,
			Size:         0.01,
			Value:        20.9658,
			Timestamp:    time.Now(),
		}},
		state: &AccountState{
			TotalEquity: 1000,
			Positions: map[string]*Position{
				posID: binanceTestPosition(posID, 0.02),
			},
		},
	}
	e.provider = provider

	e.poll()

	if provider.stateCalls == 0 {
		t.Fatalf("expected realtime position sync")
	}
	if !e.isSeen("delayed-trade-history-fill") {
		t.Fatalf("expected delayed trade-history fill to be marked seen after snapshot signal")
	}

	select {
	case dec := <-e.decisionCh:
		if len(dec.Decisions) != 1 {
			t.Fatalf("decision len=%d want 1", len(dec.Decisions))
		}
		got := dec.Decisions[0]
		if got.Action != "open_long" || got.LeaderPosID != posID || got.LeaderPosSize != 0.02 {
			t.Fatalf("unexpected decision from snapshot: %+v", got)
		}
	default:
		t.Fatalf("expected snapshot decision")
	}
	select {
	case extra := <-e.decisionCh:
		t.Fatalf("unexpected duplicate decision: %+v", extra.Decisions)
	default:
	}
}

func TestBinancePollStillUsesSnapshotWhenTradeHistoryFails(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"
	e, _ := newTestCopyTradeEngine(t, ProviderBinance)
	e.provider = &binancePollTestProvider{
		fillsErr: errors.New("temporary trade-history delay"),
		state: &AccountState{
			TotalEquity: 1000,
			Positions: map[string]*Position{
				posID: binanceTestPosition(posID, 0.01),
			},
		},
	}

	e.poll()

	select {
	case dec := <-e.decisionCh:
		if got := dec.Decisions[0]; got.Action != "open_long" || got.LeaderPosID != posID {
			t.Fatalf("unexpected decision: %+v", got)
		}
	default:
		t.Fatalf("expected snapshot decision despite trade-history error")
	}
}

func TestBinanceCloseSnapshotUsesPositionHistoryPrice(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	saveActiveMapping(t, st, posID, 0.039)
	e.provider = &binancePollTestProvider{
		history: []BinancePositionHistoryRecord{{
			Symbol:        "ETHUSDT",
			Side:          "Long",
			Status:        "All Closed",
			AvgClosePrice: 2041.57358974,
			ClosedVolume:  0.039,
			Closed:        time.Now().UnixMilli(),
		}},
	}

	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 {
		t.Fatalf("fills len=%d want 1", len(fills))
	}
	if fills[0].Action != ActionClose {
		t.Fatalf("unexpected action: %+v", fills[0])
	}
	if fills[0].Price != 2041.57358974 {
		t.Fatalf("close price=%f want position-history avg close", fills[0].Price)
	}
}

func TestBinanceLateTradeHistoryDoesNotDuplicateSnapshotOpenAdd(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	saveActiveMapping(t, st, posID, 0.01)
	e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.01)

	signal := e.buildSignal(&Fill{
		ID:           "late-fill",
		Symbol:       "ETHUSDT",
		Side:         "buy",
		PositionSide: SideLong,
		Action:       ActionOpen,
		Price:        2096.58,
		Size:         0.01,
		Value:        20.9658,
		Timestamp:    time.Now(),
	})
	result := e.matchOpenAddSignal(signal, e.buildLeaderPosMap())
	if result.ShouldFollow {
		t.Fatalf("expected Binance duplicate open/add to be skipped: %+v", result)
	}
	if !strings.Contains(result.Reason, "size 未增加") {
		t.Fatalf("unexpected reason: %s", result.Reason)
	}
}

func TestOKXOpenAddFallbackIsUnchanged(t *testing.T) {
	const posID = "okx-pos-id"
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	saveActiveMapping(t, st, posID, 0.01)
	e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.01)
	e.leaderState.Positions[posID].PosID = posID

	signal := e.buildSignal(&Fill{
		ID:           "okx-fill",
		Symbol:       "ETHUSDT",
		Side:         "buy",
		PositionSide: SideLong,
		Action:       ActionOpen,
		Price:        2096.58,
		Size:         0.01,
		Value:        20.9658,
		Timestamp:    time.Now(),
	})
	result := e.matchOpenAddSignal(signal, e.buildLeaderPosMap())
	if !result.ShouldFollow || result.Action != ActionAdd {
		t.Fatalf("expected OKX fallback add behavior unchanged, got %+v", result)
	}
}

func TestBinanceLateTradeHistoryDoesNotDuplicateSnapshotReduce(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	saveActiveMapping(t, st, posID, 0.01)
	e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.01)

	signal := e.buildSignal(&Fill{
		ID:           "late-reduce",
		Symbol:       "ETHUSDT",
		Side:         "sell",
		PositionSide: SideLong,
		Action:       ActionClose,
		Price:        2096.58,
		Size:         0.01,
		Value:        20.9658,
		Timestamp:    time.Now(),
	})
	result := e.matchCloseReduceSignal(signal, e.buildLeaderPosMap())
	if result.ShouldFollow {
		t.Fatalf("expected Binance duplicate reduce/close to be skipped: %+v", result)
	}
	if !strings.Contains(result.Reason, "size 未减少") {
		t.Fatalf("unexpected reason: %s", result.Reason)
	}
}
