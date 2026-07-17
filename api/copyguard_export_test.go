package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"nofx/store"
)

func TestExportRiskCycleUsesTraderNameAndOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st, err := store.New(filepath.Join(t.TempDir(), "copyguard-export.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.User().Create(&store.User{ID: "user-1", Email: "owner@example.com", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.User().Create(&store.User{ID: "user-2", Email: "other@example.com", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Trader().Create(&store.Trader{ID: "trader-1", UserID: "user-1", Name: "156-平凡无奇交易员", AIModelID: "model", ExchangeID: "exchange"}); err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "position", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: store.CopyGuardFollowing, PolicySnapshot: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewCopyTradeHandler(st, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("user_id", "user-1")
	ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(cycle.ID, 10)}}
	handler.ExportRiskCycle(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("owner export status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// schema_version 5 必须带完整 AI 候选/分析、后验评价和默认值代次。
	if !strings.Contains(recorder.Body.String(), "156-平凡无奇交易员") || !strings.Contains(recorder.Body.String(), `"schema_version":5`) || !strings.Contains(recorder.Body.String(), `"watch_samples"`) || !strings.Contains(recorder.Body.String(), `"ai_candidates"`) || !strings.Contains(recorder.Body.String(), `"ai_analyses"`) || !strings.Contains(recorder.Body.String(), `"ai_decision_evaluations"`) || !strings.Contains(recorder.Body.String(), `"ai_effect_summary"`) || !strings.Contains(recorder.Body.String(), `"defaults_version":7`) {
		t.Fatalf("export missing display metadata: %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Set("user_id", "user-2")
	ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(cycle.ID, 10)}}
	handler.ExportRiskCycle(ctx)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-owner export status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
