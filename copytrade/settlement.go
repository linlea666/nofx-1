package copytrade

import (
	"fmt"
	"nofx/store"
	"nofx/trader"
	"time"
)

// Reuse the ordinary position ledger's exact flat-to-flat evidence check.
// A cumulative OKX posId history row is never an attempt identity by itself.
func (ti *TraderIntegration) verifyCopyGuardSettlement(cycle *store.CopyGuardCycle, attemptNo int, record *trader.ClosedPnLRecord) error {
	if record == nil {
		return fmt.Errorf("settlement missing")
	}
	// Other exchanges retain their current accounting adapters. OKX exposes
	// native contracts plus QuantityCoins; its history requires scope proof.
	if record.QuantityCoins <= 0 && !record.RequiresScopedSettlementProof {
		return nil
	}
	if record.QuantityCoins <= 0 {
		return fmt.Errorf("settlement contract-to-coin conversion unavailable")
	}
	history, ok := ti.executor.(trader.ScopedTradeHistoryProvider)
	if !ok {
		return fmt.Errorf("settlement scoped fills unavailable")
	}
	fills, err := history.GetTradesForPosition(record.Symbol, record.MarginMode, record.EntryTime.Add(-time.Second))
	if err != nil {
		return err
	}
	evidence, err := trader.BuildPositionSettlement("copy-guard-evidence", *record, fills)
	if err != nil {
		return err
	}
	attempts, err := ti.store.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil {
		return err
	}
	entry := ""
	for _, a := range attempts {
		if a.AttemptNo == attemptNo {
			entry = a.EntryOrderID
			break
		}
	}
	if attemptNo == 0 {
		if first, e := ti.store.CopyTrade().GetCopyGuardInitialFillEvidence(cycle.ID); e == nil && first.ExchangeOrderID != "" {
			entry = first.ExchangeOrderID
		}
	}
	if entry == "" {
		return fmt.Errorf("settlement original entry identity unavailable")
	}
	// Only the original owner of the flat-to-flat venue lifecycle can consume
	// its cumulative costs. An add from another cycle cannot claim them again.
	if evidence.FirstEntryOrderID == entry {
		return nil
	}
	return fmt.Errorf("settlement belongs to a different source attempt")
}
