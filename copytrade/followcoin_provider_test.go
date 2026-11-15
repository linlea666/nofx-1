package copytrade

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Fixtures follow the real api-v2.cryptostrategyget.com response shapes.
const fcPositionsFixture = `{"ok":true,"code":"200","msg":"请求成功","data":[
 {"marginCoin":"USDT","symbol":"TAC","instId":"TAC-USDT-SWAP","instType":"SWAP","holdSide":"short","total":8151038.0,"maxTotal":null,"leverage":10.0,"achievedProfits":2.12,"openPriceAvg":0.001221,"marginSize":1062.89,"marginMode":"cross","unrealizedPL":-676.5,"uplRatio":-0.63,"liquidationPrice":0.0064,"marketPrice":0.001304,"totalFee":7.1,"exchangeId":1,"ctime":1794751926215,"utime":1794767408000},
 {"marginCoin":"USDT","symbol":"BTC","instId":"BTC-USDT-SWAP","instType":"SWAP","holdSide":"long","total":0.019,"maxTotal":null,"leverage":100.0,"achievedProfits":-0.69,"openPriceAvg":64053.55,"marginSize":12.25,"marginMode":"cross","unrealizedPL":8.79,"uplRatio":0.71,"liquidationPrice":41423.18,"marketPrice":64516.3,"totalFee":-0.08,"exchangeId":1,"ctime":1794694989778,"utime":1794767408000}
]}`

const fcOverviewFixture = `{"ok":true,"code":"200","msg":"请求成功","data":{"apiId":57,"exchangeId":1,"totalEquity":35514.86,"availableBalance":35514.86}}`

type fcFakeServer struct {
	positions, overview, accounts, userInfo string
	positionCalls, overviewCalls            atomic.Int32
	lastToken                               atomic.Value
}

func (f *fcFakeServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.lastToken.Store(r.Header.Get("token"))
		switch r.URL.Path {
		case followCoinPositionsPath:
			f.positionCalls.Add(1)
			fmt.Fprint(w, f.positions)
		case followCoinOverviewPath:
			f.overviewCalls.Add(1)
			fmt.Fprint(w, f.overview)
		case followCoinAccountsPath:
			fmt.Fprint(w, f.accounts)
		case followCoinUserInfoPath:
			fmt.Fprint(w, f.userInfo)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newFCTestProvider(t *testing.T, f *fcFakeServer) *FollowCoinProvider {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	p := NewFollowCoinProviderWithToken("test-token")
	p.client = srv.Client()
	p.baseURL = srv.URL
	return p
}

func TestFollowCoinSnapshotMapsRealResponse(t *testing.T) {
	f := &fcFakeServer{positions: fcPositionsFixture, overview: fcOverviewFixture}
	p := newFCTestProvider(t, f)
	s, err := p.GetPositionSnapshot("57")
	if err != nil {
		t.Fatal(err)
	}
	if f.lastToken.Load() != "test-token" {
		t.Fatalf("token header not sent: %v", f.lastToken.Load())
	}
	tac := s.Positions["fc_57_TAC-USDT-SWAP_short"]
	if tac == nil || tac.Symbol != "TACUSDT" || tac.Side != SideShort || tac.Size != 8151038 ||
		tac.Leverage != 10 || tac.MarginMode != "cross" || tac.OpenedMS != 1794751926215 {
		t.Fatalf("TAC mapping wrong: %+v", tac)
	}
	if math.Abs(tac.PositionValue-8151038*0.001304) > 1e-6 || !tac.ValueUSDValid || tac.MarkPrice != 0.001304 {
		t.Fatalf("TAC value must be total x marketPrice: %+v", tac)
	}
	btc := s.Positions["fc_57_BTC-USDT-SWAP_long"]
	if btc == nil || btc.Side != SideLong || btc.Size != 0.019 || btc.PosID != "fc_57_BTC-USDT-SWAP_long" {
		t.Fatalf("BTC mapping wrong: %+v", btc)
	}
	// App "账户资产" = wallet totalEquity + current unrealized PnL.
	if want := 35514.86 + (-676.5 + 8.79); math.Abs(s.TotalEquity-want) > 1e-6 {
		t.Fatalf("equity = wallet + UPL: got %.4f want %.4f", s.TotalEquity, want)
	}
	if s.Timestamp.UnixMilli() != 1794767408000 {
		t.Fatalf("snapshot time must be the platform utime: %v", s.Timestamp)
	}
	if !p.UseAuthoritativeSnapshotHotPath() {
		t.Fatal("FollowCoin must use the snapshot hot path")
	}
	if fills, err := p.GetFills("57", s.Timestamp); err != nil || len(fills) != 0 {
		t.Fatalf("raw fills never feed the hot path: %v %v", fills, err)
	}
}

func TestFollowCoinInvalidResponsesNeverReadAsFlat(t *testing.T) {
	cases := map[string]struct {
		body    string
		wantErr error
	}{
		"token missing":   {`{"code":"20001","msg":"Token is null","ok":false}`, ErrFollowCoinTokenInvalid},
		"token invalid":   {`{"code":"20003","msg":"Token exception","ok":false}`, ErrFollowCoinTokenInvalid},
		"account removed": {`{"ok":false,"code":"10384","msg":"The API does not exist or has been disabled.","data":null}`, ErrFollowCoinAccountUnavailable},
		"bad json":        {`<html>`, nil},
		"null data":       {`{"ok":true,"code":"200","data":null}`, nil},
		"missing total":   {`{"ok":true,"code":"200","data":[{"instId":"BTC-USDT-SWAP","holdSide":"long","marketPrice":1}]}`, nil},
		"bad side":        {`{"ok":true,"code":"200","data":[{"instId":"BTC-USDT-SWAP","holdSide":"both","total":1,"marketPrice":1}]}`, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fcFakeServer{positions: tc.body, overview: fcOverviewFixture}
			s, err := newFCTestProvider(t, f).GetPositionSnapshot("57")
			if err == nil || s != nil {
				t.Fatalf("invalid response became a snapshot: %+v", s)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
	if _, err := newFCTestProvider(t, &fcFakeServer{}).GetPositionSnapshot("not-a-number"); err == nil {
		t.Fatal("non-numeric apiId must be rejected")
	}
}

func TestFollowCoinEquityUnavailableLeavesSnapshotUsableForExits(t *testing.T) {
	f := &fcFakeServer{positions: fcPositionsFixture, overview: `{"ok":true,"code":"200","data":{"totalEquity":null}}`}
	s, err := newFCTestProvider(t, f).GetPositionSnapshot("57")
	if err != nil {
		t.Fatalf("positions remain authoritative without equity: %v", err)
	}
	if s.TotalEquity != 0 || len(s.Positions) != 2 {
		t.Fatalf("missing equity must read as unavailable (0), not guessed: %+v", s)
	}

	f2 := &fcFakeServer{positions: fcPositionsFixture, overview: `{"code":"20003","msg":"Token exception","ok":false}`}
	if _, err := newFCTestProvider(t, f2).GetPositionSnapshot("57"); !errors.Is(err, ErrFollowCoinTokenInvalid) {
		t.Fatalf("token failure on overview must surface: %v", err)
	}
}

func TestFollowCoinSnapshotIsSharedAndEquityCached(t *testing.T) {
	f := &fcFakeServer{positions: fcPositionsFixture, overview: fcOverviewFixture}
	p := newFCTestProvider(t, f)
	for i := 0; i < 3; i++ {
		s, err := p.GetPositionSnapshot("57")
		if err != nil {
			t.Fatal(err)
		}
		s.Positions["fc_57_BTC-USDT-SWAP_long"].Size = 99 // must not leak into the cache
	}
	s, _ := p.GetPositionSnapshot("57")
	if f.positionCalls.Load() != 1 || f.overviewCalls.Load() != 1 || s.Positions["fc_57_BTC-USDT-SWAP_long"].Size != 0.019 {
		t.Fatalf("requests not merged or cached snapshot mutated: positions=%d overview=%d", f.positionCalls.Load(), f.overviewCalls.Load())
	}
	if p.SuggestedSourcePollDelay(true) <= 0 {
		t.Fatal("poll delay must be positive")
	}
}

func TestFollowCoinListAccountsAndValidateToken(t *testing.T) {
	f := &fcFakeServer{
		accounts: `{"ok":true,"code":"200","data":[{"id":58,"userId":31535,"exchangeId":2,"alias":"08天才交易员OKX实盘","status":1},{"id":57,"userId":31535,"exchangeId":1,"alias":"08天才交易员币安实盘","status":1}]}`,
		userInfo: `{"ok":true,"code":"200","data":{"id":31439,"userNickName":"FC62874404","userEmail":"secret@example.com"}}`,
	}
	p := newFCTestProvider(t, f)
	accounts, err := p.ListAccounts(31535)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts: %+v %v", accounts, err)
	}
	if accounts[0].APIID != 58 || accounts[0].ExchangeName != "OKX" || accounts[1].APIID != 57 || accounts[1].ExchangeName != "Binance" {
		t.Fatalf("account mapping wrong: %+v", accounts)
	}
	user, err := p.ValidateToken()
	if err != nil || user.NickName != "FC62874404" || user.ID != 31439 {
		t.Fatalf("validate: %+v %v", user, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", user), "secret@") {
		t.Fatal("token owner email must never be exposed")
	}

	unconfigured := NewFollowCoinProviderWithLoader(nil)
	if _, err := unconfigured.GetPositionSnapshot("57"); !errors.Is(err, ErrFollowCoinTokenInvalid) {
		t.Fatalf("missing token must read as invalid credentials: %v", err)
	}
}

func TestFollowCoinExplicitTokenNeverReusesAnotherTokensSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("token") != "good" {
			fmt.Fprint(w, `{"code":"20003","msg":"Token exception","ok":false}`)
			return
		}
		if r.URL.Path == followCoinOverviewPath {
			fmt.Fprint(w, fcOverviewFixture)
			return
		}
		fmt.Fprint(w, fcPositionsFixture)
	}))
	t.Cleanup(srv.Close)
	mk := func(token string) *FollowCoinProvider {
		p := NewFollowCoinProviderWithToken(token)
		p.client, p.baseURL = srv.Client(), srv.URL
		return p
	}
	if _, err := mk("good").GetPositionSnapshot("57"); err != nil {
		t.Fatal(err)
	}
	if _, err := mk("bad").GetPositionSnapshot("57"); !errors.Is(err, ErrFollowCoinTokenInvalid) {
		t.Fatalf("a bad token must not be served the good token's cached snapshot: %v", err)
	}
}
