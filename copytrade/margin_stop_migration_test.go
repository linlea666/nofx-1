package copytrade

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

func TestMigratedPositionLossRatioIncludesFrictionAndLeverage(t *testing.T) {
	cfg := &CopyConfig{RiskSlippageBufferBPS: 10, RiskRoundTripFeeBPS: 10}
	for _, tc := range []struct {
		name string
		side string
		mark float64
	}{
		{name: "long", side: "long", mark: 96},
		{name: "short", side: "short", mark: 104},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ratio, quantity, notional, err := migratedPositionLossRatio(cfg, map[string]interface{}{
				"side": tc.side, "entryPrice": 100.0, "markPrice": tc.mark,
				"positionAmt": 10.0, "leverage": 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Price loss is 40, friction is 2 and initial margin is 100.
			if math.Abs(ratio-0.42) > 1e-12 || quantity != 10 || notional != 1000 {
				t.Fatalf("ratio=%v quantity=%v notional=%v", ratio, quantity, notional)
			}
		})
	}
	if _, _, _, err := migratedPositionLossRatio(cfg, map[string]interface{}{
		"side": "long", "entryPrice": 100.0, "positionAmt": 10.0, "leverage": 10,
	}); err == nil {
		t.Fatal("missing fresh mark price must fail closed")
	}
}

func TestMigrationMarginStopPartialFillSurvivesRestartAndRetriesResidual(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "migration-partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cs := st.CopyTrade()
	first, claimed, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: migrationMarginStopRevisionBase + 1,
		SourceKind:   store.ExecutionIntentSourceMigrationMarginStop,
		CanonicalKey: "migration_margin_stop|t1|99|1", CycleID: 99,
		Action: "close_long", Symbol: "BTCUSDT", Side: "long",
		RequestedQuantity: 10, TargetQuantity: 10, ClientOrderID: "migration-1",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve first claimed=%v err=%v", claimed, err)
	}
	if err = cs.UpdateExecutionIntent(first.ID, store.ExecutionIntentPartiallyFilled,
		"MIGRATION_EXIT_PARTIAL", "", "order-1", 10, 0, 4); err != nil {
		t.Fatal(err)
	}

	// A new integration instance models process restart. The durable partial
	// remains a blocker until exchange reconciliation terminalizes that order.
	restarted := &TraderIntegration{store: st}
	unfinished, err := restarted.hasUnfinishedMigrationIntent(99)
	if err != nil || !unfinished {
		t.Fatalf("partial intent lost across restart: unfinished=%v err=%v", unfinished, err)
	}
	if err = cs.UpdateExecutionIntent(first.ID, store.ExecutionIntentCompletedPartial,
		"MIGRATION_EXIT_PARTIAL_RETRY_REQUIRED", "", "order-1", 10, 0, 4); err != nil {
		t.Fatal(err)
	}
	unfinished, err = restarted.hasUnfinishedMigrationIntent(99)
	if err != nil || unfinished {
		t.Fatalf("terminal partial must release residual retry: unfinished=%v err=%v", unfinished, err)
	}

	second, claimed, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "t1", LeaderPosID: "p1", SourceRevision: migrationMarginStopRevisionBase + 2,
		SourceKind:   store.ExecutionIntentSourceMigrationMarginStop,
		CanonicalKey: "migration_margin_stop|t1|99|2", CycleID: 99,
		Action: "close_long", Symbol: "BTCUSDT", Side: "long",
		RequestedQuantity: 6, TargetQuantity: 6, ClientOrderID: "migration-2",
	})
	if err != nil || !claimed || second.ID == first.ID {
		t.Fatalf("residual retry was not independently durable: second=%+v claimed=%v err=%v", second, claimed, err)
	}
	if count, countErr := cs.CountMigrationMarginStopIntents(99); countErr != nil || count != 2 {
		t.Fatalf("migration retry count=%d err=%v", count, countErr)
	}
}

func TestMigrationMarginStopRetryBackoffIsBounded(t *testing.T) {
	want := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second, 300 * time.Second}
	for i, expected := range want {
		if got := migrationMarginStopRetryBackoff(i + 1); got != expected {
			t.Fatalf("attempt %d backoff=%s want=%s", i+1, got, expected)
		}
	}
}
