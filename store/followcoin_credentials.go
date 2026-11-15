// Package store followcoin_credentials.go
// 全局 FollowCoin 登录 token 存储（多个 FollowCoin 跟单 trader 共用一份）。
//
// token 是 FollowCoin App 的登录 JWT，会过期。与 Binance 凭证同样采用
// "内存缓存 + DB 持久化"，写入即清缓存，前端更新后无需重启即对所有
// FollowCoin 跟单 trader 生效。
package store

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"nofx/logger"
)

const (
	FollowCoinCredsLabelDefault = "default"

	FollowCoinCredsStatusValid   = "valid"
	FollowCoinCredsStatusExpired = "expired"
	FollowCoinCredsStatusUnknown = "unknown"
	FollowCoinCredsStatusError   = "error"
)

// FollowCoinCredentials 全局 FollowCoin 凭证
type FollowCoinCredentials struct {
	Label           string    `json:"label"`
	Token           string    `json:"-"`
	ExpiresAt       time.Time `json:"expires_at"` // 从 JWT exp 解析；零值表示无法解析
	LastValidatedAt time.Time `json:"last_validated_at"`
	LastStatus      string    `json:"last_status"`
	LastError       string    `json:"last_error"`
	// 告警簿记：失效告警与到期预警各自记录最近一次发信时间，
	// 跨进程重启保持"首次立即、之后按间隔提醒"的节奏。
	LastFailureAlertAt time.Time `json:"-"`
	LastExpiryWarnAt   time.Time `json:"-"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (c *FollowCoinCredentials) MaskedToken() string { return maskSecret(c.Token) }

// ParseJWTExpiry 读取 JWT payload 的 exp（秒）。不校验签名，只用于到期提醒。
func ParseJWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return time.Time{}, false
	}
	exp, err := claims.Exp.Int64()
	if err != nil || exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(exp, 0), true
}

type FollowCoinCredentialsStore struct {
	db    *sql.DB
	mu    sync.RWMutex
	cache map[string]*FollowCoinCredentials
}

func (s *FollowCoinCredentialsStore) initTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS followcoin_credentials (
			label TEXT PRIMARY KEY DEFAULT 'default',
			token TEXT NOT NULL DEFAULT '',
			expires_at INTEGER NOT NULL DEFAULT 0,
			last_validated_at INTEGER NOT NULL DEFAULT 0,
			last_status TEXT NOT NULL DEFAULT 'unknown',
			last_error TEXT NOT NULL DEFAULT '',
			last_failure_alert_at INTEGER NOT NULL DEFAULT 0,
			last_expiry_warn_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`)
	return err
}

func normalizeFollowCoinLabel(label string) string {
	if strings.TrimSpace(label) == "" {
		return FollowCoinCredsLabelDefault
	}
	return label
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func timeToMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// Get 返回指定 label 的凭证；未配置返回 (nil, nil)。
func (s *FollowCoinCredentialsStore) Get(label string) (*FollowCoinCredentials, error) {
	label = normalizeFollowCoinLabel(label)
	s.mu.RLock()
	if c, ok := s.cache[label]; ok {
		s.mu.RUnlock()
		copy := *c
		return &copy, nil
	}
	s.mu.RUnlock()

	var c FollowCoinCredentials
	var expiresAt, validatedAt, failureAlertAt, expiryWarnAt, createdAt, updatedAt int64
	err := s.db.QueryRow(`
		SELECT label, token, expires_at, last_validated_at, last_status, last_error,
		       last_failure_alert_at, last_expiry_warn_at, created_at, updated_at
		FROM followcoin_credentials WHERE label = ?`, label).Scan(
		&c.Label, &c.Token, &expiresAt, &validatedAt, &c.LastStatus, &c.LastError,
		&failureAlertAt, &expiryWarnAt, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.ExpiresAt, c.LastValidatedAt = msToTime(expiresAt), msToTime(validatedAt)
	c.LastFailureAlertAt, c.LastExpiryWarnAt = msToTime(failureAlertAt), msToTime(expiryWarnAt)
	c.CreatedAt, c.UpdatedAt = msToTime(createdAt), msToTime(updatedAt)

	s.mu.Lock()
	if s.cache == nil {
		s.cache = make(map[string]*FollowCoinCredentials)
	}
	cached := c
	s.cache[label] = &cached
	s.mu.Unlock()
	return &c, nil
}

func (s *FollowCoinCredentialsStore) invalidate(label string) {
	s.mu.Lock()
	delete(s.cache, label)
	s.mu.Unlock()
}

// Set 覆盖保存 token。状态重置为 unknown，告警簿记清零（新 token 重新计算）。
func (s *FollowCoinCredentialsStore) Set(label, token string) error {
	label = normalizeFollowCoinLabel(label)
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("token is required (label=%s)", label)
	}
	var expiresAt int64
	if exp, ok := ParseJWTExpiry(token); ok {
		expiresAt = exp.UnixMilli()
	}
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		INSERT INTO followcoin_credentials (label, token, expires_at, last_status, last_error,
			last_failure_alert_at, last_expiry_warn_at, created_at, updated_at)
		VALUES (?, ?, ?, 'unknown', '', 0, 0, ?, ?)
		ON CONFLICT(label) DO UPDATE SET
			token = excluded.token, expires_at = excluded.expires_at,
			last_status = 'unknown', last_error = '',
			last_failure_alert_at = 0, last_expiry_warn_at = 0,
			updated_at = excluded.updated_at`,
		label, token, expiresAt, now, now)
	if err != nil {
		return err
	}
	s.invalidate(label)
	logger.Infof("🔑 FollowCoin credentials saved | label=%s token=%s", label, maskSecret(token))
	return nil
}

// UpdateStatus 记录一次校验/运行时观测结果。valid 同时刷新 last_validated_at。
func (s *FollowCoinCredentialsStore) UpdateStatus(label, status, errMsg string) error {
	label = normalizeFollowCoinLabel(label)
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		UPDATE followcoin_credentials SET
			last_status = ?, last_error = ?,
			last_validated_at = CASE WHEN ? = 'valid' THEN ? ELSE last_validated_at END,
			updated_at = ?
		WHERE label = ?`, status, errMsg, status, now, now, label)
	if err != nil {
		return err
	}
	s.invalidate(label)
	return nil
}

// MarkFailureAlert 记录失效告警发信时间；零值表示已恢复（清零）。
func (s *FollowCoinCredentialsStore) MarkFailureAlert(label string, at time.Time) error {
	label = normalizeFollowCoinLabel(label)
	if _, err := s.db.Exec(`UPDATE followcoin_credentials SET last_failure_alert_at=? WHERE label=?`, timeToMS(at), label); err != nil {
		return err
	}
	s.invalidate(label)
	return nil
}

// MarkExpiryWarn 记录到期预警发信时间。
func (s *FollowCoinCredentialsStore) MarkExpiryWarn(label string, at time.Time) error {
	label = normalizeFollowCoinLabel(label)
	if _, err := s.db.Exec(`UPDATE followcoin_credentials SET last_expiry_warn_at=? WHERE label=?`, timeToMS(at), label); err != nil {
		return err
	}
	s.invalidate(label)
	return nil
}

func (s *FollowCoinCredentialsStore) Delete(label string) error {
	label = normalizeFollowCoinLabel(label)
	if _, err := s.db.Exec(`DELETE FROM followcoin_credentials WHERE label = ?`, label); err != nil {
		return err
	}
	s.invalidate(label)
	return nil
}

// LoadFollowCoinToken 实现 copytrade.FollowCoinTokenLoader：未配置返回 ("", nil)。
func (s *FollowCoinCredentialsStore) LoadFollowCoinToken(label string) (string, error) {
	c, err := s.Get(label)
	if err != nil || c == nil {
		return "", err
	}
	return c.Token, nil
}

// ListFollowCoinCopyTraderIDs 列出 FollowCoin 数据源的跟单 trader（告警正文用）。
func (s *FollowCoinCredentialsStore) ListFollowCoinCopyTraderIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT trader_id FROM copy_trade_configs WHERE provider_type = 'followcoin' ORDER BY trader_id`)
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
