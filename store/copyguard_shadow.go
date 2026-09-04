package store

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	CopyGuardShadowCurrentStop       = "CURRENT_STOP"
	CopyGuardShadowWideStopEqualRisk = "WIDE_STOP_EQUAL_RISK"
	CopyGuardShadowStagedReduction   = "STAGED_REDUCTION"
	CopyGuardShadowProbeReentry25Pct = "PROBE_REENTRY_25_PCT"
	CopyGuardShadowEvaluationVersion = 1
	CopyGuardShadowScorable          = "SCORABLE"
	CopyGuardShadowNoSignal          = "NO_SIGNAL"
	CopyGuardShadowUnscorable        = "UNSCORABLE"
	CopyGuardShadowQualityVerified   = "VERIFIED"
	CopyGuardShadowQualityEstimated  = "ESTIMATED_SHADOW"
	CopyGuardShadowQualityUnscorable = "UNSCORABLE"

	CopyGuardShadowFirstEntryPositionMargin80 = "FIRST_ENTRY_POSITION_MARGIN_80"
	CopyGuardPositionMarginShadowActive       = "ACTIVE"
	CopyGuardPositionMarginShadowCrossed      = "CROSSED"
	CopyGuardPositionMarginShadowFinalized    = "FINALIZED"
)

// CopyGuardPositionMarginShadow is a low-write counterfactual ledger used
// while the real lifecycle remains on atr_structure. It never places an order
// or changes follower sizing. The first-fill anchor is immutable; mutable
// fields describe only the virtual position and one-way safety tightening.
type CopyGuardPositionMarginShadow struct {
	CycleID                       int64      `json:"cycle_id"`
	TraderID                      string     `json:"trader_id"`
	Status                        string     `json:"status"`
	Side                          string     `json:"side"`
	AnchorEntryPrice              float64    `json:"anchor_entry_price"`
	AnchorLeverage                float64    `json:"anchor_leverage"`
	AnchorInitialMargin           float64    `json:"anchor_initial_margin"`
	AnchorStopPrice               float64    `json:"anchor_stop_price"`
	ConfiguredMarginLossPct       float64    `json:"configured_margin_loss_pct"`
	PriceTickSize                 float64    `json:"price_tick_size"`
	QuantityStep                  float64    `json:"quantity_step"`
	InitialQuantity               float64    `json:"initial_quantity"`
	CurrentEntryPrice             float64    `json:"current_entry_price"`
	CurrentQuantity               float64    `json:"current_quantity"`
	CurrentLeverage               float64    `json:"current_leverage"`
	EffectiveStopPrice            float64    `json:"effective_stop_price"`
	LastLeaderSize                float64    `json:"last_leader_size"`
	CrossedEntryPrice             float64    `json:"crossed_entry_price"`
	CrossedQuantity               float64    `json:"crossed_quantity"`
	CrossedLeverage               float64    `json:"crossed_leverage"`
	CrossedPrice                  float64    `json:"crossed_price"`
	CrossedEffectiveMarginLossPct float64    `json:"crossed_effective_margin_loss_pct"`
	CrossedAt                     *time.Time `json:"crossed_at,omitempty"`
	CreatedAt                     time.Time  `json:"created_at"`
	UpdatedAt                     time.Time  `json:"updated_at"`
}

func (s *CopyTradeStore) InitializeCopyGuardPositionMarginShadow(in *CopyGuardPositionMarginShadow) (*CopyGuardPositionMarginShadow, bool, error) {
	if in == nil || in.CycleID <= 0 || strings.TrimSpace(in.TraderID) == "" ||
		(in.Side != "long" && in.Side != "short") || invalidPositiveNumber(in.AnchorEntryPrice) ||
		invalidPositiveNumber(in.AnchorLeverage) || invalidPositiveNumber(in.AnchorInitialMargin) || invalidPositiveNumber(in.AnchorStopPrice) ||
		invalidPositiveNumber(in.ConfiguredMarginLossPct) || in.ConfiguredMarginLossPct >= 1 ||
		invalidPositiveNumber(in.PriceTickSize) || invalidPositiveNumber(in.QuantityStep) || invalidPositiveNumber(in.InitialQuantity) ||
		math.IsNaN(in.LastLeaderSize) || math.IsInf(in.LastLeaderSize, 0) || in.LastLeaderSize < 0 {
		return nil, false, fmt.Errorf("invalid position-margin shadow")
	}
	if in.CurrentEntryPrice <= 0 {
		in.CurrentEntryPrice = in.AnchorEntryPrice
	}
	if in.CurrentQuantity <= 0 {
		in.CurrentQuantity = in.InitialQuantity
	}
	if in.CurrentLeverage <= 0 {
		in.CurrentLeverage = in.AnchorLeverage
	}
	if in.EffectiveStopPrice <= 0 {
		in.EffectiveStopPrice = in.AnchorStopPrice
	}
	if invalidPositiveNumber(in.CurrentEntryPrice) || invalidPositiveNumber(in.CurrentQuantity) ||
		invalidPositiveNumber(in.CurrentLeverage) || invalidPositiveNumber(in.EffectiveStopPrice) {
		return nil, false, fmt.Errorf("invalid position-margin shadow current state")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO copy_guard_position_margin_shadows
		(cycle_id,trader_id,status,side,anchor_entry_price,anchor_leverage,
		 anchor_initial_margin,anchor_stop_price,configured_margin_loss_pct,
		 price_tick_size,quantity_step,initial_quantity,current_entry_price,
		 current_quantity,current_leverage,effective_stop_price,last_leader_size)
		VALUES(?,?,'ACTIVE',?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(cycle_id) DO NOTHING`, in.CycleID, in.TraderID, in.Side,
		in.AnchorEntryPrice, in.AnchorLeverage, in.AnchorInitialMargin,
		in.AnchorStopPrice, in.ConfiguredMarginLossPct, in.PriceTickSize,
		in.QuantityStep, in.InitialQuantity, in.CurrentEntryPrice,
		in.CurrentQuantity, in.CurrentLeverage, in.EffectiveStopPrice, in.LastLeaderSize)
	if err != nil {
		return nil, false, err
	}
	written, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	shadow, err := getPositionMarginShadow(tx.QueryRow, in.CycleID)
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return shadow, written == 1, nil
}

// UpdateCopyGuardPositionMarginShadowPosition records only actual/virtual
// quantity transitions. It deliberately does not run on ordinary price ticks.
func (s *CopyTradeStore) UpdateCopyGuardPositionMarginShadowPosition(cycleID int64, entryPrice, quantity, leverage, leaderSize float64) (bool, error) {
	if cycleID <= 0 || invalidPositiveNumber(entryPrice) || invalidPositiveNumber(quantity) || invalidPositiveNumber(leverage) ||
		math.IsNaN(leaderSize) || math.IsInf(leaderSize, 0) || leaderSize < 0 {
		return false, fmt.Errorf("invalid position-margin shadow position")
	}
	res, err := s.db.Exec(`UPDATE copy_guard_position_margin_shadows SET
		current_entry_price=?,current_quantity=?,current_leverage=?,last_leader_size=?,updated_at=CURRENT_TIMESTAMP
		WHERE cycle_id=? AND status='ACTIVE' AND
		(ABS(current_entry_price-?)>1e-12 OR ABS(current_quantity-?)>1e-12 OR
		 ABS(current_leverage-?)>1e-12 OR ABS(last_leader_size-?)>1e-12)`,
		entryPrice, quantity, leverage, leaderSize, cycleID,
		entryPrice, quantity, leverage, leaderSize)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AdvanceCopyGuardPositionMarginShadowByLeader keeps a stopped real ATR path's
// virtual fixed-stop position aligned with later leader adds/reductions. Adds
// are estimated at the observed mark; reductions retain weighted entry.
func (s *CopyTradeStore) AdvanceCopyGuardPositionMarginShadowByLeader(cycleID int64, leaderSize, markPrice float64) (bool, error) {
	if cycleID <= 0 || math.IsNaN(leaderSize) || math.IsInf(leaderSize, 0) || leaderSize < 0 || invalidPositiveNumber(markPrice) {
		return false, fmt.Errorf("invalid position-margin shadow leader update")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	shadow, err := getPositionMarginShadow(tx.QueryRow, cycleID)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	if shadow.Status != CopyGuardPositionMarginShadowActive || shadow.LastLeaderSize <= 0 ||
		math.Abs(leaderSize-shadow.LastLeaderSize) <= 1e-12 {
		return false, tx.Commit()
	}
	newQuantity := shadow.CurrentQuantity * leaderSize / shadow.LastLeaderSize
	newEntry := shadow.CurrentEntryPrice
	if newQuantity > shadow.CurrentQuantity && newQuantity > 0 {
		added := newQuantity - shadow.CurrentQuantity
		newEntry = (shadow.CurrentEntryPrice*shadow.CurrentQuantity + markPrice*added) / newQuantity
	}
	if math.IsNaN(newQuantity) || math.IsInf(newQuantity, 0) || newQuantity < 0 || invalidPositiveNumber(newEntry) {
		return false, fmt.Errorf("position-margin shadow leader update overflows")
	}
	_, err = tx.Exec(`UPDATE copy_guard_position_margin_shadows SET
		current_entry_price=?,current_quantity=?,last_leader_size=?,updated_at=CURRENT_TIMESTAMP
		WHERE cycle_id=? AND status='ACTIVE'`, newEntry, newQuantity, leaderSize, cycleID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ObserveCopyGuardPositionMarginShadow persists only a one-way safety
// tightening or the first mark-price crossing. Repeated market ticks do not
// write rows.
func (s *CopyTradeStore) ObserveCopyGuardPositionMarginShadow(cycleID int64, effectiveStop, markPrice, entryPrice, quantity, leverage float64) (crossed, tightened bool, err error) {
	if cycleID <= 0 || invalidPositiveNumber(effectiveStop) || invalidPositiveNumber(markPrice) ||
		invalidPositiveNumber(entryPrice) || invalidPositiveNumber(quantity) || invalidPositiveNumber(leverage) {
		return false, false, fmt.Errorf("invalid position-margin shadow observation")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	shadow, err := getPositionMarginShadow(tx.QueryRow, cycleID)
	if err != nil {
		return false, false, err
	}
	if shadow.Status != CopyGuardPositionMarginShadowActive {
		return false, false, tx.Commit()
	}
	if shadow.Side == "long" {
		if effectiveStop < shadow.EffectiveStopPrice {
			effectiveStop = shadow.EffectiveStopPrice
		}
		tightened = effectiveStop > shadow.EffectiveStopPrice+1e-12
		crossed = markPrice <= effectiveStop
	} else {
		if effectiveStop > shadow.EffectiveStopPrice {
			effectiveStop = shadow.EffectiveStopPrice
		}
		tightened = effectiveStop < shadow.EffectiveStopPrice-1e-12
		crossed = markPrice >= effectiveStop
	}
	if !tightened && !crossed {
		return false, false, tx.Commit()
	}
	effectivePct := math.Abs(entryPrice-effectiveStop) / entryPrice * leverage
	if math.IsNaN(effectivePct) || math.IsInf(effectivePct, 0) {
		return false, false, fmt.Errorf("position-margin shadow observation overflows")
	}
	status := CopyGuardPositionMarginShadowActive
	if crossed {
		status = CopyGuardPositionMarginShadowCrossed
	}
	_, err = tx.Exec(`UPDATE copy_guard_position_margin_shadows SET
		status=?,effective_stop_price=?,current_entry_price=?,current_quantity=?,current_leverage=?,
		crossed_entry_price=CASE WHEN ? THEN ? ELSE crossed_entry_price END,
		crossed_quantity=CASE WHEN ? THEN ? ELSE crossed_quantity END,
		crossed_leverage=CASE WHEN ? THEN ? ELSE crossed_leverage END,
		crossed_price=CASE WHEN ? THEN ? ELSE crossed_price END,
		crossed_effective_margin_loss_pct=CASE WHEN ? THEN ? ELSE crossed_effective_margin_loss_pct END,
		crossed_at=CASE WHEN ? THEN CURRENT_TIMESTAMP ELSE crossed_at END,updated_at=CURRENT_TIMESTAMP
		WHERE cycle_id=? AND status='ACTIVE'`, status, effectiveStop, entryPrice, quantity, leverage,
		crossed, entryPrice, crossed, quantity, crossed, leverage, crossed, markPrice,
		crossed, effectivePct, crossed, cycleID)
	if err != nil {
		return false, false, err
	}
	return crossed, tightened, tx.Commit()
}

func (s *CopyTradeStore) GetCopyGuardPositionMarginShadow(cycleID int64) (*CopyGuardPositionMarginShadow, error) {
	return getPositionMarginShadow(s.db.QueryRow, cycleID)
}

func (s *CopyTradeStore) ListActiveCopyGuardPositionMarginShadows(traderID string) ([]*CopyGuardPositionMarginShadow, error) {
	// CROSSED rows remain observable until the leader lifecycle ends so v2 can
	// measure post-stop reversal and mark-path coverage. The legacy v1 observer
	// is terminal/idempotent for those rows and performs no additional writes.
	rows, err := s.db.Query(positionMarginShadowSelect+` WHERE trader_id=? AND status IN ('ACTIVE','CROSSED') ORDER BY cycle_id`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyGuardPositionMarginShadow
	for rows.Next() {
		shadow, scanErr := scanPositionMarginShadow(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, shadow)
	}
	return out, rows.Err()
}

func (s *CopyTradeStore) FinalizeCopyGuardPositionMarginShadow(cycleID int64, leaderClosePrice float64) error {
	if err := s.finalizeCopyGuardPositionMarginShadowV2(cycleID, leaderClosePrice); err != nil {
		return err
	}
	shadow, err := s.GetCopyGuardPositionMarginShadow(cycleID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if shadow.Status == CopyGuardPositionMarginShadowFinalized {
		return nil
	}
	evaluation := &CopyGuardShadowEvaluation{
		CycleID: shadow.CycleID, TraderID: shadow.TraderID,
		Policy:            CopyGuardShadowFirstEntryPositionMargin80,
		EvaluationVersion: CopyGuardShadowEvaluationVersion,
		Status:            CopyGuardShadowNoSignal, DataQuality: CopyGuardShadowQualityEstimated,
		SizeFactor: 1, EntryPrice: shadow.CurrentEntryPrice, ExitPrice: leaderClosePrice,
		Reason: "fixed first-fill stop was not crossed before leader lifecycle ended; shadow uses observed marks and excludes fees, funding, slippage and post-ATR-stop liquidation recalculation",
	}
	if shadow.Status == CopyGuardPositionMarginShadowCrossed {
		move := shadow.CrossedPrice - shadow.CrossedEntryPrice
		if shadow.Side == "short" {
			move = -move
		}
		evaluation.Status = CopyGuardShadowScorable
		evaluation.GrossPnL = move * shadow.CrossedQuantity
		evaluation.NetPnL = evaluation.GrossPnL
		evaluation.EntryPrice = shadow.CrossedEntryPrice
		evaluation.ExitPrice = shadow.CrossedPrice
		evaluation.Reason = fmt.Sprintf("first-fill %.2f%% margin stop crossed; current effective margin loss %.2f%%; fees, funding, slippage and jump loss excluded",
			shadow.ConfiguredMarginLossPct*100, shadow.CrossedEffectiveMarginLossPct*100)
	}
	if err := s.SaveCopyGuardShadowEvaluation(evaluation); err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE copy_guard_position_margin_shadows SET status='FINALIZED',updated_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND status<>'FINALIZED'`, cycleID)
	return err
}

const positionMarginShadowSelect = `SELECT cycle_id,trader_id,status,side,
	anchor_entry_price,anchor_leverage,anchor_initial_margin,anchor_stop_price,
	configured_margin_loss_pct,price_tick_size,quantity_step,initial_quantity,
	current_entry_price,current_quantity,current_leverage,effective_stop_price,last_leader_size,
	crossed_entry_price,crossed_quantity,crossed_leverage,crossed_price,
	crossed_effective_margin_loss_pct,crossed_at,created_at,updated_at
	FROM copy_guard_position_margin_shadows`

type positionMarginShadowRowQuery func(query string, args ...interface{}) *sql.Row

func getPositionMarginShadow(query positionMarginShadowRowQuery, cycleID int64) (*CopyGuardPositionMarginShadow, error) {
	return scanPositionMarginShadow(func(dest ...interface{}) error {
		return query(positionMarginShadowSelect+` WHERE cycle_id=?`, cycleID).Scan(dest...)
	})
}

func scanPositionMarginShadow(scan func(dest ...interface{}) error) (*CopyGuardPositionMarginShadow, error) {
	var shadow CopyGuardPositionMarginShadow
	var crossedAt sql.NullString
	var createdAt, updatedAt string
	if err := scan(&shadow.CycleID, &shadow.TraderID, &shadow.Status, &shadow.Side,
		&shadow.AnchorEntryPrice, &shadow.AnchorLeverage, &shadow.AnchorInitialMargin,
		&shadow.AnchorStopPrice, &shadow.ConfiguredMarginLossPct, &shadow.PriceTickSize,
		&shadow.QuantityStep, &shadow.InitialQuantity, &shadow.CurrentEntryPrice,
		&shadow.CurrentQuantity, &shadow.CurrentLeverage, &shadow.EffectiveStopPrice,
		&shadow.LastLeaderSize, &shadow.CrossedEntryPrice, &shadow.CrossedQuantity,
		&shadow.CrossedLeverage, &shadow.CrossedPrice, &shadow.CrossedEffectiveMarginLossPct,
		&crossedAt, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	var err error
	if crossedAt.Valid {
		parsed, parseErr := parseDBTime(crossedAt.String)
		if parseErr != nil {
			return nil, parseErr
		}
		shadow.CrossedAt = &parsed
	}
	if shadow.CreatedAt, err = parseDBTime(createdAt); err != nil {
		return nil, err
	}
	if shadow.UpdatedAt, err = parseDBTime(updatedAt); err != nil {
		return nil, err
	}
	return &shadow, nil
}

type CopyGuardShadowEvaluation struct {
	ID                int64     `json:"id"`
	CycleID           int64     `json:"cycle_id"`
	TraderID          string    `json:"trader_id"`
	Policy            string    `json:"policy"`
	EvaluationVersion int       `json:"evaluation_version"`
	Status            string    `json:"status"`
	DataQuality       string    `json:"data_quality"`
	GrossPnL          float64   `json:"gross_pnl"`
	EstimatedCost     float64   `json:"estimated_cost"`
	NetPnL            float64   `json:"net_pnl"`
	SizeFactor        float64   `json:"size_factor"`
	EntryPrice        float64   `json:"entry_price"`
	ExitPrice         float64   `json:"exit_price"`
	Reason            string    `json:"reason"`
	BaselinePolicy    string    `json:"baseline_policy"`
	BaselineNetPnL    float64   `json:"baseline_net_pnl"`
	IncrementalEffect float64   `json:"incremental_net_effect"`
	CostSource        string    `json:"cost_source"`
	MarkCoverage      float64   `json:"mark_coverage"`
	StopCrossed       bool      `json:"stop_crossed"`
	CrossingVerified  bool      `json:"crossing_verified"`
	MinimumMark       float64   `json:"minimum_mark"`
	MaximumMark       float64   `json:"maximum_mark"`
	PostStopReversed  bool      `json:"post_stop_reversed"`
	SlippageBPS       float64   `json:"slippage_bps"`
	MinimumLeverage   float64   `json:"minimum_leverage"`
	MaximumLeverage   float64   `json:"maximum_leverage"`
	MinimumNotional   float64   `json:"minimum_notional"`
	MaximumNotional   float64   `json:"maximum_notional"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (s *CopyTradeStore) SaveCopyGuardShadowEvaluation(e *CopyGuardShadowEvaluation) error {
	return upsertCopyGuardShadowEvaluation(s.db, e)
}

func upsertCopyGuardShadowEvaluation(exec interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}, e *CopyGuardShadowEvaluation) error {
	if e == nil || e.CycleID <= 0 || strings.TrimSpace(e.TraderID) == "" ||
		strings.TrimSpace(e.Policy) == "" || e.EvaluationVersion <= 0 {
		return fmt.Errorf("invalid copy guard shadow evaluation")
	}
	_, err := exec.Exec(`INSERT INTO copy_guard_shadow_evaluations
		(cycle_id,trader_id,policy,evaluation_version,status,data_quality,gross_pnl,
		 estimated_cost,net_pnl,size_factor,entry_price,exit_price,reason,
		 baseline_policy,baseline_net_pnl,incremental_net_effect,cost_source,
		 mark_coverage,stop_crossed,crossing_verified,minimum_mark,maximum_mark,
		 post_stop_reversed,slippage_bps,minimum_leverage,maximum_leverage,
		 minimum_notional,maximum_notional)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(cycle_id,policy,evaluation_version) DO UPDATE SET
			status=excluded.status,data_quality=excluded.data_quality,
			gross_pnl=excluded.gross_pnl,estimated_cost=excluded.estimated_cost,
			net_pnl=excluded.net_pnl,size_factor=excluded.size_factor,
			entry_price=excluded.entry_price,exit_price=excluded.exit_price,
			reason=excluded.reason,baseline_policy=excluded.baseline_policy,
			baseline_net_pnl=excluded.baseline_net_pnl,
			incremental_net_effect=excluded.incremental_net_effect,
			cost_source=excluded.cost_source,mark_coverage=excluded.mark_coverage,
			stop_crossed=excluded.stop_crossed,crossing_verified=excluded.crossing_verified,
			minimum_mark=excluded.minimum_mark,maximum_mark=excluded.maximum_mark,
			post_stop_reversed=excluded.post_stop_reversed,slippage_bps=excluded.slippage_bps,
			minimum_leverage=excluded.minimum_leverage,maximum_leverage=excluded.maximum_leverage,
			minimum_notional=excluded.minimum_notional,maximum_notional=excluded.maximum_notional,
			updated_at=CURRENT_TIMESTAMP`,
		e.CycleID, e.TraderID, e.Policy, e.EvaluationVersion, e.Status, e.DataQuality,
		e.GrossPnL, e.EstimatedCost, e.NetPnL, e.SizeFactor, e.EntryPrice,
		e.ExitPrice, e.Reason, e.BaselinePolicy, e.BaselineNetPnL,
		e.IncrementalEffect, e.CostSource, e.MarkCoverage, e.StopCrossed,
		e.CrossingVerified, e.MinimumMark, e.MaximumMark, e.PostStopReversed,
		e.SlippageBPS, e.MinimumLeverage, e.MaximumLeverage, e.MinimumNotional,
		e.MaximumNotional)
	return err
}

func (s *CopyTradeStore) ListCopyGuardShadowEvaluations(cycleID int64) ([]*CopyGuardShadowEvaluation, error) {
	rows, err := s.db.Query(`SELECT id,cycle_id,trader_id,policy,evaluation_version,status,
		data_quality,gross_pnl,estimated_cost,net_pnl,size_factor,entry_price,exit_price,
		reason,baseline_policy,baseline_net_pnl,incremental_net_effect,cost_source,
		mark_coverage,stop_crossed,crossing_verified,minimum_mark,maximum_mark,
		post_stop_reversed,slippage_bps,minimum_leverage,maximum_leverage,
		minimum_notional,maximum_notional,created_at,updated_at
		FROM copy_guard_shadow_evaluations WHERE cycle_id=?
		ORDER BY policy,evaluation_version`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyGuardShadowEvaluation
	for rows.Next() {
		e, scanErr := scanCopyGuardShadowEvaluation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *CopyTradeStore) ListCopyGuardShadowEvaluationsForTraders(
	traderIDs []string, from, to time.Time,
) ([]*CopyGuardShadowEvaluation, error) {
	if len(traderIDs) == 0 {
		return []*CopyGuardShadowEvaluation{}, nil
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+2)
	for _, traderID := range traderIDs {
		args = append(args, traderID)
	}
	args = append(args, from.UTC().Format("2006-01-02 15:04:05"),
		to.UTC().Format("2006-01-02 15:04:05"))
	rows, err := s.db.Query(`SELECT e.id,e.cycle_id,e.trader_id,e.policy,
		e.evaluation_version,e.status,e.data_quality,e.gross_pnl,e.estimated_cost,
		e.net_pnl,e.size_factor,e.entry_price,e.exit_price,e.reason,
		e.baseline_policy,e.baseline_net_pnl,e.incremental_net_effect,e.cost_source,
		e.mark_coverage,e.stop_crossed,e.crossing_verified,e.minimum_mark,e.maximum_mark,
		e.post_stop_reversed,e.slippage_bps,e.minimum_leverage,e.maximum_leverage,
		e.minimum_notional,e.maximum_notional,e.created_at,e.updated_at
		FROM copy_guard_shadow_evaluations e
		JOIN copy_guard_cycles c ON c.id=e.cycle_id
		WHERE e.trader_id IN (`+marks+`) AND c.closed_at>=? AND c.closed_at<?
		ORDER BY e.policy,e.cycle_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyGuardShadowEvaluation
	for rows.Next() {
		e, scanErr := scanCopyGuardShadowEvaluation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *CopyTradeStore) CountUnprotectedFilledIntents(
	traderIDs []string, from, to time.Time,
) (int, error) {
	if len(traderIDs) == 0 {
		return 0, nil
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+2)
	for _, traderID := range traderIDs {
		args = append(args, traderID)
	}
	args = append(args, from.UTC().Format("2006-01-02 15:04:05"),
		to.UTC().Format("2006-01-02 15:04:05"))
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_intents i
		JOIN copy_guard_cycles c ON c.id=i.cycle_id
		WHERE i.trader_id IN (`+marks+`) AND c.closed_at>=? AND c.closed_at<?
		  AND COALESCE(i.filled_quantity,0)>0
		  AND i.action IN ('open_long','open_short')
		  AND i.protected_at IS NULL`, args...).Scan(&count)
	return count, err
}

type shadowEvaluationScanner interface {
	Scan(dest ...interface{}) error
}

func scanCopyGuardShadowEvaluation(row shadowEvaluationScanner) (*CopyGuardShadowEvaluation, error) {
	var e CopyGuardShadowEvaluation
	var createdAt, updatedAt string
	if err := row.Scan(&e.ID, &e.CycleID, &e.TraderID, &e.Policy,
		&e.EvaluationVersion, &e.Status, &e.DataQuality, &e.GrossPnL,
		&e.EstimatedCost, &e.NetPnL, &e.SizeFactor, &e.EntryPrice,
		&e.ExitPrice, &e.Reason, &e.BaselinePolicy, &e.BaselineNetPnL,
		&e.IncrementalEffect, &e.CostSource, &e.MarkCoverage, &e.StopCrossed,
		&e.CrossingVerified, &e.MinimumMark, &e.MaximumMark, &e.PostStopReversed,
		&e.SlippageBPS, &e.MinimumLeverage, &e.MaximumLeverage,
		&e.MinimumNotional, &e.MaximumNotional, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	var err error
	if e.CreatedAt, err = parseDBTime(createdAt); err != nil {
		return nil, err
	}
	if e.UpdatedAt, err = parseDBTime(updatedAt); err != nil {
		return nil, err
	}
	return &e, nil
}

var _ shadowEvaluationScanner = (*sql.Row)(nil)
