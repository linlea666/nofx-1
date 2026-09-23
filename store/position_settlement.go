package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// PositionSettlement is a fully closed exchange lifecycle. PosID alone is
// reusable; identity includes contract, margin mode, side and opening time.
// Fills remain immutable. This ledger only supplies cumulative settlement
// costs once, shared by every local lot allocated from those fills.
type PositionSettlement struct {
	FirstEntryOrderID                                string
	ExchangeID, PositionID, Symbol, Side, MarginMode string
	OpenedAt, ClosedAt                               time.Time
	// OpenedAt remains the venue cTime used by the durable identity. These
	// fields preserve separate record and execution clocks without changing it.
	ExchangeClosedAt, FirstFillAt                                   time.Time
	Quantity, GrossPnL, Fee, FundingFee, LiquidationPenalty, NetPnL float64
	CloseType                                                       string
	TradeIDs, EntryOrderIDs                                         []string
}

func (s *PositionStore) initSettlementTables() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS position_settlements (
 id INTEGER PRIMARY KEY,exchange_id TEXT NOT NULL,identity TEXT NOT NULL,evidence TEXT NOT NULL,
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,UNIQUE(exchange_id,identity));
 CREATE TABLE IF NOT EXISTS position_fill_settlements (
 fill_id INTEGER PRIMARY KEY,settlement_id INTEGER NOT NULL,fee REAL NOT NULL,funding_fee REAL NOT NULL,
 liquidation_penalty REAL NOT NULL,net_pnl REAL NOT NULL);
 CREATE TABLE IF NOT EXISTS position_settlement_revisions (
 settlement_id INTEGER NOT NULL,evidence TEXT NOT NULL,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 UNIQUE(settlement_id,evidence));`)
	if err != nil {
		return err
	}
	if err = s.initSettlementDiagnosticTable(); err != nil {
		return err
	}
	for _, c := range []struct{ name, def string }{
		{"gross_pnl", "REAL"}, {"net_pnl", "REAL"}, {"funding_fee", "REAL NOT NULL DEFAULT 0"},
		{"liquidation_penalty", "REAL NOT NULL DEFAULT 0"}, {"settlement_verified", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err = ensureSQLiteColumn(s.db, "trader_positions", c.name, c.def); err != nil {
			return err
		}
	}
	return nil
}

func (s *PositionStore) ApplyPositionSettlement(v PositionSettlement) error {
	if v.ExchangeID == "" || v.PositionID == "" || v.Symbol == "" || (v.Side != "long" && v.Side != "short") ||
		(v.MarginMode != "cross" && v.MarginMode != "isolated") || v.OpenedAt.IsZero() || !v.ClosedAt.After(v.OpenedAt) || v.Quantity <= 0 || len(v.TradeIDs) == 0 || len(v.EntryOrderIDs) == 0 {
		return fmt.Errorf("settlement lifecycle evidence incomplete")
	}
	for _, n := range []float64{v.Quantity, v.GrossPnL, v.Fee, v.FundingFee, v.LiquidationPenalty, v.NetPnL} {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Errorf("invalid settlement amount")
		}
	}
	closeEnough := func(a, b float64) bool {
		return math.Abs(a-b) <= math.Max(0.000001, math.Max(math.Abs(a), math.Abs(b))*1e-7)
	}
	if !closeEnough(v.NetPnL, v.GrossPnL-v.Fee+v.FundingFee-v.LiquidationPenalty) {
		return fmt.Errorf("settlement net amount does not reconcile")
	}
	firstFillAt := v.FirstFillAt
	if firstFillAt.IsZero() {
		firstFillAt = v.OpenedAt // Compatible with previously stored evidence.
	}
	if firstFillAt.Sub(v.OpenedAt).Abs() > time.Second || firstFillAt.After(v.ClosedAt) || (!v.ExchangeClosedAt.IsZero() && v.ExchangeClosedAt.Sub(v.ClosedAt).Abs() > time.Second) {
		return fmt.Errorf("settlement execution clocks outside bounded venue lifecycle")
	}
	identity := settlementLifecycleIdentity(v.PositionID, v.Symbol, v.Side, v.MarginMode, v.OpenedAt)
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type fill struct {
		id       int64
		qty, pnl float64
		time     string
	}
	var fills []fill
	var qty, pnl float64
	seen := map[string]bool{}
	for _, tradeID := range v.TradeIDs {
		if seen[tradeID] {
			return fmt.Errorf("duplicate settlement fill")
		}
		seen[tradeID] = true
		var f fill
		var symbol, side, quality string
		if err = tx.QueryRow(`SELECT id,quantity,realized_pnl,fill_time,symbol,side,data_quality FROM position_close_fills WHERE exchange_id=? AND exchange_trade_id=?`, v.ExchangeID, tradeID).Scan(&f.id, &f.qty, &f.pnl, &f.time, &symbol, &side, &quality); err != nil {
			return err
		}
		ts, e := parseDBTime(f.time)
		if e != nil || ts.Before(firstFillAt) || ts.After(v.ClosedAt) || symbol != v.Symbol || !strings.EqualFold(side, v.Side) || quality != "VERIFIED" {
			return fmt.Errorf("fill outside verified settlement lifecycle")
		}
		qty += f.qty
		pnl += f.pnl
		fills = append(fills, f)
	}
	if !closeEnough(qty, v.Quantity) || !closeEnough(pnl, v.GrossPnL) {
		return fmt.Errorf("settlement and immutable fills disagree")
	}
	// Every local allocation must refer to an entry in this exact lifecycle.
	// Unknown manual/synthetic lots remain pending instead of guessing scope.
	entries := map[string]bool{}
	for _, id := range v.EntryOrderIDs {
		entries[id] = true
	}
	for _, f := range fills {
		rows, e := tx.Query(`SELECT p.entry_order_id,p.entry_time FROM position_close_allocations a JOIN trader_positions p ON p.id=a.position_id WHERE a.fill_id=?`, f.id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var id, ts string
			if e = rows.Scan(&id, &ts); e != nil {
				rows.Close()
				return e
			}
			t, e := parseDBTime(ts)
			if !entries[id] || e != nil || t.Before(v.OpenedAt.Add(-time.Second)) || t.After(v.ClosedAt) {
				rows.Close()
				return fmt.Errorf("local allocation entry not proven by settlement")
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	if _, err = tx.Exec(`INSERT INTO position_settlements(exchange_id,identity,evidence) VALUES(?,?,?) ON CONFLICT(exchange_id,identity) DO NOTHING`, v.ExchangeID, identity, string(raw)); err != nil {
		return err
	}
	var settlementID int64
	if err = tx.QueryRow(`SELECT id FROM position_settlements WHERE exchange_id=? AND identity=?`, v.ExchangeID, identity).Scan(&settlementID); err != nil {
		return err
	}
	for _, f := range fills {
		var previous int64
		e := tx.QueryRow(`SELECT settlement_id FROM position_fill_settlements WHERE fill_id=?`, f.id).Scan(&previous)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if e == nil && previous != settlementID {
			return fmt.Errorf("fill already assigned to a different settlement lifecycle")
		}
		share := f.qty / v.Quantity
		if _, err = tx.Exec(`INSERT INTO position_fill_settlements(fill_id,settlement_id,fee,funding_fee,liquidation_penalty,net_pnl) VALUES(?,?,?,?,?,?) ON CONFLICT(fill_id) DO UPDATE SET fee=excluded.fee,funding_fee=excluded.funding_fee,liquidation_penalty=excluded.liquidation_penalty,net_pnl=excluded.net_pnl`, f.id, settlementID, v.Fee*share, v.FundingFee*share, v.LiquidationPenalty*share, f.pnl+(-v.Fee+v.FundingFee-v.LiquidationPenalty)*share); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO position_settlement_revisions(settlement_id,evidence) SELECT id,evidence FROM position_settlements WHERE id=?`, settlementID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE position_settlements SET evidence=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, string(raw), settlementID); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO position_settlement_revisions(settlement_id,evidence) VALUES(?,?)`, settlementID, string(raw)); err != nil {
		return err
	}
	// Recompute from the unique fill ledger on every run; never increment costs.
	if err = reconcileSettledLots(tx, settlementID, v.CloseType); err != nil {
		return err
	}
	return tx.Commit()
}

func reconcileSettledLots(tx *sql.Tx, settlementID int64, closeType string) error {
	rows, err := tx.Query(`SELECT DISTINCT a.position_id FROM position_close_allocations a JOIN position_fill_settlements s ON s.fill_id=a.fill_id WHERE s.settlement_id=?`, settlementID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		var gross, net, fee, funding, penalty float64
		var total, settled int
		var exitTime string
		if err = tx.QueryRow(`SELECT COUNT(*),COUNT(s.fill_id),SUM(a.realized_pnl),SUM(COALESCE(s.net_pnl,f.realized_pnl)*a.quantity/f.quantity),SUM(COALESCE(s.fee,f.fee)*a.quantity/f.quantity),SUM(COALESCE(s.funding_fee,0)*a.quantity/f.quantity),SUM(COALESCE(s.liquidation_penalty,0)*a.quantity/f.quantity), (SELECT f2.fill_time FROM position_close_allocations a2 JOIN position_close_fills f2 ON f2.id=a2.fill_id WHERE a2.position_id=a.position_id ORDER BY julianday(f2.fill_time) DESC,f2.id DESC LIMIT 1) FROM position_close_allocations a JOIN position_close_fills f ON f.id=a.fill_id LEFT JOIN position_fill_settlements s ON s.fill_id=f.id WHERE a.position_id=?`, id).Scan(&total, &settled, &gross, &net, &fee, &funding, &penalty, &exitTime); err != nil {
			return err
		}
		if total != settled {
			continue
		}
		if _, err = tx.Exec(`UPDATE trader_positions SET gross_pnl=?,net_pnl=?,realized_pnl=?,fee=?,funding_fee=?,liquidation_penalty=?,settlement_verified=1,exit_time=?,close_reason=CASE WHEN ?='liquidation' THEN 'liquidation' ELSE close_reason END,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='CLOSED'`, gross, net, net, fee, funding, penalty, exitTime, closeType, id); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO position_accounting_audits(position_id,reason_code,evidence) VALUES(?,'EXCHANGE_SETTLEMENT_VERIFIED',?)`, id, fmt.Sprintf("settlement=%d; unique immutable fill allocation", settlementID)); err != nil {
			return err
		}
	}
	return nil
}

func (s *PositionStore) PendingSettlementSince(exchangeID string) (time.Time, error) {
	var value sql.NullString
	err := s.db.QueryRow(`SELECT MIN(entry_time) FROM trader_positions WHERE exchange_id=? AND exchange_type='okx' AND status='CLOSED' AND settlement_verified=0 AND julianday(entry_time)>julianday('now','-89 days') AND EXISTS(SELECT 1 FROM position_close_allocations a WHERE a.position_id=trader_positions.id)`, exchangeID).Scan(&value)
	if err != nil || !value.Valid {
		return time.Time{}, err
	}
	return parseDBTime(value.String)
}

func (s *PositionStore) hydrateSettlement(p *TraderPosition) error {
	return s.db.QueryRow(`SELECT gross_pnl,net_pnl,funding_fee,liquidation_penalty,settlement_verified FROM trader_positions WHERE id=?`, p.ID).Scan(&p.GrossPnL, &p.NetPnL, &p.FundingFee, &p.LiquidationPenalty, &p.SettlementVerified)
}

func (s *PositionStore) HasPendingSettlement(exchangeID, symbol string, opened, closed time.Time) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM trader_positions p WHERE exchange_id=? AND symbol=? AND status='CLOSED' AND settlement_verified=0 AND julianday(entry_time)>=julianday(?) AND julianday(entry_time)<=julianday(?) AND EXISTS(SELECT 1 FROM position_close_allocations a WHERE a.position_id=p.id)`, exchangeID, symbol, opened.Add(-time.Second).UTC().Format(time.RFC3339Nano), closed.UTC().Format(time.RFC3339Nano)).Scan(&n)
	return n > 0, err
}
