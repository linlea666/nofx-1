package api

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"nofx/store"
)

func TestCurrentCopyPreviewSourceCoversEverySupportedProvider(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "copy-source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = newCurrentCopySource(st, "okx", "", "leader"); err != nil {
		t.Fatalf("okx source: %v", err)
	}
	if _, err = newCurrentCopySource(st, "followcoin", "", "31535"); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("followcoin without token must explain the missing credential: %v", err)
	}
	if err = st.FollowCoinCreds().Set(store.FollowCoinCredsLabelDefault, "test-token"); err != nil {
		t.Fatal(err)
	}
	if _, err = newCurrentCopySource(st, "followcoin", "", "not-a-number"); err == nil {
		t.Fatal("invalid followcoin apiId accepted")
	}
	if p, err := newCurrentCopySource(st, "followcoin", "", "31535"); err != nil || p == nil {
		t.Fatalf("followcoin source: %v %v", p, err)
	}
	for _, tc := range []struct{ provider, mode string }{
		{"hyperliquid", ""}, {"binance", "copy_management"}, {"binance", "smart_money"},
	} {
		if p, err := newCurrentCopySource(st, tc.provider, tc.mode, "leader"); err != nil || p == nil {
			t.Fatalf("%s/%s preview source: %v %v", tc.provider, tc.mode, p, err)
		}
	}
	if _, err = newCurrentCopySource(st, "binance", "bogus", "leader"); err == nil {
		t.Fatal("unknown Binance source mode built a preview source")
	}
	if _, err = newCurrentCopySource(st, "lighter", "", "leader"); err == nil {
		t.Fatal("unsupported provider built a preview source")
	}
	// Unsupported providers are refused before any account or venue query.
	if _, err = st.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance) VALUES('t','owner','trader','','exchange',1000)`); err != nil {
		t.Fatal(err)
	}
	h := &CopyTradeHandler{store: st}
	for _, body := range []string{
		`{"provider_type":"lighter","leader_id":"0xabc","copy_ratio":1}`,
		`{"provider_type":"binance","binance_source_mode":"smart_money","leader_id":"not-a-top-trader","copy_ratio":1}`,
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("user_id", "owner")
		c.Request = httptest.NewRequest("POST", "/api/copytrade/current-positions/preview", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PreviewCurrentPositions(c)
		if w.Code != 400 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
}

func TestCurrentCopyOptionsRequestValidation(t *testing.T) {
	on, off := true, false
	ok, high := 0.5, 60.0
	sel := []store.CurrentPositionCopySelection{{LeaderPosID: "p", OpenedMS: 1}}
	if err := validateCurrentCopyOptionsRequest(&on, sel, &ok, nil); err != nil {
		t.Fatal(err)
	}
	if err := validateCurrentCopyOptionsRequest(&on, nil, nil, nil); err != nil {
		t.Fatalf("older clients without options must still register: %v", err)
	}
	if err := validateCurrentCopyOptionsRequest(&on, []store.CurrentPositionCopySelection{}, nil, nil); err == nil {
		t.Fatal("empty selection accepted")
	}
	if err := validateCurrentCopyOptionsRequest(&on, sel, &high, nil); err == nil {
		t.Fatal("out-of-range advantage accepted")
	}
	if err := validateCurrentCopyOptionsRequest(&off, []store.CurrentPositionCopySelection{}, &high, &high); err != nil {
		t.Fatalf("disabled request must ignore options: %v", err)
	}
	tolerance, negative := 1.0, -1.0
	if err := validateCurrentCopyOptionsRequest(&on, sel, &ok, &tolerance); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*float64{&negative, &high} {
		if err := validateCurrentCopyOptionsRequest(&on, sel, &ok, bad); err == nil {
			t.Fatalf("out-of-range manual tolerance %v accepted", *bad)
		}
	}
}
