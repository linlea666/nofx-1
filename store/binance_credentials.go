// Package store binance_credentials.go
// 全局币安 Web 凭证存储（账户级共享，多个 Binance 跟单 trader 共用一份凭证）
//
// 设计动机：
//   - p20t / csrftoken 是币安"账户级"凭证（cookie），不是"领航员级"凭证
//   - 旧设计每个 trader 各存一份 → 凭证过期需逐个更新，运维负担 O(N)
//   - 新设计一处更新、全局生效，告警按 label 单条触发
//
// 多账号设计：
//   - 主键 label 默认 'default'；v1 仅使用 default
//   - v2 可扩展为多 label，每个 label 对应一个币安账号
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"nofx/logger"
)

// ============================================================================
// 常量与类型
// ============================================================================

const (
	// BinanceCredsLabelDefault 默认凭证 label（v1 单账号）
	BinanceCredsLabelDefault = "default"

	// 凭证状态常量
	BinanceCredsStatusValid   = "valid"
	BinanceCredsStatusExpired = "expired"
	BinanceCredsStatusUnknown = "unknown"
	BinanceCredsStatusError   = "error"
)

// ErrBinanceCredsNotConfigured 全局凭证未配置（区别于"凭证已过期"）
// 用于上层降级到旧 trader 凭证（向后兼容路径）
var ErrBinanceCredsNotConfigured = errors.New("binance global credentials not configured")

// BinanceCredentials 全局币安凭证
type BinanceCredentials struct {
	Label           string    `json:"label"`             // 凭证标签（默认 "default"）
	P20T            string    `json:"-"`                 // 登录 cookie，输出时脱敏
	CSRFToken       string    `json:"-"`                 // CSRF header，输出时脱敏
	BinanceUserID   string    `json:"binance_user_id"`   // 从 get-user-base-info 拿到，便于识别绑定账号
	LastValidatedAt time.Time `json:"last_validated_at"` // 最后一次成功校验时间
	LastStatus      string    `json:"last_status"`       // valid / expired / unknown / error
	LastError       string    `json:"last_error"`        // 最近一次校验错误信息（脱敏后）
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MaskedP20T 返回脱敏后的 p20t（前 6 + 后 4 字符），用于 API 响应
func (c *BinanceCredentials) MaskedP20T() string {
	return maskSecret(c.P20T)
}

// MaskedCSRFToken 返回脱敏后的 csrftoken
func (c *BinanceCredentials) MaskedCSRFToken() string {
	return maskSecret(c.CSRFToken)
}

// MaskSecret 脱敏任意凭证字符串（前 6 + 后 4 字符），供 API 层复用。
// 掩码结果必然包含 "***"，可作为"未修改"哨兵在保存路径识别。
func MaskSecret(s string) string {
	return maskSecret(s)
}

func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 12 {
		return "***" // 短到一定程度直接遮蔽，不暴露长度信息
	}
	return s[:6] + "***" + s[len(s)-4:]
}

// ============================================================================
// Sub-Store
// ============================================================================

// BinanceCredentialsStore 全局币安凭证存储
//
// 内存缓存 + DB 持久化双层。所有写入路径会立即清除内存缓存，下次读触发 DB 重载，
// 实现热加载（用户在 UI 更新凭证后，无需重启即可对所有 Binance 跟单 trader 生效）。
type BinanceCredentialsStore struct {
	db *sql.DB

	// 内存缓存（按 label 索引）
	mu    sync.RWMutex
	cache map[string]*BinanceCredentials
}

func (s *BinanceCredentialsStore) initTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS binance_credentials (
			label TEXT PRIMARY KEY DEFAULT 'default',
			p20t TEXT NOT NULL DEFAULT '',
			csrf_token TEXT NOT NULL DEFAULT '',
			binance_user_id TEXT NOT NULL DEFAULT '',
			last_validated_at INTEGER NOT NULL DEFAULT 0,
			last_status TEXT NOT NULL DEFAULT 'unknown',
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`)
	return err
}

// Get 获取指定 label 的凭证（优先内存缓存）
//
// 行为：
//   - label 为空时使用 default
//   - 不存在返回 (nil, nil)，调用者据此判断"未配置"
//   - DB 错误返回 (nil, error)
func (s *BinanceCredentialsStore) Get(label string) (*BinanceCredentials, error) {
	if label == "" {
		label = BinanceCredsLabelDefault
	}

	// 1) 内存缓存
	s.mu.RLock()
	if c, ok := s.cache[label]; ok {
		s.mu.RUnlock()
		return c, nil
	}
	s.mu.RUnlock()

	// 2) DB 加载
	creds, err := s.loadFromDB(label)
	if err != nil {
		return nil, err
	}

	// 3) 写缓存（即使 nil 也不缓存，避免之后 Set 后还命中 nil）
	if creds != nil {
		s.mu.Lock()
		if s.cache == nil {
			s.cache = make(map[string]*BinanceCredentials)
		}
		s.cache[label] = creds
		s.mu.Unlock()
	}

	return creds, nil
}

func (s *BinanceCredentialsStore) loadFromDB(label string) (*BinanceCredentials, error) {
	var c BinanceCredentials
	var lastValidatedAt, createdAt, updatedAt int64

	err := s.db.QueryRow(`
		SELECT label, p20t, csrf_token, binance_user_id,
		       last_validated_at, last_status, last_error,
		       created_at, updated_at
		FROM binance_credentials WHERE label = ?
	`, label).Scan(
		&c.Label, &c.P20T, &c.CSRFToken, &c.BinanceUserID,
		&lastValidatedAt, &c.LastStatus, &c.LastError,
		&createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if lastValidatedAt > 0 {
		c.LastValidatedAt = time.UnixMilli(lastValidatedAt)
	}
	c.CreatedAt = time.UnixMilli(createdAt)
	c.UpdatedAt = time.UnixMilli(updatedAt)
	return &c, nil
}

// Set 写入或更新凭证（覆盖式 upsert）
//
// 写后立即清缓存，确保下次 Get 拿到新值（热加载关键）。
// last_status 重置为 unknown，由后续 ValidateCredentials 探活更新。
func (s *BinanceCredentialsStore) Set(label, p20t, csrfToken string) error {
	if label == "" {
		label = BinanceCredsLabelDefault
	}
	p20t = strings.TrimSpace(p20t)
	csrfToken = strings.TrimSpace(csrfToken)
	if p20t == "" || csrfToken == "" {
		return fmt.Errorf("p20t and csrfToken are required (label=%s)", label)
	}

	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		INSERT INTO binance_credentials (label, p20t, csrf_token, last_status, last_error, created_at, updated_at)
		VALUES (?, ?, ?, 'unknown', '', ?, ?)
		ON CONFLICT(label) DO UPDATE SET
			p20t = excluded.p20t,
			csrf_token = excluded.csrf_token,
			last_status = 'unknown',
			last_error = '',
			updated_at = excluded.updated_at
	`, label, p20t, csrfToken, now, now)
	if err != nil {
		return err
	}

	// 失效缓存 → 下次 Get 强制重新从 DB 读
	s.mu.Lock()
	delete(s.cache, label)
	s.mu.Unlock()

	logger.Infof("🔑 Binance global credentials saved | label=%s p20t=%s csrf=%s",
		label, maskSecret(p20t), maskSecret(csrfToken))
	return nil
}

// UpdateStatus 更新校验状态（探活后调用）
//
// 不修改 p20t / csrftoken；仅更新状态、错误、绑定的 userID、校验时间。
func (s *BinanceCredentialsStore) UpdateStatus(label, status, errMsg, userID string) error {
	if label == "" {
		label = BinanceCredsLabelDefault
	}
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		UPDATE binance_credentials SET
			last_status = ?,
			last_error = ?,
			binance_user_id = COALESCE(NULLIF(?, ''), binance_user_id),
			last_validated_at = ?,
			updated_at = ?
		WHERE label = ?
	`, status, errMsg, userID, now, now, label)
	if err != nil {
		return err
	}

	// 失效缓存
	s.mu.Lock()
	delete(s.cache, label)
	s.mu.Unlock()

	return nil
}

// List 列出所有 label 的凭证（用于 API 列表展示，输出时调用方需脱敏）
func (s *BinanceCredentialsStore) List() ([]*BinanceCredentials, error) {
	rows, err := s.db.Query(`
		SELECT label, p20t, csrf_token, binance_user_id,
		       last_validated_at, last_status, last_error,
		       created_at, updated_at
		FROM binance_credentials ORDER BY label
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var creds []*BinanceCredentials
	for rows.Next() {
		var c BinanceCredentials
		var lastValidatedAt, createdAt, updatedAt int64
		if err := rows.Scan(
			&c.Label, &c.P20T, &c.CSRFToken, &c.BinanceUserID,
			&lastValidatedAt, &c.LastStatus, &c.LastError,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, err
		}
		if lastValidatedAt > 0 {
			c.LastValidatedAt = time.UnixMilli(lastValidatedAt)
		}
		c.CreatedAt = time.UnixMilli(createdAt)
		c.UpdatedAt = time.UnixMilli(updatedAt)
		creds = append(creds, &c)
	}
	return creds, rows.Err()
}

// Delete 删除指定 label 的凭证
func (s *BinanceCredentialsStore) Delete(label string) error {
	if label == "" {
		label = BinanceCredsLabelDefault
	}
	if _, err := s.db.Exec(`DELETE FROM binance_credentials WHERE label = ?`, label); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.cache, label)
	s.mu.Unlock()

	return nil
}

// LoadBinanceCredentials 实现 copytrade.BinanceCredentialsLoader 接口
//
// 接口约定：
//   - 凭证不存在时返回 ("", "", nil)，由调用方按"未配置"语义处理
//   - DB 异常时返回 ("", "", error)
//
// BinanceProvider 通过这个接口在每次 HTTP 调用前现读最新凭证，
// 实现"前端更新凭证后无需重启即生效"。
func (s *BinanceCredentialsStore) LoadBinanceCredentials(label string) (string, string, error) {
	creds, err := s.Get(label)
	if err != nil {
		return "", "", err
	}
	if creds == nil {
		return "", "", nil
	}
	return creds.P20T, creds.CSRFToken, nil
}

// ============================================================================
// 迁移
// ============================================================================

// MigrateFromCopyTradeConfigs 首次升级时，从 copy_trade_configs 表迁移最新一份
// Binance trader 凭证到全局存储。
//
// 触发条件：
//   - 全局 default 凭证不存在或为空
//   - 存在至少一个 binance trader 配了非空凭证
//
// 选择策略：updated_at 最新的 binance trader（凭证最可能仍有效）。
//
// 返回 (是否实际迁移, error)。
//
// 设计原则：
//   - 迁移后旧字段保留不动（保守修改原则，避免数据丢失）
//   - 后续读取以全局凭证为准，旧字段作为最低优先级降级
func (s *BinanceCredentialsStore) MigrateFromCopyTradeConfigs() (bool, error) {
	// 全局已配置 → 跳过
	existing, err := s.Get(BinanceCredsLabelDefault)
	if err != nil {
		return false, fmt.Errorf("check existing global creds: %w", err)
	}
	if existing != nil && strings.TrimSpace(existing.P20T) != "" && strings.TrimSpace(existing.CSRFToken) != "" {
		return false, nil
	}

	// 找最新 binance trader 凭证
	var p20t, csrf string
	err = s.db.QueryRow(`
		SELECT COALESCE(binance_p20t, ''), COALESCE(binance_csrf_token, '')
		FROM copy_trade_configs
		WHERE provider_type = 'binance'
		  AND COALESCE(binance_p20t, '') != ''
		  AND COALESCE(binance_csrf_token, '') != ''
		ORDER BY updated_at DESC
		LIMIT 1
	`).Scan(&p20t, &csrf)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("scan latest binance trader creds: %w", err)
	}

	p20t = strings.TrimSpace(p20t)
	csrf = strings.TrimSpace(csrf)
	if p20t == "" || csrf == "" {
		return false, nil
	}

	if err := s.Set(BinanceCredsLabelDefault, p20t, csrf); err != nil {
		return false, fmt.Errorf("save migrated creds: %w", err)
	}

	logger.Infof("🔄 Binance credentials migrated from copy_trade_configs to global store | label=default")
	return true, nil
}

// ============================================================================
// 辅助
// ============================================================================

// CountBinanceCopyTraderIDs 返回当前所有 provider_type=binance 的 trader_id 列表
// 用于"哪些 trader 受全局凭证影响"的展示与告警 body
func (s *BinanceCredentialsStore) CountBinanceCopyTraderIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT trader_id FROM copy_trade_configs WHERE provider_type = 'binance' ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
