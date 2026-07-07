package copytrade

import (
	"errors"
	"io"
	"math"
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

	t.Run("close when remaining size is near zero", func(t *testing.T) {
		e, st := newTestCopyTradeEngine(t, ProviderBinance)
		saveActiveMapping(t, st, posID, 0.02)
		e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.0009)

		fills := e.detectBinancePositionSnapshotFills()
		if len(fills) != 1 {
			t.Fatalf("fills len=%d want 1", len(fills))
		}
		if fills[0].Action != ActionClose || math.Abs(fills[0].Size-0.0191) > 1e-12 {
			t.Fatalf("unexpected near-zero close fill: %+v", fills[0])
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
	// PR-3 后 ActionAdd 不再 boost；此测试场景 copySize≈2.09 USDT
	// 会被默认阈值 12 跳过，显式调小阈值以聚焦"snapshot 优先于 trade-history"主题。
	e.config.MinTradeWarn = 1
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

// OKX 兜底加仓分支现与 Binance 共用 size 增量守卫：
// lastKnownSize 有值且领航员持仓未增加时，迟到/重复成交不再触发重复加仓。
func TestOKXOpenAddFallbackSkipsWhenSizeNotIncreased(t *testing.T) {
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
	if result.ShouldFollow {
		t.Fatalf("expected OKX duplicate open/add to be skipped: %+v", result)
	}
	if !strings.Contains(result.Reason, "size 未增加") {
		t.Fatalf("unexpected reason: %s", result.Reason)
	}
}

// lastKnownSize=0（旧数据）时 OKX 兜底加仓行为保持不变（守卫不生效）。
func TestOKXOpenAddFallbackLegacyMappingStillAdds(t *testing.T) {
	const posID = "okx-pos-id-legacy"
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	saveActiveMapping(t, st, posID, 0)
	e.leaderState.Positions[posID] = binanceTestPosition(posID, 0.01)
	e.leaderState.Positions[posID].PosID = posID

	signal := e.buildSignal(&Fill{
		ID:           "okx-fill-legacy",
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
		t.Fatalf("expected OKX legacy fallback add behavior unchanged, got %+v", result)
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

func TestBinanceDecisionSyncsLeaderLeverage(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.SyncLeverage = true

	pos := binanceTestPosition("1239518824_ETHUSDT_LONG", 0.02)
	pos.Leverage = 40
	signal := &TradeSignal{
		ProviderType:   ProviderBinance,
		Fill:           &Fill{Symbol: "ETHUSDT", PositionSide: SideLong, Price: 2000},
		LeaderPosition: pos,
		LeaderEquity:   1000,
		LeaderPosID:    pos.PosID,
	}
	match := &SignalMatchResult{
		ShouldFollow:   true,
		Action:         ActionOpen,
		PosID:          pos.PosID,
		MarginMode:     "cross",
		LeaderPosition: pos,
	}

	dec := e.buildDecisionV2(signal, match, 100)
	if dec.Leverage != 40 {
		t.Fatalf("expected synced leader leverage 40x, got %dx", dec.Leverage)
	}

	shortPos := binanceTestPosition("1239518824_ETHUSDT_SHORT", 0.036)
	shortPos.Side = SideShort
	shortPos.Leverage = 19
	shortSignal := &TradeSignal{
		ProviderType:   ProviderBinance,
		Fill:           &Fill{Symbol: "ETHUSDT", PositionSide: SideShort, Price: 2000},
		LeaderPosition: shortPos,
		LeaderEquity:   1000,
		LeaderPosID:    shortPos.PosID,
	}
	shortMatch := &SignalMatchResult{
		ShouldFollow:   true,
		Action:         ActionAdd,
		PosID:          shortPos.PosID,
		MarginMode:     "cross",
		LeaderPosition: shortPos,
	}
	dec = e.buildDecisionV2(shortSignal, shortMatch, 50)
	if dec.Action != "open_short" || dec.Leverage != 19 {
		t.Fatalf("expected add short decision with synced 19x leverage, got %+v", dec)
	}

	e.config.SyncLeverage = false
	dec = e.buildDecisionV2(signal, match, 100)
	if dec.Leverage != 10 {
		t.Fatalf("expected default leverage 10x when sync disabled, got %dx", dec.Leverage)
	}
}

func TestBinanceReduceDoesNotResolveAnchorOrRequireLeverage(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	saveActiveMapping(t, st, posID, 0.02)

	bp := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		switch {
		case strings.Contains(req.URL.Path, "/lead-portfolio/detail"):
			body = `{"code":"000000","data":{"copyPortfolioId":"copy-123","hasCopy":true,"marginBalance":"1000"}}`
		case strings.Contains(req.URL.Path, "/user-position"):
			body = `{"code":"000000","data":[{
				"id":"` + posID + `",
				"symbol":"ETHUSDT",
				"positionSide":"LONG",
				"positionAmount":"0.01",
				"entryPrice":"2000",
				"markPrice":"2000",
				"notionalValue":"20",
				"initialMargin":"2",
				"positionInitialMargin":"0"
			}]}`
		case strings.Contains(req.URL.Path, "/copy-portfolio/detail-list"):
			t.Fatalf("reduce/close path must not resolve Binance follower margin anchor")
		default:
			t.Fatalf("unexpected request path: %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})
	e.provider = bp

	e.processSignal(&TradeSignal{Fill: &Fill{
		ID:           "reduce-snapshot",
		Symbol:       "ETHUSDT",
		Side:         "sell",
		PositionSide: SideLong,
		Action:       ActionClose,
		Price:        2000,
		Size:         0.01,
		Value:        20,
		Timestamp:    time.Now(),
	}})

	select {
	case fullDec := <-e.decisionCh:
		if len(fullDec.Decisions) != 1 {
			t.Fatalf("decision len=%d want 1", len(fullDec.Decisions))
		}
		dec := fullDec.Decisions[0]
		if dec.Action != "reduce_long" {
			t.Fatalf("expected reduce_long, got %+v", dec)
		}
		if dec.PositionSizeUSD != 0 {
			t.Fatalf("reduce must not carry open position size, got %.8f", dec.PositionSizeUSD)
		}
		if dec.Leverage != 0 {
			t.Fatalf("reduce must not require leverage, got %dx", dec.Leverage)
		}
	default:
		t.Fatalf("expected reduce decision")
	}
}

func TestBinanceReduceIgnoresAccumulatedRatioAndUsesSnapshotRatio(t *testing.T) {
	const posID = "1243719130_ETHUSDT_SHORT"
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.LeaderID = "4980868128621309440"

	err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:               "test-trader",
		LeaderPosID:            posID,
		LeaderID:               e.config.LeaderID,
		Symbol:                 "ETHUSDT",
		Side:                   string(SideShort),
		MarginMode:             "cross",
		OpenedAt:               time.Now(),
		OpenPrice:              2088.15,
		OpenSizeUSD:            0.097 * 2088.15,
		LastKnownSize:          0.097,
		AccumulatedReduceRatio: 0.556,
	})
	if err != nil {
		t.Fatalf("save active mapping: %v", err)
	}
	if err := st.CopyTrade().UpdateAccumulatedReduceRatio("test-trader", posID, 0.556); err != nil {
		t.Fatalf("seed accumulated ratio: %v", err)
	}

	e.leaderState.Positions[posID] = &Position{
		Symbol:     "ETHUSDT",
		Side:       SideShort,
		Size:       0.047,
		MarkPrice:  2080.07,
		MarginMode: "cross",
		PosID:      posID,
	}

	signal := e.buildSignal(&Fill{
		ID:           "binance_snapshot|" + posID + "|reduce|0.09700000|0.04700000",
		Symbol:       "ETHUSDT",
		Side:         "buy",
		PositionSide: SideShort,
		Action:       ActionReduce,
		Price:        2080.07,
		Size:         0.05,
		Value:        104.0035,
		Timestamp:    time.Now(),
	})
	match := e.matchSignalWithMapping(signal)
	if !match.ShouldFollow || match.Action != ActionReduce {
		t.Fatalf("expected Binance partial reduce match, got %+v", match)
	}

	dec := e.buildDecisionV2(signal, match, 0)
	if dec.Action != "reduce_short" {
		t.Fatalf("expected reduce_short, got %+v", dec)
	}
	wantRatio := 0.05 / 0.097
	if math.Abs(dec.CloseRatio-wantRatio) > 1e-9 {
		t.Fatalf("CloseRatio=%.12f want %.12f", dec.CloseRatio, wantRatio)
	}
	if strings.Contains(dec.Reasoning, "accumulated") || strings.Contains(dec.Reasoning, "full close") {
		t.Fatalf("Binance reduce must not be converted by accumulated ratio, reasoning=%q", dec.Reasoning)
	}
}

func TestBinanceCloseDoesNotResolveAnchorOrRequireLeverage(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	saveActiveMapping(t, st, posID, 0.02)

	bp := newBinanceProviderWithTransport(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		switch {
		case strings.Contains(req.URL.Path, "/lead-portfolio/detail"):
			body = `{"code":"000000","data":{"copyPortfolioId":"copy-123","hasCopy":true,"marginBalance":"1000"}}`
		case strings.Contains(req.URL.Path, "/user-position"):
			body = `{"code":"000000","data":[]}`
		case strings.Contains(req.URL.Path, "/copy-portfolio/detail-list"):
			t.Fatalf("close path must not resolve Binance follower margin anchor")
		default:
			t.Fatalf("unexpected request path: %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})
	e.provider = bp

	e.processSignal(&TradeSignal{Fill: &Fill{
		ID:           "close-snapshot",
		Symbol:       "ETHUSDT",
		Side:         "sell",
		PositionSide: SideLong,
		Action:       ActionClose,
		Price:        2000,
		Size:         0.02,
		Value:        40,
		Timestamp:    time.Now(),
	}})

	select {
	case fullDec := <-e.decisionCh:
		if len(fullDec.Decisions) != 1 {
			t.Fatalf("decision len=%d want 1", len(fullDec.Decisions))
		}
		dec := fullDec.Decisions[0]
		if dec.Action != "close_long" {
			t.Fatalf("expected close_long, got %+v", dec)
		}
		if dec.PositionSizeUSD != 0 {
			t.Fatalf("close must not carry open position size, got %.8f", dec.PositionSizeUSD)
		}
		if dec.Leverage != 0 {
			t.Fatalf("close must not require leverage, got %dx", dec.Leverage)
		}
	default:
		t.Fatalf("expected close decision")
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

// TestBinanceSnapshotFillsDeduplicateAcrossPolls 验证修复 A：
//
// 场景重现（"大爷的弟弟"日志）：
//   - 领航员 ETHUSDT_SHORT 已平仓（leaderState 中无该 posId）
//   - 跟随者本地 active mapping 仍指向该 posId（历史遗留）
//   - 引擎每 3 秒 poll 一次，每次 detectBinancePositionSnapshotFills 都
//     生成相同 fill.ID 的 close 信号（"binance_snapshot|posId|close|prev|0"）
//
// 修复前：每次 poll 都触发决策，下游 OKX/Binance trader 报 "position not found"
// 修复后：第一次 poll 后 fill.ID 被 markSeen，第二次起被 isSeen 过滤，决策不再生成
func TestBinanceSnapshotFillsDeduplicateAcrossPolls(t *testing.T) {
	const posID = "1243719130_ETHUSDT_SHORT"

	e, st := newTestCopyTradeEngine(t, ProviderBinance)

	// seed: 历史遗留的 active mapping（领航员持仓中没有该 posId）
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:      "test-trader",
		LeaderPosID:   posID,
		LeaderID:      "leader",
		Symbol:        "ETHUSDT",
		Side:          string(SideShort),
		MarginMode:    "cross",
		OpenedAt:      time.Now(),
		OpenPrice:     2062.21,
		LastKnownSize: 0.047,
	}); err != nil {
		t.Fatalf("seed active mapping: %v", err)
	}

	// provider 始终返回空持仓 + 空成交（模拟领航员已平仓）
	e.provider = &binancePollTestProvider{
		fills: nil,
		state: &AccountState{
			TotalEquity: 1000,
			Positions:   map[string]*Position{},
		},
	}

	// 第一次 poll：应该产生 1 条 silent close 决策
	e.poll()

	select {
	case dec := <-e.decisionCh:
		if len(dec.Decisions) != 1 {
			t.Fatalf("first poll: decision len=%d want 1", len(dec.Decisions))
		}
		if got := dec.Decisions[0]; got.Action != "close_short" || got.LeaderPosID != posID {
			t.Fatalf("first poll: unexpected decision: %+v", got)
		}
	default:
		t.Fatalf("first poll: expected close decision but got none")
	}

	// 第二/三次 poll：snapshot fill.ID 完全相同，应该被 isSeen 拦截
	for i := 2; i <= 4; i++ {
		e.poll()
		select {
		case dec := <-e.decisionCh:
			t.Fatalf("poll #%d: snapshot 应被去重，但仍生成决策: %+v", i, dec.Decisions)
		default:
			// 期望路径：无新决策
		}
	}
}

// TestAddBelowMinThresholdIsSkippedNotBoosted 验证修复 D：
// 加仓时（match.Action == ActionAdd）若 copySize < minTradeThreshold，
// 应返回 0 且发出 "add_below_threshold_skip" 警告，而不是被强制提升到阈值。
//
// 复现场景：领航员持续小额加仓 0.5 USDT，旧逻辑每次都被 boost 到 12 USDT，
// 跟随者仓位累积速度远高于领航员，破坏"按比例镜像"语义。
func TestAddBelowMinThresholdIsSkippedNotBoosted(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderOKX)
	e.config.MinTradeWarn = 12.0
	e.config.CopyRatio = 1.0
	// 跟随者权益小（100 USDT），领航员小额加仓 → 计算出的 copySize 必然 < 12
	e.getFollowerBalance = func() float64 { return 100 }

	signal := &TradeSignal{
		ProviderType: ProviderOKX,
		Fill: &Fill{
			Symbol:       "ETHUSDT",
			PositionSide: SideLong,
			Action:       ActionAdd,
			Price:        2000,
			Size:         0.0001,
			Value:        0.5, // 领航员加仓 0.5 USDT
		},
		LeaderEquity: 1000,
		LeaderPosition: &Position{
			Symbol:        "ETHUSDT",
			Side:          SideLong,
			Size:          0.0101, // 当前持仓
			EntryPrice:    2000,
			PositionValue: 20.2,
		},
	}
	match := &SignalMatchResult{
		Action:         ActionAdd,
		LeaderPosition: signal.LeaderPosition,
	}

	copySize, warnings := e.calculateCopySizeByPositionChange(signal, match)

	if copySize != 0 {
		t.Fatalf("ActionAdd 不足阈值应跳过 (copySize=0)，实际 copySize=%.4f", copySize)
	}

	found := false
	for _, w := range warnings {
		if w.Type == "add_below_threshold_skip" {
			found = true
			if w.Executed {
				t.Fatalf("add_below_threshold_skip 警告应 Executed=false")
			}
		}
	}
	if !found {
		t.Fatalf("期望 add_below_threshold_skip 警告，实际: %+v", warnings)
	}
}

// TestOpenBelowMinThresholdStillBoosted 验证修复 D 的"反向"：
// 开仓时（match.Action == ActionOpen）若 copySize < minTradeThreshold，
// 仍然要 boost 到阈值（开仓只发生一次，必须达到交易所最小起步金额）。
func TestOpenBelowMinThresholdStillBoosted(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderOKX)
	e.config.MinTradeWarn = 12.0
	e.config.CopyRatio = 1.0
	e.getFollowerBalance = func() float64 { return 100 }

	signal := &TradeSignal{
		ProviderType: ProviderOKX,
		Fill: &Fill{
			Symbol:       "ETHUSDT",
			PositionSide: SideLong,
			Action:       ActionOpen,
			Price:        2000,
			Size:         0.0001,
			Value:        0.5,
		},
		LeaderEquity: 1000,
	}
	match := &SignalMatchResult{Action: ActionOpen}

	copySize, warnings := e.calculateCopySizeByPositionChange(signal, match)

	if copySize < 11.99 || copySize > 12.01 {
		t.Fatalf("ActionOpen 不足阈值应 boost 到 12，实际 copySize=%.4f", copySize)
	}

	foundBoost := false
	foundSkip := false
	for _, w := range warnings {
		if w.Type == "size_boosted" {
			foundBoost = true
			if !w.Executed {
				t.Fatalf("size_boosted 警告应 Executed=true")
			}
		}
		if w.Type == "add_below_threshold_skip" {
			foundSkip = true
		}
	}
	if !foundBoost {
		t.Fatalf("ActionOpen 期望 size_boosted 警告，实际: %+v", warnings)
	}
	if foundSkip {
		t.Fatalf("ActionOpen 不应产生 add_below_threshold_skip 警告")
	}
}

// TestProcessSignalSkipsAddWhenBelowThreshold 端到端验证修复 D：
// 领航员小额加仓 → poll 路径不应推送决策、不应更新 last_known_size。
func TestProcessSignalSkipsAddWhenBelowThreshold(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"

	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.MinTradeWarn = 12.0
	e.config.CopyRatio = 1.0
	e.getFollowerBalance = func() float64 { return 100 }

	// seed active mapping with lastKnownSize=0.01
	saveActiveMapping(t, st, posID, 0.01)

	// 模拟领航员 size 从 0.01 → 0.0101（加仓 ~0.5 USDT）
	e.provider = &binancePollTestProvider{
		state: &AccountState{
			TotalEquity: 1000,
			Positions: map[string]*Position{
				posID: binanceTestPosition(posID, 0.0101),
			},
		},
	}

	e.poll()

	// 预期：无决策推送
	select {
	case dec := <-e.decisionCh:
		t.Fatalf("加仓金额不足应跳过决策，但收到: %+v", dec.Decisions)
	default:
	}

	// 验证 last_known_size 未被更新（仍为 0.01，不是 0.0101）
	mapping, err := st.CopyTrade().GetActiveMapping("test-trader", posID)
	if err != nil || mapping == nil {
		t.Fatalf("mapping 应仍 active: err=%v mapping=%+v", err, mapping)
	}
	if mapping.LastKnownSize != 0.01 {
		t.Fatalf("加仓跳过后 LastKnownSize 不应更新，实际=%f want 0.01", mapping.LastKnownSize)
	}
}

// TestBinanceSnapshotDifferentSizeStillTriggers 验证修复 A 的"反向"：
// 当 size 真正变化（previousSize 不同 → fill.ID 不同）时，仍然必须触发新决策。
// 防止"过度去重"误杀真实减仓/加仓信号。
func TestBinanceSnapshotDifferentSizeStillTriggers(t *testing.T) {
	const posID = "1239518824_ETHUSDT_LONG"

	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	saveActiveMapping(t, st, posID, 0.02)

	provider := &binancePollTestProvider{
		state: &AccountState{
			TotalEquity: 1000,
			Positions: map[string]*Position{
				posID: binanceTestPosition(posID, 0.01), // 减仓：0.02 → 0.01
			},
		},
	}
	e.provider = provider

	// 第一次 poll：应该产生 reduce 决策
	e.poll()
	select {
	case dec := <-e.decisionCh:
		if got := dec.Decisions[0]; got.Action != "reduce_long" {
			t.Fatalf("expected reduce_long, got %+v", got)
		}
	default:
		t.Fatalf("expected reduce decision")
	}

	// 改变持仓再 poll：fill.ID 因 previousSize/currentSize 变化而不同 → 应再次触发
	// 注：实际 LastKnownSize 在 trader 执行成功后才会更新，此处只验证 ID 变化时去重不误伤
	provider.state.Positions[posID] = binanceTestPosition(posID, 0.005) // 继续减
	// 第二次 poll 因为 mapping.LastKnownSize 还是 0.02（执行链路在此 mock 不更新），
	// 但 currentSize 已从 0.01 变为 0.005 → fill.ID 不同 → 应再次产生决策
	e.poll()
	select {
	case <-e.decisionCh:
		// 期望路径：新 size 产生新 fill.ID，未被去重
	default:
		t.Fatalf("size 变化后 fill.ID 应该不同，期望新决策但未收到")
	}
}
