package copytrade

import (
	"testing"

	"nofx/decision"
)

// A skipped add on an active mapping must advance that mapping; booking it
// as an ignored new position fails and replays the same revision forever.
func TestSkippedAddAdvancesActiveMapping(t *testing.T) {
	e := reduceTestEngine(t, ProviderHyperliquid, 1000)
	dec := decision.Decision{Action: "open_long", Symbol: "SOLUSDT", LeaderPosID: "p", CopyTradeAction: string(ActionAdd), MarginMode: "cross", LeaderPosSize: 90}
	if !e.reserveExecutionIntent(&dec) || dec.ExecutionIntentID <= 0 {
		t.Fatalf("add not reserved: %+v", dec)
	}
	e.commitSkippedReservedIntent(&dec, "PROPORTIONAL_TARGET_REACHED")
	var status, reason string
	if err := e.store.DB().QueryRow(`SELECT status,reason_code FROM copy_trade_execution_intents WHERE id=?`, dec.ExecutionIntentID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "SKIPPED" || reason != "PROPORTIONAL_TARGET_REACHED" {
		t.Fatalf("skipped add not acknowledged: %s %s", status, reason)
	}
	var revision int64
	var size float64
	var mapping string
	if err := e.store.DB().QueryRow(`SELECT source_revision,last_known_size,status FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id='p'`, e.traderID).Scan(&revision, &size, &mapping); err != nil {
		t.Fatal(err)
	}
	if revision != dec.SourceRevision || size != 90 || mapping != "active" {
		t.Fatalf("mapping not advanced: rev=%d/%d size=%v status=%s", revision, dec.SourceRevision, size, mapping)
	}
}
