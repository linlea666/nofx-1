package copyguardmetrics

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

func TestEvaluateCycleShadowPoliciesIsReportOnlyAndCostAdjusted(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "shadow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO exchanges
		(id,exchange_type,account_name,user_id,name,type,enabled)
		VALUES('exchange-1','okx','test','user-1','okx','cex',1);
		INSERT INTO traders
		(id,user_id,name,ai_model_id,exchange_id,initial_balance,lifecycle_status)
		VALUES('trader-1','user-1','trader','model','exchange-1',1000,'STOPPED')`); err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&store.CopyGuardCycle{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "HYPEUSDT", Side: "long", MarginMode: "cross",
		Status: store.CopyGuardFollowing, PolicySnapshot: "{}",
		FollowerEntryPrice: 100, FollowerNotional: 1000, BaselineNotional: 1000,
		ATRAtStop: 5, LastObservedPrice: 110,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().OpenCopyGuardAttempt(cycle.ID, 0, 100, 1000, 10, 5); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_guard_attempts SET status='STOPPED',
		exit_price=90,stop_fill_price=90,pnl=-100,reconciled=1,closed_at=CURRENT_TIMESTAMP
		WHERE cycle_id=? AND attempt_no=0`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_guard_cycles SET stop_count=1,
		baseline_source='leader_history',last_observed_price=110 WHERE id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []*store.CopyGuardWatchSample{
		{CycleID: cycle.ID, TraderID: cycle.TraderID, AttemptNo: 0, MarkPrice: 92, ATR: 5, Gate: "PRICE_NOT_RETURNED"},
		{CycleID: cycle.ID, TraderID: cycle.TraderID, AttemptNo: 0, MarkPrice: 84, ATR: 5, Gate: "ATR_EXPANSION"},
		{CycleID: cycle.ID, TraderID: cycle.TraderID, AttemptNo: 0, MarkPrice: 95, ATR: 5, Gate: "REENTRY_TRIGGERED"},
		{CycleID: cycle.ID, TraderID: cycle.TraderID, AttemptNo: 0, MarkPrice: 110, ATR: 5, Gate: "LEADER_CLOSED"},
	} {
		if err = st.CopyTrade().SaveCopyGuardWatchSample(sample); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.CopyTrade().CloseCopyGuardCycle(
		cycle.ID, store.CopyGuardLeaderClosed, -100, 100, 5, 0, 0, 5); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		results, evalErr := EvaluateCycleShadowPolicies(st, cycle.ID)
		if evalErr != nil || len(results) != 4 {
			t.Fatalf("pass %d results=%+v err=%v", pass, results, evalErr)
		}
	}
	results, err := st.CopyTrade().ListCopyGuardShadowEvaluations(cycle.ID)
	if err != nil || len(results) != 4 {
		t.Fatalf("persisted shadow rows=%+v err=%v", results, err)
	}
	byPolicy := make(map[string]*store.CopyGuardShadowEvaluation)
	for _, result := range results {
		byPolicy[result.Policy] = result
	}
	if got := byPolicy[store.CopyGuardShadowCurrentStop]; got == nil ||
		got.DataQuality != store.CopyGuardShadowQualityVerified || got.NetPnL != -100 {
		t.Fatalf("current policy was not based on reconciled net result: %+v", got)
	}
	if got := byPolicy[store.CopyGuardShadowWideStopEqualRisk]; got == nil ||
		got.DataQuality != store.CopyGuardShadowQualityEstimated || got.ExitPrice != 84 ||
		got.NetPnL >= 0 {
		t.Fatalf("wide-stop crossing was not conservatively simulated: %+v", got)
	}
	if got := byPolicy[store.CopyGuardShadowStagedReduction]; got == nil ||
		got.NetPnL != -10 {
		t.Fatalf("staged reduction blend/cost is wrong: %+v", got)
	}
	if got := byPolicy[store.CopyGuardShadowProbeReentry25Pct]; got == nil ||
		got.Status != store.CopyGuardShadowScorable || got.NetPnL <= 0 {
		t.Fatalf("probe result did not use recorded trigger path: %+v", got)
	}
	var intentCount int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents WHERE cycle_id=?`,
		cycle.ID).Scan(&intentCount); err != nil {
		t.Fatal(err)
	}
	if intentCount != 0 {
		t.Fatalf("shadow evaluation created live execution work: %d intents", intentCount)
	}
	report, err := BuildShadowPromotionReport(
		st, []string{"trader-1"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "INSUFFICIENT_DATA" {
		t.Fatalf("one shadow cycle incorrectly enabled a strategy: %+v", report)
	}
}
