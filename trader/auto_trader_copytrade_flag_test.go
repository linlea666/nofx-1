package trader

// 第二批回归测试（trader 包部分）：
//   - S6 显式 IsCopyTrade 标志（不再依赖 Reasoning 文案匹配）
//   - M13 保证金不足减半重试使用派生 ClientOrderID

import (
	"errors"
	"strings"
	"testing"

	"nofx/decision"
	"nofx/store"
)

type copyFlowFakeTrader struct {
	Trader
	failFirstOpenWithMargin bool
	openLongClientIDs       []string
	closePreservingCalls    int
	plainCloseCalls         int
	positions               []map[string]interface{}
}

func (f *copyFlowFakeTrader) GetPositions() ([]map[string]interface{}, error) {
	return f.positions, nil
}
func (f *copyFlowFakeTrader) GetMarketPrice(string) (float64, error) { return 100, nil }
func (f *copyFlowFakeTrader) SetMarginMode(string, bool) error       { return nil }
func (f *copyFlowFakeTrader) GetBalance() (map[string]interface{}, error) {
	return map[string]interface{}{"availableBalance": 10000.0, "totalEquity": 10000.0}, nil
}

func (f *copyFlowFakeTrader) OpenLongPreservingOrdersWithClientID(_ string, _ float64, _ int, clientOrderID string) (map[string]interface{}, error) {
	f.openLongClientIDs = append(f.openLongClientIDs, clientOrderID)
	if f.failFirstOpenWithMargin && len(f.openLongClientIDs) == 1 {
		return nil, errors.New("okx: insufficient margin balance")
	}
	return map[string]interface{}{"orderId": "open-ok"}, nil
}
func (f *copyFlowFakeTrader) OpenShortPreservingOrdersWithClientID(string, float64, int, string) (map[string]interface{}, error) {
	return map[string]interface{}{"orderId": "open-ok"}, nil
}
func (f *copyFlowFakeTrader) CloseLongPreservingOrdersWithClientID(string, float64, string) (map[string]interface{}, error) {
	f.closePreservingCalls++
	return map[string]interface{}{"orderId": "close-ok"}, nil
}
func (f *copyFlowFakeTrader) CloseShortPreservingOrdersWithClientID(string, float64, string) (map[string]interface{}, error) {
	f.closePreservingCalls++
	return map[string]interface{}{"orderId": "close-ok"}, nil
}
func (f *copyFlowFakeTrader) CloseLong(string, float64) (map[string]interface{}, error) {
	f.plainCloseCalls++
	return map[string]interface{}{"orderId": "close-plain"}, nil
}

func newCopyFlowAutoTrader(fake *copyFlowFakeTrader) *AutoTrader {
	return &AutoTrader{
		trader:                fake,
		exchange:              "okx",
		config:                AutoTraderConfig{IsCrossMargin: true},
		positionFirstSeenTime: make(map[string]int64),
	}
}

// S6：Reasoning 不含 "Copy trading" 时，显式 IsCopyTrade 也必须走跟单执行路径
// （保留挂单的幂等下单接口），文案调整不得静默破坏跟单闸门。
func TestExplicitIsCopyTradeFlagGatesCopyTradePath(t *testing.T) {
	fake := &copyFlowFakeTrader{}
	at := newCopyFlowAutoTrader(fake)
	dec := &decision.Decision{
		Symbol: "ETHUSDT", Action: "close_long", IsCopyTrade: true,
		Reasoning: "follow leader exit", ClientOrderID: "cg7a0", MarginMode: "cross",
	}

	if err := at.executeCloseLongWithRecord(dec, &store.DecisionAction{}); err != nil {
		t.Fatalf("close must succeed: %v", err)
	}
	if fake.closePreservingCalls != 1 || fake.plainCloseCalls != 0 {
		t.Fatalf("explicit flag must route to the copy-trade idempotent path: preserving=%d plain=%d",
			fake.closePreservingCalls, fake.plainCloseCalls)
	}
}

// AI reentry may make one explicitly audited adaptive-size retry. The retry
// must use a distinct durable client id so restart reconciliation can find it.
func TestAIReentryHalvedRetryUsesDerivedClientOrderID(t *testing.T) {
	fake := &copyFlowFakeTrader{failFirstOpenWithMargin: true}
	at := newCopyFlowAutoTrader(fake)
	dec := &decision.Decision{
		Symbol: "ETHUSDT", Action: "open_long", IsCopyTrade: true,
		CopyTradeAction: "ai_reentry", Reasoning: "Copy trading: AI reentry", ClientOrderID: "cg42a0",
		PositionSizeUSD: 100, Leverage: 5, EntryPrice: 100, MarginMode: "cross",
	}

	if err := at.executeOpenLongWithRecord(dec, &store.DecisionAction{}); err != nil {
		t.Fatalf("halved retry must succeed: %v", err)
	}
	if len(fake.openLongClientIDs) != 2 {
		t.Fatalf("expected initial attempt + one halved retry: %v", fake.openLongClientIDs)
	}
	if fake.openLongClientIDs[0] != "cg42a0" {
		t.Fatalf("initial attempt must keep the original id: %v", fake.openLongClientIDs)
	}
	retryID := fake.openLongClientIDs[1]
	if retryID == "cg42a0" || !strings.HasPrefix(retryID, "cg42a0") {
		t.Fatalf("retry must use a derived id (original id may be permanently reserved by the failed order): %q", retryID)
	}
	if dec.ClientOrderID != retryID {
		t.Fatalf("decision must carry the actually used id for downstream confirmation: %q vs %q", dec.ClientOrderID, retryID)
	}
}

func TestLeaderCopyInsufficientMarginDoesNotSilentlyShrink(t *testing.T) {
	fake := &copyFlowFakeTrader{failFirstOpenWithMargin: true}
	at := newCopyFlowAutoTrader(fake)
	dec := &decision.Decision{
		Symbol: "ETHUSDT", Action: "open_long", IsCopyTrade: true,
		CopyTradeAction: "open", Reasoning: "Copy trading: open following leader", ClientOrderID: "cg43a0",
		PositionSizeUSD: 100, Leverage: 5, EntryPrice: 100, MarginMode: "cross",
	}
	err := at.executeOpenLongWithRecord(dec, &store.DecisionAction{})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "insufficient margin") {
		t.Fatalf("ordinary leader copy must surface the exchange rejection: %v", err)
	}
	if len(fake.openLongClientIDs) != 1 {
		t.Fatalf("ordinary leader copy must not silently retry a smaller target: %v", fake.openLongClientIDs)
	}
	if dec.PositionSizeUSD != 100 {
		t.Fatalf("ordinary leader target must stay explicit, got %.2f", dec.PositionSizeUSD)
	}
}

func TestExplicitAddActionBypassesDuplicatePositionGate(t *testing.T) {
	fake := &copyFlowFakeTrader{
		positions: []map[string]interface{}{
			{"symbol": "ETHUSDT", "side": "long", "mgnMode": "cross", "positionAmt": 1.0},
		},
	}
	at := newCopyFlowAutoTrader(fake)
	dec := &decision.Decision{
		Symbol: "ETHUSDT", Action: "open_long", IsCopyTrade: true,
		CopyTradeAction: "add", Reasoning: "leader size changed", ClientOrderID: "cg44a0",
		PositionSizeUSD: 100, Leverage: 5, EntryPrice: 100, MarginMode: "cross",
	}
	if err := at.executeOpenLongWithRecord(dec, &store.DecisionAction{}); err != nil {
		t.Fatalf("explicit add action must not depend on reasoning wording: %v", err)
	}
	if len(fake.openLongClientIDs) != 1 {
		t.Fatalf("explicit add must submit exactly once: %v", fake.openLongClientIDs)
	}
}

func TestDeriveHalvedRetryClientOrderIDRespectsLengthCap(t *testing.T) {
	long := strings.Repeat("x", 32)
	derived := deriveHalvedRetryClientOrderID(long)
	if len(derived) != 32 || !strings.HasSuffix(derived, "h1") {
		t.Fatalf("derived id must stay within 32 chars with the retry suffix: %q", derived)
	}
	if deriveHalvedRetryClientOrderID("") != "" {
		t.Fatal("empty base id must stay empty")
	}
}
