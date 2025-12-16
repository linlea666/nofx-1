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
	ProviderType   string  `json:"provider_type"`    // "hyperliquid" | "okx"
	LeaderID       string  `json:"leader_id"`        // 领航员地址/uniqueName
	CopyRatio      float64 `json:"copy_ratio"`       // 跟单系数 (1.0 = 100%)
	SyncLeverage   bool    `json:"sync_leverage"`    // 同步杠杆
	SyncMarginMode bool    `json:"sync_margin_mode"` // 同步保证金模式
	MinTradeWarn   float64 `json:"min_trade_warn"`   // 小额预警阈值
	MaxTradeWarn   float64 `json:"max_trade_warn"`   // 大额预警阈值 (0=不预警)
	Enabled        bool    `json:"enabled"`          // 是否启用

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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

	return nil
}

// Create 创建跟单配置
func (s *CopyTradeStore) Create(config *CopyTradeConfig) error {
	_, err := s.db.Exec(`
		INSERT INTO copy_trade_configs 
			(trader_id, provider_type, leader_id, copy_ratio, sync_leverage, sync_margin_mode, 
			 min_trade_warn, max_trade_warn, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, config.TraderID, config.ProviderType, config.LeaderID, config.CopyRatio,
		config.SyncLeverage, config.SyncMarginMode, config.MinTradeWarn, config.MaxTradeWarn, config.Enabled)
	return err
}

// Update 更新跟单配置
func (s *CopyTradeStore) Update(config *CopyTradeConfig) error {
	_, err := s.db.Exec(`
		UPDATE copy_trade_configs SET
			provider_type = ?,
			leader_id = ?,
			copy_ratio = ?,
			sync_leverage = ?,
			sync_margin_mode = ?,
			min_trade_warn = ?,
			max_trade_warn = ?,
			enabled = ?
		WHERE trader_id = ?
	`, config.ProviderType, config.LeaderID, config.CopyRatio,
		config.SyncLeverage, config.SyncMarginMode, config.MinTradeWarn, config.MaxTradeWarn,
		config.Enabled, config.TraderID)
	return err
}

// Upsert 创建或更新跟单配置
func (s *CopyTradeStore) Upsert(config *CopyTradeConfig) error {
	_, err := s.db.Exec(`
		INSERT INTO copy_trade_configs 
			(trader_id, provider_type, leader_id, copy_ratio, sync_leverage, sync_margin_mode, 
			 min_trade_warn, max_trade_warn, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trader_id) DO UPDATE SET
			provider_type = excluded.provider_type,
			leader_id = excluded.leader_id,
			copy_ratio = excluded.copy_ratio,
			sync_leverage = excluded.sync_leverage,
			sync_margin_mode = excluded.sync_margin_mode,
			min_trade_warn = excluded.min_trade_warn,
			max_trade_warn = excluded.max_trade_warn,
			enabled = excluded.enabled
	`, config.TraderID, config.ProviderType, config.LeaderID, config.CopyRatio,
		config.SyncLeverage, config.SyncMarginMode, config.MinTradeWarn, config.MaxTradeWarn, config.Enabled)
	return err
}

// Delete 删除跟单配置
func (s *CopyTradeStore) Delete(traderID string) error {
	_, err := s.db.Exec(`DELETE FROM copy_trade_configs WHERE trader_id = ?`, traderID)
	return err
}

// GetByTraderID 根据 trader_id 获取跟单配置
func (s *CopyTradeStore) GetByTraderID(traderID string) (*CopyTradeConfig, error) {
	var config CopyTradeConfig
	var createdAt, updatedAt string

	err := s.db.QueryRow(`
		SELECT trader_id, provider_type, leader_id, copy_ratio, sync_leverage, sync_margin_mode,
		       min_trade_warn, max_trade_warn, enabled, created_at, updated_at
		FROM copy_trade_configs WHERE trader_id = ?
	`, traderID).Scan(
		&config.TraderID, &config.ProviderType, &config.LeaderID, &config.CopyRatio,
		&config.SyncLeverage, &config.SyncMarginMode, &config.MinTradeWarn, &config.MaxTradeWarn,
		&config.Enabled, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	config.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	config.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)

	return &config, nil
}

// ListEnabled 列出所有启用的跟单配置
func (s *CopyTradeStore) ListEnabled() ([]*CopyTradeConfig, error) {
	rows, err := s.db.Query(`
		SELECT trader_id, provider_type, leader_id, copy_ratio, sync_leverage, sync_margin_mode,
		       min_trade_warn, max_trade_warn, enabled, created_at, updated_at
		FROM copy_trade_configs WHERE enabled = 1
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []*CopyTradeConfig
	for rows.Next() {
		var config CopyTradeConfig
		var createdAt, updatedAt string

		err := rows.Scan(
			&config.TraderID, &config.ProviderType, &config.LeaderID, &config.CopyRatio,
			&config.SyncLeverage, &config.SyncMarginMode, &config.MinTradeWarn, &config.MaxTradeWarn,
			&config.Enabled, &createdAt, &updatedAt,
		)
		if err != nil {
			return nil, err
		}

		config.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		config.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)

		configs = append(configs, &config)
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

