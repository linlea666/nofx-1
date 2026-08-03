package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"nofx/logger"
)

// ============================================================================
// 跟单事件日志（Copy Trade Event Log）
//
// provider 无关的统一事件时间线，把开仓/加仓/减仓/平仓、止损介入、二次入场、
// 人工接手、AI 接收、保护降级、强平兜底、对账等全链条离散事件汇聚成一张可查询、
// 可过滤、可导出、可追溯的轻量表，用于监控异常与定位问题。
//
// 设计要点：
//   - 写入 best-effort：写入器在业务层 log-and-continue，绝不影响交易主链路。
//   - 幂等：dedup_key 非空时受部分唯一索引约束（配合 INSERT OR IGNORE），
//     3s 轮询与重启重放同一 fill 的相同结果不产生重复行。
//   - 轻量：只记录离散生命周期事件，不记录 watch_samples 等高频明细
//     （细致的单仓位全程记录仍在 copy_guard_cycles/attempts/watch_samples）。
// ============================================================================

// 事件分类（category）
const (
	CopyEventCategoryAction     = "action"     // 开仓/加仓/减仓/平仓
	CopyEventCategoryStopLoss   = "stoploss"   // 账户保护止损触发
	CopyEventCategoryReentry    = "reentry"    // 二次入场（自动/请求/成交/失败）
	CopyEventCategoryTakeover   = "takeover"   // 人工接手 / AI 接收
	CopyEventCategoryProtection = "protection" // 保护挂单降级/钳紧/恢复/无法保护/强平兜底
	CopyEventCategoryReconcile  = "reconcile"  // 对账/基线校准
	CopyEventCategoryError      = "error"      // 执行失败等异常
)

// 严重度（severity）
const (
	CopyEventSeverityInfo  = "info"
	CopyEventSeverityWarn  = "warn"
	CopyEventSeverityError = "error"
)

// 动作事件类型（event_type，category=action）
const (
	CopyEventTypeOpen   = "OPEN"
	CopyEventTypeAdd    = "ADD"
	CopyEventTypeReduce = "REDUCE"
	CopyEventTypeClose  = "CLOSE"
)

// CopyTradeEvent 一条跟单事件
type CopyTradeEvent struct {
	ID            int64                  `json:"id"`
	TraderID      string                 `json:"trader_id"`
	TraderName    string                 `json:"trader_name,omitempty"` // 查询时回填，不落库
	LeaderID      string                 `json:"leader_id"`
	ProviderType  string                 `json:"provider_type"` // okx | hyperliquid | binance
	Category      string                 `json:"category"`
	EventType     string                 `json:"event_type"`
	Severity      string                 `json:"severity"`
	Symbol        string                 `json:"symbol"`
	Side          string                 `json:"side"`
	MarginMode    string                 `json:"margin_mode"`
	LeaderPosID   string                 `json:"leader_pos_id"`
	FollowerPosID string                 `json:"follower_pos_id"`
	CycleID       int64                  `json:"cycle_id"` // 关联 copy_guard_cycles（Copy Guard 数据源 OKX/Binance 有值，其它为 0）
	SignalID      string                 `json:"signal_id"`
	Status        string                 `json:"status"` // success | failed | skipped | ""
	Price         float64                `json:"price"`
	Quantity      float64                `json:"quantity"`
	Notional      float64                `json:"notional"`
	PnL           float64                `json:"pnl"`
	Operator      string                 `json:"operator"` // 人工 user_id | ai:auto | ai
	Summary       string                 `json:"summary"`
	Detail        map[string]interface{} `json:"detail,omitempty"`
	DedupKey      string                 `json:"-"` // 幂等键，非空时受部分唯一索引约束
	CreatedAt     time.Time              `json:"created_at"`
}

// CopyEventFilter 查询过滤条件（空字段=不过滤）
type CopyEventFilter struct {
	Provider  string
	Category  string
	Severity  string
	Symbol    string
	EventType string
}

func (s *CopyTradeStore) initCopyEventTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL,
			leader_id TEXT DEFAULT '',
			provider_type TEXT DEFAULT '',
			category TEXT NOT NULL,
			event_type TEXT NOT NULL,
			severity TEXT NOT NULL DEFAULT 'info',
			symbol TEXT DEFAULT '',
			side TEXT DEFAULT '',
			margin_mode TEXT DEFAULT '',
			leader_pos_id TEXT DEFAULT '',
			follower_pos_id TEXT DEFAULT '',
			cycle_id INTEGER DEFAULT 0,
			signal_id TEXT DEFAULT '',
			status TEXT DEFAULT '',
			price REAL DEFAULT 0,
			quantity REAL DEFAULT 0,
			notional REAL DEFAULT 0,
			pnl REAL DEFAULT 0,
			operator TEXT DEFAULT '',
			summary TEXT DEFAULT '',
			detail_json TEXT DEFAULT '{}',
			dedup_key TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}
	// 查询索引：按 trader / category 过滤且按时间倒序分页。
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_copy_events_trader_time ON copy_trade_events(trader_id, created_at)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_copy_events_cat_time ON copy_trade_events(category, created_at)`)
	// 幂等：仅对非空 dedup_key 生效的部分唯一索引，配合 INSERT OR IGNORE。
	s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_copy_events_dedup ON copy_trade_events(dedup_key) WHERE dedup_key <> ''`)
	return nil
}

// migratePositionMarginStops v1 turns on the 50% initial-margin loss cap only
// for legacy configurations that already enabled the global stop-loss switch
// but had never enabled the position cap. Every row is then version-bookmarked
// in the same transaction so explicit 20%/50% settings and globally-disabled
// settings cannot be reconsidered on a later restart.
func (s *CopyTradeStore) migratePositionMarginStops() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type migrationTarget struct {
		traderID, leaderID, providerType string
		previousMaxLoss                  float64
	}
	rows, err := tx.Query(`
		SELECT trader_id, leader_id, provider_type, COALESCE(risk_leverage_max_loss, 0)
		FROM copy_trade_configs
		WHERE COALESCE(risk_margin_stop_migration_version, 0) < ?
		  AND risk_stop_loss_enabled = 1
		  AND risk_leverage_fallback = 0
	`, positionMarginStopMigrationVersion)
	if err != nil {
		return err
	}
	var targets []migrationTarget
	for rows.Next() {
		var target migrationTarget
		if err = rows.Scan(&target.traderID, &target.leaderID, &target.providerType, &target.previousMaxLoss); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, target)
	}
	if err = rows.Close(); err != nil {
		return err
	}

	for _, target := range targets {
		result, updateErr := tx.Exec(`
			UPDATE copy_trade_configs
			SET risk_leverage_fallback = 1,
			    risk_leverage_max_loss = 0.50,
			    risk_margin_stop_migration_version = ?
			WHERE trader_id = ?
			  AND COALESCE(risk_margin_stop_migration_version, 0) < ?
			  AND risk_stop_loss_enabled = 1
			  AND risk_leverage_fallback = 0
		`, positionMarginStopMigrationVersion, target.traderID, positionMarginStopMigrationVersion)
		if updateErr != nil {
			return updateErr
		}
		affected, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return affectedErr
		}
		if affected != 1 {
			return fmt.Errorf("position margin stop migration changed %d rows for trader %s", affected, target.traderID)
		}
		detailJSON, marshalErr := json.Marshal(map[string]interface{}{
			"migration_version":       positionMarginStopMigrationVersion,
			"previous_enabled":        false,
			"previous_max_loss_ratio": target.previousMaxLoss,
			"new_enabled":             true,
			"new_max_loss_ratio":      0.50,
		})
		if marshalErr != nil {
			return marshalErr
		}
		if _, insertErr := tx.Exec(`
			INSERT OR IGNORE INTO copy_trade_events
				(trader_id, leader_id, provider_type, category, event_type, severity,
				 status, operator, summary, detail_json, dedup_key)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, target.traderID, target.leaderID, target.providerType,
			CopyEventCategoryReconcile, "POSITION_MARGIN_STOP_MIGRATED", CopyEventSeverityInfo,
			"success", "system:migration", "历史仓位止损已迁移为初始保证金亏损 50%", string(detailJSON),
			fmt.Sprintf("position_margin_stop_migration|%s|v%d", target.traderID, positionMarginStopMigrationVersion)); insertErr != nil {
			return insertErr
		}
	}

	// Rows that were already explicitly configured, or whose global stop-loss
	// is disabled, keep every business value unchanged and only receive the
	// audit bookmark.
	if _, err = tx.Exec(`
		UPDATE copy_trade_configs
		SET risk_margin_stop_migration_version = ?
		WHERE COALESCE(risk_margin_stop_migration_version, 0) < ?
	`, positionMarginStopMigrationVersion, positionMarginStopMigrationVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// LogCopyEvent 写入一条事件（INSERT OR IGNORE 保证 dedup_key 幂等）。
func (s *CopyTradeStore) LogCopyEvent(e *CopyTradeEvent) error {
	return s.LogCopyEventContext(context.Background(), e)
}

// LogCopyEventContext is the bounded variant used by ancillary observers such
// as email delivery. Core execution callers keep the existing API above.
func (s *CopyTradeStore) LogCopyEventContext(ctx context.Context, e *CopyTradeEvent) error {
	if e == nil {
		return fmt.Errorf("nil copy trade event")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if e.Category == "" || e.EventType == "" {
		return fmt.Errorf("copy trade event missing category/event_type")
	}
	if e.Severity == "" {
		e.Severity = CopyEventSeverityInfo
	}
	detailJSON := "{}"
	if len(e.Detail) > 0 {
		if b, err := json.Marshal(e.Detail); err == nil {
			detailJSON = string(b)
		}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO copy_trade_events
			(trader_id, leader_id, provider_type, category, event_type, severity,
			 symbol, side, margin_mode, leader_pos_id, follower_pos_id, cycle_id,
			 signal_id, status, price, quantity, notional, pnl, operator, summary,
			 detail_json, dedup_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, e.TraderID, e.LeaderID, e.ProviderType, e.Category, e.EventType, e.Severity,
		e.Symbol, e.Side, e.MarginMode, e.LeaderPosID, e.FollowerPosID, e.CycleID,
		e.SignalID, e.Status, e.Price, e.Quantity, e.Notional, e.PnL, e.Operator, e.Summary,
		detailJSON, e.DedupKey)
	return err
}

// QueryCopyEvents 按 trader 集合 + 时间窗 + 过滤条件分页查询（时间倒序）。
func (s *CopyTradeStore) QueryCopyEvents(traderIDs []string, from, to time.Time, filter CopyEventFilter, limit, offset int) ([]*CopyTradeEvent, error) {
	if len(traderIDs) == 0 {
		return []*CopyTradeEvent{}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q, args := s.buildCopyEventQuery(traderIDs, from, to, filter)
	q += " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyTradeEvent{}
	for rows.Next() {
		var e CopyTradeEvent
		var detailRaw, created string
		if err := rows.Scan(
			&e.ID, &e.TraderID, &e.LeaderID, &e.ProviderType, &e.Category, &e.EventType, &e.Severity,
			&e.Symbol, &e.Side, &e.MarginMode, &e.LeaderPosID, &e.FollowerPosID, &e.CycleID,
			&e.SignalID, &e.Status, &e.Price, &e.Quantity, &e.Notional, &e.PnL, &e.Operator, &e.Summary,
			&detailRaw, &created,
		); err != nil {
			return nil, err
		}
		if detailRaw != "" && detailRaw != "{}" {
			_ = json.Unmarshal([]byte(detailRaw), &e.Detail)
		}
		if e.CreatedAt, err = parseDBTime(created); err != nil {
			return nil, fmt.Errorf("copy trade event %d created_at: %w", e.ID, err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountCopyEvents 统计满足条件的总行数（用于分页）。
func (s *CopyTradeStore) CountCopyEvents(traderIDs []string, from, to time.Time, filter CopyEventFilter) (int, error) {
	if len(traderIDs) == 0 {
		return 0, nil
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+2)
	for _, id := range traderIDs {
		args = append(args, id)
	}
	args = append(args, from.UTC().Format("2006-01-02 15:04:05"), to.UTC().Format("2006-01-02 15:04:05"))
	q := `SELECT COUNT(*) FROM copy_trade_events WHERE trader_id IN (` + marks + `) AND created_at>=? AND created_at<?`
	q, args = appendCopyEventFilter(q, args, filter)
	var n int
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

const copyEventSelect = `SELECT id,trader_id,leader_id,provider_type,category,event_type,severity,symbol,side,margin_mode,leader_pos_id,follower_pos_id,cycle_id,signal_id,status,price,quantity,notional,pnl,operator,summary,detail_json,created_at FROM copy_trade_events`

func (s *CopyTradeStore) buildCopyEventQuery(traderIDs []string, from, to time.Time, filter CopyEventFilter) (string, []interface{}) {
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+6)
	for _, id := range traderIDs {
		args = append(args, id)
	}
	args = append(args, from.UTC().Format("2006-01-02 15:04:05"), to.UTC().Format("2006-01-02 15:04:05"))
	q := copyEventSelect + ` WHERE trader_id IN (` + marks + `) AND created_at>=? AND created_at<?`
	return appendCopyEventFilter(q, args, filter)
}

func appendCopyEventFilter(q string, args []interface{}, filter CopyEventFilter) (string, []interface{}) {
	if filter.Provider != "" {
		q += " AND provider_type=?"
		args = append(args, filter.Provider)
	}
	if filter.Category != "" {
		q += " AND category=?"
		args = append(args, filter.Category)
	}
	if filter.Severity != "" {
		q += " AND severity=?"
		args = append(args, filter.Severity)
	}
	if filter.Symbol != "" {
		q += " AND symbol=?"
		args = append(args, filter.Symbol)
	}
	if filter.EventType != "" {
		q += " AND event_type=?"
		args = append(args, filter.EventType)
	}
	return q, args
}

// ============================================================================
// Seam B：把 Copy Guard 风控/保护/接手/对账事件镜像到统一跟单事件日志
//
// copy_guard_events 是 Copy Guard 数据源（OKX/Binance）按 cycle 归属的风控
// 事件流。它有两类写入源：
//   1. SaveCopyGuardEvent（多数事件）
//   2. RecordCopyGuardStop / RecordCopyGuardStopObserved /
//      RecordCopyGuardReentryFilled（在事务内直接 INSERT，STOP_TRIGGERED /
//      STOP_CONFIRMED / REENTRY_FILLED 只走这里）
//
// 为避免遗漏最关键的止损/二次入场事件，所有写入源都在提交后调用本方法
// （提取复用：白名单与摘要集中一处），post-commit + best-effort，永不影响
// Copy Guard 事务与既有告警。高频/内部明细类型（WATCH_SUMMARY /
// REENTRY_GATE_CHANGED / 身份捕获等）不镜像，保持日志轻量。
// ============================================================================

type CopyGuardEventSpec struct {
	Category, Severity, EmailLevel string
	Mirror                         bool
}

var copyGuardEventSpecs = map[string]CopyGuardEventSpec{
	"STOP_TRIGGERED":               {CopyEventCategoryStopLoss, CopyEventSeverityWarn, "important", true},
	"STOP_CONFIRMED":               {CopyEventCategoryStopLoss, CopyEventSeverityWarn, "important", true},
	"STOP_PENDING_FLAT":            {CopyEventCategoryStopLoss, CopyEventSeverityWarn, "verbose", true},
	"STOP_PARTIAL":                 {CopyEventCategoryStopLoss, CopyEventSeverityError, "critical", true},
	"STOP_FLAT_CONFIRMED":          {CopyEventCategoryStopLoss, CopyEventSeverityWarn, "important", true},
	"STOP_DUST_RESIDUAL":           {CopyEventCategoryStopLoss, CopyEventSeverityError, "critical", true},
	"STOP_RISK_THRESHOLD_EXCEEDED": {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"CATCHUP_TIMEOUT":              {CopyEventCategoryAction, CopyEventSeverityWarn, "important", false},
	"CATCHUP_PRICE_LIMIT":          {CopyEventCategoryAction, CopyEventSeverityWarn, "important", false},
	"AI_CANDIDATE_CREATED":         {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"AI_CANDIDATE_UNACTIONABLE":    {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"REENTRY_SUBMITTED":            {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"AI_REVIEW_WAIT":               {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"AI_REVIEW_REQUESTED":          {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"AI_REVIEW_ENTER":              {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"AI_REVIEW_ABANDON":            {CopyEventCategoryTakeover, CopyEventSeverityWarn, "important", true},
	"AI_REVIEW_THESIS_INVALID":     {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	// 观察态：命中不改变仓位，但它是评估「是否该按此条件离场」的唯一样本来源，
	// 因此按 important 留存，不随 verbose 一起被保留策略清掉。
	"AI_CLOSE_INVALIDATION_HIT":        {CopyEventCategoryTakeover, CopyEventSeverityWarn, "important", true},
	"AI_REVIEW_FAILED":                 {CopyEventCategoryTakeover, CopyEventSeverityWarn, "important", true},
	"AI_BUDGET_SUSPENDED":              {CopyEventCategoryTakeover, CopyEventSeverityWarn, "important", true},
	"AI_RESULT_STALE":                  {CopyEventCategoryTakeover, CopyEventSeverityWarn, "verbose", true},
	"AI_ENTRY_LEASE_WAITING_PRICE":     {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"ENTER_WINDOW_EXPIRED":             {CopyEventCategoryTakeover, CopyEventSeverityWarn, "important", true},
	"AI_CANDIDATE_TERMINATED":          {CopyEventCategoryTakeover, CopyEventSeverityWarn, "important", true},
	"AI_ANALYSIS":                      {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"AI_DECISION_OUTCOME_FINALIZED":    {CopyEventCategoryTakeover, CopyEventSeverityInfo, "", true},
	"AI_CANDIDATE_OUTCOME_FINALIZED":   {CopyEventCategoryTakeover, CopyEventSeverityInfo, "", true},
	"REENTRY_PREFLIGHT_REJECTED":       {CopyEventCategoryReentry, CopyEventSeverityWarn, "verbose", true},
	"REENTRY_REQUESTED":                {CopyEventCategoryReentry, CopyEventSeverityInfo, "verbose", true},
	"REENTRY_FILLED":                   {CopyEventCategoryReentry, CopyEventSeverityInfo, "important", true},
	"REENTRY_FILL_INCREMENT":           {CopyEventCategoryReentry, CopyEventSeverityInfo, "verbose", true},
	"REENTRY_RECOVERED_AFTER_RESTART":  {CopyEventCategoryReentry, CopyEventSeverityInfo, "important", true},
	"REENTRY_WINDOW_COLLAPSED":         {CopyEventCategoryReentry, CopyEventSeverityWarn, "important", true},
	"REENTRY_FAILED":                   {CopyEventCategoryReentry, CopyEventSeverityError, "important", true},
	"REENTRY_RECOVERY_PENDING":         {CopyEventCategoryReentry, CopyEventSeverityWarn, "important", true},
	"GUARD_MANUAL_REENTRY_SIGNAL":      {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"GUARD_MANUAL_REENTRY_CONFIRMED":   {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"GUARD_MANUAL_REENTRY_DISMISSED":   {CopyEventCategoryTakeover, CopyEventSeverityInfo, "verbose", true},
	"PROTECTION_RECOVERED":             {CopyEventCategoryProtection, CopyEventSeverityInfo, "verbose", true},
	"PROTECTIVE_STOP_ACTIVE":           {CopyEventCategoryProtection, CopyEventSeverityInfo, "verbose", true},
	"PROTECTION_PENDING":               {CopyEventCategoryProtection, CopyEventSeverityInfo, "verbose", true},
	"PROTECTION_ACTIVE":                {CopyEventCategoryProtection, CopyEventSeverityInfo, "verbose", true},
	"PROTECTION_DEGRADED":              {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"PROTECTION_CLAMPED":               {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"PROTECTION_COVERAGE_LOW":          {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"PROTECTION_COVERAGE_UNATTRIBUTED": {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"PROTECTION_RETRY":                 {CopyEventCategoryProtection, CopyEventSeverityWarn, "verbose", true},
	"PROTECTION_RETRY_THROTTLED":       {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"PROTECTION_BOOKKEEPING_HEALED":    {CopyEventCategoryProtection, CopyEventSeverityInfo, "verbose", true},
	"PROTECTIVE_STOP_GONE":             {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"PROTECTION_VERIFY_UNKNOWN":        {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"ADDON_RISK_WARNING":               {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"ADDON_RISK_SHRUNK":                {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"FORCED_EXIT":                      {CopyEventCategoryProtection, CopyEventSeverityWarn, "important", true},
	"PROTECTION_CREATE_FAILED":         {CopyEventCategoryProtection, CopyEventSeverityError, "critical", true},
	"GUARD_UNPROTECTABLE":              {CopyEventCategoryProtection, CopyEventSeverityError, "critical", true},
	"GUARD_FORCED_EXIT":                {CopyEventCategoryProtection, CopyEventSeverityError, "critical", true},
	"GUARD_FORCED_EXIT_FAILED":         {CopyEventCategoryProtection, CopyEventSeverityError, "critical", true},
	"ACCOUNTING_RECONCILED":            {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", true},
	"ATTEMPT_RECONCILED":               {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", true},
	"WATCH_SUMMARY":                    {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", false},
	"BASELINE_CALIBRATED":              {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", true},
	"MAPPING_OWNERSHIP_RECOVERED":      {CopyEventCategoryReconcile, CopyEventSeverityWarn, "important", true},
	"OWNERSHIP_AMBIGUOUS":              {CopyEventCategoryReconcile, CopyEventSeverityError, "critical", true},
	"OWNERSHIP_GAP_FLAT_RETIRED":       {CopyEventCategoryReconcile, CopyEventSeverityWarn, "important", true},
	"SUPERSEDED_BY_RECOVERED_POSITION": {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", true},
	"LEADER_CLOSED":                    {CopyEventCategoryReconcile, CopyEventSeverityInfo, "important", true},
	"CYCLE_CLOSED_SUMMARY":             {CopyEventCategoryReconcile, CopyEventSeverityInfo, "important", true},
	"CYCLE_SUMMARY_EMAIL_QUEUED":       {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", false},
	"CYCLE_SUMMARY_EMAIL_RATE_LIMITED": {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", false},
	"CYCLE_SUMMARY_EMAIL_DEDUPED":      {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", false},
	"CYCLE_SUMMARY_EMAIL_SENT":         {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", false},
	"CYCLE_SUMMARY_EMAIL_FAILED":       {CopyEventCategoryReconcile, CopyEventSeverityWarn, "important", false},
	"CYCLE_SUMMARY_EMAIL_DROPPED":      {CopyEventCategoryReconcile, CopyEventSeverityWarn, "important", false},
	"CYCLE_SUMMARY_EMAIL_DISABLED":     {CopyEventCategoryReconcile, CopyEventSeverityInfo, "verbose", false},
	"LEADER_REVERSED":                  {CopyEventCategoryReconcile, CopyEventSeverityWarn, "important", true},
	"ACCOUNTING_DELAYED":               {CopyEventCategoryReconcile, CopyEventSeverityWarn, "important", true},
	"ACCOUNTING_UNRECOVERABLE":         {CopyEventCategoryReconcile, CopyEventSeverityError, "critical", true},
}

func classifyGuardEvent(eventType string) (category, severity string, include bool) {
	s, ok := copyGuardEventSpecs[eventType]
	return s.Category, s.Severity, ok && s.Mirror
}

func GetCopyGuardEventSpec(eventType string) (CopyGuardEventSpec, bool) {
	s, ok := copyGuardEventSpecs[eventType]
	return s, ok
}

func ShouldSendCopyGuardEmail(configLevel, eventType string) bool {
	spec, ok := copyGuardEventSpecs[eventType]
	if !ok || spec.EmailLevel == "" {
		return false
	}
	switch configLevel {
	case "verbose":
		return true
	case "critical":
		return spec.EmailLevel == "critical"
	default: // important is the v7 default and safe fallback for old configs.
		return spec.EmailLevel == "important" || spec.EmailLevel == "critical"
	}
}

// copyGuardTraderProvider 取跟单配置的数据源类型（用于事件镜像标注 provider）。
// best-effort：查询失败返回空串，调用方回退默认值。
func (s *CopyTradeStore) copyGuardTraderProvider(traderID string) string {
	var provider string
	_ = s.db.QueryRow(`SELECT provider_type FROM copy_trade_configs WHERE trader_id=?`, traderID).Scan(&provider)
	return provider
}

// copyGuardCycleContext 取镜像所需的少量 cycle 上下文（仅白名单命中时调用，罕见）。
func (s *CopyTradeStore) copyGuardCycleContext(cycleID int64) (symbol, side, marginMode, leaderID, leaderPosID, followerPosID string) {
	_ = s.db.QueryRow(
		`SELECT symbol,side,margin_mode,leader_id,leader_pos_id,COALESCE(follower_pos_id,'') FROM copy_guard_cycles WHERE id=?`,
		cycleID,
	).Scan(&symbol, &side, &marginMode, &leaderID, &leaderPosID, &followerPosID)
	return
}

// mirrorGuardEventToCopyEvents 把一条 Copy Guard 事件镜像到统一跟单事件日志。
// best-effort：失败仅告警。仅在调用方事务提交成功后调用。
func (s *CopyTradeStore) mirrorGuardEventToCopyEvents(cycleID int64, traderID, eventType string, price, quantity, notional, pnl float64, metadata map[string]interface{}) {
	category, severity, ok := classifyGuardEvent(eventType)
	if !ok {
		return
	}
	symbol, side, marginMode, leaderID, leaderPosID, followerPosID := s.copyGuardCycleContext(cycleID)
	operator := metadataString(metadata, "operator")
	// Copy Guard now runs for any SupportsCopyGuard data source (OKX / Binance),
	// so the event's provider must reflect the trader's actual config rather
	// than a hard-coded value. Fall back to "okx" only if the lookup fails
	// (best-effort; events are rare and never block the guard transaction).
	providerType := "okx"
	if p := s.copyGuardTraderProvider(traderID); p != "" {
		providerType = p
	}
	ev := &CopyTradeEvent{
		TraderID:      traderID,
		LeaderID:      leaderID,
		ProviderType:  providerType,
		Category:      category,
		EventType:     eventType,
		Severity:      severity,
		Symbol:        symbol,
		Side:          side,
		MarginMode:    marginMode,
		LeaderPosID:   leaderPosID,
		FollowerPosID: followerPosID,
		CycleID:       cycleID,
		Price:         price,
		Quantity:      quantity,
		Notional:      notional,
		PnL:           pnl,
		Operator:      operator,
		Summary:       guardEventSummary(eventType, symbol, side, operator),
		Detail:        metadata,
	}
	if err := s.LogCopyEvent(ev); err != nil {
		logger.Warnf("⚠️ 镜像 Copy Guard 事件到跟单日志失败 (cycle=%d type=%s): %v", cycleID, eventType, err)
	}
}

func guardEventSummary(eventType, symbol, side, operator string) string {
	pair := strings.TrimSpace(symbol + " " + side)
	switch eventType {
	case "STOP_TRIGGERED", "STOP_CONFIRMED", "STOP_FLAT_CONFIRMED":
		return fmt.Sprintf("账户保护止损触发 | %s", pair)
	case "STOP_PENDING_FLAT":
		return fmt.Sprintf("止损单已触发，等待仓位归零 | %s", pair)
	case "STOP_PARTIAL", "STOP_DUST_RESIDUAL":
		return fmt.Sprintf("止损后仍有残仓（%s）| %s", eventType, pair)
	case "AI_CANDIDATE_CREATED":
		return fmt.Sprintf("AI 持续观察候选已创建 | %s", pair)
	case "AI_REVIEW_WAIT":
		return fmt.Sprintf("AI 继续观察 | %s", pair)
	case "AI_REVIEW_REQUESTED":
		return fmt.Sprintf("操作员请求 AI 尽快复查 | %s", pair)
	case "AI_REVIEW_ENTER":
		return fmt.Sprintf("AI 建议重入，进入确定性预检 | %s", pair)
	case "AI_REVIEW_ABANDON":
		return fmt.Sprintf("AI 建议放弃候选 | %s", pair)
	case "AI_REVIEW_THESIS_INVALID":
		return fmt.Sprintf("AI 判断当前论点失效，候选休眠等待结构恢复 | %s", pair)
	case "AI_CLOSE_INVALIDATION_HIT":
		return fmt.Sprintf("AI 收盘失效条件已命中（仅记录，未自动离场）| %s", pair)
	case "AI_REVIEW_FAILED", "AI_BUDGET_SUSPENDED", "AI_RESULT_STALE":
		return fmt.Sprintf("AI 重入审查异常（%s）| %s", eventType, pair)
	case "AI_CANDIDATE_TERMINATED":
		return fmt.Sprintf("AI 候选已由操作员终止 | %s", pair)
	case "AI_DECISION_OUTCOME_FINALIZED":
		return fmt.Sprintf("AI 单次决策后验评价已完成 | %s", pair)
	case "AI_CANDIDATE_OUTCOME_FINALIZED":
		return fmt.Sprintf("AI 候选最终效果评价已完成 | %s", pair)
	case "REENTRY_REQUESTED":
		return fmt.Sprintf("二次入场触发 | %s", pair)
	case "REENTRY_FILLED", "REENTRY_RECOVERED_AFTER_RESTART":
		return fmt.Sprintf("二次入场已成交 | %s", pair)
	case "REENTRY_FAILED":
		return fmt.Sprintf("二次入场失败 | %s", pair)
	case "REENTRY_PREFLIGHT_REJECTED":
		return fmt.Sprintf("二次入场预检拒绝，返回观察 | %s", pair)
	case "REENTRY_WINDOW_COLLAPSED":
		return fmt.Sprintf("旧规则二次入场窗口已关闭 | %s", pair)
	case "GUARD_MANUAL_REENTRY_SIGNAL":
		return fmt.Sprintf("人工重入信号已生成，等待确认 | %s", pair)
	case "GUARD_MANUAL_REENTRY_CONFIRMED":
		who := "人工接手"
		if strings.HasPrefix(operator, "ai") {
			who = "AI 接收"
		}
		return fmt.Sprintf("%s：重入已确认执行 | %s", who, pair)
	case "GUARD_MANUAL_REENTRY_DISMISSED":
		return fmt.Sprintf("人工重入信号已忽略 | %s", pair)
	case "PROTECTION_PENDING":
		return fmt.Sprintf("保护止损单建立中 | %s", pair)
	case "PROTECTIVE_STOP_ACTIVE", "PROTECTION_ACTIVE":
		return fmt.Sprintf("保护止损单已生效 | %s", pair)
	case "PROTECTION_RECOVERED":
		return fmt.Sprintf("保护止损单已恢复 | %s", pair)
	case "PROTECTION_DEGRADED", "PROTECTION_COVERAGE_LOW", "PROTECTIVE_STOP_GONE", "PROTECTION_VERIFY_UNKNOWN":
		return fmt.Sprintf("保护止损单异常（%s）| %s", eventType, pair)
	case "PROTECTION_CLAMPED":
		return fmt.Sprintf("保护止损被强平价钳紧 | %s", pair)
	case "PROTECTION_CREATE_FAILED":
		return fmt.Sprintf("保护止损单创建失败 | %s", pair)
	case "GUARD_UNPROTECTABLE":
		return fmt.Sprintf("仓位无法保护（裸跑/兜底）| %s", pair)
	case "GUARD_FORCED_EXIT":
		return fmt.Sprintf("无法保护强平兜底 | %s", pair)
	case "GUARD_FORCED_EXIT_FAILED":
		return fmt.Sprintf("强平兜底执行失败 | %s", pair)
	case "ADDON_RISK_WARNING", "ADDON_RISK_SHRUNK":
		return fmt.Sprintf("加仓风险预算已限制 | %s", pair)
	case "ACCOUNTING_RECONCILED":
		return fmt.Sprintf("周期对账完成 | %s", pair)
	case "ACCOUNTING_DELAYED":
		return fmt.Sprintf("对账数据延迟，系统重试中 | %s", pair)
	case "ACCOUNTING_UNRECOVERABLE":
		return fmt.Sprintf("对账数据不可恢复 | %s", pair)
	case "MAPPING_OWNERSHIP_RECOVERED":
		return fmt.Sprintf("跟单仓位所有权已恢复 | %s", pair)
	case "OWNERSHIP_AMBIGUOUS":
		return fmt.Sprintf("跟单仓位所有权待核验 | %s", pair)
	case "OWNERSHIP_GAP_FLAT_RETIRED":
		return fmt.Sprintf("所有权缺口已按实时空仓安全收尾 | %s", pair)
	case "SUPERSEDED_BY_RECOVERED_POSITION":
		return fmt.Sprintf("重复开仓意图已由恢复仓位取代 | %s", pair)
	case "BASELINE_CALIBRATED":
		return fmt.Sprintf("兜底基线已校准 | %s", pair)
	case "LEADER_CLOSED":
		return fmt.Sprintf("领航员已平仓 | %s", pair)
	case "LEADER_REVERSED":
		return fmt.Sprintf("领航员方向反转 | %s", pair)
	}
	return fmt.Sprintf("%s | %s", eventType, pair)
}

// StreamCopyEvents 分批回调所有满足条件的事件（时间正序，用于导出）。
func (s *CopyTradeStore) StreamCopyEvents(traderIDs []string, from, to time.Time, filter CopyEventFilter, fn func(*CopyTradeEvent) error) error {
	if len(traderIDs) == 0 {
		return nil
	}
	const batch = 500
	base, baseArgs := s.buildCopyEventQuery(traderIDs, from, to, filter)
	base += " ORDER BY created_at ASC, id ASC LIMIT ? OFFSET ?"
	for offset := 0; ; offset += batch {
		args := append(append([]interface{}{}, baseArgs...), batch, offset)
		rows, err := s.db.Query(base, args...)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			var e CopyTradeEvent
			var detailRaw, created string
			if err := rows.Scan(
				&e.ID, &e.TraderID, &e.LeaderID, &e.ProviderType, &e.Category, &e.EventType, &e.Severity,
				&e.Symbol, &e.Side, &e.MarginMode, &e.LeaderPosID, &e.FollowerPosID, &e.CycleID,
				&e.SignalID, &e.Status, &e.Price, &e.Quantity, &e.Notional, &e.PnL, &e.Operator, &e.Summary,
				&detailRaw, &created,
			); err != nil {
				rows.Close()
				return err
			}
			if detailRaw != "" && detailRaw != "{}" {
				_ = json.Unmarshal([]byte(detailRaw), &e.Detail)
			}
			if e.CreatedAt, err = parseDBTime(created); err != nil {
				rows.Close()
				return fmt.Errorf("copy trade event %d created_at: %w", e.ID, err)
			}
			if err := fn(&e); err != nil {
				rows.Close()
				return err
			}
			n++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if n < batch {
			break
		}
	}
	return nil
}
