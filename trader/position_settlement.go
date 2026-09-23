package trader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"nofx/logger"
	"nofx/store"
)

// SettlementTimeTolerance admits candidates across venue-record timestamps,
// never proof by proximity. Quantity, identity, scope and one continuous
// flat-to-flat lifecycle must still all agree before any costs are applied.
const SettlementTimeTolerance = time.Second

type SettlementProofError struct {
	Code, Detail string
}

func (e *SettlementProofError) Error() string { return e.Detail }

func settlementProofError(code, detail string) error {
	return &SettlementProofError{Code: code, Detail: detail}
}

func fillsWithinWindow(fills []TradeRecord, start, end time.Time) []TradeRecord {
	result := make([]TradeRecord, 0, len(fills))
	for _, f := range fills {
		if !f.Time.Before(start) && !f.Time.After(end) {
			result = append(result, f)
		}
	}
	return result
}

// ReadPositionSettlementTrades keeps settlement bounded without changing the
// open-ended history contract used to detect an original position ending.
func ReadPositionSettlementTrades(history ScopedTradeHistoryProvider, record ClosedPnLRecord) ([]TradeRecord, error) {
	start, end := record.EntryTime.Add(-SettlementTimeTolerance), record.ExitTime.Add(SettlementTimeTolerance)
	if scoped, ok := history.(ScopedTradeHistoryWindowProvider); ok {
		return scoped.GetTradesForPositionWindow(record.Symbol, record.MarginMode, start, end)
	}
	fills, err := history.GetTradesForPosition(record.Symbol, record.MarginMode, start)
	if err != nil {
		return nil, err
	}
	return fillsWithinWindow(fills, start, end), nil
}

// BuildPositionSettlement accepts cumulative costs only after proving exactly
// one flat-to-flat lifecycle from scoped immutable fills. A reused posId, an
// incomplete page, or a later same-side manual entry cannot reuse its costs.
func BuildPositionSettlement(account string, r ClosedPnLRecord, trades []TradeRecord) (store.PositionSettlement, error) {
	out := store.PositionSettlement{ExchangeID: account, PositionID: r.ExchangeID, Symbol: r.Symbol, Side: strings.ToLower(r.Side), MarginMode: r.MarginMode,
		OpenedAt: r.EntryTime, ExchangeClosedAt: r.ExitTime, ClosedAt: r.ExitTime, Quantity: r.QuantityCoins, GrossPnL: r.GrossPnL, NetPnL: r.RealizedPnL, Fee: r.Fee, FundingFee: r.FundingFee, LiquidationPenalty: r.LiquidationPenalty, CloseType: r.CloseType}
	if out.Quantity <= 0 || r.EntryTime.IsZero() || r.ExitTime.IsZero() || r.ExitTime.Before(r.EntryTime) || r.ExchangeID == "" || r.Symbol == "" || (out.Side != "long" && out.Side != "short") || (r.MarginMode != "cross" && r.MarginMode != "isolated") {
		return out, settlementProofError("LIFECYCLE_IDENTITY_MISSING", "settlement lifecycle identity or quantity unavailable")
	}
	for _, amount := range []float64{r.QuantityCoins, r.GrossPnL, r.RealizedPnL, r.Fee, r.FundingFee, r.LiquidationPenalty} {
		if math.IsNaN(amount) || math.IsInf(amount, 0) {
			return out, settlementProofError("INVALID_AMOUNT", "invalid settlement amount")
		}
	}
	if math.Abs(r.RealizedPnL-(r.GrossPnL-r.Fee+r.FundingFee-r.LiquidationPenalty)) > math.Max(1e-6, math.Abs(r.RealizedPnL)*1e-7) {
		return out, settlementProofError("NET_AMOUNT_MISMATCH", "settlement fees do not reconcile to net PnL")
	}
	// Do not impose a fabricated execution order on opposite actions sharing
	// the same venue millisecond. Page order is not chronology. Multiple fills
	// of the same opening order, such as the incident's nine fills, are safe.
	candidates := make([]TradeRecord, 0, len(trades))
	seenEvidence := map[string]string{}
	for _, f := range trades {
		if f.Symbol != r.Symbol || f.MarginMode != r.MarginMode || !strings.EqualFold(f.PositionSide, r.Side) || f.Time.Before(r.EntryTime.Add(-SettlementTimeTolerance)) || f.Time.After(r.ExitTime.Add(SettlementTimeTolerance)) {
			continue
		}
		if f.TradeID == "" || f.Quantity <= 0 || f.OrderID == "" || f.Time.IsZero() || (!strings.EqualFold(f.Side, "buy") && !strings.EqualFold(f.Side, "sell")) {
			return out, settlementProofError("FILL_IDENTITY_INVALID", "ambiguous immutable settlement fill")
		}
		for _, value := range []float64{f.Quantity, f.RealizedPnL, f.Price, f.Fee} {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return out, settlementProofError("INVALID_FILL_AMOUNT", "invalid immutable fill amount")
			}
		}
		evidence, _ := json.Marshal(f)
		if previous, exists := seenEvidence[f.TradeID]; exists {
			if previous != string(evidence) {
				return out, settlementProofError("FILL_IDENTITY_CONFLICT", "conflicting immutable settlement fill")
			}
			continue
		}
		seenEvidence[f.TradeID] = string(evidence)
		candidates = append(candidates, f)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Time.Equal(candidates[j].Time) {
			return candidates[i].TradeID < candidates[j].TradeID
		}
		return candidates[i].Time.Before(candidates[j].Time)
	})
	for i := 1; i < len(candidates); i++ {
		if candidates[i].Time.UnixMilli() == candidates[i-1].Time.UnixMilli() && isClosingTradeForPosition(candidates[i], out.Side) != isClosingTradeForPosition(candidates[i-1], out.Side) {
			return out, settlementProofError("FILL_SEQUENCE_AMBIGUOUS", "opposite actions share an execution millisecond; sequence unproven")
		}
	}
	net, closed, gross := 0.0, 0.0, 0.0
	started, ended := false, false
	entries := map[string]bool{}
	var first, last time.Time
	for _, f := range candidates {
		if ended {
			return out, settlementProofError("MULTIPLE_LIFECYCLES", "history spans more than one flat-to-flat lifecycle")
		}
		if first.IsZero() {
			first = f.Time
		}
		last = f.Time
		if isClosingTradeForPosition(f, out.Side) {
			if !started || f.Quantity > net+math.Max(1e-9, r.QuantityCoins*1e-7) {
				return out, settlementProofError("OPENING_FILLS_INCOMPLETE", "settlement opening fills incomplete")
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
			} else if f.Time.UnixMilli() == first.UnixMilli() && f.OrderID != out.FirstEntryOrderID {
				return out, settlementProofError("FIRST_ENTRY_AMBIGUOUS", "first execution millisecond contains multiple opening orders")
			}
			net += f.Quantity
			started = true
			entries[f.OrderID] = true
		}
	}
	if !ended || math.Abs(closed-r.QuantityCoins) > math.Max(1e-9, r.QuantityCoins*1e-7) || math.Abs(gross-r.GrossPnL) > math.Max(1e-6, math.Abs(r.GrossPnL)*1e-7) {
		return out, settlementProofError("FILL_TOTAL_MISMATCH", "settlement quantities or gross PnL disagree with fills")
	}
	if first.Sub(r.EntryTime).Abs() > SettlementTimeTolerance || r.ExitTime.Sub(last).Abs() > SettlementTimeTolerance {
		return out, settlementProofError("CYCLE_TIME_MISMATCH", "settlement cycle times do not match immutable fills")
	}
	// Actual close time comes from execution, not sync time or history update.
	out.ClosedAt = last
	out.FirstFillAt = first
	for id := range entries {
		out.EntryOrderIDs = append(out.EntryOrderIDs, id)
	}
	sort.Strings(out.EntryOrderIDs)
	return out, nil
}

func (m *PositionSyncManager) reconcileOKXSettlements(exchangeID string, t *OKXTrader) {
	accountDiagnostic := store.PositionSettlementDiagnostic{ExchangeID: exchangeID}
	report := func(d store.PositionSettlementDiagnostic, stage, reason string, err error) {
		d.Stage, d.ReasonCode = stage, reason
		if err != nil {
			d.Detail = err.Error()
		}
		if writeErr := m.store.Position().RecordSettlementDiagnostic(d); writeErr != nil {
			logger.Warnf("persist settlement diagnostic account=%s stage=%s: %v", exchangeID, stage, writeErr)
		}
	}
	since, err := m.store.Position().PendingSettlementSince(exchangeID)
	if err != nil {
		report(accountDiagnostic, "LOCAL_CANDIDATES", "CANDIDATE_QUERY_FAILED", err)
		return
	}
	if since.IsZero() {
		_ = m.store.Position().ResolveSettlementDiagnostic(accountDiagnostic)
		return
	}
	records, err := t.closedPnLSettlementWindow(since)
	if err != nil {
		report(accountDiagnostic, "VENUE_HISTORY", "HISTORY_READ_FAILED", err)
		return
	}
	if len(records) == 0 {
		report(accountDiagnostic, "VENUE_HISTORY", "HISTORY_RECORD_UNAVAILABLE", fmt.Errorf("pending local allocations have no venue history in the evidence window"))
		return
	}
	_ = m.store.Position().ResolveSettlementDiagnostic(accountDiagnostic)
	// This runs on the accounting worker, never on source detection or exits.
	for _, record := range records {
		diagnostic := store.PositionSettlementDiagnostic{ExchangeID: exchangeID, PositionID: record.ExchangeID, Symbol: record.Symbol, Side: strings.ToLower(record.Side), MarginMode: record.MarginMode, OpenedMS: record.EntryTime.UnixMilli()}
		if record.CloseType != "liquidation" && record.CloseType != "unknown" {
			continue
		}
		pending, pendingErr := m.store.Position().HasPendingSettlement(exchangeID, record.Symbol, record.EntryTime, record.ExitTime)
		if pendingErr != nil {
			report(diagnostic, "LOCAL_CANDIDATES", "CANDIDATE_QUERY_FAILED", pendingErr)
			continue
		}
		if !pending {
			_ = m.store.Position().ResolveSettlementDiagnostic(diagnostic)
			continue
		}
		trades, err := ReadPositionSettlementTrades(t, record)
		if err != nil {
			report(diagnostic, "SCOPED_FILLS", "SCOPED_HISTORY_UNAVAILABLE", err)
			continue
		}
		settlement, err := BuildPositionSettlement(exchangeID, record, trades)
		if err != nil {
			code := "EVIDENCE_UNPROVEN"
			var proofErr *SettlementProofError
			if errors.As(err, &proofErr) {
				code = proofErr.Code
			}
			report(diagnostic, "LIFECYCLE_PROOF", code, err)
			continue
		}
		if err = m.store.Position().ApplyPositionSettlement(settlement); err != nil {
			report(diagnostic, "LOCAL_ALLOCATION", "ALLOCATION_NOT_PROVEN", err)
			continue
		}
		_ = m.store.Position().ResolveSettlementDiagnostic(diagnostic)
	}
}

// Read every history page in the evidence window. Hitting a bound returns an
// error instead of pretending that a truncated list is a complete lifecycle.
func (t *OKXTrader) closedPnLSettlementWindow(since time.Time) ([]ClosedPnLRecord, error) {
	var result []ClosedPnLRecord
	var after int64
	seen := make(map[string]string)
	var previousOldest int64
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
			id := fmt.Sprintf("%s|%s|%s|%s|%d|%d", r.Symbol, r.ExchangeID, r.Side, r.MarginMode, r.EntryTime.UnixMilli(), r.ExitTime.UnixMilli())
			raw, _ := json.Marshal(r)
			if existing, ok := seen[id]; !ok {
				seen[id] = string(raw)
				result = append(result, r)
			} else if existing != string(raw) {
				return nil, fmt.Errorf("conflicting OKX position history identity")
			}
			if oldest == 0 || r.ExitTime.UnixMilli() < oldest {
				oldest = r.ExitTime.UnixMilli()
			}
		}
		if len(records) < 100 || oldest <= since.UnixMilli() {
			return result, nil
		}
		if oldest <= 0 || (previousOldest > 0 && oldest >= previousOldest) {
			return nil, fmt.Errorf("OKX settlement history cursor stalled")
		}
		// uTime is the venue pagination clock, not the final fill time. Include
		// the boundary millisecond again so equal-uTime rows are not skipped.
		// A page saturated by that timestamp must fail instead of claiming full
		// evidence when the API cannot expose the remaining tied rows.
		previousOldest = oldest
		after = oldest + 1
	}
	return nil, fmt.Errorf("OKX settlement history incomplete after 100 pages")
}
