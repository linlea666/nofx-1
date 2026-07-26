package trader

import (
	"errors"
	"testing"

	"nofx/decision"
	"nofx/store"
)

// partialCloseFakeTrader 只实现部分减仓路径会触达的方法；
// 其余 Trader 接口方法通过内嵌接口留空（被调用即 panic，测试会失败暴露）。
type partialCloseFakeTrader struct {
	Trader
	positions    []map[string]interface{}
	positionsErr error
	instrument   *ExecutionInstrument
	instErr      error
	closedLong   []float64
	closedShort  []float64
}

func (f *partialCloseFakeTrader) GetMarketPrice(string) (float64, error) { return 100, nil }
func (f *partialCloseFakeTrader) SetMarginMode(string, bool) error       { return nil }
func (f *partialCloseFakeTrader) GetPositions() ([]map[string]interface{}, error) {
	if f.positionsErr != nil {
		return nil, f.positionsErr
	}
	return f.positions, nil
}
func (f *partialCloseFakeTrader) CloseLong(_ string, quantity float64) (map[string]interface{}, error) {
	f.closedLong = append(f.closedLong, quantity)
	return map[string]interface{}{"orderId": "close-order"}, nil
}
func (f *partialCloseFakeTrader) CloseShort(_ string, quantity float64) (map[string]interface{}, error) {
	f.closedShort = append(f.closedShort, quantity)
	return map[string]interface{}{"orderId": "close-order"}, nil
}
func (f *partialCloseFakeTrader) ResolveExecutionInstrument(string) (*ExecutionInstrument, error) {
	if f.instErr != nil {
		return nil, f.instErr
	}
	return f.instrument, nil
}

func newPartialCloseDecision(action string, closeRatio float64) *decision.Decision {
	return &decision.Decision{
		Symbol:     "ETHUSDT",
		Action:     action,
		CloseRatio: closeRatio,
		MarginMode: "cross",
		Reasoning:  "Copy trading follow reduce",
	}
}

// F1: 部分减仓意图下读仓失败必须 fail-closed，禁止带 quantity=0（全平语义）下单。
func TestPartialCloseFailsClosedWhenPositionQueryFails(t *testing.T) {
	fake := &partialCloseFakeTrader{positionsErr: errors.New("okx 5xx")}
	at := &AutoTrader{trader: fake, exchange: "okx"}

	err := at.executeCloseLongWithRecord(newPartialCloseDecision("close_long", 0.5), &store.DecisionAction{})
	if err == nil {
		t.Fatal("partial close with failed position query must return an error")
	}
	if errors.Is(err, ErrPartialCloseSkipped) {
		t.Fatalf("query failure is transient and must not be classified as skipped: %v", err)
	}
	if len(fake.closedLong) != 0 {
		t.Fatalf("no order may be placed: %v", fake.closedLong)
	}

	err = at.executeCloseShortWithRecord(newPartialCloseDecision("close_short", 0.5), &store.DecisionAction{})
	if err == nil || len(fake.closedShort) != 0 {
		t.Fatalf("short path must be symmetric: err=%v orders=%v", err, fake.closedShort)
	}
}

// F1: 部分减仓意图下仓位未匹配（totalQuantity==0）必须返回 ErrPartialCloseSkipped，
// 旧行为会带 quantity=0 下单直接全平。
func TestPartialCloseSkipsWhenNoMatchingPosition(t *testing.T) {
	fake := &partialCloseFakeTrader{positions: []map[string]interface{}{
		{"symbol": "BTCUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 1.0, "entryPrice": 50000.0},
	}}
	at := &AutoTrader{trader: fake, exchange: "okx"}

	err := at.executeCloseLongWithRecord(newPartialCloseDecision("close_long", 0.5), &store.DecisionAction{})
	if !errors.Is(err, ErrFollowerPositionMissing) {
		t.Fatalf("unmatched position must be skipped, got %v", err)
	}
	if len(fake.closedLong) != 0 {
		t.Fatalf("no order may be placed: %v", fake.closedLong)
	}

	err = at.executeCloseShortWithRecord(newPartialCloseDecision("close_short", 0.5), &store.DecisionAction{})
	if !errors.Is(err, ErrFollowerPositionMissing) || len(fake.closedShort) != 0 {
		t.Fatalf("short path must be symmetric: err=%v orders=%v", err, fake.closedShort)
	}
}

// S5: OKX ctVal=1 的币种，1 张仓位减 50% 不足一个完整交易步长。
// 动作级量化必须向下处理为 sub-lot 并跳过，禁止向上提升为 1 张导致全平。
func TestPartialCloseSkipsSubLotWithoutRoundingUpToFullPosition(t *testing.T) {
	fake := &partialCloseFakeTrader{
		positions: []map[string]interface{}{
			{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 1.0, "entryPrice": 100.0},
		},
		instrument: &ExecutionInstrument{BaseQuantityStep: 1.0},
	}
	at := &AutoTrader{trader: fake, exchange: "okx"}

	err := at.executeCloseLongWithRecord(newPartialCloseDecision("close_long", 0.5), &store.DecisionAction{})
	if !errors.Is(err, ErrPartialCloseSubLot) {
		t.Fatalf("sub-lot partial close must be skipped without rounding up, got %v", err)
	}
	if len(fake.closedLong) != 0 {
		t.Fatalf("no order may be placed: %v", fake.closedLong)
	}
}

// S5: 步长足够细时（且仓位高于旧硬阈值路径不适用），部分减仓正常放行。
func TestPartialCloseProceedsWhenStepRoundingIsSafe(t *testing.T) {
	fake := &partialCloseFakeTrader{
		positions: []map[string]interface{}{
			{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 2.0, "entryPrice": 100.0},
		},
		instrument: &ExecutionInstrument{BaseQuantityStep: 0.01},
	}
	at := &AutoTrader{trader: fake, exchange: "okx"}

	if err := at.executeCloseLongWithRecord(newPartialCloseDecision("close_long", 0.5), &store.DecisionAction{}); err != nil {
		t.Fatalf("safe partial close must proceed: %v", err)
	}
	if len(fake.closedLong) != 1 || fake.closedLong[0] != 1.0 {
		t.Fatalf("expected one partial close of 1.0: %v", fake.closedLong)
	}
}

// S5 兼容性：精确步长不可用时回退旧的 0.02 硬阈值。
func TestPartialCloseFallsBackToLegacyThresholdWithoutResolver(t *testing.T) {
	fake := &partialCloseFakeTrader{
		positions: []map[string]interface{}{
			{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 0.01, "entryPrice": 100.0},
		},
		instErr: errors.New("instrument catalog unavailable"),
	}
	at := &AutoTrader{trader: fake, exchange: "okx"}

	err := at.executeCloseLongWithRecord(newPartialCloseDecision("close_long", 0.5), &store.DecisionAction{})
	if !errors.Is(err, ErrPartialCloseBelowMinimum) {
		t.Fatalf("tiny position without resolver must fall back to legacy skip, got %v", err)
	}
	if len(fake.closedLong) != 0 {
		t.Fatalf("no order may be placed: %v", fake.closedLong)
	}
}

// F1 兼容性：全平意图（CloseRatio==0）在读仓失败时保持原有 quantity=0 全平语义。
func TestFullCloseStillClosesAllWhenPositionQueryFails(t *testing.T) {
	fake := &partialCloseFakeTrader{positionsErr: errors.New("okx 5xx")}
	at := &AutoTrader{trader: fake, exchange: "okx"}

	if err := at.executeCloseLongWithRecord(newPartialCloseDecision("close_long", 0), &store.DecisionAction{}); err != nil {
		t.Fatalf("full close must keep tolerating position query failures: %v", err)
	}
	if len(fake.closedLong) != 1 || fake.closedLong[0] != 0 {
		t.Fatalf("expected one close-all order (quantity=0): %v", fake.closedLong)
	}
}
