package api

import (
	"nofx/store"
	"path/filepath"
	"testing"
	"time"
)

func TestCopyGuardSaveReadbackDetectsDroppedFixedMode(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "readback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.DB().Exec(`INSERT INTO traders(id,name,ai_model_id,exchange_id,initial_balance) VALUES('t','trader','','exchange',1000)`); err != nil {
		t.Fatal(err)
	}
	c := store.NewCopyGuardDefaults()
	c.TraderID = "t"
	c.ProviderType = "okx"
	c.LeaderID = "l"
	c.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	store.NormalizeCopyGuardProtectionModeTransition(c, nil)
	if err = st.CopyTrade().Upsert(c); err != nil {
		t.Fatal(err)
	}
	if err = verifyCopyGuardConfigReadback(st, c); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_guard_policies SET policy_json=json_set(policy_json,'$.risk_protection_mode','atr_structure') WHERE trader_id='t'`); err != nil {
		t.Fatal(err)
	}
	if err = verifyCopyGuardConfigReadback(st, c); err == nil {
		t.Fatal("silent mode drop was reported as a successful save")
	}
	cap := copyGuardRuntimeCapabilities()
	if cap["shadow_runtime_enabled"] != false || cap["fixed_initial_margin"] != true {
		t.Fatalf("wrong capabilities: %+v", cap)
	}
}

func TestPositionMarginAuditShowsOldVenueStopAfterFailedTightening(t *testing.T) {
	p := store.NewCopyGuardDefaults()
	p.RiskProtectionMode = store.RiskProtectionModePositionMarginPct
	snapshot, err := store.EncodeCopyGuardPolicySnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cycle := &store.CopyGuardCycle{PolicySnapshot: snapshot, Side: "long", AccountEquity: 1000, ProtectionStatus: store.CopyGuardProtectionDegraded}
	artifacts := &copyGuardCycleArtifacts{Attempts: []*store.CopyGuardAttempt{{AttemptNo: 0, EntryPrice: 100, Quantity: 2, ActualLeverage: 10, StopAnchorEntryPrice: 100, StopAnchorLeverage: 10, StopAnchorInitialMargin: 10, StopAnchorPrice: 92, StopConfiguredMarginLossPct: .8, StopAnchorSource: store.CopyGuardAnchorSourceInitialFill, FinalStopPrice: 95, CurrentMarkPrice: 99, CurrentMarkAt: &now}}, Protection: &store.CopyGuardProtectiveOrder{AlgoID: "old", TriggerType: "mark", TriggerPrice: 92, Status: "live", UpdatedAt: now}}
	a := buildPositionMarginAudit(cycle, artifacts)
	if a == nil || a.EffectiveStopPrice != 92 || a.DesiredStopPrice != 95 || a.CurrentStopRiskUSD != 16 || a.DataQuality != "PARTIAL" {
		t.Fatalf("uninstalled desired stop displayed as effective: %+v", a)
	}
	artifacts.Protection = nil
	a = buildPositionMarginAudit(cycle, artifacts)
	if a.EffectiveStopPrice != 0 || a.CurrentStopRiskUSD != 0 || a.CurrentMargin != 20 {
		t.Fatalf("absent stop invented risk/erased position margin: %+v", a)
	}
}
