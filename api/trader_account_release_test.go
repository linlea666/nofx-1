package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"nofx/manager"
	"nofx/store"
	"nofx/trader"
)

type releaseTestTransport func(*http.Request) (*http.Response, error)

func (f releaseTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTraderDeletionVerifiesCurrentRiskAndReleasesAccountThroughAPI(t *testing.T) {
	for _, scenario := range []string{"delete", "legacy-start-pending", "unauthorized", "pending", "timeout", "already-released"} {
		t.Run(scenario, func(t *testing.T) {
			st, err := store.New(filepath.Join(t.TempDir(), "api-release.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			exec := func(q string) {
				t.Helper()
				if _, err := st.DB().Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance,lifecycle_status,is_running) VALUES('old','owner','九川','model','account',1000,'STOPPED',0),('new','owner','新交易员','model','account',1000,'STOPPED',0)`)
			exec(`INSERT INTO exchanges(id,user_id,exchange_type,name,type,api_key,secret_key,passphrase) VALUES('account','owner','okx','test','cex','test-key','test-secret','test-passphrase')`)
			exec(`INSERT INTO ai_models(id,user_id,name,provider) VALUES('model','owner','test','deepseek')`)
			if err = st.CopyTrade().UpsertCopyGuardProtectiveOrder(&store.CopyGuardProtectiveOrder{CycleID: 806, TraderID: "old", Symbol: "SNDKUSDT", AlgoID: "algo", Status: "mark_crossed_exit"}); err != nil {
				t.Fatal(err)
			}
			s := &Server{store: st, traderManager: manager.NewTraderManager(), positionSyncManager: trader.NewPositionSyncManager(st, 0)}
			queries := 0
			original := http.DefaultTransport
			http.DefaultTransport = releaseTestTransport(func(r *http.Request) (*http.Response, error) {
				queries++
				if r.Method != "GET" {
					t.Errorf("deletion submitted venue mutation %s %s", r.Method, r.URL.Path)
					return nil, fmt.Errorf("mutation forbidden")
				}
				switch r.URL.Path {
				case "/api/v5/account/positions", "/api/v5/trade/orders-pending", "/api/v5/trade/orders-algo-pending":
				default:
					t.Errorf("deletion queried historical data: %s", r.URL.Path)
					return nil, fmt.Errorf("unexpected query")
				}
				if scenario == "timeout" {
					return nil, fmt.Errorf("simulated snapshot timeout")
				}
				body := `{"code":"0","data":[]}`
				if (scenario == "pending" || scenario == "legacy-start-pending") && r.URL.Path == "/api/v5/trade/orders-algo-pending" {
					body = `{"code":"0","data":[{"algoId":"real-pending-algo","instId":"SNDK-USDT-SWAP","state":"live"}]}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			defer func() { http.DefaultTransport = original }()
			user, id, method := "owner", "old", "DELETE"
			want := 200
			switch scenario {
			case "unauthorized":
				user = "outsider"
				want = 404
			case "pending", "timeout":
				want = 409
			case "legacy-start-pending":
				exec(`UPDATE traders SET lifecycle_status='ARCHIVED' WHERE id='old'`)
				id, method, want = "new", "POST", 409
			case "already-released":
				if _, err = s.positionSyncManager.ArchiveTrader(user, id); err != nil {
					t.Fatal(err)
				}
				queries = 0
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("user_id", user)
			c.Params = gin.Params{{Key: "id", Value: id}}
			c.Request = httptest.NewRequest(method, "/api/traders/"+id, nil)
			if method == "DELETE" {
				s.handleDeleteTrader(c)
			} else {
				s.handleStartTrader(c)
			}
			if w.Code != want {
				t.Fatalf("status=%d want %d: %s", w.Code, want, w.Body.String())
			}
			if scenario == "unauthorized" || scenario == "already-released" {
				if queries != 0 {
					t.Fatalf("unexpected queries %d", queries)
				}
			}
			if scenario == "legacy-start-pending" || scenario == "pending" {
				if !strings.Contains(w.Body.String(), "real-pending-algo") {
					t.Fatal("venue blocker was hidden", w.Body.String())
				}
			}
			if scenario == "legacy-start-pending" {
				state, _ := st.Trader().GetLifecycle("new")
				if state.Status != store.TraderLifecycleStopped || state.Generation != 0 {
					t.Fatalf("start mutated before verification: %+v", state)
				}
			}
			if scenario == "delete" {
				if queries != 3 {
					t.Fatalf("expected only fresh positions and ordinary/algo orders, got %d", queries)
				}
				state, err := st.Trader().BeginStart("owner", "new")
				if err != nil {
					t.Fatal(err)
				}
				if err = st.Trader().CompleteCopyGuardStart("owner", "new", state.Generation, "account"); err != nil {
					t.Fatal("deleted trader still blocks account", err)
				}
			}
		})
	}
}
