package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"nofx/auth"
	"nofx/manager"
	"nofx/store"
)

func newAvailabilityTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "availability.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(manager.NewTraderManager(), st, nil, nil, 0), st
}

func TestReadyReportsDatabaseAvailability(t *testing.T) {
	server, st := newAvailabilityTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthy ready status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// Reserve the only database connection to reproduce connection starvation.
	conn, err := st.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked ready status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if elapsed := time.Since(started); elapsed < databaseReadyTimeout || elapsed > databaseReadyTimeout+time.Second {
		t.Fatalf("ready timeout=%s, expected about %s", elapsed, databaseReadyTimeout)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "DATABASE_UNAVAILABLE" {
		t.Fatalf("ready error body=%v", body)
	}
}

func TestLoginDistinguishesCredentialsAndDatabaseUnavailable(t *testing.T) {
	server, st := newAvailabilityTestServer(t)
	hash, err := auth.HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.User().Create(&store.User{ID: "user", Email: "user@example.com", PasswordHash: hash, OTPVerified: true}); err != nil {
		t.Fatal(err)
	}

	login := func(ctx context.Context, password string) *httptest.ResponseRecorder {
		t.Helper()
		payload := []byte(`{"email":"user@example.com","password":"` + password + `"}`)
		request := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(payload)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.router.ServeHTTP(recorder, request)
		return recorder
	}

	if recorder := login(context.Background(), "wrong-password"); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := login(context.Background(), "correct-password"); recorder.Code != http.StatusOK {
		t.Fatalf("valid login status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	conn, err := st.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	recorder := login(context.Background(), "correct-password")
	_ = conn.Close()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("database unavailable status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if elapsed := time.Since(started); elapsed < loginDatabaseTimeout || elapsed > loginDatabaseTimeout+time.Second {
		t.Fatalf("login timeout=%s, expected about %s", elapsed, loginDatabaseTimeout)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "DATABASE_UNAVAILABLE" {
		t.Fatalf("database unavailable response=%v", body)
	}
}

func TestEquityHistoryUsesDatabaseWithoutLiveTraderFetch(t *testing.T) {
	server, st := newAvailabilityTestServer(t)
	if _, err := st.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance) VALUES('chart','Chart','model','exchange',100)`); err != nil {
		t.Fatal(err)
	}
	if err := st.Equity().Save(&store.EquitySnapshot{
		TraderID: "chart", Timestamp: time.Now().UTC().Add(-2 * time.Minute),
		TotalEquity: 105, Balance: 100, UnrealizedPnL: 5,
	}); err != nil {
		t.Fatal(err)
	}

	result := server.getEquityHistoryForTraders(context.Background(), []string{"chart"})
	histories, ok := result["histories"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected histories type: %T", result["histories"])
	}
	history, ok := histories["chart"].([]map[string]interface{})
	if !ok || len(history) != 1 {
		t.Fatalf("history=%#v; cache miss must return database history without live trader access", histories["chart"])
	}
	if history[0]["total_equity"] != float64(105) {
		t.Fatalf("history point=%v", history[0])
	}
}
