package copytrade

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/notifier"
	"nofx/store"
)

type fcPollTestProvider struct {
	state      *AccountState
	stateErr   error
	fillsCalls int
}

func (p *fcPollTestProvider) GetFills(string, time.Time) ([]Fill, error) {
	p.fillsCalls++
	return nil, nil
}
func (p *fcPollTestProvider) GetAccountState(id string) (*AccountState, error) {
	return p.GetPositionSnapshot(id)
}
func (p *fcPollTestProvider) GetPositionSnapshot(string) (*AccountState, error) {
	if p.stateErr != nil {
		return nil, p.stateErr
	}
	return cloneLeaderSnapshot(p.state), nil
}
func (p *fcPollTestProvider) Type() ProviderType                    { return ProviderFollowCoin }
func (p *fcPollTestProvider) UseAuthoritativeSnapshotHotPath() bool { return true }

func fcTestPosition(symbol string, side SideType, size, mark float64, openedMS int64) *Position {
	inst := strings.TrimSuffix(symbol, "USDT") + "-USDT-SWAP"
	return &Position{
		PosID: fmt.Sprintf("fc_57_%s_%s", inst, side), Symbol: symbol, Side: side, Size: size,
		EntryPrice: mark, MarkPrice: mark, Leverage: 10, MarginMode: "cross",
		PositionValue: size * mark, RawPositionValue: size * mark, ValueUSDValid: true,
		ValueCurrency: "USD", OpenedMS: openedMS,
	}
}

func TestFollowCoinPollOpensFromSnapshotWithProportionalSize(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderFollowCoin)
	e.config.LeaderID = "57"
	e.config.MinTradeWarn = 1
	pos := fcTestPosition("BTCUSDT", SideLong, 0.03, 2000, 1794694989778)
	provider := &fcPollTestProvider{state: &AccountState{
		TotalEquity: 1000, Timestamp: time.Now(), Positions: map[string]*Position{pos.PosID: pos},
	}}
	e.provider = provider

	e.poll()
	if provider.fillsCalls != 0 {
		t.Fatalf("snapshot hot path must not read fills: %d", provider.fillsCalls)
	}
	select {
	case full := <-e.decisionCh:
		d := full.Decisions[0]
		if d.Action != "open_long" || d.LeaderPosID != pos.PosID || d.LeaderPosSize != 0.03 {
			t.Fatalf("unexpected decision: %+v", d)
		}
		if !strings.HasPrefix(d.SourceFillID, "followcoin_snapshot|r0|"+pos.PosID+"|open|") {
			t.Fatalf("snapshot identity must carry the revision: %s", d.SourceFillID)
		}
		// 跟单比例不变：0.03 × 2000 / 1000 × 跟随者权益 100 × 系数 1 = 6
		if math.Abs(d.PositionSizeUSD-6) > 1e-9 {
			t.Fatalf("proportional sizing changed: %.8f", d.PositionSizeUSD)
		}
	default:
		t.Fatal("expected FollowCoin open decision")
	}
	e.poll()
	select {
	case extra := <-e.decisionCh:
		t.Fatalf("unchanged snapshot produced a duplicate: %+v", extra.Decisions)
	default:
	}
}

func TestFollowCoinUnvaluedOpenFailsClosed(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderFollowCoin)
	pos := fcTestPosition("TACUSDT", SideShort, 100, 0, 1)
	pos.MarkPrice, pos.PositionValue, pos.ValueUSDValid = 0, 0, false
	fill := e.buildBinanceSnapshotFillForPosition(pos, pos.PosID, ActionOpen, pos.Size, 0, pos.Size, 0)
	if fill.ValueUSDValid || !e.blockInvalidSourceRiskIncrease(&TradeSignal{Fill: &fill}, &SignalMatchResult{Action: ActionOpen, PosID: pos.PosID}) {
		t.Fatalf("open without mark price must be blocked: %+v", fill)
	}
	if e.blockInvalidSourceRiskIncrease(&TradeSignal{Fill: &fill}, &SignalMatchResult{Action: ActionClose, PosID: pos.PosID}) {
		t.Fatal("exits are never blocked by value checks")
	}
}

func TestFollowCoinEmptySnapshotNeedsConfirmationBeforeClosingFollowers(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
	saveActiveMapping(t, st, "fc_57_ETH-USDT-SWAP_long", 0.02)
	empty := &AccountState{TotalEquity: 1000, Positions: map[string]*Position{}}
	now := time.Now()
	if err := e.confirmSmartMoneyEmptySnapshot(empty, now); err != ErrFollowCoinEmptyUnconfirmed {
		t.Fatalf("first empty view must wait for confirmation: %v", err)
	}
	if err := e.confirmSmartMoneyEmptySnapshot(empty, now.Add(10*time.Second)); err != ErrFollowCoinEmptyUnconfirmed {
		t.Fatalf("empty view must persist for the confirmation window: %v", err)
	}
	if err := e.confirmSmartMoneyEmptySnapshot(empty, now.Add(followCoinEmptyConfirmDelay)); err != nil {
		t.Fatalf("persistent empty view is accepted: %v", err)
	}

	// A position reappearing resets the candidate.
	e.smartEmptySnapshotCandidateAt = time.Time{}
	_ = e.confirmSmartMoneyEmptySnapshot(empty, now)
	held := &AccountState{Positions: map[string]*Position{"x": fcTestPosition("ETHUSDT", SideLong, 1, 1, 1)}}
	if err := e.confirmSmartMoneyEmptySnapshot(held, now.Add(time.Second)); err != nil || !e.smartEmptySnapshotCandidateAt.IsZero() {
		t.Fatalf("non-empty snapshot must clear the candidate: %v", err)
	}
}

func TestFollowCoinIgnoredPositionIsNotFollowedWhileHeld(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
	pos := fcTestPosition("ETHUSDT", SideLong, 0.02, 2000, 1000)
	if err := st.CopyTrade().SaveIgnoredPosition(e.traderID, e.config.LeaderID, pos.PosID, pos.Symbol, string(pos.Side), "cross"); err != nil {
		t.Fatal(err)
	}
	e.leaderState.Positions[pos.PosID] = pos
	if fills := e.detectBinancePositionSnapshotFills(); len(fills) != 0 {
		t.Fatalf("pre-existing leader position must not be followed: %+v", fills)
	}
	signal := e.buildSignal(&Fill{ID: "late", Symbol: pos.Symbol, PositionSide: SideLong, Action: ActionOpen, LeaderPosID: pos.PosID, Price: 2000, Size: 0.02})
	if res := e.matchOpenAddSignal(signal, e.buildLeaderPosMap()); res.ShouldFollow {
		t.Fatalf("ignored FollowCoin position followed: %+v", res)
	}
}

func TestFollowCoinPollFailsClosedOnInvalidToken(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
	saveActiveMapping(t, st, "fc_57_ETH-USDT-SWAP_long", 0.02)
	e.provider = &fcPollTestProvider{stateErr: fmt.Errorf("wrapped: %w", ErrFollowCoinTokenInvalid)}
	e.poll()
	select {
	case got := <-e.decisionCh:
		t.Fatalf("invalid token must not produce signals from cached state: %+v", got.Decisions)
	default:
	}
	if len(e.seenFills) != 0 {
		t.Fatalf("failed round must not acknowledge transitions: %+v", e.seenFills)
	}
}

func captureFollowCoinMail(t *testing.T) *[]notifier.Alert {
	t.Helper()
	var sent []notifier.Alert
	prev := followCoinNotify
	followCoinNotify = func(a notifier.Alert) { sent = append(sent, a) }
	prevUnconfigured := followCoinUnconfiguredAlertAt
	t.Cleanup(func() { followCoinNotify, followCoinUnconfiguredAlertAt = prev, prevUnconfigured })
	return &sent
}

func TestFollowCoinCredentialMonitorAlertsRemindsAndRecovers(t *testing.T) {
	_, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
	sent := captureFollowCoinMail(t)
	if err := st.FollowCoinCreds().Set("", "not.a.jwt"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	get := func() *store.FollowCoinCredentials {
		c, err := st.FollowCoinCreds().Get("")
		if err != nil || c == nil {
			t.Fatalf("credentials: %v", err)
		}
		return c
	}

	observeFollowCoinCredentialState(st, true, "code=20003", now)
	if c := get(); c.LastStatus != store.FollowCoinCredsStatusExpired || len(*sent) != 1 || !strings.Contains((*sent)[0].Title, "已失效") {
		t.Fatalf("first failure must mark expired and mail at once: status=%s mails=%d", c.LastStatus, len(*sent))
	}
	observeFollowCoinCredentialState(st, true, "code=20003", now.Add(time.Hour))
	if len(*sent) != 1 {
		t.Fatalf("no reminder before 6h: %d", len(*sent))
	}
	observeFollowCoinCredentialState(st, true, "code=20003", now.Add(followCoinFailureReminderInterval+time.Minute))
	if len(*sent) != 2 || !strings.Contains((*sent)[1].Title, "提醒") {
		t.Fatalf("reminder after 6h expected: %+v", *sent)
	}
	observeFollowCoinCredentialState(st, false, "", now.Add(7*time.Hour))
	if c := get(); c.LastStatus != store.FollowCoinCredsStatusValid || len(*sent) != 3 || !strings.Contains((*sent)[2].Title, "已恢复") {
		t.Fatalf("recovery must mark valid and mail once: status=%s mails=%d", c.LastStatus, len(*sent))
	}
	observeFollowCoinCredentialState(st, false, "", now.Add(8*time.Hour))
	if len(*sent) != 3 {
		t.Fatalf("healthy polls must stay silent: %d", len(*sent))
	}
}

func TestFollowCoinCredentialMonitorWarnsBeforeJWTExpiry(t *testing.T) {
	_, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
	sent := captureFollowCoinMail(t)
	exp := time.Now().Add(48 * time.Hour).Unix()
	token := "eyJhbGciOiJIUzI1NiJ9." + encodeTestJWTPayload(fmt.Sprintf(`{"exp":%d}`, exp)) + ".sig"
	if err := st.FollowCoinCreds().Set("", token); err != nil {
		t.Fatal(err)
	}
	c, _ := st.FollowCoinCreds().Get("")
	if c.ExpiresAt.Unix() != exp {
		t.Fatalf("JWT exp not parsed: %v", c.ExpiresAt)
	}
	now := time.Now()
	observeFollowCoinCredentialState(st, false, "", now)
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Title, "即将到期") {
		t.Fatalf("expiry within 3 days must warn: %+v", *sent)
	}
	observeFollowCoinCredentialState(st, false, "", now.Add(time.Hour))
	if len(*sent) != 1 {
		t.Fatalf("at most one expiry warning per day: %d", len(*sent))
	}
	observeFollowCoinCredentialState(st, false, "", now.Add(followCoinExpiryWarnInterval+time.Minute))
	if len(*sent) != 2 {
		t.Fatalf("daily expiry reminder expected: %d", len(*sent))
	}
}

func encodeTestJWTPayload(payload string) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(payload)), "=")
}

func fcState(positions ...*Position) *AccountState {
	m := map[string]*Position{}
	for _, p := range positions {
		m[p.PosID] = p
	}
	return &AccountState{TotalEquity: 1000, Timestamp: time.Now(), Positions: m}
}

func drainDecisions(e *Engine) []string {
	var out []string
	for {
		select {
		case full := <-e.decisionCh:
			for _, d := range full.Decisions {
				out = append(out, fmt.Sprintf("%s:%s:%s", d.CopyTradeAction, d.Action, d.LeaderPosID))
			}
		default:
			return out
		}
	}
}

// 用户要求：启动时已有的仓位（以及它们之后的加仓/减仓）一律不跟，只跟新开仓。
func TestFollowCoinOnlyFollowsPositionsOpenedAfterStart(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
	e.config.LeaderID = "57"
	e.config.MinTradeWarn = 1
	const openedAt = 1794694989778
	btc := fcTestPosition("BTCUSDT", SideLong, 1, 60000, openedAt)
	provider := &fcPollTestProvider{state: fcState(btc)}
	e.provider = provider

	if err := e.InitIgnoredPositions(); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.CopyTrade().GetMapping(e.traderID, btc.PosID); m == nil || m.Status != "ignored" {
		t.Fatalf("pre-existing position must be baselined as ignored: %+v", m)
	}

	// Leader adds to and then reduces the historical position (ctime unchanged).
	for _, size := range []float64{1.5, 0.5} {
		provider.state = fcState(fcTestPosition("BTCUSDT", SideLong, size, 60000, openedAt))
		e.poll()
		if got := drainDecisions(e); len(got) != 0 {
			t.Fatalf("add/reduce on a historical position must be skipped (size=%v): %v", size, got)
		}
	}

	// A genuinely new position is followed as an open.
	eth := fcTestPosition("ETHUSDT", SideShort, 2, 2000, openedAt+3600_000)
	provider.state = fcState(fcTestPosition("BTCUSDT", SideLong, 0.5, 60000, openedAt), eth)
	e.poll()
	if got := drainDecisions(e); len(got) != 1 || got[0] != "open:open_short:"+eth.PosID {
		t.Fatalf("new position must be followed as an open: %v", got)
	}

	// The historical position closes, then the leader opens it again: a new lifecycle.
	provider.state = fcState(eth)
	e.poll()
	if got := drainDecisions(e); len(got) != 0 {
		t.Fatalf("closing a never-followed position must not trade: %v", got)
	}
	reopened := fcTestPosition("BTCUSDT", SideLong, 0.2, 60000, openedAt+7200_000)
	provider.state = fcState(eth, reopened)
	e.poll()
	got := drainDecisions(e)
	if len(got) != 1 || got[0] != "open:open_long:"+reopened.PosID {
		t.Fatalf("re-opened position after a full close is a new open: %v", got)
	}
}

// 已跟随的仓位加仓必须是 add（按增量下单），绝不能当成新开仓按整仓再开一次。
func TestFollowCoinAddToFollowedPositionIsAddNotOpen(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderFollowCoin)
	e.config.LeaderID = "57"
	e.config.MinTradeWarn = 1
	const openedAt = 1794694989778
	eth := fcTestPosition("ETHUSDT", SideShort, 3, 2000, openedAt)
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: e.traderID, LeaderPosID: eth.PosID, LeaderID: "57", Symbol: "ETHUSDT",
		Side: string(SideShort), MarginMode: "cross", OpenedAt: time.Now(), OpenPrice: 2000,
		OpenSizeUSD: 4000, LastKnownSize: 2,
	}); err != nil {
		t.Fatal(err)
	}
	e.provider = &fcPollTestProvider{state: fcState(eth)}
	e.poll()
	select {
	case full := <-e.decisionCh:
		d := full.Decisions[0]
		if d.CopyTradeAction != "add" || d.LeaderPosID != eth.PosID || d.LeaderPosSize != 3 {
			t.Fatalf("size growth on a followed position must be an add: %+v", d)
		}
		// 只按增量 1 × 2000 = 2000 计价：2000/1000 × 跟随者权益 100 = 200，而不是整仓 6000 → 600。
		if math.Abs(d.PositionSizeUSD-200) > 1e-9 {
			t.Fatalf("add must size the delta only, got %.4f", d.PositionSizeUSD)
		}
	default:
		t.Fatal("expected an add decision")
	}
}

// 启动时拿不到领航员持仓（token 失效/网络故障）不能放行：否则已有仓位会在
// 第一次成功读取时被当成新开仓跟进。FollowCoin 必须拒绝启动。
func TestFollowCoinStartupBaselineFailureRefusesStart(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderFollowCoin)
	e.provider = &fcPollTestProvider{stateErr: ErrFollowCoinTokenInvalid}
	if err := e.InitIgnoredPositions(); err == nil {
		t.Fatal("baseline read failure must be reported")
	}
	if !e.requiresStartupBaseline() {
		t.Fatal("FollowCoin must refuse to start without a startup baseline")
	}
	okx, _ := newTestCopyTradeEngine(t, ProviderOKX)
	if okx.requiresStartupBaseline() {
		t.Fatal("OKX startup behaviour must stay unchanged")
	}
	// Binance copy management is rate limited: a throttled first read must
	// not let pre-existing positions be copied as new opens later.
	binance, _ := newTestCopyTradeEngine(t, ProviderBinance)
	if !binance.requiresStartupBaseline() {
		t.Fatal("Binance copy management must refuse to start without a startup baseline")
	}
}
