package api

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"nofx/store"
)

func TestRuntimeHealthSeparatesFeesFromExecutionAndAuthorizesOwner(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "health-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,is_running,lifecycle_status) VALUES('t','owner','华创','','account',1000,1,'RUNNING'),('other','outsider','private','','private-account',1000,1,'RUNNING')`); err != nil {
		t.Fatal(err)
	}
	cfg := &store.CopyTradeConfig{TraderID: "t", Enabled: true, ProviderType: "okx", LeaderID: "leader", CopyRatio: 1}
	if err = st.CopyTrade().Upsert(cfg); err != nil {
		t.Fatal(err)
	}
	if err = st.Position().RecordSettlementDiagnostic(store.PositionSettlementDiagnostic{ExchangeID: "account", PositionID: "p", Symbol: "ZECUSDT", Side: "long", MarginMode: "isolated", OpenedMS: 1790022269524, Stage: "fills", ReasonCode: "OPENING_FILLS_INCOMPLETE", Detail: "waiting for immutable first fill"}); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().ObserveRuntimeSource("t", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	policy, err := store.EncodeCopyGuardPolicySnapshot(store.NewCopyGuardDefaults())
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "t", LeaderID: "leader", LeaderPosID: "live", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: policy})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().UpdateCopyGuardProtectionHealth(cycle.ID, store.CopyGuardProtectionUnknown, 0, "venue verification unavailable", "", "", false); err != nil {
		t.Fatal(err)
	}
	h := &CopyTradeHandler{store: st}
	for _, tc := range []struct {
		user, query string
		status      int
	}{{"owner", "", 200}, {"outsider", "?trader_id=t", 404}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("user_id", tc.user)
		c.Request = httptest.NewRequest("GET", "/runtime-health"+tc.query, nil)
		h.RuntimeHealth(c)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.user, w.Code, w.Body.String())
		}
		if tc.status != 200 {
			continue
		}
		var body struct {
			Traders []copyRuntimeHealthView `json:"traders"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Traders) != 1 || len(body.Traders[0].ExecutionIssues) != 0 || len(body.Traders[0].SettlementIssues) != 1 || body.Traders[0].Source == nil {
			t.Fatalf("fees became execution blockage or owner isolation failed: %s", w.Body.String())
		}
		if len(body.Traders[0].RuntimeIssues) != 1 || body.Traders[0].RuntimeIssues[0].Code != "PROTECTION_UNKNOWN" {
			t.Fatalf("cycle protection failure hidden: %s", w.Body.String())
		}
	}
}
