package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	TraderLifecycleStarting                  = "STARTING"
	TraderLifecycleRunning                   = "RUNNING"
	TraderLifecycleStopping                  = "STOPPING"
	TraderLifecycleStoppingReconcileRequired = "STOPPING_RECONCILE_REQUIRED"
	TraderLifecycleStopped                   = "STOPPED"
	TraderLifecycleArchiving                 = "ARCHIVING"
	TraderLifecycleArchived                  = "ARCHIVED"
)

const (
	ReentryCandidatePausedByTrader            = "PAUSED_BY_TRADER"
	ReentryCandidateInvalidatedTraderArchived = "INVALIDATED_TRADER_ARCHIVED"
)

// Blocker codes consumed outside this package are named so a rename cannot
// silently turn a consumer into a no-op. copytrade drives its stop-time
// protection bookkeeping repair off TraderLifecycleBlockerUnprotectedFill.
const (
	TraderLifecycleBlockerExecutionReconcile = "EXECUTION_RECONCILE_REQUIRED"
	TraderLifecycleBlockerUnprotectedFill    = "UNPROTECTED_FILL_UNRESOLVED"
)

var (
	ErrTraderArchived          = errors.New("trader is archived")
	ErrTraderLifecycleConflict = errors.New("trader lifecycle transition conflict")
)

type TraderLifecycle struct {
	TraderID   string `json:"trader_id"`
	Status     string `json:"lifecycle_status"`
	Generation int64  `json:"lifecycle_generation"`
	IsRunning  bool   `json:"is_running"`
}

type TraderLifecycleBlocker struct {
	Code       string `json:"code"`
	ResourceID string `json:"resource_id"`
	Symbol     string `json:"symbol,omitempty"`
	Status     string `json:"status"`
}

func assignTraderLifecycleTimes(t *Trader, stoppedAt, archivedAt sql.NullString) {
	if t == nil {
		return
	}
	if parsed, err := parseNullableDBTime(stoppedAt); err == nil {
		t.StoppedAt = parsed
	}
	if parsed, err := parseNullableDBTime(archivedAt); err == nil {
		t.ArchivedAt = parsed
	}
}

func (s *TraderStore) initLifecycleTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS trader_lifecycle_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			from_status TEXT NOT NULL,
			to_status TEXT NOT NULL,
			reason_code TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(trader_id,generation,to_status)
		);
		CREATE INDEX IF NOT EXISTS idx_trader_lifecycle_events_trader
			ON trader_lifecycle_events(trader_id,generation,created_at);
		CREATE TABLE IF NOT EXISTS trader_tombstones (
			trader_id TEXT PRIMARY KEY,
			lifecycle_status TEXT NOT NULL DEFAULT 'ARCHIVED',
			reason_code TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	return err
}

func recordTraderLifecycleEventTx(tx *sql.Tx, traderID string, generation int64, from, to, reason, detail string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO trader_lifecycle_events
		(trader_id,generation,from_status,to_status,reason_code,detail)
		VALUES(?,?,?,?,?,?)`, traderID, generation, from, to, reason, detail)
	return err
}

func (s *TraderStore) GetLifecycle(traderID string) (*TraderLifecycle, error) {
	var state TraderLifecycle
	state.TraderID = traderID
	err := s.db.QueryRow(`SELECT lifecycle_status,lifecycle_generation,is_running
		FROM traders WHERE id=?`, traderID).Scan(&state.Status, &state.Generation, &state.IsRunning)
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// GetLifecycleForUser is the ownership-safe lifecycle lookup used by stop and
// archive paths. Those operations must remain available even when an AI model,
// exchange, or strategy row was removed and GetFullConfig can no longer join a
// complete runtime configuration.
func (s *TraderStore) GetLifecycleForUser(userID, traderID string) (*TraderLifecycle, error) {
	var state TraderLifecycle
	state.TraderID = traderID
	err := s.db.QueryRow(`SELECT lifecycle_status,lifecycle_generation,is_running
		FROM traders WHERE id=? AND user_id=?`, traderID, userID).
		Scan(&state.Status, &state.Generation, &state.IsRunning)
	if err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *TraderStore) IsGenerationRunning(traderID string, generation int64) bool {
	var allowed int
	err := s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM traders
		WHERE id=? AND lifecycle_status='RUNNING' AND lifecycle_generation=? AND is_running=1
	)`, traderID, generation).Scan(&allowed)
	return err == nil && allowed == 1
}

func (s *TraderStore) BeginStart(userID, traderID string) (*TraderLifecycle, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var from string
	var generation int64
	if err = tx.QueryRow(`SELECT lifecycle_status,lifecycle_generation FROM traders
		WHERE id=? AND user_id=?`, traderID, userID).Scan(&from, &generation); err != nil {
		return nil, err
	}
	if from == TraderLifecycleArchived || from == TraderLifecycleArchiving {
		return nil, ErrTraderArchived
	}
	if from == TraderLifecycleRunning {
		return &TraderLifecycle{TraderID: traderID, Status: from, Generation: generation, IsRunning: true}, tx.Commit()
	}
	// STOPPING / STOPPING_RECONCILE_REQUIRED are startable on purpose. Startup
	// recovery (execution intent recovery, Copy Guard cycle recovery, margin stop
	// reconciliation) is precisely the machinery that resolves unsettled work, and
	// it needs the runtime up: requiring STOPPED first inverts the dependency and
	// turns any unclearable stop blocker into a trader with no operator action
	// left. The generation bump below fences every callback from the prior
	// generation, so the previous runtime cannot mutate state after this point.
	if from != TraderLifecycleStopped &&
		from != TraderLifecycleStopping &&
		from != TraderLifecycleStoppingReconcileRequired {
		return nil, fmt.Errorf("%w: cannot start from %s", ErrTraderLifecycleConflict, from)
	}
	generation++
	if _, err = tx.Exec(`UPDATE traders SET lifecycle_status=?,lifecycle_generation=?,
		is_running=0,stopped_at=NULL WHERE id=? AND user_id=? AND lifecycle_status=?`,
		TraderLifecycleStarting, generation, traderID, userID, from); err != nil {
		return nil, err
	}
	if err = recordTraderLifecycleEventTx(tx, traderID, generation, from, TraderLifecycleStarting, "OPERATOR_START", "startup recovery pending"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &TraderLifecycle{TraderID: traderID, Status: TraderLifecycleStarting, Generation: generation}, nil
}

func (s *TraderStore) CompleteStart(userID, traderID string, generation int64) error {
	return s.completeStart(userID, traderID, generation, "", false)
}

// CompleteCopyGuardStart performs the account exclusivity check and RUNNING
// transition in one SQLite transaction. The API also checks before save for a
// clearer error; this is the authoritative race-safe startup barrier.
func (s *TraderStore) CompleteCopyGuardStart(userID, traderID string, generation int64, exchangeID string) error {
	return s.completeStart(userID, traderID, generation, exchangeID, true)
}

func (s *TraderStore) completeStart(userID, traderID string, generation int64, exchangeID string, copyGuardExclusive bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if strings.TrimSpace(exchangeID) == "" {
		if err = tx.QueryRow(`SELECT exchange_id FROM traders WHERE id=? AND user_id=?`, traderID, userID).Scan(&exchangeID); err != nil {
			return err
		}
	}
	if strings.TrimSpace(exchangeID) != "" {
		var conflict string
		err = tx.QueryRow(`SELECT t.id
			FROM traders t
			LEFT JOIN copy_trade_configs c ON c.trader_id=t.id
			LEFT JOIN copy_guard_policies p ON p.trader_id=t.id
			WHERE t.exchange_id=? AND t.id<>?
			  AND t.lifecycle_status='RUNNING' AND t.is_running=1
			  AND (? OR (
				(p.trader_id IS NOT NULL AND c.enabled=1
				 AND COALESCE(c.risk_stop_loss_enabled,1)=1
				 AND c.provider_type IN ('okx','binance'))
				OR EXISTS (SELECT 1 FROM copy_guard_cycles active_cycle
					WHERE active_cycle.trader_id=t.id AND active_cycle.closed_at IS NULL)
			  ))
			ORDER BY t.id LIMIT 1`, exchangeID, traderID, copyGuardExclusive).Scan(&conflict)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if conflict != "" {
			if copyGuardExclusive {
				return fmt.Errorf("execution account already has running trader %s and cannot be assigned to Copy Guard", conflict)
			}
			return fmt.Errorf("execution account is already controlled by Copy Guard trader %s", conflict)
		}
	}
	res, err := tx.Exec(`UPDATE traders SET lifecycle_status=?,is_running=1
		WHERE id=? AND user_id=? AND lifecycle_status=? AND lifecycle_generation=?`,
		TraderLifecycleRunning, traderID, userID, TraderLifecycleStarting, generation)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrTraderLifecycleConflict
	}
	if err = recordTraderLifecycleEventTx(tx, traderID, generation, TraderLifecycleStarting, TraderLifecycleRunning, "STARTUP_RECOVERED", "runtime started after reconciliation"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *TraderStore) FailStart(userID, traderID string, generation int64, detail string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE traders SET lifecycle_status=?,is_running=0,stopped_at=CURRENT_TIMESTAMP
		WHERE id=? AND user_id=? AND lifecycle_status=? AND lifecycle_generation=?`,
		TraderLifecycleStopped, traderID, userID, TraderLifecycleStarting, generation)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTraderLifecycleConflict
	}
	if err = recordTraderLifecycleEventTx(tx, traderID, generation, TraderLifecycleStarting, TraderLifecycleStopped, "START_FAILED", detail); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *TraderStore) BeginStop(userID, traderID string) (*TraderLifecycle, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var from string
	var generation int64
	if err = tx.QueryRow(`SELECT lifecycle_status,lifecycle_generation FROM traders
		WHERE id=? AND user_id=?`, traderID, userID).Scan(&from, &generation); err != nil {
		return nil, err
	}
	switch from {
	case TraderLifecycleArchived, TraderLifecycleArchiving:
		return nil, ErrTraderArchived
	case TraderLifecycleStopped, TraderLifecycleStopping, TraderLifecycleStoppingReconcileRequired:
		return &TraderLifecycle{TraderID: traderID, Status: from, Generation: generation}, tx.Commit()
	case TraderLifecycleRunning, TraderLifecycleStarting:
	default:
		return nil, fmt.Errorf("%w: cannot stop from %s", ErrTraderLifecycleConflict, from)
	}
	generation++
	res, err := tx.Exec(`UPDATE traders SET lifecycle_status=?,lifecycle_generation=?,is_running=0
		WHERE id=? AND user_id=? AND lifecycle_status=?`,
		TraderLifecycleStopping, generation, traderID, userID, from)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrTraderLifecycleConflict
	}
	if err = recordTraderLifecycleEventTx(tx, traderID, generation, from, TraderLifecycleStopping, "OPERATOR_STOP", "new risk disabled; submitted work must reconcile"); err != nil {
		return nil, err
	}
	if err = pauseTraderRiskIncreaseTx(tx, traderID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &TraderLifecycle{TraderID: traderID, Status: TraderLifecycleStopping, Generation: generation}, nil
}

func pauseTraderRiskIncreaseTx(tx *sql.Tx, traderID string) error {
	if _, err := tx.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=?,pending_trigger='TRADER_STOPPED',last_error='paused by trader stop',
		    next_review_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND status IN ('WATCHING','WAITING','DORMANT_REARM','REVIEWING')`,
		ReentryCandidatePausedByTrader, traderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=?,pending_trigger='TRADER_STOPPED',last_error='paused before exchange submission',
		    next_review_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND status='ENTRY_PENDING'
		  AND NOT EXISTS (
			SELECT 1 FROM copy_trade_execution_intents i
			WHERE i.candidate_id=copy_guard_reentry_candidates.id
			  AND (i.submitted_at IS NOT NULL OR COALESCE(i.exchange_order_id,'')<>''
			       OR COALESCE(i.filled_quantity,0)>0
			       OR EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id))
		  )`, ReentryCandidatePausedByTrader, traderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_trade_source_transitions SET status='TRADER_STOPPED_PRE_SUBMIT',updated_at=CURRENT_TIMESTAMP
		WHERE intent_id IN (
			SELECT id FROM copy_trade_execution_intents
			WHERE trader_id=? AND action IN ('open_long','open_short')
			  AND status IN ('RESERVED','RECONCILING','FAILED')
			  AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
			  AND COALESCE(filled_quantity,0)=0
			  AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)
		)`, traderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_trade_execution_intents
		SET status='SKIPPED',reason_code='TRADER_STOPPED_PRE_SUBMIT',
		    last_error='trader stopped before exchange submission',terminal_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND action IN ('open_long','open_short')
		  AND status IN ('RESERVED','RECONCILING','FAILED')
		  AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
		  AND COALESCE(filled_quantity,0)=0
		  AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)`,
		traderID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE copy_guard_risk_reservations SET status='RELEASED',released_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND status='ACTIVE' AND intent_id IN (
			SELECT id FROM copy_trade_execution_intents
			WHERE trader_id=? AND status='SKIPPED' AND reason_code='TRADER_STOPPED_PRE_SUBMIT'
		)`, traderID, traderID)
	return err
}

func (s *TraderStore) CompleteStop(userID, traderID string, generation int64) error {
	return s.finishStop(userID, traderID, generation, TraderLifecycleStopped, "STOP_RECONCILED", "submitted work reconciled; runtime silent")
}

func (s *TraderStore) MarkStopReconcileRequired(userID, traderID string, generation int64, detail string) error {
	return s.finishStop(userID, traderID, generation, TraderLifecycleStoppingReconcileRequired, "STOP_RECONCILE_REQUIRED", detail)
}

func (s *TraderStore) finishStop(userID, traderID string, generation int64, target, reason, detail string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var from string
	if err = tx.QueryRow(`SELECT lifecycle_status FROM traders WHERE id=? AND user_id=? AND lifecycle_generation=?`,
		traderID, userID, generation).Scan(&from); err != nil {
		return err
	}
	if from == target {
		return tx.Commit()
	}
	if from != TraderLifecycleStopping && from != TraderLifecycleStoppingReconcileRequired {
		return ErrTraderLifecycleConflict
	}
	res, err := tx.Exec(`UPDATE traders SET lifecycle_status=?,is_running=0,stopped_at=CURRENT_TIMESTAMP
		WHERE id=? AND user_id=? AND lifecycle_generation=? AND lifecycle_status=?`,
		target, traderID, userID, generation, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrTraderLifecycleConflict
	}
	if err = recordTraderLifecycleEventTx(tx, traderID, generation, from, target, reason, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// GetStopBlockers lists execution intents whose exchange outcome is still
// unknown and therefore prevent the trader from reaching STOPPED.
//
// Scope is deliberately narrow: only work that was (or may have been) submitted
// without a confirmed outcome belongs here. Such an intent is resolvable while
// the trader is stopped, because handleReconcileTrader looks the order up on the
// venue and terminalizes it. Risk that is already *settled* — a confirmed fill we
// know we own — is not a stop blocker but an archive blocker: STOPPED explicitly
// permits retained real positions, archive is the gate that demands flat.
//
// Every blocker must correspond to real risk AND have a reachable exit. A
// blocker that can never clear is its own outage: the trader freezes in
// STOPPING_RECONCILE_REQUIRED, which also removes start (BeginStart used to
// require STOPPED) and archive, leaving no operator action at all.
func (s *TraderStore) GetStopBlockers(traderID string) ([]TraderLifecycleBlocker, error) {
	rows, err := s.db.Query(`SELECT CAST(i.id AS TEXT),COALESCE(i.symbol,''),i.status
		FROM copy_trade_execution_intents i
		WHERE i.trader_id=?
		  AND i.terminal_at IS NULL AND (
			i.status IN ('SUBMITTED','PARTIALLY_FILLED')
			OR (
				-- RECONCILING is grouped with RESERVED/FAILED rather than treated
				-- as unconditional risk: an intent can be moved to RECONCILING
				-- before it ever reaches the exchange, and such an intent has
				-- nothing to reconcile while permanently blocking the stop.
				i.status IN ('RESERVED','FAILED','RECONCILING')
				AND (
					i.submitted_at IS NOT NULL
					OR COALESCE(i.exchange_order_id,'')<>''
					OR COALESCE(i.filled_quantity,0)>0
					OR EXISTS (
						SELECT 1 FROM copy_trade_execution_order_attempts a
						WHERE a.intent_id=i.id AND (
							a.submitted_at IS NOT NULL
							OR COALESCE(a.exchange_order_id,'')<>''
							OR COALESCE(a.filled_quantity,0)>0
							OR a.status IN ('SUBMITTED','PARTIALLY_FILLED','FILLED','UNKNOWN')
						)
					)
				)
			)
		)
		ORDER BY i.id`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blockers []TraderLifecycleBlocker
	for rows.Next() {
		var b TraderLifecycleBlocker
		b.Code = TraderLifecycleBlockerExecutionReconcile
		if err = rows.Scan(&b.ResourceID, &b.Symbol, &b.Status); err != nil {
			return nil, err
		}
		blockers = append(blockers, b)
	}
	return blockers, rows.Err()
}

// GetUnprotectedFillBlockers lists confirmed opening fills that never reached a
// verified protective stop. This is archive-only risk, never a stop blocker.
//
// Refusing STOPPED for these positions protected nothing — the Copy Guard monitor
// that could arm a stop has already exited — while it did block the restart that
// would bring that monitor back, and it blocked archive. Worse, its exemptions
// were unreachable for intents Copy Guard never managed: a follower running with
// stop loss off gets cycle_id=0, and both cycle-based exemptions join
// copy_guard_cycles.id=i.cycle_id, where no row can ever have id 0.
//
// Archive keeps blocking such positions through this check plus
// OWNERSHIP_MAPPING_ACTIVE, OPEN_POSITION and the fresh exchange snapshot in
// ReconcileStoppedTrader, so nothing is silently lost by moving it here.
//
// Exported because copy trading's stop reconciliation repairs exactly this set:
// it re-derives protection from live exchange orders and backfills the intent
// bookkeeping the FILLED->PROTECTED transition missed.
func (s *TraderStore) GetUnprotectedFillBlockers(traderID string) ([]TraderLifecycleBlocker, error) {
	rows, err := s.db.Query(`SELECT CAST(i.id AS TEXT),COALESCE(i.symbol,''),i.status
		FROM copy_trade_execution_intents i
		WHERE i.trader_id=?
		  AND i.status='FILLED'
		  AND LOWER(i.action) IN ('open_long','open_short')
		  AND i.protected_at IS NULL
		  AND (
			COALESCE(i.filled_quantity,0)>0
			OR i.filled_at IS NOT NULL
			OR UPPER(COALESCE(i.exchange_state,''))='FILLED'
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM copy_guard_cycles c
			WHERE c.id=i.cycle_id AND c.closed_at IS NOT NULL
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM copy_trade_position_mappings m
			WHERE m.trader_id=i.trader_id AND m.leader_pos_id=i.leader_pos_id
			  AND m.status<>'active'
		  )
		  -- protected_at is written only by the FILLED->PROTECTED transition, so a
		  -- protection established on a later retry leaves it NULL forever. Trust
		  -- the cycle's verified protection state instead of the intent's
		  -- bookkeeping column.
		  AND NOT EXISTS (
			SELECT 1 FROM copy_guard_cycles c
			WHERE c.id=i.cycle_id
			  AND c.protection_status IN ('VERIFIED','CLAMPED')
			  AND COALESCE(c.protection_coverage,0)>=0.999
		  )
		ORDER BY i.id`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blockers []TraderLifecycleBlocker
	for rows.Next() {
		var b TraderLifecycleBlocker
		b.Code = TraderLifecycleBlockerUnprotectedFill
		if err = rows.Scan(&b.ResourceID, &b.Symbol, &b.Status); err != nil {
			return nil, err
		}
		blockers = append(blockers, b)
	}
	return blockers, rows.Err()
}

func (s *TraderStore) GetArchiveBlockers(traderID string) ([]TraderLifecycleBlocker, error) {
	blockers, err := s.GetStopBlockers(traderID)
	if err != nil {
		return nil, err
	}
	unprotected, err := s.GetUnprotectedFillBlockers(traderID)
	if err != nil {
		return nil, err
	}
	blockers = append(blockers, unprotected...)
	rows, err := s.db.Query(`SELECT CAST(id AS TEXT),symbol,status
		FROM copy_trade_position_mappings
		WHERE trader_id=? AND status='active' ORDER BY id`, traderID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b TraderLifecycleBlocker
		b.Code = "OWNERSHIP_MAPPING_ACTIVE"
		if err = rows.Scan(&b.ResourceID, &b.Symbol, &b.Status); err != nil {
			rows.Close()
			return nil, err
		}
		blockers = append(blockers, b)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.db.Query(`SELECT CAST(id AS TEXT),symbol,status
		FROM copy_guard_cycles WHERE trader_id=? AND closed_at IS NULL ORDER BY id`, traderID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b TraderLifecycleBlocker
		b.Code = "COPY_GUARD_CYCLE_OPEN"
		if err = rows.Scan(&b.ResourceID, &b.Symbol, &b.Status); err != nil {
			rows.Close()
			return nil, err
		}
		blockers = append(blockers, b)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.db.Query(`SELECT CAST(id AS TEXT),symbol,status FROM trader_positions
		WHERE trader_id=? AND status='OPEN' AND ABS(quantity)>0 ORDER BY id`, traderID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b TraderLifecycleBlocker
		b.Code = "OPEN_POSITION"
		if err = rows.Scan(&b.ResourceID, &b.Symbol, &b.Status); err != nil {
			rows.Close()
			return nil, err
		}
		blockers = append(blockers, b)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.db.Query(`SELECT CAST(cycle_id AS TEXT),symbol,
		CASE WHEN replacement_pending=1 THEN 'REPLACEMENT_PENDING' ELSE status END
		FROM copy_guard_protective_orders
		WHERE trader_id=? AND (LOWER(status)='live' OR replacement_pending=1) ORDER BY cycle_id`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b TraderLifecycleBlocker
		b.Code = "PROTECTIVE_ORDER_UNRESOLVED"
		if err = rows.Scan(&b.ResourceID, &b.Symbol, &b.Status); err != nil {
			return nil, err
		}
		blockers = append(blockers, b)
	}
	return blockers, rows.Err()
}

func (s *TraderStore) Archive(userID, traderID string) ([]TraderLifecycleBlocker, error) {
	blockers, err := s.GetArchiveBlockers(traderID)
	if err != nil {
		return nil, err
	}
	if len(blockers) > 0 {
		return blockers, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var from string
	var generation int64
	if err = tx.QueryRow(`SELECT lifecycle_status,lifecycle_generation FROM traders
		WHERE id=? AND user_id=?`, traderID, userID).Scan(&from, &generation); err != nil {
		return nil, err
	}
	if from == TraderLifecycleArchived {
		return nil, tx.Commit()
	}
	if from != TraderLifecycleStopped {
		return nil, fmt.Errorf("%w: archive requires STOPPED, current=%s", ErrTraderLifecycleConflict, from)
	}
	generation++
	if err = recordTraderLifecycleEventTx(tx, traderID, generation, from, TraderLifecycleArchiving, "OPERATOR_ARCHIVE", "archive safety checks passed"); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_reentry_candidates SET status=?,
		last_error='trader archived',pending_trigger='TRADER_ARCHIVED',
		closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND status IN ('WATCHING','WAITING','DORMANT_REARM','REVIEWING','PAUSED','PAUSED_BY_TRADER')`,
		ReentryCandidateInvalidatedTraderArchived, traderID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_risk_reservations SET status='RELEASED',released_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND status='ACTIVE'
		  AND NOT EXISTS (
			SELECT 1 FROM copy_trade_execution_intents i
			WHERE i.id=copy_guard_risk_reservations.intent_id
			  AND (i.submitted_at IS NOT NULL OR COALESCE(i.exchange_order_id,'')<>'' OR COALESCE(i.filled_quantity,0)>0)
		  )`, traderID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_configs SET enabled=0 WHERE trader_id=?`, traderID); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`UPDATE traders SET lifecycle_status=?,lifecycle_generation=?,
		is_running=0,archived_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND lifecycle_status=?`,
		TraderLifecycleArchived, generation, traderID, userID, from)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrTraderLifecycleConflict
	}
	if err = recordTraderLifecycleEventTx(tx, traderID, generation, TraderLifecycleArchiving, TraderLifecycleArchived, "ARCHIVE_COMPLETE", "history retained; trader hidden"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return nil, nil
}

// ReconcileOrphanTombstones preserves audit identity for rows left by the old
// physical-delete behavior. It invalidates only work proven to have no active
// trader; exchange-side-effect intents stay untouched for manual reconciliation.
func (s *TraderStore) ReconcileOrphanTombstones() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT OR IGNORE INTO trader_tombstones(trader_id,reason_code)
		SELECT DISTINCT trader_id,'LEGACY_PHYSICAL_DELETE' FROM (
			SELECT trader_id FROM copy_guard_reentry_candidates
			UNION SELECT trader_id FROM copy_guard_cycles
			UNION SELECT trader_id FROM copy_guard_events
			UNION SELECT trader_id FROM copy_trade_execution_intents
			UNION SELECT trader_id FROM copy_trade_signal_logs
			UNION SELECT trader_id FROM copy_trade_events
			UNION SELECT trader_id FROM trader_positions
		) history
		WHERE TRIM(trader_id)<>'' AND NOT EXISTS(SELECT 1 FROM traders t WHERE t.id=history.trader_id)`)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=?,last_error='legacy trader archived',pending_trigger='TRADER_ARCHIVED',
		    closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE trader_id IN (SELECT trader_id FROM trader_tombstones)
		  AND status IN ('WATCHING','WAITING','DORMANT_REARM','REVIEWING','PAUSED','PAUSED_BY_TRADER')`,
		ReentryCandidateInvalidatedTraderArchived); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_risk_reservations
		SET status='RELEASED',released_at=CURRENT_TIMESTAMP
		WHERE trader_id IN (SELECT trader_id FROM trader_tombstones)
		  AND status='ACTIVE' AND (
			intent_id=0 OR EXISTS (
				SELECT 1 FROM copy_trade_execution_intents i
				WHERE i.id=copy_guard_risk_reservations.intent_id
				  AND i.submitted_at IS NULL AND COALESCE(i.exchange_order_id,'')=''
				  AND COALESCE(i.filled_quantity,0)=0
				  AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id)
			)
		  )`); err != nil {
		return err
	}
	return tx.Commit()
}

func FormatLifecycleBlockers(blockers []TraderLifecycleBlocker) string {
	if len(blockers) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blockers))
	for _, b := range blockers {
		parts = append(parts, fmt.Sprintf("%s:%s:%s", b.Code, b.ResourceID, b.Status))
	}
	return strings.Join(parts, ",")
}
