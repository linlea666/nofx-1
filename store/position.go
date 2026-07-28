package store

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// TraderStats trading statistics metrics
type TraderStats struct {
	TotalTrades    int     `json:"total_trades"`     // Total trades (closed)
	WinTrades      int     `json:"win_trades"`       // Winning trades
	LossTrades     int     `json:"loss_trades"`      // Losing trades
	WinRate        float64 `json:"win_rate"`         // Win rate (%)
	ProfitFactor   float64 `json:"profit_factor"`    // Profit factor
	SharpeRatio    float64 `json:"sharpe_ratio"`     // Sharpe ratio
	TotalPnL       float64 `json:"total_pnl"`        // Total PnL
	TotalFee       float64 `json:"total_fee"`        // Total fees
	AvgWin         float64 `json:"avg_win"`          // Average win
	AvgLoss        float64 `json:"avg_loss"`         // Average loss
	MaxDrawdownPct float64 `json:"max_drawdown_pct"` // Max drawdown (%)
}

// TraderPosition position record (complete open/close position tracking)
type TraderPosition struct {
	ID                 int64      `json:"id"`
	TraderID           string     `json:"trader_id"`
	ExchangeID         string     `json:"exchange_id"`          // Exchange account UUID (for multi-account support)
	ExchangeType       string     `json:"exchange_type"`        // Exchange type: binance/bybit/okx/hyperliquid/aster/lighter
	ExchangePositionID string     `json:"exchange_position_id"` // Exchange-specific unique position ID for deduplication
	Symbol             string     `json:"symbol"`
	Side               string     `json:"side"`           // LONG/SHORT
	Quantity           float64    `json:"quantity"`       // Opening quantity
	EntryPrice         float64    `json:"entry_price"`    // Entry price
	EntryOrderID       string     `json:"entry_order_id"` // Entry order ID
	EntryTime          time.Time  `json:"entry_time"`     // Entry time
	ExitPrice          float64    `json:"exit_price"`     // Exit price
	ExitOrderID        string     `json:"exit_order_id"`  // Exit order ID
	ExitTime           *time.Time `json:"exit_time"`      // Exit time
	RealizedPnL        float64    `json:"realized_pnl"`   // Realized profit and loss
	Fee                float64    `json:"fee"`            // Fee
	Leverage           int        `json:"leverage"`       // Leverage multiplier
	Status             string     `json:"status"`         // OPEN/CLOSED
	CloseReason        string     `json:"close_reason"`   // Close reason: ai_decision/manual/stop_loss/take_profit
	Source             string     `json:"source"`         // Source: system/manual/sync
	AccountingQuality  string     `json:"accounting_quality"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// PositionStore position storage
type PositionStore struct {
	db *sql.DB
}

// NewPositionStore creates position storage instance
func NewPositionStore(db *sql.DB) *PositionStore {
	return &PositionStore{db: db}
}

// InitTables initializes position tables
func (s *PositionStore) InitTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS trader_positions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL,
			exchange_id TEXT NOT NULL DEFAULT '',
			exchange_position_id TEXT NOT NULL DEFAULT '',
			symbol TEXT NOT NULL,
			side TEXT NOT NULL,
			quantity REAL NOT NULL,
			entry_price REAL NOT NULL,
			entry_order_id TEXT DEFAULT '',
			entry_time DATETIME NOT NULL,
			exit_price REAL DEFAULT 0,
			exit_order_id TEXT DEFAULT '',
			exit_time DATETIME,
			realized_pnl REAL DEFAULT 0,
			fee REAL DEFAULT 0,
			leverage INTEGER DEFAULT 1,
			status TEXT DEFAULT 'OPEN',
			close_reason TEXT DEFAULT '',
			source TEXT DEFAULT 'system',
			accounting_quality TEXT NOT NULL DEFAULT 'VERIFIED',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create trader_positions table: %w", err)
	}

	// Migration: add exchange_id column to existing table (if not exists)
	// Must be executed before creating indexes!
	s.db.Exec(`ALTER TABLE trader_positions ADD COLUMN exchange_id TEXT NOT NULL DEFAULT ''`)
	// Migration: add exchange_type column (binance/bybit/okx/etc)
	s.db.Exec(`ALTER TABLE trader_positions ADD COLUMN exchange_type TEXT NOT NULL DEFAULT ''`)
	// Migration: add exchange_position_id for deduplication
	s.db.Exec(`ALTER TABLE trader_positions ADD COLUMN exchange_position_id TEXT NOT NULL DEFAULT ''`)
	// Migration: add source field (system/manual/sync)
	s.db.Exec(`ALTER TABLE trader_positions ADD COLUMN source TEXT DEFAULT 'system'`)
	if err := ensureSQLiteColumn(s.db, "trader_positions", "accounting_quality", "TEXT NOT NULL DEFAULT 'VERIFIED'"); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS position_close_fills (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		trader_id TEXT NOT NULL,
		exchange_id TEXT NOT NULL,
		exchange_trade_id TEXT NOT NULL,
		symbol TEXT NOT NULL,
		side TEXT NOT NULL,
		quantity REAL NOT NULL,
		exit_price REAL NOT NULL,
		realized_pnl REAL NOT NULL DEFAULT 0,
		fee REAL NOT NULL DEFAULT 0,
		fill_time DATETIME,
		data_quality TEXT NOT NULL DEFAULT 'VERIFIED',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(exchange_id,exchange_trade_id)
	)`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS position_close_allocations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		fill_id INTEGER NOT NULL DEFAULT 0,
		exchange_id TEXT NOT NULL,
		exchange_trade_id TEXT NOT NULL,
		position_id INTEGER NOT NULL,
		symbol TEXT NOT NULL,
		side TEXT NOT NULL,
		quantity REAL NOT NULL DEFAULT 0,
		exit_price REAL NOT NULL DEFAULT 0,
		realized_pnl REAL NOT NULL DEFAULT 0,
		fee REAL NOT NULL DEFAULT 0,
		allocation_quality TEXT NOT NULL DEFAULT 'ALLOCATED_ESTIMATE',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(exchange_id,exchange_trade_id,position_id)
	)`); err != nil {
		return err
	}
	if err := ensureSQLiteColumn(s.db, "position_close_allocations", "fill_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureSQLiteColumn(s.db, "position_close_allocations", "exit_price", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureSQLiteColumn(s.db, "position_close_allocations", "allocation_quality", "TEXT NOT NULL DEFAULT 'ALLOCATED_ESTIMATE'"); err != nil {
		return err
	}
	// 51eab63 already persisted uniquely allocated exchange fills, but the old
	// table had no authoritative-fill parent. Reconstruct only evidence that is
	// uniquely present in that allocation ledger, then link every old row to
	// its parent. This is deterministic and restart-idempotent.
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO position_close_fills
		(trader_id,exchange_id,exchange_trade_id,symbol,side,quantity,exit_price,realized_pnl,fee,fill_time,data_quality)
		SELECT MIN(p.trader_id),a.exchange_id,a.exchange_trade_id,MIN(a.symbol),UPPER(MIN(a.side)),
		       SUM(a.quantity),
		       CASE WHEN SUM(a.quantity)>0 THEN SUM(a.exit_price*a.quantity)/SUM(a.quantity) ELSE 0 END,
		       SUM(a.realized_pnl),SUM(a.fee),MAX(p.exit_time),'MIGRATED_VERIFIED'
		FROM position_close_allocations a
		JOIN trader_positions p ON p.id=a.position_id
		WHERE a.exchange_trade_id<>'' AND a.quantity>0
		GROUP BY a.exchange_id,a.exchange_trade_id`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE position_close_allocations
		SET fill_id=(
			SELECT f.id FROM position_close_fills f
			WHERE f.exchange_id=position_close_allocations.exchange_id
			  AND f.exchange_trade_id=position_close_allocations.exchange_trade_id
		)
		WHERE fill_id=0 AND EXISTS (
			SELECT 1 FROM position_close_fills f
			WHERE f.exchange_id=position_close_allocations.exchange_id
			  AND f.exchange_trade_id=position_close_allocations.exchange_trade_id
		)`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS position_accounting_audits (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		position_id INTEGER NOT NULL,
		reason_code TEXT NOT NULL,
		evidence TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(position_id,reason_code)
	)`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE VIEW IF NOT EXISTS trusted_closed_positions AS
		SELECT p.* FROM trader_positions p
		WHERE p.status='CLOSED' AND (
			COALESCE(p.accounting_quality,'')='ALLOCATED_ESTIMATE'
			OR (
				COALESCE(p.accounting_quality,'')='VERIFIED'
				AND (
					EXISTS (
						SELECT 1 FROM position_close_allocations a
						WHERE a.position_id=p.id AND a.fill_id>0
					)
					OR EXISTS (
						SELECT 1 FROM position_close_fills f
						WHERE f.exchange_id=p.exchange_id
						  AND f.exchange_trade_id IN (
							COALESCE(p.exchange_position_id,''),
							COALESCE(p.exit_order_id,'')
						  )
					)
				)
			)
		)`); err != nil {
		return err
	}
	// Historical equality is only a suspicion, not proof that two legitimate
	// local lots shared one exchange fill. Preserve every raw row and record an
	// audit flag for operator review instead of mutating accounting quality at
	// startup.
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO position_accounting_audits(position_id,reason_code,evidence)
		SELECT trader_positions.id,'POSSIBLE_LEGACY_DUPLICATION','same trader/symbol/side/order/time/pnl as an earlier local row'
		FROM trader_positions WHERE status='CLOSED' AND EXISTS (
			SELECT 1 FROM trader_positions p2
			WHERE p2.id<trader_positions.id
			  AND p2.trader_id=trader_positions.trader_id
			  AND p2.symbol=trader_positions.symbol AND p2.side=trader_positions.side
			  AND COALESCE(p2.exit_order_id,'')=COALESCE(trader_positions.exit_order_id,'')
			  AND p2.exit_time=trader_positions.exit_time
			  AND ABS(p2.realized_pnl-trader_positions.realized_pnl)<0.000000001
		)`); err != nil {
		return err
	}

	// Create indexes (after migration)
	indices := []string{
		`CREATE INDEX IF NOT EXISTS idx_positions_trader ON trader_positions(trader_id)`,
		`CREATE INDEX IF NOT EXISTS idx_positions_exchange ON trader_positions(exchange_id)`,
		`CREATE INDEX IF NOT EXISTS idx_positions_status ON trader_positions(trader_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_positions_symbol ON trader_positions(trader_id, symbol, side, status)`,
		`CREATE INDEX IF NOT EXISTS idx_positions_entry ON trader_positions(trader_id, entry_time DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_positions_exit ON trader_positions(trader_id, exit_time DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_position_close_fills_trader_time ON position_close_fills(trader_id,fill_time)`,
		`CREATE INDEX IF NOT EXISTS idx_position_close_fills_symbol ON position_close_fills(exchange_id,symbol,side,fill_time)`,
		`CREATE INDEX IF NOT EXISTS idx_position_close_allocations_fill ON position_close_allocations(fill_id,position_id)`,
		// Unique index based on exchange_id (account UUID), not trader_id
		// This ensures the same position from an exchange account is not duplicated across different traders
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_positions_exchange_pos_unique ON trader_positions(exchange_id, exchange_position_id) WHERE exchange_position_id != ''`,
	}
	for _, idx := range indices {
		if _, err := s.db.Exec(idx); err != nil {
			// Ignore unique index creation errors for existing data
			if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return fmt.Errorf("failed to create index: %w", err)
			}
		}
	}

	return nil
}

// Create creates position record (called when opening position)
func (s *PositionStore) Create(pos *TraderPosition) error {
	now := time.Now()
	pos.CreatedAt = now
	pos.UpdatedAt = now
	pos.Status = "OPEN"

	result, err := s.db.Exec(`
		INSERT INTO trader_positions (
			trader_id, exchange_id, exchange_type, symbol, side, quantity, entry_price, entry_order_id,
			entry_time, leverage, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		pos.TraderID, pos.ExchangeID, pos.ExchangeType, pos.Symbol, pos.Side, pos.Quantity, pos.EntryPrice,
		pos.EntryOrderID, pos.EntryTime.Format(time.RFC3339), pos.Leverage,
		pos.Status, now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("failed to create position record: %w", err)
	}

	id, _ := result.LastInsertId()
	pos.ID = id
	return nil
}

// ClosePosition closes position (updates position record)
func (s *PositionStore) ClosePosition(id int64, exitPrice float64, exitOrderID string, realizedPnL float64, fee float64, closeReason string) error {
	now := time.Now()
	_, err := s.db.Exec(`
		UPDATE trader_positions SET
			exit_price = ?, exit_order_id = ?, exit_time = ?,
			realized_pnl = ?, fee = ?, status = 'CLOSED',
			close_reason = ?, updated_at = ?
		WHERE id = ?
	`,
		exitPrice, exitOrderID, now.Format(time.RFC3339),
		realizedPnL, fee, closeReason, now.Format(time.RFC3339), id,
	)
	if err != nil {
		return fmt.Errorf("failed to update position record: %w", err)
	}
	return nil
}

type PositionCloseFill struct {
	TradeID     string
	Symbol      string
	Side        string
	Quantity    float64
	ExitPrice   float64
	RealizedPnL float64
	Fee         float64
	FillTime    time.Time
	DataQuality string
}

// ClosePositionWithAllocation is the single-fill compatibility wrapper.
func (s *PositionStore) ClosePositionWithAllocation(id int64, exchangeID, tradeID, symbol, side string, quantity, exitPrice, realizedPnL, fee float64, closeReason string) (bool, error) {
	return s.ClosePositionWithAllocations(id, exchangeID, []PositionCloseFill{{
		TradeID: tradeID, Symbol: symbol, Side: side, Quantity: quantity,
		ExitPrice: exitPrice, RealizedPnL: realizedPnL, Fee: fee, FillTime: time.Now(),
	}}, exitPrice, closeReason)
}

// ClosePositionWithAllocations first stores each exchange fill exactly once,
// then allocates the remaining quantity across compatible local lots in FIFO
// order. Authoritative account totals come from position_close_fills; local
// lots are estimates because one exchange fill may span multiple lots.
func (s *PositionStore) ClosePositionWithAllocations(id int64, exchangeID string, fills []PositionCloseFill, fallbackExitPrice float64, closeReason string) (bool, error) {
	exchangeID = strings.TrimSpace(exchangeID)
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var traderID, positionExchangeID, positionSymbol, positionSide string
	var positionQuantity float64
	if err = tx.QueryRow(`SELECT trader_id,COALESCE(exchange_id,''),symbol,side,quantity FROM trader_positions WHERE id=?`,
		id).Scan(&traderID, &positionExchangeID, &positionSymbol, &positionSide, &positionQuantity); err != nil {
		return false, err
	}
	positionExchangeID = strings.TrimSpace(positionExchangeID)
	if exchangeID != "" && positionExchangeID != "" && exchangeID != positionExchangeID {
		return false, fmt.Errorf("close fill account mismatch: position=%s fill=%s", positionExchangeID, exchangeID)
	}
	if exchangeID == "" {
		exchangeID = positionExchangeID
	}
	if exchangeID == "" {
		exchangeID = "unknown:" + traderID
	}
	type authoritativeFill struct {
		id          int64
		tradeID     string
		symbol      string
		side        string
		quantity    float64
		exitPrice   float64
		realizedPnL float64
		fee         float64
		fillTime    sql.NullString
	}
	var authoritative []authoritativeFill
	for _, fill := range fills {
		fill.TradeID = strings.TrimSpace(fill.TradeID)
		if fill.TradeID == "" || fill.Quantity <= 0 || fill.ExitPrice <= 0 {
			continue
		}
		symbol := fill.Symbol
		if symbol == "" {
			symbol = positionSymbol
		}
		side := strings.ToUpper(fill.Side)
		if side == "" {
			side = strings.ToUpper(positionSide)
		}
		if !strings.EqualFold(symbol, positionSymbol) || !strings.EqualFold(side, positionSide) {
			return false, fmt.Errorf("close fill instrument mismatch: position=%s/%s fill=%s/%s",
				positionSymbol, positionSide, symbol, side)
		}
		quality := fill.DataQuality
		if quality == "" {
			quality = "VERIFIED"
		}
		var fillTime interface{}
		if !fill.FillTime.IsZero() {
			fillTime = fill.FillTime.UTC().Format(time.RFC3339Nano)
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO position_close_fills
			(trader_id,exchange_id,exchange_trade_id,symbol,side,quantity,exit_price,realized_pnl,fee,fill_time,data_quality)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			traderID, exchangeID, fill.TradeID, symbol, side, fill.Quantity,
			fill.ExitPrice, fill.RealizedPnL, fill.Fee, fillTime, quality); err != nil {
			return false, err
		}
		var stored authoritativeFill
		var storedTrader, storedQuality string
		if err = tx.QueryRow(`SELECT id,trader_id,exchange_trade_id,symbol,side,quantity,exit_price,realized_pnl,fee,fill_time,data_quality
			FROM position_close_fills WHERE exchange_id=? AND exchange_trade_id=?`,
			exchangeID, fill.TradeID).Scan(
			&stored.id, &storedTrader, &stored.tradeID, &stored.symbol, &stored.side,
			&stored.quantity, &stored.exitPrice, &stored.realizedPnL, &stored.fee,
			&stored.fillTime, &storedQuality,
		); err != nil {
			return false, err
		}
		tolerance := func(a float64) float64 { return math.Max(1e-9, math.Abs(a)*1e-8) }
		if !strings.EqualFold(stored.symbol, symbol) || !strings.EqualFold(stored.side, side) ||
			math.Abs(stored.quantity-fill.Quantity) > tolerance(fill.Quantity) ||
			math.Abs(stored.exitPrice-fill.ExitPrice) > tolerance(fill.ExitPrice) ||
			math.Abs(stored.realizedPnL-fill.RealizedPnL) > tolerance(fill.RealizedPnL) ||
			math.Abs(stored.fee-fill.Fee) > tolerance(fill.Fee) {
			return false, fmt.Errorf("authoritative close fill conflict for account=%s trade=%s", exchangeID, fill.TradeID)
		}
		authoritative = append(authoritative, stored)
	}

	type localLot struct {
		id        int64
		quantity  float64
		status    string
		entryTime time.Time
	}
	rows, err := tx.Query(`SELECT id,quantity,status,entry_time FROM trader_positions
		WHERE (exchange_id=? OR (id=? AND COALESCE(exchange_id,'')=''))
		  AND symbol=? AND UPPER(side)=UPPER(?)
		  AND (status='OPEN' OR COALESCE(accounting_quality,'') IN ('PENDING','UNSCORABLE','ALLOCATED_ESTIMATE'))
		ORDER BY datetime(entry_time),id`,
		exchangeID, id, positionSymbol, positionSide)
	if err != nil {
		return false, err
	}
	var lots []localLot
	for rows.Next() {
		var lot localLot
		var entryTime string
		if err = rows.Scan(&lot.id, &lot.quantity, &lot.status, &entryTime); err != nil {
			_ = rows.Close()
			return false, err
		}
		lot.entryTime, _ = parseDBTime(entryTime)
		lots = append(lots, lot)
	}
	if err = rows.Close(); err != nil {
		return false, err
	}
	newAllocatedToTarget := false
	for _, fill := range authoritative {
		var allocatedFromFill float64
		if err = tx.QueryRow(`SELECT COALESCE(SUM(quantity),0) FROM position_close_allocations WHERE fill_id=?`,
			fill.id).Scan(&allocatedFromFill); err != nil {
			return false, err
		}
		fillRemaining := math.Max(0, fill.quantity-allocatedFromFill)
		for _, lot := range lots {
			if fillRemaining <= math.Max(1e-12, fill.quantity*1e-8) {
				break
			}
			if fill.fillTime.Valid {
				fillTimestamp, parseErr := parseDBTime(fill.fillTime.String)
				if parseErr == nil && !lot.entryTime.IsZero() && fillTimestamp.Before(lot.entryTime) {
					continue
				}
			}
			var allocatedToLot float64
			if err = tx.QueryRow(`SELECT COALESCE(SUM(quantity),0) FROM position_close_allocations WHERE position_id=?`,
				lot.id).Scan(&allocatedToLot); err != nil {
				return false, err
			}
			lotRemaining := math.Max(0, lot.quantity-allocatedToLot)
			allocated := math.Min(fillRemaining, lotRemaining)
			if allocated <= math.Max(1e-12, lot.quantity*1e-8) {
				continue
			}
			share := allocated / fill.quantity
			res, insertErr := tx.Exec(`INSERT OR IGNORE INTO position_close_allocations
				(fill_id,exchange_id,exchange_trade_id,position_id,symbol,side,quantity,exit_price,realized_pnl,fee,allocation_quality)
				VALUES(?,?,?,?,?,?,?,?,?,?,'ALLOCATED_ESTIMATE')`,
				fill.id, exchangeID, fill.tradeID, lot.id, fill.symbol, fill.side,
				allocated, fill.exitPrice, fill.realizedPnL*share, fill.fee*share)
			if insertErr != nil {
				return false, insertErr
			}
			if claimed, _ := res.RowsAffected(); claimed == 1 {
				fillRemaining -= allocated
				if lot.id == id {
					newAllocatedToTarget = true
				}
			}
		}
	}
	now := time.Now().Format(time.RFC3339)
	for _, lot := range lots {
		var allocatedQuantity, allocatedPnL, allocatedFee, weightedExit float64
		var exitOrderID string
		if err = tx.QueryRow(`SELECT COALESCE(SUM(quantity),0),COALESCE(SUM(realized_pnl),0),COALESCE(SUM(fee),0),
			COALESCE(SUM(exit_price*quantity),0),COALESCE(MAX(exchange_trade_id),'')
			FROM position_close_allocations WHERE exchange_id=? AND position_id=?`,
			exchangeID, lot.id).Scan(&allocatedQuantity, &allocatedPnL, &allocatedFee, &weightedExit, &exitOrderID); err != nil {
			return false, err
		}
		if allocatedQuantity <= math.Max(1e-12, lot.quantity*1e-8) {
			if lot.id == id {
				if _, err = tx.Exec(`UPDATE trader_positions SET
					exit_price=?,exit_time=COALESCE(exit_time,?),status='CLOSED',
					close_reason=?,accounting_quality='UNSCORABLE',updated_at=? WHERE id=?`,
					fallbackExitPrice, now, closeReason, now, lot.id); err != nil {
					return false, err
				}
			}
			continue
		}
		quality := "ALLOCATED_ESTIMATE"
		if allocatedQuantity+math.Max(1e-12, lot.quantity*1e-8) < lot.quantity {
			if strings.EqualFold(lot.status, "OPEN") {
				// A partial close consumes only part of this local lot. Preserve
				// the unclosed remainder as a new OPEN lot before shrinking the
				// original row to the authoritative closed quantity.
				remaining := lot.quantity - allocatedQuantity
				if _, err = tx.Exec(`INSERT INTO trader_positions
					(trader_id,exchange_id,exchange_type,exchange_position_id,symbol,side,quantity,
					 entry_price,entry_order_id,entry_time,leverage,status,close_reason,source,
					 accounting_quality,created_at,updated_at)
					SELECT trader_id,exchange_id,exchange_type,'',symbol,side,?,
					       entry_price,entry_order_id,entry_time,leverage,'OPEN','',source,
					       'VERIFIED',?,?
					FROM trader_positions WHERE id=?`,
					remaining, now, now, lot.id); err != nil {
					return false, err
				}
				lot.quantity = allocatedQuantity
			} else {
				quality = "PENDING"
			}
		}
		exitPrice := weightedExit / allocatedQuantity
		if _, err = tx.Exec(`UPDATE trader_positions SET quantity=?,exit_price=?,exit_order_id=?,exit_time=COALESCE(exit_time,?),
			realized_pnl=?,fee=?,status='CLOSED',close_reason=?,accounting_quality=?,updated_at=? WHERE id=?`,
			lot.quantity, exitPrice, exitOrderID, now, allocatedPnL, allocatedFee, closeReason, quality, now, lot.id); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	var targetAllocations int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM position_close_allocations WHERE exchange_id=? AND position_id=?`,
		exchangeID, id).Scan(&targetAllocations); err != nil {
		return false, err
	}
	return newAllocatedToTarget || targetAllocations > 0, nil
}

func (s *PositionStore) ClosePositionUnscorable(id int64, exitPrice float64, closeReason string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := s.db.Exec(`UPDATE trader_positions SET exit_price=?,exit_time=?,realized_pnl=0,fee=0,status='CLOSED',close_reason=?,accounting_quality='UNSCORABLE',updated_at=? WHERE id=?`,
		exitPrice, now, closeReason, now, id)
	return err
}

// GetOpenPositions gets all open positions
func (s *PositionStore) GetOpenPositions(traderID string) ([]*TraderPosition, error) {
	rows, err := s.db.Query(`
		SELECT id, trader_id, exchange_id, COALESCE(exchange_type, '') as exchange_type, symbol, side, quantity, entry_price, entry_order_id,
			entry_time, exit_price, exit_order_id, exit_time, realized_pnl, fee,
			leverage, status, close_reason, COALESCE(accounting_quality,'VERIFIED'), created_at, updated_at
		FROM trader_positions
		WHERE trader_id = ? AND status = 'OPEN'
		ORDER BY entry_time DESC
	`, traderID)
	if err != nil {
		return nil, fmt.Errorf("failed to query open positions: %w", err)
	}
	defer rows.Close()

	return s.scanPositions(rows)
}

// GetOpenPositionBySymbol gets open position for specified symbol and direction
func (s *PositionStore) GetOpenPositionBySymbol(traderID, symbol, side string) (*TraderPosition, error) {
	var pos TraderPosition
	var entryTime, exitTime, createdAt, updatedAt sql.NullString

	err := s.db.QueryRow(`
		SELECT id, trader_id, exchange_id, COALESCE(exchange_type, '') as exchange_type, symbol, side, quantity, entry_price, entry_order_id,
			entry_time, exit_price, exit_order_id, exit_time, realized_pnl, fee,
			leverage, status, close_reason, COALESCE(accounting_quality,'VERIFIED'), created_at, updated_at
		FROM trader_positions
		WHERE trader_id = ? AND symbol = ? AND side = ? AND status = 'OPEN'
		ORDER BY entry_time DESC LIMIT 1
	`, traderID, symbol, side).Scan(
		&pos.ID, &pos.TraderID, &pos.ExchangeID, &pos.ExchangeType, &pos.Symbol, &pos.Side, &pos.Quantity,
		&pos.EntryPrice, &pos.EntryOrderID, &entryTime, &pos.ExitPrice,
		&pos.ExitOrderID, &exitTime, &pos.RealizedPnL, &pos.Fee,
		&pos.Leverage, &pos.Status, &pos.CloseReason, &pos.AccountingQuality, &createdAt, &updatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	s.parsePositionTimes(&pos, entryTime, exitTime, createdAt, updatedAt)
	return &pos, nil
}

// GetClosedPositions gets closed positions (historical records)
func (s *PositionStore) GetClosedPositions(traderID string, limit int) ([]*TraderPosition, error) {
	rows, err := s.db.Query(`
		SELECT id, trader_id, exchange_id, COALESCE(exchange_type, '') as exchange_type, symbol, side, quantity, entry_price, entry_order_id,
			entry_time, exit_price, exit_order_id, exit_time, realized_pnl, fee,
			leverage, status, close_reason, COALESCE(accounting_quality,'VERIFIED'), created_at, updated_at
		FROM trader_positions
		WHERE trader_id = ? AND status = 'CLOSED'
		ORDER BY exit_time DESC
		LIMIT ?
	`, traderID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query closed positions: %w", err)
	}
	defer rows.Close()

	return s.scanPositions(rows)
}

// GetPendingClosedPositions returns locally closed lots whose exchange
// settlement is incomplete. They remain eligible for delayed authoritative
// fills instead of being frozen by the first UNSCORABLE observation.
func (s *PositionStore) GetPendingClosedPositions(traderID string, limit int) ([]*TraderPosition, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`
		SELECT id, trader_id, exchange_id, COALESCE(exchange_type, '') as exchange_type, symbol, side, quantity, entry_price, entry_order_id,
			entry_time, exit_price, exit_order_id, exit_time, realized_pnl, fee,
			leverage, status, close_reason, COALESCE(accounting_quality,'VERIFIED'), created_at, updated_at
		FROM trader_positions
		WHERE trader_id = ? AND status = 'CLOSED'
		  AND COALESCE(accounting_quality,'') IN ('PENDING','UNSCORABLE')
		ORDER BY datetime(entry_time), id
		LIMIT ?
	`, traderID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query pending closed positions: %w", err)
	}
	defer rows.Close()
	return s.scanPositions(rows)
}

// GetAllOpenPositions gets all traders' open positions (for global sync)
func (s *PositionStore) GetAllOpenPositions() ([]*TraderPosition, error) {
	rows, err := s.db.Query(`
		SELECT id, trader_id, exchange_id, COALESCE(exchange_type, '') as exchange_type, symbol, side, quantity, entry_price, entry_order_id,
			entry_time, exit_price, exit_order_id, exit_time, realized_pnl, fee,
			leverage, status, close_reason, COALESCE(accounting_quality,'VERIFIED'), created_at, updated_at
		FROM trader_positions
		WHERE status = 'OPEN'
		ORDER BY trader_id, entry_time DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query all open positions: %w", err)
	}
	defer rows.Close()

	return s.scanPositions(rows)
}

// GetPositionStats gets position statistics (simplified version)
func (s *PositionStore) GetPositionStats(traderID string) (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	// Total trades
	var totalTrades, winTrades int
	var totalPnL, totalFee float64

	err := s.db.QueryRow(`
		SELECT
			COUNT(*) as total,
			SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END) as wins,
			COALESCE(SUM(realized_pnl), 0) as total_pnl,
			COALESCE(SUM(fee), 0) as total_fee
		FROM trusted_closed_positions
		WHERE trader_id = ?
	`, traderID).Scan(&totalTrades, &winTrades, &totalPnL, &totalFee)
	if err != nil {
		return nil, err
	}

	stats["total_trades"] = totalTrades
	stats["win_trades"] = winTrades
	stats["total_pnl"] = totalPnL
	stats["total_fee"] = totalFee
	if totalTrades > 0 {
		stats["win_rate"] = float64(winTrades) / float64(totalTrades) * 100
	} else {
		stats["win_rate"] = 0.0
	}

	return stats, nil
}

// GetFullStats gets complete trading statistics (compatible with TraderStats)
func (s *PositionStore) GetFullStats(traderID string) (*TraderStats, error) {
	stats := &TraderStats{}

	// Query all closed positions
	rows, err := s.db.Query(`
		SELECT realized_pnl, fee, exit_time
		FROM trusted_closed_positions
		WHERE trader_id = ?
		ORDER BY exit_time ASC
	`, traderID)
	if err != nil {
		return nil, fmt.Errorf("failed to query position statistics: %w", err)
	}
	defer rows.Close()

	var pnls []float64
	var totalWin, totalLoss float64

	for rows.Next() {
		var pnl, fee float64
		var exitTime sql.NullString
		if err := rows.Scan(&pnl, &fee, &exitTime); err != nil {
			continue
		}

		stats.TotalTrades++
		stats.TotalPnL += pnl
		stats.TotalFee += fee
		pnls = append(pnls, pnl)

		if pnl > 0 {
			stats.WinTrades++
			totalWin += pnl
		} else if pnl < 0 {
			stats.LossTrades++
			totalLoss += -pnl // Convert to positive
		}
	}

	// Calculate win rate
	if stats.TotalTrades > 0 {
		stats.WinRate = float64(stats.WinTrades) / float64(stats.TotalTrades) * 100
	}

	// Calculate profit factor
	if totalLoss > 0 {
		stats.ProfitFactor = totalWin / totalLoss
	}

	// Calculate average profit/loss
	if stats.WinTrades > 0 {
		stats.AvgWin = totalWin / float64(stats.WinTrades)
	}
	if stats.LossTrades > 0 {
		stats.AvgLoss = totalLoss / float64(stats.LossTrades)
	}

	// Calculate Sharpe ratio
	if len(pnls) > 1 {
		stats.SharpeRatio = calculateSharpeRatioFromPnls(pnls)
	}

	// Calculate maximum drawdown
	if len(pnls) > 0 {
		stats.MaxDrawdownPct = calculateMaxDrawdownFromPnls(pnls)
	}

	return stats, nil
}

// RecentTrade recent trade record (for AI input)
type RecentTrade struct {
	Symbol       string  `json:"symbol"`
	Side         string  `json:"side"` // long/short
	EntryPrice   float64 `json:"entry_price"`
	ExitPrice    float64 `json:"exit_price"`
	RealizedPnL  float64 `json:"realized_pnl"`
	PnLPct       float64 `json:"pnl_pct"`
	EntryTime    string  `json:"entry_time"`    // Entry time (开仓时间)
	ExitTime     string  `json:"exit_time"`     // Exit time (平仓时间)
	HoldDuration string  `json:"hold_duration"` // Hold duration (持仓时长), e.g. "2h30m"
}

// GetRecentTrades gets recent closed trades
func (s *PositionStore) GetRecentTrades(traderID string, limit int) ([]RecentTrade, error) {
	rows, err := s.db.Query(`
		SELECT symbol, side, entry_price, exit_price, realized_pnl, leverage, entry_time, exit_time
		FROM trusted_closed_positions
		WHERE trader_id = ?
		ORDER BY exit_time DESC
		LIMIT ?
	`, traderID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query recent trades: %w", err)
	}
	defer rows.Close()

	var trades []RecentTrade
	for rows.Next() {
		var t RecentTrade
		var leverage int
		var entryTime, exitTime sql.NullString

		err := rows.Scan(&t.Symbol, &t.Side, &t.EntryPrice, &t.ExitPrice, &t.RealizedPnL, &leverage, &entryTime, &exitTime)
		if err != nil {
			continue
		}

		// Convert side format
		if t.Side == "LONG" {
			t.Side = "long"
		} else if t.Side == "SHORT" {
			t.Side = "short"
		}

		// Calculate profit/loss percentage
		if t.EntryPrice > 0 {
			if t.Side == "long" {
				t.PnLPct = (t.ExitPrice - t.EntryPrice) / t.EntryPrice * 100 * float64(leverage)
			} else {
				t.PnLPct = (t.EntryPrice - t.ExitPrice) / t.EntryPrice * 100 * float64(leverage)
			}
		}

		// Format entry time and exit time (always use UTC and indicate it)
		var parsedEntryTime, parsedExitTime time.Time
		if entryTime.Valid {
			if parsed, err := time.Parse(time.RFC3339, entryTime.String); err == nil {
				parsedEntryTime = parsed.UTC()
				t.EntryTime = parsedEntryTime.Format("01-02 15:04 UTC")
			}
		}
		if exitTime.Valid {
			if parsed, err := time.Parse(time.RFC3339, exitTime.String); err == nil {
				parsedExitTime = parsed.UTC()
				t.ExitTime = parsedExitTime.Format("01-02 15:04 UTC")
			}
		}

		// Calculate hold duration
		if !parsedEntryTime.IsZero() && !parsedExitTime.IsZero() {
			duration := parsedExitTime.Sub(parsedEntryTime)
			t.HoldDuration = formatDuration(duration)
		}

		trades = append(trades, t)
	}

	return trades, nil
}

// formatDuration formats a duration into a human-readable string
// e.g. "2d3h", "5h30m", "45m", "30s"
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}

// calculateSharpeRatioFromPnls calculates Sharpe ratio
func calculateSharpeRatioFromPnls(pnls []float64) float64 {
	if len(pnls) < 2 {
		return 0
	}

	var sum float64
	for _, pnl := range pnls {
		sum += pnl
	}
	mean := sum / float64(len(pnls))

	var variance float64
	for _, pnl := range pnls {
		variance += (pnl - mean) * (pnl - mean)
	}
	stdDev := math.Sqrt(variance / float64(len(pnls)-1))

	if stdDev == 0 {
		return 0
	}

	return mean / stdDev
}

// calculateMaxDrawdownFromPnls calculates maximum drawdown
func calculateMaxDrawdownFromPnls(pnls []float64) float64 {
	if len(pnls) == 0 {
		return 0
	}

	var cumulative, peak, maxDD float64
	for _, pnl := range pnls {
		cumulative += pnl
		if cumulative > peak {
			peak = cumulative
		}
		if peak > 0 {
			dd := (peak - cumulative) / peak * 100
			if dd > maxDD {
				maxDD = dd
			}
		}
	}

	return maxDD
}

// scanPositions scans position rows into structs
func (s *PositionStore) scanPositions(rows *sql.Rows) ([]*TraderPosition, error) {
	var positions []*TraderPosition
	for rows.Next() {
		var pos TraderPosition
		var entryTime, exitTime, createdAt, updatedAt sql.NullString

		err := rows.Scan(
			&pos.ID, &pos.TraderID, &pos.ExchangeID, &pos.ExchangeType, &pos.Symbol, &pos.Side, &pos.Quantity,
			&pos.EntryPrice, &pos.EntryOrderID, &entryTime, &pos.ExitPrice,
			&pos.ExitOrderID, &exitTime, &pos.RealizedPnL, &pos.Fee,
			&pos.Leverage, &pos.Status, &pos.CloseReason, &pos.AccountingQuality, &createdAt, &updatedAt,
		)
		if err != nil {
			continue
		}

		s.parsePositionTimes(&pos, entryTime, exitTime, createdAt, updatedAt)
		positions = append(positions, &pos)
	}

	return positions, nil
}

// parsePositionTimes parses time fields
func (s *PositionStore) parsePositionTimes(pos *TraderPosition, entryTime, exitTime, createdAt, updatedAt sql.NullString) {
	if entryTime.Valid {
		pos.EntryTime, _ = time.Parse(time.RFC3339, entryTime.String)
	}
	if exitTime.Valid {
		t, _ := time.Parse(time.RFC3339, exitTime.String)
		pos.ExitTime = &t
	}
	if createdAt.Valid {
		pos.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
	}
	if updatedAt.Valid {
		pos.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
	}
}

// SymbolStats per-symbol trading statistics
type SymbolStats struct {
	Symbol      string  `json:"symbol"`
	TotalTrades int     `json:"total_trades"`
	WinTrades   int     `json:"win_trades"`
	WinRate     float64 `json:"win_rate"`
	TotalPnL    float64 `json:"total_pnl"`
	AvgPnL      float64 `json:"avg_pnl"`
	AvgHoldMins float64 `json:"avg_hold_mins"` // Average holding time in minutes
}

// GetSymbolStats gets per-symbol trading statistics
func (s *PositionStore) GetSymbolStats(traderID string, limit int) ([]SymbolStats, error) {
	rows, err := s.db.Query(`
		SELECT
			symbol,
			COUNT(*) as total_trades,
			SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END) as win_trades,
			COALESCE(SUM(realized_pnl), 0) as total_pnl,
			COALESCE(AVG(realized_pnl), 0) as avg_pnl,
			COALESCE(AVG((julianday(exit_time) - julianday(entry_time)) * 24 * 60), 0) as avg_hold_mins
		FROM trusted_closed_positions
		WHERE trader_id = ?
		GROUP BY symbol
		ORDER BY total_pnl DESC
		LIMIT ?
	`, traderID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query symbol stats: %w", err)
	}
	defer rows.Close()

	var stats []SymbolStats
	for rows.Next() {
		var s SymbolStats
		err := rows.Scan(&s.Symbol, &s.TotalTrades, &s.WinTrades, &s.TotalPnL, &s.AvgPnL, &s.AvgHoldMins)
		if err != nil {
			continue
		}
		if s.TotalTrades > 0 {
			s.WinRate = float64(s.WinTrades) / float64(s.TotalTrades) * 100
		}
		stats = append(stats, s)
	}
	return stats, nil
}

// HoldingTimeStats holding duration analysis
type HoldingTimeStats struct {
	Range      string  `json:"range"` // e.g., "<1h", "1-4h", "4-24h", ">24h"
	TradeCount int     `json:"trade_count"`
	WinRate    float64 `json:"win_rate"`
	AvgPnL     float64 `json:"avg_pnl"`
}

// GetHoldingTimeStats analyzes performance by holding duration
func (s *PositionStore) GetHoldingTimeStats(traderID string) ([]HoldingTimeStats, error) {
	rows, err := s.db.Query(`
		WITH holding AS (
			SELECT
				realized_pnl,
				(julianday(exit_time) - julianday(entry_time)) * 24 as hold_hours
			FROM trusted_closed_positions
			WHERE trader_id = ? AND exit_time IS NOT NULL
		)
		SELECT
			CASE
				WHEN hold_hours < 1 THEN '<1h'
				WHEN hold_hours < 4 THEN '1-4h'
				WHEN hold_hours < 24 THEN '4-24h'
				ELSE '>24h'
			END as time_range,
			COUNT(*) as trade_count,
			SUM(CASE WHEN realized_pnl > 0 THEN 1.0 ELSE 0.0 END) / COUNT(*) * 100 as win_rate,
			AVG(realized_pnl) as avg_pnl
		FROM holding
		GROUP BY time_range
		ORDER BY
			CASE time_range
				WHEN '<1h' THEN 1
				WHEN '1-4h' THEN 2
				WHEN '4-24h' THEN 3
				ELSE 4
			END
	`, traderID)
	if err != nil {
		return nil, fmt.Errorf("failed to query holding time stats: %w", err)
	}
	defer rows.Close()

	var stats []HoldingTimeStats
	for rows.Next() {
		var s HoldingTimeStats
		err := rows.Scan(&s.Range, &s.TradeCount, &s.WinRate, &s.AvgPnL)
		if err != nil {
			continue
		}
		stats = append(stats, s)
	}
	return stats, nil
}

// DirectionStats long/short performance comparison
type DirectionStats struct {
	Side       string  `json:"side"`
	TradeCount int     `json:"trade_count"`
	WinRate    float64 `json:"win_rate"`
	TotalPnL   float64 `json:"total_pnl"`
	AvgPnL     float64 `json:"avg_pnl"`
}

// GetDirectionStats analyzes long vs short performance
func (s *PositionStore) GetDirectionStats(traderID string) ([]DirectionStats, error) {
	rows, err := s.db.Query(`
		SELECT
			side,
			COUNT(*) as trade_count,
			SUM(CASE WHEN realized_pnl > 0 THEN 1.0 ELSE 0.0 END) / COUNT(*) * 100 as win_rate,
			COALESCE(SUM(realized_pnl), 0) as total_pnl,
			COALESCE(AVG(realized_pnl), 0) as avg_pnl
		FROM trusted_closed_positions
		WHERE trader_id = ?
		GROUP BY side
	`, traderID)
	if err != nil {
		return nil, fmt.Errorf("failed to query direction stats: %w", err)
	}
	defer rows.Close()

	var stats []DirectionStats
	for rows.Next() {
		var s DirectionStats
		err := rows.Scan(&s.Side, &s.TradeCount, &s.WinRate, &s.TotalPnL, &s.AvgPnL)
		if err != nil {
			continue
		}
		stats = append(stats, s)
	}
	return stats, nil
}

// HistorySummary comprehensive trading history for AI context
type HistorySummary struct {
	// Overall stats
	TotalTrades    int     `json:"total_trades"`
	WinRate        float64 `json:"win_rate"`
	TotalPnL       float64 `json:"total_pnl"`
	AvgTradeReturn float64 `json:"avg_trade_return"` // Percentage

	// Best/Worst performers
	BestSymbols  []SymbolStats `json:"best_symbols"`  // Top 3 profitable
	WorstSymbols []SymbolStats `json:"worst_symbols"` // Top 3 losing

	// Direction analysis
	LongWinRate  float64 `json:"long_win_rate"`
	ShortWinRate float64 `json:"short_win_rate"`
	LongPnL      float64 `json:"long_pnl"`
	ShortPnL     float64 `json:"short_pnl"`

	// Time analysis
	AvgHoldingMins float64 `json:"avg_holding_mins"`
	BestHoldRange  string  `json:"best_hold_range"` // e.g., "1-4h"

	// Recent performance (last 20 trades)
	RecentWinRate float64 `json:"recent_win_rate"`
	RecentPnL     float64 `json:"recent_pnl"`

	// Streak info
	CurrentStreak int `json:"current_streak"` // Positive = wins, negative = losses
	MaxWinStreak  int `json:"max_win_streak"`
	MaxLoseStreak int `json:"max_lose_streak"`
}

// GetHistorySummary generates comprehensive AI context summary
func (s *PositionStore) GetHistorySummary(traderID string) (*HistorySummary, error) {
	summary := &HistorySummary{}

	// Get overall stats
	fullStats, err := s.GetFullStats(traderID)
	if err != nil {
		return nil, err
	}
	summary.TotalTrades = fullStats.TotalTrades
	summary.WinRate = fullStats.WinRate
	summary.TotalPnL = fullStats.TotalPnL
	if fullStats.TotalTrades > 0 {
		summary.AvgTradeReturn = fullStats.TotalPnL / float64(fullStats.TotalTrades)
	}

	// Get symbol stats - best performers
	symbolStats, _ := s.GetSymbolStats(traderID, 20)
	if len(symbolStats) > 0 {
		// Best 3
		for i := 0; i < len(symbolStats) && i < 3; i++ {
			if symbolStats[i].TotalPnL > 0 {
				summary.BestSymbols = append(summary.BestSymbols, symbolStats[i])
			}
		}
		// Worst 3 (from the end)
		for i := len(symbolStats) - 1; i >= 0 && len(summary.WorstSymbols) < 3; i-- {
			if symbolStats[i].TotalPnL < 0 {
				summary.WorstSymbols = append(summary.WorstSymbols, symbolStats[i])
			}
		}
	}

	// Get direction stats
	dirStats, _ := s.GetDirectionStats(traderID)
	for _, d := range dirStats {
		if d.Side == "LONG" {
			summary.LongWinRate = d.WinRate
			summary.LongPnL = d.TotalPnL
		} else if d.Side == "SHORT" {
			summary.ShortWinRate = d.WinRate
			summary.ShortPnL = d.TotalPnL
		}
	}

	// Get holding time stats
	holdStats, _ := s.GetHoldingTimeStats(traderID)
	var bestHoldWinRate float64
	for _, h := range holdStats {
		if h.WinRate > bestHoldWinRate && h.TradeCount >= 3 {
			bestHoldWinRate = h.WinRate
			summary.BestHoldRange = h.Range
		}
	}

	// Calculate average holding time
	var avgHold sql.NullFloat64
	s.db.QueryRow(`
		SELECT AVG((julianday(exit_time) - julianday(entry_time)) * 24 * 60)
		FROM trusted_closed_positions
		WHERE trader_id = ? AND exit_time IS NOT NULL
	`, traderID).Scan(&avgHold)
	if avgHold.Valid {
		summary.AvgHoldingMins = avgHold.Float64
	}

	// Get recent 20 trades performance
	var recentWins int
	var recentTotal int
	var recentPnL float64
	rows, err := s.db.Query(`
		SELECT realized_pnl FROM trusted_closed_positions
		WHERE trader_id = ?
		ORDER BY exit_time DESC LIMIT 20
	`, traderID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var pnl float64
			rows.Scan(&pnl)
			recentTotal++
			recentPnL += pnl
			if pnl > 0 {
				recentWins++
			}
		}
	}
	if recentTotal > 0 {
		summary.RecentWinRate = float64(recentWins) / float64(recentTotal) * 100
		summary.RecentPnL = recentPnL
	}

	// Calculate streaks
	s.calculateStreaks(traderID, summary)

	return summary, nil
}

// calculateStreaks calculates win/loss streaks
func (s *PositionStore) calculateStreaks(traderID string, summary *HistorySummary) {
	rows, err := s.db.Query(`
		SELECT realized_pnl FROM trusted_closed_positions
		WHERE trader_id = ?
		ORDER BY exit_time DESC
	`, traderID)
	if err != nil {
		return
	}
	defer rows.Close()

	var currentStreak, maxWin, maxLose int
	var prevWin *bool
	isFirst := true

	for rows.Next() {
		var pnl float64
		rows.Scan(&pnl)
		isWin := pnl > 0

		if isFirst {
			if isWin {
				currentStreak = 1
			} else {
				currentStreak = -1
			}
			isFirst = false
		}

		if prevWin == nil {
			prevWin = &isWin
		} else if *prevWin == isWin {
			if isWin {
				currentStreak++
				if currentStreak > maxWin {
					maxWin = currentStreak
				}
			} else {
				currentStreak--
				if -currentStreak > maxLose {
					maxLose = -currentStreak
				}
			}
		} else {
			if isWin {
				currentStreak = 1
			} else {
				currentStreak = -1
			}
			*prevWin = isWin
		}
	}

	summary.CurrentStreak = currentStreak
	summary.MaxWinStreak = maxWin
	summary.MaxLoseStreak = maxLose
}

// =============================================================================
// Deduplication and Sync Methods
// =============================================================================

// ExistsWithExchangePositionID checks if a position with the given exchange position ID already exists
// Note: Uses exchange_id (account UUID) for deduplication, not trader_id
// This ensures that the same position from an exchange account is not duplicated across different traders
func (s *PositionStore) ExistsWithExchangePositionID(exchangeID, exchangePositionID string) (bool, error) {
	if exchangePositionID == "" {
		return false, nil
	}

	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM trader_positions
		WHERE exchange_id = ? AND exchange_position_id = ?
	`, exchangeID, exchangePositionID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check position existence: %w", err)
	}
	return count > 0, nil
}

// CreateFromClosedPnL creates a closed position record from exchange closed PnL data
// This is used for syncing historical positions from exchange
// Returns true if created, false if already exists (deduped) or invalid data
func (s *PositionStore) CreateFromClosedPnL(traderID, exchangeID, exchangeType string, record *ClosedPnLRecord) (bool, error) {
	// ==========================================================================
	// Step 1: Validate required fields
	// ==========================================================================
	if record.Symbol == "" {
		return false, nil // Skip: no symbol
	}

	// Normalize and validate side
	side := strings.ToUpper(record.Side)
	if side == "LONG" || side == "BUY" {
		side = "LONG"
	} else if side == "SHORT" || side == "SELL" {
		side = "SHORT"
	} else {
		return false, nil // Skip: invalid side
	}

	// Validate quantity
	if record.Quantity <= 0 {
		return false, nil // Skip: invalid quantity
	}

	// Validate prices (entry price can be calculated, but should be positive)
	if record.ExitPrice <= 0 {
		return false, nil // Skip: invalid exit price
	}
	if record.EntryPrice <= 0 {
		return false, nil // Skip: invalid entry price
	}

	// ==========================================================================
	// Step 2: Generate unique exchange position ID for deduplication
	// ==========================================================================
	exchangePositionID := record.ExchangeID
	if exchangePositionID == "" {
		// Fallback: generate from symbol + side + exit time + pnl (to ensure uniqueness)
		exchangePositionID = fmt.Sprintf("%s_%s_%d_%.8f",
			record.Symbol, side, record.ExitTime.UnixMilli(), record.RealizedPnL)
	}

	// ==========================================================================
	// Step 3: Check for duplicates based on (exchange_id, exchange_position_id)
	// ==========================================================================
	exists, err := s.ExistsWithExchangePositionID(exchangeID, exchangePositionID)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil // Already exists, skip
	}

	// ==========================================================================
	// Step 4: Handle timestamps
	// ==========================================================================
	now := time.Now()
	exitTime := record.ExitTime
	entryTime := record.EntryTime

	// Validate exit time
	if exitTime.IsZero() || exitTime.Year() < 2000 {
		return false, nil // Skip: invalid exit time
	}

	// Handle zero entry time - use exit time as approximation
	if entryTime.IsZero() || entryTime.Year() < 2000 {
		entryTime = exitTime
	}

	// Entry time should not be after exit time
	if entryTime.After(exitTime) {
		entryTime = exitTime
	}

	// ==========================================================================
	// Step 5: Insert into database
	// ==========================================================================
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO position_close_fills
		(trader_id,exchange_id,exchange_trade_id,symbol,side,quantity,exit_price,realized_pnl,fee,fill_time,data_quality)
		VALUES(?,?,?,?,?,?,?,?,?,?, 'VERIFIED')`,
		traderID, exchangeID, exchangePositionID, record.Symbol, side, record.Quantity,
		record.ExitPrice, record.RealizedPnL, record.Fee, exitTime.UTC().Format(time.RFC3339Nano)); err != nil {
		return false, fmt.Errorf("persist authoritative closed position: %w", err)
	}
	_, err = tx.Exec(`
		INSERT INTO trader_positions (
			trader_id, exchange_id, exchange_type, exchange_position_id, symbol, side, quantity,
			entry_price, entry_order_id, entry_time,
			exit_price, exit_order_id, exit_time,
			realized_pnl, fee, leverage, status, close_reason, source, accounting_quality,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'CLOSED', ?, 'sync', 'VERIFIED', ?, ?)
	`,
		traderID, exchangeID, exchangeType, exchangePositionID, record.Symbol, side, record.Quantity,
		record.EntryPrice, "", entryTime.Format(time.RFC3339),
		record.ExitPrice, record.OrderID, exitTime.Format(time.RFC3339),
		record.RealizedPnL, record.Fee, record.Leverage, record.CloseType,
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		// Duplicate key error, treat as already exists
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return false, nil
		}
		return false, fmt.Errorf("failed to create position from closed PnL: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ClosedPnLRecord represents a closed position record from exchange (duplicated here for store package)
type ClosedPnLRecord struct {
	Symbol      string
	Side        string
	EntryPrice  float64
	ExitPrice   float64
	Quantity    float64
	RealizedPnL float64
	Fee         float64
	Leverage    int
	EntryTime   time.Time
	ExitTime    time.Time
	OrderID     string
	CloseType   string
	ExchangeID  string
}

// GetLastClosedPositionTime gets the most recent exit time from closed positions
// This is used to determine the start time for syncing new closed positions
func (s *PositionStore) GetLastClosedPositionTime(traderID string) (time.Time, error) {
	var exitTime sql.NullString
	err := s.db.QueryRow(`
		SELECT exit_time FROM trusted_closed_positions
		WHERE trader_id = ? AND exit_time IS NOT NULL
		ORDER BY exit_time DESC LIMIT 1
	`, traderID).Scan(&exitTime)

	if err == sql.ErrNoRows || !exitTime.Valid {
		// No closed positions, return 30 days ago as default
		return time.Now().Add(-30 * 24 * time.Hour), nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get last closed position time: %w", err)
	}

	t, _ := time.Parse(time.RFC3339, exitTime.String)
	return t, nil
}

// CreateOpenPosition creates an open position record with exchange position ID
func (s *PositionStore) CreateOpenPosition(pos *TraderPosition) error {
	_, err := s.CreateOpenPositionIfAbsent(pos)
	return err
}

// CreateOpenPositionIfAbsent preserves the legacy idempotent insert behavior
// while exposing whether a row was actually created. Reconciliation callers
// must not report a successful repair when a closed or differently-owned row
// already holds the exchange identity.
func (s *PositionStore) CreateOpenPositionIfAbsent(pos *TraderPosition) (bool, error) {
	// Check if already exists by exchange position ID (based on exchange_id, not trader_id)
	if pos.ExchangePositionID != "" && pos.ExchangeID != "" {
		exists, err := s.ExistsWithExchangePositionID(pos.ExchangeID, pos.ExchangePositionID)
		if err != nil {
			return false, err
		}
		if exists {
			return false, nil // Already exists, skip
		}
	}

	now := time.Now()
	pos.CreatedAt = now
	pos.UpdatedAt = now
	pos.Status = "OPEN"
	if pos.Source == "" {
		pos.Source = "system"
	}

	result, err := s.db.Exec(`
		INSERT INTO trader_positions (
			trader_id, exchange_id, exchange_type, exchange_position_id, symbol, side, quantity,
			entry_price, entry_order_id, entry_time, leverage, status, source,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		pos.TraderID, pos.ExchangeID, pos.ExchangeType, pos.ExchangePositionID, pos.Symbol, pos.Side, pos.Quantity,
		pos.EntryPrice, pos.EntryOrderID, pos.EntryTime.Format(time.RFC3339), pos.Leverage,
		pos.Status, pos.Source, now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return false, nil // Already exists
		}
		return false, fmt.Errorf("failed to create open position: %w", err)
	}

	id, _ := result.LastInsertId()
	pos.ID = id
	return true, nil
}

// ClosePositionWithAccurateData closes a position with accurate data from exchange
func (s *PositionStore) ClosePositionWithAccurateData(id int64, exitPrice float64, exitOrderID string, exitTime time.Time, realizedPnL float64, fee float64, closeReason string) error {
	now := time.Now()
	_, err := s.db.Exec(`
		UPDATE trader_positions SET
			exit_price = ?, exit_order_id = ?, exit_time = ?,
			realized_pnl = ?, fee = ?, status = 'CLOSED',
			close_reason = ?, updated_at = ?
		WHERE id = ?
	`,
		exitPrice, exitOrderID, exitTime.Format(time.RFC3339),
		realizedPnL, fee, closeReason, now.Format(time.RFC3339), id,
	)
	if err != nil {
		return fmt.Errorf("failed to close position with accurate data: %w", err)
	}
	return nil
}

// SyncClosedPositions syncs closed positions from exchange to local database
// Returns (created count, skipped count, error)
func (s *PositionStore) SyncClosedPositions(traderID, exchangeID, exchangeType string, records []ClosedPnLRecord) (int, int, error) {
	created, skipped := 0, 0
	for _, record := range records {
		rec := record // Create local copy to avoid closure issues
		side := strings.ToUpper(rec.Side)
		if side == "BUY" {
			side = "LONG"
		} else if side == "SELL" {
			side = "SHORT"
		}
		var pendingID int64
		pendingErr := s.db.QueryRow(`SELECT id FROM trader_positions
			WHERE trader_id=? AND exchange_id=? AND symbol=? AND UPPER(side)=?
			  AND status='CLOSED' AND COALESCE(accounting_quality,'') IN ('PENDING','UNSCORABLE')
			  AND datetime(entry_time)<=datetime(?)
			ORDER BY datetime(entry_time),id LIMIT 1`,
			traderID, exchangeID, rec.Symbol, side, rec.ExitTime.UTC().Format(time.RFC3339Nano)).Scan(&pendingID)
		if pendingErr != nil && pendingErr != sql.ErrNoRows {
			return created, skipped, pendingErr
		}
		if pendingErr == nil {
			tradeID := strings.TrimSpace(rec.ExchangeID)
			if tradeID == "" {
				tradeID = strings.TrimSpace(rec.OrderID)
			}
			if tradeID == "" {
				tradeID = fmt.Sprintf("%s|%s|%d|%.12g|%.12g", rec.Symbol, side, rec.ExitTime.UnixNano(), rec.Quantity, rec.ExitPrice)
			}
			claimed, reconcileErr := s.ClosePositionWithAllocations(pendingID, exchangeID, []PositionCloseFill{{
				TradeID: tradeID, Symbol: rec.Symbol, Side: side, Quantity: rec.Quantity,
				ExitPrice: rec.ExitPrice, RealizedPnL: rec.RealizedPnL, Fee: rec.Fee,
				FillTime: rec.ExitTime, DataQuality: "VERIFIED",
			}}, rec.ExitPrice, rec.CloseType)
			if reconcileErr != nil {
				return created, skipped, fmt.Errorf("reconcile delayed closed fill: %w", reconcileErr)
			}
			if claimed {
				skipped++
				continue
			}
			// The fill may have been consumed by an older compatible FIFO lot
			// rather than the provisional row selected above. In that case its
			// authoritative account record already exists and creating a new
			// synthetic CLOSED position would duplicate strategy-level PnL.
			var authoritativeExists int
			if authorityErr := s.db.QueryRow(`SELECT COUNT(*) FROM position_close_fills
				WHERE exchange_id=? AND exchange_trade_id=?`,
				exchangeID, tradeID).Scan(&authoritativeExists); authorityErr != nil {
				return created, skipped, authorityErr
			}
			if authoritativeExists > 0 {
				skipped++
				continue
			}
		}
		wasCreated, err := s.CreateFromClosedPnL(traderID, exchangeID, exchangeType, &rec)
		if err != nil {
			return created, skipped, fmt.Errorf("failed to sync position: %w", err)
		}
		if wasCreated {
			created++
		} else {
			skipped++
		}
	}
	return created, skipped, nil
}
