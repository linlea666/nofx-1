package trader

import "testing"

func TestIsClosingTradeForPositionUsesDirectionNotPnL(t *testing.T) {
	tests := []struct {
		name         string
		trade        TradeRecord
		positionSide string
		want         bool
	}{
		{"hedge long close at break even", TradeRecord{Side: "SELL", PositionSide: "LONG", RealizedPnL: 0}, "long", true},
		{"hedge long add", TradeRecord{Side: "BUY", PositionSide: "LONG", RealizedPnL: 12}, "long", false},
		{"hedge short close", TradeRecord{Side: "BUY", PositionSide: "SHORT"}, "short", true},
		{"hedge short add", TradeRecord{Side: "SELL", PositionSide: "SHORT"}, "short", false},
		{"one way long close", TradeRecord{Side: "SELL", PositionSide: "BOTH"}, "long", true},
		{"one way short close", TradeRecord{Side: "BUY"}, "short", true},
		{"unknown position side fails closed", TradeRecord{Side: "SELL", PositionSide: "SIDEWAYS"}, "long", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClosingTradeForPosition(tt.trade, tt.positionSide); got != tt.want {
				t.Fatalf("isClosingTradeForPosition()=%v want=%v", got, tt.want)
			}
		})
	}
}
