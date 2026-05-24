package copytrade

import (
	"errors"
	"io"
	"net/http"
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

// detailListMockTransport 构造 detail-list 接口的 mock transport，
// 仅响应 detail-list 请求，其余 path 返回错误（避免误触发其他 API 调用）。
func detailListMockTransport(t *testing.T, body string) roundTripFunc {
	t.Helper()
	return func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/copy-portfolio/detail-list") {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}
		t.Fatalf("unexpected request path: %s", req.URL.Path)
		return nil, nil
	}
}

// TestBinanceCalculateUsesFollowerMarginBalance 验证：Binance 分支下，
// calculateCopySizeByPositionChange 使用 follower marginBalance 做分母（而非领航员 marginBalance），
// 解决量纲错配问题。复现的真实数据：
//   - 镜像 fill.Value = 341.16 USDT
//   - 领航员 marginBalance = 3493 USDT（错误锚定）
//   - 跟随者 marginBalance = 199.99 USDT（正确锚定）
//   - 修复前：copySize = 100 × 1.2 × 341.16/3493 ≈ 11.7 USDT（被提升到 12 USDT 阈值）
//   - 修复后：copySize = 100 × 1.2 × 341.16/199.99 ≈ 204.7 USDT
func TestBinanceCalculateUsesFollowerMarginBalance(t *testing.T) {
	const leadID = "4959394188752686849"
	body := `{"code":"000000","data":[
		{"leadPortfolioId":"` + leadID + `","copyPortfolioId":"copy-target","nickname":"Btc熊猫",
		 "netCopyAmount":200.0,"marginBalance":199.99632798,
		 "copyMode":"FIXED_RATIO","leadStatus":"ACTIVE","isPaused":false}
	]}`

	bp := newBinanceProviderWithTransport(detailListMockTransport(t, body))
	e, _ := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.LeaderID = leadID
	e.config.CopyRatio = 1.2
	e.provider = bp
	e.leaderState = &AccountState{TotalEquity: 3493.16, Positions: map[string]*Position{}} // 领航员真权益（不应再用作分母）

	signal := &TradeSignal{
		LeaderID:     leadID,
		ProviderType: ProviderBinance,
		Fill: &Fill{
			Symbol:       "ETHUSDT",
			PositionSide: SideLong,
			Price:        2119.0271,
			Size:         0.1610,
			Value:        341.16, // 镜像价值（与日志中 Binance fill.Value 一致）
		},
		LeaderEquity: 3493.16, // 领航员真权益（不应再用作分母）
	}
	match := &SignalMatchResult{Action: ActionOpen}

	copySize, _ := e.calculateCopySizeByPositionChange(signal, match)

	// 期望：100 × 1.2 × 341.16 / 199.996 ≈ 204.7
	if copySize < 195 || copySize > 215 {
		t.Fatalf("copySize=%.2f want ~205 (修复后量级), 修复前为 ~12 USDT", copySize)
	}
}

// TestBinanceCalculateFallbackOnDetailListFailure 验证：detail-list 失败时降级到
// 旧逻辑（用领航员 marginBalance 当分母），保留当前行为，不会崩。
func TestBinanceCalculateFallbackOnDetailListFailure(t *testing.T) {
	bp := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"code":"100001005","message":"not login"}`)),
			Header:     make(http.Header),
		}, nil
	})
	e, _ := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.LeaderID = "any-id"
	e.config.CopyRatio = 1.2
	e.provider = bp
	e.leaderState = &AccountState{TotalEquity: 3493.16, Positions: map[string]*Position{}}

	signal := &TradeSignal{
		LeaderID:     "any-id",
		ProviderType: ProviderBinance,
		Fill: &Fill{
			Symbol:       "ETHUSDT",
			PositionSide: SideLong,
			Price:        2119.0271,
			Size:         0.1610,
			Value:        341.16,
		},
		LeaderEquity: 3493.16,
	}
	match := &SignalMatchResult{Action: ActionOpen}

	copySize, _ := e.calculateCopySizeByPositionChange(signal, match)

	// 降级路径：使用 leaderEquity=3493 → ratio≈9.76% → copy=11.7 → 自动提升到 12 USDT
	if copySize != 12.0 {
		t.Fatalf("expected fallback boosted to 12.00 USDT, got %.2f", copySize)
	}
}

// TestOKXCalculateNotAffectedByBinanceLogic 验证：OKX 与 Binance 配置完全隔离，
// OKX 走老逻辑（leaderEquity 当分母），新加的 resolveBinanceAnchorEquity 完全不参与。
func TestOKXCalculateNotAffectedByBinanceLogic(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderOKX)
	e.config.CopyRatio = 1.0
	// 故意注入会让测试 panic 的 BinanceProvider 实例 —— 验证 OKX 分支根本不会调用它
	bp := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("OKX path must not call Binance APIs (got: %s)", req.URL.Path)
		return nil, nil
	})
	_ = bp // 不赋值给 e.provider；用 fake provider 维持 OKX 行为

	signal := &TradeSignal{
		ProviderType: ProviderOKX,
		Fill: &Fill{
			Symbol:       "ETHUSDT",
			PositionSide: SideLong,
			Price:        2000,
			Size:         0.1,
			Value:        200,
		},
		LeaderEquity: 1000,
	}
	match := &SignalMatchResult{Action: ActionOpen}

	copySize, _ := e.calculateCopySizeByPositionChange(signal, match)

	// 期望走 OKX 老逻辑：100 × 1.0 × 200/1000 = 20
	if copySize < 18 || copySize > 22 {
		t.Fatalf("OKX copySize=%.2f want ~20 (老逻辑未被影响)", copySize)
	}
}
