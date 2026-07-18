package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// CopyTradeStore 跟单配置存储
type CopyTradeStore struct {
	db *sql.DB
}

// CopyTradeConfig 跟单配置（存储在数据库中）
type CopyTradeConfig struct {
	TraderID       string  `json:"trader_id"`
	ProviderType   string  `json:"provider_type"`    // "hyperliquid" | "okx" | "binance"
	LeaderID       string  `json:"leader_id"`        // 领航员地址/uniqueName，Binance 模式下复用为 portfolioId
	CopyRatio      float64 `json:"copy_ratio"`       // 跟单系数 (1.0 = 100%)
	SyncLeverage   bool    `json:"sync_leverage"`    // 同步杠杆
	SyncMarginMode bool    `json:"sync_margin_mode"` // 同步保证金模式
	MinTradeWarn   float64 `json:"min_trade_warn"`   // 小额预警阈值
	MaxTradeWarn   float64 `json:"max_trade_warn"`   // 大额预警阈值 (0=不预警)
	Enabled        bool    `json:"enabled"`          // 是否启用

	// Binance Web 私有接口凭证（仅 ProviderType=binance 时使用，明文存储）
	BinanceP20T        string `json:"binance_p20t,omitempty"`       // 登录 cookie p20t
	BinanceCSRFToken   string `json:"binance_csrf_token,omitempty"` // CSRF header csrftoken
	BinanceSourceMode  string `json:"binance_source_mode"`          // copy_management | smart_money
	BinanceTopTraderID string `json:"binance_top_trader_id,omitempty"`
	SourceGeneration   int    `json:"source_generation"`

	// ============================================================
	// Copy Guard v7：波动/结构止损、完整保护状态机与 AI guarded 重入。
	// OKX/Binance 可作为领航员数据源；保护能力由跟随执行交易所复核。
	// v3 旧策略与噪音下限/周期熔断/反加仓铁律/价格容差等参数已于 v5 下线：
	// 数据库旧列（risk_atr_enabled / risk_reentry_tolerance /
	// risk_reentry_block_addback / risk_reentry_addback_tolerance）保留但
	// 不再读写；存量 v3 配置在读取时强制升级 v4（见 upgradeLegacyRiskPolicy）
	// ============================================================

	// 主开关
	RiskStopLossEnabled bool `json:"risk_stop_loss_enabled"` // 默认 true

	// 账户硬兜底（v5：单笔最多亏账户的百分比，任何模式下的最终封顶）
	RiskAccountPct float64 `json:"risk_account_pct"` // v7 单次尝试风险预算，默认 0.02 (=2%)

	// ATR 基线（v5.2 抗噪主力线：止损距离 = k×ATR，与账户线取严）
	RiskATRMultiplier float64 `json:"risk_atr_multiplier"` // 默认 2.0，范围 0.5-5
	RiskATRTimeframe  string  `json:"risk_atr_timeframe"`  // 默认 "1h"，可选 "15m"/"1h"/"4h"

	// 仓位保证金止损（可选的更严封顶；高杠杆下会把止损压进噪音区，默认关）
	RiskLeverageFallback bool    `json:"risk_leverage_fallback"` // 默认 false
	RiskLeverageMaxLoss  float64 `json:"risk_leverage_max_loss"` // 默认 0.2 (=20% 保证金，仅开启时生效)

	// 二次进场：新配置默认 ai_guarded，旧配置保留 legacy_rule。
	RiskReentryEnabled bool    `json:"risk_reentry_enabled"`
	RiskReentryRatio   float64 `json:"risk_reentry_ratio"` // × 被止损仓位名义，默认 0.5
	// RiskReentryDecisionMode 决定止损后的重入决策来源。
	// legacy_rule 仅为存量兼容；ai_guarded 由 AI 判断方向、代码执行风控；disabled 禁用重入。
	RiskReentryDecisionMode string `json:"risk_reentry_decision_mode"`

	// vNext 风险预算。RiskAccountPct 保留原 JSON/DB 名称以兼容旧客户端，
	// 在新策略中语义明确为“单次尝试最大账户风险”。
	RiskCycleLossBudgetPct     float64 `json:"risk_cycle_loss_budget_pct"`
	RiskPortfolioLossBudgetPct float64 `json:"risk_portfolio_loss_budget_pct"`
	RiskRoundTripFeeBPS        float64 `json:"risk_round_trip_fee_bps"`

	// AI 持续观察调度预算（均持久化在 policy JSON 中）。
	RiskAIConfidenceThreshold float64 `json:"risk_ai_confidence_threshold"`
	RiskAIMinReviewSeconds    int     `json:"risk_ai_min_review_seconds"`
	RiskAIDailyCallLimit      int     `json:"risk_ai_daily_call_limit"`
	RiskAILifecycleCallLimit  int     `json:"risk_ai_lifecycle_call_limit"`
	RiskNotificationLevel     string  `json:"risk_notification_level"`

	// 历史人工重入字段仅为数据库/回滚兼容；v7 始终关闭且不再执行。
	RiskManualReentryEnabled bool `json:"risk_manual_reentry_enabled"`

	// Copy Guard v4+。独立存储于 copy_guard_policies，避免破坏 v3 配置表和旧版回滚。
	RiskPolicyVersion int `json:"risk_policy_version"`
	// RiskStopMode 兼容字段：v4 曾区分止损模式，现 ComputeRiskDistanceV4 统一
	// 双通道取更远口径，该值不再影响计算（仅 ValidateRiskPolicyV4 做枚举校验、
	// 序列化保持向后兼容）。固定写 "volatility_priority"。
	RiskStopMode               string  `json:"risk_stop_mode"`
	RiskATRPeriod              int     `json:"risk_atr_period"`
	RiskATRCacheMaxAgeMinutes  int     `json:"risk_atr_cache_max_age_minutes"`
	RiskATRFallbackPct         float64 `json:"risk_atr_fallback_pct"`
	RiskTriggerPriceType       string  `json:"risk_trigger_price_type"`
	RiskSlippageBufferBPS      float64 `json:"risk_slippage_buffer_bps"`
	RiskLiquidationBufferATR   float64 `json:"risk_liquidation_buffer_atr"`
	RiskMaxReentries           int     `json:"risk_max_reentries"`
	RiskReentryBandATR         float64 `json:"risk_reentry_band_atr"`
	RiskReentryCooldownSeconds int     `json:"risk_reentry_cooldown_seconds"`
	RiskReentryMaxChaseATR     float64 `json:"risk_reentry_max_chase_atr"`
	RiskReentryMaxATRExpansion float64 `json:"risk_reentry_max_atr_expansion"`
	RiskWatchTimeoutMinutes    int     `json:"risk_watch_timeout_minutes"`
	// RiskMigrationConfirmed 升级书签：v3→v4 升级完成时置 true，业务运行时
	// 不读取（无任何门控），仅供审计/回滚排查确认该配置已完成迁移。
	RiskMigrationConfirmed bool `json:"risk_migration_confirmed"`
	// RiskAddonBudgetPct: 加仓账户风险预算（v4）。跟随领航员加仓时预估
	// "加仓后总敞口按当前止损距离全损"占账户权益的比例；v7 超限自动缩量，
	// 低于最小下单量则拒绝。默认 0.15 (=15%)。
	RiskAddonBudgetPct float64 `json:"risk_addon_budget_pct"`
	// RiskReentryMinRecoveryATR: 重入最小恢复幅度（v4，单位 ATR 倍数）。
	// 止损后价格必须从止损成交价向有利方向恢复至少该幅度才允许重入，
	// 防止"刚止损又原地接回"。默认 0.5。
	RiskReentryMinRecoveryATR float64 `json:"risk_reentry_min_recovery_atr"`
	// RiskReentryCooldownEscalation: 第 N 次重入冷却时间倍率（v4）。
	// 实际冷却 = cooldown_seconds × escalation^已重入次数。默认 3。
	RiskReentryCooldownEscalation float64 `json:"risk_reentry_cooldown_escalation"`
	// RiskReentryRecoveryEscalation: 第 N 次重入最小恢复幅度倍率（v4）。
	// 实际要求 = min_recovery_atr × escalation^已重入次数。默认 1.5。
	RiskReentryRecoveryEscalation float64 `json:"risk_reentry_recovery_escalation"`
	// RiskUnprotectableAction: 保护单不可建立（clamp 到强平缓冲价也不可行）
	// 时的处置模式（v5）。
	//   "close"（默认）：保护优先——立即平掉跟单仓位，周期进入观察期
	//   "follow"：跟单优先——继续裸跑，UI 标红 + 升级告警（用户显式选择）
	RiskUnprotectableAction string `json:"risk_unprotectable_action"`
	// RiskReentryNoiseOverride: 止损距离/ATR < 0.3（极易扫损档）时默认禁用
	// 自动重入；置 true 可强制放行（按谨慎档执行）。
	RiskReentryNoiseOverride bool `json:"risk_reentry_noise_override"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FillRiskDefaults 为零值字段填充默认值
// 调用时机：从数据库读出后立即调用，或前端未传字段时
// 设计目的：保证旧库（ALTER TABLE 后字段为 0/false/""）也能跑出合理默认行为
//
// v5.2 默认值选型（与 copytrade.CopyConfig.FillRiskDefaults 保持一致）：
//   - RiskATRMultiplier 2.0：止损距离基线（k×ATR），抗噪主力线
//   - RiskAccountPct 0.02：v7 单次尝试风险预算（含费用与滑点）
//   - RiskLeverageMaxLoss 0.20：仅当 RiskLeverageFallback 开启时的保证金封顶（默认关）
func (c *CopyTradeConfig) FillRiskDefaults() {
	if c.BinanceSourceMode == "" {
		c.BinanceSourceMode = "copy_management"
	}
	if c.SourceGeneration <= 0 {
		c.SourceGeneration = 1
	}
	if c.RiskAccountPct == 0 {
		c.RiskAccountPct = 0.02
	}
	if c.RiskATRMultiplier == 0 {
		// v5.2 抗噪：止损距离基线默认 2.0×ATR（与 copytrade.CopyConfig 保持一致）
		c.RiskATRMultiplier = 2.0
	}
	if c.RiskATRTimeframe == "" {
		c.RiskATRTimeframe = "1h"
	}
	if c.RiskLeverageMaxLoss == 0 {
		c.RiskLeverageMaxLoss = 0.2
	}
	if c.RiskReentryRatio == 0 {
		c.RiskReentryRatio = 0.5
	}
	if c.RiskReentryDecisionMode == "" {
		// 存量策略没有该字段时保持旧规则；新建配置由 API 的推荐默认值
		// 显式写 ai_guarded，避免升级时静默改变真实交易行为。
		c.RiskReentryDecisionMode = "legacy_rule"
	}
	if c.RiskCycleLossBudgetPct == 0 {
		c.RiskCycleLossBudgetPct = 0.05
		if c.RiskAccountPct > c.RiskCycleLossBudgetPct {
			// 存量配置可能显式使用高于新推荐值的单次预算；补默认值时
			// 只能向上兼容，不能生成 attempt > cycle 的非法组合。
			c.RiskCycleLossBudgetPct = c.RiskAccountPct
		}
	}
	if c.RiskPortfolioLossBudgetPct == 0 {
		c.RiskPortfolioLossBudgetPct = 0.08
		if c.RiskCycleLossBudgetPct > c.RiskPortfolioLossBudgetPct {
			c.RiskPortfolioLossBudgetPct = c.RiskCycleLossBudgetPct
		}
	}
	if c.RiskRoundTripFeeBPS == 0 {
		c.RiskRoundTripFeeBPS = 12
	}
	if c.RiskAIConfidenceThreshold == 0 {
		c.RiskAIConfidenceThreshold = 0.80
	}
	if c.RiskAIMinReviewSeconds == 0 {
		c.RiskAIMinReviewSeconds = 300
	}
	if c.RiskAIDailyCallLimit == 0 {
		c.RiskAIDailyCallLimit = 12
	}
	if c.RiskAILifecycleCallLimit == 0 {
		c.RiskAILifecycleCallLimit = 30
	}
	if c.RiskNotificationLevel == "" {
		c.RiskNotificationLevel = "important"
	}
	if c.RiskPolicyVersion >= 4 {
		if c.RiskStopMode == "" {
			c.RiskStopMode = "volatility_priority"
		}
		if c.RiskATRPeriod == 0 {
			c.RiskATRPeriod = 14
		}
		if c.RiskATRCacheMaxAgeMinutes == 0 {
			c.RiskATRCacheMaxAgeMinutes = 120
		}
		if c.RiskATRFallbackPct == 0 {
			c.RiskATRFallbackPct = 0.02
		}
		if c.RiskTriggerPriceType == "" {
			c.RiskTriggerPriceType = "mark"
		}
		if c.RiskReentryMaxATRExpansion == 0 {
			c.RiskReentryMaxATRExpansion = 2
		}
		if c.RiskAddonBudgetPct == 0 {
			c.RiskAddonBudgetPct = 0.15
		}
		if c.RiskReentryMinRecoveryATR == 0 {
			c.RiskReentryMinRecoveryATR = 0.5
		}
		if c.RiskReentryCooldownEscalation == 0 {
			c.RiskReentryCooldownEscalation = 3
		}
		if c.RiskReentryRecoveryEscalation == 0 {
			c.RiskReentryRecoveryEscalation = 1.5
		}
		if c.RiskUnprotectableAction == "" {
			c.RiskUnprotectableAction = "close"
		}
	}
}

// NewCopyGuardDefaults 是所有后端创建入口与 defaults API 的唯一默认值来源。
// 存量配置读取仍走 FillRiskDefaults 的兼容语义，不会被静默切换到 AI。
func NewCopyGuardDefaults() *CopyTradeConfig {
	c := &CopyTradeConfig{
		// 最小跟单金额默认 12 USDT（预留交易所 10 USDT 最小名义 + 精度损失
		// 余量），与引擎侧 fallback（copytrade.DefaultMinTradeNotional）统一。
		MinTradeWarn:               12,
		RiskStopLossEnabled:        true,
		RiskAccountPct:             0.02,
		RiskATRMultiplier:          2.0,
		RiskATRTimeframe:           "1h",
		RiskReentryEnabled:         true,
		RiskReentryRatio:           0.50,
		RiskReentryDecisionMode:    "ai_guarded",
		RiskPolicyVersion:          4,
		RiskStopMode:               "volatility_priority",
		RiskATRPeriod:              14,
		RiskATRCacheMaxAgeMinutes:  120,
		RiskATRFallbackPct:         0.02,
		RiskTriggerPriceType:       "mark",
		RiskSlippageBufferBPS:      10,
		RiskLiquidationBufferATR:   0.5,
		RiskMaxReentries:           2,
		RiskReentryBandATR:         0.5,
		RiskReentryCooldownSeconds: 300,
		RiskReentryMaxChaseATR:     0.5,
		RiskReentryMaxATRExpansion: 2,
		RiskWatchTimeoutMinutes:    4320,
		RiskMigrationConfirmed:     true,
		RiskAddonBudgetPct:         0.15,
		RiskReentryMinRecoveryATR:  0.5,
		RiskUnprotectableAction:    "close",
		RiskCycleLossBudgetPct:     0.05,
		RiskPortfolioLossBudgetPct: 0.08,
		RiskRoundTripFeeBPS:        12,
		RiskAIConfidenceThreshold:  0.80,
		RiskAIMinReviewSeconds:     300,
		RiskAIDailyCallLimit:       12,
		RiskAILifecycleCallLimit:   30,
		RiskNotificationLevel:      "important",
		RiskManualReentryEnabled:   false,
	}
	c.FillRiskDefaults()
	return c
}

// upgradeLegacyRiskPolicy v5：v3 旧止损策略已下线。存量 Copy Guard 数据源
// （OKX / Binance，与 copytrade.SupportsCopyGuard 对齐；store 不能反向依赖
// copytrade 包，故此处内联集合）+ 启用止损但仍是 v3（version<4）的配置在
// 读取时强制升级为 v4（Copy Guard 启用标记），并把 v3 时代的激进默认值
// （账户线 50%、保证金 50%）归零，交由 FillRiskDefaults 填充 v7 默认
// （RiskAccountPct=0.02 / RiskLeverageMaxLoss=0.20）；用户显式改过的其他值保留。
// 只改内存值，下次保存时随 saveCopyGuardPolicy 持久化。
func upgradeLegacyRiskPolicy(c *CopyTradeConfig) {
	if c == nil || !RiskPolicyUpgradeSupported(c.ProviderType) || !c.RiskStopLossEnabled || c.RiskPolicyVersion >= 4 {
		return
	}
	c.RiskPolicyVersion = 4
	c.RiskMigrationConfirmed = true
	if c.RiskAccountPct == 0.5 {
		c.RiskAccountPct = 0
	}
	if c.RiskLeverageMaxLoss == 0.5 {
		c.RiskLeverageMaxLoss = 0
	}
}

// RiskPolicyUpgradeSupported 与 copytrade.SupportsCopyGuard 保持同一集合
// （OKX / Binance）。store 是底层包不能 import copytrade，由
// copytrade 侧的一致性测试锁定两者不漂移。
func RiskPolicyUpgradeSupported(providerType string) bool {
	return providerType == "okx" || providerType == "binance"
}

func (s *CopyTradeStore) initTables() error {
	// 创建跟单配置表
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_configs (
			trader_id TEXT PRIMARY KEY,
			provider_type TEXT NOT NULL,
			leader_id TEXT NOT NULL,
			copy_ratio REAL DEFAULT 1.0,
			sync_leverage BOOLEAN DEFAULT 1,
			sync_margin_mode BOOLEAN DEFAULT 1,
			min_trade_warn REAL DEFAULT 10,
			max_trade_warn REAL DEFAULT 0,
			enabled BOOLEAN DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (trader_id) REFERENCES traders(id) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return err
	}

	// 创建触发器
	_, err = s.db.Exec(`
		CREATE TRIGGER IF NOT EXISTS update_copy_trade_configs_updated_at
		AFTER UPDATE ON copy_trade_configs
		BEGIN
			UPDATE copy_trade_configs SET updated_at = CURRENT_TIMESTAMP WHERE trader_id = NEW.trader_id;
		END
	`)
	if err != nil {
		return err
	}

	// 给 traders 表添加 decision_mode 字段
	s.db.Exec(`ALTER TABLE traders ADD COLUMN decision_mode TEXT DEFAULT 'ai'`)

	// 给 copy_trade_configs 表添加 Binance 凭证字段（兼容旧库）
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN binance_p20t TEXT DEFAULT ''`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN binance_csrf_token TEXT DEFAULT ''`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN binance_source_mode TEXT DEFAULT 'copy_management'`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN binance_top_trader_id TEXT DEFAULT ''`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN source_generation INTEGER DEFAULT 1`)

	// 给 copy_trade_configs 表添加风控字段（账户保护 / 止损兜底）
	// 旧库 ALTER 失败说明已存在，忽略；新库随表创建即有这些列
	// v5 注：v3 时代的 risk_atr_enabled / risk_reentry_tolerance /
	// risk_reentry_block_addback / risk_reentry_addback_tolerance 列不再
	// 创建也不再读写（旧库中的存量列保留为休眠数据）
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN risk_stop_loss_enabled INTEGER DEFAULT 1`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN risk_account_pct REAL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN risk_atr_multiplier REAL DEFAULT 2.0`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN risk_atr_timeframe TEXT DEFAULT '1h'`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN risk_leverage_fallback INTEGER DEFAULT 0`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN risk_leverage_max_loss REAL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN risk_reentry_enabled INTEGER DEFAULT 0`)
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN risk_reentry_ratio REAL DEFAULT 0.5`)
	// v5.1 人工重入：DEFAULT 1 = 新旧配置默认开启（ALTER 对存量行生效 DEFAULT）
	s.db.Exec(`ALTER TABLE copy_trade_configs ADD COLUMN risk_manual_reentry_enabled INTEGER DEFAULT 1`)

	return nil
}

// Create 创建跟单配置
func (s *CopyTradeStore) Create(config *CopyTradeConfig) error {
	config.FillRiskDefaults()
	_, err := s.db.Exec(`
		INSERT INTO copy_trade_configs 
			(trader_id, provider_type, leader_id, copy_ratio, sync_leverage, sync_margin_mode, 
			 min_trade_warn, max_trade_warn, enabled, binance_p20t, binance_csrf_token,
			 binance_source_mode, binance_top_trader_id, source_generation,
			 risk_stop_loss_enabled, risk_account_pct, risk_atr_multiplier,
			 risk_atr_timeframe, risk_leverage_fallback, risk_leverage_max_loss,
			 risk_reentry_enabled, risk_reentry_ratio, risk_manual_reentry_enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, config.TraderID, config.ProviderType, config.LeaderID, config.CopyRatio,
		config.SyncLeverage, config.SyncMarginMode, config.MinTradeWarn, config.MaxTradeWarn, config.Enabled,
		config.BinanceP20T, config.BinanceCSRFToken, config.BinanceSourceMode,
		config.BinanceTopTraderID, config.SourceGeneration,
		config.RiskStopLossEnabled, config.RiskAccountPct, config.RiskATRMultiplier,
		config.RiskATRTimeframe, config.RiskLeverageFallback, config.RiskLeverageMaxLoss,
		config.RiskReentryEnabled, config.RiskReentryRatio, config.RiskManualReentryEnabled)
	if err != nil {
		return err
	}
	return s.saveCopyGuardPolicy(config)
}

// Update 更新跟单配置
func (s *CopyTradeStore) Update(config *CopyTradeConfig) error {
	config.FillRiskDefaults()
	_, err := s.db.Exec(`
		UPDATE copy_trade_configs SET
			provider_type = ?,
			leader_id = ?,
			copy_ratio = ?,
			sync_leverage = ?,
			sync_margin_mode = ?,
			min_trade_warn = ?,
			max_trade_warn = ?,
			enabled = ?,
			binance_p20t = ?,
			binance_csrf_token = ?,
			binance_source_mode = ?,
			binance_top_trader_id = ?,
			source_generation = ?,
			risk_stop_loss_enabled = ?,
			risk_account_pct = ?,
			risk_atr_multiplier = ?,
			risk_atr_timeframe = ?,
			risk_leverage_fallback = ?,
			risk_leverage_max_loss = ?,
			risk_reentry_enabled = ?,
			risk_reentry_ratio = ?,
			risk_manual_reentry_enabled = ?
		WHERE trader_id = ?
	`, config.ProviderType, config.LeaderID, config.CopyRatio,
		config.SyncLeverage, config.SyncMarginMode, config.MinTradeWarn, config.MaxTradeWarn,
		config.Enabled, config.BinanceP20T, config.BinanceCSRFToken,
		config.BinanceSourceMode, config.BinanceTopTraderID, config.SourceGeneration,
		config.RiskStopLossEnabled, config.RiskAccountPct, config.RiskATRMultiplier,
		config.RiskATRTimeframe, config.RiskLeverageFallback, config.RiskLeverageMaxLoss,
		config.RiskReentryEnabled, config.RiskReentryRatio, config.RiskManualReentryEnabled,
		config.TraderID)
	if err != nil {
		return err
	}
	return s.saveCopyGuardPolicy(config)
}

// Upsert 创建或更新跟单配置
func (s *CopyTradeStore) Upsert(config *CopyTradeConfig) error {
	config.FillRiskDefaults()
	_, err := s.db.Exec(`
		INSERT INTO copy_trade_configs 
			(trader_id, provider_type, leader_id, copy_ratio, sync_leverage, sync_margin_mode, 
			 min_trade_warn, max_trade_warn, enabled, binance_p20t, binance_csrf_token,
			 binance_source_mode, binance_top_trader_id, source_generation,
			 risk_stop_loss_enabled, risk_account_pct, risk_atr_multiplier,
			 risk_atr_timeframe, risk_leverage_fallback, risk_leverage_max_loss,
			 risk_reentry_enabled, risk_reentry_ratio, risk_manual_reentry_enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trader_id) DO UPDATE SET
			provider_type = excluded.provider_type,
			leader_id = excluded.leader_id,
			copy_ratio = excluded.copy_ratio,
			sync_leverage = excluded.sync_leverage,
			sync_margin_mode = excluded.sync_margin_mode,
			min_trade_warn = excluded.min_trade_warn,
			max_trade_warn = excluded.max_trade_warn,
			enabled = excluded.enabled,
			binance_p20t = excluded.binance_p20t,
			binance_csrf_token = excluded.binance_csrf_token,
			binance_source_mode = excluded.binance_source_mode,
			binance_top_trader_id = excluded.binance_top_trader_id,
			source_generation = excluded.source_generation,
			risk_stop_loss_enabled = excluded.risk_stop_loss_enabled,
			risk_account_pct = excluded.risk_account_pct,
			risk_atr_multiplier = excluded.risk_atr_multiplier,
			risk_atr_timeframe = excluded.risk_atr_timeframe,
			risk_leverage_fallback = excluded.risk_leverage_fallback,
			risk_leverage_max_loss = excluded.risk_leverage_max_loss,
			risk_reentry_enabled = excluded.risk_reentry_enabled,
			risk_reentry_ratio = excluded.risk_reentry_ratio,
			risk_manual_reentry_enabled = excluded.risk_manual_reentry_enabled
	`, config.TraderID, config.ProviderType, config.LeaderID, config.CopyRatio,
		config.SyncLeverage, config.SyncMarginMode, config.MinTradeWarn, config.MaxTradeWarn, config.Enabled,
		config.BinanceP20T, config.BinanceCSRFToken, config.BinanceSourceMode,
		config.BinanceTopTraderID, config.SourceGeneration,
		config.RiskStopLossEnabled, config.RiskAccountPct, config.RiskATRMultiplier,
		config.RiskATRTimeframe, config.RiskLeverageFallback, config.RiskLeverageMaxLoss,
		config.RiskReentryEnabled, config.RiskReentryRatio, config.RiskManualReentryEnabled)
	if err != nil {
		return err
	}
	return s.saveCopyGuardPolicy(config)
}

// Delete 删除跟单配置
func (s *CopyTradeStore) Delete(traderID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM copy_trade_source_health WHERE trader_id = ?`, traderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM copy_trade_source_baselines WHERE trader_id = ?`, traderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM copy_trade_unsupported_instruments WHERE trader_id = ?`, traderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM copy_trade_configs WHERE trader_id = ?`, traderID); err != nil {
		return err
	}
	return tx.Commit()
}

// copyTradeConfigSelectColumns 统一的 SELECT 列清单
// 提取为常量便于 GetByTraderID / ListEnabled 共用，新增字段时一处改动
const copyTradeConfigSelectColumns = `
	trader_id, provider_type, leader_id, copy_ratio, sync_leverage, sync_margin_mode,
	min_trade_warn, max_trade_warn, enabled,
	COALESCE(binance_p20t, '') AS binance_p20t,
	COALESCE(binance_csrf_token, '') AS binance_csrf_token,
	COALESCE(binance_source_mode, 'copy_management') AS binance_source_mode,
	COALESCE(binance_top_trader_id, '') AS binance_top_trader_id,
	COALESCE(source_generation, 1) AS source_generation,
	COALESCE(risk_stop_loss_enabled, 1) AS risk_stop_loss_enabled,
	COALESCE(risk_account_pct, 0) AS risk_account_pct,
	COALESCE(risk_atr_multiplier, 1.5) AS risk_atr_multiplier,
	COALESCE(risk_atr_timeframe, '1h') AS risk_atr_timeframe,
	COALESCE(risk_leverage_fallback, 0) AS risk_leverage_fallback,
	COALESCE(risk_leverage_max_loss, 0) AS risk_leverage_max_loss,
	COALESCE(risk_reentry_enabled, 0) AS risk_reentry_enabled,
	COALESCE(risk_reentry_ratio, 0.5) AS risk_reentry_ratio,
	COALESCE(risk_manual_reentry_enabled, 1) AS risk_manual_reentry_enabled,
	created_at, updated_at`

// scanCopyTradeConfig 共用 Scan 逻辑（避免 GetByTraderID 与 ListEnabled 重复实现）
// 参数列表必须与 copyTradeConfigSelectColumns 顺序一致
func scanCopyTradeConfig(scanner interface {
	Scan(dest ...interface{}) error
}) (*CopyTradeConfig, error) {
	var config CopyTradeConfig
	var createdAt, updatedAt string
	var p20t, csrf sql.NullString

	err := scanner.Scan(
		&config.TraderID, &config.ProviderType, &config.LeaderID, &config.CopyRatio,
		&config.SyncLeverage, &config.SyncMarginMode, &config.MinTradeWarn, &config.MaxTradeWarn,
		&config.Enabled, &p20t, &csrf, &config.BinanceSourceMode,
		&config.BinanceTopTraderID, &config.SourceGeneration,
		&config.RiskStopLossEnabled, &config.RiskAccountPct, &config.RiskATRMultiplier,
		&config.RiskATRTimeframe, &config.RiskLeverageFallback, &config.RiskLeverageMaxLoss,
		&config.RiskReentryEnabled, &config.RiskReentryRatio,
		&config.RiskManualReentryEnabled,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	if p20t.Valid {
		config.BinanceP20T = p20t.String
	}
	if csrf.Valid {
		config.BinanceCSRFToken = csrf.String
	}
	config.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	config.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	config.FillRiskDefaults() // 兜底：即使 COALESCE 失效也确保默认值
	return &config, nil
}

// GetByTraderID 根据 trader_id 获取跟单配置
func (s *CopyTradeStore) GetByTraderID(traderID string) (*CopyTradeConfig, error) {
	row := s.db.QueryRow(`SELECT `+copyTradeConfigSelectColumns+` FROM copy_trade_configs WHERE trader_id = ?`, traderID)
	config, err := scanCopyTradeConfig(row)
	if err != nil {
		return nil, err
	}
	if err := s.loadCopyGuardPolicy(config); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	upgradeLegacyRiskPolicy(config)
	config.FillRiskDefaults()
	return config, nil
}

// ListEnabled 列出所有启用的跟单配置
func (s *CopyTradeStore) ListEnabled() ([]*CopyTradeConfig, error) {
	rows, err := s.db.Query(`SELECT ` + copyTradeConfigSelectColumns + ` FROM copy_trade_configs WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []*CopyTradeConfig
	for rows.Next() {
		config, err := scanCopyTradeConfig(rows)
		if err != nil {
			return nil, err
		}
		if err := s.loadCopyGuardPolicy(config); err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		upgradeLegacyRiskPolicy(config)
		config.FillRiskDefaults()
		configs = append(configs, config)
	}
	return configs, nil
}

// SetEnabled 设置跟单配置启用状态
func (s *CopyTradeStore) SetEnabled(traderID string, enabled bool) error {
	_, err := s.db.Exec(`UPDATE copy_trade_configs SET enabled = ? WHERE trader_id = ?`, enabled, traderID)
	return err
}

// UpdateDecisionMode 更新 trader 的决策模式
func (s *CopyTradeStore) UpdateDecisionMode(traderID, mode string) error {
	_, err := s.db.Exec(`UPDATE traders SET decision_mode = ? WHERE id = ?`, mode, traderID)
	return err
}

// GetDecisionMode 获取 trader 的决策模式
func (s *CopyTradeStore) GetDecisionMode(traderID string) (string, error) {
	var mode sql.NullString
	err := s.db.QueryRow(`SELECT decision_mode FROM traders WHERE id = ?`, traderID).Scan(&mode)
	if err != nil {
		return "ai", err
	}
	if !mode.Valid || mode.String == "" {
		return "ai", nil
	}
	return mode.String, nil
}

// ============================================================================
// 跟单信号日志（可选，用于调试）
// ============================================================================

// CopyTradeSignalLog 跟单信号日志
type CopyTradeSignalLog struct {
	ID           int64     `json:"id"`
	TraderID     string    `json:"trader_id"`
	LeaderID     string    `json:"leader_id"`
	ProviderType string    `json:"provider_type"`
	SignalID     string    `json:"signal_id"`
	Symbol       string    `json:"symbol"`
	Action       string    `json:"action"`
	PositionSide string    `json:"position_side"`
	LeaderPrice  float64   `json:"leader_price"`
	LeaderValue  float64   `json:"leader_value"`
	CopySize     float64   `json:"copy_size"`
	Followed     bool      `json:"followed"`
	FollowReason string    `json:"follow_reason"`
	WarningsJSON string    `json:"warnings_json"`
	Status       string    `json:"status"` // pending | executed | failed | skipped
	ErrorMessage string    `json:"error_message"`
	CreatedAt    time.Time `json:"created_at"`
}

func (s *CopyTradeStore) initSignalLogTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_signal_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL,
			leader_id TEXT NOT NULL,
			provider_type TEXT NOT NULL,
			signal_id TEXT NOT NULL,
			symbol TEXT NOT NULL,
			action TEXT NOT NULL,
			position_side TEXT NOT NULL,
			leader_price REAL,
			leader_value REAL,
			copy_size REAL,
			followed BOOLEAN DEFAULT 0,
			follow_reason TEXT,
			warnings_json TEXT,
			status TEXT DEFAULT 'pending',
			error_message TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(trader_id, signal_id)
		)
	`)
	if err != nil {
		return err
	}

	// 创建索引
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_signal_logs_trader ON copy_trade_signal_logs(trader_id)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_signal_logs_time ON copy_trade_signal_logs(created_at)`)

	return nil
}

// SaveSignalLog 保存信号日志
func (s *CopyTradeStore) SaveSignalLog(log *CopyTradeSignalLog) error {
	_, err := s.db.Exec(`
		INSERT INTO copy_trade_signal_logs 
			(trader_id, leader_id, provider_type, signal_id, symbol, action, position_side,
			 leader_price, leader_value, copy_size, followed, follow_reason, warnings_json, status, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trader_id, signal_id) DO UPDATE SET
			status = excluded.status,
			error_message = excluded.error_message
	`, log.TraderID, log.LeaderID, log.ProviderType, log.SignalID, log.Symbol, log.Action,
		log.PositionSide, log.LeaderPrice, log.LeaderValue, log.CopySize, log.Followed,
		log.FollowReason, log.WarningsJSON, log.Status, log.ErrorMessage)
	return err
}

// GetRecentSignalLogs 获取最近的信号日志
func (s *CopyTradeStore) GetRecentSignalLogs(traderID string, limit int) ([]*CopyTradeSignalLog, error) {
	rows, err := s.db.Query(`
		SELECT id, trader_id, leader_id, provider_type, signal_id, symbol, action, position_side,
		       leader_price, leader_value, copy_size, followed, follow_reason, warnings_json, status, 
		       COALESCE(error_message, ''), created_at
		FROM copy_trade_signal_logs 
		WHERE trader_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, traderID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*CopyTradeSignalLog
	for rows.Next() {
		var log CopyTradeSignalLog
		var createdAt string

		err := rows.Scan(
			&log.ID, &log.TraderID, &log.LeaderID, &log.ProviderType, &log.SignalID,
			&log.Symbol, &log.Action, &log.PositionSide, &log.LeaderPrice, &log.LeaderValue,
			&log.CopySize, &log.Followed, &log.FollowReason, &log.WarningsJSON,
			&log.Status, &log.ErrorMessage, &createdAt,
		)
		if err != nil {
			return nil, err
		}

		log.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		logs = append(logs, &log)
	}

	return logs, nil
}

// ============================================================================
// 仓位映射（跟单仓位生命周期管理）
// ============================================================================

// Mapping 状态常量（避免散落的魔法字符串）
const (
	MappingStatusActive        = "active"
	MappingStatusClosed        = "closed"
	MappingStatusIgnored       = "ignored"
	MappingStatusStoppedByRisk = "stopped_by_risk" // v3 风控：账户保护止损被交易所触发
)

// CopyTradePositionMapping 仓位映射记录
// 一条映射 = 一笔跟单仓位的完整生命周期（开仓 → 平仓）
// 用于精确匹配领航员仓位与跟随者仓位，解决同币种多仓位（cross/isolated）的识别问题
type CopyTradePositionMapping struct {
	ID                   int64  `json:"id"`
	TraderID             string `json:"trader_id"`     // 跟随者 trader ID（多账户隔离）
	LeaderPosID          string `json:"leader_pos_id"` // 领航员仓位 ID = 本地标识（OKX posId）
	LeaderID             string `json:"leader_id"`     // 领航员 ID
	Symbol               string `json:"symbol"`        // LINKUSDT
	SourceSymbol         string `json:"source_symbol,omitempty"`
	ExecutionSymbol      string `json:"execution_symbol,omitempty"`
	SourceQuoteAsset     string `json:"source_quote_asset,omitempty"`
	ExecutionSettleAsset string `json:"execution_settle_asset,omitempty"`
	// SourceRevision is the persisted acknowledgement sequence for source
	// position changes. Smart Money snapshot IDs include it so an identical
	// size transition in a later lifecycle cannot reuse an earlier order ID,
	// while a retry before acknowledgement keeps the same identity.
	SourceRevision int64  `json:"source_revision,omitempty"`
	Side           string `json:"side"`        // long | short
	MarginMode     string `json:"margin_mode"` // cross | isolated
	Status         string `json:"status"`      // active | closed | ignored | stopped_by_risk

	// 开仓信息
	OpenedAt      time.Time `json:"opened_at"`       // 跟单开仓时间
	OpenPrice     float64   `json:"open_price"`      // 领航员开仓价格
	OpenSizeUSD   float64   `json:"open_size_usd"`   // 跟单开仓金额
	LastKnownSize float64   `json:"last_known_size"` // 领航员上次已知持仓数量（用于精确匹配 posId）

	// 平仓信息（平仓时填充）
	ClosedAt   *time.Time `json:"closed_at"`   // 平仓时间
	ClosePrice float64    `json:"close_price"` // 平仓价格

	// 累计统计（加仓/减仓时更新）
	AddCount               int       `json:"add_count"`                // 累计加仓次数
	ReduceCount            int       `json:"reduce_count"`             // 累计减仓次数
	AccumulatedReduceRatio float64   `json:"accumulated_reduce_ratio"` // 累计减仓比例（用于触发全平）
	UpdatedAt              time.Time `json:"updated_at"`               // 最后更新时间

	// 连续失败熔断（执行失败时累加，成功时清零；超过阈值自动 CloseMapping）
	ConsecutiveFailCount int        `json:"consecutive_fail_count"` // 连续失败次数
	LastFailureAt        *time.Time `json:"last_failure_at"`        // 最后一次失败时间
	LastFailureReason    string     `json:"last_failure_reason"`    // 最后一次失败原因

	// ============================================================
	// 账户保护止损触发快照（v3 风控，仅 status=stopped_by_risk 时有效）
	// 用于判据 E（双门控）二次进场判断
	// ============================================================
	StoppedAt        *time.Time `json:"stopped_at,omitempty"`          // SL 触发时间
	LeaderPnLAtStop  float64    `json:"leader_pnl_at_stop,omitempty"`  // SL 触发时领航员该仓位浮亏（负值）
	LeaderSizeAtStop float64    `json:"leader_size_at_stop,omitempty"` // SL 触发时领航员该仓位大小
	AddCountAtStop   int        `json:"add_count_at_stop,omitempty"`   // SL 触发时 AddCount 值（判反加仓）
	ReentryUsed      bool       `json:"reentry_used,omitempty"`        // 该 posId 二次进场是否已用过（限 1 次）
}

// initPositionMappingTable 初始化仓位映射表
func (s *CopyTradeStore) initPositionMappingTable() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_trade_position_mappings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL,
			leader_pos_id TEXT NOT NULL,
			leader_id TEXT NOT NULL,
			symbol TEXT NOT NULL,
			source_symbol TEXT DEFAULT '',
			execution_symbol TEXT DEFAULT '',
			source_quote_asset TEXT DEFAULT '',
			execution_settle_asset TEXT DEFAULT '',
			source_revision INTEGER DEFAULT 0,
			side TEXT NOT NULL,
			margin_mode TEXT NOT NULL,
			status TEXT DEFAULT 'active',
			
			opened_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			open_price REAL DEFAULT 0,
			open_size_usd REAL DEFAULT 0,
			
			closed_at DATETIME,
			close_price REAL DEFAULT 0,
			
			add_count INTEGER DEFAULT 0,
			reduce_count INTEGER DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			
			UNIQUE(trader_id, leader_pos_id)
		)
	`)
	if err != nil {
		return err
	}

	// 创建索引
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_mapping_trader_status ON copy_trade_position_mappings(trader_id, status)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_mapping_trader_symbol ON copy_trade_position_mappings(trader_id, symbol, side, status)`)

	// 添加 last_known_size 字段（如果不存在）
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN last_known_size REAL DEFAULT 0`)

	// 添加 accumulated_reduce_ratio 字段（用于累积减仓触发全平）
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN accumulated_reduce_ratio REAL DEFAULT 0`)

	// 添加连续失败熔断字段（兼容旧库；ALTER 失败说明已存在，忽略）
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN consecutive_fail_count INTEGER DEFAULT 0`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN last_failure_at DATETIME`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN last_failure_reason TEXT DEFAULT ''`)

	// 添加账户保护止损触发快照字段（v3 风控）
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN stopped_at DATETIME`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN leader_pnl_at_stop REAL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN leader_size_at_stop REAL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN add_count_at_stop INTEGER DEFAULT 0`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN reentry_used INTEGER DEFAULT 0`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN source_symbol TEXT DEFAULT ''`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN execution_symbol TEXT DEFAULT ''`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN source_quote_asset TEXT DEFAULT ''`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN execution_settle_asset TEXT DEFAULT ''`)
	s.db.Exec(`ALTER TABLE copy_trade_position_mappings ADD COLUMN source_revision INTEGER DEFAULT 0`)

	return nil
}

// SavePositionMapping 保存仓位映射（开仓时调用）
func (s *CopyTradeStore) SavePositionMapping(mapping *CopyTradePositionMapping) error {
	_, err := s.db.Exec(`
		INSERT INTO copy_trade_position_mappings 
			(trader_id, leader_pos_id, leader_id, symbol, source_symbol, execution_symbol, source_quote_asset, execution_settle_asset, source_revision, side, margin_mode, status,
			 opened_at, open_price, open_size_usd, last_known_size, add_count, reduce_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 'active', ?, ?, ?, ?, 0, 0, CURRENT_TIMESTAMP)
		ON CONFLICT(trader_id, leader_pos_id) DO UPDATE SET
			side = excluded.side,
			symbol = excluded.symbol,
			source_symbol = excluded.source_symbol,
			execution_symbol = excluded.execution_symbol,
			source_quote_asset = excluded.source_quote_asset,
			execution_settle_asset = excluded.execution_settle_asset,
			source_revision = COALESCE(copy_trade_position_mappings.source_revision, 0) + 1,
			margin_mode = excluded.margin_mode,
			status = 'active',
			opened_at = excluded.opened_at,
			open_price = excluded.open_price,
			open_size_usd = excluded.open_size_usd,
			last_known_size = excluded.last_known_size,
			add_count = 0,
			reduce_count = 0,
			updated_at = CURRENT_TIMESTAMP
	`, mapping.TraderID, mapping.LeaderPosID, mapping.LeaderID, mapping.Symbol,
		mapping.SourceSymbol, mapping.ExecutionSymbol, mapping.SourceQuoteAsset, mapping.ExecutionSettleAsset,
		mapping.Side, mapping.MarginMode, mapping.OpenedAt, mapping.OpenPrice, mapping.OpenSizeUSD, mapping.LastKnownSize)
	return err
}

// mappingSelectColumns 统一的 mapping SELECT 列清单
// 提取为常量便于多处 SELECT 共用，新增字段时一处改动
const mappingSelectColumns = `
	id, trader_id, leader_pos_id, leader_id, symbol,
	COALESCE(source_symbol,''), COALESCE(execution_symbol,''), COALESCE(source_quote_asset,''), COALESCE(execution_settle_asset,''),
	COALESCE(source_revision,0),
	side, margin_mode, status,
	opened_at, open_price, open_size_usd, last_known_size, closed_at, close_price,
	add_count, reduce_count, updated_at,
	COALESCE(stopped_at, '') AS stopped_at,
	COALESCE(leader_pnl_at_stop, 0) AS leader_pnl_at_stop,
	COALESCE(leader_size_at_stop, 0) AS leader_size_at_stop,
	COALESCE(add_count_at_stop, 0) AS add_count_at_stop,
	COALESCE(reentry_used, 0) AS reentry_used`

// scanMapping 共用 mapping Scan 逻辑
// 参数顺序与 mappingSelectColumns 一致
func scanMapping(scanner interface {
	Scan(dest ...interface{}) error
}) (*CopyTradePositionMapping, error) {
	var mapping CopyTradePositionMapping
	var openedAt, updatedAt string
	var closedAt, stoppedAt sql.NullString

	err := scanner.Scan(
		&mapping.ID, &mapping.TraderID, &mapping.LeaderPosID, &mapping.LeaderID,
		&mapping.Symbol, &mapping.SourceSymbol, &mapping.ExecutionSymbol, &mapping.SourceQuoteAsset, &mapping.ExecutionSettleAsset,
		&mapping.SourceRevision,
		&mapping.Side, &mapping.MarginMode, &mapping.Status,
		&openedAt, &mapping.OpenPrice, &mapping.OpenSizeUSD, &mapping.LastKnownSize, &closedAt, &mapping.ClosePrice,
		&mapping.AddCount, &mapping.ReduceCount, &updatedAt,
		&stoppedAt, &mapping.LeaderPnLAtStop, &mapping.LeaderSizeAtStop,
		&mapping.AddCountAtStop, &mapping.ReentryUsed,
	)
	if err != nil {
		return nil, err
	}

	mapping.OpenedAt, _ = time.Parse("2006-01-02 15:04:05", openedAt)
	mapping.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	if closedAt.Valid && closedAt.String != "" {
		t, _ := time.Parse("2006-01-02 15:04:05", closedAt.String)
		mapping.ClosedAt = &t
	}
	if stoppedAt.Valid && stoppedAt.String != "" {
		t, _ := time.Parse("2006-01-02 15:04:05", stoppedAt.String)
		mapping.StoppedAt = &t
	}
	return &mapping, nil
}

// GetActiveMapping 查询活跃的仓位映射（判断开仓/加仓时调用）
func (s *CopyTradeStore) GetActiveMapping(traderID, leaderPosID string) (*CopyTradePositionMapping, error) {
	return s.getMappingByStatus(traderID, leaderPosID, "active")
}

// GetMapping 查询任意状态的仓位映射（包括 active/ignored/closed/stopped_by_risk）
// 用于判断是否应该跟随信号：
//   - active: 已跟随的仓位 → 继续跟随
//   - ignored: 启动时的历史仓位 → 不跟随
//   - closed: 已平仓 → 可以重新开仓
//   - stopped_by_risk: 风控止损触发 → 不跟随，等领航员完全平掉该 posId
//   - nil: 无映射 → 新开仓
func (s *CopyTradeStore) GetMapping(traderID, leaderPosID string) (*CopyTradePositionMapping, error) {
	return s.getMappingByStatus(traderID, leaderPosID, "")
}

// GetSourceSnapshotRevision returns the last persisted acknowledgement
// sequence even when the mapping is closed. GetMapping intentionally hides
// closed mappings, but Smart Money must retain their sequence so a reopened
// position cannot reuse the previous lifecycle's synthetic fill/order ID.
func (s *CopyTradeStore) GetSourceSnapshotRevision(traderID, leaderPosID string) (int64, error) {
	var revision int64
	err := s.db.QueryRow(`
		SELECT COALESCE(source_revision, 0)
		FROM copy_trade_position_mappings
		WHERE trader_id = ? AND leader_pos_id = ?
	`, traderID, leaderPosID).Scan(&revision)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return revision, err
}

// getMappingByStatus 内部方法：按状态查询映射
func (s *CopyTradeStore) getMappingByStatus(traderID, leaderPosID, status string) (*CopyTradePositionMapping, error) {
	query := `SELECT ` + mappingSelectColumns + ` FROM copy_trade_position_mappings WHERE trader_id = ? AND leader_pos_id = ?`
	args := []interface{}{traderID, leaderPosID}

	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	} else {
		// 无状态筛选时，优先级 active > stopped_by_risk > ignored，忽略 closed
		// stopped_by_risk 排在 ignored 前面：让上层 matchSignal 能及时看到熔断状态
		query += " AND status IN ('active', 'stopped_by_risk', 'ignored') ORDER BY CASE status WHEN 'active' THEN 1 WHEN 'stopped_by_risk' THEN 2 WHEN 'ignored' THEN 3 END LIMIT 1"
	}

	row := s.db.QueryRow(query, args...)
	mapping, err := scanMapping(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return mapping, err
}

// SaveIgnoredPosition 保存历史仓位（启动跟单时调用）
// 标记为 ignored 状态，后续这些仓位的操作都不跟随
// HasMappingsForLeader 该 trader 对该 leader 是否已有任何 mapping 记录（任意状态）。
// 用于区分"首次启用跟单"（应全量标 ignored 保护历史仓位）与"重启/配置变更"
// （跳过全量 ignored，避免停机窗口期的新仓被误标为永久忽略）。
func (s *CopyTradeStore) HasMappingsForLeader(traderID, leaderID string) (bool, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(1) FROM copy_trade_position_mappings
		WHERE trader_id = ? AND leader_id = ?
	`, traderID, leaderID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// HasLiveSourceState protects source identity changes from orphaning an active
// follower position or a Copy Guard lifecycle that is still stopped_by_risk.
func (s *CopyTradeStore) HasLiveSourceState(traderID string) (bool, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT
			(SELECT COUNT(1) FROM copy_trade_position_mappings WHERE trader_id=? AND status IN ('active','stopped_by_risk')) +
			(SELECT COUNT(1) FROM copy_guard_cycles WHERE trader_id=? AND closed_at IS NULL)
	`, traderID, traderID).Scan(&count)
	return count > 0, err
}

func (s *CopyTradeStore) SaveIgnoredPosition(traderID, leaderID, leaderPosID, symbol, side, marginMode string) error {
	_, err := s.db.Exec(`
		INSERT INTO copy_trade_position_mappings 
			(trader_id, leader_pos_id, leader_id, symbol, side, margin_mode, status,
			 opened_at, open_price, open_size_usd, add_count, reduce_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'ignored', CURRENT_TIMESTAMP, 0, 0, 0, 0, CURRENT_TIMESTAMP)
		ON CONFLICT(trader_id, leader_pos_id) DO NOTHING
	`, traderID, leaderPosID, leaderID, symbol, side, marginMode)
	return err
}

// RebaselineIgnoredPosition is used after a Smart Money visibility gap or an
// unsupported execution instrument. It may turn a previously closed lifecycle
// into ignored, but never overwrites an active or stopped-by-risk mapping.
func (s *CopyTradeStore) RebaselineIgnoredPosition(traderID, leaderID, leaderPosID, symbol, side, marginMode string, size float64) error {
	_, err := s.db.Exec(`
		INSERT INTO copy_trade_position_mappings
			(trader_id, leader_pos_id, leader_id, symbol, source_revision, side, margin_mode, status,
			 opened_at, open_price, open_size_usd, last_known_size, add_count, reduce_count, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?, 'ignored', CURRENT_TIMESTAMP, 0, 0, ?, 0, 0, CURRENT_TIMESTAMP)
		ON CONFLICT(trader_id, leader_pos_id) DO UPDATE SET
			leader_id=excluded.leader_id, symbol=excluded.symbol, side=excluded.side,
			margin_mode=excluded.margin_mode, status='ignored', last_known_size=excluded.last_known_size,
			source_revision=COALESCE(copy_trade_position_mappings.source_revision, 0) + 1,
			updated_at=CURRENT_TIMESTAMP
		WHERE copy_trade_position_mappings.status='closed'
	`, traderID, leaderPosID, leaderID, symbol, side, marginMode, size)
	return err
}

// IncrementAddCount 增加加仓次数（加仓时调用）
func (s *CopyTradeStore) IncrementAddCount(traderID, leaderPosID string) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings 
		SET add_count = add_count + 1, updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
	`, traderID, leaderPosID)
	return err
}

// IncrementReduceCount 增加减仓次数（减仓时调用）
func (s *CopyTradeStore) IncrementReduceCount(traderID, leaderPosID string) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings 
		SET reduce_count = reduce_count + 1, updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
	`, traderID, leaderPosID)
	return err
}

// UpdateAccumulatedReduceRatio 更新累积减仓比例（减仓时调用）
// 用于跟踪累积减仓进度，当超过阈值（如 90%）时触发全平
func (s *CopyTradeStore) UpdateAccumulatedReduceRatio(traderID, leaderPosID string, ratio float64) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings 
		SET accumulated_reduce_ratio = ?, updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
	`, ratio, traderID, leaderPosID)
	return err
}

// GetAccumulatedReduceRatio 获取累积减仓比例
func (s *CopyTradeStore) GetAccumulatedReduceRatio(traderID, leaderPosID string) (float64, error) {
	var ratio float64
	err := s.db.QueryRow(`
		SELECT COALESCE(accumulated_reduce_ratio, 0)
		FROM copy_trade_position_mappings 
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
	`, traderID, leaderPosID).Scan(&ratio)
	if err != nil {
		return 0, err
	}
	return ratio, nil
}

// ClearAccumulatedReduceRatio 清除累积减仓比例（全平后调用）
func (s *CopyTradeStore) ClearAccumulatedReduceRatio(traderID, leaderPosID string) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings 
		SET accumulated_reduce_ratio = 0, updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ?
	`, traderID, leaderPosID)
	return err
}

// IncrementMappingFailure 累加 active mapping 的连续失败次数并记录原因，
// 返回累加后的最新 count。
//
// 调用时机：跟单决策执行失败且非"良性失败"时（良性失败已主动 CloseMapping）。
// 配合 ResetMappingFailure（成功时清零）与上层熔断阈值，自动回收"长期失败"的映射。
//
// 错误返回包括：
//   - active mapping 不存在（UPDATE 影响 0 行 → 返回 0, nil；上层可据此判断"无熔断对象"）
//   - 数据库写入失败 → 返回 0, err
func (s *CopyTradeStore) IncrementMappingFailure(traderID, leaderPosID, reason string) (int, error) {
	res, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings 
		SET consecutive_fail_count = consecutive_fail_count + 1,
		    last_failure_at = CURRENT_TIMESTAMP,
		    last_failure_reason = ?,
		    updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
	`, reason, traderID, leaderPosID)
	if err != nil {
		return 0, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// 无 active mapping（可能已被 CloseMapping/ignored），无熔断对象
		return 0, nil
	}

	var count int
	err = s.db.QueryRow(`
		SELECT COALESCE(consecutive_fail_count, 0)
		FROM copy_trade_position_mappings 
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
	`, traderID, leaderPosID).Scan(&count)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return count, nil
}

// ResetMappingFailure 清零 active mapping 的连续失败计数（执行成功时调用）。
// 若没有 active mapping 则视为 no-op（不报错）。
func (s *CopyTradeStore) ResetMappingFailure(traderID, leaderPosID string) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings 
		SET consecutive_fail_count = 0,
		    last_failure_reason = '',
		    updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
		  AND consecutive_fail_count > 0
	`, traderID, leaderPosID)
	return err
}

// UpdateLastKnownSize 更新领航员上次已知持仓数量（加仓/减仓后调用）
// 用于精确匹配：通过 size 变化确定是哪个 posId 发生了操作
func (s *CopyTradeStore) UpdateLastKnownSize(traderID, leaderPosID string, size float64) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings 
		SET last_known_size = ?, source_revision = COALESCE(source_revision, 0) + 1, updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
	`, size, traderID, leaderPosID)
	return err
}

// CloseMapping 关闭仓位映射（平仓时调用）
func (s *CopyTradeStore) CloseMapping(traderID, leaderPosID string, closePrice float64) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings 
		SET status = 'closed', closed_at = CURRENT_TIMESTAMP, close_price = ?,
		    source_revision = COALESCE(source_revision, 0) + 1, updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
	`, closePrice, traderID, leaderPosID)
	return err
}

// ListActiveMappings 列出某 trader 所有活跃映射（调试/展示）
func (s *CopyTradeStore) ListActiveMappings(traderID string) ([]*CopyTradePositionMapping, error) {
	return s.listMappings(traderID, "active", 0)
}

// ListIgnoredMappings 列出某 trader 所有 ignored 映射
// 用于检测历史仓位是否已被领航员平仓
func (s *CopyTradeStore) ListIgnoredMappings(traderID string) ([]*CopyTradePositionMapping, error) {
	return s.listMappings(traderID, "ignored", 0)
}

// MarkIgnoredAsClosed 将 ignored 状态的映射标记为 closed
// 当检测到领航员的历史仓位已被平仓时调用，这样后续重新开仓可以跟随
func (s *CopyTradeStore) MarkIgnoredAsClosed(traderID, leaderPosID string) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings 
		SET status = 'closed', closed_at = CURRENT_TIMESTAMP,
		    source_revision = COALESCE(source_revision, 0) + 1, updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'ignored'
	`, traderID, leaderPosID)
	return err
}

// FindActiveBySymbolSide 查找某 symbol+side 的所有活跃映射
// 用于平仓/减仓时的反向查找：从本地映射出发，对比领航员持仓判断动作
func (s *CopyTradeStore) FindActiveBySymbolSide(traderID, symbol, side string) ([]*CopyTradePositionMapping, error) {
	query := `SELECT ` + mappingSelectColumns + ` FROM copy_trade_position_mappings WHERE trader_id = ? AND symbol = ? AND side = ? AND status = 'active' ORDER BY opened_at ASC`

	rows, err := s.db.Query(query, traderID, symbol, side)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []*CopyTradePositionMapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	return mappings, nil
}

// FindActiveBySymbol 查找某 symbol 的所有活跃映射（不限方向）
// 用于单向持仓模式平仓时的反向查找：领航员持仓已消失，无法确定方向
func (s *CopyTradeStore) FindActiveBySymbol(traderID, symbol string) ([]*CopyTradePositionMapping, error) {
	query := `SELECT ` + mappingSelectColumns + ` FROM copy_trade_position_mappings WHERE trader_id = ? AND symbol = ? AND status = 'active' ORDER BY opened_at ASC`

	rows, err := s.db.Query(query, traderID, symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []*CopyTradePositionMapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	return mappings, nil
}

// ListAllMappings 列出某 trader 所有映射（含历史）
func (s *CopyTradeStore) ListAllMappings(traderID string, limit int) ([]*CopyTradePositionMapping, error) {
	return s.listMappings(traderID, "", limit)
}

// listMappings 内部方法：查询映射列表
func (s *CopyTradeStore) listMappings(traderID, status string, limit int) ([]*CopyTradePositionMapping, error) {
	query := `SELECT ` + mappingSelectColumns + ` FROM copy_trade_position_mappings WHERE trader_id = ?`
	args := []interface{}{traderID}

	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}

	query += " ORDER BY opened_at DESC"

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []*CopyTradePositionMapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	return mappings, nil
}

// ListStoppedByRiskMappings 列出某 trader 所有 stopped_by_risk 状态的映射
// 用于二次进场监控循环
func (s *CopyTradeStore) ListStoppedByRiskMappings(traderID string) ([]*CopyTradePositionMapping, error) {
	return s.listMappings(traderID, MappingStatusStoppedByRisk, 0)
}

// MarkStoppedByRisk 标记 active 映射为风控止损触发，并保存快照
// 用于 SL 被交易所触发后的对账识别
//
// 参数：
//   - leaderPnL: SL 触发时领航员该 posId 的浮亏（应为负值）
//   - leaderSize: SL 触发时领航员该 posId 的持仓数量
//   - addCount: SL 触发时 mapping.AddCount 的当前值（用于反加仓判据）
//
// 仅 active 状态的映射会被标记，幂等：重复调用不会出错
func (s *CopyTradeStore) MarkStoppedByRisk(traderID, leaderPosID string, leaderPnL, leaderSize float64, addCount int) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings
		SET status = 'stopped_by_risk',
		    stopped_at = CURRENT_TIMESTAMP,
		    leader_pnl_at_stop = ?,
		    leader_size_at_stop = ?,
		    add_count_at_stop = ?,
		    updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'active'
	`, leaderPnL, leaderSize, addCount, traderID, leaderPosID)
	return err
}

// ActivateMappingAfterReentry is the v4 success-only transition. It is deliberately called after
// exchange execution succeeds, so a rejected order never consumes or activates a reentry.
func (s *CopyTradeStore) ActivateMappingAfterReentry(traderID, leaderPosID string, entryPrice, openSizeUSD, leaderSize float64) error {
	_, err := s.db.Exec(`UPDATE copy_trade_position_mappings SET status='active', open_price=?, open_size_usd=?,
		last_known_size=?, stopped_at=NULL, source_revision=COALESCE(source_revision, 0) + 1, updated_at=CURRENT_TIMESTAMP
		WHERE trader_id=? AND leader_pos_id=? AND status='stopped_by_risk'`, entryPrice, openSizeUSD, leaderSize, traderID, leaderPosID)
	return err
}

// MarkStoppedByRiskAsClosed 将 stopped_by_risk 映射标记为 closed
// 调用时机：领航员完全平掉旧 posId（其持仓中已无该 posId）后
// 这样下次他用同一 posId 重新开仓（理论上不会复用，但兼容处理），会按 closed → 新开仓走
func (s *CopyTradeStore) MarkStoppedByRiskAsClosed(traderID, leaderPosID string) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_position_mappings
		SET status = 'closed', closed_at = CURRENT_TIMESTAMP,
		    source_revision = COALESCE(source_revision, 0) + 1, updated_at = CURRENT_TIMESTAMP
		WHERE trader_id = ? AND leader_pos_id = ? AND status = 'stopped_by_risk'
	`, traderID, leaderPosID)
	return err
}

// ============================================================================
// 辅助函数
// ============================================================================

// ToJSON 将配置转换为 JSON 字符串
func (c *CopyTradeConfig) ToJSON() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// FromJSON 从 JSON 字符串解析配置
func CopyTradeConfigFromJSON(jsonStr string) (*CopyTradeConfig, error) {
	var config CopyTradeConfig
	err := json.Unmarshal([]byte(jsonStr), &config)
	return &config, err
}
