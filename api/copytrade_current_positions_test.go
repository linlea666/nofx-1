package api

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"nofx/store"
)

func TestCurrentPositionPreviewRequiresOwnershipBeforeAnyVenueQuery(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "copy-preview.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance) VALUES('t','owner','trader','','exchange',1000)`); err != nil {
		t.Fatal(err)
	}
	h := &CopyTradeHandler{store: st}
	for _, tc := range []struct {
		user, body string
		status     int
	}{
		{"", `{"provider_type":"okx","leader_id":"leader","copy_ratio":1}`, 401},
		{"other", `{"trader_id":"t","exchange_id":"exchange","provider_type":"okx","leader_id":"leader","copy_ratio":1}`, 404},
		{"owner", `{"trader_id":"t","exchange_id":"someone-else","provider_type":"okx","leader_id":"leader","copy_ratio":1}`, 404},
		{"owner", `{"provider_type":"binance","leader_id":"leader","copy_ratio":1}`, 400},
		{"owner", `{"provider_type":"okx","leader_id":"leader","copy_ratio":100}`, 400},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("user_id", tc.user)
		c.Request = httptest.NewRequest("POST", "/api/copytrade/current-positions/preview", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PreviewCurrentPositions(c)
		if w.Code != tc.status {
			t.Fatalf("user %s: %d %s", tc.user, w.Code, w.Body.String())
		}
	}
	var count int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_current_position_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("preview registered work: %d %v", count, err)
	}
}

func TestCurrentCopyRegistrationFailsBeforeTraderChanges(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "copy-request.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,lifecycle_status,is_running,decision_mode) VALUES('t','owner','trader','','exchange',1000,'RUNNING',1,'copy_trade')`); err != nil {
		t.Fatal(err)
	}
	on := true
	off := false
	id := uuid.NewString()
	for _, tc := range []struct {
		trader, mode, provider, request string
		flag                            *bool
		ok                              bool
	}{
		{"t", "copy_trade", "okx", id, nil, true},
		{"t", "copy_trade", "okx", id, &off, true},
		{"t", "copy_trade", "okx", id, &on, false},
		{"", "copy_trade", "okx", "bad-id", &on, false},
		{"", "ai", "okx", id, &on, false},
		{"", "copy_trade", "binance", id, &on, false},
		{"", "copy_trade", "okx", id, &on, true},
	} {
		err := validateCurrentCopyRegistration(st, tc.trader, "exchange", tc.mode, tc.provider, "leader", tc.request, tc.flag)
		if (err == nil) != tc.ok {
			t.Fatalf("case %+v: %v", tc, err)
		}
	}
}
