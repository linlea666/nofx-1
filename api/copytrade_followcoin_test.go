package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"nofx/copytrade"
	"nofx/store"
)

type stubFollowCoinProbe struct {
	validateErr, snapshotErr error
	accounts                 []copytrade.FollowCoinAccount
}

func (s stubFollowCoinProbe) ValidateToken() (*copytrade.FollowCoinUser, error) {
	if s.validateErr != nil {
		return nil, s.validateErr
	}
	return &copytrade.FollowCoinUser{ID: 1, NickName: "tester"}, nil
}
func (s stubFollowCoinProbe) ListAccounts(int64) ([]copytrade.FollowCoinAccount, error) {
	return s.accounts, nil
}
func (s stubFollowCoinProbe) GetPositionSnapshot(string) (*copytrade.AccountState, error) {
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	return &copytrade.AccountState{Positions: map[string]*copytrade.Position{}}, nil
}

func useFollowCoinProbe(t *testing.T, probe stubFollowCoinProbe) {
	t.Helper()
	prev := newFollowCoinProbe
	newFollowCoinProbe = func(string) followCoinProbe { return probe }
	t.Cleanup(func() { newFollowCoinProbe = prev })
}

func callCopyTradeHandler(t *testing.T, fn func(*gin.Context), method, target string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, reader)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", "owner")
	fn(c)
	return w
}

const testFollowCoinToken = "eyJraWQiOiJ4In0.eyJleHAiOjE4MDg5MTY1Mzd9.signaturepart"

func TestExtractFollowCoinTokenAcceptsTokenHeaderAndCurl(t *testing.T) {
	for in, want := range map[string]string{
		testFollowCoinToken:               testFollowCoinToken,
		"  " + testFollowCoinToken + "\n": testFollowCoinToken,
		"token: " + testFollowCoinToken:   testFollowCoinToken,
		"curl -X POST 'https://api-v2.cryptostrategyget.com/v1/x' -H 'token: " + testFollowCoinToken + "' -H 'os: ios'": testFollowCoinToken,
		"some random text": "",
	} {
		if got := extractFollowCoinToken(in); got != want {
			t.Fatalf("extract(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSetFollowCoinCredentialsValidatesBeforeSaving(t *testing.T) {
	_, st := newTraderSaveTestServer(t)
	h := &CopyTradeHandler{store: st}

	useFollowCoinProbe(t, stubFollowCoinProbe{validateErr: fmt.Errorf("x: %w", copytrade.ErrFollowCoinTokenInvalid)})
	w := callCopyTradeHandler(t, h.SetFollowCoinCredentials, http.MethodPost, "/", map[string]string{"token": testFollowCoinToken})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid token must be rejected: %d %s", w.Code, w.Body.String())
	}
	if c, _ := st.FollowCoinCreds().Get(""); c != nil {
		t.Fatal("rejected token must not be saved")
	}

	useFollowCoinProbe(t, stubFollowCoinProbe{})
	w = callCopyTradeHandler(t, h.SetFollowCoinCredentials, http.MethodPost, "/", map[string]string{"token": testFollowCoinToken})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), testFollowCoinToken) {
		t.Fatalf("valid token saved, response must be masked: %d %s", w.Code, w.Body.String())
	}
	c, _ := st.FollowCoinCreds().Get("")
	if c == nil || c.Token != testFollowCoinToken || c.LastStatus != store.FollowCoinCredsStatusValid || c.ExpiresAt.Unix() != 1808916537 {
		t.Fatalf("saved credentials wrong: %+v", c)
	}

	// A definitely-invalid replacement must not overwrite the working token.
	useFollowCoinProbe(t, stubFollowCoinProbe{validateErr: copytrade.ErrFollowCoinTokenInvalid})
	_ = callCopyTradeHandler(t, h.SetFollowCoinCredentials, http.MethodPost, "/", map[string]string{"token": "eyJbad.eyJbad.bad"})
	if c, _ := st.FollowCoinCreds().Get(""); c.Token != testFollowCoinToken {
		t.Fatal("working token was overwritten by an invalid one")
	}
}

func TestListFollowCoinAccountsRequiresToken(t *testing.T) {
	_, st := newTraderSaveTestServer(t)
	h := &CopyTradeHandler{store: st}
	useFollowCoinProbe(t, stubFollowCoinProbe{accounts: []copytrade.FollowCoinAccount{{APIID: 57, Alias: "币安实盘", ExchangeID: 1, ExchangeName: "Binance"}}})

	if w := callCopyTradeHandler(t, h.ListFollowCoinAccounts, http.MethodGet, "/?userId=31535", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("lookup without token must be rejected: %d", w.Code)
	}
	if err := st.FollowCoinCreds().Set("", testFollowCoinToken); err != nil {
		t.Fatal(err)
	}
	if w := callCopyTradeHandler(t, h.ListFollowCoinAccounts, http.MethodGet, "/?userId=abc", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric homepage id must be rejected: %d", w.Code)
	}
	w := callCopyTradeHandler(t, h.ListFollowCoinAccounts, http.MethodGet, "/?userId=31535", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"api_id":57`) {
		t.Fatalf("accounts not returned: %d %s", w.Code, w.Body.String())
	}
}

func TestCreateFollowCoinCopyTraderValidatesSelectedAccount(t *testing.T) {
	s, st := newTraderSaveTestServer(t)
	if err := st.FollowCoinCreds().Set("", testFollowCoinToken); err != nil {
		t.Fatal(err)
	}

	useFollowCoinProbe(t, stubFollowCoinProbe{snapshotErr: copytrade.ErrFollowCoinAccountUnavailable})
	w := callTraderHandler(t, s.handleCreateTrader, "", copyTraderBody("followcoin", "57", 1))
	if w.Code != http.StatusBadRequest || countTraders(t, st) != 0 {
		t.Fatalf("disabled real account must be rejected before persisting: %d %s", w.Code, w.Body.String())
	}

	if w := callTraderHandler(t, s.handleCreateTrader, "", copyTraderBody("followcoin", "abc", 1)); w.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric apiId must be rejected: %d", w.Code)
	}

	useFollowCoinProbe(t, stubFollowCoinProbe{})
	w = callTraderHandler(t, s.handleCreateTrader, "", copyTraderBody("followcoin", "057", 1))
	if w.Code != http.StatusCreated {
		t.Fatalf("valid FollowCoin config must be created: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		TraderID string `json:"trader_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	cfg, err := st.CopyTrade().GetByTraderID(resp.TraderID)
	if err != nil || cfg.ProviderType != "followcoin" || cfg.LeaderID != "57" || cfg.RiskPolicyVersion != 0 {
		t.Fatalf("persisted FollowCoin config wrong: %+v err=%v", cfg, err)
	}
}
