package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	ExecutionIntentSourceMigrationMarginStop = "MIGRATION_MARGIN_STOP"

	ExecutionIntentReserved         = "RESERVED"
	ExecutionIntentSubmitted        = "SUBMITTED"
	ExecutionIntentFilled           = "FILLED"
	ExecutionIntentPartiallyFilled  = "PARTIALLY_FILLED"
	ExecutionIntentCompletedPartial = "COMPLETED_PARTIAL"
	ExecutionIntentProtected        = "PROTECTED"
	ExecutionIntentSkipped          = "SKIPPED"
	ExecutionIntentFailed           = "FAILED"
	ExecutionIntentReconciling      = "RECONCILING"

	ExecutionIntentCycleTerminatedBeforeSubmit = "CYCLE_TERMINATED_BEFORE_SUBMIT"

	ExecutionOrderAttemptPrepared        = "PREPARED"
	ExecutionOrderAttemptSubmitted       = "SUBMITTED"
	ExecutionOrderAttemptPartiallyFilled = "PARTIALLY_FILLED"
	ExecutionOrderAttemptFilled          = "FILLED"
	ExecutionOrderAttemptFailed          = "FAILED"
	ExecutionOrderAttemptUnknown         = "UNKNOWN"
	ExecutionOrderAttemptTerminalNoFill  = "TERMINAL_NO_FILL"
)

func (s *CopyTradeStore) CountMigrationMarginStopIntents(cycleID int64) (int, error) {
	if cycleID <= 0 {
		return 0, nil
	}
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents WHERE cycle_id=? AND source_kind=?`,
		cycleID, ExecutionIntentSourceMigrationMarginStop).Scan(&count)
	return count, err
}

func (s *CopyTradeStore) HasUnfinishedMigrationMarginStopIntents(traderID string) (bool, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM copy_trade_execution_intents
		WHERE trader_id=? AND source_kind=?
		  AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED')
	`, traderID, ExecutionIntentSourceMigrationMarginStop).Scan(&count)
	return count > 0, err
}

var ErrOrdinaryCatchupUnsettled = errors.New("ordinary catch-up has unresolved exchange side effects")

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
	QuantityKind      string     `json:"quantity_kind"`
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
	TargetQuantity            float64    `json:"target_quantity"`
	RequestedQuantity         float64    `json:"requested_quantity"`
	QuantizedQuantity         float64    `json:"quantized_quantity"`
	QuantityStep              float64    `json:"quantity_step"`
	ExchangeMinQuantity       float64    `json:"exchange_min_quantity"`
	ExchangeMinNotional       float64    `json:"exchange_min_notional"`
	MinimumExecutableQuantity float64    `json:"minimum_executable_quantity"`
	FilledQuantity            float64    `json:"filled_quantity"`
	FilledNotional            float64    `json:"filled_notional"`
	FollowerEquityAtTarget    float64    `json:"follower_equity_at_target"`
	TargetAccountPct          float64    `json:"target_account_pct"`
	CatchupDeadlineAt         *time.Time `json:"catchup_deadline_at,omitempty"`
	CatchupAnchorPrice        float64    `json:"catchup_anchor_price"`
	LastCatchupReason         string     `json:"last_catchup_reason"`
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
	ClientOrderID        string
	ExchangeOrderID      string
	ExchangeState        string
	AttemptQuantity      float64
	OrderTerminal        bool
}

type AIReentryFillSnapshot struct {
	IntentID           int64
	TraderID           string
	ClientOrderID      string
	ExchangeOrderID    string
	ExchangeState      string
	CumulativeQuantity float64
	CumulativeNotional float64
	AveragePrice       float64
	AttemptQuantity    float64
	LeaderTargetSize   float64
	ATR                float64
	OrderTerminal      bool
}

type AIReentryFillResult struct {
	DeltaQuantity      float64
	DeltaNotional      float64
	CumulativeQuantity float64
	CumulativeNotional float64
	IntentStatus       string
	FirstFill          bool
}

func executionFillSnapshotTx(tx *sql.Tx, intentID int64, clientOrderID, exchangeOrderID string) (string, float64, float64, error) {
	keys := make([]string, 0, 2)
	for _, candidate := range []string{clientOrderID, exchangeOrderID} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || (len(keys) > 0 && keys[0] == candidate) {
			continue
		}
		keys = append(keys, candidate)
	}
	if len(keys) == 0 {
		return "", 0, 0, fmt.Errorf("intent %d acknowledged fill has no durable order identity", intentID)
	}
	var foundKey string
	var filled, notional float64
	for _, key := range keys {
		var candidateFilled, candidateNotional float64
		err := tx.QueryRow(`SELECT filled_quantity,filled_notional
			FROM copy_trade_execution_fill_commits WHERE intent_id=? AND fill_key=?`,
			intentID, key).Scan(&candidateFilled, &candidateNotional)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return "", 0, 0, err
		}
		if foundKey != "" {
			return "", 0, 0, fmt.Errorf("intent %d has duplicate client/exchange fill aliases", intentID)
		}
		foundKey, filled, notional = key, candidateFilled, candidateNotional
	}
	if foundKey != "" {
		return foundKey, filled, notional, nil
	}
	// Client order ID is generated and persisted before submission, so it is
	// the stable canonical identity. Exchange order ID is a compatibility
	// fallback for old/recovered attempts where no client ID is available.
	return keys[0], 0, 0, nil
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
	var targetQuantity, previousFilled, previousFilledNotional float64
	if err = tx.QueryRow(`SELECT status,
		COALESCE(NULLIF(target_quantity,0),NULLIF(quantized_quantity,0),requested_quantity,0),
		COALESCE(filled_quantity,0),COALESCE(filled_notional,0)
		FROM copy_trade_execution_intents
		WHERE id=? AND trader_id=? AND leader_pos_id=? AND source_revision=?`,
		c.IntentID, c.TraderID, c.LeaderPosID, c.SourceRevision).
		Scan(&intentStatus, &targetQuantity, &previousFilled, &previousFilledNotional); err != nil {
		return err
	}
	if intentStatus != ExecutionIntentFilled && intentStatus != ExecutionIntentProtected &&
		intentStatus != ExecutionIntentSubmitted && intentStatus != ExecutionIntentReconciling &&
		intentStatus != ExecutionIntentReserved && intentStatus != ExecutionIntentPartiallyFilled {
		return fmt.Errorf("intent %d cannot commit fill from %s", c.IntentID, intentStatus)
	}
	if c.FilledNotional <= 0 && c.FillPrice > 0 {
		c.FilledNotional = c.FilledQuantity * c.FillPrice
	}
	// Exchange order status exposes cumulative execution. Persist that
	// cumulative snapshot and apply only its positive delta. This makes NEW →
	// PARTIALLY_FILLED → FILLED and restart replays idempotent for one order.
	fillKey, previousOrderFilled, previousOrderNotional, snapshotErr := executionFillSnapshotTx(
		tx, c.IntentID, c.ClientOrderID, c.ExchangeOrderID,
	)
	if snapshotErr != nil {
		return snapshotErr
	}
	epsilon := math.Max(1e-12, math.Abs(c.FilledQuantity)*1e-9)
	if c.FilledQuantity+epsilon < previousOrderFilled {
		return fmt.Errorf("exchange cumulative fill regressed for intent %d order %s: %.8f < %.8f",
			c.IntentID, fillKey, c.FilledQuantity, previousOrderFilled)
	}
	deltaFilled := c.FilledQuantity - previousOrderFilled
	deltaNotional := c.FilledNotional - previousOrderNotional
	notionalEpsilon := math.Max(0.01, math.Abs(c.FilledNotional)*1e-9)
	if deltaNotional < -notionalEpsilon {
		return fmt.Errorf("exchange cumulative fill notional regressed for intent %d order %s: %.8f < %.8f",
			c.IntentID, fillKey, c.FilledNotional, previousOrderNotional)
	}
	if deltaNotional < 0 {
		deltaNotional = 0
	}
	if strings.TrimSpace(c.ClientOrderID) != "" {
		attemptTarget := c.AttemptQuantity
		if attemptTarget <= 0 {
			attemptTarget = c.FilledQuantity
		}
		attemptStatus := ExecutionOrderAttemptPartiallyFilled
		if c.OrderTerminal && c.FilledQuantity+math.Max(1e-12, attemptTarget*1e-8) >= attemptTarget {
			attemptStatus = ExecutionOrderAttemptFilled
		}
		if _, err = tx.Exec(`UPDATE copy_trade_execution_order_attempts SET
			status=?,exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,
			exchange_state=CASE WHEN ?<>'' THEN ? ELSE exchange_state END,
			filled_quantity=CASE WHEN ?>filled_quantity THEN ? ELSE filled_quantity END,
			last_error='',submitted_at=COALESCE(submitted_at,CURRENT_TIMESTAMP),
			filled_at=COALESCE(filled_at,CURRENT_TIMESTAMP),
			terminal_at=CASE WHEN ? THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE terminal_at END,
			updated_at=CURRENT_TIMESTAMP
			WHERE intent_id=? AND client_order_id=?`,
			attemptStatus, c.ExchangeOrderID, c.ExchangeOrderID, c.ExchangeState, c.ExchangeState,
			c.FilledQuantity, c.FilledQuantity, c.OrderTerminal, c.IntentID, c.ClientOrderID); err != nil {
			return err
		}
	}
	if deltaFilled <= epsilon {
		return tx.Commit()
	}
	if deltaNotional <= 0 && c.FillPrice > 0 {
		deltaNotional = deltaFilled * c.FillPrice
	}
	if _, err = tx.Exec(`INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional)
		VALUES(?,?,?,?) ON CONFLICT(intent_id,fill_key) DO UPDATE SET
		filled_quantity=excluded.filled_quantity,filled_notional=excluded.filled_notional,updated_at=CURRENT_TIMESTAMP`,
		c.IntentID, fillKey, c.FilledQuantity, c.FilledNotional); err != nil {
		return err
	}
	cumulativeFilled := previousFilled + deltaFilled
	cumulativeFilledNotional := previousFilledNotional + deltaNotional
	nextStatus := ExecutionIntentFilled
	if targetQuantity > 0 && cumulativeFilled+math.Max(1e-12, targetQuantity*1e-8) < targetQuantity {
		nextStatus = ExecutionIntentPartiallyFilled
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
		_, err = tx.Exec(`UPDATE copy_trade_execution_intents SET status=?,filled_quantity=?,filled_notional=?,
			reason_code=CASE WHEN ?='PARTIALLY_FILLED' THEN 'CATCHUP_PENDING' ELSE reason_code END,
			last_catchup_reason=CASE WHEN ?='PARTIALLY_FILLED' THEN 'CATCHUP_PENDING' ELSE last_catchup_reason END,
			exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,exchange_state=?,
			filled_at=COALESCE(filled_at,CURRENT_TIMESTAMP),
			terminal_at=CASE WHEN ?='FILLED' THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE NULL END,
			updated_at=CURRENT_TIMESTAMP WHERE id=?`,
			nextStatus, cumulativeFilled, cumulativeFilledNotional, nextStatus, nextStatus,
			c.ExchangeOrderID, c.ExchangeOrderID, c.ExchangeState, nextStatus, c.IntentID)
		if err != nil {
			return err
		}
		if open {
			if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET open_size_usd=COALESCE(open_size_usd,0)+?,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=?`, deltaNotional, c.TraderID, c.LeaderPosID); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, nextStatus, c.IntentID); err != nil {
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
			c.TraderID, c.LeaderPosID, c.LeaderID, c.Symbol, c.SourceSymbol, c.ExecutionSymbol, c.SourceQuoteAsset, c.ExecutionSettleAsset, c.SourceRevision, c.Side, c.MarginMode, c.FillPrice, deltaNotional, c.LeaderTargetSize)
	case open && (mappingStatus == MappingStatusClosed || mappingStatus == MappingStatusIgnored):
		_, err = tx.Exec(`UPDATE copy_trade_position_mappings SET leader_id=?,symbol=?,source_symbol=?,execution_symbol=?,source_quote_asset=?,execution_settle_asset=?,source_revision=?,side=?,margin_mode=?,status='active',opened_at=CURRENT_TIMESTAMP,closed_at=NULL,close_price=0,open_price=?,open_size_usd=?,last_known_size=?,add_count=0,reduce_count=0,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=?`,
			c.LeaderID, c.Symbol, c.SourceSymbol, c.ExecutionSymbol, c.SourceQuoteAsset, c.ExecutionSettleAsset, c.SourceRevision, c.Side, c.MarginMode, c.FillPrice, deltaNotional, c.LeaderTargetSize, c.TraderID, c.LeaderPosID)
	case open && mappingStatus == MappingStatusActive && strings.EqualFold(mappingSide, c.Side):
		addIncrement := 0
		if c.IsAdd {
			addIncrement = 1
		}
		_, err = tx.Exec(`UPDATE copy_trade_position_mappings SET
			source_revision=?,last_known_size=?,open_size_usd=COALESCE(open_size_usd,0)+?,
			add_count=add_count+?,updated_at=CURRENT_TIMESTAMP
			WHERE trader_id=? AND leader_pos_id=? AND status='active'`,
			c.SourceRevision, c.LeaderTargetSize, deltaNotional, addIncrement, c.TraderID, c.LeaderPosID)
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
	res, err := tx.Exec(`UPDATE copy_trade_execution_intents SET status=?,filled_quantity=?,filled_notional=?,
		reason_code=CASE WHEN ?='PARTIALLY_FILLED' THEN 'CATCHUP_PENDING' ELSE reason_code END,
		last_catchup_reason=CASE WHEN ?='PARTIALLY_FILLED' THEN 'CATCHUP_PENDING' ELSE last_catchup_reason END,
		exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,exchange_state=?,
		filled_at=COALESCE(filled_at,CURRENT_TIMESTAMP),
		terminal_at=CASE WHEN ?='FILLED' THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE NULL END,
		updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED')`,
		nextStatus, cumulativeFilled, cumulativeFilledNotional, nextStatus, nextStatus,
		c.ExchangeOrderID, c.ExchangeOrderID, c.ExchangeState, nextStatus, c.IntentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("intent %d fill commit lost state race", c.IntentID)
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, nextStatus, c.IntentID); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyAIReentryFillSnapshot applies one exchange cumulative-fill snapshot to
// the independent AI lifecycle. It intentionally does not touch ordinary
// leader source sizing or catch-up state. The fill snapshot, durable order
// attempt, execution intent, stopped mapping, Copy Guard cycle and attempt are
// committed in one transaction so a partial fill is immediately auditable and
// protectable without being counted twice on later polls or restart.
func (s *CopyTradeStore) ApplyAIReentryFillSnapshot(c AIReentryFillSnapshot) (AIReentryFillResult, error) {
	out := AIReentryFillResult{}
	if c.IntentID <= 0 || c.TraderID == "" || c.CumulativeQuantity <= 0 {
		return out, fmt.Errorf("invalid AI reentry fill snapshot")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	var sourceKind, status, leaderPosID, symbol, side, marginMode string
	var cycleID, candidateID, analysisID int64
	var attemptNo int
	var generation int
	var intentFilled, intentNotional float64
	if err = tx.QueryRow(`SELECT COALESCE(source_kind,''),status,leader_pos_id,COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),
		COALESCE(cycle_id,0),COALESCE(candidate_id,0),COALESCE(analysis_id,0),COALESCE(attempt_no,0),COALESCE(decision_generation,0),
		COALESCE(filled_quantity,0),COALESCE(filled_notional,0)
		FROM copy_trade_execution_intents WHERE id=? AND trader_id=?`,
		c.IntentID, c.TraderID).Scan(
		&sourceKind, &status, &leaderPosID, &symbol, &side, &marginMode,
		&cycleID, &candidateID, &analysisID, &attemptNo, &generation,
		&intentFilled, &intentNotional,
	); err != nil {
		return out, err
	}
	if sourceKind != "AI_REENTRY" || cycleID <= 0 || candidateID <= 0 || analysisID <= 0 || attemptNo <= 0 || generation <= 0 {
		return out, fmt.Errorf("intent %d is not a complete AI reentry intent", c.IntentID)
	}
	switch status {
	case ExecutionIntentReserved, ExecutionIntentSubmitted, ExecutionIntentReconciling,
		ExecutionIntentPartiallyFilled, ExecutionIntentFilled, ExecutionIntentProtected:
	default:
		return out, fmt.Errorf("AI reentry intent %d cannot accept fill from %s", c.IntentID, status)
	}

	fillKey, previousOrderFilled, previousOrderNotional, snapshotErr := executionFillSnapshotTx(
		tx, c.IntentID, c.ClientOrderID, c.ExchangeOrderID,
	)
	if snapshotErr != nil {
		return out, snapshotErr
	}
	epsilon := math.Max(1e-12, math.Abs(c.CumulativeQuantity)*1e-9)
	if c.CumulativeQuantity+epsilon < previousOrderFilled {
		return out, fmt.Errorf("AI exchange cumulative fill regressed for intent %d order %s: %.8f < %.8f",
			c.IntentID, fillKey, c.CumulativeQuantity, previousOrderFilled)
	}
	if c.CumulativeNotional <= 0 && c.AveragePrice > 0 {
		c.CumulativeNotional = c.CumulativeQuantity * c.AveragePrice
	}
	if c.CumulativeNotional+math.Max(0.01, math.Abs(c.CumulativeNotional)*1e-9) < previousOrderNotional {
		return out, fmt.Errorf("AI exchange cumulative notional regressed for intent %d order %s", c.IntentID, fillKey)
	}
	out.DeltaQuantity = c.CumulativeQuantity - previousOrderFilled
	out.DeltaNotional = c.CumulativeNotional - previousOrderNotional
	if out.DeltaNotional <= 0 && out.DeltaQuantity > epsilon && c.AveragePrice > 0 {
		out.DeltaNotional = out.DeltaQuantity * c.AveragePrice
	}
	out.CumulativeQuantity = intentFilled + math.Max(0, out.DeltaQuantity)
	out.CumulativeNotional = intentNotional + math.Max(0, out.DeltaNotional)
	out.IntentStatus = ExecutionIntentPartiallyFilled
	if c.OrderTerminal {
		out.IntentStatus = ExecutionIntentFilled
	}
	if out.DeltaQuantity <= epsilon && status == ExecutionIntentProtected {
		// A terminal/duplicate venue snapshot after protection must not regress
		// the durable lifecycle back to FILLED. A positive later delta does
		// intentionally leave PROTECTED so the enlarged position is protected
		// again before returning to that state.
		out.IntentStatus = ExecutionIntentProtected
	}

	attemptStatus := ExecutionOrderAttemptPartiallyFilled
	if c.OrderTerminal && c.AttemptQuantity > 0 &&
		c.CumulativeQuantity+math.Max(1e-12, c.AttemptQuantity*1e-8) >= c.AttemptQuantity {
		attemptStatus = ExecutionOrderAttemptFilled
	}
	if strings.TrimSpace(c.ClientOrderID) != "" {
		res, updateErr := tx.Exec(`UPDATE copy_trade_execution_order_attempts SET
			status=?,exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,
			exchange_state=CASE WHEN ?<>'' THEN ? ELSE exchange_state END,
			filled_quantity=CASE WHEN ?>filled_quantity THEN ? ELSE filled_quantity END,
			last_error='',submitted_at=COALESCE(submitted_at,CURRENT_TIMESTAMP),
			filled_at=COALESCE(filled_at,CURRENT_TIMESTAMP),
			terminal_at=CASE WHEN ? THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE terminal_at END,
			updated_at=CURRENT_TIMESTAMP
			WHERE intent_id=? AND client_order_id=?`,
			attemptStatus, c.ExchangeOrderID, c.ExchangeOrderID, c.ExchangeState, c.ExchangeState,
			c.CumulativeQuantity, c.CumulativeQuantity, c.OrderTerminal, c.IntentID, c.ClientOrderID)
		if updateErr != nil {
			return out, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return out, fmt.Errorf("AI reentry intent %d has no matching durable order attempt", c.IntentID)
		}
	}
	if _, err = tx.Exec(`INSERT INTO copy_trade_execution_fill_commits(intent_id,fill_key,filled_quantity,filled_notional)
		VALUES(?,?,?,?) ON CONFLICT(intent_id,fill_key) DO UPDATE SET
		filled_quantity=excluded.filled_quantity,filled_notional=excluded.filled_notional,updated_at=CURRENT_TIMESTAMP`,
		c.IntentID, fillKey, c.CumulativeQuantity, c.CumulativeNotional); err != nil {
		return out, err
	}
	if out.DeltaQuantity <= epsilon {
		// Even a duplicate quantity snapshot may carry the terminal venue state.
		if _, err = tx.Exec(`UPDATE copy_trade_execution_intents SET status=?,reason_code=CASE WHEN ?='PARTIALLY_FILLED' THEN 'EXCHANGE_ORDER_PARTIAL' ELSE '' END,
			exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,exchange_state=?,
			terminal_at=CASE WHEN ? THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE terminal_at END,
			updated_at=CURRENT_TIMESTAMP WHERE id=?`,
			out.IntentStatus, out.IntentStatus, c.ExchangeOrderID, c.ExchangeOrderID,
			c.ExchangeState, c.OrderTerminal, c.IntentID); err != nil {
			return out, err
		}
		if err = tx.Commit(); err != nil {
			return out, err
		}
		return out, nil
	}
	if out.DeltaNotional <= 0 {
		return out, fmt.Errorf("AI reentry fill delta has no usable notional")
	}
	deltaPrice := out.DeltaNotional / out.DeltaQuantity

	var cycleStatus, mappingStatus string
	var cycleReentryCount int
	var cycleNotional float64
	if err = tx.QueryRow(`SELECT status,reentry_count,COALESCE(follower_notional,0) FROM copy_guard_cycles WHERE id=? AND trader_id=? AND leader_pos_id=?`,
		cycleID, c.TraderID, leaderPosID).Scan(&cycleStatus, &cycleReentryCount, &cycleNotional); err != nil {
		return out, err
	}
	if err = tx.QueryRow(`SELECT status FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`,
		c.TraderID, leaderPosID).Scan(&mappingStatus); err != nil {
		return out, err
	}
	out.FirstFill = cycleStatus == CopyGuardReentryPending && cycleReentryCount == attemptNo-1
	if out.FirstFill {
		if mappingStatus != MappingStatusStoppedByRisk {
			return out, fmt.Errorf("AI reentry mapping cannot activate from %s", mappingStatus)
		}
		res, updateErr := tx.Exec(`UPDATE copy_trade_position_mappings SET
			status='active',open_price=?,open_size_usd=?,last_known_size=?,stopped_at=NULL,updated_at=CURRENT_TIMESTAMP
			WHERE trader_id=? AND leader_pos_id=? AND status='stopped_by_risk'`,
			deltaPrice, out.DeltaNotional, c.LeaderTargetSize, c.TraderID, leaderPosID)
		if updateErr != nil {
			return out, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return out, fmt.Errorf("AI reentry mapping activation lost state race")
		}
		res, updateErr = tx.Exec(`UPDATE copy_guard_cycles SET
			status=?,reentry_count=?,follower_entry_price=?,follower_notional=?,
			entry_order_id=CASE WHEN ?<>'' THEN ? ELSE entry_order_id END,
			pending_since=NULL,updated_at=CURRENT_TIMESTAMP
			WHERE id=? AND status=? AND reentry_count=?`,
			CopyGuardFollowingReentry, attemptNo, deltaPrice, out.DeltaNotional,
			c.ExchangeOrderID, c.ExchangeOrderID, cycleID, CopyGuardReentryPending, attemptNo-1)
		if updateErr != nil {
			return out, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return out, fmt.Errorf("AI reentry cycle activation lost state race")
		}
		if _, err = tx.Exec(`INSERT INTO copy_guard_attempts
			(cycle_id,attempt_no,status,entry_price,quantity,notional,atr,entry_order_id)
			VALUES(?,?,'OPEN',?,?,?,?,?)
			ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET
				status='OPEN',entry_price=excluded.entry_price,quantity=excluded.quantity,
				notional=excluded.notional,atr=excluded.atr,
				entry_order_id=CASE WHEN excluded.entry_order_id<>'' THEN excluded.entry_order_id ELSE copy_guard_attempts.entry_order_id END,
				opened_at=CURRENT_TIMESTAMP,closed_at=NULL`,
			cycleID, attemptNo, deltaPrice, out.DeltaQuantity, out.DeltaNotional, c.ATR, c.ExchangeOrderID); err != nil {
			return out, err
		}
	} else {
		if cycleStatus != CopyGuardFollowingReentry || cycleReentryCount != attemptNo || mappingStatus != MappingStatusActive {
			return out, fmt.Errorf("AI reentry fill increment has incompatible lifecycle: cycle=%s/%d mapping=%s attempt=%d",
				cycleStatus, cycleReentryCount, mappingStatus, attemptNo)
		}
		if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET
			open_size_usd=COALESCE(open_size_usd,0)+?,last_known_size=?,updated_at=CURRENT_TIMESTAMP
			WHERE trader_id=? AND leader_pos_id=? AND status='active'`,
			out.DeltaNotional, c.LeaderTargetSize, c.TraderID, leaderPosID); err != nil {
			return out, err
		}
		if _, err = tx.Exec(`UPDATE copy_guard_cycles SET
			follower_entry_price=CASE WHEN follower_notional+?>0 THEN
				(follower_notional+?)/((CASE WHEN follower_entry_price>0 THEN follower_notional/follower_entry_price ELSE 0 END)+?) ELSE follower_entry_price END,
			follower_notional=follower_notional+?,
			entry_order_id=CASE WHEN ?<>'' THEN ? ELSE entry_order_id END,
			updated_at=CURRENT_TIMESTAMP WHERE id=?`,
			out.DeltaNotional, out.DeltaNotional, out.DeltaQuantity,
			out.DeltaNotional, c.ExchangeOrderID, c.ExchangeOrderID, cycleID); err != nil {
			return out, err
		}
		if _, err = tx.Exec(`UPDATE copy_guard_attempts SET
			entry_price=CASE WHEN quantity+?>0 THEN ((entry_price*quantity)+?)/(quantity+?) ELSE entry_price END,
			quantity=quantity+?,notional=notional+?,
			atr=CASE WHEN ?>0 THEN ? ELSE atr END,
			entry_order_id=CASE WHEN ?<>'' THEN ? ELSE entry_order_id END
			WHERE cycle_id=? AND attempt_no=? AND status='OPEN'`,
			out.DeltaQuantity, deltaPrice*out.DeltaQuantity, out.DeltaQuantity,
			out.DeltaQuantity, out.DeltaNotional, c.ATR, c.ATR,
			c.ExchangeOrderID, c.ExchangeOrderID, cycleID, attemptNo); err != nil {
			return out, err
		}
	}
	if _, err = tx.Exec(`UPDATE copy_trade_execution_intents SET
		status=?,filled_quantity=?,filled_notional=?,
		reason_code=CASE WHEN ?='PARTIALLY_FILLED' THEN 'EXCHANGE_ORDER_PARTIAL' ELSE '' END,
		last_error='',exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,
		exchange_state=?,filled_at=COALESCE(filled_at,CURRENT_TIMESTAMP),
		terminal_at=CASE WHEN ? THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE terminal_at END,
		updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		out.IntentStatus, out.CumulativeQuantity, out.CumulativeNotional, out.IntentStatus,
		c.ExchangeOrderID, c.ExchangeOrderID, c.ExchangeState,
		c.OrderTerminal, c.IntentID); err != nil {
		return out, err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`,
		out.IntentStatus, c.IntentID); err != nil {
		return out, err
	}
	eventType := "REENTRY_FILL_INCREMENT"
	if out.FirstFill {
		eventType = "REENTRY_FILLED"
	}
	eventMetadata := fmt.Sprintf(`{"intent_id":%d,"candidate_id":%d,"analysis_id":%d,"decision_generation":%d,"client_order_id":%q,"exchange_order_id":%q,"exchange_state":%q,"delta_quantity":%.12g,"cumulative_quantity":%.12g}`,
		c.IntentID, candidateID, analysisID, generation, c.ClientOrderID, c.ExchangeOrderID,
		c.ExchangeState, out.DeltaQuantity, c.CumulativeQuantity)
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,price,quantity,notional,metadata_json)
		VALUES(?,?,?,?,?,?,?)`, cycleID, c.TraderID, eventType, deltaPrice, out.DeltaQuantity, out.DeltaNotional, eventMetadata); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.mirrorGuardEventToCopyEvents(cycleID, c.TraderID, eventType, deltaPrice, out.DeltaQuantity, out.DeltaNotional, 0,
		map[string]interface{}{"intent_id": c.IntentID, "candidate_id": candidateID, "analysis_id": analysisID, "decision_generation": generation})
	return out, nil
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

type OrdinaryCatchupSettlement struct {
	IntentID   int64
	TraderID   string
	LeaderID   string
	ReasonCode string
	Detail     string
}

// SettleOrdinaryCatchupTransition is the source-revision acknowledgement
// boundary for an ordinary open/add catch-up that will no longer submit more
// quantity. It advances the mapping only after every durable order attempt is
// terminal, so a later leader close cannot be blocked by a dead revision and
// cannot race a late risk-increasing fill.
func (s *CopyTradeStore) SettleOrdinaryCatchupTransition(c OrdinaryCatchupSettlement) (string, error) {
	if c.IntentID <= 0 || strings.TrimSpace(c.TraderID) == "" {
		return "", fmt.Errorf("invalid ordinary catch-up settlement")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var leaderPosID, sourceKind, action, symbol, side, marginMode, intentStatus, exchangeOrderID string
	var sourceRevision int64
	var leaderTargetSize, filledQuantity float64
	if err = tx.QueryRow(`SELECT leader_pos_id,COALESCE(source_kind,'LEADER_TRANSITION'),action,
		COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),
		source_revision,COALESCE(leader_target_size,0),COALESCE(filled_quantity,0),
		status,COALESCE(exchange_order_id,'')
		FROM copy_trade_execution_intents WHERE id=? AND trader_id=?`,
		c.IntentID, c.TraderID).Scan(
		&leaderPosID, &sourceKind, &action, &symbol, &side, &marginMode,
		&sourceRevision, &leaderTargetSize, &filledQuantity, &intentStatus, &exchangeOrderID,
	); err != nil {
		return "", err
	}
	if sourceKind != "LEADER_TRANSITION" ||
		(action != "open_long" && action != "open_short") ||
		leaderPosID == "" || sourceRevision <= 0 || leaderTargetSize < 0 {
		return "", fmt.Errorf("intent %d is not an ordinary risk-increase catch-up", c.IntentID)
	}

	terminalStatus := ExecutionIntentSkipped
	if filledQuantity > 0 {
		terminalStatus = ExecutionIntentCompletedPartial
	}
	if intentStatus == terminalStatus {
		return terminalStatus, tx.Commit()
	}
	switch intentStatus {
	case ExecutionIntentReserved, ExecutionIntentSubmitted, ExecutionIntentReconciling,
		ExecutionIntentPartiallyFilled, ExecutionIntentFailed:
	default:
		return "", fmt.Errorf("intent %d cannot settle ordinary catch-up from %s", c.IntentID, intentStatus)
	}

	var attemptCount, unresolvedAttempts int
	var attemptFilled float64
	if err = tx.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN terminal_at IS NULL OR status NOT IN ('FILLED','FAILED','TERMINAL_NO_FILL') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(filled_quantity),0)
		FROM copy_trade_execution_order_attempts WHERE intent_id=?`, c.IntentID).
		Scan(&attemptCount, &unresolvedAttempts, &attemptFilled); err != nil {
		return "", err
	}
	tolerance := math.Max(1e-12, math.Abs(filledQuantity)*1e-8)
	if unresolvedAttempts != 0 ||
		(filledQuantity <= tolerance && attemptFilled > tolerance) ||
		(filledQuantity > tolerance && (attemptCount == 0 || attemptFilled+tolerance < filledQuantity)) ||
		(attemptCount == 0 && strings.TrimSpace(exchangeOrderID) != "") {
		return "", ErrOrdinaryCatchupUnsettled
	}

	var currentRevision int64
	var mappingStatus string
	mappingErr := tx.QueryRow(`SELECT COALESCE(source_revision,0),status
		FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`,
		c.TraderID, leaderPosID).Scan(&currentRevision, &mappingStatus)
	switch {
	case mappingErr == sql.ErrNoRows && sourceRevision == 1 && filledQuantity <= tolerance:
		if strings.TrimSpace(c.LeaderID) == "" {
			return "", fmt.Errorf("missing leader id for ignored initial catch-up")
		}
		_, err = tx.Exec(`INSERT INTO copy_trade_position_mappings
			(trader_id,leader_pos_id,leader_id,symbol,source_revision,side,margin_mode,status,
			 opened_at,open_price,open_size_usd,last_known_size,add_count,reduce_count,updated_at)
			VALUES(?,?,?,?,?,?,?,'ignored',CURRENT_TIMESTAMP,0,0,?,0,0,CURRENT_TIMESTAMP)`,
			c.TraderID, leaderPosID, c.LeaderID, symbol, sourceRevision, side, marginMode, leaderTargetSize)
	case mappingErr == nil && mappingStatus == MappingStatusActive && currentRevision == sourceRevision-1 && filledQuantity <= tolerance:
		_, err = tx.Exec(`UPDATE copy_trade_position_mappings
			SET source_revision=?,last_known_size=?,updated_at=CURRENT_TIMESTAMP
			WHERE trader_id=? AND leader_pos_id=? AND status='active' AND COALESCE(source_revision,0)=?`,
			sourceRevision, leaderTargetSize, c.TraderID, leaderPosID, sourceRevision-1)
	case mappingErr == nil && currentRevision == sourceRevision &&
		(mappingStatus == MappingStatusActive || mappingStatus == MappingStatusIgnored ||
			mappingStatus == MappingStatusStoppedByRisk || mappingStatus == MappingStatusDetached):
		// A confirmed partial fill already advanced the mapping. Only the
		// residual catch-up lifecycle remains to be terminalized.
	case mappingErr == nil && mappingStatus == MappingStatusClosed &&
		currentRevision == sourceRevision-1 && filledQuantity <= tolerance:
		// The authoritative close retired the mapping before this zero-fill
		// catch-up revision was acknowledged. Advance only the durable source
		// sequence so the next leader transition receives a fresh canonical
		// revision; never reopen the mapping or copy the stale catch-up target.
		_, err = tx.Exec(`UPDATE copy_trade_position_mappings
			SET source_revision=?,updated_at=CURRENT_TIMESTAMP
			WHERE trader_id=? AND leader_pos_id=? AND status='closed' AND COALESCE(source_revision,0)=?`,
			sourceRevision, c.TraderID, leaderPosID, sourceRevision-1)
	case mappingErr == nil && mappingStatus == MappingStatusClosed && currentRevision >= sourceRevision:
		// A later authoritative close already advanced and retired the mapping.
		// The old risk-increasing intent may still carry a terminal no-fill or
		// confirmed partial-fill residual. Terminate only that residual; never
		// reopen or rewind a closed mapping.
	case mappingErr != nil:
		return "", mappingErr
	default:
		return "", fmt.Errorf("ordinary catch-up mapping revision conflict: status=%s current=%d expected=%d",
			mappingStatus, currentRevision, sourceRevision)
	}
	if err != nil {
		return "", err
	}

	reasonCode := strings.TrimSpace(c.ReasonCode)
	if reasonCode == "" {
		if filledQuantity > tolerance {
			reasonCode = "CATCHUP_RESIDUAL_SUPERSEDED"
		} else {
			reasonCode = "CATCHUP_SKIPPED_NO_FILL"
		}
	}
	res, err := tx.Exec(`UPDATE copy_trade_execution_intents
		SET status=?,reason_code=?,last_error=?,last_catchup_reason=?,
		    terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND trader_id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED','FAILED')`,
		terminalStatus, reasonCode, c.Detail, reasonCode, c.IntentID, c.TraderID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", fmt.Errorf("ordinary catch-up intent %d lost state race", c.IntentID)
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`,
		terminalStatus, c.IntentID); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_risk_reservations
		SET status='RELEASED',released_at=CURRENT_TIMESTAMP
		WHERE intent_id=? AND status='ACTIVE'`, c.IntentID); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return terminalStatus, nil
}

// ReclaimUnsubmittedExecutionIntent returns an exact source transition to the
// executable RESERVED state after a fresh authoritative signal proves the
// transition is still current. It is intentionally limited to intents with no
// order attempt, submitted timestamp, exchange identity, or fill evidence.
// This closes the restart/manual-review deadlock without replaying uncertain
// exchange work.
func (s *CopyTradeStore) ReclaimUnsubmittedExecutionIntent(id int64, traderID, action string, leaderTargetSize float64) (bool, error) {
	if id <= 0 || strings.TrimSpace(traderID) == "" || strings.TrimSpace(action) == "" || leaderTargetSize < 0 {
		return false, fmt.Errorf("invalid execution intent reclaim")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE copy_trade_execution_intents SET
		status='RESERVED',reason_code='',last_error='',terminal_at=NULL,
		reconciliation_attempts=0,first_reconciling_at=NULL,updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND trader_id=? AND action=?
		  AND ABS(COALESCE(leader_target_size,0)-?)<=MAX(1e-12,ABS(?) * 1e-9)
		  AND status IN ('RECONCILING','FAILED')
		  AND reason_code IN (
			'SOURCE_REVALIDATION_REQUIRED','SOURCE_DATA_UNAVAILABLE','MIGRATION_RECONCILING',
			'SOURCE_VALUE_UNAVAILABLE','MANUAL_REVIEW_REQUIRED',
			'ORDER_LOOKUP_FAILED','ORDER_LOOKUP_UNAVAILABLE'
		  )
		  AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
		  AND COALESCE(filled_quantity,0)=0
		  AND NOT EXISTS (
			SELECT 1 FROM copy_trade_execution_order_attempts a
			WHERE a.intent_id=copy_trade_execution_intents.id
		  )`,
		id, traderID, action, leaderTargetSize, leaderTargetSize)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions
		SET status='RESERVED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, id); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *CopyTradeStore) MarkOrdinaryCatchupReconciliationPending(intentID int64, traderID, reasonCode, detail string) error {
	if intentID <= 0 || strings.TrimSpace(traderID) == "" {
		return fmt.Errorf("invalid ordinary catch-up reconciliation")
	}
	if strings.TrimSpace(reasonCode) == "" {
		reasonCode = "CATCHUP_RECONCILIATION_PENDING"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE copy_trade_execution_intents
		SET status='RECONCILING',reason_code=?,last_error=?,terminal_at=NULL,updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND trader_id=? AND source_kind='LEADER_TRANSITION'
		  AND action IN ('open_long','open_short')
		  AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED','FAILED')`,
		reasonCode, detail, intentID, traderID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("ordinary catch-up intent %d lost reconciliation race", intentID)
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='RECONCILING',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, intentID); err != nil {
		return err
	}
	return tx.Commit()
}

type ExecutionReconciliationReport struct {
	UnfinishedIntents          int `json:"unfinished_intents"`
	ManualReviewIntents        int `json:"manual_review_intents"`
	OpenCyclesWithoutMapping   int `json:"open_cycles_without_mapping"`
	RecoverableOwnershipGaps   int `json:"recoverable_ownership_gaps"`
	AmbiguousOwnershipGaps     int `json:"ambiguous_ownership_gaps"`
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
		{&report.UnfinishedIntents, `SELECT COUNT(*) FROM copy_trade_execution_intents WHERE trader_id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED')`},
		{&report.ManualReviewIntents, `SELECT COUNT(*) FROM copy_trade_execution_intents WHERE trader_id=? AND status='FAILED' AND reason_code='MANUAL_REVIEW_REQUIRED'`},
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
	gaps, err := s.ListOpenCycleOwnershipGaps(traderID)
	if err != nil {
		return report, err
	}
	for _, gap := range gaps {
		if gap.Recoverable {
			report.RecoverableOwnershipGaps++
		} else {
			report.AmbiguousOwnershipGaps++
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
	var cycleID int64
	if err = tx.QueryRow(`SELECT id FROM copy_guard_cycles
		WHERE trader_id=? AND leader_pos_id=? AND closed_at IS NULL ORDER BY id DESC LIMIT 1`,
		traderID, leaderPosID).Scan(&cycleID); err != nil && err != sql.ErrNoRows {
		return err
	}
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
		SET status=?,accounting_status=?,
		    accounting_error=?,closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND leader_pos_id=? AND closed_at IS NULL`, CopyGuardDetached, CopyGuardAccountingUnscorable, reasonCode, traderID, leaderPosID); err != nil {
		return err
	}
	if cycleID > 0 {
		if err = terminalizeCopyGuardAuxiliaryStateTx(tx, cycleID, CopyGuardDetached); err != nil {
			return err
		}
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
			target_quantity REAL DEFAULT 0,
			requested_quantity REAL DEFAULT 0,
			quantized_quantity REAL DEFAULT 0,
			quantity_step REAL DEFAULT 0,
			exchange_min_quantity REAL DEFAULT 0,
			exchange_min_notional REAL DEFAULT 0,
			minimum_executable_quantity REAL DEFAULT 0,
			filled_quantity REAL DEFAULT 0,
			filled_notional REAL DEFAULT 0,
			follower_equity_at_target REAL DEFAULT 0,
			target_account_pct REAL DEFAULT 0,
			catchup_deadline_at DATETIME,
			catchup_anchor_price REAL DEFAULT 0,
			last_catchup_reason TEXT DEFAULT '',
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
		{"target_quantity", "REAL DEFAULT 0"},
		{"filled_notional", "REAL DEFAULT 0"},
		{"follower_equity_at_target", "REAL DEFAULT 0"},
		{"target_account_pct", "REAL DEFAULT 0"},
		{"catchup_deadline_at", "DATETIME"},
		{"catchup_anchor_price", "REAL DEFAULT 0"},
		{"last_catchup_reason", "TEXT DEFAULT ''"},
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
	// This secondary index is fully derivable but is on the write path of every
	// execution-intent status transition. A production database from the legacy
	// pre-durable-attempt build was observed with two missing index entries:
	// reads still passed quick_check, while the next UPDATE failed with
	// SQLITE_CORRUPT_INDEX (779). Store.New already owns the process lock, so a
	// bounded rebuild here safely repairs that exact blocker before trading can
	// start without changing table data or schema.
	if _, err = s.db.Exec(`REINDEX idx_copy_intent_trader_status`); err != nil {
		return fmt.Errorf("rebuild execution intent status index: %w", err)
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
			quantity_kind TEXT NOT NULL DEFAULT 'INITIAL_OPEN',
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
	if err = ensureSQLiteColumn(s.db, "copy_trade_execution_order_attempts", "quantity_kind", "TEXT NOT NULL DEFAULT 'INITIAL_OPEN'"); err != nil {
		return fmt.Errorf("migrate copy_trade_execution_order_attempts.quantity_kind: %w", err)
	}
	if _, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_copy_order_attempt_intent ON copy_trade_execution_order_attempts(intent_id,attempt_no)`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_execution_fill_commits (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			intent_id INTEGER NOT NULL,
			fill_key TEXT NOT NULL,
			filled_quantity REAL NOT NULL,
			filled_notional REAL NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(intent_id,fill_key),
			FOREIGN KEY(intent_id) REFERENCES copy_trade_execution_intents(id) ON DELETE CASCADE
		)
	`); err != nil {
		return err
	}
	// SQLite rejects non-constant defaults in ALTER TABLE ADD COLUMN. Fresh
	// databases still receive the CREATE-table default above; legacy rows may
	// remain NULL until the first cumulative fill upsert explicitly updates it.
	if err = ensureSQLiteColumn(s.db, "copy_trade_execution_fill_commits", "updated_at", "DATETIME"); err != nil {
		return fmt.Errorf("migrate copy_trade_execution_fill_commits.updated_at: %w", err)
	}
	if _, err = s.db.Exec(`UPDATE copy_trade_execution_order_attempts
		SET status='FILLED',
			submitted_at=COALESCE(submitted_at,updated_at,CURRENT_TIMESTAMP),
			filled_at=COALESCE(filled_at,updated_at,CURRENT_TIMESTAMP),
			terminal_at=COALESCE(terminal_at,updated_at,CURRENT_TIMESTAMP),
			updated_at=CURRENT_TIMESTAMP
		WHERE status='UNKNOWN' AND COALESCE(filled_quantity,0)>0
		  AND UPPER(COALESCE(exchange_state,''))='FILLED'`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`UPDATE copy_trade_execution_order_attempts
		SET status='PARTIALLY_FILLED',
			submitted_at=COALESCE(submitted_at,updated_at,CURRENT_TIMESTAMP),
			filled_at=COALESCE(filled_at,updated_at,CURRENT_TIMESTAMP),
			terminal_at=COALESCE(terminal_at,updated_at,CURRENT_TIMESTAMP),
			updated_at=CURRENT_TIMESTAMP
		WHERE status='UNKNOWN' AND COALESCE(filled_quantity,0)>0
		  AND UPPER(COALESCE(exchange_state,'')) IN ('REJECTED','CANCELED','CANCELLED','EXPIRED','FAILED')`); err != nil {
		return err
	}
	if _, err = s.db.Exec(`UPDATE copy_trade_execution_order_attempts
		SET status='TERMINAL_NO_FILL',
			terminal_at=COALESCE(terminal_at,updated_at,CURRENT_TIMESTAMP),
			updated_at=CURRENT_TIMESTAMP
		WHERE status='UNKNOWN' AND COALESCE(filled_quantity,0)=0
		  AND UPPER(COALESCE(exchange_state,'')) IN ('REJECTED','CANCELED','CANCELLED','EXPIRED','FAILED')`); err != nil {
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
			 leader_target_size,requested_notional,target_quantity,requested_quantity,quantized_quantity,
			 follower_equity_at_target,target_account_pct,client_order_id,status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'RESERVED')
	`, intent.TraderID, intent.LeaderPosID, intent.SourceRevision, intent.SourceFillID, intent.SourceKind, intent.CanonicalKey,
		intent.CycleID, intent.CandidateID, intent.AnalysisID, intent.AttemptNo, intent.DecisionGeneration, intent.Action,
		intent.Symbol, intent.Side, intent.MarginMode, intent.LeaderTargetSize, intent.RequestedNotional,
		intent.TargetQuantity, intent.RequestedQuantity, intent.QuantizedQuantity,
		intent.FollowerEquityAtTarget, intent.TargetAccountPct, intent.ClientOrderID)
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
				target_quantity=?,requested_quantity=?,quantized_quantity=?,
				follower_equity_at_target=?,target_account_pct=?,client_order_id=?,
				status='RESERVED',reason_code='',last_error='',failure_counted=0,
				reconciliation_attempts=0,first_reconciling_at=NULL,terminal_at=NULL,updated_at=CURRENT_TIMESTAMP
			WHERE canonical_key=? AND canonical_key<>''
			  AND status='RECONCILING' AND reason_code IN ('SOURCE_REVALIDATION_REQUIRED','SOURCE_DATA_UNAVAILABLE','SOURCE_VALUE_UNAVAILABLE','MIGRATION_RECONCILING')
			  AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
			  AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)
		`, intent.SourceFillID, intent.Action, intent.CycleID, intent.CandidateID, intent.AnalysisID,
			intent.AttemptNo, intent.DecisionGeneration, intent.Symbol, intent.Side, intent.MarginMode,
			intent.LeaderTargetSize, intent.RequestedNotional, intent.TargetQuantity, intent.RequestedQuantity,
			intent.QuantizedQuantity, intent.FollowerEquityAtTarget, intent.TargetAccountPct,
			intent.ClientOrderID, intent.CanonicalKey)
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
				target_quantity=?,requested_quantity=?,quantized_quantity=?,
				follower_equity_at_target=?,target_account_pct=?,
				client_order_id=CASE WHEN client_order_id='' THEN ? ELSE client_order_id END,
				status='RESERVED',reason_code='',last_error='',failure_counted=0,
				reconciliation_attempts=0,first_reconciling_at=NULL,
				submitted_at=NULL,filled_at=NULL,protected_at=NULL,terminal_at=NULL,updated_at=CURRENT_TIMESTAMP
			WHERE trader_id=? AND leader_pos_id=? AND source_revision=? AND action=?
			  AND COALESCE(exchange_order_id,'')=''
			  AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)
			  AND (
			    (submitted_at IS NULL AND (
			      (status='FAILED' AND (reason_code IN ('PRE_SUBMIT','DECISION_CHANNEL_BUSY','STARTUP_REPLAY_REQUIRED') OR reason_code LIKE 'PRECHECK_%'))
			      OR (status='SKIPPED' AND reason_code IN ('RISK_CAP','MIN_NOTIONAL','SOURCE_SUPERSEDED') AND ?<>'' AND COALESCE(source_fill_id,'')<>?)
			      OR (status='RECONCILING' AND reason_code IN ('SOURCE_REVALIDATION_REQUIRED','SOURCE_DATA_UNAVAILABLE','SOURCE_VALUE_UNAVAILABLE','MIGRATION_RECONCILING'))
			    ))
			    OR (
			      status='FAILED' AND reason_code='EXECUTION_FAILED'
			      AND action IN ('close_long','close_short')
			      AND COALESCE(requested_quantity,0)=0 AND COALESCE(quantized_quantity,0)=0
			      AND COALESCE(filled_quantity,0)=0 AND COALESCE(exchange_state,'')=''
			      AND last_error IN (
			        'persist close long attempt: invalid execution order attempt',
			        'persist close short attempt: invalid execution order attempt'
			      )
			    )
			  )
		`, intent.SourceFillID, intent.CycleID, intent.CandidateID, intent.AnalysisID, intent.AttemptNo, intent.DecisionGeneration,
			intent.Symbol, intent.Side, intent.MarginMode, intent.LeaderTargetSize,
			intent.RequestedNotional, intent.TargetQuantity, intent.RequestedQuantity, intent.QuantizedQuantity,
			intent.FollowerEquityAtTarget, intent.TargetAccountPct, intent.ClientOrderID,
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
// and advances the parent intent to SUBMITTED in the same transaction, before
// the adapter is called. Quantity zero is the established close-all sentinel
// and is valid only for close actions. Reusing the same client id is idempotent.
func (s *CopyTradeStore) PrepareExecutionOrderAttempt(intentID int64, clientOrderID string, quantity float64) (*CopyTradeExecutionOrderAttempt, error) {
	return s.PrepareExecutionOrderAttemptWithQuantities(intentID, clientOrderID, quantity, quantity)
}

// PrepareExecutionOrderAttemptWithQuantities preserves both the raw requested
// quantity and the concrete quantized exchange quantity at the same durable
// submission boundary.
func (s *CopyTradeStore) PrepareExecutionOrderAttemptWithQuantities(intentID int64, clientOrderID string, requestedQuantity, quantizedQuantity float64) (*CopyTradeExecutionOrderAttempt, error) {
	return s.PrepareExecutionOrderAttemptWithKind(intentID, clientOrderID, "", requestedQuantity, quantizedQuantity)
}

func (s *CopyTradeStore) PrepareExecutionOrderAttemptWithKind(intentID int64, clientOrderID, quantityKind string, requestedQuantity, quantizedQuantity float64) (*CopyTradeExecutionOrderAttempt, error) {
	clientOrderID = strings.TrimSpace(clientOrderID)
	if intentID <= 0 || clientOrderID == "" ||
		requestedQuantity < 0 || math.IsNaN(requestedQuantity) || math.IsInf(requestedQuantity, 0) ||
		quantizedQuantity < 0 || math.IsNaN(quantizedQuantity) || math.IsInf(quantizedQuantity, 0) {
		return nil, fmt.Errorf("invalid execution order attempt")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var action, intentStatus string
	if err = tx.QueryRow(`SELECT action,status FROM copy_trade_execution_intents WHERE id=?`, intentID).Scan(&action, &intentStatus); err != nil {
		return nil, err
	}
	quantityKind = strings.ToUpper(strings.TrimSpace(quantityKind))
	if quantityKind == "" {
		switch action {
		case "open_long", "open_short":
			quantityKind = "INITIAL_OPEN"
		case "reduce_long", "reduce_short":
			quantityKind = "REDUCE"
		case "close_long", "close_short":
			quantityKind = "CLOSE"
		default:
			quantityKind = "UNKNOWN"
		}
	}
	closeAll := quantizedQuantity == 0 && (action == "close_long" || action == "close_short")
	if quantizedQuantity == 0 && !closeAll {
		return nil, fmt.Errorf("zero quantity execution order attempt is only valid for close actions")
	}
	if intentStatus != ExecutionIntentReserved && intentStatus != ExecutionIntentSubmitted && intentStatus != ExecutionIntentPartiallyFilled {
		return nil, fmt.Errorf("execution intent %d cannot prepare order attempt from %s", intentID, intentStatus)
	}
	var attemptNo int
	err = tx.QueryRow(`SELECT attempt_no FROM copy_trade_execution_order_attempts WHERE intent_id=? AND client_order_id=?`, intentID, clientOrderID).Scan(&attemptNo)
	if err == sql.ErrNoRows {
		if err = tx.QueryRow(`SELECT COALESCE(MAX(attempt_no),0)+1 FROM copy_trade_execution_order_attempts WHERE intent_id=?`, intentID).Scan(&attemptNo); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,quantity_kind,requested_quantity,quantized_quantity,status) VALUES(?,?,?,?,?,?,'PREPARED')`,
			intentID, attemptNo, clientOrderID, quantityKind, requestedQuantity, quantizedQuantity); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`UPDATE copy_trade_execution_intents SET
		status='SUBMITTED',
		requested_quantity=CASE WHEN requested_quantity<=0 AND ?>0 THEN ? ELSE requested_quantity END,
		quantized_quantity=CASE WHEN quantized_quantity<=0 AND ?>0 THEN ? ELSE quantized_quantity END,
		submitted_at=COALESCE(submitted_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN ('RESERVED','SUBMITTED','PARTIALLY_FILLED')`,
		requestedQuantity, requestedQuantity, quantizedQuantity, quantizedQuantity, intentID)
	if err != nil {
		return nil, err
	}
	if updated, rowsErr := res.RowsAffected(); rowsErr != nil {
		return nil, rowsErr
	} else if updated != 1 {
		return nil, fmt.Errorf("execution intent %d changed before order attempt preparation", intentID)
	}
	if _, err = tx.Exec(`UPDATE copy_trade_source_transitions SET status='SUBMITTED',updated_at=CURRENT_TIMESTAMP WHERE intent_id=?`, intentID); err != nil {
		return nil, err
	}
	row := tx.QueryRow(`SELECT id,intent_id,attempt_no,client_order_id,COALESCE(quantity_kind,''),requested_quantity,quantized_quantity,filled_quantity,exchange_order_id,exchange_state,status,last_error,created_at,updated_at,submitted_at,filled_at,terminal_at FROM copy_trade_execution_order_attempts WHERE intent_id=? AND client_order_id=?`, intentID, clientOrderID)
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
	orderTerminal := executionOrderAttemptTerminal(status, exchangeState)
	res, err := s.db.Exec(`UPDATE copy_trade_execution_order_attempts SET status=?,
		exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,
		exchange_state=CASE WHEN ?<>'' THEN ? ELSE exchange_state END,
		filled_quantity=CASE WHEN ?>filled_quantity THEN ? ELSE filled_quantity END,last_error=?,
		submitted_at=CASE WHEN ? IN ('SUBMITTED','PARTIALLY_FILLED','FILLED') THEN COALESCE(submitted_at,CURRENT_TIMESTAMP) ELSE submitted_at END,
		filled_at=CASE WHEN ? IN ('PARTIALLY_FILLED','FILLED') THEN COALESCE(filled_at,CURRENT_TIMESTAMP) ELSE filled_at END,
		terminal_at=CASE WHEN ? THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE terminal_at END,
		updated_at=CURRENT_TIMESTAMP WHERE intent_id=? AND client_order_id=?`,
		status, exchangeOrderID, exchangeOrderID, exchangeState, exchangeState, filledQuantity, filledQuantity, lastError,
		status, status, orderTerminal, intentID, clientOrderID)
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

func executionOrderAttemptTerminal(status, exchangeState string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case ExecutionOrderAttemptFilled, ExecutionOrderAttemptFailed, ExecutionOrderAttemptTerminalNoFill:
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(exchangeState)) {
	case "FILLED", "CANCELED", "CANCELLED", "REJECTED", "EXPIRED", "FAILED":
		return true
	default:
		return false
	}
}

func (s *CopyTradeStore) ListExecutionOrderAttempts(intentID int64) ([]*CopyTradeExecutionOrderAttempt, error) {
	rows, err := s.db.Query(`SELECT id,intent_id,attempt_no,client_order_id,COALESCE(quantity_kind,''),requested_quantity,quantized_quantity,filled_quantity,exchange_order_id,exchange_state,status,last_error,created_at,updated_at,submitted_at,filled_at,terminal_at FROM copy_trade_execution_order_attempts WHERE intent_id=? ORDER BY attempt_no`, intentID)
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
	if err := scanner.Scan(&x.ID, &x.IntentID, &x.AttemptNo, &x.ClientOrderID, &x.QuantityKind, &x.RequestedQuantity, &x.QuantizedQuantity,
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
		submitted_at,filled_at,protected_at,terminal_at,
		COALESCE(target_quantity,0),COALESCE(filled_notional,0),
		COALESCE(follower_equity_at_target,0),COALESCE(target_account_pct,0),
		catchup_deadline_at,COALESCE(catchup_anchor_price,0),COALESCE(last_catchup_reason,'')
		FROM copy_trade_execution_intents WHERE `+where+` ORDER BY CASE WHEN canonical_key=? THEN 0 ELSE 1 END LIMIT 1`, append(args, canonicalKey)...)
	return scanExecutionIntent(row)
}

func (s *CopyTradeStore) GetExecutionIntentByID(id int64) (*CopyTradeExecutionIntent, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid execution intent id")
	}
	row := s.db.QueryRow(`SELECT id,trader_id,leader_pos_id,source_revision,COALESCE(source_fill_id,''),action,
		COALESCE(source_kind,'LEADER_TRANSITION'),COALESCE(canonical_key,''),
		COALESCE(cycle_id,0),COALESCE(candidate_id,0),COALESCE(analysis_id,0),COALESCE(attempt_no,0),COALESCE(decision_generation,0),
		COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),COALESCE(leader_target_size,0),
		COALESCE(requested_notional,0),COALESCE(requested_quantity,0),COALESCE(quantized_quantity,0),
		COALESCE(quantity_step,0),COALESCE(exchange_min_quantity,0),COALESCE(exchange_min_notional,0),COALESCE(minimum_executable_quantity,0),
		COALESCE(filled_quantity,0),COALESCE(client_order_id,''),COALESCE(exchange_order_id,''),status,
		COALESCE(exchange_state,''),COALESCE(reason_code,''),COALESCE(last_error,''),COALESCE(failure_counted,0),created_at,updated_at,
		submitted_at,filled_at,protected_at,terminal_at,
		COALESCE(target_quantity,0),COALESCE(filled_notional,0),
		COALESCE(follower_equity_at_target,0),COALESCE(target_account_pct,0),
		catchup_deadline_at,COALESCE(catchup_anchor_price,0),COALESCE(last_catchup_reason,'')
		FROM copy_trade_execution_intents WHERE id=?`, id)
	return scanExecutionIntent(row)
}

// BindExecutionIntentCycle attaches an ordinary execution to the Copy Guard
// lifecycle created from its confirmed fill. It never rebinds an intent across
// cycles and keeps AI intents (which are born with a cycle id) unchanged.
func (s *CopyTradeStore) BindExecutionIntentCycle(intentID, cycleID int64) error {
	if intentID <= 0 || cycleID <= 0 {
		return nil
	}
	res, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET cycle_id=?,updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND (COALESCE(cycle_id,0)=0 OR cycle_id=?)`, cycleID, intentID, cycleID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("execution intent %d is already bound to another cycle", intentID)
	}
	return nil
}

func scanExecutionIntent(scanner interface{ Scan(...interface{}) error }) (*CopyTradeExecutionIntent, error) {
	var x CopyTradeExecutionIntent
	var created, updated string
	var submitted, filled, protected, terminal, catchupDeadline sql.NullString
	if err := scanner.Scan(&x.ID, &x.TraderID, &x.LeaderPosID, &x.SourceRevision, &x.SourceFillID, &x.Action,
		&x.SourceKind, &x.CanonicalKey,
		&x.CycleID, &x.CandidateID, &x.AnalysisID, &x.AttemptNo, &x.DecisionGeneration,
		&x.Symbol, &x.Side, &x.MarginMode, &x.LeaderTargetSize, &x.RequestedNotional, &x.RequestedQuantity,
		&x.QuantizedQuantity, &x.QuantityStep, &x.ExchangeMinQuantity, &x.ExchangeMinNotional, &x.MinimumExecutableQuantity,
		&x.FilledQuantity, &x.ClientOrderID, &x.ExchangeOrderID, &x.Status,
		&x.ExchangeState, &x.ReasonCode, &x.LastError, &x.FailureCounted, &created, &updated,
		&submitted, &filled, &protected, &terminal,
		&x.TargetQuantity, &x.FilledNotional, &x.FollowerEquityAtTarget, &x.TargetAccountPct,
		&catchupDeadline, &x.CatchupAnchorPrice, &x.LastCatchupReason); err != nil {
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
	if x.CatchupDeadlineAt, err = parseNullableDBTime(catchupDeadline); err != nil {
		return nil, err
	}
	if x.TargetQuantity <= 0 {
		x.TargetQuantity = x.QuantizedQuantity
		if x.TargetQuantity <= 0 {
			x.TargetQuantity = x.RequestedQuantity
		}
	}
	return &x, nil
}

func (s *CopyTradeStore) UpdateExecutionIntent(id int64, status, reasonCode, lastError, exchangeOrderID string, requestedQty, quantizedQty, filledQty float64) error {
	if id <= 0 || status == "" {
		return fmt.Errorf("invalid execution intent transition")
	}
	res, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET status=?,
		reason_code=CASE WHEN ?<>'' THEN ? ELSE reason_code END,last_error=?,
		last_catchup_reason=CASE WHEN ? LIKE 'CATCHUP_%' THEN ? ELSE last_catchup_reason END,
		exchange_order_id=CASE WHEN ?<>'' THEN ? ELSE exchange_order_id END,
		requested_quantity=CASE WHEN requested_quantity<=0 AND ?>0 THEN ? ELSE requested_quantity END,
		quantized_quantity=CASE WHEN quantized_quantity<=0 AND ?>0 THEN ? ELSE quantized_quantity END,
		filled_quantity=CASE WHEN ?>filled_quantity THEN ? ELSE filled_quantity END,
		submitted_at=CASE WHEN ?='SUBMITTED' THEN COALESCE(submitted_at,CURRENT_TIMESTAMP) ELSE submitted_at END,
		filled_at=CASE WHEN ? IN ('FILLED','COMPLETED_PARTIAL') THEN COALESCE(filled_at,CURRENT_TIMESTAMP) ELSE filled_at END,
		protected_at=CASE WHEN ?='PROTECTED' THEN COALESCE(protected_at,CURRENT_TIMESTAMP) ELSE protected_at END,
		terminal_at=CASE WHEN ? IN ('SKIPPED','FAILED','COMPLETED_PARTIAL','PROTECTED') THEN COALESCE(terminal_at,CURRENT_TIMESTAMP) ELSE terminal_at END,
		updated_at=CURRENT_TIMESTAMP WHERE id=? AND (`+validExecutionIntentTransitionSQL()+`)`, status, reasonCode, reasonCode, lastError,
		reasonCode, reasonCode,
		exchangeOrderID, exchangeOrderID, requestedQty, requestedQty, quantizedQty, quantizedQty, filledQty, filledQty,
		status, status, status, status, id, status, status, status, status, status, status)
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

// finalizeExecutionIntentTerminalMigration runs after mapping and Copy Guard
// tables exist. Historical PROTECTED rows are always terminal. A historical
// FILLED row is terminal only when its surrounding ownership/cycle is already
// closed; active unprotected opens deliberately remain non-terminal so stop
// reconciliation cannot hide exchange risk.
func (s *CopyTradeStore) finalizeExecutionIntentTerminalMigration() error {
	if _, err := s.db.Exec(`UPDATE copy_trade_execution_intents
		SET protected_at=COALESCE(protected_at,filled_at,updated_at,CURRENT_TIMESTAMP),
		    terminal_at=COALESCE(terminal_at,protected_at,filled_at,updated_at,CURRENT_TIMESTAMP),
		    updated_at=CURRENT_TIMESTAMP
		WHERE status='PROTECTED' AND terminal_at IS NULL`); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE copy_trade_execution_intents AS i
		SET terminal_at=COALESCE(i.terminal_at,i.filled_at,i.updated_at,CURRENT_TIMESTAMP),
		    updated_at=CURRENT_TIMESTAMP
		WHERE i.status='FILLED' AND i.terminal_at IS NULL
		  AND UPPER(COALESCE(i.exchange_state,''))='FILLED'
		  AND (
			LOWER(i.action) IN ('close_long','close_short','reduce_long','reduce_short')
			OR EXISTS (
				SELECT 1 FROM copy_guard_cycles c
				WHERE c.id=i.cycle_id AND c.closed_at IS NOT NULL
			)
			OR EXISTS (
				SELECT 1 FROM copy_trade_position_mappings m
				WHERE m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id
				  AND m.status<>'active'
			)
		  )`)
	return err
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
		  AND status='RECONCILING'
		  AND reason_code IN ('SOURCE_REVALIDATION_REQUIRED','SOURCE_DATA_UNAVAILABLE','SOURCE_VALUE_UNAVAILABLE','MIGRATION_RECONCILING')
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
		target_quantity=CASE WHEN target_quantity<=0 AND ?>0 THEN ? ELSE target_quantity END,
		requested_quantity=CASE WHEN requested_quantity<=0 THEN ? ELSE requested_quantity END,
		quantized_quantity=CASE WHEN quantized_quantity<=0 AND ?>0 THEN ? ELSE quantized_quantity END,
		quantity_step=?,
		exchange_min_quantity=?,exchange_min_notional=?,minimum_executable_quantity=?,
		updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		quantized, quantized, requested, quantized, quantized, step,
		exchangeMinQuantity, exchangeMinNotional, minimumExecutable, id)
	return err
}

func (s *CopyTradeStore) InitializeExecutionCatchupPolicy(id int64, deadline time.Time, anchorPrice float64) error {
	if id <= 0 || deadline.IsZero() || anchorPrice <= 0 {
		return fmt.Errorf("invalid execution catch-up policy")
	}
	_, err := s.db.Exec(`UPDATE copy_trade_execution_intents SET
		catchup_deadline_at=COALESCE(catchup_deadline_at,?),
		catchup_anchor_price=CASE WHEN catchup_anchor_price<=0 THEN ? ELSE catchup_anchor_price END,
		updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		deadline.UTC(), anchorPrice, id)
	return err
}

func validExecutionIntentTransitionSQL() string {
	return `(status=? OR
		(status='RESERVED' AND ? IN ('SUBMITTED','FILLED','PARTIALLY_FILLED','COMPLETED_PARTIAL','SKIPPED','FAILED','RECONCILING')) OR
		(status='SUBMITTED' AND ? IN ('FILLED','PARTIALLY_FILLED','COMPLETED_PARTIAL','SKIPPED','FAILED','RECONCILING')) OR
		(status='RECONCILING' AND ? IN ('FILLED','PARTIALLY_FILLED','COMPLETED_PARTIAL','SKIPPED','FAILED','RECONCILING')) OR
		(status='PARTIALLY_FILLED' AND ? IN ('SUBMITTED','FILLED','PARTIALLY_FILLED','COMPLETED_PARTIAL','FAILED','RECONCILING')) OR
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
		submitted_at,filled_at,protected_at,terminal_at,
		COALESCE(target_quantity,0),COALESCE(filled_notional,0),
		COALESCE(follower_equity_at_target,0),COALESCE(target_account_pct,0),
		catchup_deadline_at,COALESCE(catchup_anchor_price,0),COALESCE(last_catchup_reason,'')
		FROM copy_trade_execution_intents WHERE trader_id=? AND status IN ('RESERVED','SUBMITTED','RECONCILING','PARTIALLY_FILLED') ORDER BY id`, traderID)
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

// ListRecoverableOrdinaryCatchupIntents returns only terminal catch-up states
// that may be source-revision blockers. SettleOrdinaryCatchupTransition still
// performs the authoritative attempt-side-effect checks before changing them.
func (s *CopyTradeStore) ListRecoverableOrdinaryCatchupIntents(traderID string) ([]*CopyTradeExecutionIntent, error) {
	rows, err := s.db.Query(`SELECT id,trader_id,leader_pos_id,source_revision,COALESCE(source_fill_id,''),action,
		COALESCE(source_kind,'LEADER_TRANSITION'),COALESCE(canonical_key,''),
		COALESCE(cycle_id,0),COALESCE(candidate_id,0),COALESCE(analysis_id,0),COALESCE(attempt_no,0),COALESCE(decision_generation,0),
		COALESCE(symbol,''),COALESCE(side,''),COALESCE(margin_mode,''),COALESCE(leader_target_size,0),
		COALESCE(requested_notional,0),COALESCE(requested_quantity,0),COALESCE(quantized_quantity,0),
		COALESCE(quantity_step,0),COALESCE(exchange_min_quantity,0),COALESCE(exchange_min_notional,0),COALESCE(minimum_executable_quantity,0),
		COALESCE(filled_quantity,0),COALESCE(client_order_id,''),COALESCE(exchange_order_id,''),status,
		COALESCE(exchange_state,''),COALESCE(reason_code,''),COALESCE(last_error,''),COALESCE(failure_counted,0),created_at,updated_at,
		submitted_at,filled_at,protected_at,terminal_at,
		COALESCE(target_quantity,0),COALESCE(filled_notional,0),
		COALESCE(follower_equity_at_target,0),COALESCE(target_account_pct,0),
		catchup_deadline_at,COALESCE(catchup_anchor_price,0),COALESCE(last_catchup_reason,'')
		FROM copy_trade_execution_intents i
		WHERE i.trader_id=? AND i.source_kind='LEADER_TRANSITION'
		  AND i.action IN ('open_long','open_short')
		  AND i.status='FAILED' AND i.reason_code LIKE 'CATCHUP_%'
		  AND (
			(i.source_revision=1 AND NOT EXISTS (
				SELECT 1 FROM copy_trade_position_mappings m
				WHERE m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id))
			OR EXISTS (
				SELECT 1 FROM copy_trade_position_mappings m
					WHERE m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id
					  AND (
						(m.status='active' AND COALESCE(m.source_revision,0)=i.source_revision-1)
						OR (m.status IN ('active','ignored','stopped_by_risk','detached')
							AND COALESCE(m.source_revision,0)=i.source_revision)
						OR (m.status='closed' AND (
							COALESCE(m.source_revision,0)>=i.source_revision
							OR (COALESCE(m.source_revision,0)=i.source_revision-1
								AND COALESCE(i.filled_quantity,0)=0)
						))
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
		i.submitted_at,i.filled_at,i.protected_at,i.terminal_at,
		COALESCE(i.target_quantity,0),COALESCE(i.filled_notional,0),
		COALESCE(i.follower_equity_at_target,0),COALESCE(i.target_account_pct,0),
		i.catchup_deadline_at,COALESCE(i.catchup_anchor_price,0),COALESCE(i.last_catchup_reason,'')
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
		submitted_at,filled_at,protected_at,terminal_at,
		COALESCE(target_quantity,0),COALESCE(filled_notional,0),
		COALESCE(follower_equity_at_target,0),COALESCE(target_account_pct,0),
		catchup_deadline_at,COALESCE(catchup_anchor_price,0),COALESCE(last_catchup_reason,'')
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
