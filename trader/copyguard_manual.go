package trader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

func CopyPositionKey(symbol, side, mode, posID string) string {
	return strings.ToUpper(symbol) + "|" + strings.ToLower(side) + "|" + strings.ToLower(mode) + "|" + posID
}

func (at *AutoTrader) SupportsPositionContinuity() bool {
	return implementsTraderCapability[ScopedTradeHistoryProvider](at.trader)
}

func (at *AutoTrader) ListProtectiveStops(symbol string) ([]ProtectiveStopOrder, error) {
	lister, ok := at.trader.(ProtectiveStopLister)
	if !ok {
		return nil, fmt.Errorf("exchange cannot list protective stops")
	}
	return lister.ListProtectiveStops(symbol)
}

func (at *AutoTrader) GetTradesForSymbol(symbol string, start time.Time, limit int) ([]TradeRecord, error) {
	p, ok := at.trader.(SymbolTradeHistoryProvider)
	if !ok {
		return nil, fmt.Errorf("exchange cannot read immutable fill history")
	}
	return p.GetTradesForSymbol(symbol, start, limit)
}

func (at *AutoTrader) GetTradesForPosition(symbol, mode string, start time.Time) ([]TradeRecord, error) {
	if p, ok := at.trader.(ScopedTradeHistoryProvider); ok {
		return p.GetTradesForPosition(symbol, mode, start)
	}
	return nil, fmt.Errorf("exchange cannot prove scoped fill history")
}

func (t *OKXTrader) GetTradesForPosition(symbol, mode string, start time.Time) ([]TradeRecord, error) {
	if start.Before(time.Now().Add(-90 * 24 * time.Hour)) {
		return nil, fmt.Errorf("position continuity exceeds available OKX history; prior durable observations required")
	}
	fills, err := t.GetTradesForSymbol(symbol, start, 100)
	if err != nil {
		return nil, err
	}
	var result []TradeRecord
	for _, f := range fills {
		key := symbol + "|" + f.OrderID
		cached, ok := t.fillOrderModes.Load(key)
		if !ok {
			order, e := t.GetOrderStatus(symbol, f.OrderID)
			if e != nil {
				return nil, fmt.Errorf("confirm fill margin scope: %w", e)
			}
			confirmed, _ := order["marginMode"].(string)
			if confirmed != "cross" && confirmed != "isolated" {
				return nil, fmt.Errorf("fill order margin scope unavailable")
			}
			cached = confirmed
			t.fillOrderModes.Store(key, confirmed)
		}
		f.MarginMode = cached.(string)
		if f.MarginMode == mode {
			result = append(result, f)
		}
	}
	return result, nil
}

func (t *FuturesTrader) GetTradesForPosition(symbol, mode string, start time.Time) ([]TradeRecord, error) {
	// Binance cannot simultaneously hold cross and isolated positions for the
	// same symbol. A mode switch requires flat; fill continuity still proves it.
	if start.Before(time.Now().AddDate(0, -3, 0)) {
		return nil, fmt.Errorf("position continuity exceeds available Binance history; prior durable observations required")
	}
	return t.GetTradesForSymbol(symbol, start, 1000)
}

func (t *OKXTrader) ListProtectiveStops(symbol string) ([]ProtectiveStopOrder, error) {
	var result []ProtectiveStopOrder
	after := ""
	for page := 0; page < 100; page++ {
		path := okxAlgoPendingPath + "?ordType=conditional&instType=SWAP&limit=100&instId=" + url.QueryEscape(t.convertSymbol(symbol))
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		data, err := t.doRequest("GET", path, nil)
		if err != nil {
			return nil, err
		}
		var rows []struct {
			AlgoID         string `json:"algoId"`
			InstID         string `json:"instId"`
			Side           string `json:"side"`
			PositionSide   string `json:"posSide"`
			StopOrderPrice string `json:"slOrdPx"`
			Stop           string `json:"slTriggerPx"`
			TakeProfit     string `json:"tpTriggerPx"`
		}
		if err = json.Unmarshal(data, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			// A combined TP/SL order is not safe to amend as our stop-only order.
			closing := (row.PositionSide == "long" && row.Side == "sell") || (row.PositionSide == "short" && row.Side == "buy")
			if row.InstID != t.convertSymbol(symbol) || row.Stop == "" || row.TakeProfit != "" || !closing || row.StopOrderPrice != "-1" {
				continue
			}
			order, err := t.GetProtectiveStop(row.AlgoID, symbol)
			if err != nil {
				return nil, err
			}
			result = append(result, *order)
		}
		if len(rows) < 100 {
			return result, nil
		}
		next := rows[len(rows)-1].AlgoID
		if next == "" || next == after {
			return nil, fmt.Errorf("protective order pagination did not advance")
		}
		after = next
	}
	return nil, fmt.Errorf("protective order pagination limit exceeded")
}

func (t *FuturesTrader) ListProtectiveStops(symbol string) ([]ProtectiveStopOrder, error) {
	orders, err := t.client.NewListOpenAlgoOrdersService().Symbol(symbol).Do(context.Background())
	if err != nil {
		return nil, err
	}
	var result []ProtectiveStopOrder
	for _, order := range orders {
		closing := (order.PositionSide == futures.PositionSideTypeLong && order.Side == futures.SideTypeSell) || (order.PositionSide == futures.PositionSideTypeShort && order.Side == futures.SideTypeBuy)
		if order.OrderType != futures.AlgoOrderTypeStopMarket || !closing {
			continue
		}
		stop, err := t.GetProtectiveStop(strconv.FormatInt(order.AlgoId, 10), symbol)
		if err != nil {
			return nil, err
		}
		result = append(result, *stop)
	}
	return result, nil
}
