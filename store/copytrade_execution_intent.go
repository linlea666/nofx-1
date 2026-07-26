package store

import (
	"database/sql"
	"fmt"
	"strings"
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

	ExecutionOrderAttemptPrepared  = "PREPARED"
	ExecutionOrderAttemptSubmitted = "SUBMITTED"
	ExecutionOrderAttemptFilled    = "FILLED"
	ExecutionOrderAttemptFailed    = "FAILED"
	ExecutionOrderAttemptUnknown   = "UNKNOWN"
)

type CopyTradeExecutionIntentSource struct {
	ID           int64     `json:"id"`
	IntentID     int64     `json:"intent_id"`
	SourceFillID string    `json:"source_fill_id"`
	CreatedAt    time.Time `json:"created_at"`
}

type CopyTradeExecutionOrderAttempt struct {
	ID                int64      `json:"id"`
	IntentID          int64      `json:"intent_id"`
	AttemptNo         int        `json:"attempt_no"`
	ClientOrderID     string     `json:"client_order_id"`
	RequestedQuantity float64    `json:"requested_quantity"`
	QuantizedQuantity float64    `json:"quantized_quantity"`
	FilledQuantity    float64    `json:"filled_quantity"`
	ExchangeOrderID   string     `json:"exchange_order_id"`
	ExchangeState     string     `json:"exchange_state"`
	Status            string     `json:"status"`
	LastError         string     `json:"last_error"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	SubmittedAt       *time.Time `json:"submitted_at,omitempty"`
	FilledAt          *time.Time `json:"filled_at,omitempty"`
	TerminalAt        *time.Time `json:"terminal_at,omitempty"`
}

// CopyTradeExecutionIntent is the durable identity and acknowledgement record
// for one source-position transition. Position mappings describe the current
// relationship; intents describe the individual exchange mutation that moves
// that relationship forward.
type CopyTradeExecutionIntent struct {
	ID                        int64      `json:"id"`
	TraderID                  string     `json:"trader_id"`
	LeaderPosID               string     `json:"leader_pos_id"`
	SourceRevision            int64      `json:"source_revision"`
	SourceFillID              string     `json:"source_fill_id"`
	SourceKind                string     `json:"source_kind"`
	CanonicalKey              string     `json:"canonical_key"`
	CycleID                   int64      `json:"cycle_id,omitempty"`
	CandidateID               int64      `json:"candidate_id,omitempty"`
	AnalysisID                int64      `json:"analysis_id,omitempty"`
	AttemptNo                 int        `json:"attempt_no,omitempty"`
	DecisionGeneration        int        `json:"decision_generation,omitempty"`
	Action                    string     `json:"action"`
	Symbol                    string     `json:"symbol"`
	Side                      string     `json:"side"`
	MarginMode                string     `json:"margin_mode"`
	LeaderTargetSize          float64    `json:"leader_target_size"`
	RequestedNotional         float64    `json:"requested_notional"`
	RequestedQuantity         float64    `json:"requested_quantity"`
	QuantizedQuantity         float64    `json:"quantized_quantity"`
	QuantityStep              float64    `json:"quantity_step"`
	ExchangeMinQuantity       float64    `json:"exchange_min_quantity"`
	ExchangeMinNotional       float64    `json:"exchange_min_notional"`
	MinimumExecutableQuantity float64    `json:"minimum_executable_quantity"`
	FilledQuantity            float64    `json:"filled_quantity"`
	ClientOrderID             string     `json:"client_order_id"`
	ExchangeOrderID           string     `json:"exchange_order_id"`
	ExchangeState             string     `json:"exchange_state"`
	Status                    string     `json:"status"`
	ReasonCode                string     `json:"reason_code"`
	LastError                 string     `json:"last_error"`
	FailureCounted            bool       `json:"failure_counted"`
	CreatedAt                 time.Time  `json:"created_at"`
	UpdatedAt                 time.Time  `json:"updated_at"`
	SubmittedAt               *time.Time `json:"submitted_at,omitempty"`
	FilledAt                  *time.Time `json:"filled_at,omitempty"`
	ProtectedAt               *time.Time `json:"protected_at,omitempty"`
	TerminalAt                *time.Time `json:"terminal_at,omitempty"`
	SourceFillIDs             []string   `json:"source_fill_ids,omitempty"`
}

type LeaderExecutionCommit struct {
	IntentID             int64
	TraderID             string
	LeaderID             string
	LeaderPosID          string
	SourceRevision       int64
	Action               string
	IsAdd                bool
	Symbol               string
	Side                 string
	MarginMode           string
	SourceSymbol         string
	ExecutionSymbol      string
	SourceQuoteAsset     string
	ExecutionSettleAsset string
	LeaderTargetSize     float64
	FillPrice            float64
	FilledQuantity       float64
	FilledNotional       float64
	ExchangeOrderID      string
	ExchangeState        string
}

// CommitLeaderExecutionFill is the single acknowledgement boundary for a
// leader-driven mutation. The durable intent and mapping revision advance in
// one SQLite transaction only after the exchange provides a real fill.
func (s *CopyTradeStore) CommitLeaderExecutionFill(c LeaderExecutionCommit) error {
	if c.IntentID <= 0 || c.TraderID == "" || c.LeaderPosID == "" || c.SourceRevision <= 0 || c.FilledQuantity <= 0 {
		return fmt.Errorf("invalid acknowledged leader execution commit")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var intentStatus string
	if err = tx.QueryRow(`SELECT status FROM copy_trade_execution_intents WHERE id=? AND trader_id=? AND leader_pos_id=? AND source_revision=?`, c.IntentID, c.TraderID, c.LeaderPosID, c.SourceRevision).Scan(&intentStatus); err != nil {
		return err
	}
	if intentStatus != ExecutionIntentFilled && intentStatus != ExecutionIntentProtected &&
		intentStatus != ExecutionIntentSubmitted && intentStatus != ExecutionIntentReconciling && intentStatus != ExecutionIntentReserved {
		return fmt.Errorf("intent %d cannot commit fill from %s", c.IntentID, intentStatus)
	}
	open := c.Action == "open_long" || c.Action == "open_short"
	reduce := c.Action == "reduce_long" || c.Action == "reduce_short"
	closeAction := c.Action == "close_long" || c.Action == "close_short"
	var currentRevision int64
	var mappingStatus, mappingSide string
	err = tx.QueryRow(`SELECT COALESCE(source_revision,0),status,side FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, c.TraderID, c.LeaderPosID).Scan(&currentRevision, &mappingStatus, &mappingSide)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && currentRevision == c.SourceRevision {
		stateMatches := (open && mappingStatus == MappingStatusActive && strings.EqualFold(mappingSide, c.Side)) ||
			(reduce && mappingStatus == MappingStatusActive) ||
			(closeAction && mappingStatus == MappingStatusClosed)
		if !stateMatches {
			return fmt.Errorf("mapping revision %d has incompatible state %s/%s for %s", currentRevision, mappingStatus, mappingSide, c.Action)
		}
		if intentStatus == ExecutionIntentProtected {
			return tx.Commit()
		}
		_, err = tx.Exec(`UPDATE copy_trade_execution_intents SET status='FILLED',filled_quantity=?,exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,exchange_state=?,filled_at=COALESCE(filled_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE id=?`, c.FilledQuantity, c.ExchangeOrderID, c.ExchangeOrderID, c.ExchangeState, c.IntentID)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='FILLED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, c.IntentID); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err == nil && currentRevision > c.SourceRevision {
		return fmt.Errorf("stale execution intent %d: mapping revision=%d intent revision=%d", c.IntentID, currentRevision, c.SourceRevision)
	}
	if intentStatus == ExecutionIntentFilled || intentStatus == ExecutionIntentProtected {
		return fmt.Errorf("intent %d is %s but mapping acknowledgement is missing", c.IntentID, intentStatus)
	}
	if currentRevision != c.SourceRevision-1 {
		return fmt.Errorf("mapping revision conflict for %s: current=%d expected=%d", c.LeaderPosID, currentRevision, c.SourceRevision-1)
	}
	switch {
	case open && err == sql.ErrNoRows:
		_, err = tx.Exec(`INSERT INTO copy_trade_position_mappings
			(trader_id,leader_pos_id,leader_id,symbol,source_symbol,execution_symbol,source_quote_asset,execution_settle_asset,source_revision,side,margin_mode,status,opened_at,open_price,open_size_usd,last_known_size,add_count,reduce_count,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,'active',CURRENT_TIMESTAMP,?,?,?,0,0,CURRENT_TIMESTAMP)`,
			c.TraderID, c.LeaderPosID, c.LeaderID, c.Symbol, c.SourceSymbol, c.ExecutionSymbol, c.SourceQuoteAsset, c.ExecutionSettleAsset, c.SourceRevision, c.Side, c.MarginMode, c.FillPrice, c.FilledNotional, c.LeaderTargetSize)
	case open && (mappingStatus == MappingStatusClosed || mappingStatus == MappingStatusIgnored):
		_, err = tx.Exec(`UPDATE copy_trade_position_mappings SET leader_id=?,symbol=?,source_symbol=?,execution_symbol=?,source_quote_asset=?,execution_settle_asset=?,source_revision=?,side=?,margin_mode=?,status='active',opened_at=CURRENT_TIMESTAMP,closed_at=NULL,close_price=0,open_price=?,open_size_usd=?,last_known_size=?,add_count=0,reduce_count=0,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=?`,
			c.LeaderID, c.Symbol, c.SourceSymbol, c.ExecutionSymbol, c.SourceQuoteAsset, c.ExecutionSettleAsset, c.SourceRevision, c.Side, c.MarginMode, c.FillPrice, c.FilledNotional, c.LeaderTargetSize, c.TraderID, c.LeaderPosID)
	case open && mappingStatus == MappingStatusActive && strings.EqualFold(mappingSide, c.Side):
		addIncrement := 0
		if c.IsAdd {
			addIncrement = 1
		}
		_, err = tx.Exec(`UPDATE copy_trade_position_mappings SET source_revision=?,last_known_size=?,add_count=add_count+?,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='active'`, c.SourceRevision, c.LeaderTargetSize, addIncrement, c.TraderID, c.LeaderPosID)
	case reduce && mappingStatus == MappingStatusActive:
		_, err = tx.Exec(`UPDATE copy_trade_position_mappings SET source_revision=?,last_known_size=?,reduce_count=reduce_count+1,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='active'`, c.SourceRevision, c.LeaderTargetSize, c.TraderID, c.LeaderPosID)
	case closeAction && mappingStatus == MappingStatusActive:
		_, err = tx.Exec(`UPDATE copy_trade_position_mappings SET source_revision=?,status='closed',closed_at=CURRENT_TIMESTAMP,close_price=?,last_known_size=0,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='active'`, c.SourceRevision, c.FillPrice, c.TraderID, c.LeaderPosID)
	default:
		return fmt.Errorf("mapping state %s/%s cannot apply %s revision %d", mappingStatus, mappingSide, c.Action, c.SourceRevision)
	}
	if err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE copy_trade_execution_intents SET status='FILLED',filled_quantity=?,exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,exchange_state=?,filled_at=COALESCE(filled_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING')`, c.FilledQuantity, c.ExchangeOrderID, c.ExchangeOrderID, c.ExchangeState, c.IntentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("intent %d fill commit lost state race", c.IntentID)
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='FILLED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, c.IntentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CopyTradeStore) CommitSkippedSubLot(intentID int64, traderID, leaderPosID string, sourceRevision int64, leaderTargetSize float64) error {
	return s.CommitSkippedLeaderTransition(intentID, traderID, leaderPosID, sourceRevision, leaderTargetSize, "SKIPPED_SUBLOT")
}

type IgnoredLeaderTransition struct {
	IntentID         int64
	TraderID         string
	LeaderID         string
	LeaderPosID      string
	SourceRevision   int64
	Symbol           string
	Side             string
	MarginMode       string
	LeaderTargetSize float64
	ReasonCode       string
}

type ExecutionReconciliationReport struct {
	UnfinishedIntents          int `json:"unfinished_intents"`
	OpenCyclesWithoutMapping   int `json:"open_cycles_without_mapping"`
	ActiveMappingsWithoutCycle int `json:"active_mappings_without_cycle"`
	UnprotectedOpenCycles      int `json:"unprotected_open_cycles"`
	PendingIntentRisk          int `json:"pending_intent_risk"`
}

// GetExecutionReconciliationReport is intentionally read-only. Startup uses it
// after order reconciliation to expose lifecycle gaps before source polling;
// it never infers a fill or places a compensating order.
func (s *CopyTradeStore) GetExecutionReconciliationReport(traderID string) (*ExecutionReconciliationReport, error) {
	report := &ExecutionReconciliationReport{}
	queries := []struct {
		target *int
		sql    string
	}{
		{&report.UnfinishedIntents, `SELECT COUNT(*) FROM copy_trade_execution_intents WHERE trader_id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING')`},
		{&report.OpenCyclesWithoutMapping, `SELECT COUNT(*) FROM copy_guard_cycles c WHERE c.trader_id=? AND c.closed_at IS NULL AND NOT EXISTS (SELECT 1 FROM copy_trade_position_mappings m WHERE m.trader_id=c.trader_id AND m.leader_pos_id=c.leader_pos_id AND m.status IN ('active','stopped_by_risk','detached'))`},
		{&report.ActiveMappingsWithoutCycle, `SELECT COUNT(*) FROM copy_trade_position_mappings m WHERE m.trader_id=? AND m.status='active' AND NOT EXISTS (SELECT 1 FROM copy_guard_cycles c WHERE c.trader_id=m.trader_id AND c.leader_pos_id=m.leader_pos_id AND c.closed_at IS NULL)`},
		{&report.UnprotectedOpenCycles, `SELECT COUNT(*) FROM copy_guard_cycles WHERE trader_id=? AND closed_at IS NULL AND status IN ('FOLLOWING','FOLLOWING_REENTRY') AND (protection_status NOT IN ('VERIFIED','CLAMPED') OR protection_coverage<0.999)`},
		{&report.PendingIntentRisk, `SELECT COUNT(*) FROM copy_guard_risk_reservations WHERE trader_id=? AND intent_id>0 AND status='ACTIVE'`},
	}
	for _, query := range queries {
		if err := s.db.QueryRow(query.sql, traderID).Scan(query.target); err != nil {
			return report, err
		}
	}
	return report, nil
}

// CommitIgnoredLeaderTransition is the no-fill acknowledgement boundary for a
// new leader position that deterministic execution policy rejects. Creating or
// reopening the ignored mapping and terminating the reserved intent must be one
// transaction; otherwise a crash between the old two calls causes the source
// revision to be replayed under a different canonical intent.
func (s *CopyTradeStore) CommitIgnoredLeaderTransition(c IgnoredLeaderTransition) error {
	if c.IntentID <= 0 || c.TraderID == "" || c.LeaderPosID == "" || c.SourceRevision <= 0 || c.LeaderTargetSize < 0 {
		return fmt.Errorf("invalid ignored leader transition")
	}
	if strings.TrimSpace(c.ReasonCode) == "" {
		c.ReasonCode = "SKIPPED"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRevision int64
	var mappingStatus string
	mappingErr := tx.QueryRow(`SELECT COALESCE(source_revision,0),status FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, c.TraderID, c.LeaderPosID).Scan(&currentRevision, &mappingStatus)
	switch {
	case mappingErr == sql.ErrNoRows && c.SourceRevision == 1:
		_, err = tx.Exec(`INSERT INTO copy_trade_position_mappings
			(trader_id,leader_pos_id,leader_id,symbol,source_revision,side,margin_mode,status,opened_at,open_price,open_size_usd,last_known_size,add_count,reduce_count,updated_at)
			VALUES(?,?,?,?,?,?,?,'ignored',CURRENT_TIMESTAMP,0,0,?,0,0,CURRENT_TIMESTAMP)`,
			c.TraderID, c.LeaderPosID, c.LeaderID, c.Symbol, c.SourceRevision, c.Side, c.MarginMode, c.LeaderTargetSize)
	case mappingErr == nil && mappingStatus == MappingStatusClosed && currentRevision == c.SourceRevision-1:
		_, err = tx.Exec(`UPDATE copy_trade_position_mappings SET leader_id=?,symbol=?,source_revision=?,side=?,margin_mode=?,status='ignored',opened_at=CURRENT_TIMESTAMP,closed_at=NULL,close_price=0,open_price=0,open_size_usd=0,last_known_size=?,add_count=0,reduce_count=0,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='closed' AND COALESCE(source_revision,0)=?`,
			c.LeaderID, c.Symbol, c.SourceRevision, c.Side, c.MarginMode, c.LeaderTargetSize, c.TraderID, c.LeaderPosID, c.SourceRevision-1)
	case mappingErr == nil && mappingStatus == MappingStatusIgnored && currentRevision == c.SourceRevision:
		// Idempotent retry after the mapping commit but before the caller saw it.
	case mappingErr != nil:
		return mappingErr
	default:
		return fmt.Errorf("ignored mapping revision conflict: status=%s current=%d expected=%d", mappingStatus, currentRevision, c.SourceRevision-1)
	}
	if err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE copy_trade_execution_intents SET status='SKIPPED',reason_code=?,last_error='',terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE id=? AND trader_id=? AND leader_pos_id=? AND source_revision=? AND status IN ('RESERVED','SUBMITTED','RECONCILING')`,
		c.ReasonCode, c.IntentID, c.TraderID, c.LeaderPosID, c.SourceRevision)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		var status string
		if scanErr := tx.QueryRow(`SELECT status FROM copy_trade_execution_intents WHERE id=?`, c.IntentID).Scan(&status); scanErr != nil || status != ExecutionIntentSkipped {
			return fmt.Errorf("ignored intent %d lost state race", c.IntentID)
		}
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='SKIPPED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, c.IntentID); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitSkippedLeaderTransition acknowledges a source revision that correctly
// produced no exchange order (for example a sub-lot add/reduction). It advances
// source state without inventing a fill,
// add/reduce counter, or accumulated reduction.
func (s *CopyTradeStore) CommitSkippedLeaderTransition(intentID int64, traderID, leaderPosID string, sourceRevision int64, leaderTargetSize float64, reasonCode string) error {
	if intentID <= 0 || traderID == "" || leaderPosID == "" || sourceRevision <= 0 || leaderTargetSize < 0 {
		return fmt.Errorf("invalid skipped leader transition")
	}
	if strings.TrimSpace(reasonCode) == "" {
		reasonCode = "SKIPPED"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE copy_trade_position_mappings SET last_known_size=?,source_revision=?,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='active' AND COALESCE(source_revision,0)=?`, leaderTargetSize, sourceRevision, traderID, leaderPosID, sourceRevision-1)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		var current int64
		if scanErr := tx.QueryRow(`SELECT COALESCE(source_revision,0) FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, leaderPosID).Scan(&current); scanErr != nil || current != sourceRevision {
			return fmt.Errorf("skipped sub-lot mapping revision conflict: current=%d expected=%d", current, sourceRevision-1)
		}
	}
	res, err = tx.Exec(`UPDATE copy_trade_execution_intents SET status='SKIPPED',reason_code=?,last_error='',terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING')`, reasonCode, intentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		var status string
		if scanErr := tx.QueryRow(`SELECT status FROM copy_trade_execution_intents WHERE id=?`, intentID).Scan(&status); scanErr != nil || status != ExecutionIntentSkipped {
			return fmt.Errorf("skipped sub-lot intent %d lost state race", intentID)
		}
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='SKIPPED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, intentID); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitDetachedLeaderTransition atomically acknowledges a source reduction
// after a fresh exchange read proves that the mapped follower position is
// absent. This is intentionally distinct from a protective stop: the mapping
// remains source-tracked, the Copy Guard cycle is made unscorable, and no AI
// reentry candidate may be produced.
func (s *CopyTradeStore) CommitDetachedLeaderTransition(intentID int64, traderID, leaderPosID string, sourceRevision int64, leaderTargetSize float64, reasonCode string) error {
	if intentID <= 0 || traderID == "" || leaderPosID == "" || sourceRevision <= 0 || leaderTargetSize < 0 {
		return fmt.Errorf("invalid detached leader transition")
	}
	if strings.TrimSpace(reasonCode) == "" {
		reasonCode = "FOLLOWER_POSITION_MISSING"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE copy_trade_position_mappings
		SET status='detached',last_known_size=?,source_revision=?,last_failure_reason=?,
		    consecutive_fail_count=0,updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND leader_pos_id=? AND status='active' AND COALESCE(source_revision,0)=?`,
		leaderTargetSize, sourceRevision, reasonCode, traderID, leaderPosID, sourceRevision-1)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		var current int64
		var status string
		if scanErr := tx.QueryRow(`SELECT COALESCE(source_revision,0),status FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, traderID, leaderPosID).Scan(&current, &status); scanErr != nil || current != sourceRevision || status != MappingStatusDetached {
			return fmt.Errorf("detached mapping revision conflict: status=%s current=%d expected=%d", status, current, sourceRevision-1)
		}
	}
	res, err = tx.Exec(`UPDATE copy_trade_execution_intents
		SET status='SKIPPED',reason_code=?,last_error='',terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND trader_id=? AND leader_pos_id=? AND source_revision=? AND status IN ('RESERVED','SUBMITTED','RECONCILING')`,
		reasonCode, intentID, traderID, leaderPosID, sourceRevision)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		var status string
		if scanErr := tx.QueryRow(`SELECT status FROM copy_trade_execution_intents WHERE id=?`, intentID).Scan(&status); scanErr != nil || status != ExecutionIntentSkipped {
			return fmt.Errorf("detached intent %d lost state race", intentID)
		}
	}
	// A detached lifecycle has no verified follower stop fill. Close only its
	// analytics/AI lifecycle as unscorable; protective-order cleanup remains in
	// the exchange reconciliation loop and must be terminally confirmed.
	if _, err = tx.Exec(`UPDATE copy_guard_cycles
		SET status=?,accounting_status='UNSCORABLE',
		    accounting_error=?,closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND leader_pos_id=? AND closed_at IS NULL`, CopyGuardDetached, reasonCode, traderID, leaderPosID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_reentry_candidates
		SET status='INVALIDATED',last_error=?,closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND leader_pos_id=? AND status IN ('WATCHING','REVIEWING','WAITING','ENTRY_PENDING','PAUSED')`,
		reasonCode, traderID, leaderPosID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='DETACHED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, intentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CopyTradeStore) initExecutionIntentTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_execution_intents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL,
			leader_pos_id TEXT NOT NULL,
			source_revision INTEGER NOT NULL,
			source_fill_id TEXT DEFAULT '',
			source_kind TEXT NOT NULL DEFAULT 'LEADER_TRANSITION',
			canonical_key TEXT NOT NULL DEFAULT '',
			cycle_id INTEGER NOT NULL DEFAULT 0,
			candidate_id INTEGER NOT NULL DEFAULT 0,
			analysis_id INTEGER NOT NULL DEFAULT 0,
			attempt_no INTEGER NOT NULL DEFAULT 0,
			decision_generation INTEGER NOT NULL DEFAULT 0,
			action TEXT NOT NULL,
			symbol TEXT DEFAULT '',
			side TEXT DEFAULT '',
			margin_mode TEXT DEFAULT '',
			leader_target_size REAL DEFAULT 0,
			requested_notional REAL DEFAULT 0,
			requested_quantity REAL DEFAULT 0,
			quantized_quantity REAL DEFAULT 0,
			quantity_step REAL DEFAULT 0,
			exchange_min_quantity REAL DEFAULT 0,
			exchange_min_notional REAL DEFAULT 0,
			minimum_executable_quantity REAL DEFAULT 0,
			filled_quantity REAL DEFAULT 0,
			client_order_id TEXT DEFAULT '',
			exchange_order_id TEXT DEFAULT '',
			exchange_state TEXT DEFAULT '',
			status TEXT NOT NULL DEFAULT 'RESERVED',
			reason_code TEXT DEFAULT '',
			last_error TEXT DEFAULT '',
			failure_counted BOOLEAN NOT NULL DEFAULT 0,
			reconciliation_attempts INTEGER NOT NULL DEFAULT 0,
			first_reconciling_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			submitted_at DATETIME,
			filled_at DATETIME,
			protected_at DATETIME,
			terminal_at DATETIME,
			UNIQUE(trader_id, leader_pos_id, source_revision, action)
		)
	`)
	if err != nil {
		return err
	}
	columns := []struct{ name, definition string }{
		{"failure_counted", "BOOLEAN NOT NULL DEFAULT 0"},
		{"source_kind", "TEXT NOT NULL DEFAULT 'LEADER_TRANSITION'"},
		{"canonical_key", "TEXT NOT NULL DEFAULT ''"},
		{"cycle_id", "INTEGER NOT NULL DEFAULT 0"},
		{"candidate_id", "INTEGER NOT NULL DEFAULT 0"},
		{"analysis_id", "INTEGER NOT NULL DEFAULT 0"},
		{"attempt_no", "INTEGER NOT NULL DEFAULT 0"},
		{"decision_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"exchange_state", "TEXT DEFAULT ''"},
		{"quantity_step", "REAL DEFAULT 0"},
		{"exchange_min_quantity", "REAL DEFAULT 0"},
		{"exchange_min_notional", "REAL DEFAULT 0"},
		{"minimum_executable_quantity", "REAL DEFAULT 0"},
		{"reconciliation_attempts", "INTEGER NOT NULL DEFAULT 0"},
		{"first_reconciling_at", "DATETIME"},
		{"submitted_at", "DATETIME"}, {"filled_at", "DATETIME"},
		{"protected_at", "DATETIME"}, {"terminal_at", "DATETIME"},
	}
	for _, column := range columns {
		if err := ensureSQLiteColumn(s.db, "copy_trade_execution_intents", column.name, column.definition); err != nil {
			return fmt.Errorf("migrate copy_trade_execution_intents.%s: %w", column.name, err)
		}
	}
	if _, err = s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_copy_intent_canonical ON copy_trade_execution_intents(canonical_key) WHERE canonical_key<>''`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_copy_intent_trader_status ON copy_trade_execution_intents(trader_id,status,updated_at)`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_execution_intent_sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			intent_id INTEGER NOT NULL,
			source_fill_id TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(intent_id, source_fill_id),
			FOREIGN KEY(intent_id) REFERENCES copy_trade_execution_intents(id) ON DELETE CASCADE
		)
	`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_copy_intent_sources_fill ON copy_trade_execution_intent_sources(source_fill_id)`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_source_transitions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL,
			leader_pos_id TEXT NOT NULL,
			source_fill_id TEXT NOT NULL,
			source_revision INTEGER NOT NULL,
			action TEXT NOT NULL,
			leader_target_size REAL DEFAULT 0,
			intent_id INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'RESERVED',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(trader_id,leader_pos_id,source_fill_id),
			FOREIGN KEY(intent_id) REFERENCES copy_trade_execution_intents(id) ON DELETE CASCADE
		)
	`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_copy_source_transition_intent ON copy_trade_source_transitions(intent_id,status)`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_execution_order_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			intent_id INTEGER NOT NULL,
			attempt_no INTEGER NOT NULL,
			client_order_id TEXT NOT NULL,
			requested_quantity REAL DEFAULT 0,
			quantized_quantity REAL DEFAULT 0,
			filled_quantity REAL DEFAULT 0,
			exchange_order_id TEXT DEFAULT '',
			exchange_state TEXT DEFAULT '',
			status TEXT NOT NULL DEFAULT 'PREPARED',
			last_error TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			submitted_at DATETIME,
			filled_at DATETIME,
			terminal_at DATETIME,
			UNIQUE(intent_id, attempt_no),
			UNIQUE(intent_id, client_order_id),
			FOREIGN KEY(intent_id) REFERENCES copy_trade_execution_intents(id) ON DELETE CASCADE
		)
	`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_copy_order_attempt_intent ON copy_trade_execution_order_attempts(intent_id,attempt_no)`); err != nil {
		return err
	}
	return nil
}

// ReserveExecutionIntent atomically claims a source transition. Concurrent
// fills that observe the same mapping revision collapse to the same intent.
// FAILED intents may be reclaimed with the same client order id after a
// proven pre-submit failure. A restart-recovered RESERVED intent may also be
// reclaimed only after the healthy source pipeline emits the authoritative
// transition again; the guards below prove that no exchange submission exists.
// Every other existing state is returned unclaimed.
func (s *CopyTradeStore) ReserveExecutionIntent(intent *CopyTradeExecutionIntent) (*CopyTradeExecutionIntent, bool, error) {
	if intent == nil || intent.TraderID == "" || intent.LeaderPosID == "" || intent.SourceRevision <= 0 || intent.Action == "" {
		return nil, false, fmt.Errorf("invalid copy trade execution intent")
	}
	if intent.SourceKind == "" {
		intent.SourceKind = "LEADER_TRANSITION"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`
		INSERT OR IGNORE INTO copy_trade_execution_intents
			(trader_id,leader_pos_id,source_revision,source_fill_id,source_kind,canonical_key,cycle_id,candidate_id,analysis_id,attempt_no,decision_generation,action,symbol,side,margin_mode,
			 leader_target_size,requested_notional,requested_quantity,quantized_quantity,client_order_id,status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'RESERVED')
	`, intent.TraderID, intent.LeaderPosID, intent.SourceRevision, intent.SourceFillID, intent.SourceKind, intent.CanonicalKey,
		intent.CycleID, intent.CandidateID, intent.AnalysisID, intent.AttemptNo, intent.DecisionGeneration, intent.Action,
		intent.Symbol, intent.Side, intent.MarginMode, intent.LeaderTargetSize, intent.RequestedNotional,
		intent.RequestedQuantity, intent.QuantizedQuantity, intent.ClientOrderID)
	if err != nil {
		return nil, false, err
	}
	affected, _ := res.RowsAffected()
	claimed := affected == 1
	if !claimed {
		// A crash can leave a pre-submit source transition reserved while the
		// leader changes direction/target before the first healthy snapshot
		// after restart. With no order attempt there is no exchange side effect,
		// so the same canonical revision may be safely rebound to the current
		// authoritative transition instead of being blocked forever by its old
		// action identity.
		res, err = tx.Exec(`
			UPDATE copy_trade_execution_intents SET
				source_fill_id=?,action=?,cycle_id=?,candidate_id=?,analysis_id=?,attempt_no=?,decision_generation=?,
				symbol=?,side=?,margin_mode=?,leader_target_size=?,requested_notional=?,
				requested_quantity=?,quantized_quantity=?,client_order_id=?,
				status='RESERVED',reason_code='',last_error='',failure_counted=0,
				reconciliation_attempts=0,first_reconciling_at=NULL,terminal_at=NULL,updated_at=CURRENT_TIMESTAMP
			WHERE canonical_key=? AND canonical_key<>''
			  AND status='RECONCILING' AND reason_code='SOURCE_REVALIDATION_REQUIRED'
			  AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
			  AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)
		`, intent.SourceFillID, intent.Action, intent.CycleID, intent.CandidateID, intent.AnalysisID,
			intent.AttemptNo, intent.DecisionGeneration, intent.Symbol, intent.Side, intent.MarginMode,
			intent.LeaderTargetSize, intent.RequestedNotional, intent.RequestedQuantity,
			intent.QuantizedQuantity, intent.ClientOrderID, intent.CanonicalKey)
		if err != nil {
			return nil, false, err
		}
		affected, _ = res.RowsAffected()
		claimed = affected == 1
	}
	if !claimed {
		res, err = tx.Exec(`
			UPDATE copy_trade_execution_intents SET
				source_fill_id=?,cycle_id=?,candidate_id=?,analysis_id=?,attempt_no=?,decision_generation=?,symbol=?,side=?,margin_mode=?,leader_target_size=?,requested_notional=?,
				requested_quantity=?,quantized_quantity=?,client_order_id=CASE WHEN client_order_id='' THEN ? ELSE client_order_id END,
				status='RESERVED',reason_code='',last_error='',failure_counted=0,terminal_at=NULL,updated_at=CURRENT_TIMESTAMP
			WHERE trader_id=? AND leader_pos_id=? AND source_revision=? AND action=?
			  AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
			  AND (
			    (status='FAILED' AND (reason_code IN ('PRE_SUBMIT','DECISION_CHANNEL_BUSY','STARTUP_REPLAY_REQUIRED') OR reason_code LIKE 'PRECHECK_%'))
			    OR (status='SKIPPED' AND reason_code IN ('RISK_CAP','MIN_NOTIONAL','SOURCE_SUPERSEDED') AND ?<>'' AND COALESCE(source_fill_id,'')<>?)
			    OR (status='RECONCILING' AND reason_code='SOURCE_REVALIDATION_REQUIRED'
			        AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id))
			  )
		`, intent.SourceFillID, intent.CycleID, intent.CandidateID, intent.AnalysisID, intent.AttemptNo, intent.DecisionGeneration,
			intent.Symbol, intent.Side, intent.MarginMode, intent.LeaderTargetSize,
			intent.RequestedNotional, intent.RequestedQuantity, intent.QuantizedQuantity, intent.ClientOrderID,
			intent.TraderID, intent.LeaderPosID, intent.SourceRevision, intent.Action, intent.SourceFillID, intent.SourceFillID)
		if err != nil {
			return nil, false, err
		}
		affected, _ = res.RowsAffected()
		claimed = affected == 1
	}
	stored, err := getExecutionIntentTx(tx, intent.TraderID, intent.LeaderPosID, intent.SourceRevision, intent.Action, intent.CanonicalKey)
	if err != nil {
		return nil, false, err
	}
	sourceIDs := append([]string(nil), intent.SourceFillIDs...)
	if len(sourceIDs) == 0 && intent.SourceFillID != "" {
		sourceIDs = []string{intent.SourceFillID}
	}
	mergeSources := claimed ||
		((stored.Status == ExecutionIntentReserved || stored.Status == ExecutionIntentSubmitted || stored.Status == ExecutionIntentReconciling) &&
			stored.Action == intent.Action && stored.LeaderTargetSize == intent.LeaderTargetSize)
	if mergeSources {
		for _, sourceID := range sourceIDs {
			sourceID = strings.TrimSpace(sourceID)
			if sourceID == "" {
				continue
			}
			if _, err = tx.Exec(`INSERT OR IGNORE INTO copy_trade_execution_intent_sources(intent_id,source_fill_id) VALUES(?,?)`, stored.ID, sourceID); err != nil {
				return nil, false, err
			}
			if _, err = tx.Exec(`INSERT INTO copy_trade_source_transitions
				(trader_id,leader_pos_id,source_fill_id,source_revision,action,leader_target_size,intent_id,status)
				VALUES(?,?,?,?,?,?,?,?)
				ON CONFLICT(trader_id,leader_pos_id,source_fill_id) DO UPDATE SET
					source_revision=excluded.source_revision,action=excluded.action,
					leader_target_size=excluded.leader_target_size,intent_id=excluded.intent_id,
					status=excluded.status,updated_at=CURRENT_TIMESTAMP
				WHERE copy_trade_source_transitions.status='SOURCE_REPLAY_PENDING'`,
				intent.TraderID, intent.LeaderPosID, sourceID, stored.SourceRevision,
				intent.Action, intent.LeaderTargetSize, stored.ID, stored.Status); err != nil {
				return nil, false, err
			}
		}
	} else {
		// A terminal or different in-flight transition must never absorb later
		// fills. Persist them as replay-pending audit facts before the caller
		// releases its in-memory de-dup keys. A later successful reservation
		// atomically rebinds the row to the new canonical intent.
		for _, sourceID := range sourceIDs {
			sourceID = strings.TrimSpace(sourceID)
			if sourceID == "" {
				continue
			}
			if _, err = tx.Exec(`INSERT INTO copy_trade_source_transitions
				(trader_id,leader_pos_id,source_fill_id,source_revision,action,leader_target_size,intent_id,status)
				VALUES(?,?,?,?,?,?,?,'SOURCE_REPLAY_PENDING')
				ON CONFLICT(trader_id,leader_pos_id,source_fill_id) DO UPDATE SET
					source_revision=excluded.source_revision,action=excluded.action,
					leader_target_size=excluded.leader_target_size,intent_id=excluded.intent_id,
					status='SOURCE_REPLAY_PENDING',updated_at=CURRENT_TIMESTAMP`,
				intent.TraderID, intent.LeaderPosID, sourceID, intent.SourceRevision,
				intent.Action, intent.LeaderTargetSize, stored.ID); err != nil {
				return nil, false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	stored.SourceFillIDs = sourceIDs
	return stored, claimed, nil
}

// PrepareExecutionOrderAttempt durably records a concrete exchange submission
// before the adapter is called. Reusing the same client id is idempotent.
func (s *CopyTradeStore) PrepareExecutionOrderAttempt(intentID int64, clientOrderID string, quantity float64) (*CopyTradeExecutionOrderAttempt, error) {
	if intentID <= 0 || strings.TrimSpace(clientOrderID) == "" || quantity <= 0 {
		return nil, fmt.Errorf("invalid execution order attempt")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var attemptNo int
	err = tx.QueryRow(`SELECT attempt_no FROM copy_trade_execution_order_attempts WHERE intent_id=? AND client_order_id=?`, intentID, clientOrderID).Scan(&attemptNo)
	if err == sql.ErrNoRows {
		if err = tx.QueryRow(`SELECT COALESCE(MAX(attempt_no),0)+1 FROM copy_trade_execution_order_attempts WHERE intent_id=?`, intentID).Scan(&attemptNo); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,requested_quantity,quantized_quantity,status) VALUES(?,?,?,?,?,'PREPARED')`,
			intentID, attemptNo, clientOrderID, quantity, quantity); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	row := tx.QueryRow(`SELECT id,intent_id,attempt_no,client_order_id,requested_quantity,quantized_quantity,filled_quantity,exchange_order_id,exchange_state,status,last_error,created_at,updated_at,submitted_at,filled_at,terminal_at FROM copy_trade_execution_order_attempts WHERE intent_id=? AND client_order_id=?`, intentID, clientOrderID)
	attempt, err := scanExecutionOrderAttempt(row)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return attempt, nil
}

func (s *CopyTradeStore) CompleteExecutionOrderAttempt(intentID int64, clientOrderID, status, exchangeOrderID, exchangeState, lastError string, filledQuantity float64) error {
	if intentID <= 0 || strings.TrimSpace(clientOrderID) == "" || status == "" {
		return fmt.Errorf("invalid execution order attempt update")
	}
	res, err := s.db.Exec(`UPDATE copy_trade_execution_order_attempts SET status=?,
		exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,
		exchange_state=CASE WHEN ?<>'' THEN ? ELSE exchange_state END,
		filled_quantity=CASE WHEN ?>0 THEN ? ELSE filled_quantity END,last_error=?,
		submitted_at=CASE WHEN ? IN ('SUBMITTED','FILLED') THEN COALESCE(submitted_at,CURRENT_TIMESTAMP) ELSE submitted_at END,
		filled_at=CASE WHEN ?='FILLED' THEN COALESCE(filled_at,CURRENT_TIMESTAMP) ELSE filled_at END,
		terminal_at=CASE WHEN ? IN ('FILLED','FAILED') THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE terminal_at END,
		updated_at=CURRENT_TIMESTAMP WHERE intent_id=? AND client_order_id=?`,
		status, exchangeOrderID, exchangeOrderID, exchangeState, exchangeState, filledQuantity, filledQuantity, lastError,
		status, status, status, intentID, clientOrderID)
	if err != nil {
		return err
	}
	updated, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return fmt.Errorf("execution order attempt not found: intent=%d client_order_id=%s", intentID, clientOrderID)
	}
	return nil
}

func (s *CopyTradeStore) ListExecutionOrderAttempts(intentID int64) ([]*CopyTradeExecutionOrderAttempt, error) {
	rows, err := s.db.Query(`SELECT id,intent_id,attempt_no,client_order_id,requested_quantity,quantized_quantity,filled_quantity,exchange_order_id,exchange_state,status,last_error,created_at,updated_at,submitted_at,filled_at,terminal_at FROM copy_trade_execution_order_attempts WHERE intent_id=? ORDER BY attempt_no`, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyTradeExecutionOrderAttempt
	for rows.Next() {
		attempt, scanErr := scanExecutionOrderAttempt(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, attempt)
	}
	return out, rows.Err()
}

func scanExecutionOrderAttempt(scanner interface{ Scan(...interface{}) error }) (*CopyTradeExecutionOrderAttempt, error) {
	var x CopyTradeExecutionOrderAttempt
	var created, updated string
	var submitted, filled, terminal sql.NullString
	if err := scanner.Scan(&x.ID, &x.IntentID, &x.AttemptNo, &x.ClientOrderID, &x.RequestedQuantity, &x.QuantizedQuantity,
		&x.FilledQuantity, &x.ExchangeOrderID, &x.ExchangeState, &x.Status, &x.LastError, &created, &updated, &submitted, &filled, &terminal); err != nil {
		return nil, err
	}
	var err error
	if x.CreatedAt, err = parseDBTime(created); err != nil {
		return nil, err
	}
	if x.UpdatedAt, err = parseDBTime(updated); err != nil {
		return nil, err
	}
	if x.SubmittedAt, err = parseNullableDBTime(submitted); err != nil {
		return nil, err
	}
	if x.FilledAt, err = parseNullableDBTime(filled); err != nil {
		return nil, err
	}
	if x.TerminalAt, err = parseNullableDBTime(terminal); err != nil {
		return nil, err
	}
	return &x, nil
}

func getExecutionIntentTx(tx *sql.Tx, traderID, leaderPosID string, sourceRevision int64, action, canonicalKey string) (*CopyTradeExecutionIntent, error) {
	where := `trader_id=? AND leader_pos_id=? AND source_revision=? AND action=?`
	args := []interface{}{traderID, leaderPosID, sourceRevision, action}
	if canonicalKey != "" {
		where = `canonical_key=? OR (` + where + `)`
		args = append([]interface{}{canonicalKey}, args...)
	}
	row := tx.QueryRow(`SELECT id,trader_id,leader_pos_id,source_revision,COALESCE(source_fill_id,''),action,
		COALESCE(source_kind,'LEADER_TRANSITION'),COALESCE(canonical_key,''),
		COALESCE(cycle_id,0),COALESCE(candidate_id,0),COALESCE(analysis_id,0),COALESCE(attempt_no,0),COALESCE(decision_generation,0),
		COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),COALESCE(leader_target_size,0),
		COALESCE(requested_notional,0),COALESCE(requested_quantity,0),COALESCE(quantized_quantity,0),
		COALESCE(quantity_step,0),COALESCE(exchange_min_quantity,0),COALESCE(exchange_min_notional,0),COALESCE(minimum_executable_quantity,0),
		COALESCE(filled_quantity,0),COALESCE(client_order_id,''),COALESCE(exchange_order_id,''),status,
		COALESCE(exchange_state,''),COALESCE(reason_code,''),COALESCE(last_error,''),COALESCE(failure_counted,0),created_at,updated_at,
		submitted_at,filled_at,protected_at,terminal_at
		FROM copy_trade_execution_intents WHERE `+where+` ORDER BY CASE WHEN canonical_key=? THEN 0 ELSE 1 END LIMIT 1`, append(args, canonicalKey)...)
	return scanExecutionIntent(row)
}

func scanExecutionIntent(scanner interface{ Scan(...interface{}) error }) (*CopyTradeExecutionIntent, error) {
	var x CopyTradeExecutionIntent
	var created, updated string
	var submitted, filled, protected, terminal sql.NullString
	if err := scanner.Scan(&x.ID, &x.TraderID, &x.LeaderPosID, &x.SourceRevision, &x.SourceFillID, &x.Action,
		&x.SourceKind, &x.CanonicalKey,
		&x.CycleID, &x.CandidateID, &x.AnalysisID, &x.AttemptNo, &x.DecisionGeneration,
		&x.Symbol, &x.Side, &x.MarginMode, &x.LeaderTargetSize, &x.RequestedNotional, &x.RequestedQuantity,
		&x.QuantizedQuantity, &x.QuantityStep, &x.ExchangeMinQuantity, &x.ExchangeMinNotional, &x.MinimumExecutableQuantity,
		&x.FilledQuantity, &x.ClientOrderID, &x.ExchangeOrderID, &x.Status,
		&x.ExchangeState, &x.ReasonCode, &x.LastError, &x.FailureCounted, &created, &updated,
		&submitted, &filled, &protected, &terminal); err != nil {
		return nil, err
	}
	var err error
	if x.CreatedAt, err = parseDBTime(created); err != nil {
		return nil, err
	}
	if x.UpdatedAt, err = parseDBTime(updated); err != nil {
		return nil, err
	}
	if x.SubmittedAt, err = parseNullableDBTime(submitted); err != nil {
		return nil, err
	}
	if x.FilledAt, err = parseNullableDBTime(filled); err != nil {
		return nil, err
	}
	if x.ProtectedAt, err = parseNullableDBTime(protected); err != nil {
		return nil, err
	}
	if x.TerminalAt, err = parseNullableDBTime(terminal); err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *CopyTradeStore) UpdateExecutionIntent(id int64, status, reasonCode, lastError, exchangeOrderID string, requestedQty, quantizedQty, filledQty float64) error {
	if id <= 0 || status == "" {
		return fmt.Errorf("invalid execution intent transition")
	}
	res, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET status=?,
		reason_code=CASE WHEN ?<>'' THEN ? ELSE reason_code END,last_error=?,
		exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,
		requested_quantity=CASE WHEN ?>0 THEN ? ELSE requested_quantity END,
		quantized_quantity=CASE WHEN ?>0 THEN ? ELSE quantized_quantity END,
		filled_quantity=CASE WHEN ?>0 THEN ? ELSE filled_quantity END,
		submitted_at=CASE WHEN ?='SUBMITTED' THEN COALESCE(submitted_at,CURRENT_TIMESTAMP) ELSE submitted_at END,
		filled_at=CASE WHEN ?='FILLED' THEN COALESCE(filled_at,CURRENT_TIMESTAMP) ELSE filled_at END,
		protected_at=CASE WHEN ?='PROTECTED' THEN COALESCE(protected_at,CURRENT_TIMESTAMP) ELSE protected_at END,
		terminal_at=CASE WHEN ? IN ('SKIPPED','FAILED') THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE terminal_at END,
		updated_at=CURRENT_TIMESTAMP WHERE id=? AND (`+validExecutionIntentTransitionSQL()+`)`, status, reasonCode, reasonCode, lastError,
		exchangeOrderID, exchangeOrderID, requestedQty, requestedQty, quantizedQty, quantizedQty, filledQty, filledQty,
		status, status, status, status, id, status, status, status, status, status)
	if err != nil {
		return err
	}
	updated, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 1 {
		_, _ = s.db.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, status, id)
		return nil
	}
	var current string
	if err := s.db.QueryRow(`SELECT status FROM copy_trade_execution_intents WHERE id=?`, id).Scan(&current); err != nil {
		return err
	}
	if current == status {
		return nil
	}
	return fmt.Errorf("invalid execution intent transition %s -> %s for intent %d", current, status, id)
}

func (s *CopyTradeStore) UpdateExecutionIntentExchangeState(id int64, exchangeState string) error {
	if id <= 0 || strings.TrimSpace(exchangeState) == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET exchange_state=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, strings.ToUpper(strings.TrimSpace(exchangeState)), id)
	return err
}

// RecordExecutionReconciliationFailure bounds historical pre-submit recovery.
// An intent with any submission evidence must remain RECONCILING indefinitely:
// stopping automatic exchange lookup could strand a filled but unmapped and
// unprotected position. Only a reservation with no submitted_at, exchange id
// or order-attempt row may become the explicit manual-review terminal.
func (s *CopyTradeStore) RecordExecutionReconciliationFailure(id int64, reasonCode, message string, maxAttempts int, maxAge time.Duration) (bool, error) {
	if id <= 0 || strings.TrimSpace(reasonCode) == "" || maxAttempts <= 0 || maxAge <= 0 {
		return false, fmt.Errorf("invalid execution reconciliation failure")
	}
	res, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET
		reconciliation_attempts=COALESCE(reconciliation_attempts,0)+1,
		first_reconciling_at=COALESCE(first_reconciling_at,CURRENT_TIMESTAMP),
		status=CASE WHEN (COALESCE(reconciliation_attempts,0)+1>=?
			OR (julianday(CURRENT_TIMESTAMP)-julianday(COALESCE(first_reconciling_at,CURRENT_TIMESTAMP)))*86400>=?)
			AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
			AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)
			THEN 'FAILED' ELSE 'RECONCILING' END,
		reason_code=CASE WHEN (COALESCE(reconciliation_attempts,0)+1>=?
			OR (julianday(CURRENT_TIMESTAMP)-julianday(COALESCE(first_reconciling_at,CURRENT_TIMESTAMP)))*86400>=?)
			AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
			AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)
			THEN 'MANUAL_REVIEW_REQUIRED' ELSE ? END,
		last_error=?,
		terminal_at=CASE WHEN (COALESCE(reconciliation_attempts,0)+1>=?
			OR (julianday(CURRENT_TIMESTAMP)-julianday(COALESCE(first_reconciling_at,CURRENT_TIMESTAMP)))*86400>=?)
			AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
			AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)
			THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE terminal_at END,
		updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING')`,
		maxAttempts, maxAge.Seconds(), maxAttempts, maxAge.Seconds(), reasonCode, message,
		maxAttempts, maxAge.Seconds(), id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var status string
		if err = s.db.QueryRow(`SELECT status FROM copy_trade_execution_intents WHERE id=?`, id).Scan(&status); err != nil {
			return false, err
		}
		return status == ExecutionIntentFailed, nil
	}
	var status string
	if err = s.db.QueryRow(`SELECT status FROM copy_trade_execution_intents WHERE id=?`, id).Scan(&status); err != nil {
		return false, err
	}
	if _, syncErr := s.db.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, status, id); syncErr != nil {
		return status == ExecutionIntentFailed, syncErr
	}
	return status == ExecutionIntentFailed, nil
}

// SupersedeUnsubmittedExecutionIntent closes a pre-submit restart reservation
// after a healthy source snapshot proves that the leader position no longer
// exists. No mapping revision is advanced because no follower relationship was
// ever created; a later new source fill may safely reclaim this zero-side-effect
// intent identity.
func (s *CopyTradeStore) SupersedeUnsubmittedExecutionIntent(id int64, traderID, leaderPosID string) error {
	if id <= 0 || strings.TrimSpace(traderID) == "" || strings.TrimSpace(leaderPosID) == "" {
		return fmt.Errorf("invalid superseded execution intent")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE copy_trade_execution_intents SET
		status='SKIPPED',reason_code='SOURCE_SUPERSEDED',
		last_error='healthy source snapshot no longer contains the reserved position',
		terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND trader_id=? AND leader_pos_id=?
		  AND status='RECONCILING' AND reason_code='SOURCE_REVALIDATION_REQUIRED'
		  AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
		  AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)`,
		id, traderID, leaderPosID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("execution intent %d is not safely supersedable", id)
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions
		SET status='SOURCE_SUPERSEDED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CopyTradeStore) UpdateExecutionIntentQuantityConstraints(id int64, requested, quantized, step, exchangeMinQuantity, exchangeMinNotional, minimumExecutable float64) error {
	if id <= 0 || requested <= 0 || step <= 0 || minimumExecutable <= 0 {
		return fmt.Errorf("invalid execution quantity constraints")
	}
	_, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET
		requested_quantity=?,quantized_quantity=?,quantity_step=?,
		exchange_min_quantity=?,exchange_min_notional=?,minimum_executable_quantity=?,
		updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		requested, quantized, step, exchangeMinQuantity, exchangeMinNotional, minimumExecutable, id)
	return err
}

func validExecutionIntentTransitionSQL() string {
	return `(status=? OR
		(status='RESERVED' AND ? IN ('SUBMITTED','FILLED','SKIPPED','FAILED','RECONCILING')) OR
		(status='SUBMITTED' AND ? IN ('FILLED','SKIPPED','FAILED','RECONCILING')) OR
		(status='RECONCILING' AND ? IN ('FILLED','SKIPPED','FAILED','RECONCILING')) OR
		(status='FILLED' AND ? IN ('PROTECTED','FAILED','RECONCILING')))`
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
		COALESCE(source_kind,'LEADER_TRANSITION'),COALESCE(canonical_key,''),
		COALESCE(cycle_id,0),COALESCE(candidate_id,0),COALESCE(analysis_id,0),COALESCE(attempt_no,0),COALESCE(decision_generation,0),
		COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),COALESCE(leader_target_size,0),
		COALESCE(requested_notional,0),COALESCE(requested_quantity,0),COALESCE(quantized_quantity,0),
		COALESCE(quantity_step,0),COALESCE(exchange_min_quantity,0),COALESCE(exchange_min_notional,0),COALESCE(minimum_executable_quantity,0),
		COALESCE(filled_quantity,0),COALESCE(client_order_id,''),COALESCE(exchange_order_id,''),status,
		COALESCE(exchange_state,''),COALESCE(reason_code,''),COALESCE(last_error,''),COALESCE(failure_counted,0),created_at,updated_at,
		submitted_at,filled_at,protected_at,terminal_at
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

// ListFilledExecutionIntentsWithLifecycleGap finds the narrow crash windows
// after CommitLeaderExecutionFill succeeded but the Copy Guard lifecycle hook
// did not. FILLED remains a valid terminal execution state, so this query does
// not broadly replay it: it only returns an exact mapping revision with either
// a missing open protection cycle (open/add/reduce) or an open cycle left
// behind after a confirmed leader close.
func (s *CopyTradeStore) ListFilledExecutionIntentsWithLifecycleGap(traderID string) ([]*CopyTradeExecutionIntent, error) {
	rows, err := s.db.Query(`SELECT i.id,i.trader_id,i.leader_pos_id,i.source_revision,COALESCE(i.source_fill_id,''),i.action,
		COALESCE(i.source_kind,'LEADER_TRANSITION'),COALESCE(i.canonical_key,''),
		COALESCE(i.cycle_id,0),COALESCE(i.candidate_id,0),COALESCE(i.analysis_id,0),COALESCE(i.attempt_no,0),COALESCE(i.decision_generation,0),
		COALESCE(i.symbol,''),COALESCE(i.side,''),COALESCE(i.margin_mode,''),COALESCE(i.leader_target_size,0),
		COALESCE(i.requested_notional,0),COALESCE(i.requested_quantity,0),COALESCE(i.quantized_quantity,0),
		COALESCE(i.quantity_step,0),COALESCE(i.exchange_min_quantity,0),COALESCE(i.exchange_min_notional,0),COALESCE(i.minimum_executable_quantity,0),
		COALESCE(i.filled_quantity,0),COALESCE(i.client_order_id,''),COALESCE(i.exchange_order_id,''),i.status,
		COALESCE(i.exchange_state,''),COALESCE(i.reason_code,''),COALESCE(i.last_error,''),COALESCE(i.failure_counted,0),i.created_at,i.updated_at,
		i.submitted_at,i.filled_at,i.protected_at,i.terminal_at
		FROM copy_trade_execution_intents i
		JOIN copy_trade_position_mappings m
		  ON m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id
		 AND COALESCE(m.source_revision,0)=i.source_revision
		WHERE i.trader_id=? AND i.source_kind='LEADER_TRANSITION' AND i.status='FILLED'
		  AND (
			(i.action IN ('open_long','open_short','reduce_long','reduce_short')
			 AND m.status='active'
			 AND NOT EXISTS (
				SELECT 1 FROM copy_guard_cycles c
				WHERE c.trader_id=i.trader_id AND c.leader_pos_id=i.leader_pos_id
				  AND c.closed_at IS NULL
			 ))
			OR
			(i.action IN ('close_long','close_short')
			 AND m.status='closed'
			 AND EXISTS (
				SELECT 1 FROM copy_guard_cycles c
				WHERE c.trader_id=i.trader_id AND c.leader_pos_id=i.leader_pos_id
				  AND c.closed_at IS NULL
			 ))
		  )
		ORDER BY i.id`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyTradeExecutionIntent
	for rows.Next() {
		x, scanErr := scanExecutionIntent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// HasUnfinishedLeaderExecutionIntent prevents startup baseline initialization
// from relabeling a pre-crash source transition as a historical ignored
// position. The first healthy source snapshot must be allowed to reclaim a
// RESERVED/SOURCE_REVALIDATION_REQUIRED intent and run the normal preflight.
func (s *CopyTradeStore) HasUnfinishedLeaderExecutionIntent(traderID, leaderPosID string) (bool, error) {
	if strings.TrimSpace(traderID) == "" || strings.TrimSpace(leaderPosID) == "" {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM copy_trade_execution_intents
		WHERE trader_id=? AND leader_pos_id=? AND source_kind='LEADER_TRANSITION'
		  AND status IN ('RESERVED','SUBMITTED','RECONCILING')
	)`, traderID, leaderPosID).Scan(&exists)
	return exists, err
}

func (s *CopyTradeStore) ListExecutionIntentsByCycle(cycleID int64) ([]*CopyTradeExecutionIntent, error) {
	rows, err := s.db.Query(`SELECT id,trader_id,leader_pos_id,source_revision,COALESCE(source_fill_id,''),action,
		COALESCE(source_kind,'LEADER_TRANSITION'),COALESCE(canonical_key,''),
		COALESCE(cycle_id,0),COALESCE(candidate_id,0),COALESCE(analysis_id,0),COALESCE(attempt_no,0),COALESCE(decision_generation,0),
		COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),COALESCE(leader_target_size,0),
		COALESCE(requested_notional,0),COALESCE(requested_quantity,0),COALESCE(quantized_quantity,0),
		COALESCE(quantity_step,0),COALESCE(exchange_min_quantity,0),COALESCE(exchange_min_notional,0),COALESCE(minimum_executable_quantity,0),
		COALESCE(filled_quantity,0),COALESCE(client_order_id,''),COALESCE(exchange_order_id,''),status,
		COALESCE(exchange_state,''),COALESCE(reason_code,''),COALESCE(last_error,''),COALESCE(failure_counted,0),created_at,updated_at,
		submitted_at,filled_at,protected_at,terminal_at
		FROM copy_trade_execution_intents WHERE cycle_id=? ORDER BY id`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyTradeExecutionIntent
	for rows.Next() {
		x, scanErr := scanExecutionIntent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
