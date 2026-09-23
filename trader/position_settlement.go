package trader

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"nofx/logger"
	"nofx/store"
)

// BuildPositionSettlement accepts cumulative costs only after proving exactly
// one flat-to-flat lifecycle from scoped immutable fills. A reused posId, an
// incomplete page, or a later same-side manual entry cannot reuse its costs.
func BuildPositionSettlement(account string, r ClosedPnLRecord, trades []TradeRecord) (store.PositionSettlement, error) {
	out := store.PositionSettlement{ExchangeID: account, PositionID: r.ExchangeID, Symbol: r.Symbol, Side: strings.ToLower(r.Side), MarginMode: r.MarginMode,
		OpenedAt: r.EntryTime, ClosedAt: r.ExitTime, Quantity: r.QuantityCoins, GrossPnL: r.GrossPnL, NetPnL: r.RealizedPnL, Fee: r.Fee, FundingFee: r.FundingFee, LiquidationPenalty: r.LiquidationPenalty, CloseType: r.CloseType}
	if out.Quantity <= 0 || r.EntryTime.IsZero() || r.ExitTime.IsZero() || r.ExchangeID == "" {
		return out, fmt.Errorf("settlement lifecycle identity or quantity unavailable")
	}
	for _, amount := range []float64{r.QuantityCoins, r.GrossPnL, r.RealizedPnL, r.Fee, r.FundingFee, r.LiquidationPenalty} {
		if math.IsNaN(amount) || math.IsInf(amount, 0) {
			return out, fmt.Errorf("invalid settlement amount")
		}
	}
	if math.Abs(r.RealizedPnL-(r.GrossPnL-r.Fee+r.FundingFee-r.LiquidationPenalty)) > math.Max(1e-6, math.Abs(r.RealizedPnL)*1e-7) {
		return out, fmt.Errorf("settlement fees do not reconcile to net PnL")
	}
	trades = append([]TradeRecord(nil), trades...)
	sort.SliceStable(trades, func(i, j int) bool { return trades[i].Time.Before(trades[j].Time) })
	net, closed, gross := 0.0, 0.0, 0.0
	started, ended := false, false
	seen := map[string]bool{}
	entries := map[string]bool{}
	var first, last time.Time
	for _, f := range trades {
		if f.Symbol != r.Symbol || f.MarginMode != r.MarginMode || !strings.EqualFold(f.PositionSide, r.Side) || f.Time.Before(r.EntryTime) || f.Time.After(r.ExitTime) {
			continue
		}
		if f.TradeID == "" || seen[f.TradeID] || f.Quantity <= 0 || f.OrderID == "" {
			return out, fmt.Errorf("ambiguous immutable settlement fill")
		}
		seen[f.TradeID] = true
		if ended {
			return out, fmt.Errorf("history spans more than one flat-to-flat lifecycle")
		}
		if first.IsZero() {
			first = f.Time
		}
		last = f.Time
		if isClosingTradeForPosition(f, out.Side) {
			if !started || f.Quantity > net+math.Max(1e-9, r.QuantityCoins*1e-7) {
				return out, fmt.Errorf("settlement opening fills incomplete")
			}
			net -= f.Quantity
			closed += f.Quantity
			gross += f.RealizedPnL
			out.TradeIDs = append(out.TradeIDs, f.TradeID)
			if math.Abs(net) < 1e-9 {
				ended = true
			}
		} else {
			if !started {
				out.FirstEntryOrderID = f.OrderID
			}
			net += f.Quantity
			started = true
			entries[f.OrderID] = true
		}
	}
	if !ended || math.Abs(closed-r.QuantityCoins) > math.Max(1e-9, r.QuantityCoins*1e-7) || math.Abs(gross-r.GrossPnL) > math.Max(1e-6, math.Abs(r.GrossPnL)*1e-7) {
		return out, fmt.Errorf("settlement quantities or gross PnL disagree with fills")
	}
	if first.Sub(r.EntryTime) > time.Second || r.ExitTime.Sub(last) > time.Second {
		return out, fmt.Errorf("settlement cycle times do not match immutable fills")
	}
	// Actual close time comes from execution, not sync time or history update.
	out.ClosedAt = last
	for id := range entries {
		out.EntryOrderIDs = append(out.EntryOrderIDs, id)
	}
	sort.Strings(out.EntryOrderIDs)
	return out, nil
}

func (m *PositionSyncManager) reconcileOKXSettlements(exchangeID string, t *OKXTrader) {
	since, err := m.store.Position().PendingSettlementSince(exchangeID)
	if err != nil || since.IsZero() {
		return
	}
	records, err := t.closedPnLSettlementWindow(since)
	if err != nil {
		return
	}
	// This runs on the accounting worker, never on source detection or exits.
	for _, record := range records {
		if record.CloseType != "liquidation" && record.CloseType != "unknown" {
			continue
		}
		pending, pendingErr := m.store.Position().HasPendingSettlement(exchangeID, record.Symbol, record.EntryTime, record.ExitTime)
		if pendingErr != nil || !pending {
			continue
		}
		trades, err := t.GetTradesForPosition(record.Symbol, record.MarginMode, record.EntryTime.Add(-time.Second))
		if err != nil {
			continue
		}
		settlement, err := BuildPositionSettlement(exchangeID, record, trades)
		if err != nil {
			continue
		}
		if err = m.store.Position().ApplyPositionSettlement(settlement); err != nil {
			logger.Debugf("OKX settlement pending %s %s %s: %v", record.Symbol, record.ExchangeID, record.EntryTime.Format(time.RFC3339), err)
		}
	}
}

// Read every history page in the evidence window. Hitting a bound returns an
// error instead of pretending that a truncated list is a complete lifecycle.
func (t *OKXTrader) closedPnLSettlementWindow(since time.Time) ([]ClosedPnLRecord, error) {
	var result []ClosedPnLRecord
	var after int64
	seen := make(map[string]bool)
	for page := 0; page < 100; page++ {
		path := fmt.Sprintf("/api/v5/account/positions-history?instType=SWAP&limit=100&before=%d", since.UnixMilli())
		if after > 0 {
			path += fmt.Sprintf("&after=%d", after)
		}
		records, err := t.getClosedPnLFromPath(path)
		if err != nil {
			return nil, err
		}
		oldest := int64(0)
		for _, r := range records {
			id := fmt.Sprintf("%s|%s|%d|%d", r.Symbol, r.ExchangeID, r.EntryTime.UnixMilli(), r.ExitTime.UnixMilli())
			if !seen[id] {
				seen[id] = true
				result = append(result, r)
			}
			if oldest == 0 || r.ExitTime.UnixMilli() < oldest {
				oldest = r.ExitTime.UnixMilli()
			}
		}
		if len(records) < 100 || oldest <= since.UnixMilli() {
			return result, nil
		}
		if oldest <= 0 || (after > 0 && oldest >= after) {
			return nil, fmt.Errorf("OKX settlement history cursor stalled")
		}
		after = oldest
	}
	return nil, fmt.Errorf("OKX settlement history incomplete after 100 pages")
}
