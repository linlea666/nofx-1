package copytrade

// 第二批漏单/幂等修复的回归测试：
//   - S2 OKX 分页截断 fail-closed（同毫秒满页停滞 / 页数硬顶用尽）
//   - S3 copyPortfolioID 绑定 60s TTL + stale 降级
//   - S4 Provider 指纹去重降级为单次调用内去重（跨轮去重收敛到引擎）
//   - S9 STOP_PARTIAL 残仓重试耗尽后升级 GUARD_UNPROTECTABLE 处置

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader"
)

// ---------------------------------------------------------------------------
// S2: OKX GetFills 分页 fail-closed
// ---------------------------------------------------------------------------

type okxFillsRoundTripper struct {
	calls int
	page  func(call int) OKXTradeRecordsResp
}

func (rt *okxFillsRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls++
	body, _ := json.Marshal(rt.page(rt.calls))
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func okxTestRecord(ordID string, fillTimeMs int64) OKXTradeRecord {
	return OKXTradeRecord{
		AvgPx: "100", FillTime: strconv.FormatInt(fillTimeMs, 10), InstId: "ETH-USDT-SWAP",
		OrdId: ordID, Side: "buy", PosSide: "long", Sz: "1", Value: "100",
	}
}

func TestOKXGetFillsFailsClosedOnSameMillisecondFullPageStall(t *testing.T) {
	future := time.Now().Add(10 * time.Minute).UnixMilli()
	rt := &okxFillsRoundTripper{page: func(call int) OKXTradeRecordsResp {
		records := make([]OKXTradeRecord, okxFillsPageLimit)
		for i := range records {
			// 同一毫秒且不早于右边界：右边界无法前移，分页停滞
			records[i] = okxTestRecord(fmt.Sprintf("stall-%d", i), future)
		}
		return OKXTradeRecordsResp{Code: "0", Data: records}
	}}
	p := &OKXProvider{client: &http.Client{Transport: rt}}

	fills, err := p.GetFills("leader", time.Now().Add(-5*time.Minute))
	if err == nil {
		t.Fatalf("stalled full-page pagination must fail closed, got %d fills", len(fills))
	}
	if rt.calls != 1 {
		t.Fatalf("stall must be detected on the first page: calls=%d", rt.calls)
	}
}

func TestOKXGetFillsFailsClosedWhenPageCapStillFull(t *testing.T) {
	base := time.Now().UnixMilli()
	rt := &okxFillsRoundTripper{page: func(call int) OKXTradeRecordsResp {
		records := make([]OKXTradeRecord, okxFillsPageLimit)
		for i := range records {
			// 每页时间前移 1s 但始终没到窗口左边界；页数硬顶后仍是满页
			records[i] = okxTestRecord(fmt.Sprintf("cap-%d-%d", call, i), base-int64(call)*1000)
		}
		return OKXTradeRecordsResp{Code: "0", Data: records}
	}}
	p := &OKXProvider{client: &http.Client{Transport: rt}}

	fills, err := p.GetFills("leader", time.Now().Add(-5*time.Minute))
	if err == nil {
		t.Fatalf("exceeding the page cap with full pages must fail closed, got %d fills", len(fills))
	}
	if rt.calls != okxFillsMaxPages {
		t.Fatalf("must stop at the page cap: calls=%d", rt.calls)
	}
}

func TestOKXGetFillsReturnsCompleteWindow(t *testing.T) {
	now := time.Now().UnixMilli()
	rt := &okxFillsRoundTripper{page: func(call int) OKXTradeRecordsResp {
		return OKXTradeRecordsResp{Code: "0", Data: []OKXTradeRecord{
			okxTestRecord("a", now-1000),
			okxTestRecord("b", now-2000),
		}}
	}}
	p := &OKXProvider{client: &http.Client{Transport: rt}}

	fills, err := p.GetFills("leader", time.Now().Add(-5*time.Minute))
	if err != nil || len(fills) != 2 {
		t.Fatalf("partial page completes the window: fills=%d err=%v", len(fills), err)
	}
}

// ---------------------------------------------------------------------------
// S3/S4: Binance copyPortfolioId TTL + Provider 去重收敛到引擎
// ---------------------------------------------------------------------------

type binanceWebRoundTripper struct {
	cpid       string
	detailFail bool
	trades     []BinanceTradeRecord
}

func (rt *binanceWebRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	respond := func(status int, payload interface{}) (*http.Response, error) {
		body, _ := json.Marshal(payload)
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	}
	switch {
	case strings.Contains(req.URL.Path, "lead-portfolio/detail"):
		if rt.detailFail {
			return respond(http.StatusInternalServerError, map[string]string{"error": "boom"})
		}
		return respond(http.StatusOK, BinanceLeadPortfolioDetailResp{
			Code: BinanceCodeSuccess, Success: true,
			Data: BinanceLeadPortfolioDetailData{LeadPortfolioID: "lead-1", CopyPortfolioID: rt.cpid, HasCopy: true, MarginBalance: "1000"},
		})
	case strings.Contains(req.URL.Path, "trade-history"):
		return respond(http.StatusOK, BinanceTradeHistoryResp{
			Code: BinanceCodeSuccess, Success: true,
			Data: BinanceTradeHistoryData{Total: len(rt.trades), List: rt.trades},
		})
	default:
		return respond(http.StatusOK, map[string]string{"code": BinanceCodeSuccess})
	}
}

func TestResolveCopyPortfolioIDRefreshesAfterTTLAndFallsBackToStale(t *testing.T) {
	rt := &binanceWebRoundTripper{cpid: "cpid-a"}
	p := NewBinanceProvider("p20t", "csrf")
	p.client = &http.Client{Transport: rt}

	got, err := p.resolveCopyPortfolioID("lead-1")
	if err != nil || got != "cpid-a" {
		t.Fatalf("first resolve: got=%q err=%v", got, err)
	}

	// TTL 内不刷新：用户重建关系前的旧值直接命中
	rt.cpid = "cpid-b"
	if got, _ = p.resolveCopyPortfolioID("lead-1"); got != "cpid-a" {
		t.Fatalf("within TTL must serve cache: %q", got)
	}

	// TTL 过期后必须刷新拿到新关系 ID
	p.mu.Lock()
	p.cpidFetchedAt = time.Now().Add(-2 * binanceCopyDetailTTL)
	p.mu.Unlock()
	if got, err = p.resolveCopyPortfolioID("lead-1"); err != nil || got != "cpid-b" {
		t.Fatalf("expired TTL must refresh: got=%q err=%v", got, err)
	}

	// 刷新失败沿用旧值降级（stale 模式），不得整体失败
	p.mu.Lock()
	p.cpidFetchedAt = time.Now().Add(-2 * binanceCopyDetailTTL)
	p.mu.Unlock()
	rt.detailFail = true
	if got, err = p.resolveCopyPortfolioID("lead-1"); err != nil || got != "cpid-b" {
		t.Fatalf("transient refresh failure must fall back to stale cpid: got=%q err=%v", got, err)
	}
}

func TestBinanceGetFillsKeepsFillsVisibleAcrossPollsForEngineReplay(t *testing.T) {
	record := BinanceTradeRecord{
		Time: time.Now().UnixMilli(), Symbol: "ETHUSDT", Side: "BUY", Price: 100,
		Quantity: 100, Qty: 1, PositionSide: "LONG",
	}
	// 同一响应内的重复记录仍需去重（单次调用内）
	rt := &binanceWebRoundTripper{cpid: "cpid-a", trades: []BinanceTradeRecord{record, record}}
	p := NewBinanceProvider("p20t", "csrf")
	p.client = &http.Client{Transport: rt}
	since := time.Now().Add(-5 * time.Minute)

	first, err := p.GetFills("lead-1", since)
	if err != nil || len(first) != 1 {
		t.Fatalf("first poll: fills=%d err=%v", len(first), err)
	}
	// 跨轮不得吞掉同一成交：引擎 UnmarkSeen 重放依赖 fill 在后续 poll 仍可拉到
	second, err := p.GetFills("lead-1", since)
	if err != nil || len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("second poll must return the same fill for engine-level dedup/replay: fills=%d err=%v", len(second), err)
	}
}

// ---------------------------------------------------------------------------
// S9: STOP_PARTIAL 残仓强平耗尽后升级 GUARD_UNPROTECTABLE 处置
// ---------------------------------------------------------------------------

func TestStopPartialEscalatesToUnprotectableAfterRetriesExhausted(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "stop-partial-escape.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mock := &mockStopMgr{byID: map[string]*trader.ProtectiveStopOrder{
		"stop-algo": {AlgoID: "stop-algo", PositionSide: "long", MarginMode: "cross", Quantity: .05, TriggerPrice: 98, State: "effective"},
	}}
	executor := &stopMgrExecutor{mockStopMgr: mock, positions: []map[string]interface{}{
		{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "positionAmt": .01, "entryPrice": 100},
	}}
	ti := NewTraderIntegration("trader-1", executor, st)
	ti.engine = &Engine{traderID: "trader-1", config: &CopyConfig{ProviderType: ProviderOKX, RiskPolicyVersion: 4, RiskATRTimeframe: "unsupported", RiskATRPeriod: 14, RiskATRFallbackPct: .02}, store: st, leaderState: &AccountState{Positions: map[string]*Position{}}}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "leader-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardStopPartial, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 100, FollowerNotional: 5, AccountEquity: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 100, 5, .05, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: cycle.ID, TraderID: "trader-1", AlgoID: "stop-algo", AlgoClientID: "cg", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: .05, TriggerPrice: 98, TriggerType: "mark", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	// 模拟 3 次 reduce-only 强平已耗尽且冷却期已过
	ti.residualCloseTries[cycle.ID] = 3
	ti.residualCloseLast[cycle.ID] = time.Now().Add(-2 * unprotectableRecheckDelay)

	ti.pollV4ProtectiveStops()

	if executor.closeCalls != 1 {
		t.Fatalf("escalation must issue exactly one forced exit: %d", executor.closeCalls)
	}
	got, err := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.CopyGuardStoppedWatching {
		t.Fatalf("exhausted residual retries must escape STOP_PARTIAL via unprotectable handling: %+v", got.Status)
	}
}
