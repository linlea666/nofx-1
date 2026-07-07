package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestCopyEventDedupAndQuery 验证统一跟单事件日志的幂等去重、过滤与分页。
func TestCopyEventDedupAndQuery(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "copyevents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()

	base := &CopyTradeEvent{
		TraderID: "trader-1", LeaderID: "leader", ProviderType: "binance",
		Category: CopyEventCategoryAction, EventType: CopyEventTypeOpen, Severity: CopyEventSeverityInfo,
		Symbol: "BTCUSDT", Side: "long", Status: "success", Notional: 1000,
		DedupKey: "a|trader-1|fill-1|OPEN|success",
	}
	if err := cs.LogCopyEvent(base); err != nil {
		t.Fatal(err)
	}
	// 相同 dedup_key 重复写入应被 INSERT OR IGNORE 幂等吞掉（模拟轮询/重启重放）。
	dup := *base
	if err := cs.LogCopyEvent(&dup); err != nil {
		t.Fatal(err)
	}
	// 不同分类事件（dedup_key 为空，不去重）。
	if err := cs.LogCopyEvent(&CopyTradeEvent{
		TraderID: "trader-1", ProviderType: "binance",
		Category: CopyEventCategoryError, EventType: CopyEventTypeClose, Severity: CopyEventSeverityError,
		Symbol: "ETHUSDT", Side: "short", Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	// 另一个 trader 的事件（作用域隔离验证）。
	if err := cs.LogCopyEvent(&CopyTradeEvent{
		TraderID: "trader-2", ProviderType: "okx",
		Category: CopyEventCategoryStopLoss, EventType: "STOP_TRIGGERED", Severity: CopyEventSeverityWarn,
		Symbol: "BTCUSDT", Side: "long",
	}); err != nil {
		t.Fatal(err)
	}

	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC().Add(time.Hour)

	// trader-1 应有 2 条（去重后 OPEN 仅 1 条 + 1 条 CLOSE）。
	total, err := cs.CountCopyEvents([]string{"trader-1"}, from, to, CopyEventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("expected 2 events for trader-1 after dedup, got %d", total)
	}

	// 分类过滤：action 仅 1 条。
	actions, err := cs.QueryCopyEvents([]string{"trader-1"}, from, to, CopyEventFilter{Category: CopyEventCategoryAction}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].EventType != CopyEventTypeOpen {
		t.Fatalf("category filter failed: %+v", actions)
	}

	// 作用域：trader-2 的事件不应出现在 trader-1 查询里。
	all1, err := cs.QueryCopyEvents([]string{"trader-1"}, from, to, CopyEventFilter{}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all1 {
		if e.TraderID != "trader-1" {
			t.Fatalf("scope leak: got trader %s in trader-1 query", e.TraderID)
		}
	}

	// 分页：limit=1 分两页取回 trader-1 的 2 条。
	page0, _ := cs.QueryCopyEvents([]string{"trader-1"}, from, to, CopyEventFilter{}, 1, 0)
	page1, _ := cs.QueryCopyEvents([]string{"trader-1"}, from, to, CopyEventFilter{}, 1, 1)
	if len(page0) != 1 || len(page1) != 1 || page0[0].ID == page1[0].ID {
		t.Fatalf("pagination failed: p0=%+v p1=%+v", page0, page1)
	}
}

// TestGuardEventMirroredToCopyEvents 验证 Copy Guard 事件（含事务内直插的
// STOP_TRIGGERED / REENTRY_FILLED）被镜像进统一跟单事件日志，且高频类型被排除。
func TestGuardEventMirroredToCopyEvents(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	cycle, err := cs.EnsureCopyGuardCycle(&CopyGuardCycle{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pos-1",
		Symbol: "BTCUSDT", Side: "long", MarginMode: "cross", Status: CopyGuardFollowing,
		PolicySnapshot: "{}", LeaderEntryPrice: 100, FollowerEntryPrice: 101, FollowerNotional: 1000,
		AccountEquity: 5000, LastObservedPrice: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.OpenCopyGuardAttempt(cycle.ID, 0, 101, 1000, 10, 2); err != nil {
		t.Fatal(err)
	}
	// 事务内直插的止损事件（关键：不走 SaveCopyGuardEvent）。
	if err := cs.RecordCopyGuardStop(cycle, 2, 98, -30, 1, 2, "algo-1", map[string]interface{}{"quantity": 10.0}); err != nil {
		t.Fatal(err)
	}
	// 白名单命中的保护事件（走 SaveCopyGuardEvent）。
	if err := cs.SaveCopyGuardEvent(&CopyGuardEvent{CycleID: cycle.ID, TraderID: "trader-1", Type: "GUARD_UNPROTECTABLE"}); err != nil {
		t.Fatal(err)
	}
	// 高频/内部明细类型：不应被镜像。
	if err := cs.SaveCopyGuardEvent(&CopyGuardEvent{CycleID: cycle.ID, TraderID: "trader-1", Type: "WATCH_SUMMARY"}); err != nil {
		t.Fatal(err)
	}

	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC().Add(time.Hour)
	events, err := cs.QueryCopyEvents([]string{"trader-1"}, from, to, CopyEventFilter{}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]*CopyTradeEvent{}
	for _, e := range events {
		seen[e.EventType] = e
	}
	if _, ok := seen["STOP_TRIGGERED"]; !ok {
		t.Fatalf("STOP_TRIGGERED not mirrored to copy_trade_events: %+v", events)
	}
	if _, ok := seen["GUARD_UNPROTECTABLE"]; !ok {
		t.Fatalf("GUARD_UNPROTECTABLE not mirrored: %+v", events)
	}
	if _, ok := seen["WATCH_SUMMARY"]; ok {
		t.Fatalf("WATCH_SUMMARY should NOT be mirrored (high-frequency)")
	}
	// 镜像应回填 cycle 上下文与分类。
	stop := seen["STOP_TRIGGERED"]
	if stop.Symbol != "BTCUSDT" || stop.Side != "long" || stop.Category != CopyEventCategoryStopLoss || stop.CycleID != cycle.ID {
		t.Fatalf("mirrored stop event context wrong: %+v", stop)
	}
	if stop.ProviderType != "okx" {
		t.Fatalf("mirrored guard event should be provider okx, got %s", stop.ProviderType)
	}
}
