package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCopyTradeListEnabledReleasesRowsBeforePolicyHydration(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "list-enabled.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	seed := func(id, provider, sourceMode string, enabled bool, minNotional float64) {
		t.Helper()
		if _, err := st.DB().Exec(`INSERT INTO traders(id, name, ai_model_id, exchange_id, initial_balance) VALUES(?,?,?,?,100)`, id, id, "model", "exchange"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`
			INSERT INTO copy_trade_configs(trader_id, provider_type, leader_id, enabled, binance_source_mode)
			VALUES(?,?,?,?,?)`, id, provider, "leader-"+id, enabled, sourceMode); err != nil {
			t.Fatal(err)
		}
		policy, err := json.Marshal(CopyGuardPolicy{DefaultsVersion: CopyGuardDefaultsVersion(), ReentryMinNotional: minNotional})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO copy_guard_policies(trader_id, policy_json) VALUES(?,?)`, id, string(policy)); err != nil {
			t.Fatal(err)
		}
	}
	seed("smart-a", "binance", "smart_money", true, 12)
	seed("smart-b", "BINANCE", "SMART_MONEY", true, 20)
	seed("copy-management", "binance", "copy_management", true, 30)
	seed("disabled", "binance", "smart_money", false, 40)

	type result struct {
		configs []*CopyTradeConfig
		err     error
	}
	resultCh := make(chan result, 1)
	started := time.Now()
	go func() {
		configs, err := st.CopyTrade().ListEnabled()
		resultCh <- result{configs: configs, err: err}
	}()
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Fatalf("ListEnabled took %s; single-connection hydration must finish within one second", elapsed)
		}
		if len(got.configs) != 3 {
			t.Fatalf("enabled configs=%d, want 3", len(got.configs))
		}
		minimums := map[string]float64{}
		for _, config := range got.configs {
			minimums[config.TraderID] = config.RiskReentryMinNotional
		}
		if !reflect.DeepEqual(minimums, map[string]float64{"smart-a": 12, "smart-b": 20, "copy-management": 30}) {
			t.Fatalf("policies were not hydrated after materialization: %v", minimums)
		}
	case <-time.After(time.Second):
		t.Fatal("ListEnabled deadlocked while loading Copy Guard policies with MaxOpenConns(1)")
	}

	ids, err := st.CopyTrade().ListEnabledTraderIDsBySource("binance", "smart_money")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"smart-a", "smart-b"}) {
		t.Fatalf("incident member IDs=%v, want [smart-a smart-b]", ids)
	}
}
