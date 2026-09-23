package store

import (
	"path/filepath"
	"testing"
)

func TestSettlementDiagnosticsRemainSeparateScopedAndBounded(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "diagnostics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := PositionSettlementDiagnostic{ExchangeID: "account-a", PositionID: "reused-pos", Symbol: "ZECUSDT", Side: "long", MarginMode: "isolated", OpenedMS: 1790022269524, Stage: "LIFECYCLE_PROOF", ReasonCode: "OPENING_FILLS_INCOMPLETE", Detail: "first order evidence missing"}
	for i := 0; i < 3; i++ {
		if err = st.Position().RecordSettlementDiagnostic(d); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.Position().ListSettlementDiagnostics("account-a", false, 10)
	if err != nil || len(rows) != 1 || rows[0].Attempts != 3 || rows[0].Status != "PENDING" {
		t.Fatalf("retries grew rows or disappeared: %+v %v", rows, err)
	}
	identity := rows[0].Identity
	other, err := st.Position().ListSettlementDiagnostics("account-b", true, 10)
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-account diagnostic: %+v %v", other, err)
	}
	if err = st.Position().ResolveSettlementDiagnostic(d); err != nil {
		t.Fatal(err)
	}
	rows, err = st.Position().ListSettlementDiagnostics("account-a", false, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("resolved still pending: %+v %v", rows, err)
	}
	rows, err = st.Position().ListSettlementDiagnostics("account-a", true, 10)
	if err != nil || len(rows) != 1 || rows[0].Identity != identity || rows[0].ResolvedAt == "" {
		t.Fatalf("audit/identity missing: %+v %v", rows, err)
	}
	// A later lifecycle may reuse the native position ID but cannot overwrite
	// the historical diagnostic or its evidence of resolution.
	d.OpenedMS += 60000
	if err = st.Position().RecordSettlementDiagnostic(d); err != nil {
		t.Fatal(err)
	}
	rows, err = st.Position().ListSettlementDiagnostics("account-a", true, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("reused posId merged: %+v %v", rows, err)
	}
}
