package trader

import (
	"errors"
	"testing"
)

type autoTraderMarkFallbackVenue struct {
	Trader
	endpointPrice float64
	endpointErr   error
	positions     []map[string]interface{}
}

func (v *autoTraderMarkFallbackVenue) GetMarkPrice(string) (float64, error) {
	return v.endpointPrice, v.endpointErr
}

func (v *autoTraderMarkFallbackVenue) GetPositions() ([]map[string]interface{}, error) {
	return v.positions, nil
}

func TestAutoTraderMarkPriceFallsBackToFreshPositionMark(t *testing.T) {
	venue := &autoTraderMarkFallbackVenue{
		endpointErr: errors.New("temporary public endpoint failure"),
		positions: []map[string]interface{}{{
			"symbol": "ETHUSDT", "markPrice": "102.5",
		}},
	}
	auto := &AutoTrader{trader: venue}
	price, err := auto.GetMarkPrice("ethusdt")
	if err != nil || price != 102.5 {
		t.Fatalf("fresh exchange position mark was not used after endpoint failure: price=%v err=%v", price, err)
	}
}

func TestAutoTraderMarkPriceNeverFallsBackToLastPrice(t *testing.T) {
	venue := &autoTraderMarkFallbackVenue{
		endpointErr: errors.New("temporary public endpoint failure"),
		positions:   []map[string]interface{}{},
	}
	auto := &AutoTrader{trader: venue}
	if price, err := auto.GetMarkPrice("ETHUSDT"); err == nil || price != 0 {
		t.Fatalf("missing authoritative mark unexpectedly produced a price: price=%v err=%v", price, err)
	}
}
