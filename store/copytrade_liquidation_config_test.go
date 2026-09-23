package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestIndependentLiquidationGuardConfigAndSnapshotSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	if _, err = st.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance) VALUES('t','t','m','e',0)`); err != nil {
		t.Fatal(err)
	}
	cfg := NewCopyGuardDefaults()
	if !cfg.RiskLiquidationGuardEnabled || cfg.FollowExitPolicyVersion != 2 {
		t.Fatal("new default contract missing")
	}
	cfg.TraderID, cfg.ProviderType, cfg.LeaderID = "t", "okx", "leader"
	cfg.RiskStopLossEnabled = false
	for _, enabled := range []bool{true, false} {
		cfg.RiskLiquidationGuardEnabled = enabled
		if err = st.CopyTrade().Upsert(cfg); err != nil {
			t.Fatal(err)
		}
		st.Close()
		st, err = New(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.CopyTrade().GetByTraderID("t")
		if err != nil || got.RiskStopLossEnabled || got.RiskLiquidationGuardEnabled != enabled || got.FollowExitPolicyVersion != 2 {
			t.Fatalf("independent toggle changed on restart: %+v %v", got, err)
		}
		raw, err := EncodeCopyGuardPolicySnapshot(got)
		if err != nil {
			t.Fatal(err)
		}
		var policy CopyGuardPolicy
		if err = json.Unmarshal([]byte(raw), &policy); err != nil {
			t.Fatal(err)
		}
		if policy.StrategyStopEnabled == nil || *policy.StrategyStopEnabled || policy.LiquidationGuardEnabled == nil || *policy.LiquidationGuardEnabled != enabled {
			t.Fatalf("immutable policy lost explicit toggle: %+v", policy)
		}
	}
}
