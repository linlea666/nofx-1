package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DecisionStore decision log storage
type DecisionStore struct {
	db *sql.DB
}

// DecisionRecord decision record
type DecisionRecord struct {
	ID                  int64              `json:"id"`
	TraderID            string             `json:"trader_id"`
	CycleNumber         int                `json:"cycle_number"`
	Timestamp           time.Time          `json:"timestamp"`
	SystemPrompt        string             `json:"system_prompt"`
	InputPrompt         string             `json:"input_prompt"`
	CoTTrace            string             `json:"cot_trace"`
	DecisionJSON        string             `json:"decision_json"`
	RawResponse         string             `json:"raw_response"` // Raw AI response for debugging
	CandidateCoins      []string           `json:"candidate_coins"`
	ExecutionLog        []string           `json:"execution_log"`
	Success             bool               `json:"success"`
	ErrorMessage        string             `json:"error_message"`
	AIRequestDurationMs int64              `json:"ai_request_duration_ms"`
	AccountState        AccountSnapshot    `json:"account_state"`
	Positions           []PositionSnapshot `json:"positions"`
	Decisions           []DecisionAction   `json:"decisions"`
}

// AccountSnapshot account state snapshot
type AccountSnapshot struct {
	TotalBalance          float64 `json:"total_balance"`
	AvailableBalance      float64 `json:"available_balance"`
	TotalUnrealizedProfit float64 `json:"total_unrealized_profit"`
	PositionCount         int     `json:"position_count"`
	MarginUsedPct         float64 `json:"margin_used_pct"`
	InitialBalance        float64 `json:"initial_balance"`
}

// PositionSnapshot position snapshot
type PositionSnapshot struct {
	Symbol           string  `json:"symbol"`
	Side             string  `json:"side"`
	PositionAmt      float64 `json:"position_amt"`
	EntryPrice       float64 `json:"entry_price"`
	MarkPrice        float64 `json:"mark_price"`
	UnrealizedProfit float64 `json:"unrealized_profit"`
	Leverage         float64 `json:"leverage"`
	LiquidationPrice float64 `json:"liquidation_price"`
}

// DecisionAction decision action
type DecisionAction struct {
	Action                    string    `json:"action"`
	Symbol                    string    `json:"symbol"`
	Quantity                  float64   `json:"quantity"`
	Leverage                  int       `json:"leverage"`
	Price                     float64   `json:"price"`
	StopLoss                  float64   `json:"stop_loss,omitempty"`   // Stop loss price
	TakeProfit                float64   `json:"take_profit,omitempty"` // Take profit price
	Confidence                int       `json:"confidence,omitempty"`  // AI confidence (0-100)
	Reasoning                 string    `json:"reasoning,omitempty"`   // Brief reasoning
	OrderID                   int64     `json:"order_id"`
	Timestamp                 time.Time `json:"timestamp"`
	Success                   bool      `json:"success"`
	Error                     string    `json:"error"`
	ExecutionIntentID         int64     `json:"execution_intent_id,omitempty"`
	SourceFillID              string    `json:"source_fill_id,omitempty"`
	LeaderPosID               string    `json:"leader_pos_id,omitempty"`
	SourceRevision            int64     `json:"source_revision,omitempty"`
	RequestedQuantity         float64   `json:"requested_quantity,omitempty"`
	QuantizedQuantity         float64   `json:"quantized_quantity,omitempty"`
	FilledQuantity            float64   `json:"filled_quantity,omitempty"`
	QuantityStep              float64   `json:"quantity_step,omitempty"`
	ExchangeMinQuantity       float64   `json:"exchange_min_quantity,omitempty"`
	ExchangeMinNotional       float64   `json:"exchange_min_notional,omitempty"`
	MinimumExecutableQuantity float64   `json:"minimum_executable_quantity,omitempty"`
	ExchangeOrderID           string    `json:"exchange_order_id,omitempty"`
	ClientOrderID             string    `json:"client_order_id,omitempty"`
	ExchangeOrderState        string    `json:"exchange_order_state,omitempty"`
	ExecutionStatus           string    `json:"execution_status,omitempty"`
	ExecutionReasonCode       string    `json:"execution_reason_code,omitempty"`
	ProtectionStatus          string    `json:"protection_status,omitempty"`
	ProtectionCoverage        float64   `json:"protection_coverage,omitempty"`
	CopyGuardCycleStatus      string    `json:"copy_guard_cycle_status,omitempty"`
}

// Statistics statistics information
type Statistics struct {
	TotalCycles         int `json:"total_cycles"`
	SuccessfulCycles    int `json:"successful_cycles"`
	FailedCycles        int `json:"failed_cycles"`
	TotalOpenPositions  int `json:"total_open_positions"`
	TotalClosePositions int `json:"total_close_positions"`
}

// initTables initializes AI decision log tables
// Note: Account equity curve data has been migrated to trader_equity_snapshots table (managed by EquityStore)
func (s *DecisionStore) initTables() error {
	queries := []string{
		// AI decision log table (records AI input/output, chain of thought, etc.)
		`CREATE TABLE IF NOT EXISTS decision_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL,
			cycle_number INTEGER NOT NULL,
			timestamp DATETIME NOT NULL,
			system_prompt TEXT DEFAULT '',
			input_prompt TEXT DEFAULT '',
			cot_trace TEXT DEFAULT '',
			decision_json TEXT DEFAULT '',
			raw_response TEXT DEFAULT '',
			candidate_coins TEXT DEFAULT '',
			execution_log TEXT DEFAULT '',
			success BOOLEAN DEFAULT 0,
			error_message TEXT DEFAULT '',
			ai_request_duration_ms INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		// Indexes
		`CREATE INDEX IF NOT EXISTS idx_decision_records_trader_time ON decision_records(trader_id, timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_decision_records_timestamp ON decision_records(timestamp DESC)`,
		// GetLastCycleNumber runs MAX(cycle_number) per trader during startup;
		// without this index it scans every (large JSON) row of the trader on a
		// multi-GB database, which stalled trader auto-start for minutes.
		`CREATE INDEX IF NOT EXISTS idx_decision_records_trader_cycle ON decision_records(trader_id, cycle_number)`,
	}

	for _, query := range queries {
		if _, err := s.db.Exec(query); err != nil {
			return fmt.Errorf("failed to execute SQL: %w", err)
		}
	}

	for _, migration := range []struct {
		column, definition string
	}{
		{"raw_response", "TEXT DEFAULT ''"},
		{"decisions", "TEXT DEFAULT '[]'"},
	} {
		if err := ensureSQLiteColumn(s.db, "decision_records", migration.column, migration.definition); err != nil {
			return fmt.Errorf("migrate decision_records.%s: %w", migration.column, err)
		}
	}

	return nil
}

// LogDecision logs decision (only saves AI decision log, equity curve has been migrated to equity table)
func (s *DecisionStore) LogDecision(record *DecisionRecord) error {
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	} else {
		record.Timestamp = record.Timestamp.UTC()
	}

	// Serialize candidate coins, execution log and decisions to JSON
	candidateCoinsJSON, _ := json.Marshal(record.CandidateCoins)
	executionLogJSON, _ := json.Marshal(record.ExecutionLog)
	decisionsJSON, _ := json.Marshal(record.Decisions)

	// Insert decision record main table (only save AI decision related content)
	result, err := s.db.Exec(`
		INSERT INTO decision_records (
			trader_id, cycle_number, timestamp, system_prompt, input_prompt,
			cot_trace, decision_json, raw_response, candidate_coins, execution_log,
			decisions, success, error_message, ai_request_duration_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		record.TraderID, record.CycleNumber, record.Timestamp.Format(time.RFC3339),
		record.SystemPrompt, record.InputPrompt, record.CoTTrace, record.DecisionJSON,
		record.RawResponse, string(candidateCoinsJSON), string(executionLogJSON),
		string(decisionsJSON), record.Success, record.ErrorMessage, record.AIRequestDurationMs,
	)
	if err != nil {
		return fmt.Errorf("failed to insert decision record: %w", err)
	}

	decisionID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get decision ID: %w", err)
	}
	record.ID = decisionID

	return nil
}

// GetLatestRecords gets the latest N records for specified trader (sorted by time in ascending order: old to new)
func (s *DecisionStore) GetLatestRecords(traderID string, n int) ([]*DecisionRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, trader_id, cycle_number, timestamp, system_prompt, input_prompt,
			   cot_trace, decision_json, candidate_coins, execution_log,
			   COALESCE(decisions, '[]'), success, error_message, ai_request_duration_ms
		FROM decision_records
		WHERE trader_id = ?
		ORDER BY timestamp DESC
		LIMIT ?
	`, traderID, n)
	if err != nil {
		return nil, fmt.Errorf("failed to query decision records: %w", err)
	}
	defer rows.Close()

	var records []*DecisionRecord
	for rows.Next() {
		record, err := s.scanDecisionRecord(rows)
		if err != nil {
			continue
		}
		records = append(records, record)
	}

	// Fill associated data
	for _, record := range records {
		s.fillRecordDetails(record)
	}

	// Reverse array to sort time from old to new
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}

	return records, nil
}

// GetAllLatestRecords gets the latest N records for all traders
func (s *DecisionStore) GetAllLatestRecords(n int) ([]*DecisionRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, trader_id, cycle_number, timestamp, system_prompt, input_prompt,
			   cot_trace, decision_json, candidate_coins, execution_log,
			   COALESCE(decisions, '[]'), success, error_message, ai_request_duration_ms
		FROM decision_records
		ORDER BY timestamp DESC
		LIMIT ?
	`, n)
	if err != nil {
		return nil, fmt.Errorf("failed to query decision records: %w", err)
	}
	defer rows.Close()

	var records []*DecisionRecord
	for rows.Next() {
		record, err := s.scanDecisionRecord(rows)
		if err != nil {
			continue
		}
		records = append(records, record)
	}

	// Reverse array
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}

	return records, nil
}

// GetRecordsByDate gets all records for a specified trader on a specified date
func (s *DecisionStore) GetRecordsByDate(traderID string, date time.Time) ([]*DecisionRecord, error) {
	dateStr := date.Format("2006-01-02")

	rows, err := s.db.Query(`
		SELECT id, trader_id, cycle_number, timestamp, system_prompt, input_prompt,
			   cot_trace, decision_json, candidate_coins, execution_log,
			   COALESCE(decisions, '[]'), success, error_message, ai_request_duration_ms
		FROM decision_records
		WHERE trader_id = ? AND DATE(timestamp) = ?
		ORDER BY timestamp ASC
	`, traderID, dateStr)
	if err != nil {
		return nil, fmt.Errorf("failed to query decision records: %w", err)
	}
	defer rows.Close()

	var records []*DecisionRecord
	for rows.Next() {
		record, err := s.scanDecisionRecord(rows)
		if err != nil {
			continue
		}
		records = append(records, record)
	}

	return records, nil
}

// CleanOldRecords cleans old records from N days ago
func (s *DecisionStore) CleanOldRecords(traderID string, days int) (int64, error) {
	cutoffTime := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)

	result, err := s.db.Exec(`
		DELETE FROM decision_records
		WHERE trader_id = ? AND timestamp < ?
	`, traderID, cutoffTime)
	if err != nil {
		return 0, fmt.Errorf("failed to clean old records: %w", err)
	}

	return result.RowsAffected()
}

// GetStatistics gets statistics information for specified trader
func (s *DecisionStore) GetStatistics(traderID string) (*Statistics, error) {
	stats := &Statistics{}

	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM decision_records WHERE trader_id = ?
	`, traderID).Scan(&stats.TotalCycles)
	if err != nil {
		return nil, fmt.Errorf("failed to query total cycles: %w", err)
	}

	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM decision_records WHERE trader_id = ? AND success = 1
	`, traderID).Scan(&stats.SuccessfulCycles)
	if err != nil {
		return nil, fmt.Errorf("failed to query successful cycles: %w", err)
	}
	stats.FailedCycles = stats.TotalCycles - stats.SuccessfulCycles

	// Count from trader_positions table
	s.db.QueryRow(`
		SELECT COUNT(*) FROM trader_positions
		WHERE trader_id = ?
	`, traderID).Scan(&stats.TotalOpenPositions)

	s.db.QueryRow(`
		SELECT COUNT(*) FROM trader_positions
		WHERE trader_id = ? AND status = 'CLOSED'
	`, traderID).Scan(&stats.TotalClosePositions)

	return stats, nil
}

// GetAllStatistics gets statistics information for all traders
func (s *DecisionStore) GetAllStatistics() (*Statistics, error) {
	stats := &Statistics{}

	s.db.QueryRow(`SELECT COUNT(*) FROM decision_records`).Scan(&stats.TotalCycles)
	s.db.QueryRow(`SELECT COUNT(*) FROM decision_records WHERE success = 1`).Scan(&stats.SuccessfulCycles)
	stats.FailedCycles = stats.TotalCycles - stats.SuccessfulCycles

	// Count from trader_positions table
	s.db.QueryRow(`
		SELECT COUNT(*) FROM trader_positions
	`).Scan(&stats.TotalOpenPositions)

	s.db.QueryRow(`
		SELECT COUNT(*) FROM trader_positions
		WHERE status = 'CLOSED'
	`).Scan(&stats.TotalClosePositions)

	return stats, nil
}

// GetLastCycleNumber gets the last cycle number for specified trader
func (s *DecisionStore) GetLastCycleNumber(traderID string) (int, error) {
	var cycleNumber int
	err := s.db.QueryRow(`
		SELECT COALESCE(MAX(cycle_number), 0) FROM decision_records WHERE trader_id = ?
	`, traderID).Scan(&cycleNumber)
	if err != nil {
		return 0, err
	}
	return cycleNumber, nil
}

// scanDecisionRecord scans decision record from row
func (s *DecisionStore) scanDecisionRecord(rows *sql.Rows) (*DecisionRecord, error) {
	var record DecisionRecord
	var timestampStr string
	var candidateCoinsJSON, executionLogJSON, decisionsJSON string

	err := rows.Scan(
		&record.ID, &record.TraderID, &record.CycleNumber, &timestampStr,
		&record.SystemPrompt, &record.InputPrompt, &record.CoTTrace,
		&record.DecisionJSON, &candidateCoinsJSON, &executionLogJSON,
		&decisionsJSON, &record.Success, &record.ErrorMessage, &record.AIRequestDurationMs,
	)
	if err != nil {
		return nil, err
	}

	record.Timestamp, _ = time.Parse(time.RFC3339, timestampStr)
	json.Unmarshal([]byte(candidateCoinsJSON), &record.CandidateCoins)
	json.Unmarshal([]byte(executionLogJSON), &record.ExecutionLog)
	json.Unmarshal([]byte(decisionsJSON), &record.Decisions)

	return &record, nil
}

// fillRecordDetails overlays the durable execution-intent terminal state on
// the immutable decision payload. Protection/reconciliation may finish after
// the decision row is inserted; without this overlay the dashboard can keep a
// stale green "open succeeded" card after a later forced exit.
func (s *DecisionStore) fillRecordDetails(record *DecisionRecord) {
	if record == nil || len(record.Decisions) == 0 {
		return
	}
	overall := true
	errors := make([]string, 0)
	for i := range record.Decisions {
		action := &record.Decisions[i]
		if action.ExecutionIntentID <= 0 {
			if !action.Success {
				overall = false
			}
			continue
		}
		var status, reason, lastError, exchangeOrderID, exchangeOrderState string
		var requested, quantized, filled, quantityStep, exchangeMinQuantity, exchangeMinNotional, minimumExecutableQuantity float64
		err := s.db.QueryRow(`SELECT status,COALESCE(reason_code,''),COALESCE(last_error,''),COALESCE(exchange_order_id,''),COALESCE(exchange_state,''),COALESCE(requested_quantity,0),COALESCE(quantized_quantity,0),COALESCE(filled_quantity,0),COALESCE(quantity_step,0),COALESCE(exchange_min_quantity,0),COALESCE(exchange_min_notional,0),COALESCE(minimum_executable_quantity,0) FROM copy_trade_execution_intents WHERE id=?`, action.ExecutionIntentID).
			Scan(&status, &reason, &lastError, &exchangeOrderID, &exchangeOrderState, &requested, &quantized, &filled, &quantityStep, &exchangeMinQuantity, &exchangeMinNotional, &minimumExecutableQuantity)
		if err != nil {
			overall = false
			continue
		}
		action.ExecutionStatus = status
		action.ExecutionReasonCode = reason
		action.ExchangeOrderID = exchangeOrderID
		action.ExchangeOrderState = exchangeOrderState
		action.RequestedQuantity = requested
		action.QuantizedQuantity = quantized
		action.FilledQuantity = filled
		action.QuantityStep = quantityStep
		action.ExchangeMinQuantity = exchangeMinQuantity
		action.ExchangeMinNotional = exchangeMinNotional
		action.MinimumExecutableQuantity = minimumExecutableQuantity
		action.Success = status == ExecutionIntentCompletedPartial ||
			status == ExecutionIntentFilled || status == ExecutionIntentProtected
		terminalWithoutExecution := status == ExecutionIntentSkipped
		if !action.Success && !terminalWithoutExecution {
			overall = false
			if lastError != "" {
				action.Error = lastError
				errors = append(errors, lastError)
			}
		}
		if action.LeaderPosID != "" {
			_ = s.db.QueryRow(`SELECT COALESCE(status,''),COALESCE(protection_status,''),COALESCE(protection_coverage,0)
				FROM copy_guard_cycles WHERE trader_id=? AND leader_pos_id=? ORDER BY id DESC LIMIT 1`,
				record.TraderID, action.LeaderPosID).
				Scan(&action.CopyGuardCycleStatus, &action.ProtectionStatus, &action.ProtectionCoverage)
		}
	}
	record.Success = overall
	if len(errors) > 0 {
		record.ErrorMessage = strings.Join(errors, "; ")
	}
}
