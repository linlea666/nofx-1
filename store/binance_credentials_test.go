package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func newCredsTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "creds-test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestBinanceCredentialsCRUD 验证 Set / Get / List / Delete 全流程
func TestBinanceCredentialsCRUD(t *testing.T) {
	st := newCredsTestStore(t)
	cs := st.BinanceCreds()

	// 1) 初始无数据
	if got, _ := cs.Get(""); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}

	// 2) Set + Get
	if err := cs.Set("", "p20t-value-abcdef0123456789", "csrf-value-xxxx"); err != nil {
		t.Fatalf("set: %v", err)
	}
	c, err := cs.Get("")
	if err != nil || c == nil {
		t.Fatalf("get: %v / %+v", err, c)
	}
	if c.Label != BinanceCredsLabelDefault {
		t.Fatalf("label=%s want default", c.Label)
	}
	if c.P20T != "p20t-value-abcdef0123456789" {
		t.Fatalf("p20t mismatch: %s", c.P20T)
	}
	if c.LastStatus != BinanceCredsStatusUnknown {
		t.Fatalf("status=%s want unknown after Set", c.LastStatus)
	}

	// 3) Update overwrites
	if err := cs.Set("default", "new-p20t-1234567890abcdef", "new-csrf-token-value"); err != nil {
		t.Fatalf("set update: %v", err)
	}
	c2, _ := cs.Get("")
	if c2.P20T != "new-p20t-1234567890abcdef" {
		t.Fatalf("after update p20t mismatch: %s", c2.P20T)
	}

	// 4) UpdateStatus
	if err := cs.UpdateStatus("", BinanceCredsStatusValid, "", "user-id-12345"); err != nil {
		t.Fatalf("update status: %v", err)
	}
	c3, _ := cs.Get("")
	if c3.LastStatus != BinanceCredsStatusValid {
		t.Fatalf("status=%s want valid", c3.LastStatus)
	}
	if c3.BinanceUserID != "user-id-12345" {
		t.Fatalf("userid=%s want user-id-12345", c3.BinanceUserID)
	}
	if c3.LastValidatedAt.IsZero() {
		t.Fatalf("LastValidatedAt should be set")
	}

	// 5) List
	list, err := cs.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len=%d want 1", len(list))
	}

	// 6) Delete
	if err := cs.Delete(""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := cs.Get(""); got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}
}

// TestBinanceCredentialsSetRequiresBothFields 校验空值拒绝
func TestBinanceCredentialsSetRequiresBothFields(t *testing.T) {
	st := newCredsTestStore(t)
	cs := st.BinanceCreds()

	cases := []struct {
		p20t, csrf string
	}{
		{"", "csrf"},
		{"p20t", ""},
		{"   ", "csrf"},
		{"p20t", "  "},
	}
	for _, tc := range cases {
		if err := cs.Set("", tc.p20t, tc.csrf); err == nil {
			t.Fatalf("Set(%q, %q) should fail but got nil", tc.p20t, tc.csrf)
		}
	}
}

// TestBinanceCredentialsHotReload 验证 Set 后内存缓存立即失效，下次 Get 拿到新值
//
// 这是热加载的核心保证：用户在前端更新凭证后，所有运行中的 Binance Provider
// 调用 LoadBinanceCredentials() 会立即拿到新值（无需重启）。
func TestBinanceCredentialsHotReload(t *testing.T) {
	st := newCredsTestStore(t)
	cs := st.BinanceCreds()

	if err := cs.Set("", "old-p20t-value-aaaaaaaaaaaaaaaa", "old-csrf-aaaaaaaaaa"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	// 触发缓存填充
	first, _, _ := cs.LoadBinanceCredentials("")
	if first != "old-p20t-value-aaaaaaaaaaaaaaaa" {
		t.Fatalf("first load: %s", first)
	}

	// 写入新值
	if err := cs.Set("", "new-p20t-value-bbbbbbbbbbbbbbbb", "new-csrf-bbbbbbbbbb"); err != nil {
		t.Fatalf("second set: %v", err)
	}

	// 再次 Load 应拿到新值（缓存已被 Set 失效）
	second, _, _ := cs.LoadBinanceCredentials("")
	if second != "new-p20t-value-bbbbbbbbbbbbbbbb" {
		t.Fatalf("hot reload failed: got %s want new-p20t-value-bbbbbbbbbbbbbbbb", second)
	}
}

// TestBinanceCredentialsLoaderInterface 验证作为接口实现的契约
//
// 关键约定：未配置 → ("", "", nil) 而非 error，让调用方区分"未配置"vs"DB 异常"
func TestBinanceCredentialsLoaderInterface(t *testing.T) {
	st := newCredsTestStore(t)
	cs := st.BinanceCreds()

	// 未配置场景
	p20t, csrf, err := cs.LoadBinanceCredentials("")
	if err != nil {
		t.Fatalf("unconfigured should not return error, got: %v", err)
	}
	if p20t != "" || csrf != "" {
		t.Fatalf("unconfigured should return empty, got %s/%s", p20t, csrf)
	}

	// 配置后
	_ = cs.Set("", "p20t-1234567890abcdef", "csrf-aaaaaaaaa")
	p20t, csrf, err = cs.LoadBinanceCredentials("")
	if err != nil {
		t.Fatalf("LoadBinanceCredentials: %v", err)
	}
	if p20t == "" || csrf == "" {
		t.Fatalf("expected non-empty creds, got %q / %q", p20t, csrf)
	}
}

// TestBinanceCredentialsMaskedOutput 验证 API 输出脱敏正确性
func TestBinanceCredentialsMaskedOutput(t *testing.T) {
	c := &BinanceCredentials{
		P20T:      "web.1133692136.4F5BA4684C8FF8DB046658A1C8864568",
		CSRFToken: "744018a7cff08c9b98234a9d3eb33db5",
	}
	masked := c.MaskedP20T()
	if !strings.HasPrefix(masked, "web.11") || !strings.HasSuffix(masked, "4568") {
		t.Fatalf("p20t masked unexpected: %s", masked)
	}
	if strings.Contains(masked, "4F5BA468") {
		t.Fatalf("p20t leaked sensitive content: %s", masked)
	}
	if c.MaskedCSRFToken() == c.CSRFToken {
		t.Fatalf("csrf not masked")
	}

	// 短串完全遮蔽
	short := &BinanceCredentials{P20T: "short", CSRFToken: "x"}
	if short.MaskedP20T() != "***" {
		t.Fatalf("short p20t should be ***, got %s", short.MaskedP20T())
	}

	// 空值不遮蔽（空字符串原样返回，前端用于显示"未配置"）
	empty := &BinanceCredentials{}
	if empty.MaskedP20T() != "" {
		t.Fatalf("empty should remain empty, got %s", empty.MaskedP20T())
	}
}

// TestBinanceCredentialsMigration 验证从 copy_trade_configs 迁移到全局
func TestBinanceCredentialsMigration(t *testing.T) {
	st := newCredsTestStore(t)
	cs := st.BinanceCreds()

	// 1) 没有任何 binance trader 时，迁移什么都不做
	migrated, err := cs.MigrateFromCopyTradeConfigs()
	if err != nil {
		t.Fatalf("migrate empty: %v", err)
	}
	if migrated {
		t.Fatalf("should not migrate when no source")
	}

	// 2) 创建一个 binance trader（通过 traders + copy_trade_configs 表）
	if _, err := st.db.Exec(`
		INSERT INTO traders (id, name, ai_model_id, exchange_id, initial_balance)
		VALUES ('test-trader', 'Test', 'mock-model', 'mock-exchange', 100.0)
	`); err != nil {
		t.Fatalf("insert trader: %v", err)
	}
	if _, err := st.db.Exec(`
		INSERT INTO copy_trade_configs (trader_id, provider_type, leader_id, copy_ratio, enabled,
		    binance_p20t, binance_csrf_token, created_at, updated_at)
		VALUES ('test-trader', 'binance', 'lead-id-x', 1.0, 1, 'migrated-p20t-1234567890', 'migrated-csrf-aaaaaaaa',
		    CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`); err != nil {
		t.Fatalf("insert copy_trade_config: %v", err)
	}

	// 3) 迁移应成功
	migrated, err = cs.MigrateFromCopyTradeConfigs()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !migrated {
		t.Fatalf("expected migrated=true")
	}

	c, _ := cs.Get("")
	if c == nil || c.P20T != "migrated-p20t-1234567890" {
		t.Fatalf("migrated creds wrong: %+v", c)
	}

	// 4) 二次迁移应跳过（全局已有 → 不覆盖）
	migrated2, _ := cs.MigrateFromCopyTradeConfigs()
	if migrated2 {
		t.Fatalf("second migrate should be no-op")
	}
}

// TestBinanceCredentialsAffectedTraders 验证 CountBinanceCopyTraderIDs
func TestBinanceCredentialsAffectedTraders(t *testing.T) {
	st := newCredsTestStore(t)
	cs := st.BinanceCreds()

	ids, err := cs.CountBinanceCopyTraderIDs()
	if err != nil {
		t.Fatalf("count empty: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("empty count should be 0, got %d", len(ids))
	}

	// 创建 1 个 binance trader + 1 个 OKX trader（OKX 不应被计入）
	if _, err := st.db.Exec(`INSERT INTO traders (id, name, ai_model_id, exchange_id, initial_balance) VALUES ('t-binance', 'B', 'm', 'e', 100)`); err != nil {
		t.Fatalf("insert binance trader: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO traders (id, name, ai_model_id, exchange_id, initial_balance) VALUES ('t-okx', 'O', 'm', 'e', 100)`); err != nil {
		t.Fatalf("insert okx trader: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO copy_trade_configs (trader_id, provider_type, leader_id, copy_ratio, enabled, created_at, updated_at)
		VALUES ('t-binance', 'binance', 'lid', 1.0, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert binance config: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO copy_trade_configs (trader_id, provider_type, leader_id, copy_ratio, enabled, created_at, updated_at)
		VALUES ('t-okx', 'okx', 'oid', 1.0, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert okx config: %v", err)
	}

	ids, err = cs.CountBinanceCopyTraderIDs()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(ids) != 1 || ids[0] != "t-binance" {
		t.Fatalf("expected only binance trader, got %v", ids)
	}
}
