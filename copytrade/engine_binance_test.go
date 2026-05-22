package copytrade

import (
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
