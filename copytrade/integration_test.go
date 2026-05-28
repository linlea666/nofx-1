package copytrade

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

func TestExecFailureDedupKeyStableForSameFailureState(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}
	err := errors.New("short position not found")
	dec := &decision.Decision{
		Action:        "reduce_short",
		Symbol:        "ETHUSDT",
		CloseRatio:    0.5154639175,
		MarginMode:    "cross",
		LeaderPosID:   "1243719130_ETHUSDT_SHORT",
		LeaderPosSize: 0.047,
	}

	first := ti.execFailureDedupKey(dec, err)
	second := ti.execFailureDedupKey(dec, err)

	if first != second {
		t.Fatalf("same failure state produced different keys:\n%s\n%s", first, second)
	}
}

func TestExecFailureDedupKeyChangesWhenLeaderOperationChanges(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}
	err := errors.New("short position not found")
	base := &decision.Decision{
		Action:        "reduce_short",
		Symbol:        "ETHUSDT",
		CloseRatio:    0.5154639175,
		MarginMode:    "cross",
		LeaderPosID:   "1243719130_ETHUSDT_SHORT",
		LeaderPosSize: 0.047,
	}
	next := *base
	next.LeaderPosSize = 0.02
	next.CloseRatio = 0.5744680851

	if ti.execFailureDedupKey(base, err) == ti.execFailureDedupKey(&next, err) {
		t.Fatalf("different leader operation should produce a different dedupe key")
	}
}

// TestIsBenignCloseErrorRecognizesAllTraderFormats 覆盖各家 trader 的
// "本地无对应仓位"错误格式（关键字必须能识别）。
// 来源参考：
//   - OKX:     trader/okx_trader.go:856,946
//   - Bitget:  trader/bitget_trader.go:613,676
//   - Binance: trader/binance_futures.go:443,498
//   - Aster:   trader/aster_trader.go:755,846
//   - HL:      trader/hyperliquid_trader.go:516,588
//   - Bybit:   trader/bybit_trader.go:383,428
func TestIsBenignCloseErrorRecognizesAllTraderFormats(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}

	cases := []struct {
		name   string
		action string
		errMsg string
		want   bool
	}{
		// OKX
		{"okx close_long position not found",
			"close_long", "long position not found for ETHUSDT (mgnMode=cross)", true},
		{"okx close_short position not found",
			"close_short", "short position not found for ETHUSDT (mgnMode=cross)", true},
		{"okx reduce_short position not found",
			"reduce_short", "short position not found for ETHUSDT (mgnMode=cross)", true},
		// Bitget
		{"bitget close_long position not found",
			"close_long", "long position not found for BTCUSDT", true},
		// Binance / Aster / HL
		{"binance close_long no long position",
			"close_long", "no long position found for ETHUSDT", true},
		{"binance close_short no short position",
			"close_short", "no short position found for ETHUSDT", true},
		// Bybit
		{"bybit close_long no long position to close",
			"close_long", "no long position to close", true},
		{"bybit close_short no short position to close",
			"close_short", "no short position to close", true},
		// Binance fapi reduce-only 保险关键字
		{"binance reduceonly rejected",
			"close_long", "ReduceOnly Order is rejected", true},
		{"binance position size is 0",
			"reduce_long", "position size is 0", true},
		// 大小写不敏感
		{"upper case OKX",
			"close_short", "SHORT POSITION NOT FOUND for ETHUSDT", true},

		// 非 close/reduce 动作永远 false（防止开仓时被误判）
		{"open_long with position not found should NOT be benign",
			"open_long", "long position not found for ETHUSDT", false},
		{"open_short with no short position should NOT be benign",
			"open_short", "no short position found for ETHUSDT", false},
		{"hold action should NOT be benign",
			"hold", "position not found", false},

		// 其他错误不是良性（保证金不足、网络等）
		{"insufficient margin is NOT benign close",
			"close_long", "Order failed. Insufficient USDT margin in account", false},
		{"network error is NOT benign",
			"close_long", "context deadline exceeded", false},
		{"unrelated error is NOT benign",
			"close_long", "rate limit exceeded", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := &decision.Decision{
				Action:      tc.action,
				Symbol:      "ETHUSDT",
				LeaderPosID: "pos-1",
			}
			got := ti.isBenignCloseError(dec, errors.New(tc.errMsg))
			if got != tc.want {
				t.Fatalf("action=%s err=%q got=%v want=%v",
					tc.action, tc.errMsg, got, tc.want)
			}
		})
	}
}

func TestIsBenignCloseErrorHandlesNilInputs(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine:   &Engine{config: &CopyConfig{ProviderType: ProviderBinance}},
	}
	if ti.isBenignCloseError(nil, errors.New("position not found")) {
		t.Fatalf("nil decision should not be benign")
	}
	if ti.isBenignCloseError(&decision.Decision{Action: "close_long"}, nil) {
		t.Fatalf("nil error should not be benign")
	}
}

// TestHandleBenignCloseFailureClosesMappingAndAvoidsDeadlock 验证：
//   - 本地存在 active mapping
//   - 收到良性 close 失败
//   - handleBenignCloseFailure 后 mapping 状态变为 closed
// 这是阻断"大爷的弟弟"那种死循环的关键链路。
func TestHandleBenignCloseFailureClosesMappingAndAvoidsDeadlock(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-benign-close.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const traderID = "test-trader"
	const posID = "1243719130_ETHUSDT_SHORT"

	// 模拟历史遗留的 active mapping（即"大爷的弟弟"日志里那条）
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:      traderID,
		LeaderPosID:   posID,
		LeaderID:      "leader",
		Symbol:        "ETHUSDT",
		Side:          string(SideShort),
		MarginMode:    "cross",
		OpenedAt:      time.Now(),
		OpenPrice:     2062.21,
		OpenSizeUSD:   96.92,
		LastKnownSize: 0.047,
	}); err != nil {
		t.Fatalf("seed active mapping: %v", err)
	}

	// 验证 seed 成功
	before, err := st.CopyTrade().GetActiveMapping(traderID, posID)
	if err != nil || before == nil {
		t.Fatalf("seed mapping not active: err=%v mapping=%+v", err, before)
	}

	ti := &TraderIntegration{
		traderID: traderID,
		store:    st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}

	dec := &decision.Decision{
		Action:      "close_short",
		Symbol:      "ETHUSDT",
		LeaderPosID: posID,
		EntryPrice:  2062.21,
		MarginMode:  "cross",
	}
	closeErr := errors.New("short position not found for ETHUSDT (mgnMode=cross)")

	// 前置条件：必须是良性错误（否则不应调用 handle）
	if !ti.isBenignCloseError(dec, closeErr) {
		t.Fatalf("test setup error: expected benign close")
	}

	ti.handleBenignCloseFailure(dec, closeErr)

	// 关键断言：active mapping 被回收（GetActiveMapping 应返回 nil）
	after, err := st.CopyTrade().GetActiveMapping(traderID, posID)
	if err != nil {
		t.Fatalf("get mapping after handle: %v", err)
	}
	if after != nil {
		t.Fatalf("expected mapping closed (deadlock broken), still active: %+v", after)
	}
}

// TestIncrementMappingFailureAccumulatesAndResetsCorrectly 验证 store 层
// IncrementMappingFailure + ResetMappingFailure 的基本语义。
func TestIncrementMappingFailureAccumulatesAndResetsCorrectly(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-fail-count.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const traderID = "test-trader"
	const posID = "leader-pos-1"

	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:    traderID,
		LeaderPosID: posID,
		LeaderID:    "leader",
		Symbol:      "ETHUSDT",
		Side:        string(SideShort),
		MarginMode:  "cross",
		OpenedAt:    time.Now(),
		OpenPrice:   2000,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 累加 3 次
	for i := 1; i <= 3; i++ {
		c, err := st.CopyTrade().IncrementMappingFailure(traderID, posID, "fake error")
		if err != nil {
			t.Fatalf("increment #%d: %v", i, err)
		}
		if c != i {
			t.Fatalf("increment #%d: count=%d want %d", i, c, i)
		}
	}

	// 清零
	if err := st.CopyTrade().ResetMappingFailure(traderID, posID); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// 再累加 1 次：应该回到 1
	c, err := st.CopyTrade().IncrementMappingFailure(traderID, posID, "another error")
	if err != nil {
		t.Fatalf("increment after reset: %v", err)
	}
	if c != 1 {
		t.Fatalf("after reset increment: count=%d want 1", c)
	}
}

// TestIncrementMappingFailureReturnsZeroWhenNoActiveMapping 验证：
// active mapping 不存在时 IncrementMappingFailure 应返回 (0, nil)，
// 上层 checkAndTripMappingCircuit 可以据此短路。
func TestIncrementMappingFailureReturnsZeroWhenNoActiveMapping(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-no-mapping.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	c, err := st.CopyTrade().IncrementMappingFailure("test-trader", "nonexistent-pos", "err")
	if err != nil {
		t.Fatalf("increment on missing mapping: %v", err)
	}
	if c != 0 {
		t.Fatalf("expected count=0 for missing mapping, got %d", c)
	}
}

// TestCheckAndTripMappingCircuitTripsAtThreshold 验证：
// integration 层熔断逻辑在累计失败 ≥ mappingFailureCircuitThreshold 次时
// 自动 CloseMapping。
func TestCheckAndTripMappingCircuitTripsAtThreshold(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-circuit.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const traderID = "test-trader"
	const posID = "circuit-pos-1"

	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:    traderID,
		LeaderPosID: posID,
		LeaderID:    "leader",
		Symbol:      "ETHUSDT",
		Side:        string(SideShort),
		MarginMode:  "cross",
		OpenedAt:    time.Now(),
		OpenPrice:   2000,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ti := &TraderIntegration{
		traderID: traderID,
		store:    st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderOKX,
			LeaderID:     "leader",
		}},
	}
	dec := &decision.Decision{
		Action:      "open_long",
		Symbol:      "ETHUSDT",
		LeaderPosID: posID,
		EntryPrice:  2000,
		MarginMode:  "cross",
	}
	hardErr := errors.New("Order failed. Insufficient USDT margin in account")

	// 累加到阈值前一步：mapping 应仍 active
	for i := 1; i < mappingFailureCircuitThreshold; i++ {
		ti.checkAndTripMappingCircuit(dec, hardErr)
	}
	if m, _ := st.CopyTrade().GetActiveMapping(traderID, posID); m == nil {
		t.Fatalf("到达阈值前 mapping 不应被熔断关闭")
	}

	// 第 threshold 次：熔断 → CloseMapping
	ti.checkAndTripMappingCircuit(dec, hardErr)

	if m, _ := st.CopyTrade().GetActiveMapping(traderID, posID); m != nil {
		t.Fatalf("熔断后 mapping 应被关闭，仍 active: %+v", m)
	}
}

// TestCheckAndTripMappingCircuitNoopWithoutMapping 验证：
// 无 active mapping 时（例如良性失败已自动 CloseMapping），
// checkAndTripMappingCircuit 应为 no-op 不报错。
func TestCheckAndTripMappingCircuitNoopWithoutMapping(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-circuit-noop.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ti := &TraderIntegration{
		traderID: "test-trader",
		store:    st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderOKX,
			LeaderID:     "leader",
		}},
	}
	dec := &decision.Decision{
		Action:      "open_long",
		Symbol:      "ETHUSDT",
		LeaderPosID: "no-such-pos",
	}
	// 应安全返回，不 panic
	for i := 0; i < 10; i++ {
		ti.checkAndTripMappingCircuit(dec, errors.New("any error"))
	}
}

// TestExecuteDecisionDoesNotAlertOnBenignCloseFailure 验证：
// executeFullDecision 在遇到良性 close 失败时，不应该走告警分支
// （这里只能间接验证：状态保存为 silent_close 而非 failed）
func TestBenignCloseStatusDistinctFromHardFailure(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-status.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const traderID = "test-trader"
	const posID = "test-pos"

	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:      traderID,
		LeaderPosID:   posID,
		LeaderID:      "leader",
		Symbol:        "ETHUSDT",
		Side:          string(SideShort),
		MarginMode:    "cross",
		OpenedAt:      time.Now(),
		OpenPrice:     2062.21,
		LastKnownSize: 0.047,
	}); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	ti := &TraderIntegration{
		traderID: traderID,
		store:    st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}

	dec := &decision.Decision{
		Action:      "close_short",
		Symbol:      "ETHUSDT",
		LeaderPosID: posID,
		EntryPrice:  2062.21,
	}
	ti.handleBenignCloseFailure(dec, errors.New("short position not found"))

	logs, err := st.CopyTrade().GetRecentSignalLogs(traderID, 10)
	if err != nil {
		t.Fatalf("query signal logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected at least one signal log")
	}
	found := false
	for _, log := range logs {
		if log.Status == "silent_close" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected signal log with status=silent_close, got: %+v", logs)
	}
}
