package api

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"nofx/store"
)

func TestResumeFollowingRequiresOwnerAndRunningTrader(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "resume.db"))
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
		{"other", `{"leader_pos_id":"p"}`, 404},
		{"owner", `{}`, 400},
		{"owner", `{"leader_pos_id":"p"}`, 409},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("user_id", tc.user)
		c.Params = gin.Params{{Key: "trader_id", Value: "t"}}
		c.Request = httptest.NewRequest("POST", "/resume-follow", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.ResumeFollowPosition(c)
		if w.Code != tc.status {
			t.Fatalf("user=%s body=%s: status %d response %s", tc.user, tc.body, w.Code, w.Body.String())
		}
	}
}
