package store

import (
	"path/filepath"
	"testing"
)

func TestCopyGuardAccountPolicyDefaultAndTraderOverride(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	exchangeID, err := st.Exchange().Create(
		"user-1", "okx", "main", true, "", "", "", false,
		"", "", "", "", "", "", "", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Trader().Create(&Trader{
		ID: "trader-1", UserID: "user-1", Name: "copy", AIModelID: "ai",
		ExchangeID: exchangeID, InitialBalance: 100,
	}); err != nil {
		t.Fatal(err)
	}

	inherited, err := st.CopyTrade().EffectiveCopyGuardStopPolicy("trader-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if inherited.MaxPositionLossPct != 0.10 || inherited.Source != "account_default" {
		t.Fatalf("unexpected inherited policy: %+v", inherited)
	}
	if err := st.CopyTrade().UpsertCopyGuardAccountPolicy(exchangeID, 0.20); err != nil {
		t.Fatal(err)
	}
	account, err := st.CopyTrade().EffectiveCopyGuardStopPolicy("trader-1", 0)
	if err != nil || account.MaxPositionLossPct != 0.20 {
		t.Fatalf("account policy=%+v err=%v", account, err)
	}
	override, err := st.CopyTrade().EffectiveCopyGuardStopPolicy("trader-1", 0.30)
	if err != nil || override.MaxPositionLossPct != 0.30 || override.Source != "trader_override" {
		t.Fatalf("override policy=%+v err=%v", override, err)
	}
	if err := st.CopyTrade().UpsertCopyGuardAccountPolicy(exchangeID, 0.31); err == nil {
		t.Fatal("policy above 30% must be rejected")
	}
}

func TestCopyGuardLifecycleSnapshotFreezesInheritedAccountPolicy(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "lifecycle-policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	exchangeID, err := st.Exchange().Create(
		"user-1", "okx", "main", true, "", "", "", false,
		"", "", "", "", "", "", "", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Trader().Create(&Trader{
		ID: "trader-1", UserID: "user-1", Name: "copy", AIModelID: "ai",
		ExchangeID: exchangeID, InitialBalance: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().UpsertCopyGuardAccountPolicy(exchangeID, 0.20); err != nil {
		t.Fatal(err)
	}

	config := NewCopyGuardDefaults()
	config.TraderID = "trader-1"
	config.RiskStopMaxAccountLossPct = 0
	snapshot, err := st.CopyTrade().EncodeCopyGuardLifecyclePolicySnapshot(config)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DecodeCopyGuardPolicySnapshot(snapshot)
	if err != nil || policy.StopMaxAccountLossPct != 0.20 {
		t.Fatalf("inherited policy was not frozen: policy=%+v err=%v", policy, err)
	}

	if err = st.CopyTrade().UpsertCopyGuardAccountPolicy(exchangeID, 0.10); err != nil {
		t.Fatal(err)
	}
	policy, err = DecodeCopyGuardPolicySnapshot(snapshot)
	if err != nil || policy.StopMaxAccountLossPct != 0.20 {
		t.Fatalf("existing lifecycle leaked a later account-policy change: policy=%+v err=%v", policy, err)
	}

	config.RiskProtectionMode = RiskProtectionModePositionMarginPct
	fixedSnapshot, err := st.CopyTrade().EncodeCopyGuardLifecyclePolicySnapshot(config)
	if err != nil {
		t.Fatal(err)
	}
	fixedPolicy, err := DecodeCopyGuardPolicySnapshot(fixedSnapshot)
	if err != nil || fixedPolicy.StopMaxAccountLossPct != 0 {
		t.Fatalf("fixed mode unexpectedly consumed the account stop: policy=%+v err=%v", fixedPolicy, err)
	}
}
