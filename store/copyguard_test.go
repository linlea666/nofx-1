package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestCopyGuardCycleAndEventLedger(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-1", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 101, FollowerNotional: 1000, AccountEquity: 5000, LastObservedPrice: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.OpenCopyGuardAttempt(cycle.ID, 0, 101, 1000, 10, 2); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardAttemptIdentity(cycle.ID, 0, "follower-pos-0", "entry-0", ""); err != nil {
		t.Fatal(err)
	}
	if err := cs.RecordCopyGuardStop(cycle, 2, 98, -30, 1, 2, "algo-1", map[string]interface{}{"quantity": 10.0}); err != nil {
		t.Fatal(err)
	}
	if err := cs.RecordCopyGuardStop(cycle, 2, 98, -30, 1, 2, "algo-1", map[string]interface{}{"quantity": 10.0}); err != nil {
		t.Fatal(err)
	}
	idempotentCycle, _ := cs.GetCopyGuardCycle(cycle.ID)
	idempotentEvents, _ := cs.ListCopyGuardEvents(cycle.ID)
	if idempotentCycle.StopCount != 1 || idempotentCycle.ActualPnL != -30 || len(idempotentEvents) != 1 {
		t.Fatalf("duplicate stop replay changed ledger: cycle=%+v events=%+v", idempotentCycle, idempotentEvents)
	}
	if err := cs.ReconcileCopyGuardAttempt(cycle.ID, 0, -25, 2, -1, 0); err != nil {
		t.Fatal(err)
	}
	cycle, _ = cs.GetCopyGuardCycle(cycle.ID)
	if cycle.ActualPnL != -25 || cycle.Fees != 2 || cycle.FundingFee != -1 {
		t.Fatalf("attempt reconciliation not reflected in cycle: %+v", cycle)
	}
	if err := cs.UpdateCopyGuardObservation(cycle.ID, CopyGuardReentryPending, cycle.LeaderEntryPrice, cycle.LastObservedPrice, 0); err != nil {
		t.Fatal(err)
	}
	if err := cs.SavePositionMapping(&CopyTradePositionMapping{TraderID: cycle.TraderID, LeaderPosID: cycle.LeaderPosID, LeaderID: cycle.LeaderID, Symbol: cycle.Symbol, Side: cycle.Side, MarginMode: cycle.MarginMode, OpenedAt: time.Now(), OpenPrice: 101, OpenSizeUSD: 1000, LastKnownSize: 10}); err != nil {
		t.Fatal(err)
	}
	if err := cs.MarkStoppedByRisk(cycle.TraderID, cycle.LeaderPosID, -30, 10, 0); err != nil {
		t.Fatal(err)
	}
	cycle, _ = cs.GetCopyGuardCycle(cycle.ID)
	if err := cs.RecordCopyGuardReentryFilled(cycle, 99, 500, 5, 2, map[string]interface{}{"activate_mapping": true, "leader_size": 8.0, "exchange_order_id": "reentry-order-1"}); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardAttemptIdentity(cycle.ID, 1, "follower-pos-1", "entry-1", ""); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != CopyGuardFollowingReentry || got.ReentryCount != 1 || got.StopCount != 1 {
		t.Fatalf("unexpected cycle: %+v", got)
	}
	mapping, err := cs.GetActiveMapping(cycle.TraderID, cycle.LeaderPosID)
	if err != nil || mapping == nil || mapping.OpenPrice != 99 || mapping.LastKnownSize != 8 {
		t.Fatalf("reentry mapping was not committed atomically: mapping=%+v err=%v", mapping, err)
	}
	attempts, err := cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 2 || attempts[0].FollowerPosID != "follower-pos-0" || attempts[1].FollowerPosID != "follower-pos-1" || attempts[1].EntryOrderID != "entry-1" {
		t.Fatalf("attempt identities were overwritten: %+v, %v", attempts, err)
	}
	events, err := cs.ListCopyGuardEvents(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "STOP_TRIGGERED" || events[1].Type != "REENTRY_FILLED" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestParseDBTimeSupportsSQLiteAndRFC3339(t *testing.T) {
	for _, value := range []string{"2026-07-03 13:05:43", "2026-07-03T13:05:43Z", "2026-07-03T13:05:43.123456789Z"} {
		got, err := parseDBTime(value)
		if err != nil || got.Year() != 2026 {
			t.Fatalf("parseDBTime(%q) = %v, %v", value, got, err)
		}
	}
	if _, err := parseDBTime("not-a-time"); err == nil {
		t.Fatal("invalid database timestamp must not silently become zero")
	}
	if got, err := parseNullableDBTime(sql.NullString{}); err != nil || got != nil {
		t.Fatalf("null timestamp = %v, %v", got, err)
	}
}

func TestCopyGuardReentryFillRollsBackWithoutOwnedMapping(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-reentry-atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-atomic", LeaderID: "leader", LeaderPosID: "pos-atomic", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardReentryPending, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 100, FollowerNotional: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err = cs.RecordCopyGuardReentryFilled(cycle, 101, 50, .5, 2, map[string]interface{}{"activate_mapping": true}); err == nil {
		t.Fatal("reentry fill must fail when its stopped mapping is missing")
	}
	fresh, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != CopyGuardReentryPending || fresh.ReentryCount != 0 {
		t.Fatalf("failed atomic commit mutated cycle: %+v", fresh)
	}
	attempts, err := cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("failed atomic commit created attempt: %+v err=%v", attempts, err)
	}
}

func TestCopyGuardTailRiskUsesReconciledPnLPath(t *testing.T) {
	maxDrawdown, worstLoss, cvar95 := copyGuardTailRisk([]float64{10, -4, -20, 8, -2})
	if maxDrawdown != 24 || worstLoss != 20 || cvar95 != 20 {
		t.Fatalf("tail risk = %.2f/%.2f/%.2f, want 24/20/20", maxDrawdown, worstLoss, cvar95)
	}
	maxDrawdown, worstLoss, cvar95 = copyGuardTailRisk([]float64{2, 3})
	if maxDrawdown != 0 || worstLoss != 0 || cvar95 != 0 {
		t.Fatalf("winning path must have zero loss tail metrics: %.2f/%.2f/%.2f", maxDrawdown, worstLoss, cvar95)
	}
}

func TestCopyGuardAccountingNoStopHasZeroGuardEffect(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-accounting.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-accounting", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 1734, FollowerEntryPrice: 1735.19, FollowerNotional: 286.99, AccountEquity: 18})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.OpenCopyGuardAttempt(cycle.ID, 0, 1735.19, 286.99, 0.165, 8); err != nil {
		t.Fatal(err)
	}
	if err := cs.BeginCopyGuardAccounting(cycle.ID, CopyGuardLeaderClosed, "exit-order", 1.986091); err != nil {
		t.Fatal(err)
	}
	pendingSummary, err := cs.CopyGuardSummary([]string{"trader-1"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), CopyGuardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if pendingSummary.ActualPnL != 0 || pendingSummary.BaselinePnL != 0 || pendingSummary.AccountingPendingCount != 1 {
		t.Fatalf("pending accounting polluted summary: %+v", pendingSummary)
	}
	if err := cs.CompleteCopyGuardAccounting(cycle.ID, 0, 1746.73, 1.61, 0.29, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountingStatus != CopyGuardAccountingReconciled || got.ActualPnL != 1.61 || got.NetGuardEffect != 0 {
		t.Fatalf("unexpected reconciled cycle: %+v", got)
	}
	if got.TrackingDifference > -0.37 || got.TrackingDifference < -0.39 {
		t.Fatalf("tracking difference = %v, want about -0.376", got.TrackingDifference)
	}
	summary, err := cs.CopyGuardSummary([]string{"trader-1"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), CopyGuardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ActualPnL != 1.61 || summary.OpportunityCost != 0 || summary.NetGuardEffect != 0 {
		t.Fatalf("no-stop cycle polluted guard metrics: %+v", summary)
	}
}

func TestCopyGuardProtectionRetryIsAtomic(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-retry", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := cs.BeginCopyGuardProtectionRetry(cycle, 0)
	if err != nil || !claimed {
		t.Fatalf("first retry claim = %v, %v", claimed, err)
	}
	claimed, err = cs.BeginCopyGuardProtectionRetry(cycle, 0)
	if err != nil || claimed {
		t.Fatalf("stale retry claim = %v, %v", claimed, err)
	}
	got, _ := cs.GetCopyGuardCycle(cycle.ID)
	events, _ := cs.ListCopyGuardEvents(cycle.ID)
	if got.ProtectionRetries != 1 || len(events) != 1 || events[0].Type != "PROTECTION_RETRY" {
		t.Fatalf("retry was not atomic: cycle=%+v events=%+v", got, events)
	}
}

func TestCopyGuardProtectiveOrderPersistsQuantityStep(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-step.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-step", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	want := &CopyGuardProtectiveOrder{CycleID: cycle.ID, TraderID: "trader-1", AlgoID: "algo-step", AlgoClientID: "cg-step", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 0.064, QuantityStep: 0.001, TriggerPrice: 1711.63, TriggerType: "mark", Status: "live"}
	if err := cs.UpsertCopyGuardProtectiveOrder(want); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardProtectiveOrder(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.QuantityStep != want.QuantityStep {
		t.Fatalf("quantity step = %v, want %v", got.QuantityStep, want.QuantityStep)
	}
	active, err := cs.ListActiveCopyGuardProtectiveOrders("trader-1")
	if err != nil || len(active) != 1 || active[0].QuantityStep != want.QuantityStep {
		t.Fatalf("active protection lost quantity step: %+v, %v", active, err)
	}
}

func TestExistingClosedCopyGuardCycleBecomesLegacyUnverified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copyguard-legacy.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := st.CopyTrade().EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "legacy-pos", Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE copy_guard_cycles SET status='LEADER_CLOSED',closed_at=CURRENT_TIMESTAMP,accounting_status='OPEN' WHERE id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.CopyTrade().GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountingStatus != CopyGuardAccountingLegacyUnverified {
		t.Fatalf("legacy cycle status = %s", got.AccountingStatus)
	}
}

func TestCopyGuardProtectionHealthAndShadowLedger(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyguard-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-1", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 100, FollowerNotional: 1000, AccountEquity: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardProtectionHealth(cycle.ID, CopyGuardProtectionUnknown, 0.4, "timeout", "follower-pos", "", true); err != nil {
		t.Fatal(err)
	}
	got, err := cs.GetCopyGuardCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProtectionStatus != CopyGuardProtectionUnknown || got.ProtectionMissingAt == nil || got.ProtectionRetries != 1 {
		t.Fatalf("unexpected degraded health: %+v", got)
	}
	if _, err := cs.db.Exec(`UPDATE copy_guard_cycles SET protection_missing_at=datetime('now','-1 minute') WHERE id=?`, cycle.ID); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardProtectionHealth(cycle.ID, CopyGuardProtectionVerified, 1, "", "follower-pos", "", false); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.GetCopyGuardCycle(cycle.ID)
	if got.ProtectionMissingAt != nil || got.ProtectionStatus != CopyGuardProtectionVerified || got.ProtectionMissingSeconds < 50 {
		t.Fatalf("health did not recover: %+v", got)
	}

	if err := cs.UpdateCopyGuardShadow(cycle.ID, 100, 100, 1000, 10); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardShadow(cycle.ID, 100, 110, 500, 5); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.GetCopyGuardCycle(cycle.ID)
	if got.BaselineRealizedPnL < 49.99 || got.BaselineRealizedPnL > 50.01 {
		t.Fatalf("shadow partial close pnl = %v, want 50", got.BaselineRealizedPnL)
	}
	if err := cs.UpdateCopyGuardObservation(cycle.ID, CopyGuardReentryPending, got.LeaderEntryPrice, got.LastObservedPrice, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.GetCopyGuardCycle(cycle.ID)
	if err := cs.RecordCopyGuardReentryFilled(got, 105, 500, 5, 2, map[string]interface{}{"test": true}); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.GetCopyGuardCycle(cycle.ID)
	attempts, err := cs.ListCopyGuardAttempts(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != CopyGuardFollowingReentry || got.ReentryCount != 1 || len(attempts) != 1 {
		t.Fatalf("transactional reentry incomplete: cycle=%+v attempts=%+v", got, attempts)
	}
	summary, err := cs.CopyGuardSummary([]string{"trader-1"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), CopyGuardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectedCount != 1 || summary.AverageCoverage != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

// 观察更新与周期关闭存在竞态（止损与领航员平仓同一轮发生）：
// 已关闭的周期绝不能被 UpdateCopyGuardObservation 改回 STOPPED_WATCHING，
// 也不能被 UpdateCopyGuardObservedPrice 改写观测价。
func TestObservationUpdatesGuardClosedCycle(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "observation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-obs", Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing, PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 100, FollowerNotional: 1000, AccountEquity: 5000, LastObservedPrice: 100})
	if err != nil {
		t.Fatal(err)
	}

	// 跟随期轻量刷新：开放周期允许更新观测价
	if err := cs.UpdateCopyGuardObservedPrice("trader-1", "pos-obs", 105); err != nil {
		t.Fatal(err)
	}
	got, _ := cs.GetCopyGuardCycle(cycle.ID)
	if got.LastObservedPrice != 105 {
		t.Fatalf("open cycle must accept observed price refresh, got %.2f", got.LastObservedPrice)
	}

	if err := cs.CloseCopyGuardCycle(cycle.ID, CopyGuardLeaderClosed, -2, -3, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardObservation(cycle.ID, CopyGuardStoppedWatching, 100, 111, 5); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateCopyGuardObservedPrice("trader-1", "pos-obs", 120); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.GetCopyGuardCycle(cycle.ID)
	if got.Status != CopyGuardLeaderClosed {
		t.Fatalf("closed cycle status must not be overwritten, got %s", got.Status)
	}
	if got.LastObservedPrice != 105 {
		t.Fatalf("closed cycle observed price must stay frozen, got %.2f", got.LastObservedPrice)
	}
}

// 默认值代次迁移（当前代次 5）：低代次行仅把等于旧默认值的 max_chase(0)/
// cooldown(60) 替换为新默认值；代次 5 回补只针对被代次 4 规则降为 1 的
// max_reentries（fromVersion==4 且值==1）；用户显式配置的其他值保留；
// 已达当前代次的策略不再重扫。
func TestMigratePolicyDefaults(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "defaults-v4.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()

	seed := func(traderID string, p CopyGuardPolicy) {
		t.Helper()
		// copy_guard_policies.trader_id 外键引用 traders(id)，先建 trader 行
		if _, err := st.DB().Exec(`INSERT INTO traders(id, name, ai_model_id, exchange_id, initial_balance) VALUES(?,?,?,?,0)`, traderID, traderID, "m", "e"); err != nil {
			t.Fatal(err)
		}
		b, merr := json.Marshal(p)
		if merr != nil {
			t.Fatal(merr)
		}
		if _, err := st.DB().Exec(`INSERT INTO copy_guard_policies(trader_id, policy_json) VALUES(?,?)`, traderID, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	load := func(traderID string) CopyGuardPolicy {
		t.Helper()
		var raw string
		if err := st.DB().QueryRow(`SELECT policy_json FROM copy_guard_policies WHERE trader_id=?`, traderID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var p CopyGuardPolicy
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// v7 cannot distinguish an old inherited value from an explicit choice, so
	// every stored number must survive unchanged.
	seed("t-old-defaults", CopyGuardPolicy{Version: 4, ReentryMaxChaseATR: 0, ReentryCooldownSec: 60, MaxReentries: 2, DefaultsVersion: 2})
	// 低代次存量策略：用户显式改过 → 应保留
	seed("t-custom", CopyGuardPolicy{Version: 4, ReentryMaxChaseATR: 1.2, ReentryCooldownSec: 900, MaxReentries: 3, DefaultsVersion: 2})
	// Even a value previously produced by a migration is preserved.
	seed("t-gen4-reduced", CopyGuardPolicy{Version: 4, ReentryMaxChaseATR: 0, ReentryCooldownSec: 60, MaxReentries: 1, DefaultsVersion: 4})
	// 已是当前代次：即使值等于旧默认也不得再动（用户设回旧值的选择）
	seed("t-current", CopyGuardPolicy{Version: 4, ReentryMaxChaseATR: 0, ReentryCooldownSec: 60, MaxReentries: 1, DefaultsVersion: copyGuardPolicyDefaultsVersion})

	cs.migrateCopyGuardPolicyDefaults()

	if p := load("t-old-defaults"); p.ReentryMaxChaseATR != 0 || p.ReentryCooldownSec != 60 || p.MaxReentries != 2 || p.ReentryDecisionMode != "legacy_rule" || p.DefaultsVersion != copyGuardPolicyDefaultsVersion {
		t.Fatalf("v7 migration must preserve stored values and pin legacy mode: %+v", p)
	}
	if p := load("t-custom"); p.ReentryMaxChaseATR != 1.2 || p.ReentryCooldownSec != 900 || p.MaxReentries != 3 || p.DefaultsVersion != copyGuardPolicyDefaultsVersion {
		t.Fatalf("explicit values must be preserved: %+v", p)
	}
	if p := load("t-gen4-reduced"); p.MaxReentries != 1 || p.ReentryMaxChaseATR != 0 || p.ReentryCooldownSec != 60 || p.ReentryDecisionMode != "legacy_rule" || p.DefaultsVersion != copyGuardPolicyDefaultsVersion {
		t.Fatalf("v7 migration must not reinterpret prior values: %+v", p)
	}
	if p := load("t-current"); p.ReentryMaxChaseATR != 0 || p.ReentryCooldownSec != 60 || p.MaxReentries != 1 {
		t.Fatalf("policies at the current version must not be rescanned: %+v", p)
	}
}

// v7 migration never rewrites main-table risk values; equality with an old
// default is not proof that the user did not choose it explicitly.
func TestMigratePolicyDefaultsGen6(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "defaults-gen6.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()

	// 播种 trader + copy_guard_policy(DefaultsVersion<6) + copy_trade_configs 行
	seed := func(traderID string, defaultsVer int, atr float64, levFallback int) {
		t.Helper()
		if _, err := st.DB().Exec(`INSERT INTO traders(id, name, ai_model_id, exchange_id, initial_balance) VALUES(?,?,?,?,0)`, traderID, traderID, "m", "e"); err != nil {
			t.Fatal(err)
		}
		b, merr := json.Marshal(CopyGuardPolicy{Version: 4, DefaultsVersion: defaultsVer})
		if merr != nil {
			t.Fatal(merr)
		}
		if _, err := st.DB().Exec(`INSERT INTO copy_guard_policies(trader_id, policy_json) VALUES(?,?)`, traderID, string(b)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO copy_trade_configs(trader_id, provider_type, leader_id, risk_atr_multiplier, risk_leverage_fallback) VALUES(?, 'okx', 'lead', ?, ?)`, traderID, atr, levFallback); err != nil {
			t.Fatal(err)
		}
	}
	loadCfg := func(traderID string) (float64, int) {
		t.Helper()
		var atr float64
		var lev int
		if err := st.DB().QueryRow(`SELECT risk_atr_multiplier, risk_leverage_fallback FROM copy_trade_configs WHERE trader_id=?`, traderID).Scan(&atr, &lev); err != nil {
			t.Fatal(err)
		}
		return atr, lev
	}

	// Values equal to old defaults are still preserved.
	seed("t-old", 5, 1.5, 1)
	// 用户显式行：1.8 与已关闭 fallback 都不得被改
	seed("t-custom", 5, 1.8, 0)
	// 已达当前代次：不重扫（即使值等于旧默认）
	seed("t-current", copyGuardPolicyDefaultsVersion, 1.5, 1)

	cs.migrateCopyGuardPolicyDefaults()

	if atr, lev := loadCfg("t-old"); atr != 1.5 || lev != 1 {
		t.Fatalf("存量主表值不得自动覆盖，got atr=%.2f lev=%d", atr, lev)
	}
	if atr, lev := loadCfg("t-custom"); atr != 1.8 || lev != 0 {
		t.Fatalf("用户显式值必须保留，got atr=%.2f lev=%d", atr, lev)
	}
	if atr, lev := loadCfg("t-current"); atr != 1.5 || lev != 1 {
		t.Fatalf("已达当前代次的行不得被重扫，got atr=%.2f lev=%d", atr, lev)
	}
}
