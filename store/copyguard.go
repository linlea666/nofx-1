package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"
)

const (
	CopyGuardFollowing         = "FOLLOWING"
	CopyGuardStopTriggered     = "STOP_TRIGGERED"
	CopyGuardStopPendingFlat   = "STOP_PENDING_FLAT"
	CopyGuardStopPartial       = "STOP_PARTIAL"
	CopyGuardStoppedWatching   = "STOPPED_WATCHING"
	CopyGuardReentryPending    = "REENTRY_PENDING"
	CopyGuardFollowingReentry  = "FOLLOWING_REENTRY"
	CopyGuardLeaderClosed      = "LEADER_CLOSED"
	CopyGuardLeaderReversed    = "LEADER_REVERSED"
	CopyGuardAttemptsExhausted = "ATTEMPTS_EXHAUSTED"
	CopyGuardWatchTimeout      = "WATCH_TIMEOUT"
	CopyGuardAIWatching        = "AI_WATCHING"
	CopyGuardAIReviewing       = "AI_REVIEWING"
	CopyGuardAIWaiting         = "AI_WAITING"
	CopyGuardAIAbandoned       = "AI_ABANDONED"
	CopyGuardBudgetSuspended   = "BUDGET_SUSPENDED"
	// CopyGuardProtectionExited is a market exit caused by failure to establish
	// protection. It is not a protective-stop fill and must never seed reentry.
	CopyGuardProtectionExited = "PROTECTION_EXITED"
	// CopyGuardDetached closes analytics without claiming a stop fill when a
	// fresh exchange read proves that the mapped follower position is absent.
	CopyGuardDetached = "DETACHED"
	// CYCLE_LOSS_CAPPED: 周期累计亏损熔断（v4.1），v5 已下线——仓位止损 +
	// 账户硬兜底 + max_reentries 已完整覆盖其职能。常量仅保留用于历史数据
	// 的展示与清理，引擎不再产生该状态。
	CopyGuardCycleLossCapped = "CYCLE_LOSS_CAPPED"
	// 不可保护（裸跑）不是周期状态——warn 模式下周期保持 FOLLOWING
	// （否则保护重试链会跳过它、无法自动复检恢复），信号载体是
	// protection_status=UNPROTECTED_WARNING + GUARD_UNPROTECTABLE 事件。
)

const (
	CopyGuardAccountingOpen       = "OPEN"
	CopyGuardAccountingPending    = "PENDING"
	CopyGuardAccountingReconciled = "RECONCILED"
	// UNSCORABLE means the lifecycle cannot be included in financial/strategy
	// attribution because follower ownership or settlement identity is not
	// authoritative. It is intentionally distinct from a transient PENDING or
	// DELAYED exchange settlement.
	CopyGuardAccountingUnscorable = "UNSCORABLE"
	// DELAYED: the execution exchange (OKX position history / Binance closed
	// PnL) has not returned matching settlement data yet; the system keeps
	// retrying automatically. This replaces the former NEEDS_REVIEW state
	// whose wording wrongly demanded manual action.
	CopyGuardAccountingDelayed = "DELAYED"
	// UNRECOVERABLE: permanently missing identifiers or data; automatic
	// reconciliation stopped and the user is told to inspect the logs.
	CopyGuardAccountingUnrecoverable    = "UNRECOVERABLE"
	CopyGuardAccountingLegacyUnverified = "LEGACY_UNVERIFIED"
)

const (
	CopyGuardProtectionPending  = "PENDING"
	CopyGuardProtectionVerified = "VERIFIED"
	CopyGuardProtectionUnknown  = "UNKNOWN"
	CopyGuardProtectionDegraded = "DEGRADED"
	// CopyGuardProtectionUnprotectedWarning is an explicit user-authorized
	// ordinary-copy state. The position remains open, alerts stay active and
	// the retry loop continues; it is never considered protected.
	CopyGuardProtectionUnprotectedWarning = "UNPROTECTED_WARNING"
	CopyGuardProtectionForcedExitPending  = "FORCED_EXIT_PENDING"
	CopyGuardProtectionTriggered          = "TRIGGERED"
	CopyGuardProtectionCanceled           = "CANCELED"
	// CLAMPED (v5): 保护单已挂出且交易所确认 live，但触发价被钳到强平
	// 安全线（比策略期望价更紧）。属于"已保护但保护质量降级"，UI 需醒目
	// 提示随时可能被扫损。
	CopyGuardProtectionClamped = "CLAMPED"
	// UNPROTECTABLE is retained for old rows and API compatibility. New
	// ordinary-copy failures use UNPROTECTED_WARNING; close mode first enters
	// FORCED_EXIT_PENDING.
	CopyGuardProtectionUnprotectable = "UNPROTECTABLE"
)

// CopyGuardPolicy is persisted as JSON so v4 can evolve without widening the legacy v3 table.
type CopyGuardPolicy struct {
	Version                int     `json:"version"`
	StopMode               string  `json:"stop_mode"`
	StopPriority           string  `json:"stop_priority"`
	StopMaxAccountLossPct  float64 `json:"stop_max_account_loss_pct,omitempty"`
	ATRPeriod              int     `json:"atr_period"`
	ATRCacheMaxAgeMinutes  int     `json:"atr_cache_max_age_minutes"`
	ATRFallbackPct         float64 `json:"atr_fallback_pct"`
	TriggerPriceType       string  `json:"trigger_price_type"`
	SlippageBufferBPS      float64 `json:"slippage_buffer_bps"`
	RoundTripFeeBPS        float64 `json:"round_trip_fee_bps"`
	LiquidationBufferATR   float64 `json:"liquidation_buffer_atr"`
	MaxReentries           int     `json:"max_reentries"`
	ReentryBandATR         float64 `json:"reentry_band_atr"`
	ReentryCooldownSec     int     `json:"reentry_cooldown_seconds"`
	ReentryMaxChaseATR     float64 `json:"reentry_max_chase_atr"`
	MaxATRExpansion        float64 `json:"max_atr_expansion"`
	WatchTimeoutMinutes    int     `json:"watch_timeout_minutes"`
	MigrationConfirmed     bool    `json:"migration_confirmed"`
	AddonBudgetPct         float64 `json:"addon_budget_pct"`
	CycleLossBudgetPct     float64 `json:"cycle_loss_budget_pct"`
	PortfolioLossBudgetPct float64 `json:"portfolio_loss_budget_pct"`
	ReentryDecisionMode    string  `json:"reentry_decision_mode"`
	ReentryMinNotional     float64 `json:"reentry_min_notional"`
	AIConfidenceThreshold  float64 `json:"ai_confidence_threshold"`
	AIMinReviewSeconds     int     `json:"ai_min_review_seconds"`
	AIDailyCallLimit       int     `json:"ai_daily_call_limit"`
	AILifecycleCallLimit   int     `json:"ai_lifecycle_call_limit"`
	NotificationLevel      string  `json:"notification_level"`
	// v4.1 重入加严（字段含义见 store.CopyTradeConfig 同名注释）
	// v5 注：stop_noise_floor_atr / cycle_max_loss_pct 已下线，旧 JSON 中的
	// 存量值在反序列化时被忽略。
	ReentryMinRecoveryATR         float64 `json:"reentry_min_recovery_atr"`
	ReentryMinRecoveryATRExplicit bool    `json:"reentry_min_recovery_atr_explicit,omitempty"`
	ReentryCooldownEscalation     float64 `json:"reentry_cooldown_escalation"`
	ReentryRecoveryEscalation     float64 `json:"reentry_recovery_escalation"`
	// v5 可保护性状态机 / 重入噪音档（字段含义见 store.CopyTradeConfig 同名注释）
	UnprotectableDisposition string `json:"unprotectable_disposition,omitempty"`
	UnprotectableAction      string `json:"unprotectable_action"`
	ReentryNoiseOverride     bool   `json:"reentry_noise_override"`
	// DefaultsVersion: 默认值代次书签。
	//   2 = v4.1 默认值迁移（risk_account_pct 0.02→0.20、cooldown 60→300、
	//       leverage_max_loss 0.5→0.3）
	//   3 = 重入默认放宽（max_chase 0→0.5、cycle_max_loss 0.10→1.0）
	//   4 = v5 两层硬止损（leverage_max_loss 0.3→0.2、account_pct 0.20→0.10、
	//       max_reentries 2→1；均只替换等于旧默认值的行）
	//   5 = 重入默认回调（max_reentries 1→2，仅回补被代次 4 降过的行——
	//       确认式重入的结构性门槛已足够约束坏重入）
	DefaultsVersion int `json:"defaults_version"`
}

// copyGuardPolicyDefaultsVersion 当前默认值代次；migrateCopyGuardPolicyDefaults
// 只处理低于该值的存量策略。
const copyGuardPolicyDefaultsVersion = 8

// CopyGuardDefaultsVersion 暴露给 API/导出，用于前端识别推荐默认值代次。
func CopyGuardDefaultsVersion() int { return copyGuardPolicyDefaultsVersion }

type CopyGuardCycle struct {
	ID                  int64   `json:"id"`
	TraderID            string  `json:"trader_id"`
	TraderName          string  `json:"trader_name,omitempty"`
	LeaderID            string  `json:"leader_id"`
	LeaderPosID         string  `json:"leader_pos_id"`
	Symbol              string  `json:"symbol"`
	Side                string  `json:"side"`
	MarginMode          string  `json:"margin_mode"`
	Status              string  `json:"status"`
	PolicySnapshot      string  `json:"policy_snapshot"`
	LeaderEntryPrice    float64 `json:"leader_entry_price"`
	FollowerEntryPrice  float64 `json:"follower_entry_price"`
	FollowerNotional    float64 `json:"follower_notional"`
	BaselineNotional    float64 `json:"baseline_notional"`
	BaselineRealizedPnL float64 `json:"baseline_realized_pnl"`
	BaselineLeaderSize  float64 `json:"baseline_leader_size"`
	ShadowLeaderSize    float64 `json:"shadow_leader_size"`
	BaselineAvailable   bool    `json:"baseline_leader_size_available"`
	BaselineQuality     string  `json:"baseline_leader_size_quality"`
	AccountEquity       float64 `json:"account_equity"`
	ATRAtEntry          float64 `json:"atr_at_entry"`
	ATRAtStop           float64 `json:"atr_at_stop"`
	// LeaderEntryAtStop: 最近一次止损触发时领航员的持仓均价快照。重入锚点用
	// 保守值（多单取 max、空单取 min(live, snapshot)），防止领航员止损后摊低
	// 均价把重入边界拖向更差的位置。
	LeaderEntryAtStop  float64 `json:"leader_entry_at_stop"`
	LastObservedPrice  float64 `json:"last_observed_price"`
	ReentryCount       int     `json:"reentry_count"`
	StopCount          int     `json:"stop_count"`
	ActualPnL          float64 `json:"actual_pnl"`
	BaselinePnL        float64 `json:"baseline_pnl"`
	Fees               float64 `json:"fees"`
	FundingFee         float64 `json:"funding_fee"`
	LiquidationPenalty float64 `json:"liquidation_penalty"`
	Slippage           float64 `json:"slippage"`
	NetGuardEffect     float64 `json:"net_guard_effect"`
	TrackingDifference float64 `json:"tracking_difference"`
	AccountingStatus   string  `json:"accounting_status"`
	AccountingError    string  `json:"accounting_error"`
	// BaselineSource: 未兜底基线的最终价格来源。
	// "" = 非估算基线；"last_observed" = 最后观测 mark price 估算（待补全）；
	// "leader_history" = 已用领航员公共历史仓位的 closeAvgPx 校准。
	BaselineSource           string     `json:"baseline_source"`
	ProtectionStatus         string     `json:"protection_status"`
	ProtectionRetries        int        `json:"protection_retries"`
	ProtectionCoverage       float64    `json:"protection_coverage"`
	ProtectionError          string     `json:"protection_error"`
	ProtectionMissingSeconds float64    `json:"protection_missing_seconds"`
	FollowerPosID            string     `json:"follower_pos_id"`
	EntryOrderID             string     `json:"entry_order_id"`
	ExitOrderID              string     `json:"exit_order_id"`
	ProtectionMissingAt      *time.Time `json:"protection_missing_at,omitempty"`
	ProtectionLastRetryAt    *time.Time `json:"protection_last_retry_at,omitempty"`
	PendingSince             *time.Time `json:"pending_since,omitempty"`
	ReconciledAt             *time.Time `json:"reconciled_at,omitempty"`
	OpenedAt                 time.Time  `json:"opened_at"`
	StoppedAt                *time.Time `json:"stopped_at,omitempty"`
	ClosedAt                 *time.Time `json:"closed_at,omitempty"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

// CopyGuardWatchSample 观察期采样：止损出局后（STOPPED_WATCHING 及终态观察）
// 每个轮询 tick 按"固定间隔必采 + 门控原因变化立即补采"记录一条，用于复盘
// "出局后价格如何演化、每个时刻为什么没有重入"，以及离线回测/参数调优。
// Gate 取值见 copytrade 包 watch gate 常量（COOLDOWN / PRICE_NOT_RETURNED /
// CHASE_EXCEEDED / ATR_EXPANSION / MIN_NOTIONAL / REENTRY_TRIGGERED / 终态状态等）。
type CopyGuardWatchSample struct {
	ID        int64  `json:"id"`
	CycleID   int64  `json:"cycle_id"`
	TraderID  string `json:"trader_id"`
	AttemptNo int    `json:"attempt_no"`
	// MarkPrice/ATR: 采样时的标记价与 ATR（ATR=0 表示当时不可得）
	MarkPrice float64 `json:"mark_price"`
	ATR       float64 `json:"atr"`
	// LeaderEntryPrice/LeaderSize: 领航员实时均价与仓位数量（观察期加减仓轨迹）
	LeaderEntryPrice float64 `json:"leader_entry_price"`
	LeaderSize       float64 `json:"leader_size"`
	// ReentryBoundary/ChaseLimit: 当时的重入穿越边界与追价上限（0 = 该 tick 未计算到）
	ReentryBoundary float64 `json:"reentry_boundary"`
	ChaseLimit      float64 `json:"chase_limit"`
	// Gate: 该 tick 未重入的主导原因（或 REENTRY_TRIGGERED）
	Gate      string    `json:"gate"`
	CreatedAt time.Time `json:"created_at"`
}

type CopyGuardEvent struct {
	ID        int64                  `json:"id"`
	CycleID   int64                  `json:"cycle_id"`
	TraderID  string                 `json:"trader_id"`
	Type      string                 `json:"type"`
	Price     float64                `json:"price"`
	Quantity  float64                `json:"quantity"`
	Notional  float64                `json:"notional"`
	PnL       float64                `json:"pnl"`
	Fee       float64                `json:"fee"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
}

type CopyGuardAttempt struct {
	ID                  int64      `json:"id"`
	CycleID             int64      `json:"cycle_id"`
	AttemptNo           int        `json:"attempt_no"`
	Status              string     `json:"status"`
	EntryPrice          float64    `json:"entry_price"`
	ExitPrice           float64    `json:"exit_price"`
	Quantity            float64    `json:"quantity"`
	Notional            float64    `json:"notional"`
	StopTriggerPrice    float64    `json:"stop_trigger_price"`
	StopFillPrice       float64    `json:"stop_fill_price"`
	StopAlgoID          string     `json:"stop_algo_id"`
	ProtectionAlgoID    string     `json:"protection_algo_id"`
	ProtectionStatus    string     `json:"protection_status"`
	ProtectionCoverage  float64    `json:"protection_coverage"`
	ProtectionUpdatedAt *time.Time `json:"protection_updated_at,omitempty"`
	FollowerPosID       string     `json:"follower_pos_id"`
	EntryOrderID        string     `json:"entry_order_id"`
	ExitOrderID         string     `json:"exit_order_id"`
	PnL                 float64    `json:"pnl"`
	Fee                 float64    `json:"fee"`
	FundingFee          float64    `json:"funding_fee"`
	LiquidationPenalty  float64    `json:"liquidation_penalty"`
	Reconciled          bool       `json:"reconciled"`
	ATR                 float64    `json:"atr"`
	OpenedAt            time.Time  `json:"opened_at"`
	ClosedAt            *time.Time `json:"closed_at,omitempty"`
}

func (s *CopyTradeStore) initCopyGuardTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_guard_account_policies (
			exchange_id TEXT PRIMARY KEY,
			max_position_loss_pct REAL NOT NULL DEFAULT 0.10,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (exchange_id) REFERENCES exchanges(id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS copy_guard_policies (
			trader_id TEXT PRIMARY KEY,
			policy_json TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (trader_id) REFERENCES traders(id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS copy_guard_cycles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL, leader_id TEXT NOT NULL, leader_pos_id TEXT NOT NULL,
			symbol TEXT NOT NULL, side TEXT NOT NULL, margin_mode TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'FOLLOWING', policy_snapshot TEXT NOT NULL DEFAULT '{}',
			leader_entry_price REAL DEFAULT 0, follower_entry_price REAL DEFAULT 0,
			follower_notional REAL DEFAULT 0, baseline_notional REAL DEFAULT 0, baseline_realized_pnl REAL DEFAULT 0, baseline_leader_size REAL DEFAULT 0,
			shadow_leader_size REAL DEFAULT 0, baseline_leader_size_available BOOLEAN NOT NULL DEFAULT 0,
			baseline_leader_size_quality TEXT NOT NULL DEFAULT 'UNSCORABLE', account_equity REAL DEFAULT 0,
			atr_at_entry REAL DEFAULT 0, atr_at_stop REAL DEFAULT 0, last_observed_price REAL DEFAULT 0,
			reentry_count INTEGER DEFAULT 0, stop_count INTEGER DEFAULT 0,
			actual_pnl REAL DEFAULT 0, baseline_pnl REAL DEFAULT 0, fees REAL DEFAULT 0,
			funding_fee REAL DEFAULT 0, liquidation_penalty REAL DEFAULT 0, slippage REAL DEFAULT 0, net_guard_effect REAL DEFAULT 0,
			tracking_difference REAL DEFAULT 0, accounting_status TEXT DEFAULT 'OPEN', accounting_error TEXT DEFAULT '', reconciled_at DATETIME,
			protection_status TEXT DEFAULT 'PENDING', protection_retries INTEGER DEFAULT 0,
			protection_coverage REAL DEFAULT 0, protection_error TEXT DEFAULT '',
			follower_pos_id TEXT DEFAULT '', entry_order_id TEXT DEFAULT '', exit_order_id TEXT DEFAULT '',
			protection_missing_at DATETIME, protection_missing_seconds REAL DEFAULT 0, protection_last_retry_at DATETIME, pending_since DATETIME,
			opened_at DATETIME DEFAULT CURRENT_TIMESTAMP, stopped_at DATETIME, closed_at DATETIME,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(trader_id, leader_pos_id, opened_at)
		);
		CREATE INDEX IF NOT EXISTS idx_copy_guard_cycles_filter ON copy_guard_cycles(trader_id, opened_at, status);
		CREATE TABLE IF NOT EXISTS copy_guard_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT, cycle_id INTEGER NOT NULL, attempt_no INTEGER NOT NULL,
			status TEXT NOT NULL, entry_price REAL DEFAULT 0, exit_price REAL DEFAULT 0,
			quantity REAL DEFAULT 0, notional REAL DEFAULT 0, stop_trigger_price REAL DEFAULT 0,
			stop_fill_price REAL DEFAULT 0, stop_algo_id TEXT DEFAULT '',
			protection_algo_id TEXT DEFAULT '', protection_status TEXT DEFAULT 'PENDING',
			protection_coverage REAL DEFAULT 0, protection_updated_at DATETIME,
			follower_pos_id TEXT DEFAULT '', entry_order_id TEXT DEFAULT '', exit_order_id TEXT DEFAULT '', pnl REAL DEFAULT 0,
			fee REAL DEFAULT 0, funding_fee REAL DEFAULT 0, liquidation_penalty REAL DEFAULT 0, reconciled BOOLEAN DEFAULT 0, atr REAL DEFAULT 0,
			opened_at DATETIME DEFAULT CURRENT_TIMESTAMP, closed_at DATETIME,
			UNIQUE(cycle_id, attempt_no)
		);
		CREATE TABLE IF NOT EXISTS copy_guard_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, cycle_id INTEGER NOT NULL, trader_id TEXT NOT NULL,
			type TEXT NOT NULL, price REAL DEFAULT 0, quantity REAL DEFAULT 0, notional REAL DEFAULT 0,
			pnl REAL DEFAULT 0, fee REAL DEFAULT 0, metadata_json TEXT DEFAULT '{}',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_copy_guard_events_cycle ON copy_guard_events(cycle_id, created_at);
		CREATE TABLE IF NOT EXISTS copy_guard_protective_orders (
			cycle_id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL, algo_id TEXT NOT NULL,
			algo_client_id TEXT DEFAULT '', symbol TEXT NOT NULL, side TEXT NOT NULL, margin_mode TEXT NOT NULL,
			quantity REAL NOT NULL, quantity_step REAL DEFAULT 0, trigger_price REAL NOT NULL, trigger_type TEXT NOT NULL,
			status TEXT NOT NULL, previous_algo_id TEXT DEFAULT '', previous_algo_client_id TEXT DEFAULT '',
			replacement_pending BOOLEAN DEFAULT 0, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS copy_guard_watch_samples (
			id INTEGER PRIMARY KEY AUTOINCREMENT, cycle_id INTEGER NOT NULL, trader_id TEXT NOT NULL,
			attempt_no INTEGER NOT NULL DEFAULT 0,
			mark_price REAL DEFAULT 0, atr REAL DEFAULT 0,
			leader_entry_price REAL DEFAULT 0, leader_size REAL DEFAULT 0,
			reentry_boundary REAL DEFAULT 0, chase_limit REAL DEFAULT 0,
			gate TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_copy_guard_watch_samples_cycle ON copy_guard_watch_samples(cycle_id, created_at);
	`)
	if err == nil {
		migrations := []struct {
			table, name, definition string
		}{
			{"copy_guard_cycles", "baseline_notional", "REAL DEFAULT 0"},
			{"copy_guard_cycles", "baseline_realized_pnl", "REAL DEFAULT 0"},
			{"copy_guard_cycles", "baseline_leader_size", "REAL DEFAULT 0"},
			{"copy_guard_cycles", "protection_status", "TEXT DEFAULT 'PENDING'"},
			{"copy_guard_cycles", "protection_retries", "INTEGER DEFAULT 0"},
			{"copy_guard_cycles", "protection_coverage", "REAL DEFAULT 0"},
			{"copy_guard_cycles", "protection_error", "TEXT DEFAULT ''"},
			{"copy_guard_cycles", "follower_pos_id", "TEXT DEFAULT ''"},
			{"copy_guard_cycles", "entry_order_id", "TEXT DEFAULT ''"},
			{"copy_guard_cycles", "exit_order_id", "TEXT DEFAULT ''"},
			{"copy_guard_cycles", "protection_missing_at", "DATETIME"},
			{"copy_guard_cycles", "pending_since", "DATETIME"},
			{"copy_guard_cycles", "liquidation_penalty", "REAL DEFAULT 0"},
			{"copy_guard_cycles", "protection_missing_seconds", "REAL DEFAULT 0"},
			{"copy_guard_cycles", "protection_last_retry_at", "DATETIME"},
			{"copy_guard_cycles", "tracking_difference", "REAL DEFAULT 0"},
			{"copy_guard_cycles", "accounting_status", "TEXT DEFAULT 'OPEN'"},
			{"copy_guard_cycles", "accounting_error", "TEXT DEFAULT ''"},
			{"copy_guard_cycles", "reconciled_at", "DATETIME"},
			{"copy_guard_attempts", "liquidation_penalty", "REAL DEFAULT 0"},
			{"copy_guard_attempts", "reconciled", "BOOLEAN DEFAULT 0"},
			{"copy_guard_attempts", "follower_pos_id", "TEXT DEFAULT ''"},
			{"copy_guard_attempts", "entry_order_id", "TEXT DEFAULT ''"},
			{"copy_guard_attempts", "exit_order_id", "TEXT DEFAULT ''"},
			{"copy_guard_attempts", "protection_algo_id", "TEXT DEFAULT ''"},
			{"copy_guard_attempts", "protection_status", "TEXT DEFAULT 'PENDING'"},
			{"copy_guard_attempts", "protection_coverage", "REAL DEFAULT 0"},
			{"copy_guard_attempts", "protection_updated_at", "DATETIME"},
			{"copy_guard_cycles", "baseline_source", "TEXT DEFAULT ''"},
			// baseline_version: 1 = 旧口径（领航员比例折算的影子名义），
			// 2 = own-path 口径（每个 attempt 按自身名义持有到领航员平仓价）。
			// 仅作启动一次性重算的书签，业务逻辑不读取。
			{"copy_guard_cycles", "baseline_version", "INTEGER DEFAULT 1"},
			{"copy_guard_cycles", "leader_entry_at_stop", "REAL DEFAULT 0"},
			{"copy_guard_watch_samples", "attempt_no", "INTEGER NOT NULL DEFAULT 0"},
			{"copy_guard_protective_orders", "quantity_step", "REAL DEFAULT 0"},
			{"copy_guard_protective_orders", "previous_algo_id", "TEXT DEFAULT ''"},
			{"copy_guard_protective_orders", "previous_algo_client_id", "TEXT DEFAULT ''"},
			{"copy_guard_protective_orders", "replacement_pending", "BOOLEAN DEFAULT 0"},
			{"copy_guard_cycles", "shadow_leader_size", "REAL DEFAULT 0"},
			{"copy_guard_cycles", "baseline_leader_size_available", "BOOLEAN NOT NULL DEFAULT 0"},
			{"copy_guard_cycles", "baseline_leader_size_quality", "TEXT NOT NULL DEFAULT 'UNSCORABLE'"},
		}
		for _, migration := range migrations {
			if migrationErr := ensureSQLiteColumn(s.db, migration.table, migration.name, migration.definition); migrationErr != nil {
				return fmt.Errorf("migrate %s.%s: %w", migration.table, migration.name, migrationErr)
			}
		}
		// Recover an immutable cycle baseline only from the earliest confirmed
		// leader open. The legacy baseline_leader_size value was a mutable
		// shadow and is not accepted as evidence. Shadow continuity may retain
		// that legacy observation, but unavailable baselines stay unavailable.
		if _, migrationErr := s.db.Exec(`UPDATE copy_guard_cycles
			SET shadow_leader_size=CASE WHEN shadow_leader_size<=0 THEN baseline_leader_size ELSE shadow_leader_size END
			WHERE closed_at IS NULL`); migrationErr != nil {
			return fmt.Errorf("initialize copy guard shadow size: %w", migrationErr)
		}
		if _, migrationErr := s.db.Exec(`UPDATE copy_guard_cycles
			SET baseline_leader_size=(
				SELECT i.leader_target_size FROM copy_trade_execution_intents i
				WHERE i.trader_id=copy_guard_cycles.trader_id
				  AND i.leader_pos_id=copy_guard_cycles.leader_pos_id
				  AND i.action IN ('open_long','open_short')
				  AND i.status IN ('COMPLETED_PARTIAL','FILLED','PROTECTED')
				  AND i.leader_target_size>0
				  AND i.source_revision > COALESCE((
					SELECT MAX(closed.source_revision)
					FROM copy_trade_execution_intents closed
					WHERE closed.trader_id=i.trader_id
					  AND closed.leader_pos_id=i.leader_pos_id
					  AND closed.action IN ('close_long','close_short')
					  AND closed.status IN ('FILLED','PROTECTED')
				  ),0)
				ORDER BY i.source_revision,i.id LIMIT 1
			),baseline_leader_size_available=1,baseline_leader_size_quality='VERIFIED'
			WHERE closed_at IS NULL AND EXISTS(
				SELECT 1 FROM copy_trade_execution_intents i
				WHERE i.trader_id=copy_guard_cycles.trader_id
				  AND i.leader_pos_id=copy_guard_cycles.leader_pos_id
				  AND i.action IN ('open_long','open_short')
				  AND i.status IN ('COMPLETED_PARTIAL','FILLED','PROTECTED')
				  AND i.leader_target_size>0
				  AND i.source_revision > COALESCE((
					SELECT MAX(closed.source_revision)
					FROM copy_trade_execution_intents closed
					WHERE closed.trader_id=i.trader_id
					  AND closed.leader_pos_id=i.leader_pos_id
					  AND closed.action IN ('close_long','close_short')
					  AND closed.status IN ('FILLED','PROTECTED')
				  ),0)
			)`); migrationErr != nil {
			return fmt.Errorf("recover copy guard immutable baseline: %w", migrationErr)
		}
		// Existing closed cycles were produced before deterministic settlement was
		// available. Preserve their raw values but never present them as verified.
		if _, migrationErr := s.db.Exec(`UPDATE copy_guard_cycles SET accounting_status=?,accounting_error=CASE WHEN COALESCE(accounting_error,'')='' THEN 'created before deterministic OKX settlement; raw values preserved' ELSE accounting_error END WHERE closed_at IS NOT NULL AND COALESCE(accounting_status,'OPEN')='OPEN'`, CopyGuardAccountingLegacyUnverified); migrationErr != nil {
			return fmt.Errorf("mark legacy copy guard accounting: %w", migrationErr)
		}
		// NEEDS_REVIEW was renamed to DELAYED (automatic retry continues);
		// idempotent so it can run on every startup.
		if _, migrationErr := s.db.Exec(`UPDATE copy_guard_cycles SET accounting_status=? WHERE accounting_status='NEEDS_REVIEW'`, CopyGuardAccountingDelayed); migrationErr != nil {
			return fmt.Errorf("migrate copy guard delayed accounting status: %w", migrationErr)
		}
		// v4.1 默认值代次迁移（幂等，defaults_version 书签控制）
		if migrationErr := s.migrateCopyGuardPolicyDefaults(); migrationErr != nil {
			return migrationErr
		}
		// v5.1 人工重入信号表
		if merr := s.initManualReentryTables(); merr != nil {
			return merr
		}
	}
	return err
}

type CopyGuardProtectiveOrder struct {
	CycleID              int64   `json:"cycle_id"`
	TraderID             string  `json:"trader_id"`
	AlgoID               string  `json:"algo_id"`
	AlgoClientID         string  `json:"algo_client_id"`
	Symbol               string  `json:"symbol"`
	Side                 string  `json:"side"`
	MarginMode           string  `json:"margin_mode"`
	Quantity             float64 `json:"quantity"`
	QuantityStep         float64 `json:"quantity_step"`
	TriggerPrice         float64 `json:"trigger_price"`
	TriggerType          string  `json:"trigger_type"`
	Status               string  `json:"status"`
	PreviousAlgoID       string  `json:"previous_algo_id,omitempty"`
	PreviousAlgoClientID string  `json:"previous_algo_client_id,omitempty"`
	ReplacementPending   bool    `json:"replacement_pending"`
}

func (s *CopyTradeStore) UpsertCopyGuardProtectiveOrder(o *CopyGuardProtectiveOrder) error {
	if o == nil {
		return fmt.Errorf("nil protective order")
	}
	_, err := s.db.Exec(`INSERT INTO copy_guard_protective_orders(cycle_id,trader_id,algo_id,algo_client_id,symbol,side,margin_mode,quantity,quantity_step,trigger_price,trigger_type,status,previous_algo_id,previous_algo_client_id,replacement_pending) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(cycle_id) DO UPDATE SET algo_id=excluded.algo_id,algo_client_id=excluded.algo_client_id,quantity=excluded.quantity,quantity_step=excluded.quantity_step,trigger_price=excluded.trigger_price,trigger_type=excluded.trigger_type,status=excluded.status,previous_algo_id=excluded.previous_algo_id,previous_algo_client_id=excluded.previous_algo_client_id,replacement_pending=excluded.replacement_pending,updated_at=CURRENT_TIMESTAMP`, o.CycleID, o.TraderID, o.AlgoID, o.AlgoClientID, o.Symbol, o.Side, o.MarginMode, o.Quantity, o.QuantityStep, o.TriggerPrice, o.TriggerType, o.Status, o.PreviousAlgoID, o.PreviousAlgoClientID, o.ReplacementPending)
	return err
}

func (s *CopyTradeStore) GetCopyGuardProtectiveOrder(cycleID int64) (*CopyGuardProtectiveOrder, error) {
	var o CopyGuardProtectiveOrder
	err := s.db.QueryRow(`SELECT cycle_id,trader_id,algo_id,algo_client_id,symbol,side,margin_mode,quantity,quantity_step,trigger_price,trigger_type,status,COALESCE(previous_algo_id,''),COALESCE(previous_algo_client_id,''),COALESCE(replacement_pending,0) FROM copy_guard_protective_orders WHERE cycle_id=?`, cycleID).Scan(&o.CycleID, &o.TraderID, &o.AlgoID, &o.AlgoClientID, &o.Symbol, &o.Side, &o.MarginMode, &o.Quantity, &o.QuantityStep, &o.TriggerPrice, &o.TriggerType, &o.Status, &o.PreviousAlgoID, &o.PreviousAlgoClientID, &o.ReplacementPending)
	return &o, err
}

func (s *CopyTradeStore) CompleteCopyGuardProtectiveReplacement(cycleID int64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_protective_orders SET previous_algo_id='',previous_algo_client_id='',replacement_pending=0,updated_at=CURRENT_TIMESTAMP WHERE cycle_id=?`, cycleID)
	return err
}

func (s *CopyTradeStore) UpdateCopyGuardProtectiveOrderStatus(cycleID int64, status string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_protective_orders SET status=?,updated_at=CURRENT_TIMESTAMP WHERE cycle_id=?`, status, cycleID)
	return err
}

func (s *CopyTradeStore) ListActiveCopyGuardProtectiveOrders(traderID string) ([]*CopyGuardProtectiveOrder, error) {
	rows, err := s.db.Query(`SELECT cycle_id,trader_id,algo_id,algo_client_id,symbol,side,margin_mode,quantity,quantity_step,trigger_price,trigger_type,status,COALESCE(previous_algo_id,''),COALESCE(previous_algo_client_id,''),COALESCE(replacement_pending,0) FROM copy_guard_protective_orders WHERE trader_id=? AND (status='live' OR replacement_pending=1)`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardProtectiveOrder{}
	for rows.Next() {
		var o CopyGuardProtectiveOrder
		if err := rows.Scan(&o.CycleID, &o.TraderID, &o.AlgoID, &o.AlgoClientID, &o.Symbol, &o.Side, &o.MarginMode, &o.Quantity, &o.QuantityStep, &o.TriggerPrice, &o.TriggerType, &o.Status, &o.PreviousAlgoID, &o.PreviousAlgoClientID, &o.ReplacementPending); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// SaveCopyGuardWatchSample 追加一条观察期采样。
func (s *CopyTradeStore) SaveCopyGuardWatchSample(w *CopyGuardWatchSample) error {
	if w == nil {
		return fmt.Errorf("nil copy guard watch sample")
	}
	_, err := s.db.Exec(`INSERT INTO copy_guard_watch_samples(cycle_id,trader_id,attempt_no,mark_price,atr,leader_entry_price,leader_size,reentry_boundary,chase_limit,gate) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		w.CycleID, w.TraderID, w.AttemptNo, w.MarkPrice, w.ATR, w.LeaderEntryPrice, w.LeaderSize, w.ReentryBoundary, w.ChaseLimit, w.Gate)
	return err
}

// GetLatestCopyGuardWatchSample 取周期最近一条采样（无采样返回 sql.ErrNoRows）。
// 引擎用它做降噪：固定间隔未到且门控原因未变时跳过写入；跨重启依然成立。
func (s *CopyTradeStore) GetLatestCopyGuardWatchSample(cycleID int64) (*CopyGuardWatchSample, error) {
	row := s.db.QueryRow(`SELECT id,cycle_id,trader_id,attempt_no,mark_price,atr,leader_entry_price,leader_size,reentry_boundary,chase_limit,gate,created_at FROM copy_guard_watch_samples WHERE cycle_id=? ORDER BY id DESC LIMIT 1`, cycleID)
	return scanCopyGuardWatchSample(row.Scan)
}

// ListCopyGuardWatchSamples 按时间序返回周期全部采样（timeline API / 导出用）。
func (s *CopyTradeStore) ListCopyGuardWatchSamples(cycleID int64) ([]*CopyGuardWatchSample, error) {
	rows, err := s.db.Query(`SELECT id,cycle_id,trader_id,attempt_no,mark_price,atr,leader_entry_price,leader_size,reentry_boundary,chase_limit,gate,created_at FROM copy_guard_watch_samples WHERE cycle_id=? ORDER BY id`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardWatchSample{}
	for rows.Next() {
		w, err := scanCopyGuardWatchSample(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func scanCopyGuardWatchSample(scan func(dest ...interface{}) error) (*CopyGuardWatchSample, error) {
	var w CopyGuardWatchSample
	var created string
	if err := scan(&w.ID, &w.CycleID, &w.TraderID, &w.AttemptNo, &w.MarkPrice, &w.ATR, &w.LeaderEntryPrice, &w.LeaderSize, &w.ReentryBoundary, &w.ChaseLimit, &w.Gate, &created); err != nil {
		return nil, err
	}
	var err error
	if w.CreatedAt, err = parseDBTime(created); err != nil {
		return nil, fmt.Errorf("copy guard watch sample %d created_at: %w", w.ID, err)
	}
	return &w, nil
}

func policyFromConfig(c *CopyTradeConfig) CopyGuardPolicy {
	return CopyGuardPolicy{
		Version: c.RiskPolicyVersion, StopMode: c.RiskStopMode, StopPriority: c.RiskStopPriority,
		StopMaxAccountLossPct: c.RiskStopMaxAccountLossPct,
		ATRPeriod:             c.RiskATRPeriod, ATRCacheMaxAgeMinutes: c.RiskATRCacheMaxAgeMinutes,
		ATRFallbackPct: c.RiskATRFallbackPct, TriggerPriceType: c.RiskTriggerPriceType,
		SlippageBufferBPS: c.RiskSlippageBufferBPS, RoundTripFeeBPS: c.RiskRoundTripFeeBPS,
		LiquidationBufferATR: c.RiskLiquidationBufferATR, MaxReentries: c.RiskMaxReentries,
		ReentryBandATR: c.RiskReentryBandATR, ReentryCooldownSec: c.RiskReentryCooldownSeconds,
		ReentryMaxChaseATR: c.RiskReentryMaxChaseATR, MaxATRExpansion: c.RiskReentryMaxATRExpansion,
		WatchTimeoutMinutes: c.RiskWatchTimeoutMinutes, MigrationConfirmed: c.RiskMigrationConfirmed,
		AddonBudgetPct: c.RiskAddonBudgetPct, CycleLossBudgetPct: c.RiskCycleLossBudgetPct,
		PortfolioLossBudgetPct: c.RiskPortfolioLossBudgetPct,
		ReentryDecisionMode:    c.RiskReentryDecisionMode, ReentryMinNotional: c.RiskReentryMinNotional,
		AIConfidenceThreshold: c.RiskAIConfidenceThreshold, AIMinReviewSeconds: c.RiskAIMinReviewSeconds,
		AIDailyCallLimit: c.RiskAIDailyCallLimit, AILifecycleCallLimit: c.RiskAILifecycleCallLimit,
		NotificationLevel:             c.RiskNotificationLevel,
		ReentryMinRecoveryATR:         c.RiskReentryMinRecoveryATR,
		ReentryMinRecoveryATRExplicit: c.RiskReentryMinRecoveryATRExplicit,
		ReentryCooldownEscalation:     c.RiskReentryCooldownEscalation,
		ReentryRecoveryEscalation:     c.RiskReentryRecoveryEscalation,
		UnprotectableDisposition:      c.RiskUnprotectableDisposition,
		UnprotectableAction:           c.RiskUnprotectableAction,
		ReentryNoiseOverride:          c.RiskReentryNoiseOverride,
		DefaultsVersion:               copyGuardPolicyDefaultsVersion,
	}
}

type sqlExecer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func saveCopyGuardPolicyWithExecutor(exec sqlExecer, c *CopyTradeConfig) error {
	if c == nil {
		return nil
	}
	if c.RiskPolicyVersion < 4 {
		// A stale v4 JSON row would otherwise override the downgraded base
		// config on the next read and silently reactivate Copy Guard.
		_, err := exec.Exec(`DELETE FROM copy_guard_policies WHERE trader_id=?`, c.TraderID)
		return err
	}
	b, err := json.Marshal(policyFromConfig(c))
	if err != nil {
		return err
	}
	_, err = exec.Exec(`INSERT INTO copy_guard_policies(trader_id, policy_json, updated_at) VALUES(?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(trader_id) DO UPDATE SET policy_json=excluded.policy_json, updated_at=CURRENT_TIMESTAMP`, c.TraderID, string(b))
	return err
}

func (s *CopyTradeStore) saveCopyGuardPolicy(c *CopyTradeConfig) error {
	return saveCopyGuardPolicyWithExecutor(s.db, c)
}

func (s *CopyTradeStore) loadCopyGuardPolicy(c *CopyTradeConfig) error {
	var raw string
	if err := s.db.QueryRow(`SELECT policy_json FROM copy_guard_policies WHERE trader_id=?`, c.TraderID).Scan(&raw); err != nil {
		return err
	}
	var p CopyGuardPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return err
	}
	c.RiskPolicyVersion, c.RiskStopMode, c.RiskATRPeriod = p.Version, p.StopMode, p.ATRPeriod
	c.RiskStopPriority = p.StopPriority
	c.RiskStopMaxAccountLossPct = p.StopMaxAccountLossPct
	c.RiskATRCacheMaxAgeMinutes = p.ATRCacheMaxAgeMinutes
	c.RiskATRFallbackPct, c.RiskTriggerPriceType = p.ATRFallbackPct, p.TriggerPriceType
	c.RiskSlippageBufferBPS, c.RiskLiquidationBufferATR = p.SlippageBufferBPS, p.LiquidationBufferATR
	c.RiskRoundTripFeeBPS = p.RoundTripFeeBPS
	c.RiskMaxReentries, c.RiskReentryBandATR = p.MaxReentries, p.ReentryBandATR
	c.RiskReentryCooldownSeconds, c.RiskReentryMaxChaseATR = p.ReentryCooldownSec, p.ReentryMaxChaseATR
	c.RiskReentryMaxATRExpansion, c.RiskWatchTimeoutMinutes = p.MaxATRExpansion, p.WatchTimeoutMinutes
	c.RiskMigrationConfirmed = p.MigrationConfirmed
	c.RiskAddonBudgetPct = p.AddonBudgetPct
	c.RiskCycleLossBudgetPct = p.CycleLossBudgetPct
	c.RiskPortfolioLossBudgetPct = p.PortfolioLossBudgetPct
	c.RiskReentryDecisionMode = p.ReentryDecisionMode
	c.RiskReentryMinNotional = p.ReentryMinNotional
	c.RiskAIConfidenceThreshold = p.AIConfidenceThreshold
	c.RiskAIMinReviewSeconds = p.AIMinReviewSeconds
	c.RiskAIDailyCallLimit = p.AIDailyCallLimit
	c.RiskAILifecycleCallLimit = p.AILifecycleCallLimit
	c.RiskNotificationLevel = p.NotificationLevel
	c.RiskReentryMinRecoveryATR = p.ReentryMinRecoveryATR
	c.RiskReentryMinRecoveryATRExplicit = p.ReentryMinRecoveryATRExplicit
	c.RiskReentryCooldownEscalation = p.ReentryCooldownEscalation
	c.RiskReentryRecoveryEscalation = p.ReentryRecoveryEscalation
	c.RiskUnprotectableDisposition = p.UnprotectableDisposition
	c.RiskUnprotectableAction = p.UnprotectableAction
	c.RiskReentryNoiseOverride = p.ReentryNoiseOverride
	c.FillRiskDefaults()
	return nil
}

// migrateCopyGuardPolicyDefaults upgrades only the storage contract. Numeric
// risk settings remain untouched because the database cannot distinguish an
// inherited value from an explicit choice. The v8 unprotectable disposition is
// the sole exception: older servers forced "close" as a default and old clients
// always echoed it, so there is no durable evidence of an explicit user choice.
// All pre-v8 policies therefore migrate once to the confirmed warn/follow
// default; users may explicitly choose close after the upgrade.
func (s *CopyTradeStore) migrateCopyGuardPolicyDefaults() error {
	rows, err := s.db.Query(`SELECT trader_id, policy_json FROM copy_guard_policies`)
	if err != nil {
		return fmt.Errorf("read copy guard policy defaults migration: %w", err)
	}
	type pendingPolicy struct {
		traderID string
		policy   CopyGuardPolicy
	}
	var pending []pendingPolicy
	for rows.Next() {
		var traderID, raw string
		if err := rows.Scan(&traderID, &raw); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan copy guard policy defaults migration: %w", err)
		}
		var p CopyGuardPolicy
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode copy guard policy for trader %s: %w", traderID, err)
		}
		if p.Version < 4 || p.DefaultsVersion >= copyGuardPolicyDefaultsVersion {
			continue
		}
		pending = append(pending, pendingPolicy{traderID: traderID, policy: p})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate copy guard policy defaults migration: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close copy guard policy defaults migration rows: %w", err)
	}
	for _, item := range pending {
		p := item.policy
		if p.ReentryDecisionMode == "" {
			p.ReentryDecisionMode = "legacy_rule"
		}
		p.UnprotectableDisposition = "warn"
		p.UnprotectableAction = "follow"
		p.DefaultsVersion = copyGuardPolicyDefaultsVersion
		b, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("encode copy guard policy for trader %s: %w", item.traderID, err)
		}
		if _, err := s.db.Exec(`UPDATE copy_guard_policies SET policy_json=?, updated_at=CURRENT_TIMESTAMP WHERE trader_id=?`, string(b), item.traderID); err != nil {
			return fmt.Errorf("persist copy guard policy defaults for trader %s: %w", item.traderID, err)
		}
	}
	return nil
}

func (s *CopyTradeStore) EnsureCopyGuardCycle(c *CopyGuardCycle) (*CopyGuardCycle, error) {
	if c == nil {
		return nil, fmt.Errorf("nil copy guard cycle")
	}
	var existing int64
	err := s.db.QueryRow(`SELECT id FROM copy_guard_cycles WHERE trader_id=? AND leader_pos_id=? AND closed_at IS NULL ORDER BY id DESC LIMIT 1`, c.TraderID, c.LeaderPosID).Scan(&existing)
	if err == nil {
		if c.BaselineLeaderSize > 0 {
			if _, updateErr := s.db.Exec(`UPDATE copy_guard_cycles SET baseline_leader_size=?,shadow_leader_size=CASE WHEN shadow_leader_size<=0 THEN ? ELSE shadow_leader_size END,baseline_leader_size_available=1,baseline_leader_size_quality='VERIFIED' WHERE id=? AND baseline_leader_size<=0`, c.BaselineLeaderSize, c.BaselineLeaderSize, existing); updateErr != nil {
				return nil, updateErr
			}
		}
		return s.GetCopyGuardCycle(existing)
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	if c.ProtectionStatus == "" {
		c.ProtectionStatus = CopyGuardProtectionPending
	}
	available, quality := c.BaselineLeaderSize > 0, "UNSCORABLE"
	if available {
		quality = "VERIFIED"
	}
	shadow := c.ShadowLeaderSize
	if shadow <= 0 {
		shadow = c.BaselineLeaderSize
	}
	res, err := s.db.Exec(`INSERT INTO copy_guard_cycles(trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot,leader_entry_price,follower_entry_price,follower_notional,baseline_notional,baseline_leader_size,shadow_leader_size,baseline_leader_size_available,baseline_leader_size_quality,account_equity,atr_at_entry,last_observed_price,protection_status,baseline_version,pending_since) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,2,CURRENT_TIMESTAMP)`,
		c.TraderID, c.LeaderID, c.LeaderPosID, c.Symbol, c.Side, c.MarginMode, c.Status, c.PolicySnapshot, c.LeaderEntryPrice, c.FollowerEntryPrice, c.FollowerNotional, c.FollowerNotional, c.BaselineLeaderSize, shadow, available, quality, c.AccountEquity, c.ATRAtEntry, c.LastObservedPrice, c.ProtectionStatus)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetCopyGuardCycle(id)
}

// GetConfirmedInitialLeaderSize returns the only evidence accepted for an
// immutable cycle baseline: the earliest exchange-confirmed leader open.
func (s *CopyTradeStore) GetConfirmedInitialLeaderSize(traderID, leaderPosID string) (float64, bool, error) {
	var size float64
	err := s.db.QueryRow(`SELECT leader_target_size FROM copy_trade_execution_intents
		WHERE trader_id=? AND leader_pos_id=?
		  AND action IN ('open_long','open_short')
		  AND status IN ('COMPLETED_PARTIAL','FILLED','PROTECTED')
		  AND leader_target_size>0
		  AND source_revision > COALESCE((
			SELECT MAX(source_revision) FROM copy_trade_execution_intents
			WHERE trader_id=? AND leader_pos_id=?
			  AND action IN ('close_long','close_short')
			  AND status IN ('FILLED','PROTECTED')
		  ),0)
		ORDER BY source_revision,id LIMIT 1`, traderID, leaderPosID, traderID, leaderPosID).Scan(&size)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return size, true, nil
}

func (s *CopyTradeStore) InitializeCopyGuardLeaderBaseline(id int64, leaderSize float64) error {
	if id <= 0 || leaderSize <= 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET baseline_leader_size=?,shadow_leader_size=CASE WHEN shadow_leader_size<=0 THEN ? ELSE shadow_leader_size END,baseline_leader_size_available=1,baseline_leader_size_quality='VERIFIED',updated_at=CURRENT_TIMESTAMP WHERE id=? AND baseline_leader_size<=0`, leaderSize, leaderSize, id)
	return err
}

func (s *CopyTradeStore) GetCopyGuardCycle(id int64) (*CopyGuardCycle, error) {
	row := s.db.QueryRow(copyGuardCycleSelect+` WHERE id=?`, id)
	return scanCopyGuardCycle(row)
}

func (s *CopyTradeStore) GetOpenCopyGuardCycle(traderID, leaderPosID string) (*CopyGuardCycle, error) {
	row := s.db.QueryRow(copyGuardCycleSelect+` WHERE trader_id=? AND leader_pos_id=? AND closed_at IS NULL ORDER BY id DESC LIMIT 1`, traderID, leaderPosID)
	return scanCopyGuardCycle(row)
}

type rowScanner interface{ Scan(...interface{}) error }

const copyGuardCycleSelect = `SELECT id,trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot,leader_entry_price,follower_entry_price,follower_notional,baseline_notional,baseline_realized_pnl,baseline_leader_size,shadow_leader_size,baseline_leader_size_available,baseline_leader_size_quality,account_equity,atr_at_entry,atr_at_stop,leader_entry_at_stop,last_observed_price,reentry_count,stop_count,actual_pnl,baseline_pnl,fees,funding_fee,liquidation_penalty,slippage,net_guard_effect,tracking_difference,accounting_status,accounting_error,baseline_source,protection_status,protection_retries,protection_coverage,protection_error,protection_missing_seconds,follower_pos_id,entry_order_id,exit_order_id,protection_missing_at,protection_last_retry_at,pending_since,reconciled_at,opened_at,stopped_at,closed_at,updated_at FROM copy_guard_cycles`

func scanCopyGuardCycle(row rowScanner) (*CopyGuardCycle, error) {
	var c CopyGuardCycle
	var opened, updated string
	var stopped, closed, missing, lastRetry, pending, reconciled sql.NullString
	err := row.Scan(&c.ID, &c.TraderID, &c.LeaderID, &c.LeaderPosID, &c.Symbol, &c.Side, &c.MarginMode, &c.Status, &c.PolicySnapshot, &c.LeaderEntryPrice, &c.FollowerEntryPrice, &c.FollowerNotional, &c.BaselineNotional, &c.BaselineRealizedPnL, &c.BaselineLeaderSize, &c.ShadowLeaderSize, &c.BaselineAvailable, &c.BaselineQuality, &c.AccountEquity, &c.ATRAtEntry, &c.ATRAtStop, &c.LeaderEntryAtStop, &c.LastObservedPrice, &c.ReentryCount, &c.StopCount, &c.ActualPnL, &c.BaselinePnL, &c.Fees, &c.FundingFee, &c.LiquidationPenalty, &c.Slippage, &c.NetGuardEffect, &c.TrackingDifference, &c.AccountingStatus, &c.AccountingError, &c.BaselineSource, &c.ProtectionStatus, &c.ProtectionRetries, &c.ProtectionCoverage, &c.ProtectionError, &c.ProtectionMissingSeconds, &c.FollowerPosID, &c.EntryOrderID, &c.ExitOrderID, &missing, &lastRetry, &pending, &reconciled, &opened, &stopped, &closed, &updated)
	if err != nil {
		return nil, err
	}
	if c.OpenedAt, err = parseDBTime(opened); err != nil {
		return nil, fmt.Errorf("copy guard cycle %d opened_at: %w", c.ID, err)
	}
	if c.UpdatedAt, err = parseDBTime(updated); err != nil {
		return nil, fmt.Errorf("copy guard cycle %d updated_at: %w", c.ID, err)
	}
	if c.StoppedAt, err = parseNullableDBTime(stopped); err != nil {
		return nil, fmt.Errorf("copy guard cycle %d stopped_at: %w", c.ID, err)
	}
	if c.ClosedAt, err = parseNullableDBTime(closed); err != nil {
		return nil, fmt.Errorf("copy guard cycle %d closed_at: %w", c.ID, err)
	}
	if c.ProtectionMissingAt, err = parseNullableDBTime(missing); err != nil {
		return nil, fmt.Errorf("copy guard cycle %d protection_missing_at: %w", c.ID, err)
	}
	if c.ProtectionLastRetryAt, err = parseNullableDBTime(lastRetry); err != nil {
		return nil, fmt.Errorf("copy guard cycle %d protection_last_retry_at: %w", c.ID, err)
	}
	if c.PendingSince, err = parseNullableDBTime(pending); err != nil {
		return nil, fmt.Errorf("copy guard cycle %d pending_since: %w", c.ID, err)
	}
	if c.ReconciledAt, err = parseNullableDBTime(reconciled); err != nil {
		return nil, fmt.Errorf("copy guard cycle %d reconciled_at: %w", c.ID, err)
	}
	return &c, nil
}

func parseDBTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}

func parseNullableDBTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil, nil
	}
	parsed, err := parseDBTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// UpdateCopyGuardProtectionHealth 更新周期保护健康状态。
//   - protection_retries：进入健康态（VERIFIED/CLAMPED）时清零——重试封顶
//     （protectionRetryMaxAttempts）的语义是"本次故障连续重试用尽"，不清零
//     会让长生命周期仓位把多次瞬时故障的重试累计起来，之后任何一次轻微
//     降级都直接越过封顶触发 GUARD_UNPROTECTABLE 处置。
//   - 保护缺失窗口（missing_at/missing_seconds）：UNKNOWN/DEGRADED 之外，
//     UNPROTECTED_WARNING（warn 模式裸跑）也是"没有有效保护"，同样计入。
func (s *CopyTradeStore) UpdateCopyGuardProtectionHealth(id int64, status string, coverage float64, lastError, followerPosID, entryOrderID string, incrementRetry bool) error {
	retry := 0
	if incrementRetry {
		retry = 1
	}
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET protection_status=?,protection_coverage=?,protection_error=?,follower_pos_id=CASE WHEN ?<>'' THEN ? ELSE follower_pos_id END,entry_order_id=CASE WHEN ?<>'' THEN ? ELSE entry_order_id END,protection_retries=CASE WHEN ? IN ('VERIFIED','CLAMPED') THEN 0 ELSE protection_retries+? END,protection_missing_seconds=protection_missing_seconds+CASE WHEN ? NOT IN ('UNKNOWN','DEGRADED','UNPROTECTABLE','UNPROTECTED_WARNING','FORCED_EXIT_PENDING') AND protection_missing_at IS NOT NULL THEN MAX(0,(julianday(CURRENT_TIMESTAMP)-julianday(protection_missing_at))*86400) ELSE 0 END,protection_missing_at=CASE WHEN ? IN ('UNKNOWN','DEGRADED','UNPROTECTABLE','UNPROTECTED_WARNING','FORCED_EXIT_PENDING') THEN COALESCE(protection_missing_at,CURRENT_TIMESTAMP) ELSE NULL END,pending_since=CASE WHEN ?='PENDING' THEN COALESCE(pending_since,CURRENT_TIMESTAMP) ELSE NULL END,updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, coverage, lastError, followerPosID, followerPosID, entryOrderID, entryOrderID, status, retry, status, status, status, id)
	return err
}

// BeginCopyGuardProtectionRetry atomically claims one retry slot and records
// the event. Multiple monitors or fast ticks cannot create duplicate retries.
func (s *CopyTradeStore) BeginCopyGuardProtectionRetry(cycle *CopyGuardCycle, delay time.Duration) (bool, error) {
	if cycle == nil {
		return false, fmt.Errorf("nil copy guard cycle")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	delaySeconds := delay.Seconds()
	message := cycle.ProtectionError
	status := cycle.ProtectionStatus
	if status == CopyGuardProtectionPending && cycle.PendingSince != nil && time.Since(*cycle.PendingSince) >= 10*time.Second {
		status = CopyGuardProtectionUnknown
		if message == "" {
			message = "protective stop was not established within 10 seconds"
		}
	}
	res, err := tx.Exec(`UPDATE copy_guard_cycles SET protection_status=?,protection_error=?,protection_retries=protection_retries+1,protection_last_retry_at=CURRENT_TIMESTAMP,protection_missing_at=CASE WHEN ? IN ('UNKNOWN','DEGRADED','UNPROTECTABLE','UNPROTECTED_WARNING','FORCED_EXIT_PENDING') THEN COALESCE(protection_missing_at,CURRENT_TIMESTAMP) ELSE protection_missing_at END,updated_at=CURRENT_TIMESTAMP WHERE id=? AND protection_retries=? AND (protection_last_retry_at IS NULL OR (julianday(CURRENT_TIMESTAMP)-julianday(protection_last_retry_at))*86400>=?)`, status, message, status, cycle.ID, cycle.ProtectionRetries, delaySeconds)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	if err != nil || changed == 0 {
		return false, err
	}
	metadata, _ := json.Marshal(map[string]interface{}{
		"retry":      cycle.ProtectionRetries + 1,
		"last_error": message,
		"coverage":   cycle.ProtectionCoverage,
	})
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,metadata_json) VALUES(?,?,?,?)`, cycle.ID, cycle.TraderID, "PROTECTION_RETRY", string(metadata)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *CopyTradeStore) ListOpenCopyGuardCycles(traderID string) ([]*CopyGuardCycle, error) {
	rows, err := s.db.Query(copyGuardCycleSelect+` WHERE trader_id=? AND closed_at IS NULL ORDER BY id`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyGuardCycle
	for rows.Next() {
		c, err := scanCopyGuardCycle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCopyGuardObservation 更新观察期状态与观测价。closed_at 防护：周期关闭
// 与观察轮询存在竞态（如止损触发与领航员平仓同一轮发生），已关闭的周期绝不能
// 被观察更新改回 STOPPED_WATCHING。
func (s *CopyTradeStore) UpdateCopyGuardObservation(id int64, status string, leaderEntry, lastPrice, atr float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET status=?,leader_entry_price=?,last_observed_price=?,atr_at_stop=CASE WHEN ? > 0 THEN ? ELSE atr_at_stop END,updated_at=CURRENT_TIMESTAMP WHERE id=? AND closed_at IS NULL`, status, leaderEntry, lastPrice, atr, atr, id)
	return err
}

// UpdateCopyGuardObservedPrice 只刷新最后观测价（FOLLOWING 阶段轻量更新）。
// 背景：last_observed_price 原来只在观察期更新，跟随期一直停留在开仓价；
// 一旦周期在跟随期直接结束（止损与领航员平仓同轮发生），估算基线会用到
// 陈旧价格。该方法在跟随期轮询时保持观测价新鲜，不改动状态与其他列。
// 按 trader+leaderPosID 定位，省去每轮先读周期的开销。
func (s *CopyTradeStore) UpdateCopyGuardObservedPrice(traderID, leaderPosID string, lastPrice float64) error {
	if lastPrice <= 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET last_observed_price=?,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND closed_at IS NULL`, lastPrice, traderID, leaderPosID)
	return err
}

// SnapshotCopyGuardLeaderEntryAtStop 记录止损触发时的领航员持仓均价快照，
// 供重入锚点取保守值（见 CopyGuardCycle.LeaderEntryAtStop）。
func (s *CopyTradeStore) SnapshotCopyGuardLeaderEntryAtStop(id int64, price float64) error {
	if price <= 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET leader_entry_at_stop=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND closed_at IS NULL`, price, id)
	return err
}

// BackfillCopyGuardAccountEquity 回填开仓时因 API 限流（OKX 50011）等原因
// 未取到的账户权益快照；仅在当前值为 0 时写入，不覆盖真实快照。
func (s *CopyTradeStore) BackfillCopyGuardAccountEquity(id int64, equity float64) error {
	if equity <= 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET account_equity=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_equity<=0`, equity, id)
	return err
}

func (s *CopyTradeStore) UpdateCopyGuardPendingOrder(id int64, clientOrderID string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET status=?,entry_order_id=?,pending_since=COALESCE(pending_since,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE id=?`, CopyGuardReentryPending, clientOrderID, id)
	return err
}

func (s *CopyTradeStore) UpdateCopyGuardExecutionOrder(id int64, entryOrderID, exitOrderID string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET entry_order_id=CASE WHEN ?<>'' THEN ? ELSE entry_order_id END,exit_order_id=CASE WHEN ?<>'' THEN ? ELSE exit_order_id END,updated_at=CURRENT_TIMESTAMP WHERE id=?`, entryOrderID, entryOrderID, exitOrderID, exitOrderID, id)
	return err
}

func (s *CopyTradeStore) UpdateCopyGuardFollowerPosition(id int64, followerPosID string, entryPrice, notional float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET follower_pos_id=CASE WHEN ?<>'' THEN ? ELSE follower_pos_id END,follower_entry_price=CASE WHEN ?>0 THEN ? ELSE follower_entry_price END,follower_notional=CASE WHEN ?>0 THEN ? ELSE follower_notional END,updated_at=CURRENT_TIMESTAMP WHERE id=?`, followerPosID, followerPosID, entryPrice, entryPrice, notional, notional, id)
	return err
}

func (s *CopyTradeStore) UpdateCopyGuardShadow(id int64, leaderEntry, lastPrice, baselineNotional, leaderSize float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldEntry, oldNotional, oldLeaderSize, realized float64
	var side string
	if err = tx.QueryRow(`SELECT leader_entry_price,baseline_notional,baseline_realized_pnl,shadow_leader_size,side FROM copy_guard_cycles WHERE id=?`, id).Scan(&oldEntry, &oldNotional, &realized, &oldLeaderSize, &side); err != nil {
		return err
	}
	// A size reduction realizes the corresponding shadow quantity at the current
	// observed price. Adds are represented by OKX's new average entry and target
	// notional, so no synthetic realized PnL is invented for them.
	if oldLeaderSize > 0 && leaderSize < oldLeaderSize && oldEntry > 0 && lastPrice > 0 {
		reduced := oldNotional * (oldLeaderSize - leaderSize) / oldLeaderSize
		move := (lastPrice - oldEntry) / oldEntry
		if strings.EqualFold(side, "short") {
			move = -move
		}
		realized += reduced * move
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET leader_entry_price=?,last_observed_price=?,baseline_notional=?,baseline_realized_pnl=?,shadow_leader_size=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, leaderEntry, lastPrice, baselineNotional, realized, leaderSize, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CopyTradeStore) RecordCopyGuardStop(cycle *CopyGuardCycle, atr, exitPrice, pnl, fee, slippage float64, algoID string, metadata map[string]interface{}) error {
	if cycle == nil {
		return fmt.Errorf("nil copy guard cycle")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actualOrderID := metadataString(metadata, "actual_order_id")
	res, err := tx.Exec(`UPDATE copy_guard_attempts SET status='STOPPED',exit_price=?,stop_fill_price=?,pnl=?,fee=?,stop_algo_id=?,exit_order_id=CASE WHEN ?<>'' THEN ? ELSE exit_order_id END,closed_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND attempt_no=? AND status='OPEN'`, exitPrice, exitPrice, pnl, fee, algoID, actualOrderID, actualOrderID, cycle.ID, cycle.ReentryCount)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Restart/crash replay of the same exchange stop. Only an attempt that is
		// already STOPPED is idempotent. A missing/non-open attempt is a broken
		// lifecycle and must not silently mark the exchange order as handled.
		var status string
		if queryErr := tx.QueryRow(`SELECT status FROM copy_guard_attempts WHERE cycle_id=? AND attempt_no=?`, cycle.ID, cycle.ReentryCount).Scan(&status); queryErr != nil {
			return fmt.Errorf("verify duplicate copy guard stop: %w", queryErr)
		}
		if status != "STOPPED" {
			return fmt.Errorf("copy guard stop attempt is not open or stopped: status=%s", status)
		}
		return tx.Commit()
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET status=?,atr_at_stop=?,stop_count=stop_count+1,actual_pnl=actual_pnl+?,fees=fees+?,slippage=slippage+?,stopped_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, CopyGuardStoppedWatching, atr, pnl, fee, slippage, cycle.ID); err != nil {
		return err
	}
	raw, _ := json.Marshal(metadata)
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,price,quantity,pnl,fee,metadata_json) VALUES(?,?,?,?,?,?,?,?)`, cycle.ID, cycle.TraderID, "STOP_TRIGGERED", exitPrice, metadataFloat(metadata, "quantity"), pnl, fee, string(raw)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Seam B：提交后镜像止损事件（best-effort）
	s.mirrorGuardEventToCopyEvents(cycle.ID, cycle.TraderID, "STOP_TRIGGERED", exitPrice, metadataFloat(metadata, "quantity"), 0, pnl, metadata)
	return nil
}

func (s *CopyTradeStore) ReconcileCopyGuardAttempt(cycleID int64, attempt int, pnl, fee, funding, penalty float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldPnL, oldFee, oldFunding, oldPenalty float64
	var reconciled bool
	if err = tx.QueryRow(`SELECT pnl,fee,funding_fee,liquidation_penalty,reconciled FROM copy_guard_attempts WHERE cycle_id=? AND attempt_no=?`, cycleID, attempt).Scan(&oldPnL, &oldFee, &oldFunding, &oldPenalty, &reconciled); err != nil {
		return err
	}
	if reconciled {
		return tx.Commit()
	}
	if _, err = tx.Exec(`UPDATE copy_guard_attempts SET pnl=?,fee=?,funding_fee=?,liquidation_penalty=?,reconciled=1 WHERE cycle_id=? AND attempt_no=?`, pnl, fee, funding, penalty, cycleID, attempt); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET actual_pnl=actual_pnl+?-?,fees=fees+?-?,funding_fee=funding_fee+?-?,liquidation_penalty=liquidation_penalty+?-?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, pnl, oldPnL, fee, oldFee, funding, oldFunding, penalty, oldPenalty, cycleID); err != nil {
		return err
	}
	return tx.Commit()
}

func metadataFloat(metadata map[string]interface{}, key string) float64 {
	if metadata == nil {
		return 0
	}
	switch v := metadata[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func metadataString(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return value
}

func metadataBool(metadata map[string]interface{}, key string) bool {
	if metadata == nil {
		return false
	}
	value, _ := metadata[key].(bool)
	return value
}

// RecordCopyGuardStopObserved marks a stop that was detected indirectly (the
// follower position vanished while the leader still holds): the cycle enters
// STOPPED_WATCHING and the current attempt is closed with the observed price
// so it can be reconciled later, mirroring RecordCopyGuardStop.
//
// 统计口径修正（v5）：事件 pnl 字段不再写入领航员浮亏（旧行为把领航员的
// -358 记成了跟随者盈亏）；跟随者真实盈亏由 attempt 对账（ATTEMPT_RECONCILED）
// 补全，领航员参考信息放 metadata。
func (s *CopyTradeStore) RecordCopyGuardStopObserved(cycleID int64, traderID string, attemptNo int, atr, price, quantity float64, metadata map[string]interface{}) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE copy_guard_attempts SET status='STOPPED',exit_price=?,stop_fill_price=?,closed_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND attempt_no=? AND status='OPEN'`, price, price, cycleID, attemptNo)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var status string
		if queryErr := tx.QueryRow(`SELECT status FROM copy_guard_attempts WHERE cycle_id=? AND attempt_no=?`, cycleID, attemptNo).Scan(&status); queryErr != nil {
			return fmt.Errorf("verify duplicate observed copy guard stop: %w", queryErr)
		}
		if status != "STOPPED" {
			return fmt.Errorf("observed copy guard stop attempt is not open or stopped: status=%s", status)
		}
		return tx.Commit()
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET status=?,atr_at_stop=?,stop_count=stop_count+1,stopped_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, CopyGuardStoppedWatching, atr, cycleID); err != nil {
		return err
	}
	raw, _ := json.Marshal(metadata)
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,price,quantity,metadata_json) VALUES(?,?,?,?,?,?)`, cycleID, traderID, "STOP_CONFIRMED", price, quantity, string(raw)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Seam B：提交后镜像止损确认事件（best-effort）
	s.mirrorGuardEventToCopyEvents(cycleID, traderID, "STOP_CONFIRMED", price, quantity, 0, 0, metadata)
	return nil
}

// RecordCopyGuardUnprotectedExit closes a position that was market-exited
// because no protective order could be established. It deliberately does not
// increment stop_count, write stop_fill_price or mark stopped_by_risk: those
// facts are reserved for actual/verified protective-stop outcomes and are the
// eligibility boundary for AI reentry.
func (s *CopyTradeStore) RecordCopyGuardUnprotectedExit(cycleID int64, traderID, leaderPosID string, attemptNo int, price, quantity float64, reason string) error {
	if cycleID <= 0 || traderID == "" || leaderPosID == "" || attemptNo < 0 {
		return fmt.Errorf("invalid unprotected exit")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE copy_guard_attempts
		SET status='FORCED_EXIT',exit_price=?,closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP)
		WHERE cycle_id=? AND attempt_no=? AND status='OPEN'`, price, cycleID, attemptNo)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var status string
		if scanErr := tx.QueryRow(`SELECT status FROM copy_guard_attempts WHERE cycle_id=? AND attempt_no=?`, cycleID, attemptNo).Scan(&status); scanErr != nil || status != "FORCED_EXIT" {
			return fmt.Errorf("unprotected exit attempt is not open or already exited: status=%s", status)
		}
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET
		status=?,protection_status=?,protection_coverage=0,protection_error=?,
		accounting_status='PENDING',updated_at=CURRENT_TIMESTAMP
		WHERE id=?`, CopyGuardProtectionExited, CopyGuardProtectionCanceled, reason, cycleID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_trade_position_mappings SET
		status=?,updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND leader_pos_id=? AND status IN ('active','stopped_by_risk')`,
		MappingStatusDetached, traderID, leaderPosID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE copy_guard_reentry_candidates SET
		status='INVALIDATED',last_error=?,closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND leader_pos_id=? AND status IN ('WATCHING','REVIEWING','WAITING','ENTRY_PENDING','PAUSED')`,
		"PROTECTION_EXITED: "+reason, traderID, leaderPosID); err != nil {
		return err
	}
	metadata := normalizeCopyGuardEventMetadata(cycleID, traderID, "GUARD_FORCED_EXIT", map[string]interface{}{
		"confirmation": "unprotected_market_exit",
		"reason":       reason,
		"reason_code":  "PROTECTION_ESTABLISHMENT_FAILED",
		"attempt_no":   attemptNo,
	})
	raw, _ := json.Marshal(metadata)
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,price,quantity,metadata_json)
		VALUES(?,?,?,?,?,?)`, cycleID, traderID, "GUARD_FORCED_EXIT", price, quantity, string(raw)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.mirrorGuardEventToCopyEvents(cycleID, traderID, "GUARD_FORCED_EXIT", price, quantity, 0, 0, metadata)
	return nil
}

func (s *CopyTradeStore) RecordCopyGuardReentryFilled(cycle *CopyGuardCycle, entryPrice, notional, quantity, atr float64, metadata map[string]interface{}) error {
	if cycle == nil {
		return fmt.Errorf("nil copy guard cycle")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Production execution asks this transaction to reactivate the stopped
	// mapping together with the cycle/attempt ledger. Previously these were two
	// independent commits: a process/database failure between them could leave
	// an exchange fill attached to an active mapping but a still-pending cycle.
	// Tests and import tools may omit activate_mapping when no mapping exists.
	if metadataBool(metadata, "activate_mapping") {
		leaderSize := metadataFloat(metadata, "leader_size")
		mappingResult, mappingErr := tx.Exec(`UPDATE copy_trade_position_mappings SET status='active',open_price=?,open_size_usd=?,last_known_size=?,stopped_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE trader_id=? AND leader_pos_id=? AND status='stopped_by_risk'`, entryPrice, notional, leaderSize, cycle.TraderID, cycle.LeaderPosID)
		if mappingErr != nil {
			return mappingErr
		}
		if n, _ := mappingResult.RowsAffected(); n != 1 {
			// Backward-compatible crash recovery: older builds activated the
			// mapping in a separate transaction before writing the cycle ledger.
			// An already-active mapping is therefore acceptable only while this
			// cycle still owns the REENTRY_PENDING transition checked below.
			var mappingStatus string
			if queryErr := tx.QueryRow(`SELECT status FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`, cycle.TraderID, cycle.LeaderPosID).Scan(&mappingStatus); queryErr != nil {
				return fmt.Errorf("copy guard stopped mapping is unavailable for reentry: %w", queryErr)
			}
			if mappingStatus != "active" {
				return fmt.Errorf("copy guard mapping cannot enter reentry: status=%s", mappingStatus)
			}
		}
	}
	exchangeOrderID := metadataString(metadata, "exchange_order_id")
	res, err := tx.Exec(`UPDATE copy_guard_cycles SET status=?,reentry_count=reentry_count+1,follower_entry_price=?,follower_notional=?,entry_order_id=CASE WHEN ?<>'' THEN ? ELSE entry_order_id END,pending_since=NULL,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`, CopyGuardFollowingReentry, entryPrice, notional, exchangeOrderID, exchangeOrderID, cycle.ID, CopyGuardReentryPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("copy guard reentry fill is no longer pending")
	}
	attempt := cycle.ReentryCount + 1
	if _, err = tx.Exec(`INSERT INTO copy_guard_attempts(cycle_id,attempt_no,status,entry_price,quantity,notional,atr,entry_order_id) VALUES(?,?,'OPEN',?,?,?,?,?) ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET status='OPEN',entry_price=excluded.entry_price,quantity=excluded.quantity,notional=excluded.notional,atr=excluded.atr,entry_order_id=CASE WHEN excluded.entry_order_id<>'' THEN excluded.entry_order_id ELSE copy_guard_attempts.entry_order_id END,opened_at=CURRENT_TIMESTAMP,closed_at=NULL`, cycle.ID, attempt, entryPrice, quantity, notional, atr, exchangeOrderID); err != nil {
		return err
	}
	raw, _ := json.Marshal(metadata)
	eventType := "REENTRY_FILLED"
	if recovered, _ := metadata["recovered_after_restart"].(bool); recovered {
		eventType = "REENTRY_RECOVERED_AFTER_RESTART"
	}
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,price,quantity,notional,metadata_json) VALUES(?,?,?,?,?,?,?)`, cycle.ID, cycle.TraderID, eventType, entryPrice, quantity, notional, string(raw)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Seam B：提交后镜像二次入场成交事件（best-effort）
	s.mirrorGuardEventToCopyEvents(cycle.ID, cycle.TraderID, eventType, entryPrice, quantity, notional, 0, metadata)
	return nil
}

func (s *CopyTradeStore) CloseCopyGuardCycle(id int64, status string, actual, baseline, fees, funding, penalty, slippage float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pending int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_guard_attempts WHERE cycle_id=? AND (reconciled=0 OR status='OPEN')`, id).Scan(&pending); err != nil {
		return err
	}
	accountingStatus := CopyGuardAccountingReconciled
	tracking, guardEffect := actual-baseline, 0.0
	if pending > 0 {
		accountingStatus = CopyGuardAccountingPending
		tracking = 0
	} else {
		var stopCount int
		if err = tx.QueryRow(`SELECT stop_count FROM copy_guard_cycles WHERE id=?`, id).Scan(&stopCount); err != nil {
			return err
		}
		intervened, interventionErr := cycleHasGuardInterventionTx(tx, id, stopCount)
		if interventionErr != nil {
			return interventionErr
		}
		if intervened {
			guardEffect = actual - baseline
		}
	}
	_, err = tx.Exec(`UPDATE copy_guard_cycles SET status=?,actual_pnl=?,baseline_pnl=?,fees=?,funding_fee=?,liquidation_penalty=?,slippage=?,tracking_difference=?,net_guard_effect=?,accounting_status=?,accounting_error='',reconciled_at=CASE WHEN ?=? THEN CURRENT_TIMESTAMP ELSE NULL END,closed_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, actual, baseline, fees, funding, penalty, slippage, tracking, guardEffect, accountingStatus, accountingStatus, CopyGuardAccountingReconciled, id)
	if err != nil {
		return err
	}
	if err = terminalizeCopyGuardAuxiliaryStateTx(tx, id, status); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if accountingStatus == CopyGuardAccountingReconciled {
		s.emitCopyGuardClosedSummary(id)
	}
	return nil
}

// BeginCopyGuardAccounting closes the trading lifecycle while keeping financial
// settlement pending until the execution exchange returns terminal
// fill/settlement data (OKX position history / Binance closed PnL).
func (s *CopyTradeStore) BeginCopyGuardAccounting(id int64, status, exitOrderID string, baseline float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET status=?,exit_order_id=CASE WHEN ?<>'' THEN ? ELSE exit_order_id END,baseline_pnl=?,net_guard_effect=0,tracking_difference=0,accounting_status=?,accounting_error='',closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, exitOrderID, exitOrderID, baseline, CopyGuardAccountingPending, id); err != nil {
		return err
	}
	if err = terminalizeCopyGuardAuxiliaryStateTx(tx, id, status); err != nil {
		return err
	}
	if exitOrderID != "" {
		if _, err = tx.Exec(`UPDATE copy_guard_attempts SET exit_order_id=? WHERE cycle_id=? AND attempt_no=(SELECT reentry_count FROM copy_guard_cycles WHERE id=?)`, exitOrderID, id, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// terminalizeCopyGuardAuxiliaryStateTx is the single cycle-termination boundary
// for manual signals, AI candidates, risk reservations and provably pre-submit
// AI execution leases. Intents with any durable order attempt or submission
// identity are deliberately left for exchange reconciliation.
func terminalizeCopyGuardAuxiliaryStateTx(tx *sql.Tx, cycleID int64, status string) error {
	if tx == nil || cycleID <= 0 {
		return fmt.Errorf("invalid Copy Guard auxiliary termination")
	}
	detail := "cycle terminated before exchange submission: " + status
	if _, err := tx.Exec(`UPDATE copy_guard_manual_reentry_signals
		SET status=?,error='周期已结束: '||?
		WHERE cycle_id=? AND status=?`,
		ManualReentryStatusInvalidated, status, cycleID, ManualReentryStatusPending); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_guard_reentry_candidates
		SET status=?,last_error='cycle ended: '||?,closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE cycle_id=? AND status IN (?,?,?,?,?)`,
		ReentryCandidateInvalidated, status, cycleID,
		ReentryCandidateWatching, ReentryCandidateReviewing, ReentryCandidateWaiting,
		ReentryCandidateEntryPending, ReentryCandidatePaused); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_guard_risk_reservations
		SET status='RELEASED',released_at=COALESCE(released_at,CURRENT_TIMESTAMP)
		WHERE cycle_id=? AND status IN ('ACTIVE','CONSUMED')`, cycleID); err != nil {
		return err
	}
	// Synchronize source-transition audit rows before changing the parent
	// intent status, using the same no-side-effect proof as the intent UPDATE.
	if _, err := tx.Exec(`UPDATE copy_trade_source_transitions SET status=?,updated_at=CURRENT_TIMESTAMP
		WHERE intent_id IN (
			SELECT i.id FROM copy_trade_execution_intents i
			WHERE i.cycle_id=? AND i.source_kind='AI_REENTRY'
			  AND i.status IN ('RESERVED','RECONCILING')
			  AND i.submitted_at IS NULL AND COALESCE(i.exchange_order_id,'')=''
			  AND COALESCE(i.filled_quantity,0)=0
			  AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=i.id)
		)`, ExecutionIntentCycleTerminatedBeforeSubmit, cycleID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_trade_execution_intents
		SET status='FAILED',reason_code=?,last_error=?,terminal_at=COALESCE(terminal_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP
		WHERE cycle_id=? AND source_kind='AI_REENTRY'
		  AND status IN ('RESERVED','RECONCILING')
		  AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')=''
		  AND COALESCE(filled_quantity,0)=0
		  AND NOT EXISTS (SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)`,
		ExecutionIntentCycleTerminatedBeforeSubmit, detail, cycleID); err != nil {
		return err
	}
	return nil
}

// MarkCopyGuardAccountingDelayed flags a cycle whose execution-exchange
// settlement data (OKX position history / Binance closed PnL) is late;
// automatic reconciliation keeps retrying (DELAYED cycles stay in
// ListCopyGuardCyclesPendingAccounting).
func (s *CopyTradeStore) MarkCopyGuardAccountingDelayed(id int64, message string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET accounting_status=?,accounting_error=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND accounting_status IN (?,?)`, CopyGuardAccountingDelayed, message, id, CopyGuardAccountingPending, CopyGuardAccountingDelayed)
	return err
}

// MarkCopyGuardAccountingUnrecoverable permanently parks a cycle whose
// settlement can no longer be reconciled automatically; it leaves the retry
// queue and the UI shows "not automatically recoverable".
func (s *CopyTradeStore) MarkCopyGuardAccountingUnrecoverable(id int64, message string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET accounting_status=?,accounting_error=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND accounting_status IN (?,?)`, CopyGuardAccountingUnrecoverable, message, id, CopyGuardAccountingPending, CopyGuardAccountingDelayed)
	return err
}

// CompleteCopyGuardAccounting applies one authoritative OKX position-history
// record exactly once and only then exposes the cycle to aggregate metrics.
// SetCopyGuardBaselineSource records how the (estimated) baseline price was
// obtained; see CopyGuardCycle.BaselineSource.
func (s *CopyTradeStore) SetCopyGuardBaselineSource(id int64, source string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET baseline_source=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, source, id)
	return err
}

// UpdateCopyGuardBaselineOutcome replaces the estimated baseline of a closed
// cycle (e.g. once the leader's public position history became available) and
// recomputes tracking difference / net guard effect when the cycle is already
// reconciled. For PENDING/DELAYED cycles only baseline_pnl changes; the final
// numbers are computed by CompleteCopyGuardAccounting later.
func (s *CopyTradeStore) UpdateCopyGuardBaselineOutcome(id int64, baseline float64, source string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actual float64
	var stopCount int
	var accountingStatus string
	if err = tx.QueryRow(`SELECT actual_pnl,stop_count,accounting_status FROM copy_guard_cycles WHERE id=?`, id).Scan(&actual, &stopCount, &accountingStatus); err != nil {
		return err
	}
	if accountingStatus == CopyGuardAccountingReconciled {
		tracking, guardEffect := actual-baseline, 0.0
		intervened, interventionErr := cycleHasGuardInterventionTx(tx, id, stopCount)
		if interventionErr != nil {
			return interventionErr
		}
		if intervened {
			guardEffect = tracking
		}
		if _, err = tx.Exec(`UPDATE copy_guard_cycles SET baseline_pnl=?,baseline_source=?,tracking_difference=?,net_guard_effect=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, baseline, source, tracking, guardEffect, id); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec(`UPDATE copy_guard_cycles SET baseline_pnl=?,baseline_source=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, baseline, source, id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.emitCopyGuardClosedSummary(id)
	return nil
}

// emitCopyGuardClosedSummary writes exactly one final, reconciled attribution
// anchor. Email dispatchers and schema-v4 exporters consume the same event so
// the trading transaction never depends on notification availability.
func (s *CopyTradeStore) emitCopyGuardClosedSummary(cycleID int64) {
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_events WHERE cycle_id=? AND type='CYCLE_CLOSED_SUMMARY'`, cycleID).Scan(&exists); err != nil || exists > 0 {
		return
	}
	cycle, err := s.GetCopyGuardCycle(cycleID)
	if err != nil || cycle.AccountingStatus != CopyGuardAccountingReconciled || cycle.ClosedAt == nil {
		return
	}
	attempts, err := s.ListCopyGuardAttempts(cycleID)
	if err != nil {
		return
	}
	stopOnly, first, second, reentries := float64(0), float64(0), float64(0), float64(0)
	for _, attempt := range attempts {
		switch attempt.AttemptNo {
		case 0:
			stopOnly = attempt.PnL
		case 1:
			first = attempt.PnL
			reentries += attempt.PnL
		case 2:
			second = attempt.PnL
			reentries += attempt.PnL
		default:
			reentries += attempt.PnL
		}
	}
	_ = s.SaveCopyGuardEvent(&CopyGuardEvent{CycleID: cycle.ID, TraderID: cycle.TraderID, Type: "CYCLE_CLOSED_SUMMARY", PnL: cycle.ActualPnL, Metadata: map[string]interface{}{"accounting_status": cycle.AccountingStatus, "actual_pnl": cycle.ActualPnL, "baseline_no_guard_pnl": cycle.BaselinePnL, "stop_only_pnl": stopOnly, "first_reentry_pnl": first, "second_reentry_pnl": second, "reentry_contribution": reentries, "net_guard_effect": cycle.NetGuardEffect, "fees": cycle.Fees, "slippage": cycle.Slippage}})
}

// ListCopyGuardCyclesNeedingBaselineMigration returns closed cycles still on
// the v1 baseline (leader-scaled shadow notional) that are eligible for the
// one-time own-path recomputation: reconciled and at least one stop fired
// (cycles without stops have net_guard_effect fixed at 0, nothing to fix).
func (s *CopyTradeStore) ListCopyGuardCyclesNeedingBaselineMigration() ([]*CopyGuardCycle, error) {
	rows, err := s.db.Query(copyGuardCycleSelect+` WHERE COALESCE(baseline_version,1)<2 AND closed_at IS NOT NULL AND accounting_status=?
		AND (stop_count>0 OR EXISTS (SELECT 1 FROM copy_guard_events e WHERE e.cycle_id=copy_guard_cycles.id AND e.type='GUARD_FORCED_EXIT'))
		ORDER BY id`, CopyGuardAccountingReconciled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardCycle{}
	for rows.Next() {
		c, scanErr := scanCopyGuardCycle(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ApplyCopyGuardBaselineMigration overwrites a reconciled cycle's baseline with
// the own-path recomputation and refreshes the derived metrics. baseline_source
// is preserved (the close price origin did not change, only the notional basis).
func (s *CopyTradeStore) ApplyCopyGuardBaselineMigration(id int64, baseline float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actual float64
	var stopCount int
	if err = tx.QueryRow(`SELECT actual_pnl,stop_count FROM copy_guard_cycles WHERE id=? AND accounting_status=?`, id, CopyGuardAccountingReconciled).Scan(&actual, &stopCount); err != nil {
		return err
	}
	tracking, guardEffect := actual-baseline, 0.0
	intervened, interventionErr := cycleHasGuardInterventionTx(tx, id, stopCount)
	if interventionErr != nil {
		return interventionErr
	}
	if intervened {
		guardEffect = tracking
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET baseline_pnl=?,tracking_difference=?,net_guard_effect=?,baseline_version=2,updated_at=CURRENT_TIMESTAMP WHERE id=?`, baseline, tracking, guardEffect, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.emitCopyGuardClosedSummary(id)
	return nil
}

// FinishCopyGuardBaselineMigration bumps every remaining v1 row to v2 so the
// startup migration never rescans them: rows we could not recompute keep their
// original values, and rows still open will close under the new formula.
func (s *CopyTradeStore) FinishCopyGuardBaselineMigration() error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET baseline_version=2 WHERE COALESCE(baseline_version,1)<2`)
	return err
}

// ListCopyGuardCyclesWithEstimatedBaseline returns recently closed cycles
// whose baseline still relies on the last observed mark price, so the engine
// can retry calibrating them from the leader's public position history.
func (s *CopyTradeStore) ListCopyGuardCyclesWithEstimatedBaseline(traderID string, maxAge time.Duration) ([]*CopyGuardCycle, error) {
	cutoff := time.Now().Add(-maxAge).UTC().Format("2006-01-02 15:04:05")
	rows, err := s.db.Query(copyGuardCycleSelect+` WHERE trader_id=? AND baseline_source='last_observed' AND closed_at IS NOT NULL AND closed_at>=? ORDER BY closed_at,id`, traderID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardCycle{}
	for rows.Next() {
		c, scanErr := scanCopyGuardCycle(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *CopyTradeStore) CompleteCopyGuardAccounting(cycleID int64, attemptNo int, exitPrice, pnl, fee, funding, penalty float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actual, fees, fundingTotal, penaltyTotal, baseline float64
	var stopCount int
	var accountingStatus string
	if err = tx.QueryRow(`SELECT actual_pnl,fees,funding_fee,liquidation_penalty,baseline_pnl,stop_count,accounting_status FROM copy_guard_cycles WHERE id=?`, cycleID).Scan(&actual, &fees, &fundingTotal, &penaltyTotal, &baseline, &stopCount, &accountingStatus); err != nil {
		return err
	}
	if accountingStatus == CopyGuardAccountingReconciled {
		return tx.Commit()
	}
	var oldPnL, oldFee, oldFunding, oldPenalty float64
	var reconciled bool
	if err = tx.QueryRow(`SELECT pnl,fee,funding_fee,liquidation_penalty,reconciled FROM copy_guard_attempts WHERE cycle_id=? AND attempt_no=?`, cycleID, attemptNo).Scan(&oldPnL, &oldFee, &oldFunding, &oldPenalty, &reconciled); err != nil {
		return err
	}
	if !reconciled {
		actual += pnl - oldPnL
		fees += fee - oldFee
		fundingTotal += funding - oldFunding
		penaltyTotal += penalty - oldPenalty
	}
	if _, err = tx.Exec(`UPDATE copy_guard_attempts SET status='CLOSED',exit_price=?,pnl=?,fee=?,funding_fee=?,liquidation_penalty=?,reconciled=1,closed_at=COALESCE(closed_at,CURRENT_TIMESTAMP) WHERE cycle_id=? AND attempt_no=?`, exitPrice, pnl, fee, funding, penalty, cycleID, attemptNo); err != nil {
		return err
	}
	tracking := actual - baseline
	guardEffect := 0.0
	intervened, interventionErr := cycleHasGuardInterventionTx(tx, cycleID, stopCount)
	if interventionErr != nil {
		return interventionErr
	}
	if intervened {
		guardEffect = tracking
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET actual_pnl=?,fees=?,funding_fee=?,liquidation_penalty=?,tracking_difference=?,net_guard_effect=?,accounting_status=?,accounting_error='',reconciled_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, actual, fees, fundingTotal, penaltyTotal, tracking, guardEffect, CopyGuardAccountingReconciled, cycleID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.emitCopyGuardClosedSummary(cycleID)
	return nil
}

func (s *CopyTradeStore) ListCopyGuardCyclesPendingAccounting(traderID string) ([]*CopyGuardCycle, error) {
	rows, err := s.db.Query(copyGuardCycleSelect+` WHERE trader_id=? AND accounting_status IN (?,?) ORDER BY closed_at,id`, traderID, CopyGuardAccountingPending, CopyGuardAccountingDelayed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CopyGuardCycle
	for rows.Next() {
		cycle, scanErr := scanCopyGuardCycle(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, cycle)
	}
	return out, rows.Err()
}

// ListCopyGuardCyclesPendingSummaryEmail returns final cycles whose canonical
// summary exists but whose asynchronous email has not reached a terminal
// delivery state. A persistent SENT/DISABLED event prevents restart duplicates;
// transient rate-limit, queue-full and SMTP failures remain eligible to retry.
func (s *CopyTradeStore) ListCopyGuardCyclesPendingSummaryEmail(traderID string, limit int) ([]*CopyGuardCycle, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := s.db.Query(`SELECT c.id FROM copy_guard_cycles c
		WHERE c.trader_id=? AND c.closed_at IS NOT NULL AND c.accounting_status=?
		AND EXISTS (SELECT 1 FROM copy_guard_events e WHERE e.cycle_id=c.id AND e.type='CYCLE_CLOSED_SUMMARY')
		AND NOT EXISTS (SELECT 1 FROM copy_guard_events e WHERE e.cycle_id=c.id AND e.type IN ('CYCLE_SUMMARY_EMAIL_SENT','CYCLE_SUMMARY_EMAIL_DISABLED'))
		ORDER BY c.closed_at DESC,c.id DESC LIMIT ?`, traderID, CopyGuardAccountingReconciled, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*CopyGuardCycle, 0, len(ids))
	for _, id := range ids {
		cycle, err := s.GetCopyGuardCycle(id)
		if err != nil {
			return nil, err
		}
		out = append(out, cycle)
	}
	return out, nil
}

func (s *CopyTradeStore) ListCopyGuardCyclesWithUnreconciledStops(traderID string) ([]*CopyGuardCycle, error) {
	rows, err := s.db.Query(`SELECT DISTINCT c.id FROM copy_guard_cycles c JOIN copy_guard_attempts a ON a.cycle_id=c.id WHERE c.trader_id=? AND a.status IN ('STOPPED','FORCED_EXIT') AND a.reconciled=0 ORDER BY c.id`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*CopyGuardCycle, 0, len(ids))
	for _, id := range ids {
		cycle, err := s.GetCopyGuardCycle(id)
		if err != nil {
			return nil, err
		}
		out = append(out, cycle)
	}
	return out, nil
}

func (s *CopyTradeStore) FinalizeCopyGuardAccountingFromAttempts(cycleID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pending int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM copy_guard_attempts WHERE cycle_id=? AND (reconciled=0 OR status='OPEN')`, cycleID).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return tx.Commit()
	}
	var actual, baseline float64
	var stopCount int
	if err = tx.QueryRow(`SELECT actual_pnl,baseline_pnl,stop_count FROM copy_guard_cycles WHERE id=?`, cycleID).Scan(&actual, &baseline, &stopCount); err != nil {
		return err
	}
	tracking, guardEffect := actual-baseline, 0.0
	intervened, interventionErr := cycleHasGuardInterventionTx(tx, cycleID, stopCount)
	if interventionErr != nil {
		return interventionErr
	}
	if intervened {
		guardEffect = tracking
	}
	if _, err = tx.Exec(`UPDATE copy_guard_cycles SET tracking_difference=?,net_guard_effect=?,accounting_status=?,accounting_error='',reconciled_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, tracking, guardEffect, CopyGuardAccountingReconciled, cycleID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.emitCopyGuardClosedSummary(cycleID)
	return nil
}

func normalizeCopyGuardEventMetadata(cycleID int64, traderID, eventType string, metadata map[string]interface{}) map[string]interface{} {
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	metadata["schema_version"] = 4
	metadata["cycle_id"] = cycleID
	metadata["trader_id"] = traderID
	// Schema v4 keeps a stable correlation envelope on every event. Producers
	// overwrite these defaults when the value is known; zero/empty explicitly
	// means unavailable, avoiding per-event shape guessing in exports and audits.
	defaults := map[string]interface{}{
		"attempt_no": 0, "candidate_id": int64(0), "analysis_id": int64(0),
		"decision_generation": 0, "reason_code": eventType, "data_hash": "",
		"snapshot_at": time.Now().UTC().Format(time.RFC3339Nano), "model": "",
		"prompt_version": "", "planned_qty": float64(0), "actual_qty": float64(0),
		"stop_price": float64(0), "risk_budget": float64(0), "latency_ms": int64(0),
	}
	for key, value := range defaults {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	return metadata
}

func cycleHasGuardInterventionTx(tx *sql.Tx, cycleID int64, stopCount int) (bool, error) {
	if stopCount > 0 {
		return true, nil
	}
	var exists int
	if err := tx.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM copy_guard_events WHERE cycle_id=? AND type='GUARD_FORCED_EXIT'
	)`, cycleID).Scan(&exists); err != nil {
		return false, err
	}
	return exists != 0, nil
}

func (s *CopyTradeStore) SaveCopyGuardEvent(e *CopyGuardEvent) error {
	if e == nil {
		return fmt.Errorf("nil copy guard event")
	}
	e.Metadata = normalizeCopyGuardEventMetadata(e.CycleID, e.TraderID, e.Type, e.Metadata)
	b, _ := json.Marshal(e.Metadata)
	_, err := s.db.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,price,quantity,notional,pnl,fee,metadata_json) VALUES(?,?,?,?,?,?,?,?,?)`, e.CycleID, e.TraderID, e.Type, e.Price, e.Quantity, e.Notional, e.PnL, e.Fee, string(b))
	if err == nil {
		// Seam B：镜像到统一跟单事件日志（best-effort，白名单命中才落库）
		s.mirrorGuardEventToCopyEvents(e.CycleID, e.TraderID, e.Type, e.Price, e.Quantity, e.Notional, e.PnL, e.Metadata)
	}
	return err
}

func (s *CopyTradeStore) OpenCopyGuardAttempt(cycleID int64, attempt int, entryPrice, notional, quantity, atr float64) error {
	_, err := s.db.Exec(`INSERT INTO copy_guard_attempts(cycle_id,attempt_no,status,entry_price,quantity,notional,atr) VALUES(?,?,'OPEN',?,?,?,?) ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET status='OPEN',entry_price=excluded.entry_price,quantity=excluded.quantity,notional=excluded.notional,atr=excluded.atr,opened_at=CURRENT_TIMESTAMP,closed_at=NULL`, cycleID, attempt, entryPrice, quantity, notional, atr)
	return err
}

func (s *CopyTradeStore) UpdateCopyGuardAttemptPosition(cycleID int64, attempt int, entryPrice, notional, quantity, atr float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_attempts SET entry_price=?,notional=?,quantity=?,atr=CASE WHEN ?>0 THEN ? ELSE atr END WHERE cycle_id=? AND attempt_no=? AND status='OPEN'`, entryPrice, notional, quantity, atr, atr, cycleID, attempt)
	return err
}

func (s *CopyTradeStore) UpdateCopyGuardAttemptIdentity(cycleID int64, attempt int, followerPosID, entryOrderID, exitOrderID string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_attempts SET follower_pos_id=CASE WHEN ?<>'' THEN ? ELSE follower_pos_id END,entry_order_id=CASE WHEN ?<>'' THEN ? ELSE entry_order_id END,exit_order_id=CASE WHEN ?<>'' THEN ? ELSE exit_order_id END WHERE cycle_id=? AND attempt_no=?`, followerPosID, followerPosID, entryOrderID, entryOrderID, exitOrderID, exitOrderID, cycleID, attempt)
	return err
}

func (s *CopyTradeStore) UpdateCopyGuardAttemptProtection(cycleID int64, attempt int, triggerPrice float64, algoID, status string, coverage float64) error {
	if cycleID <= 0 || attempt < 0 || strings.TrimSpace(status) == "" {
		return fmt.Errorf("invalid copy guard attempt protection update")
	}
	_, err := s.db.Exec(`UPDATE copy_guard_attempts SET
		stop_trigger_price=CASE WHEN ?>0 THEN ? ELSE stop_trigger_price END,
		protection_algo_id=CASE WHEN ?<>'' THEN ? ELSE protection_algo_id END,
		protection_status=?,protection_coverage=?,protection_updated_at=CURRENT_TIMESTAMP
		WHERE cycle_id=? AND attempt_no=?`,
		triggerPrice, triggerPrice, algoID, algoID, status, coverage, cycleID, attempt)
	return err
}

func (s *CopyTradeStore) ListCopyGuardAttempts(cycleID int64) ([]*CopyGuardAttempt, error) {
	rows, err := s.db.Query(`SELECT id,cycle_id,attempt_no,status,entry_price,exit_price,quantity,notional,stop_trigger_price,stop_fill_price,stop_algo_id,
		COALESCE(protection_algo_id,''),COALESCE(protection_status,'PENDING'),COALESCE(protection_coverage,0),protection_updated_at,
		follower_pos_id,entry_order_id,exit_order_id,pnl,fee,funding_fee,liquidation_penalty,reconciled,atr,opened_at,closed_at
		FROM copy_guard_attempts WHERE cycle_id=? ORDER BY attempt_no,id`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardAttempt{}
	for rows.Next() {
		var a CopyGuardAttempt
		var opened string
		var protectionUpdated, closed sql.NullString
		if err := rows.Scan(&a.ID, &a.CycleID, &a.AttemptNo, &a.Status, &a.EntryPrice, &a.ExitPrice, &a.Quantity, &a.Notional, &a.StopTriggerPrice, &a.StopFillPrice, &a.StopAlgoID,
			&a.ProtectionAlgoID, &a.ProtectionStatus, &a.ProtectionCoverage, &protectionUpdated,
			&a.FollowerPosID, &a.EntryOrderID, &a.ExitOrderID, &a.PnL, &a.Fee, &a.FundingFee, &a.LiquidationPenalty, &a.Reconciled, &a.ATR, &opened, &closed); err != nil {
			return nil, err
		}
		if a.OpenedAt, err = parseDBTime(opened); err != nil {
			return nil, fmt.Errorf("copy guard attempt %d opened_at: %w", a.ID, err)
		}
		if a.ClosedAt, err = parseNullableDBTime(closed); err != nil {
			return nil, fmt.Errorf("copy guard attempt %d closed_at: %w", a.ID, err)
		}
		if a.ProtectionUpdatedAt, err = parseNullableDBTime(protectionUpdated); err != nil {
			return nil, fmt.Errorf("copy guard attempt %d protection_updated_at: %w", a.ID, err)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

type CopyGuardSummary struct {
	FollowerCount          int     `json:"follower_count"`
	CycleCount             int     `json:"cycle_count"`
	StopCount              int     `json:"stop_count"`
	ReentryCount           int     `json:"reentry_count"`
	ActualPnL              float64 `json:"actual_pnl"`
	BaselinePnL            float64 `json:"baseline_pnl"`
	AvoidedLoss            float64 `json:"avoided_loss"`
	OpportunityCost        float64 `json:"opportunity_cost"`
	NetGuardEffect         float64 `json:"net_guard_effect"`
	StopOnlyPnL            float64 `json:"stop_only_pnl"`
	ReentryContribution    float64 `json:"reentry_contribution"`
	Fees                   float64 `json:"fees"`
	FundingFee             float64 `json:"funding_fee"`
	LiquidationPenalty     float64 `json:"liquidation_penalty"`
	Slippage               float64 `json:"slippage"`
	ProtectedCount         int     `json:"protected_count"`
	PendingProtectionCount int     `json:"pending_protection_count"`
	UnknownCount           int     `json:"unknown_count"`
	DegradedCount          int     `json:"degraded_count"`
	// v5 可保护性状态机：活跃仓位中止损被强平价钳紧 / 无法保护且正在
	// 强制退出或等待交易所终态的数量。
	ClampedCount           int `json:"clamped_count"`
	UnprotectableCount     int `json:"unprotectable_count"`
	AccountingPendingCount int `json:"accounting_pending_count"`
	// AccountingDelayedCount: cycles whose execution-exchange settlement data
	// is late; the system keeps retrying automatically (formerly "needs review").
	AccountingDelayedCount       int     `json:"accounting_delayed_count"`
	AccountingUnrecoverableCount int     `json:"accounting_unrecoverable_count"`
	AccountingUnscorableCount    int     `json:"accounting_unscorable_count"`
	OwnershipAmbiguousCount      int     `json:"ownership_ambiguous_count"`
	LegacyUnverifiedCount        int     `json:"legacy_unverified_count"`
	AverageCoverage              float64 `json:"average_coverage"`
	IgnoredCount                 int     `json:"ignored_count"`
	ReentryFirst                 int     `json:"reentry_first"`
	ReentrySecond                int     `json:"reentry_second"`
	ReentryThirdPlus             int     `json:"reentry_third_plus"`
	MaxAvoidedLoss               float64 `json:"max_avoided_loss"`
	MaxOpportunityCost           float64 `json:"max_opportunity_cost"`
	ProtectionMissingSeconds     float64 `json:"protection_missing_seconds"`
	ReentrySuccessRate           float64 `json:"reentry_success_rate"`
	FalseKillRate                float64 `json:"false_kill_rate"`
	// v5 统计口径修正：比率必须带样本数展示（"误杀率 66.7%（3 样本）"单独
	// 展示会严重误导），估算基线的净效果与实测口径分层列示。
	ReentrySampleCount int `json:"reentry_sample_count"` // 重入成功率的分母（已结束的重入 attempt 数）
	StoppedCycleCount  int `json:"stopped_cycle_count"`  // 误杀率的分母（已对账且发生过止损的周期数）
	FalseKillCount     int `json:"false_kill_count"`     // 误杀次数（分子）
	// EstimatedBaselineCycles: 基线仍为"最后观测价估算"（baseline_source=
	// last_observed）的已对账周期数；其净效果单独累加在
	// EstimatedNetGuardEffect，headline NetGuardEffect 中已实测部分 =
	// NetGuardEffect − EstimatedNetGuardEffect。
	EstimatedBaselineCycles  int          `json:"estimated_baseline_cycles"`
	EstimatedNetGuardEffect  float64      `json:"estimated_net_guard_effect"`
	UnscorableBaselineCycles int          `json:"unscorable_baseline_cycles"`
	VerifiedBaselineCycles   int          `json:"verified_baseline_cycles"`
	ReentrySuccessEstimate   RateEstimate `json:"reentry_success_estimate"`
	FalseKillEstimate        RateEstimate `json:"false_kill_estimate"`
	MeanNetEffectCI          MeanEstimate `json:"mean_net_guard_effect_estimate"`
	// Drawdown/tail metrics only use reconciled cycles that actually stopped.
	// With small samples CVaR95 intentionally collapses to the worst observation.
	MaxRealizedDrawdownUSD float64               `json:"max_realized_drawdown_usd"`
	WorstCycleLossUSD      float64               `json:"worst_cycle_loss_usd"`
	TailLossCVaR95USD      float64               `json:"tail_loss_cvar_95_usd"`
	Trend                  []CopyGuardTrendPoint `json:"trend"`
}

type RateEstimate struct {
	Numerator   int     `json:"numerator"`
	Denominator int     `json:"denominator"`
	Rate        float64 `json:"rate"`
	CI95Low     float64 `json:"ci95_low"`
	CI95High    float64 `json:"ci95_high"`
	Method      string  `json:"method"`
	Status      string  `json:"status"`
}

type MeanEstimate struct {
	SampleCount int     `json:"sample_count"`
	Mean        float64 `json:"mean"`
	CI95Low     float64 `json:"ci95_low"`
	CI95High    float64 `json:"ci95_high"`
	Method      string  `json:"method"`
	Status      string  `json:"status"`
}

func wilsonRate(successes, total int) RateEstimate {
	out := RateEstimate{Numerator: successes, Denominator: total, Method: "WILSON_95", Status: "INSUFFICIENT_DATA"}
	if total <= 0 {
		return out
	}
	p, z := float64(successes)/float64(total), 1.959963984540054
	n := float64(total)
	denom := 1 + z*z/n
	center := (p + z*z/(2*n)) / denom
	margin := z * math.Sqrt((p*(1-p)+z*z/(4*n))/n) / denom
	out.Rate, out.CI95Low, out.CI95High, out.Status = p, math.Max(0, center-margin), math.Min(1, center+margin), "AVAILABLE"
	return out
}

func bootstrapMean(values []float64) MeanEstimate {
	out := MeanEstimate{SampleCount: len(values), Method: "BOOTSTRAP_95_FIXED_SEED", Status: "INSUFFICIENT_SAMPLE"}
	if len(values) == 0 {
		return out
	}
	for _, v := range values {
		out.Mean += v
	}
	out.Mean /= float64(len(values))
	if len(values) < 20 {
		return out
	}
	rng := rand.New(rand.NewSource(1))
	means := make([]float64, 5000)
	for i := range means {
		for j := 0; j < len(values); j++ {
			means[i] += values[rng.Intn(len(values))]
		}
		means[i] /= float64(len(values))
	}
	sort.Float64s(means)
	out.CI95Low, out.CI95High, out.Status = means[124], means[4874], "AVAILABLE"
	return out
}

func copyGuardTailRisk(pnls []float64) (maxDrawdown, worstLoss, cvar95 float64) {
	peak, cumulative := float64(0), float64(0)
	losses := make([]float64, 0, len(pnls))
	for _, pnl := range pnls {
		cumulative += pnl
		if cumulative > peak {
			peak = cumulative
		}
		if drawdown := peak - cumulative; drawdown > maxDrawdown {
			maxDrawdown = drawdown
		}
		if pnl < 0 {
			losses = append(losses, -pnl)
		}
	}
	if len(losses) == 0 {
		return maxDrawdown, 0, 0
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(losses)))
	worstLoss = losses[0]
	tailCount := int(math.Ceil(float64(len(pnls)) * 0.05))
	if tailCount < 1 {
		tailCount = 1
	}
	if tailCount > len(losses) {
		tailCount = len(losses)
	}
	for _, loss := range losses[:tailCount] {
		cvar95 += loss
	}
	cvar95 /= float64(tailCount)
	return maxDrawdown, worstLoss, cvar95
}

type CopyGuardTrendPoint struct {
	Date      string  `json:"date"`
	Actual    float64 `json:"actual"`
	Baseline  float64 `json:"baseline"`
	NetEffect float64 `json:"net_effect"`
}

type CopyGuardFilter struct {
	LeaderID   string
	Symbol     string
	Status     string
	ResultType string
}

// copyGuardInterventionPredicate keeps analytics aligned with lifecycle facts:
// either a verified protective stop fired, or protection establishment failed
// and the configured close policy caused a market exit. The latter is a real
// guard intervention but deliberately does not increment stop_count.
const copyGuardInterventionPredicate = `(stop_count>0 OR EXISTS (
	SELECT 1 FROM copy_guard_events guard_event
	WHERE guard_event.cycle_id=copy_guard_cycles.id AND guard_event.type='GUARD_FORCED_EXIT'
))`

func appendCopyGuardFilter(q string, args []interface{}, f CopyGuardFilter) (string, []interface{}) {
	if f.LeaderID != "" {
		q += " AND leader_id=?"
		args = append(args, f.LeaderID)
	}
	if f.Symbol != "" {
		q += " AND symbol=?"
		args = append(args, strings.ToUpper(f.Symbol))
	}
	if f.Status != "" {
		q += " AND status=?"
		args = append(args, f.Status)
	}
	switch f.ResultType {
	case "improved":
		q += " AND accounting_status='RECONCILED' AND " + copyGuardInterventionPredicate + " AND net_guard_effect>0"
	case "cost":
		q += " AND accounting_status='RECONCILED' AND " + copyGuardInterventionPredicate + " AND net_guard_effect<0"
	case "neutral":
		q += " AND accounting_status='RECONCILED' AND net_guard_effect=0"
	case "open":
		q += " AND closed_at IS NULL"
	}
	return q, args
}

func (s *CopyTradeStore) CopyGuardSummary(traderIDs []string, from, to time.Time, filter CopyGuardFilter) (*CopyGuardSummary, error) {
	if len(traderIDs) == 0 {
		return &CopyGuardSummary{}, nil
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+2)
	for _, id := range traderIDs {
		args = append(args, id)
	}
	args = append(args, from.UTC().Format("2006-01-02 15:04:05"), to.UTC().Format("2006-01-02 15:04:05"))
	q := `SELECT COUNT(DISTINCT trader_id),COUNT(*),COALESCE(SUM(stop_count),0),COALESCE(SUM(reentry_count),0),COALESCE(SUM(CASE WHEN accounting_status='RECONCILED' THEN actual_pnl ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='RECONCILED' AND COALESCE(baseline_source,'')<>'missing' THEN baseline_pnl ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='RECONCILED' AND COALESCE(baseline_source,'')<>'missing' AND ` + copyGuardInterventionPredicate + ` AND net_guard_effect>0 THEN net_guard_effect ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='RECONCILED' AND COALESCE(baseline_source,'')<>'missing' AND ` + copyGuardInterventionPredicate + ` AND net_guard_effect<0 THEN -net_guard_effect ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='RECONCILED' AND COALESCE(baseline_source,'')<>'missing' AND ` + copyGuardInterventionPredicate + ` THEN net_guard_effect ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='RECONCILED' THEN fees ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='RECONCILED' THEN funding_fee ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='RECONCILED' THEN liquidation_penalty ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='RECONCILED' THEN slippage ELSE 0 END),0),COALESCE(SUM(CASE WHEN closed_at IS NULL AND protection_status='VERIFIED' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN closed_at IS NULL AND protection_status='PENDING' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN closed_at IS NULL AND protection_status='UNKNOWN' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN closed_at IS NULL AND protection_status='DEGRADED' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN closed_at IS NULL AND protection_status='CLAMPED' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN closed_at IS NULL AND protection_status IN ('UNPROTECTABLE','UNPROTECTED_WARNING','FORCED_EXIT_PENDING') THEN 1 ELSE 0 END),0),COALESCE(AVG(CASE WHEN closed_at IS NULL THEN protection_coverage END),0),COALESCE(SUM(CASE WHEN accounting_status='PENDING' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='DELAYED' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='UNRECOVERABLE' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='` + CopyGuardAccountingUnscorable + `' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN closed_at IS NULL AND accounting_status='` + CopyGuardAccountingUnscorable + `' AND accounting_error LIKE 'OWNERSHIP_AMBIGUOUS:%' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN accounting_status='LEGACY_UNVERIFIED' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN reentry_count>=1 THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN reentry_count>=2 THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN reentry_count>=3 THEN 1 ELSE 0 END),0),COALESCE(MAX(CASE WHEN accounting_status='RECONCILED' AND COALESCE(baseline_source,'')<>'missing' AND ` + copyGuardInterventionPredicate + ` AND net_guard_effect>0 THEN net_guard_effect ELSE 0 END),0),COALESCE(MAX(CASE WHEN accounting_status='RECONCILED' AND COALESCE(baseline_source,'')<>'missing' AND ` + copyGuardInterventionPredicate + ` AND net_guard_effect<0 THEN -net_guard_effect ELSE 0 END),0),COALESCE(SUM(protection_missing_seconds+CASE WHEN protection_missing_at IS NOT NULL THEN MAX(0,(julianday(COALESCE(closed_at,CURRENT_TIMESTAMP))-julianday(protection_missing_at))*86400) ELSE 0 END),0) FROM copy_guard_cycles WHERE trader_id IN (` + marks + `) AND opened_at>=? AND opened_at<?`
	q, args = appendCopyGuardFilter(q, args, filter)
	var x CopyGuardSummary
	err := s.db.QueryRow(q, args...).Scan(&x.FollowerCount, &x.CycleCount, &x.StopCount, &x.ReentryCount, &x.ActualPnL, &x.BaselinePnL, &x.AvoidedLoss, &x.OpportunityCost, &x.NetGuardEffect, &x.Fees, &x.FundingFee, &x.LiquidationPenalty, &x.Slippage, &x.ProtectedCount, &x.PendingProtectionCount, &x.UnknownCount, &x.DegradedCount, &x.ClampedCount, &x.UnprotectableCount, &x.AverageCoverage, &x.AccountingPendingCount, &x.AccountingDelayedCount, &x.AccountingUnrecoverableCount, &x.AccountingUnscorableCount, &x.OwnershipAmbiguousCount, &x.LegacyUnverifiedCount, &x.ReentryFirst, &x.ReentrySecond, &x.ReentryThirdPlus, &x.MaxAvoidedLoss, &x.MaxOpportunityCost, &x.ProtectionMissingSeconds)
	if err != nil {
		return &x, err
	}
	ignoredArgs := make([]interface{}, len(traderIDs))
	for i, id := range traderIDs {
		ignoredArgs[i] = id
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_trade_position_mappings WHERE trader_id IN (`+marks+`) AND status='ignored'`, ignoredArgs...).Scan(&x.IgnoredCount); err != nil {
		return &x, err
	}
	rateArgs := append(append([]interface{}{}, ignoredArgs...), from.UTC().Format("2006-01-02 15:04:05"), to.UTC().Format("2006-01-02 15:04:05"))
	filteredCycleQuery := `SELECT id FROM copy_guard_cycles WHERE trader_id IN (` + marks + `) AND opened_at>=? AND opened_at<?`
	filteredCycleQuery, rateArgs = appendCopyGuardFilter(filteredCycleQuery, rateArgs, filter)
	var endedReentries, winningReentries, stoppedCycles, falseKills int
	if err := s.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN pnl>0 THEN 1 ELSE 0 END),0) FROM copy_guard_attempts WHERE cycle_id IN (`+filteredCycleQuery+`) AND attempt_no>0 AND closed_at IS NOT NULL`, rateArgs...).Scan(&endedReentries, &winningReentries); err != nil {
		return &x, err
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN stop_count>0 AND accounting_status='RECONCILED' AND COALESCE(baseline_source,'')<>'missing' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN stop_count>0 AND accounting_status='RECONCILED' AND COALESCE(baseline_source,'')<>'missing' AND baseline_pnl>0 AND net_guard_effect<0 THEN 1 ELSE 0 END),0) FROM copy_guard_cycles WHERE id IN (`+filteredCycleQuery+`)`, rateArgs...).Scan(&stoppedCycles, &falseKills); err != nil {
		return &x, err
	}
	x.ReentrySampleCount, x.StoppedCycleCount, x.FalseKillCount = endedReentries, stoppedCycles, falseKills
	if endedReentries > 0 {
		x.ReentrySuccessRate = float64(winningReentries) / float64(endedReentries)
	}
	if stoppedCycles > 0 {
		x.FalseKillRate = float64(falseKills) / float64(stoppedCycles)
	}
	x.ReentrySuccessEstimate = wilsonRate(winningReentries, endedReentries)
	x.FalseKillEstimate = wilsonRate(falseKills, stoppedCycles)
	if err := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN a.attempt_no=0 THEN a.pnl ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN a.attempt_no>0 THEN a.pnl ELSE 0 END),0)
		FROM copy_guard_attempts a JOIN copy_guard_cycles c ON c.id=a.cycle_id
		WHERE c.id IN (`+filteredCycleQuery+`) AND c.accounting_status='RECONCILED'`, rateArgs...).Scan(&x.StopOnlyPnL, &x.ReentryContribution); err != nil {
		return &x, err
	}
	// v5 统计分层：估算基线（最后观测价，领航员真实离场价未获得）的周期
	// 单列，避免估算值混入 headline 后误导（实测口径 = 总值 − 估算部分）。
	// 计数与求和口径一致：只统计发生过真实 Guard 干预的周期（保护止损
	// 或因无法建立保护而按配置退出），避免把纯跟随周期混入效果归因。
	if err := s.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(net_guard_effect),0) FROM copy_guard_cycles WHERE id IN (`+filteredCycleQuery+`) AND accounting_status='RECONCILED' AND baseline_source='last_observed' AND `+copyGuardInterventionPredicate, rateArgs...).Scan(&x.EstimatedBaselineCycles, &x.EstimatedNetGuardEffect); err != nil {
		return &x, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_cycles WHERE id IN (`+filteredCycleQuery+`) AND accounting_status='RECONCILED' AND baseline_source='missing' AND `+copyGuardInterventionPredicate, rateArgs...).Scan(&x.UnscorableBaselineCycles); err != nil {
		return &x, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_guard_cycles WHERE id IN (`+filteredCycleQuery+`) AND accounting_status='RECONCILED' AND COALESCE(baseline_source,'') NOT IN ('missing','last_observed') AND `+copyGuardInterventionPredicate, rateArgs...).Scan(&x.VerifiedBaselineCycles); err != nil {
		return &x, err
	}
	verifiedRows, verifiedErr := s.db.Query(`SELECT net_guard_effect FROM copy_guard_cycles WHERE id IN (`+filteredCycleQuery+`) AND accounting_status='RECONCILED' AND COALESCE(baseline_source,'') NOT IN ('missing','last_observed') AND `+copyGuardInterventionPredicate+` ORDER BY id`, rateArgs...)
	if verifiedErr != nil {
		return &x, verifiedErr
	}
	var verifiedEffects []float64
	for verifiedRows.Next() {
		var effect float64
		if scanErr := verifiedRows.Scan(&effect); scanErr != nil {
			_ = verifiedRows.Close()
			return &x, scanErr
		}
		verifiedEffects = append(verifiedEffects, effect)
	}
	if rowsErr := verifiedRows.Err(); rowsErr != nil {
		_ = verifiedRows.Close()
		return &x, rowsErr
	}
	_ = verifiedRows.Close()
	x.MeanNetEffectCI = bootstrapMean(verifiedEffects)
	rows, queryErr := s.db.Query(`SELECT actual_pnl FROM copy_guard_cycles WHERE id IN (`+filteredCycleQuery+`) AND accounting_status='RECONCILED' AND `+copyGuardInterventionPredicate+` ORDER BY COALESCE(closed_at,opened_at),id`, rateArgs...)
	if queryErr != nil {
		return &x, queryErr
	}
	var pnls []float64
	for rows.Next() {
		var pnl float64
		if scanErr := rows.Scan(&pnl); scanErr != nil {
			_ = rows.Close()
			return &x, scanErr
		}
		pnls = append(pnls, pnl)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		_ = rows.Close()
		return &x, rowsErr
	}
	_ = rows.Close()
	x.MaxRealizedDrawdownUSD, x.WorstCycleLossUSD, x.TailLossCVaR95USD = copyGuardTailRisk(pnls)
	trendArgs := make([]interface{}, 0, len(traderIDs)+2)
	for _, id := range traderIDs {
		trendArgs = append(trendArgs, id)
	}
	trendArgs = append(trendArgs, from.UTC().Format("2006-01-02 15:04:05"), to.UTC().Format("2006-01-02 15:04:05"))
	trendQuery := `SELECT date(opened_at),COALESCE(SUM(actual_pnl),0),COALESCE(SUM(CASE WHEN COALESCE(baseline_source,'')<>'missing' THEN baseline_pnl ELSE 0 END),0),COALESCE(SUM(CASE WHEN ` + copyGuardInterventionPredicate + ` AND COALESCE(baseline_source,'')<>'missing' THEN net_guard_effect ELSE 0 END),0) FROM copy_guard_cycles WHERE trader_id IN (` + marks + `) AND opened_at>=? AND opened_at<? AND accounting_status='RECONCILED'`
	trendQuery, trendArgs = appendCopyGuardFilter(trendQuery, trendArgs, filter)
	trendQuery += ` GROUP BY date(opened_at) ORDER BY date(opened_at)`
	rows, queryErr = s.db.Query(trendQuery, trendArgs...)
	if queryErr != nil {
		return &x, queryErr
	}
	defer rows.Close()
	for rows.Next() {
		var point CopyGuardTrendPoint
		if scanErr := rows.Scan(&point.Date, &point.Actual, &point.Baseline, &point.NetEffect); scanErr != nil {
			return &x, scanErr
		}
		x.Trend = append(x.Trend, point)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return &x, rowsErr
	}
	return &x, nil
}

func (s *CopyTradeStore) ListCopyGuardCycles(traderIDs []string, from, to time.Time, limit, offset int, filter CopyGuardFilter) ([]*CopyGuardCycle, error) {
	if len(traderIDs) == 0 {
		return []*CopyGuardCycle{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+4)
	for _, id := range traderIDs {
		args = append(args, id)
	}
	args = append(args, from.UTC().Format("2006-01-02 15:04:05"), to.UTC().Format("2006-01-02 15:04:05"))
	q := copyGuardCycleSelect + ` WHERE trader_id IN (` + marks + `) AND opened_at>=? AND opened_at<?`
	q, args = appendCopyGuardFilter(q, args, filter)
	q += " ORDER BY opened_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardCycle{}
	for rows.Next() {
		c, err := scanCopyGuardCycle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *CopyTradeStore) ListCopyGuardEvents(cycleID int64) ([]*CopyGuardEvent, error) {
	rows, err := s.db.Query(`SELECT id,cycle_id,trader_id,type,price,quantity,notional,pnl,fee,metadata_json,created_at FROM copy_guard_events WHERE cycle_id=? ORDER BY created_at,id`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardEvent{}
	for rows.Next() {
		var e CopyGuardEvent
		var raw, created string
		if err := rows.Scan(&e.ID, &e.CycleID, &e.TraderID, &e.Type, &e.Price, &e.Quantity, &e.Notional, &e.PnL, &e.Fee, &raw, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &e.Metadata)
		if e.CreatedAt, err = parseDBTime(created); err != nil {
			return nil, fmt.Errorf("copy guard event %d created_at: %w", e.ID, err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
