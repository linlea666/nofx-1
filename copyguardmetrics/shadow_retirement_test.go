package copyguardmetrics

import (
	"nofx/store"
	"path/filepath"
	"testing"
	"time"
)

func TestShadowRuntimeReadOnlyHistoryAndNoPromotion(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "retired-shadow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{TraderID: "t", LeaderID: "l", LeaderPosID: "p", Symbol: "ETHUSDT", Side: "long", PolicySnapshot: `{"version":4}`})
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		rows, err := EvaluateCycleShadowPolicies(st, c.ID)
		if err != nil || len(rows) != 0 {
			t.Fatalf("runtime created shadow rows: %+v %v", rows, err)
		}
	}
	if err = st.CopyTrade().SaveCopyGuardShadowEvaluation(&store.CopyGuardShadowEvaluation{CycleID: c.ID, TraderID: "t", Policy: store.CopyGuardShadowFirstEntryPositionMargin80, EvaluationVersion: 2, Status: store.CopyGuardShadowScorable, NetPnL: 123}); err != nil {
		t.Fatal(err)
	}
	rows, err := EvaluateCycleShadowPolicies(st, c.ID)
	if err != nil || len(rows) != 1 || rows[0].NetPnL != 123 {
		t.Fatalf("history changed: %+v %v", rows, err)
	}
	report, err := BuildShadowPromotionReport(st, []string{"t"}, time.Now().Add(-time.Hour), time.Now())
	if err != nil || len(report.Policies) != 0 || report.MinimumIndependentCycles != 0 {
		t.Fatalf("retired promotion gate ran: %+v %v", report, err)
	}
}
