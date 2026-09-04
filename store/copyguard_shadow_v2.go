package store

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

const CopyGuardPositionMarginShadowEvaluationVersion = 2

// CopyGuardPositionMarginShadowV2 is an independent counterfactual position
// ledger. Unlike the legacy v1 row, it realizes reductions, freezes after a
// stop crossing, records full turnover/cost inputs and carries mark-path
// completeness as first-class evidence.
type CopyGuardPositionMarginShadowV2 struct {
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
	RealizedGrossPnL              float64    `json:"realized_gross_pnl"`
	EntryTurnover                 float64    `json:"entry_turnover"`
	ExitTurnover                  float64    `json:"exit_turnover"`
	MinimumLeverage               float64    `json:"minimum_leverage"`
	MaximumLeverage               float64    `json:"maximum_leverage"`
	MinimumNotional               float64    `json:"minimum_notional"`
	MaximumNotional               float64    `json:"maximum_notional"`
	ConfiguredCostBPS             float64    `json:"configured_cost_bps"`
	CrossedPrice                  float64    `json:"crossed_price"`
	CrossedQuantity               float64    `json:"crossed_quantity"`
	CrossedEffectiveMarginLossPct float64    `json:"crossed_effective_margin_loss_pct"`
	CrossingSource                string     `json:"crossing_source"`
	CrossedAt                     *time.Time `json:"crossed_at,omitempty"`
	MinimumMark                   float64    `json:"minimum_mark"`
	MaximumMark                   float64    `json:"maximum_mark"`
	LastMark                      float64    `json:"last_mark"`
	PostCrossMinimumMark          float64    `json:"post_cross_minimum_mark"`
	PostCrossMaximumMark          float64    `json:"post_cross_maximum_mark"`
	MarkCoveredSeconds            float64    `json:"mark_covered_seconds"`
	MarkGapSeconds                float64    `json:"mark_gap_seconds"`
	MarkObservationCount          int        `json:"mark_observation_count"`
	FirstMarkAt                   *time.Time `json:"first_mark_at,omitempty"`
	LastMarkAt                    *time.Time `json:"last_mark_at,omitempty"`
	DataQuality                   string     `json:"data_quality"`
	UnscorableReason              string     `json:"unscorable_reason"`
	CreatedAt                     time.Time  `json:"created_at"`
	UpdatedAt                     time.Time  `json:"updated_at"`
}

type CopyGuardShadowMarkCheckpoint struct {
	Key              string
	BucketAt         time.Time
	MinimumMark      float64
	MaximumMark      float64
	LastMark         float64
	ObservationCount int
	CoveredSeconds   float64
	GapSeconds       float64
	ObservedAt       time.Time
	Source           string
}

func validatePositionMarginShadowV2(in *CopyGuardPositionMarginShadowV2) error {
	if in == nil || in.CycleID <= 0 || strings.TrimSpace(in.TraderID) == "" ||
		(in.Side != "long" && in.Side != "short") || invalidPositiveNumber(in.AnchorEntryPrice) ||
		invalidPositiveNumber(in.AnchorLeverage) || invalidPositiveNumber(in.AnchorInitialMargin) ||
		invalidPositiveNumber(in.AnchorStopPrice) || invalidPositiveNumber(in.ConfiguredMarginLossPct) ||
		in.ConfiguredMarginLossPct >= 1 || invalidPositiveNumber(in.PriceTickSize) ||
		invalidPositiveNumber(in.QuantityStep) || invalidPositiveNumber(in.InitialQuantity) ||
		invalidPositiveNumber(in.CurrentEntryPrice) || invalidPositiveNumber(in.CurrentQuantity) ||
		invalidPositiveNumber(in.CurrentLeverage) || invalidPositiveNumber(in.EffectiveStopPrice) ||
		math.IsNaN(in.ConfiguredCostBPS) || math.IsInf(in.ConfiguredCostBPS, 0) || in.ConfiguredCostBPS < 0 ||
		math.IsNaN(in.LastLeaderSize) || math.IsInf(in.LastLeaderSize, 0) || in.LastLeaderSize < 0 {
		return fmt.Errorf("invalid position-margin shadow v2")
	}
	return nil
}

func (s *CopyTradeStore) InitializeCopyGuardPositionMarginShadowV2(in *CopyGuardPositionMarginShadowV2) (*CopyGuardPositionMarginShadowV2, bool, error) {
	if err := validatePositionMarginShadowV2(in); err != nil {
		return nil, false, err
	}
	notional := in.CurrentEntryPrice * in.CurrentQuantity
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO copy_guard_position_margin_shadow_v2
		(cycle_id,trader_id,status,side,anchor_entry_price,anchor_leverage,
		 anchor_initial_margin,anchor_stop_price,configured_margin_loss_pct,
		 price_tick_size,quantity_step,initial_quantity,current_entry_price,
		 current_quantity,current_leverage,effective_stop_price,last_leader_size,
		 entry_turnover,minimum_leverage,maximum_leverage,minimum_notional,
		 maximum_notional,configured_cost_bps,data_quality)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(cycle_id) DO NOTHING`, in.CycleID, in.TraderID, CopyGuardPositionMarginShadowActive, in.Side,
		in.AnchorEntryPrice, in.AnchorLeverage, in.AnchorInitialMargin,
		in.AnchorStopPrice, in.ConfiguredMarginLossPct, in.PriceTickSize,
		in.QuantityStep, in.InitialQuantity, in.CurrentEntryPrice,
		in.CurrentQuantity, in.CurrentLeverage, in.EffectiveStopPrice,
		in.LastLeaderSize, notional, in.CurrentLeverage, in.CurrentLeverage,
		notional, notional, in.ConfiguredCostBPS, CopyGuardShadowQualityVerified)
	if err != nil {
		return nil, false, err
	}
	written, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	shadow, err := getPositionMarginShadowV2(tx.QueryRow, in.CycleID)
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return shadow, written == 1, nil
}

func (s *CopyTradeStore) GetCopyGuardPositionMarginShadowV2(cycleID int64) (*CopyGuardPositionMarginShadowV2, error) {
	return getPositionMarginShadowV2(s.db.QueryRow, cycleID)
}

func shadowDirectionalPnL(side string, entry, exit, quantity float64) float64 {
	pnl := (exit - entry) * quantity
	if side == "short" {
		pnl = -pnl
	}
	return pnl
}

func minPositive(current, candidate float64) float64 {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

func maxPositive(current, candidate float64) float64 {
	if candidate > current {
		return candidate
	}
	return current
}

// SyncCopyGuardPositionMarginShadowV2 applies one confirmed follower position
// transition. Adds update the weighted entry; reductions realize PnL at the
// confirmed order price. The immutable stop anchor is never changed.
func (s *CopyTradeStore) SyncCopyGuardPositionMarginShadowV2(cycleID int64, entryPrice, quantity, leverage, leaderSize, executionPrice float64) (bool, error) {
	if cycleID <= 0 || invalidPositiveNumber(entryPrice) || invalidPositiveNumber(quantity) ||
		invalidPositiveNumber(leverage) || math.IsNaN(leaderSize) || math.IsInf(leaderSize, 0) || leaderSize < 0 ||
		invalidPositiveNumber(executionPrice) {
		return false, fmt.Errorf("invalid position-margin shadow v2 transition")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	shadow, err := getPositionMarginShadowV2(tx.QueryRow, cycleID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if shadow.Status != CopyGuardPositionMarginShadowActive {
		return false, tx.Commit()
	}
	quantityDelta := quantity - shadow.CurrentQuantity
	changed := math.Abs(quantityDelta) > 1e-12 || math.Abs(entryPrice-shadow.CurrentEntryPrice) > 1e-12 ||
		math.Abs(leverage-shadow.CurrentLeverage) > 1e-12 || math.Abs(leaderSize-shadow.LastLeaderSize) > 1e-12
	if !changed {
		return false, tx.Commit()
	}
	realized, entryTurnover, exitTurnover := shadow.RealizedGrossPnL, shadow.EntryTurnover, shadow.ExitTurnover
	if quantityDelta > 1e-12 {
		fillPrice := executionPrice
		derived := (entryPrice*quantity - shadow.CurrentEntryPrice*shadow.CurrentQuantity) / quantityDelta
		if !invalidPositiveNumber(derived) {
			fillPrice = derived
		}
		entryTurnover += fillPrice * quantityDelta
	} else if quantityDelta < -1e-12 {
		reduced := -quantityDelta
		realized += shadowDirectionalPnL(shadow.Side, shadow.CurrentEntryPrice, executionPrice, reduced)
		exitTurnover += executionPrice * reduced
	}
	notional := entryPrice * quantity
	_, err = tx.Exec(`UPDATE copy_guard_position_margin_shadow_v2 SET
		current_entry_price=?,current_quantity=?,current_leverage=?,last_leader_size=?,
		realized_gross_pnl=?,entry_turnover=?,exit_turnover=?,
		minimum_leverage=?,maximum_leverage=?,minimum_notional=?,maximum_notional=?,
		updated_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND status='ACTIVE'`,
		entryPrice, quantity, leverage, leaderSize, realized, entryTurnover, exitTurnover,
		minPositive(shadow.MinimumLeverage, leverage), maxPositive(shadow.MaximumLeverage, leverage),
		minPositive(shadow.MinimumNotional, notional), maxPositive(shadow.MaximumNotional, notional), cycleID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// AdvanceCopyGuardPositionMarginShadowV2ByLeader keeps the virtual fixed-stop
// path alive after the real ATR path has exited. The execution venue's
// authoritative mark is the counterfactual fill price; a crossed shadow
// remains frozen. Leader prices are deliberately not accepted here because
// they can differ from the venue where the follower would actually trade.
func (s *CopyTradeStore) AdvanceCopyGuardPositionMarginShadowV2ByLeader(cycleID int64, leaderSize, markPrice, leverage float64) (bool, error) {
	if cycleID <= 0 || math.IsNaN(leaderSize) || math.IsInf(leaderSize, 0) || leaderSize < 0 ||
		invalidPositiveNumber(markPrice) || invalidPositiveNumber(leverage) {
		return false, fmt.Errorf("invalid position-margin shadow v2 leader transition")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	shadow, err := getPositionMarginShadowV2(tx.QueryRow, cycleID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if shadow.Status != CopyGuardPositionMarginShadowActive || shadow.LastLeaderSize <= 0 || math.Abs(leaderSize-shadow.LastLeaderSize) <= 1e-12 {
		return false, tx.Commit()
	}
	newQuantity := shadow.CurrentQuantity * leaderSize / shadow.LastLeaderSize
	if newQuantity <= 0 || math.IsNaN(newQuantity) || math.IsInf(newQuantity, 0) {
		return false, fmt.Errorf("position-margin shadow v2 quantity overflows")
	}
	newEntry := shadow.CurrentEntryPrice
	realized, entryTurnover, exitTurnover := shadow.RealizedGrossPnL, shadow.EntryTurnover, shadow.ExitTurnover
	if newQuantity > shadow.CurrentQuantity {
		added := newQuantity - shadow.CurrentQuantity
		newEntry = (shadow.CurrentEntryPrice*shadow.CurrentQuantity + markPrice*added) / newQuantity
		entryTurnover += markPrice * added
	} else {
		reduced := shadow.CurrentQuantity - newQuantity
		realized += shadowDirectionalPnL(shadow.Side, shadow.CurrentEntryPrice, markPrice, reduced)
		exitTurnover += markPrice * reduced
	}
	notional := newEntry * newQuantity
	_, err = tx.Exec(`UPDATE copy_guard_position_margin_shadow_v2 SET
		current_entry_price=?,current_quantity=?,current_leverage=?,last_leader_size=?,
		realized_gross_pnl=?,entry_turnover=?,exit_turnover=?,
		minimum_leverage=?,maximum_leverage=?,minimum_notional=?,maximum_notional=?,
		updated_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND status='ACTIVE'`,
		newEntry, newQuantity, leverage, leaderSize, realized, entryTurnover, exitTurnover,
		minPositive(shadow.MinimumLeverage, leverage), maxPositive(shadow.MaximumLeverage, leverage),
		minPositive(shadow.MinimumNotional, notional), maxPositive(shadow.MaximumNotional, notional), cycleID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func validateShadowCheckpoint(cycleID int64, cp CopyGuardShadowMarkCheckpoint, effectiveStop float64) error {
	if cycleID <= 0 || strings.TrimSpace(cp.Key) == "" || cp.BucketAt.IsZero() || cp.ObservedAt.IsZero() ||
		invalidPositiveNumber(cp.MinimumMark) || invalidPositiveNumber(cp.MaximumMark) || invalidPositiveNumber(cp.LastMark) ||
		cp.MinimumMark > cp.MaximumMark || cp.ObservationCount <= 0 || invalidPositiveNumber(effectiveStop) ||
		math.IsNaN(cp.CoveredSeconds) || math.IsInf(cp.CoveredSeconds, 0) || cp.CoveredSeconds < 0 ||
		math.IsNaN(cp.GapSeconds) || math.IsInf(cp.GapSeconds, 0) || cp.GapSeconds < 0 || strings.TrimSpace(cp.Source) == "" {
		return fmt.Errorf("invalid position-margin shadow v2 mark checkpoint")
	}
	return nil
}

// CheckpointCopyGuardPositionMarginShadowV2 persists one minute/event mark
// aggregate. checkpoint_key makes retry and history overlap idempotent.
func (s *CopyTradeStore) CheckpointCopyGuardPositionMarginShadowV2(cycleID int64, cp CopyGuardShadowMarkCheckpoint, effectiveStop float64) (bool, error) {
	if err := validateShadowCheckpoint(cycleID, cp, effectiveStop); err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	shadow, err := getPositionMarginShadowV2(tx.QueryRow, cycleID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if shadow.Status == CopyGuardPositionMarginShadowFinalized {
		return false, tx.Commit()
	}
	res, err := tx.Exec(`INSERT INTO copy_guard_shadow_mark_checkpoints
		(cycle_id,checkpoint_key,bucket_at,minimum_mark,maximum_mark,last_mark,
		 observation_count,covered_seconds,gap_seconds,source)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(cycle_id,checkpoint_key) DO NOTHING`,
		cycleID, cp.Key, cp.BucketAt.UTC().Format("2006-01-02 15:04:05"), cp.MinimumMark,
		cp.MaximumMark, cp.LastMark, cp.ObservationCount, cp.CoveredSeconds, cp.GapSeconds, cp.Source)
	if err != nil {
		return false, err
	}
	written, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if written == 0 {
		return false, tx.Commit()
	}
	if shadow.Side == "long" && effectiveStop < shadow.EffectiveStopPrice {
		effectiveStop = shadow.EffectiveStopPrice
	}
	if shadow.Side == "short" && effectiveStop > shadow.EffectiveStopPrice {
		effectiveStop = shadow.EffectiveStopPrice
	}
	minimumMark := minPositive(shadow.MinimumMark, cp.MinimumMark)
	maximumMark := maxPositive(shadow.MaximumMark, cp.MaximumMark)
	postMin, postMax := shadow.PostCrossMinimumMark, shadow.PostCrossMaximumMark
	status := shadow.Status
	crossed := false
	crossedPrice := shadow.CrossedPrice
	crossedQuantity := shadow.CrossedQuantity
	crossedPct := shadow.CrossedEffectiveMarginLossPct
	crossingSource := shadow.CrossingSource
	realized := shadow.RealizedGrossPnL
	exitTurnover := shadow.ExitTurnover
	currentQuantity := shadow.CurrentQuantity
	if status == CopyGuardPositionMarginShadowActive {
		crossed = (shadow.Side == "long" && cp.MinimumMark <= effectiveStop) ||
			(shadow.Side == "short" && cp.MaximumMark >= effectiveStop)
		if crossed {
			crossedPrice = effectiveStop
			if (shadow.Side == "long" && cp.LastMark <= effectiveStop) || (shadow.Side == "short" && cp.LastMark >= effectiveStop) {
				crossedPrice = cp.LastMark
			}
			crossedQuantity = currentQuantity
			crossedPct = math.Abs(shadow.CurrentEntryPrice-effectiveStop) / shadow.CurrentEntryPrice * shadow.CurrentLeverage
			crossingSource = cp.Source
			realized += shadowDirectionalPnL(shadow.Side, shadow.CurrentEntryPrice, crossedPrice, currentQuantity)
			exitTurnover += crossedPrice * currentQuantity
			currentQuantity = 0
			status = CopyGuardPositionMarginShadowCrossed
			// The checkpoint extrema do not retain intra-bucket ordering. An
			// earlier high/low from the crossing minute therefore cannot prove a
			// post-stop reversal. The bucket close is known to be after every
			// earlier observation and is the only safe starting point; subsequent
			// checkpoints extend the post-cross extrema normally.
			postMin, postMax = cp.LastMark, cp.LastMark
		}
	} else if status == CopyGuardPositionMarginShadowCrossed {
		postMin = minPositive(postMin, cp.MinimumMark)
		postMax = maxPositive(postMax, cp.MaximumMark)
	}
	_, err = tx.Exec(`UPDATE copy_guard_position_margin_shadow_v2 SET
		status=?,current_quantity=?,effective_stop_price=?,realized_gross_pnl=?,exit_turnover=?,
		crossed_price=?,crossed_quantity=?,crossed_effective_margin_loss_pct=?,crossing_source=?,
		crossed_at=CASE WHEN ? THEN ? ELSE crossed_at END,
		minimum_mark=?,maximum_mark=?,last_mark=?,post_cross_minimum_mark=?,post_cross_maximum_mark=?,
		mark_covered_seconds=mark_covered_seconds+?,mark_gap_seconds=mark_gap_seconds+?,
		mark_observation_count=mark_observation_count+?,first_mark_at=COALESCE(first_mark_at,?),
		last_mark_at=CASE WHEN last_mark_at IS NULL OR last_mark_at<? THEN ? ELSE last_mark_at END,
		updated_at=CURRENT_TIMESTAMP WHERE cycle_id=?`,
		status, currentQuantity, effectiveStop, realized, exitTurnover, crossedPrice, crossedQuantity,
		crossedPct, crossingSource, crossed, cp.ObservedAt.UTC().Format("2006-01-02 15:04:05"),
		minimumMark, maximumMark, cp.LastMark, postMin, postMax, cp.CoveredSeconds, cp.GapSeconds,
		cp.ObservationCount, cp.BucketAt.UTC().Format("2006-01-02 15:04:05"),
		cp.ObservedAt.UTC().Format("2006-01-02 15:04:05"), cp.ObservedAt.UTC().Format("2006-01-02 15:04:05"), cycleID)
	if err != nil {
		return false, err
	}
	return crossed, tx.Commit()
}

func shadowV2MarkCoverage(shadow *CopyGuardPositionMarginShadowV2) float64 {
	if shadow == nil {
		return 0
	}
	total := shadow.MarkCoveredSeconds + shadow.MarkGapSeconds
	if total <= 0 {
		if shadow.MarkObservationCount >= 2 {
			return 1
		}
		return 0
	}
	return math.Max(0, math.Min(1, shadow.MarkCoveredSeconds/total))
}

func shadowV2PostStopReversed(shadow *CopyGuardPositionMarginShadowV2) bool {
	if shadow == nil || shadow.Status == CopyGuardPositionMarginShadowActive || shadow.CrossedPrice <= 0 {
		return false
	}
	if shadow.Side == "long" {
		return shadow.PostCrossMaximumMark >= shadow.AnchorEntryPrice
	}
	return shadow.PostCrossMinimumMark > 0 && shadow.PostCrossMinimumMark <= shadow.AnchorEntryPrice
}

func (s *CopyTradeStore) finalizeCopyGuardPositionMarginShadowV2(cycleID int64, leaderClosePrice float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	shadow, err := getPositionMarginShadowV2(tx.QueryRow, cycleID)
	if err == sql.ErrNoRows || (err == nil && shadow.Status == CopyGuardPositionMarginShadowFinalized) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	gross, exitTurnover, exitPrice := shadow.RealizedGrossPnL, shadow.ExitTurnover, shadow.CrossedPrice
	stopCrossed := shadow.Status == CopyGuardPositionMarginShadowCrossed
	missingClosePrice := false
	if !stopCrossed {
		if invalidPositiveNumber(leaderClosePrice) {
			// Shadow bookkeeping must never keep a closed trading lifecycle open.
			// Preserve the best observable valuation for diagnostics, but exclude
			// the result from scoring because the leader's actual close is absent.
			missingClosePrice = true
			leaderClosePrice = shadow.LastMark
			if invalidPositiveNumber(leaderClosePrice) {
				leaderClosePrice = shadow.CurrentEntryPrice
			}
		}
		gross += shadowDirectionalPnL(shadow.Side, shadow.CurrentEntryPrice, leaderClosePrice, shadow.CurrentQuantity)
		exitTurnover += leaderClosePrice * shadow.CurrentQuantity
		exitPrice = leaderClosePrice
	}
	coverage := shadowV2MarkCoverage(shadow)
	quality := CopyGuardShadowQualityEstimated
	status := CopyGuardShadowScorable
	reason := "v2 virtual ledger includes weighted adds, realized reductions and the remaining leader-close exit; configured BPS costs are provisional"
	if stopCrossed {
		reason = "v2 virtual ledger froze the full remaining position at the first authoritative fixed-stop crossing; configured BPS costs are provisional"
	}
	if missingClosePrice || coverage < .95 || shadow.MarkObservationCount == 0 || shadow.UnscorableReason != "" {
		quality = CopyGuardShadowQualityUnscorable
		status = CopyGuardShadowUnscorable
		reason = strings.TrimSpace(shadow.UnscorableReason)
		if reason == "" && missingClosePrice {
			reason = "leader close price is unavailable; diagnostic valuation used the last mark and cannot be scored"
		}
		if reason == "" {
			reason = fmt.Sprintf("mark path coverage %.2f%% is below 95%%", coverage*100)
		}
	}
	turnover := shadow.EntryTurnover + exitTurnover
	// ConfiguredCostBPS is the existing full round-trip fee plus slippage
	// budget. EntryTurnover+ExitTurnover counts both legs, so applying the
	// round-trip rate to the raw two-sided turnover would charge it twice.
	cost := turnover * shadow.ConfiguredCostBPS / 20000
	evaluation := &CopyGuardShadowEvaluation{
		CycleID: shadow.CycleID, TraderID: shadow.TraderID,
		Policy: CopyGuardShadowFirstEntryPositionMargin80, EvaluationVersion: CopyGuardPositionMarginShadowEvaluationVersion,
		Status: status, DataQuality: quality, GrossPnL: gross, EstimatedCost: cost, NetPnL: gross - cost,
		SizeFactor: 1, EntryPrice: shadow.AnchorEntryPrice, ExitPrice: exitPrice, Reason: reason,
		BaselinePolicy: CopyGuardShadowCurrentStop, CostSource: "CONFIGURED_BPS",
		MarkCoverage: coverage, StopCrossed: stopCrossed,
		CrossingVerified: stopCrossed && (shadow.CrossingSource == "LIVE_MARK" || shadow.CrossingSource == "HISTORY_MARK_1M"),
		MinimumMark:      shadow.MinimumMark, MaximumMark: shadow.MaximumMark,
		PostStopReversed: shadowV2PostStopReversed(shadow), MinimumLeverage: shadow.MinimumLeverage,
		MaximumLeverage: shadow.MaximumLeverage, MinimumNotional: shadow.MinimumNotional, MaximumNotional: shadow.MaximumNotional,
	}
	if err = upsertCopyGuardShadowEvaluation(tx, evaluation); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_position_margin_shadow_v2 SET
		status='FINALIZED',realized_gross_pnl=?,exit_turnover=?,current_quantity=0,
		updated_at=CURRENT_TIMESTAMP WHERE cycle_id=?`, gross, exitTurnover, cycleID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileCopyGuardPositionMarginShadowV2 replaces provisional configured
// costs with observed exchange costs once the real cycle has reconciled, and
// stores the incremental effect against the actual ATR/current-stop path.
func (s *CopyTradeStore) ReconcileCopyGuardPositionMarginShadowV2(cycle *CopyGuardCycle, baselineNetPnL float64, baselineAvailable bool) error {
	if cycle == nil || cycle.ID <= 0 {
		return fmt.Errorf("invalid position-margin shadow v2 reconciliation")
	}
	shadow, err := s.GetCopyGuardPositionMarginShadowV2(cycle.ID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if shadow.Status != CopyGuardPositionMarginShadowFinalized {
		return nil
	}
	rows, err := s.ListCopyGuardShadowEvaluations(cycle.ID)
	if err != nil {
		return err
	}
	var evaluation *CopyGuardShadowEvaluation
	for _, row := range rows {
		if row.Policy == CopyGuardShadowFirstEntryPositionMargin80 && row.EvaluationVersion == CopyGuardPositionMarginShadowEvaluationVersion {
			evaluation = row
			break
		}
	}
	if evaluation == nil {
		return fmt.Errorf("position-margin shadow v2 evaluation is missing")
	}
	var realTurnover, realEntryTurnover float64
	if err = s.db.QueryRow(`SELECT
		COALESCE(SUM(ABS(filled_notional)),0),
		COALESCE(SUM(CASE WHEN action IN ('open_long','open_short') THEN ABS(filled_notional) ELSE 0 END),0)
		FROM copy_trade_execution_intents WHERE cycle_id=? AND filled_quantity>0`, cycle.ID).Scan(&realTurnover, &realEntryTurnover); err != nil {
		return err
	}
	shadowTurnover := shadow.EntryTurnover + shadow.ExitTurnover
	if realTurnover > 0 {
		observedTradingCosts := math.Abs(cycle.Fees) + math.Abs(cycle.Slippage) + math.Abs(cycle.LiquidationPenalty)
		evaluation.EstimatedCost = observedTradingCosts * shadowTurnover / realTurnover
		fundingScale := 1.0
		if realEntryTurnover > 0 {
			fundingScale = shadow.EntryTurnover / realEntryTurnover
		}
		evaluation.NetPnL = evaluation.GrossPnL - evaluation.EstimatedCost + cycle.FundingFee*fundingScale
		evaluation.CostSource = "OBSERVED_PRORATED"
		if realTurnover > 0 {
			evaluation.SlippageBPS = math.Abs(cycle.Slippage) / realTurnover * 10000
		}
	}
	evaluation.BaselinePolicy = CopyGuardShadowCurrentStop
	evaluation.BaselineNetPnL = baselineNetPnL
	evaluation.IncrementalEffect = evaluation.NetPnL - baselineNetPnL
	if cycle.AccountingStatus == CopyGuardAccountingUnscorable || cycle.AccountingStatus == CopyGuardAccountingLegacyUnverified {
		evaluation.Status = CopyGuardShadowUnscorable
		evaluation.DataQuality = CopyGuardShadowQualityUnscorable
		evaluation.Reason = "real cycle cannot provide a scoreable comparison baseline: " + cycle.AccountingError
	} else if !baselineAvailable || cycle.AccountingStatus != CopyGuardAccountingReconciled {
		evaluation.Status = CopyGuardShadowUnscorable
		evaluation.DataQuality = CopyGuardShadowQualityUnscorable
		evaluation.Reason = "real current-stop comparison baseline is unavailable"
	} else if evaluation.MarkCoverage >= .95 && (!evaluation.StopCrossed || evaluation.CrossingVerified) && evaluation.CostSource == "OBSERVED_PRORATED" {
		evaluation.Status = CopyGuardShadowScorable
		evaluation.DataQuality = CopyGuardShadowQualityVerified
		evaluation.Reason += "; reconciled exchange costs were prorated by turnover and compared with the actual current-stop result"
	} else if evaluation.CostSource != "OBSERVED_PRORATED" {
		evaluation.Status = CopyGuardShadowUnscorable
		evaluation.DataQuality = CopyGuardShadowQualityEstimated
		evaluation.Reason += "; exchange turnover/cost evidence is incomplete, so configured BPS estimates cannot enter promotion statistics"
	}
	return s.SaveCopyGuardShadowEvaluation(evaluation)
}

func (s *CopyTradeStore) MarkCopyGuardPositionMarginShadowV2Unscorable(cycleID int64, reason string) error {
	if cycleID <= 0 || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("invalid position-margin shadow v2 unscorable reason")
	}
	_, err := s.db.Exec(`UPDATE copy_guard_position_margin_shadow_v2 SET
		data_quality='UNSCORABLE',unscorable_reason=CASE WHEN unscorable_reason='' THEN ? ELSE unscorable_reason END,
		updated_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND status<>'FINALIZED'`, reason, cycleID)
	return err
}

const positionMarginShadowV2Select = `SELECT cycle_id,trader_id,status,side,
	anchor_entry_price,anchor_leverage,anchor_initial_margin,anchor_stop_price,
	configured_margin_loss_pct,price_tick_size,quantity_step,initial_quantity,
	current_entry_price,current_quantity,current_leverage,effective_stop_price,last_leader_size,
	realized_gross_pnl,entry_turnover,exit_turnover,minimum_leverage,maximum_leverage,
	minimum_notional,maximum_notional,configured_cost_bps,crossed_price,crossed_quantity,
	crossed_effective_margin_loss_pct,crossing_source,crossed_at,minimum_mark,maximum_mark,last_mark,
	post_cross_minimum_mark,post_cross_maximum_mark,mark_covered_seconds,mark_gap_seconds,
	mark_observation_count,first_mark_at,last_mark_at,data_quality,unscorable_reason,created_at,updated_at
	FROM copy_guard_position_margin_shadow_v2`

type positionMarginShadowV2RowQuery func(query string, args ...interface{}) *sql.Row

func getPositionMarginShadowV2(query positionMarginShadowV2RowQuery, cycleID int64) (*CopyGuardPositionMarginShadowV2, error) {
	return scanPositionMarginShadowV2(func(dest ...interface{}) error {
		return query(positionMarginShadowV2Select+` WHERE cycle_id=?`, cycleID).Scan(dest...)
	})
}

func scanPositionMarginShadowV2(scan func(dest ...interface{}) error) (*CopyGuardPositionMarginShadowV2, error) {
	var shadow CopyGuardPositionMarginShadowV2
	var crossedAt, firstMarkAt, lastMarkAt sql.NullString
	var createdAt, updatedAt string
	if err := scan(&shadow.CycleID, &shadow.TraderID, &shadow.Status, &shadow.Side,
		&shadow.AnchorEntryPrice, &shadow.AnchorLeverage, &shadow.AnchorInitialMargin,
		&shadow.AnchorStopPrice, &shadow.ConfiguredMarginLossPct, &shadow.PriceTickSize,
		&shadow.QuantityStep, &shadow.InitialQuantity, &shadow.CurrentEntryPrice,
		&shadow.CurrentQuantity, &shadow.CurrentLeverage, &shadow.EffectiveStopPrice,
		&shadow.LastLeaderSize, &shadow.RealizedGrossPnL, &shadow.EntryTurnover,
		&shadow.ExitTurnover, &shadow.MinimumLeverage, &shadow.MaximumLeverage,
		&shadow.MinimumNotional, &shadow.MaximumNotional, &shadow.ConfiguredCostBPS,
		&shadow.CrossedPrice, &shadow.CrossedQuantity, &shadow.CrossedEffectiveMarginLossPct,
		&shadow.CrossingSource, &crossedAt, &shadow.MinimumMark, &shadow.MaximumMark,
		&shadow.LastMark, &shadow.PostCrossMinimumMark, &shadow.PostCrossMaximumMark,
		&shadow.MarkCoveredSeconds, &shadow.MarkGapSeconds, &shadow.MarkObservationCount,
		&firstMarkAt, &lastMarkAt, &shadow.DataQuality, &shadow.UnscorableReason,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	var err error
	if shadow.CrossedAt, err = parseNullableDBTime(crossedAt); err != nil {
		return nil, err
	}
	if shadow.FirstMarkAt, err = parseNullableDBTime(firstMarkAt); err != nil {
		return nil, err
	}
	if shadow.LastMarkAt, err = parseNullableDBTime(lastMarkAt); err != nil {
		return nil, err
	}
	if shadow.CreatedAt, err = parseDBTime(createdAt); err != nil {
		return nil, err
	}
	if shadow.UpdatedAt, err = parseDBTime(updatedAt); err != nil {
		return nil, err
	}
	return &shadow, nil
}
