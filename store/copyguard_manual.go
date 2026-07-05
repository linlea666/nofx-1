package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// Copy Guard v5.1 人工重入信号（manual reentry signals）
//
// 自动重入次数用尽（ATTEMPTS_EXHAUSTED）后，引擎继续复用确认式重入门控链
// 观察合格信号；出现时在此表落一条 PENDING 信号并邮件提醒用户。
// 用户在前端确认后由系统代执行（复用自动重入的执行与保护链路）。
//
// 生命周期状态机：
//   PENDING     信号有效，等待用户确认/忽略（无过期语义——人工确认最高优先，
//               仅在领航员平仓/反向等根本性变化时被 INVALIDATED）
//   EXECUTING   用户已确认，系统抢占执行中（幂等抢占：同一信号只能被确认一次）
//   EXECUTED    重入下单成功
//   FAILED      重入下单失败（error 字段带原因）
//   DISMISSED   用户手动忽略
//   INVALIDATED 领航员平仓/反向/周期终结导致信号失效
// ============================================================================

const (
	ManualReentryStatusPending     = "PENDING"
	ManualReentryStatusExecuting   = "EXECUTING"
	ManualReentryStatusExecuted    = "EXECUTED"
	ManualReentryStatusFailed      = "FAILED"
	ManualReentryStatusDismissed   = "DISMISSED"
	ManualReentryStatusInvalidated = "INVALIDATED"
)

// ManualReentryMaxSignalsPerCycle 单周期人工重入信号总量上限（防御性护栏，
// 正常情况下同周期同时只有一条 PENDING；100 实际等价于不限制）
const ManualReentryMaxSignalsPerCycle = 100

// ManualReentryAlertCooldown 同一信号的邮件提醒冷却（防轰炸；信号本身不过期）
const ManualReentryAlertCooldown = time.Hour

// CopyGuardManualReentrySignal 人工重入信号记录
type CopyGuardManualReentrySignal struct {
	ID          int64  `json:"id"`
	CycleID     int64  `json:"cycle_id"`
	TraderID    string `json:"trader_id"`
	LeaderPosID string `json:"leader_pos_id"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	MarginMode  string `json:"margin_mode"`
	Status      string `json:"status"`

	// 信号快照（引擎门控链通过时的市场状态）
	TriggerPrice        float64 `json:"trigger_price"`         // 信号触发时标记价
	ATR                 float64 `json:"atr"`                   // 信号触发时 ATR
	DistanceATRRatio    float64 `json:"distance_atr_ratio"`    // 止损距离/ATR（噪音档参考）
	ReentryBoundary     float64 `json:"reentry_boundary"`      // 重入穿越边界
	RecommendedNotional float64 `json:"recommended_notional"`  // 建议重入名义（USDT）
	StopCount           int     `json:"stop_count"`            // 周期累计止损次数
	ReentryCount        int     `json:"reentry_count"`         // 周期已自动重入次数
	LeaderSize          float64 `json:"leader_size"`           // 领航员当前持仓数量
	LeaderEntryPrice    float64 `json:"leader_entry_price"`    // 领航员当前均价
	Protectable         bool    `json:"protectable"`           // 可保护性预检（仅提示，不拦截）
	Reason              string  `json:"reason"`                // 信号说明（门控链通过摘要）

	// 确认/执行信息
	Operator     string  `json:"operator"`      // 确认操作者（user_id）
	ConfirmPrice float64 `json:"confirm_price"` // 确认时标记价
	Error        string  `json:"error"`         // 失败/失效原因

	CreatedAt   time.Time  `json:"created_at"`
	LastAlertAt *time.Time `json:"last_alert_at,omitempty"` // 上次邮件提醒时间（冷却判断）
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	ExecutedAt  *time.Time `json:"executed_at,omitempty"`

	// TraderName 展示用（API 层按所属关系填充，不落库）
	TraderName string `json:"trader_name,omitempty"`
}

func (s *CopyTradeStore) initManualReentryTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_guard_manual_reentry_signals (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			cycle_id INTEGER NOT NULL,
			trader_id TEXT NOT NULL,
			leader_pos_id TEXT NOT NULL,
			symbol TEXT NOT NULL,
			side TEXT NOT NULL,
			margin_mode TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'PENDING',
			trigger_price REAL DEFAULT 0,
			atr REAL DEFAULT 0,
			distance_atr_ratio REAL DEFAULT 0,
			reentry_boundary REAL DEFAULT 0,
			recommended_notional REAL DEFAULT 0,
			stop_count INTEGER DEFAULT 0,
			reentry_count INTEGER DEFAULT 0,
			leader_size REAL DEFAULT 0,
			leader_entry_price REAL DEFAULT 0,
			protectable BOOLEAN DEFAULT 1,
			reason TEXT DEFAULT '',
			operator TEXT DEFAULT '',
			confirm_price REAL DEFAULT 0,
			error TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_alert_at DATETIME,
			confirmed_at DATETIME,
			executed_at DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_cg_manual_signals_trader_status ON copy_guard_manual_reentry_signals(trader_id, status);
		CREATE INDEX IF NOT EXISTS idx_cg_manual_signals_cycle ON copy_guard_manual_reentry_signals(cycle_id, status);
	`)
	return err
}

const manualReentrySignalColumns = `id, cycle_id, trader_id, leader_pos_id, symbol, side, margin_mode, status,
	trigger_price, atr, distance_atr_ratio, reentry_boundary, recommended_notional,
	stop_count, reentry_count, leader_size, leader_entry_price, protectable, reason,
	operator, confirm_price, error, created_at, last_alert_at, confirmed_at, executed_at`

func scanManualReentrySignal(row rowScanner) (*CopyGuardManualReentrySignal, error) {
	var sig CopyGuardManualReentrySignal
	var created string
	var lastAlert, confirmed, executed sql.NullString
	if err := row.Scan(&sig.ID, &sig.CycleID, &sig.TraderID, &sig.LeaderPosID, &sig.Symbol, &sig.Side, &sig.MarginMode, &sig.Status,
		&sig.TriggerPrice, &sig.ATR, &sig.DistanceATRRatio, &sig.ReentryBoundary, &sig.RecommendedNotional,
		&sig.StopCount, &sig.ReentryCount, &sig.LeaderSize, &sig.LeaderEntryPrice, &sig.Protectable, &sig.Reason,
		&sig.Operator, &sig.ConfirmPrice, &sig.Error, &created, &lastAlert, &confirmed, &executed); err != nil {
		return nil, err
	}
	var err error
	if sig.CreatedAt, err = parseDBTime(created); err != nil {
		return nil, fmt.Errorf("manual reentry signal %d created_at: %w", sig.ID, err)
	}
	if sig.LastAlertAt, err = parseNullableDBTime(lastAlert); err != nil {
		return nil, fmt.Errorf("manual reentry signal %d last_alert_at: %w", sig.ID, err)
	}
	if sig.ConfirmedAt, err = parseNullableDBTime(confirmed); err != nil {
		return nil, fmt.Errorf("manual reentry signal %d confirmed_at: %w", sig.ID, err)
	}
	if sig.ExecutedAt, err = parseNullableDBTime(executed); err != nil {
		return nil, fmt.Errorf("manual reentry signal %d executed_at: %w", sig.ID, err)
	}
	return &sig, nil
}

// SaveManualReentrySignal 落一条人工重入信号。
// 幂等语义：同周期已有 PENDING 信号时不重复插入，改为刷新其市场快照字段
// （价格/ATR/边界/建议金额/领航员状态/可保护性），保留 created_at 与
// last_alert_at（邮件冷却基于首次/上次提醒时间，快照刷新不重置冷却）。
// 返回当前生效的信号行（新插入或刷新后的既有行）。
func (s *CopyTradeStore) SaveManualReentrySignal(sig *CopyGuardManualReentrySignal) (*CopyGuardManualReentrySignal, error) {
	if sig == nil {
		return nil, fmt.Errorf("nil manual reentry signal")
	}
	existing, err := s.getPendingManualReentrySignalByCycle(sig.CycleID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if existing != nil {
		_, err = s.db.Exec(`UPDATE copy_guard_manual_reentry_signals SET
			trigger_price=?, atr=?, distance_atr_ratio=?, reentry_boundary=?, recommended_notional=?,
			stop_count=?, reentry_count=?, leader_size=?, leader_entry_price=?, protectable=?, reason=?
			WHERE id=? AND status=?`,
			sig.TriggerPrice, sig.ATR, sig.DistanceATRRatio, sig.ReentryBoundary, sig.RecommendedNotional,
			sig.StopCount, sig.ReentryCount, sig.LeaderSize, sig.LeaderEntryPrice, sig.Protectable, sig.Reason,
			existing.ID, ManualReentryStatusPending)
		if err != nil {
			return nil, err
		}
		return s.GetManualReentrySignal(existing.ID)
	}
	res, err := s.db.Exec(`INSERT INTO copy_guard_manual_reentry_signals
		(cycle_id, trader_id, leader_pos_id, symbol, side, margin_mode, status,
		 trigger_price, atr, distance_atr_ratio, reentry_boundary, recommended_notional,
		 stop_count, reentry_count, leader_size, leader_entry_price, protectable, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sig.CycleID, sig.TraderID, sig.LeaderPosID, sig.Symbol, sig.Side, sig.MarginMode, ManualReentryStatusPending,
		sig.TriggerPrice, sig.ATR, sig.DistanceATRRatio, sig.ReentryBoundary, sig.RecommendedNotional,
		sig.StopCount, sig.ReentryCount, sig.LeaderSize, sig.LeaderEntryPrice, sig.Protectable, sig.Reason)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetManualReentrySignal(id)
}

func (s *CopyTradeStore) getPendingManualReentrySignalByCycle(cycleID int64) (*CopyGuardManualReentrySignal, error) {
	row := s.db.QueryRow(`SELECT `+manualReentrySignalColumns+` FROM copy_guard_manual_reentry_signals WHERE cycle_id=? AND status=? ORDER BY id DESC LIMIT 1`, cycleID, ManualReentryStatusPending)
	return scanManualReentrySignal(row)
}

// GetManualReentrySignal 按 ID 读取信号
func (s *CopyTradeStore) GetManualReentrySignal(id int64) (*CopyGuardManualReentrySignal, error) {
	row := s.db.QueryRow(`SELECT `+manualReentrySignalColumns+` FROM copy_guard_manual_reentry_signals WHERE id=?`, id)
	return scanManualReentrySignal(row)
}

// ListManualReentrySignals 列出指定 trader 集合的信号（statuses 为空 = 全部状态），
// 按创建时间倒序。用于前端待确认横幅与历史列表。
func (s *CopyTradeStore) ListManualReentrySignals(traderIDs []string, statuses []string, limit int) ([]*CopyGuardManualReentrySignal, error) {
	if len(traderIDs) == 0 {
		return []*CopyGuardManualReentrySignal{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args := make([]interface{}, 0, len(traderIDs)+len(statuses)+1)
	q := `SELECT ` + manualReentrySignalColumns + ` FROM copy_guard_manual_reentry_signals WHERE trader_id IN (?` + strings.Repeat(",?", len(traderIDs)-1) + `)`
	for _, id := range traderIDs {
		args = append(args, id)
	}
	if len(statuses) > 0 {
		q += ` AND status IN (?` + strings.Repeat(",?", len(statuses)-1) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardManualReentrySignal{}
	for rows.Next() {
		sig, err := scanManualReentrySignal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}

// ClaimManualReentrySignal 幂等抢占：PENDING → EXECUTING。
// 原子 UPDATE + RowsAffected 判定，并发确认/重复点击时只有一次成功。
func (s *CopyTradeStore) ClaimManualReentrySignal(id int64, operator string, confirmPrice float64) (bool, error) {
	res, err := s.db.Exec(`UPDATE copy_guard_manual_reentry_signals
		SET status=?, operator=?, confirm_price=?, confirmed_at=CURRENT_TIMESTAMP
		WHERE id=? AND status=?`,
		ManualReentryStatusExecuting, operator, confirmPrice, id, ManualReentryStatusPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ReleaseManualReentrySignal 抢占后确认前的回退：EXECUTING → 指定状态。
// 用途：硬校验失败时回退 PENDING（保留信号可再次确认）或 INVALIDATED
// （领航员已平仓/反向，信号根本性失效）。
func (s *CopyTradeStore) ReleaseManualReentrySignal(id int64, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_manual_reentry_signals SET status=?, error=? WHERE id=? AND status=?`,
		status, errMsg, id, ManualReentryStatusExecuting)
	return err
}

// MarkManualReentrySignalOutcome 执行结果回写：EXECUTING → EXECUTED/FAILED
func (s *CopyTradeStore) MarkManualReentrySignalOutcome(id int64, status, errMsg string) error {
	executedAt := ""
	if status == ManualReentryStatusExecuted {
		executedAt = `, executed_at=CURRENT_TIMESTAMP`
	}
	_, err := s.db.Exec(`UPDATE copy_guard_manual_reentry_signals SET status=?, error=?`+executedAt+` WHERE id=? AND status=?`,
		status, errMsg, id, ManualReentryStatusExecuting)
	return err
}

// GetExecutingManualReentrySignalByCycle 查找周期当前 EXECUTING 的信号
// （executeFullDecision 重入成败回写时定位用）
func (s *CopyTradeStore) GetExecutingManualReentrySignalByCycle(cycleID int64) (*CopyGuardManualReentrySignal, error) {
	row := s.db.QueryRow(`SELECT `+manualReentrySignalColumns+` FROM copy_guard_manual_reentry_signals WHERE cycle_id=? AND status=? ORDER BY id DESC LIMIT 1`, cycleID, ManualReentryStatusExecuting)
	return scanManualReentrySignal(row)
}

// DismissManualReentrySignal 用户忽略：PENDING → DISMISSED（幂等，非 PENDING 返回 false）
func (s *CopyTradeStore) DismissManualReentrySignal(id int64, operator string) (bool, error) {
	res, err := s.db.Exec(`UPDATE copy_guard_manual_reentry_signals SET status=?, operator=? WHERE id=? AND status=?`,
		ManualReentryStatusDismissed, operator, id, ManualReentryStatusPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// InvalidateManualReentrySignalsForCycle 周期根本性变化（领航员平仓/反向/
// 周期终结）时使所有 PENDING 信号失效
func (s *CopyTradeStore) InvalidateManualReentrySignalsForCycle(cycleID int64, reason string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_manual_reentry_signals SET status=?, error=? WHERE cycle_id=? AND status=?`,
		ManualReentryStatusInvalidated, reason, cycleID, ManualReentryStatusPending)
	return err
}

// CountManualReentrySignalsForCycle 周期内信号总数（配额护栏判断用）
func (s *CopyTradeStore) CountManualReentrySignalsForCycle(cycleID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_manual_reentry_signals WHERE cycle_id=?`, cycleID).Scan(&n)
	return n, err
}

// MarkManualReentrySignalAlerted 记录邮件提醒时间（冷却判断基准）
func (s *CopyTradeStore) MarkManualReentrySignalAlerted(id int64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_manual_reentry_signals SET last_alert_at=CURRENT_TIMESTAMP WHERE id=?`, id)
	return err
}
