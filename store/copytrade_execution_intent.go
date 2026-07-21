package store

import (
	"database/sql"
	"fmt"
	"time"
)

const (
	ExecutionIntentReserved    = "RESERVED"
	ExecutionIntentSubmitted   = "SUBMITTED"
	ExecutionIntentFilled      = "FILLED"
	ExecutionIntentProtected   = "PROTECTED"
	ExecutionIntentSkipped     = "SKIPPED"
	ExecutionIntentFailed      = "FAILED"
	ExecutionIntentReconciling = "RECONCILING"
)

// CopyTradeExecutionIntent is the durable identity and acknowledgement record
// for one source-position transition. Position mappings describe the current
// relationship; intents describe the individual exchange mutation that moves
// that relationship forward.
type CopyTradeExecutionIntent struct {
	ID                int64     `json:"id"`
	TraderID          string    `json:"trader_id"`
	LeaderPosID       string    `json:"leader_pos_id"`
	SourceRevision    int64     `json:"source_revision"`
	SourceFillID      string    `json:"source_fill_id"`
	Action            string    `json:"action"`
	Symbol            string    `json:"symbol"`
	Side              string    `json:"side"`
	MarginMode        string    `json:"margin_mode"`
	LeaderTargetSize  float64   `json:"leader_target_size"`
	RequestedNotional float64   `json:"requested_notional"`
	RequestedQuantity float64   `json:"requested_quantity"`
	QuantizedQuantity float64   `json:"quantized_quantity"`
	FilledQuantity    float64   `json:"filled_quantity"`
	ClientOrderID     string    `json:"client_order_id"`
	ExchangeOrderID   string    `json:"exchange_order_id"`
	Status            string    `json:"status"`
	ReasonCode        string    `json:"reason_code"`
	LastError         string    `json:"last_error"`
	FailureCounted    bool      `json:"failure_counted"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (s *CopyTradeStore) initExecutionIntentTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_execution_intents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL,
			leader_pos_id TEXT NOT NULL,
			source_revision INTEGER NOT NULL,
			source_fill_id TEXT DEFAULT '',
			action TEXT NOT NULL,
			symbol TEXT DEFAULT '',
			side TEXT DEFAULT '',
			margin_mode TEXT DEFAULT '',
			leader_target_size REAL DEFAULT 0,
			requested_notional REAL DEFAULT 0,
			requested_quantity REAL DEFAULT 0,
			quantized_quantity REAL DEFAULT 0,
			filled_quantity REAL DEFAULT 0,
			client_order_id TEXT DEFAULT '',
			exchange_order_id TEXT DEFAULT '',
			status TEXT NOT NULL DEFAULT 'RESERVED',
			reason_code TEXT DEFAULT '',
			last_error TEXT DEFAULT '',
			failure_counted BOOLEAN NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(trader_id, leader_pos_id, source_revision, action)
		)
	`)
	if err != nil {
		return err
	}
	_, _ = s.db.Exec(`ALTER TABLE copy_trade_execution_intents ADD COLUMN failure_counted BOOLEAN NOT NULL DEFAULT 0`)
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_copy_intent_trader_status ON copy_trade_execution_intents(trader_id,status,updated_at)`)
	return err
}

// ReserveExecutionIntent atomically claims a source transition. Concurrent
// fills that observe the same mapping revision collapse to the same intent.
// FAILED intents may be reclaimed with the same client order id after a
// proven pre-submit failure; every other existing state is returned unclaimed.
func (s *CopyTradeStore) ReserveExecutionIntent(intent *CopyTradeExecutionIntent) (*CopyTradeExecutionIntent, bool, error) {
	if intent == nil || intent.TraderID == "" || intent.LeaderPosID == "" || intent.SourceRevision <= 0 || intent.Action == "" {
		return nil, false, fmt.Errorf("invalid copy trade execution intent")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`
		INSERT OR IGNORE INTO copy_trade_execution_intents
			(trader_id,leader_pos_id,source_revision,source_fill_id,action,symbol,side,margin_mode,
			 leader_target_size,requested_notional,requested_quantity,quantized_quantity,client_order_id,status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'RESERVED')
	`, intent.TraderID, intent.LeaderPosID, intent.SourceRevision, intent.SourceFillID, intent.Action,
		intent.Symbol, intent.Side, intent.MarginMode, intent.LeaderTargetSize, intent.RequestedNotional,
		intent.RequestedQuantity, intent.QuantizedQuantity, intent.ClientOrderID)
	if err != nil {
		return nil, false, err
	}
	affected, _ := res.RowsAffected()
	claimed := affected == 1
	if !claimed {
		res, err = tx.Exec(`
			UPDATE copy_trade_execution_intents SET
				source_fill_id=?,symbol=?,side=?,margin_mode=?,leader_target_size=?,requested_notional=?,
				requested_quantity=?,quantized_quantity=?,client_order_id=CASE WHEN client_order_id='' THEN ? ELSE client_order_id END,status='RESERVED',reason_code='',last_error='',updated_at=CURRENT_TIMESTAMP
			WHERE trader_id=? AND leader_pos_id=? AND source_revision=? AND action=? AND status='FAILED'
		`, intent.SourceFillID, intent.Symbol, intent.Side, intent.MarginMode, intent.LeaderTargetSize,
			intent.RequestedNotional, intent.RequestedQuantity, intent.QuantizedQuantity, intent.ClientOrderID,
			intent.TraderID, intent.LeaderPosID, intent.SourceRevision, intent.Action)
		if err != nil {
			return nil, false, err
		}
		affected, _ = res.RowsAffected()
		claimed = affected == 1
	}
	stored, err := getExecutionIntentTx(tx, intent.TraderID, intent.LeaderPosID, intent.SourceRevision, intent.Action)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return stored, claimed, nil
}

func getExecutionIntentTx(tx *sql.Tx, traderID, leaderPosID string, sourceRevision int64, action string) (*CopyTradeExecutionIntent, error) {
	row := tx.QueryRow(`SELECT id,trader_id,leader_pos_id,source_revision,COALESCE(source_fill_id,''),action,
		COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),COALESCE(leader_target_size,0),
		COALESCE(requested_notional,0),COALESCE(requested_quantity,0),COALESCE(quantized_quantity,0),
		COALESCE(filled_quantity,0),COALESCE(client_order_id,''),COALESCE(exchange_order_id,''),status,
		COALESCE(reason_code,''),COALESCE(last_error,''),COALESCE(failure_counted,0),created_at,updated_at
		FROM copy_trade_execution_intents WHERE trader_id=? AND leader_pos_id=? AND source_revision=? AND action=?`,
		traderID, leaderPosID, sourceRevision, action)
	return scanExecutionIntent(row)
}

func scanExecutionIntent(scanner interface{ Scan(...interface{}) error }) (*CopyTradeExecutionIntent, error) {
	var x CopyTradeExecutionIntent
	var created, updated string
	if err := scanner.Scan(&x.ID, &x.TraderID, &x.LeaderPosID, &x.SourceRevision, &x.SourceFillID, &x.Action,
		&x.Symbol, &x.Side, &x.MarginMode, &x.LeaderTargetSize, &x.RequestedNotional, &x.RequestedQuantity,
		&x.QuantizedQuantity, &x.FilledQuantity, &x.ClientOrderID, &x.ExchangeOrderID, &x.Status,
		&x.ReasonCode, &x.LastError, &x.FailureCounted, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if x.CreatedAt, err = parseDBTime(created); err != nil {
		return nil, err
	}
	if x.UpdatedAt, err = parseDBTime(updated); err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *CopyTradeStore) UpdateExecutionIntent(id int64, status, reasonCode, lastError, exchangeOrderID string, requestedQty, quantizedQty, filledQty float64) error {
	if id <= 0 || status == "" {
		return fmt.Errorf("invalid execution intent transition")
	}
	_, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET status=?,reason_code=?,last_error=?,
		exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,
		requested_quantity=CASE WHEN ?>0 THEN ? ELSE requested_quantity END,
		quantized_quantity=CASE WHEN ?>0 THEN ? ELSE quantized_quantity END,
		filled_quantity=CASE WHEN ?>0 THEN ? ELSE filled_quantity END,
		updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, reasonCode, lastError,
		exchangeOrderID, exchangeOrderID, requestedQty, requestedQty, quantizedQty, quantizedQty, filledQty, filledQty, id)
	return err
}

// MarkExecutionIntentFailureCounted makes the mapping circuit breaker count a
// canonical intent at most once, even if the same source transition is replayed.
func (s *CopyTradeStore) MarkExecutionIntentFailureCounted(id int64) (bool, error) {
	if id <= 0 {
		return true, nil
	}
	res, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET failure_counted=1,updated_at=CURRENT_TIMESTAMP WHERE id=? AND failure_counted=0 AND status='FAILED'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *CopyTradeStore) ListUnfinishedExecutionIntents(traderID string) ([]*CopyTradeExecutionIntent, error) {
	rows, err := s.db.Query(`SELECT id,trader_id,leader_pos_id,source_revision,COALESCE(source_fill_id,''),action,
		COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),COALESCE(leader_target_size,0),
		COALESCE(requested_notional,0),COALESCE(requested_quantity,0),COALESCE(quantized_quantity,0),
		COALESCE(filled_quantity,0),COALESCE(client_order_id,''),COALESCE(exchange_order_id,''),status,
		COALESCE(reason_code,''),COALESCE(last_error,''),COALESCE(failure_counted,0),created_at,updated_at
		FROM copy_trade_execution_intents WHERE trader_id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING') ORDER BY id`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyTradeExecutionIntent
	for rows.Next() {
		x, err := scanExecutionIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
