package store

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestLegacyDatabaseMigration opens a disposable copy of a real legacy
// database when NOFX_LEGACY_DB is provided. It is skipped in ordinary CI so no
// private production data is required, while release validation can exercise
// the complete Store startup migration against an operational schema.
func TestLegacyDatabaseMigration(t *testing.T) {
	source := os.Getenv("NOFX_LEGACY_DB")
	if source == "" {
		t.Skip("NOFX_LEGACY_DB is not set")
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	target := filepath.Join(t.TempDir(), "legacy-copy.db")
	output, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(output, input); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err = output.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := New(target)
	if err != nil {
		t.Fatalf("legacy database migration startup failed: %v", err)
	}
	defer st.Close()

	for _, check := range []struct {
		table  string
		column string
	}{
		{"copy_trade_configs", "copy_catchup_window_seconds"},
		{"copy_trade_configs", "copy_catchup_max_adverse_bps"},
		{"copy_trade_execution_intents", "filled_quantity"},
		{"copy_guard_reentry_candidates", "decision_expires_at"},
		{"copy_guard_reentry_candidates", "regular_review_at"},
		{"copy_guard_reentry_candidates", "event_review_at"},
		{"copy_guard_reentry_candidates", "budget_blocked_until"},
		{"trader_positions", "accounting_quality"},
		{"position_close_allocations", "exit_price"},
	} {
		if !sqliteColumnExists(t, st.db, check.table, check.column) {
			t.Fatalf("migration did not create %s.%s", check.table, check.column)
		}
	}
	for _, table := range []string{"copy_trade_execution_fill_commits", "position_close_allocations"} {
		var found int
		if err = st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&found); err != nil || found != 1 {
			t.Fatalf("migration did not create %s: found=%d err=%v", table, found, err)
		}
	}
}

func sqliteColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue interface{}
		if err = rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}
