package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"nofx/manager"
	"nofx/store"
)

func newTraderSaveTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st, err := store.New(filepath.Join(t.TempDir(), "trader-save.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Server{store: st, traderManager: manager.NewTraderManager()}, st
}

func callTraderHandler(t *testing.T, handler func(*gin.Context), traderID string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/traders", strings.NewReader(string(raw)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", "owner")
	if traderID != "" {
		c.Params = gin.Params{{Key: "id", Value: traderID}}
	}
	handler(c)
	return w
}

func countTraders(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(1) FROM traders`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func copyTraderBody(provider, leader string, ratio float64) map[string]interface{} {
	return map[string]interface{}{
		"name": "AguilaTrades", "ai_model_id": "model", "exchange_id": "account",
		"decision_mode": "copy_trade",
		"copy_config": map[string]interface{}{
			"provider_type": provider, "leader_id": leader, "copy_ratio": ratio, "sync_leverage": true,
		},
	}
}

// Reentry AI is a Copy Guard feature. A source that cannot run Copy Guard
// must not be rejected for lacking it, while OKX/Binance keep the requirement.
func TestAIGuardedPrerequisiteAppliesOnlyToCopyGuardSources(t *testing.T) {
	_, st := newTraderSaveTestServer(t)
	for provider, wantErr := range map[string]bool{"hyperliquid": false, "okx": true, "binance": true} {
		cfg := store.NewCopyGuardDefaults()
		cfg.ProviderType = provider
		applyCopyConfigRiskFields(cfg, &CopyConfigReq{ProviderType: provider})
		err := validateAIGuardedPrerequisites(st, cfg)
		if (err != nil) != wantErr {
			t.Fatalf("provider=%s with Reentry AI disabled: err=%v wantErr=%v", provider, err, wantErr)
		}
	}
}

func TestCreateHyperliquidCopyTraderWithoutReentryAIPersistsConfig(t *testing.T) {
	s, st := newTraderSaveTestServer(t)
	const leader = "0x1f250df59a777d61cb8bd043c12970f3afe4f925"
	w := callTraderHandler(t, s.handleCreateTrader, "", copyTraderBody("hyperliquid", leader, 1))
	if w.Code != http.StatusCreated {
		t.Fatalf("HL copy trader must be creatable without Reentry AI: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		TraderID string `json:"trader_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.TraderID == "" {
		t.Fatalf("missing trader id: %s", w.Body.String())
	}
	cfg, err := st.CopyTrade().GetByTraderID(resp.TraderID)
	if err != nil {
		t.Fatalf("copy config must be persisted with the trader: %v", err)
	}
	if cfg.ProviderType != "hyperliquid" || cfg.LeaderID != leader || cfg.RiskPolicyVersion != 0 {
		t.Fatalf("unexpected persisted config: provider=%s leader=%s v=%d", cfg.ProviderType, cfg.LeaderID, cfg.RiskPolicyVersion)
	}
}

func TestCreateCopyTraderValidationFailureLeavesNoTrader(t *testing.T) {
	s, st := newTraderSaveTestServer(t)
	w := callTraderHandler(t, s.handleCreateTrader, "", copyTraderBody("hyperliquid", "0xabc", maxCopyRatio+1))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid copy config must be rejected: %d %s", w.Code, w.Body.String())
	}
	if n := countTraders(t, st); n != 0 {
		t.Fatalf("rejected copy config left a half-created trader behind: %d traders", n)
	}
}

func TestUpdateCopyTraderValidationFailureKeepsTraderUnchanged(t *testing.T) {
	s, st := newTraderSaveTestServer(t)
	if _, err := st.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,lifecycle_status,is_running,decision_mode)
		VALUES('t1','owner','original','model','account',1000,'STOPPED',0,'ai')`); err != nil {
		t.Fatal(err)
	}
	body := copyTraderBody("hyperliquid", "0xabc", maxCopyRatio+1)
	body["name"] = "renamed"
	w := callTraderHandler(t, s.handleUpdateTrader, "t1", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid copy config must be rejected: %d %s", w.Code, w.Body.String())
	}
	var name, mode string
	if err := st.DB().QueryRow(`SELECT name, decision_mode FROM traders WHERE id='t1'`).Scan(&name, &mode); err != nil {
		t.Fatal(err)
	}
	if name != "original" || mode != "ai" {
		t.Fatalf("rejected update must not persist trader changes: name=%s mode=%s", name, mode)
	}
}

// A trader left half-created by the old ordering must be repairable from the
// edit dialog: saving an HL config through the update path persists it.
func TestUpdateRepairsHalfCreatedHyperliquidCopyTrader(t *testing.T) {
	s, st := newTraderSaveTestServer(t)
	if _, err := st.DB().Exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,lifecycle_status,is_running,decision_mode)
		VALUES('t1','owner','AguilaTrades','model','account',1000,'STOPPED',0,'copy_trade')`); err != nil {
		t.Fatal(err)
	}
	const leader = "0x1f250df59a777d61cb8bd043c12970f3afe4f925"
	w := callTraderHandler(t, s.handleUpdateTrader, "t1", copyTraderBody("hyperliquid", leader, 1))
	if w.Code != http.StatusOK {
		t.Fatalf("editing a half-created HL trader must save its config: %d %s", w.Code, w.Body.String())
	}
	cfg, err := st.CopyTrade().GetByTraderID("t1")
	if err != nil || cfg.ProviderType != "hyperliquid" || cfg.LeaderID != leader {
		t.Fatalf("copy config not repaired: %+v err=%v", cfg, err)
	}
}
