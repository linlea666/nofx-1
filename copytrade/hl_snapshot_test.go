package copytrade

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const hlClearinghouseFixture = `{
  "marginSummary": {"accountValue": "172.018426", "totalNtlPos": "80.50809", "totalRawUsd": "91.510336", "totalMarginUsed": "7.536583"},
  "withdrawable": "163.636587",
  "assetPositions": [
    {"type": "oneWay", "position": {"coin": "BTC", "szi": "0.00033", "leverage": {"type": "cross", "value": 9},
      "entryPx": "90282.0", "positionValue": "29.79273", "unrealizedPnl": "-0.00033", "marginUsed": "3.310303"}},
    {"type": "oneWay", "position": {"coin": "ETH", "szi": "-0.0164", "leverage": {"type": "isolated", "value": 12},
      "entryPx": "3089.62", "positionValue": "50.71536", "unrealizedPnl": "0.04548", "marginUsed": "4.22628"}}
  ],
  "time": 1765596089726
}`

func hlTestProvider(handler func(*http.Request) (*http.Response, error)) *HyperliquidProvider {
	return &HyperliquidProvider{client: &http.Client{Transport: roundTripFunc(handler)}}
}

func hlJSONResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

// hlRequestType reads the info request type; the snapshot hot path also asks
// for the account abstraction mode when the leader holds positions.
func hlRequestType(r *http.Request) string {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body["type"]
}

func TestHLSnapshotParsesClearinghouseStateIntoCanonicalPositions(t *testing.T) {
	var requestTypes []string
	p := hlTestProvider(func(r *http.Request) (*http.Response, error) {
		requestType := hlRequestType(r)
		requestTypes = append(requestTypes, requestType)
		if requestType == "userAbstraction" {
			return hlJSONResponse(200, `"default"`), nil
		}
		return hlJSONResponse(200, hlClearinghouseFixture), nil
	})
	s, err := p.GetPositionSnapshot(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(requestTypes, ",") != "clearinghouseState,userAbstraction" {
		t.Fatalf("hot path must read the snapshot and the account mode only, got %v", requestTypes)
	}
	if s.TotalEquity != 172.018426 || s.AvailableBalance != 163.636587 {
		t.Fatalf("equity mapping wrong: %+v", s)
	}
	if s.Timestamp.UnixMilli() != 1765596089726 {
		t.Fatalf("snapshot time lost: %v", s.Timestamp)
	}
	btc := s.Positions["BTCUSDT_long"]
	if btc == nil || btc.PosID != "BTCUSDT_long" || btc.Size != 0.00033 || btc.Leverage != 9 || btc.MarginMode != "cross" {
		t.Fatalf("BTC long position wrong: %+v", btc)
	}
	if math.Abs(btc.MarkPrice-29.79273/0.00033) > 1e-6 {
		t.Fatalf("mark price must derive from positionValue/size: %f", btc.MarkPrice)
	}
	eth := s.Positions["ETHUSDT_short"]
	if eth == nil || eth.PosID != "ETHUSDT_short" || eth.Side != SideShort || eth.Size != 0.0164 || eth.MarginMode != "isolated" {
		t.Fatalf("ETH short szi normalization wrong: %+v", eth)
	}
	if eth.OpenedMS != 0 {
		t.Fatalf("HL snapshot has no open time; OpenedMS must stay 0: %d", eth.OpenedMS)
	}

	// GetAccountState shares the parser: identity must be byte-identical so
	// InitIgnoredPositions and snapshot diffing agree on leader_pos_id.
	full, err := p.GetAccountState(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	for key, pos := range full.Positions {
		if pos.PosID != key || s.Positions[key] == nil {
			t.Fatalf("GetAccountState identity diverged: key=%s pos=%+v", key, pos)
		}
	}
}

func TestHLSnapshotHotPathIsSharedAndCloned(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if hlRequestType(r) == "userAbstraction" {
			return hlJSONResponse(200, `"default"`), nil
		}
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		return hlJSONResponse(200, hlClearinghouseFixture), nil
	})
	p := &HyperliquidProvider{client: &http.Client{Transport: transport}}
	p2 := &HyperliquidProvider{client: &http.Client{Transport: transport}}
	var wg sync.WaitGroup
	for _, provider := range []*HyperliquidProvider{p, p2} {
		wg.Add(1)
		go func(p *HyperliquidProvider) {
			defer wg.Done()
			s, err := p.GetPositionSnapshot(t.Name())
			if err != nil {
				t.Error(err)
				return
			}
			s.Positions["BTCUSDT_long"].Size = 1
		}(provider)
	}
	wg.Wait()
	s, err := p.GetPositionSnapshot(t.Name())
	if err != nil || calls.Load() != 1 || s.Positions["BTCUSDT_long"].Size != 0.00033 {
		t.Fatalf("requests not merged or shared snapshot mutated: calls=%d %+v %v", calls.Load(), s, err)
	}
	if !p.UseAuthoritativeSnapshotHotPath() {
		t.Fatal("HL must use the authoritative snapshot hot path")
	}
	if p.SuggestedSourcePollDelay(true) != 2*time.Second || p.SuggestedSourcePollDelay(false) != 5*time.Second {
		t.Fatal("wrong poll schedule")
	}
}

func TestHLInvalidSnapshotAndRateLimitAreNotFlatPositions(t *testing.T) {
	invalid := []string{
		`not json`,
		`{}`, // zero time: partial/empty body must never read as flat
		`{"marginSummary":{"accountValue":"100"},"assetPositions":[{"position":{"coin":"BTC","szi":"bad"}}],"time":1}`,
		`{"marginSummary":{"accountValue":"0"},"assetPositions":[{"position":{"coin":"BTC","szi":"1","positionValue":"60000"}}],"time":1}`,
		`{"marginSummary":{"accountValue":""},"assetPositions":[{"position":{"coin":"BTC","szi":"-1","positionValue":"60000"}}],"time":1}`,
	}
	for n, body := range invalid {
		p := hlTestProvider(func(*http.Request) (*http.Response, error) { return hlJSONResponse(200, body), nil })
		if _, err := p.GetPositionSnapshot(fmt.Sprintf("%s-%d", t.Name(), n)); err == nil {
			t.Fatalf("invalid body became a snapshot: %s", body)
		}
	}

	// A genuinely flat account with valid time is authoritative.
	flat := hlTestProvider(func(*http.Request) (*http.Response, error) {
		return hlJSONResponse(200, `{"marginSummary":{"accountValue":"50"},"withdrawable":"50","assetPositions":[],"time":1765596089726}`), nil
	})
	s, err := flat.GetPositionSnapshot(t.Name() + "-flat")
	if err != nil || len(s.Positions) != 0 || s.TotalEquity != 50 {
		t.Fatalf("valid flat snapshot rejected: %+v %v", s, err)
	}

	calls := 0
	p := hlTestProvider(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	for i := 0; i < 2; i++ {
		if _, err := p.GetPositionSnapshot(t.Name()); err == nil {
			t.Fatal("rate limit became flat")
		}
	}
	if calls != 1 {
		t.Fatalf("Retry-After ignored: calls=%d", calls)
	}
}

// A unified-account leader's perp accountValue is only the collateral moved
// into perps. Production: perp 3,305,850 vs account equity 6,554,442.
func TestHLUnifiedAccountEquityUsesSpotCollateral(t *testing.T) {
	const clearinghouse = `{"marginSummary":{"accountValue":"3305850.0"},"withdrawable":"79200.0","assetPositions":[{"position":{"coin":"BTC","szi":"-500.0","leverage":{"type":"cross","value":40},"entryPx":"64643.6","positionValue":"32266500.0"}}],"time":1765596089726}`
	reply := func(mode, spot string, modeStatus, spotStatus int) func(*http.Request) (*http.Response, error) {
		return func(r *http.Request) (*http.Response, error) {
			switch hlRequestType(r) {
			case "userAbstraction":
				return hlJSONResponse(modeStatus, mode), nil
			case "spotClearinghouseState":
				return hlJSONResponse(spotStatus, spot), nil
			}
			return hlJSONResponse(200, clearinghouse), nil
		}
	}
	const spot = `{"balances":[{"coin":"HYPE","total":"12.0","hold":"0.0"},{"coin":"USDC","total":"6554442.30","hold":"3226650.0"}]}`
	cases := []struct {
		name, mode, spot     string
		modeStatus, spotCode int
		want                 float64
	}{
		{"unified uses spot USDC total", `"unifiedAccount"`, spot, 200, 200, 6554442.30},
		{"portfolio margin uses spot USDC total", `"portfolioMargin"`, spot, 200, 200, 6554442.30},
		{"standard keeps perp equity", `"default"`, spot, 200, 200, 3305850},
		{"unknown mode pauses risk increase", `{}`, spot, 500, 200, 0},
		{"missing spot USDC pauses risk increase", `"unifiedAccount"`, `{"balances":[]}`, 200, 200, 0},
		{"spot read failure pauses risk increase", `"unifiedAccount"`, `{}`, 200, 429, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := hlTestProvider(reply(c.mode, c.spot, c.modeStatus, c.spotCode))
			s, err := p.GetPositionSnapshot(t.Name())
			if err != nil {
				t.Fatal(err)
			}
			if s.TotalEquity != c.want || len(s.Positions) != 1 {
				t.Fatalf("equity=%v want %v (%+v)", s.TotalEquity, c.want, s)
			}
		})
	}
}

func TestHLLeaderEquityCachesModeAndKeepsLastKnownMode(t *testing.T) {
	var modeCalls, spotCalls atomic.Int32
	modeDown := false
	p := hlTestProvider(func(r *http.Request) (*http.Response, error) {
		switch hlRequestType(r) {
		case "userAbstraction":
			modeCalls.Add(1)
			if modeDown {
				return hlJSONResponse(500, `{}`), nil
			}
			return hlJSONResponse(200, `"unifiedAccount"`), nil
		case "spotClearinghouseState":
			spotCalls.Add(1)
			return hlJSONResponse(200, `{"balances":[{"coin":"USDC","total":"1000","hold":"10"}]}`), nil
		}
		return hlJSONResponse(200, hlClearinghouseFixture), nil
	})
	leader := t.Name()
	for i := 0; i < 3; i++ {
		if got := p.leaderAccountEquity(leader, 172); got != 1000 {
			t.Fatalf("equity=%v", got)
		}
	}
	if modeCalls.Load() != 1 || spotCalls.Load() != 1 {
		t.Fatalf("mode/spot not cached: mode=%d spot=%d", modeCalls.Load(), spotCalls.Load())
	}
	// An expired mode whose refresh fails keeps the last known mode.
	value, _ := hlLeaderEquityCaches.Load(fmt.Sprintf("%p|%s", p.client.Transport, leader))
	c := value.(*hlLeaderEquityCache)
	c.modeAt = time.Now().Add(-2 * hlAbstractionTTL)
	c.spotEquityAt = time.Time{}
	modeDown = true
	if got := p.leaderAccountEquity(leader, 172); got != 1000 {
		t.Fatalf("stale mode lost on refresh failure: %v", got)
	}
}
