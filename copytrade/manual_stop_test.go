package copytrade

// 仓位级手动停止跟单（manual_stopped 生命周期）测试
//
// 覆盖：
//  1. 停跟后同 posId 的加仓信号不跟随
//  2. 停跟后同币种同方向的新 posId 开仓被屏蔽；其他币种照常跟随
//  3. 停跟后减仓/平仓信号不跟随
//  4. 快照差分不为 manual_stopped 映射生成合成信号
//  5. 收尾条件：领航员结束 + 本地已平（连续确认）→ closed，屏蔽解除
//  6. store 层状态机：仅 active 可转入，幂等

import (
	"errors"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

var errTestPositionsUnavailable = errors.New("positions API unavailable")

func saveManualTestMapping(t *testing.T, st *store.Store, posID, symbol, side string, lastKnownSize float64) {
	t.Helper()
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:      "test-trader",
		LeaderPosID:   posID,
		LeaderID:      "leader",
		Symbol:        symbol,
		Side:          side,
		MarginMode:    "cross",
		OpenedAt:      time.Now(),
		OpenPrice:     2000,
		OpenSizeUSD:   lastKnownSize * 2000,
		LastKnownSize: lastKnownSize,
	}); err != nil {
		t.Fatalf("save active mapping: %v", err)
	}
}

func markManualStopped(t *testing.T, st *store.Store, posID string) {
	t.Helper()
	changed, err := st.CopyTrade().MarkManualStopped("test-trader", posID)
	if err != nil || !changed {
		t.Fatalf("mark manual stopped: changed=%t err=%v", changed, err)
	}
}

func manualTestPosition(posID, symbol string, side SideType, size float64) *Position {
	return &Position{
		Symbol: symbol, Side: side, Size: size,
		EntryPrice: 2000, MarkPrice: 1990, Leverage: 10,
		MarginMode: "cross", PositionValue: size * 1990, PosID: posID,
	}
}

// 1. 同 posId 加仓信号不跟随
func TestManualStoppedBlocksAddOnSamePosID(t *testing.T) {
	const posID = "manual-eth-short"
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	saveManualTestMapping(t, st, posID, "ETHUSDT", "short", 1)
	markManualStopped(t, st, posID)

	// 领航员加仓到 2
	e.leaderState.Positions[posID] = manualTestPosition(posID, "ETHUSDT", SideShort, 2)

	signal := e.buildSignal(&Fill{
		ID: "manual-add", Symbol: "ETHUSDT", Side: "sell", PositionSide: SideShort,
		Action: ActionAdd, Price: 2000, Size: 1, Value: 2000, Timestamp: time.Now(),
	})
	result := e.matchOpenAddSignal(signal, e.buildLeaderPosMap())
	if result.ShouldFollow {
		t.Fatalf("manual_stopped posId must not follow add: %+v", result)
	}
}

// 2. 同币种同方向新 posId 开仓被屏蔽；其他币种照常跟随
func TestManualStoppedBlocksNewPosIDSameSymbolSideOnly(t *testing.T) {
	const stoppedPosID = "manual-eth-short"
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	saveManualTestMapping(t, st, stoppedPosID, "ETHUSDT", "short", 1)
	markManualStopped(t, st, stoppedPosID)

	// 领航员：原仓仍在 + 开了一个同币种同方向的新 posId
	e.leaderState.Positions[stoppedPosID] = manualTestPosition(stoppedPosID, "ETHUSDT", SideShort, 1)
	e.leaderState.Positions["new-eth-short"] = manualTestPosition("new-eth-short", "ETHUSDT", SideShort, 3)

	signal := e.buildSignal(&Fill{
		ID: "new-open", Symbol: "ETHUSDT", Side: "sell", PositionSide: SideShort,
		Action: ActionOpen, Price: 2000, Size: 3, Value: 6000, Timestamp: time.Now(),
	})
	result := e.matchOpenAddSignal(signal, e.buildLeaderPosMap())
	if result.ShouldFollow {
		t.Fatalf("new posId on manually stopped symbol+side must be skipped: %+v", result)
	}
	if !strings.Contains(result.Reason, "手动停止") {
		t.Fatalf("unexpected skip reason: %s", result.Reason)
	}

	// 同币种反方向（新 posId ETHUSDT long）不受屏蔽
	e.leaderState.Positions["new-eth-long"] = manualTestPosition("new-eth-long", "ETHUSDT", SideLong, 2)
	longSignal := e.buildSignal(&Fill{
		ID: "new-open-long", Symbol: "ETHUSDT", Side: "buy", PositionSide: SideLong,
		Action: ActionOpen, Price: 2000, Size: 2, Value: 4000, Timestamp: time.Now(),
	})
	longResult := e.matchOpenAddSignal(longSignal, e.buildLeaderPosMap())
	if !longResult.ShouldFollow || longResult.Action != ActionOpen || longResult.PosID != "new-eth-long" {
		t.Fatalf("opposite side must not be blocked: %+v", longResult)
	}

	// 其他币种（BTCUSDT short）照常跟随
	e.leaderState.Positions["new-btc-short"] = manualTestPosition("new-btc-short", "BTCUSDT", SideShort, 1)
	btcSignal := e.buildSignal(&Fill{
		ID: "btc-open", Symbol: "BTCUSDT", Side: "sell", PositionSide: SideShort,
		Action: ActionOpen, Price: 60000, Size: 1, Value: 60000, Timestamp: time.Now(),
	})
	btcResult := e.matchOpenAddSignal(btcSignal, e.buildLeaderPosMap())
	if !btcResult.ShouldFollow || btcResult.Action != ActionOpen || btcResult.PosID != "new-btc-short" {
		t.Fatalf("other symbols must keep following: %+v", btcResult)
	}
}

// 3. 减仓/平仓信号不跟随
func TestManualStoppedBlocksCloseReduceSignals(t *testing.T) {
	const posID = "manual-eth-short"
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	saveManualTestMapping(t, st, posID, "ETHUSDT", "short", 2)
	markManualStopped(t, st, posID)

	// 领航员减仓到 1
	e.leaderState.Positions[posID] = manualTestPosition(posID, "ETHUSDT", SideShort, 1)
	reduceSignal := e.buildSignal(&Fill{
		ID: "manual-reduce", Symbol: "ETHUSDT", Side: "buy", PositionSide: SideShort,
		Action: ActionReduce, Price: 2000, Size: 1, Value: 2000, Timestamp: time.Now(),
	})
	if result := e.matchCloseReduceSignal(reduceSignal, e.buildLeaderPosMap()); result.ShouldFollow {
		t.Fatalf("manual_stopped must not follow reduce: %+v", result)
	}

	// 领航员全平（posId 消失）
	delete(e.leaderState.Positions, posID)
	closeSignal := e.buildSignal(&Fill{
		ID: "manual-close", Symbol: "ETHUSDT", Side: "buy", PositionSide: SideShort,
		Action: ActionClose, Price: 2000, Size: 1, Value: 2000, Timestamp: time.Now(),
	})
	if result := e.matchCloseReduceSignal(closeSignal, e.buildLeaderPosMap()); result.ShouldFollow {
		t.Fatalf("manual_stopped must not follow close: %+v", result)
	}
}

// 4. 快照差分不为 manual_stopped 映射生成合成信号
func TestManualStoppedSnapshotDifferGeneratesNoFills(t *testing.T) {
	const posID = "manual-eth-short"
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	saveManualTestMapping(t, st, posID, "ETHUSDT", "short", 1)
	markManualStopped(t, st, posID)

	// 领航员加仓（size 1 → 3）：不得生成 add 合成信号
	e.leaderState.Positions[posID] = manualTestPosition(posID, "ETHUSDT", SideShort, 3)
	if fills := e.detectBinancePositionSnapshotFills(); len(fills) != 0 {
		t.Fatalf("snapshot differ must not synthesize fills for manual_stopped add: %+v", fills)
	}

	// 领航员全平（posId 消失）：不得生成 close 合成信号
	delete(e.leaderState.Positions, posID)
	if fills := e.detectBinancePositionSnapshotFills(); len(fills) != 0 {
		t.Fatalf("snapshot differ must not synthesize fills for manual_stopped close: %+v", fills)
	}
}

// 5. 收尾条件：领航员结束 + 本地已平（连续确认）→ closed，屏蔽解除
func TestManualStoppedClosesAfterLeaderExitAndLocalFlat(t *testing.T) {
	const posID = "manual-eth-short"
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	saveManualTestMapping(t, st, posID, "ETHUSDT", "short", 1)
	markManualStopped(t, st, posID)

	localHolding := []*Position{manualTestPosition("", "ETHUSDT", SideShort, 0.5)}
	local := localHolding
	e.getFollowerPositionsResult = func() (map[string]*Position, error) {
		out := map[string]*Position{}
		for i, p := range local {
			out[p.Symbol+"_"+string(p.Side)+"_"+string(rune('a'+i))] = p
		}
		return out, nil
	}

	// 场景 A：领航员原仓还在 → 无论本地状态如何都不收尾
	e.leaderState.Positions[posID] = manualTestPosition(posID, "ETHUSDT", SideShort, 1)
	for i := 0; i < manualStopFlatConfirmThreshold+1; i++ {
		e.checkManualStoppedClosed(e.buildLeaderPosMap())
	}
	if m, err := st.CopyTrade().GetMapping("test-trader", posID); err != nil || m == nil || m.Status != store.MappingStatusManualStopped {
		t.Fatalf("mapping must stay manual_stopped while leader holds: %+v err=%v", m, err)
	}

	// 场景 B：领航员已平但本地仍持仓 → 保持 manual_stopped
	delete(e.leaderState.Positions, posID)
	for i := 0; i < manualStopFlatConfirmThreshold+1; i++ {
		e.checkManualStoppedClosed(e.buildLeaderPosMap())
	}
	if m, _ := st.CopyTrade().GetMapping("test-trader", posID); m == nil || m.Status != store.MappingStatusManualStopped {
		t.Fatalf("mapping must stay manual_stopped while local position exists: %+v", m)
	}

	// 场景 C：本地也平了 → 需连续 manualStopFlatConfirmThreshold 轮确认后收尾
	local = nil
	for i := 0; i < manualStopFlatConfirmThreshold-1; i++ {
		e.checkManualStoppedClosed(e.buildLeaderPosMap())
		if m, _ := st.CopyTrade().GetMapping("test-trader", posID); m == nil || m.Status != store.MappingStatusManualStopped {
			t.Fatalf("mapping closed before flat-confirm threshold (round %d): %+v", i+1, m)
		}
	}
	e.checkManualStoppedClosed(e.buildLeaderPosMap())
	if m, _ := st.CopyTrade().GetMappingForReconciliation("test-trader", posID); m == nil || m.Status != store.MappingStatusClosed {
		t.Fatalf("mapping must close after leader exit + local flat: %+v", m)
	}

	// 屏蔽解除：领航员再开同币种同方向新仓 → 正常跟随
	e.leaderState.Positions["new-eth-short"] = manualTestPosition("new-eth-short", "ETHUSDT", SideShort, 2)
	signal := e.buildSignal(&Fill{
		ID: "reopen", Symbol: "ETHUSDT", Side: "sell", PositionSide: SideShort,
		Action: ActionOpen, Price: 2000, Size: 2, Value: 4000, Timestamp: time.Now(),
	})
	result := e.matchOpenAddSignal(signal, e.buildLeaderPosMap())
	if !result.ShouldFollow || result.Action != ActionOpen || result.PosID != "new-eth-short" {
		t.Fatalf("block must be released after lifecycle close: %+v", result)
	}
}

// 5b. 本地持仓查询失败时本轮不收尾（防误撤保护）
func TestManualStoppedSkipsFinalizeOnPositionQueryFailure(t *testing.T) {
	const posID = "manual-eth-short"
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	saveManualTestMapping(t, st, posID, "ETHUSDT", "short", 1)
	markManualStopped(t, st, posID)

	// 领航员已平，但持仓查询报错
	queryErr := true
	e.getFollowerPositionsResult = func() (map[string]*Position, error) {
		if queryErr {
			return nil, errTestPositionsUnavailable
		}
		return map[string]*Position{}, nil
	}
	for i := 0; i < manualStopFlatConfirmThreshold+2; i++ {
		e.checkManualStoppedClosed(e.buildLeaderPosMap())
	}
	if m, _ := st.CopyTrade().GetMapping("test-trader", posID); m == nil || m.Status != store.MappingStatusManualStopped {
		t.Fatalf("mapping must not close while position query fails: %+v", m)
	}
}

// 6. store 层状态机：仅 active 可转入；幂等；GetMapping 能看到 manual_stopped
func TestMarkManualStoppedStateMachine(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	_ = e

	// active → manual_stopped ✓
	saveManualTestMapping(t, st, "pos-a", "ETHUSDT", "short", 1)
	changed, err := st.CopyTrade().MarkManualStopped("test-trader", "pos-a")
	if err != nil || !changed {
		t.Fatalf("active mapping must be stoppable: changed=%t err=%v", changed, err)
	}
	// GetMapping（无状态筛选）必须能看到 manual_stopped，否则会被当成新开仓
	if m, err := st.CopyTrade().GetMapping("test-trader", "pos-a"); err != nil || m == nil || m.Status != store.MappingStatusManualStopped {
		t.Fatalf("GetMapping must surface manual_stopped: %+v err=%v", m, err)
	}
	// 幂等：重复调用不报错、无变化
	changed, err = st.CopyTrade().MarkManualStopped("test-trader", "pos-a")
	if err != nil || changed {
		t.Fatalf("repeat stop must be a no-op: changed=%t err=%v", changed, err)
	}

	// ignored 不可转入
	if err := st.CopyTrade().SaveIgnoredPosition("test-trader", "leader", "pos-b", "BTCUSDT", "long", "cross"); err != nil {
		t.Fatal(err)
	}
	changed, err = st.CopyTrade().MarkManualStopped("test-trader", "pos-b")
	if err != nil || changed {
		t.Fatalf("ignored mapping must not be stoppable: changed=%t err=%v", changed, err)
	}

	// symbol+side 屏蔽查询
	if blocked, err := st.CopyTrade().HasManualStoppedBySymbolSide("test-trader", "ETHUSDT", "short"); err != nil || !blocked {
		t.Fatalf("ETHUSDT short must be blocked: blocked=%t err=%v", blocked, err)
	}
	if blocked, _ := st.CopyTrade().HasManualStoppedBySymbolSide("test-trader", "ETHUSDT", "long"); blocked {
		t.Fatal("ETHUSDT long must not be blocked")
	}
	if blocked, _ := st.CopyTrade().HasManualStoppedBySymbolSide("test-trader", "BTCUSDT", "short"); blocked {
		t.Fatal("BTCUSDT short must not be blocked")
	}

	// manual_stopped → closed 后屏蔽解除
	if err := st.CopyTrade().MarkManualStoppedAsClosed("test-trader", "pos-a"); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.CopyTrade().GetMappingForReconciliation("test-trader", "pos-a"); m == nil || m.Status != store.MappingStatusClosed {
		t.Fatalf("close transition failed: %+v", m)
	}
	if blocked, _ := st.CopyTrade().HasManualStoppedBySymbolSide("test-trader", "ETHUSDT", "short"); blocked {
		t.Fatal("block must be released after close")
	}
}
