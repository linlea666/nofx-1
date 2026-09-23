package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func legacyRetirementFixture(t *testing.T) (*Store, int64, StoppedTraderFlatSnapshot) {
	t.Helper()
	s := resolutionStore(t)
	createLifecycleTestTrader(t, s, "old", TraderLifecycleStopped, 3)
	resolutionMapping(t, s, "old", "ended", MappingStatusClosed, 1, 95)
	i := resolutionIntent(t, s, "old", "ended", "close_long", ExecutionIntentFilled, 1, 0)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET reason_code='MAPPING_ALREADY_ACKNOWLEDGED',
 submitted_at='2026-01-02 01:00:00',filled_at='2026-01-03 01:00:00',terminal_at=NULL,exchange_state='',exchange_order_id='',
 filled_quantity=0,filled_notional=0 WHERE id=?`, i.ID)
	return s, i.ID, StoppedTraderFlatSnapshot{ExchangeID: "exchange-1", TraderGeneration: 3, ObservedAt: time.Now(), PositionsEmpty: true, OrdersEmpty: true}
}

func retirementCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_venue_retirements`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLegacyVenueRetirementKeepsOriginalExecutionEvidence(t *testing.T) {
	s, id, snapshot := legacyRetirementFixture(t)
	original, err := s.CopyTrade().GetExecutionIntentByID(id)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(original)
	if _, err = s.CopyTrade().RetireStoppedTraderCopyGuardState("old", "a description alone is not proof"); err == nil {
		t.Fatal("legacy history retired without typed exchange snapshot")
	}
	for n := 0; n < 2; n++ {
		if _, err = s.CopyTrade().RetireStoppedTraderCopyGuardStateWithSnapshot("old", "fresh flat-account evidence", snapshot); err != nil {
			t.Fatal(err)
		}
	}
	afterIntent, _ := s.CopyTrade().GetExecutionIntentByID(id)
	after, _ := json.Marshal(afterIntent)
	if string(before) != string(after) {
		t.Fatalf("retirement fabricated or rewrote historical execution\nbefore=%s\nafter=%s", before, after)
	}
	var account, audit, evidence string
	if err = s.DB().QueryRow(`SELECT exchange_id,before_json,evidence FROM copy_trade_venue_retirements WHERE intent_id=?`, id).Scan(&account, &audit, &evidence); err != nil {
		t.Fatal(err)
	}
	if retirementCount(t, s) != 1 || account != snapshot.ExchangeID || audit != string(before) || evidence != "fresh flat-account evidence" {
		t.Fatal("retirement evidence missing or duplicated")
	}
	createLifecycleTestTrader(t, s, "new", TraderLifecycleStarting, 1)
	if err = s.Trader().CompleteCopyGuardStart("user-1", "new", 1, "exchange-1"); err != nil {
		t.Fatalf("historical risk still owns the account: %v", err)
	}
}

func TestLegacyVenueRetirementRequiresExactFreshStoppedScope(t *testing.T) {
	cases := map[string]func(*Store, int64, *StoppedTraderFlatSnapshot){
		"stale":      func(_ *Store, _ int64, p *StoppedTraderFlatSnapshot) { p.ObservedAt = time.Now().Add(-2 * time.Minute) },
		"future":     func(_ *Store, _ int64, p *StoppedTraderFlatSnapshot) { p.ObservedAt = time.Now().Add(time.Minute) },
		"no-time":    func(_ *Store, _ int64, p *StoppedTraderFlatSnapshot) { p.ObservedAt = time.Time{} },
		"positions":  func(_ *Store, _ int64, p *StoppedTraderFlatSnapshot) { p.PositionsEmpty = false },
		"orders":     func(_ *Store, _ int64, p *StoppedTraderFlatSnapshot) { p.OrdersEmpty = false },
		"account":    func(_ *Store, _ int64, p *StoppedTraderFlatSnapshot) { p.ExchangeID = "other" },
		"generation": func(_ *Store, _ int64, p *StoppedTraderFlatSnapshot) { p.TraderGeneration++ },
		"draining": func(s *Store, _ int64, _ *StoppedTraderFlatSnapshot) {
			resolutionExec(t, s, `UPDATE traders SET lifecycle_status='STOPPING' WHERE id='old'`)
		},
		"running": func(s *Store, _ int64, _ *StoppedTraderFlatSnapshot) {
			resolutionExec(t, s, `UPDATE traders SET lifecycle_status='RUNNING',is_running=1 WHERE id='old'`)
		},
		"peer-starting": func(s *Store, _ int64, _ *StoppedTraderFlatSnapshot) {
			createLifecycleTestTrader(t, s, "peer", TraderLifecycleStarting, 1)
		},
		"peer-draining": func(s *Store, _ int64, _ *StoppedTraderFlatSnapshot) {
			createLifecycleTestTrader(t, s, "peer", TraderLifecycleStoppingReconcileRequired, 1)
		},
		"unknown-order": func(s *Store, id int64, _ *StoppedTraderFlatSnapshot) {
			resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET status='SUBMITTED' WHERE id=?`, id)
		},
		"real-order-id": func(s *Store, id int64, _ *StoppedTraderFlatSnapshot) {
			resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET exchange_order_id='venue-order' WHERE id=?`, id)
		},
		"real-fill": func(s *Store, id int64, _ *StoppedTraderFlatSnapshot) {
			resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET filled_quantity=1 WHERE id=?`, id)
		},
		"active-source": func(s *Store, _ int64, _ *StoppedTraderFlatSnapshot) {
			resolutionExec(t, s, `UPDATE copy_trade_position_mappings SET status='active' WHERE trader_id='old'`)
		},
		"attempt": func(s *Store, id int64, _ *StoppedTraderFlatSnapshot) {
			resolutionExec(t, s, `INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,submitted_at) VALUES(?,1,'uncertain','UNKNOWN',CURRENT_TIMESTAMP)`, id)
		},
		"local-position": func(s *Store, _ int64, _ *StoppedTraderFlatSnapshot) {
			resolutionExec(t, s, `INSERT INTO trader_positions(trader_id,exchange_id,symbol,side,quantity,entry_price,entry_time,status) VALUES('old','exchange-1','ETHUSDT','LONG',1,100,CURRENT_TIMESTAMP,'OPEN')`)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s, id, snapshot := legacyRetirementFixture(t)
			change(s, id, &snapshot)
			if _, err := s.CopyTrade().RetireStoppedTraderCopyGuardStateWithSnapshot("old", "fresh flat evidence", snapshot); err == nil {
				t.Fatal("unproven or active risk was retired")
			}
			if retirementCount(t, s) != 0 {
				t.Fatal("failed retirement left an account-release audit")
			}
		})
	}
}

func TestLegacyVenueRetirementRollsBackWithOtherUnknownOrders(t *testing.T) {
	s, _, snapshot := legacyRetirementFixture(t)
	unknown := resolutionIntent(t, s, "old", "unknown", "close_short", ExecutionIntentSubmitted, 1, 0)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET submitted_at=CURRENT_TIMESTAMP WHERE id=?`, unknown.ID)
	if _, err := s.CopyTrade().RetireStoppedTraderCopyGuardStateWithSnapshot("old", "fresh flat evidence", snapshot); err == nil {
		t.Fatal("modern unknown order accepted")
	}
	if retirementCount(t, s) != 0 {
		t.Fatal("partial audit committed before uncertainty check")
	}
}

func TestLegacyVenueRetirementCannotUseNewAccountSnapshotForPreviousClaim(t *testing.T) {
	s, id, snapshot := legacyRetirementFixture(t)
	resolutionExec(t, s, `UPDATE traders SET lifecycle_status='RUNNING',is_running=1 WHERE id='old'`)
	if err := s.Trader().ClaimRunningCopyAccount("old", 3, "exchange-1"); err != nil {
		t.Fatal(err)
	}
	// Editing while stopped does not move the durable claim or the old order.
	resolutionExec(t, s, `UPDATE traders SET lifecycle_status='STOPPED',is_running=0,exchange_id='exchange-2' WHERE id='old'`)
	snapshot.ExchangeID = "exchange-2"
	if _, err := s.CopyTrade().RetireStoppedTraderCopyGuardStateWithSnapshot("old", "new account is flat", snapshot); err == nil {
		t.Fatal("new account snapshot cleared previous account obligations")
	}
	if retirementCount(t, s) != 0 {
		t.Fatal("wrong-account evidence was committed")
	}
	resolutionExec(t, s, `UPDATE traders SET lifecycle_status='STARTING' WHERE id='old'`)
	if err := s.Trader().CompleteCopyGuardStart("user-1", "old", 3, "exchange-2"); err == nil {
		t.Fatal("unverified previous account was released")
	}
	// Restoring the original account and obtaining its own proof can retire it.
	resolutionExec(t, s, `UPDATE traders SET lifecycle_status='STOPPED',exchange_id='exchange-1' WHERE id='old'`)
	snapshot.ExchangeID = "exchange-1"
	if _, err := s.CopyTrade().RetireStoppedTraderCopyGuardStateWithSnapshot("old", "original account is flat", snapshot); err != nil {
		t.Fatal(err)
	}
	var auditAccount string
	if err := s.DB().QueryRow(`SELECT exchange_id FROM copy_trade_venue_retirements WHERE intent_id=?`, id).Scan(&auditAccount); err != nil || auditAccount != "exchange-1" {
		t.Fatalf("retirement account=%q err=%v", auditAccount, err)
	}
}

func TestLegacyVenueRetirementDoesNotHideLaterEvidence(t *testing.T) {
	for _, change := range []string{
		`UPDATE copy_trade_execution_intents SET exchange_order_id='new-evidence' WHERE id=?`,
		`UPDATE copy_trade_execution_intents SET updated_at='2099-01-01' WHERE id=?`,
		`INSERT INTO copy_trade_execution_order_attempts(intent_id,attempt_no,client_order_id,status,submitted_at) VALUES(?,1,'late-attempt','UNKNOWN',CURRENT_TIMESTAMP)`,
	} {
		t.Run(change, func(t *testing.T) {
			s, id, snapshot := legacyRetirementFixture(t)
			if _, err := s.CopyTrade().RetireStoppedTraderCopyGuardStateWithSnapshot("old", "flat", snapshot); err != nil {
				t.Fatal(err)
			}
			resolutionExec(t, s, change, id)
			createLifecycleTestTrader(t, s, "new", TraderLifecycleStarting, 1)
			if err := s.Trader().CompleteCopyGuardStart("user-1", "new", 1, "exchange-1"); err == nil {
				t.Fatal("retirement swallowed new order evidence")
			}
		})
	}
}

func TestLegacyVenueRetirementSurvivesRestartAndAccountChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retirement.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	createLifecycleTestTrader(t, s, "old", TraderLifecycleRunning, 1)
	if err = s.Trader().ClaimRunningCopyAccount("old", 1, "exchange-1"); err != nil {
		t.Fatal(err)
	}
	resolutionExec(t, s, `UPDATE traders SET lifecycle_status='STOPPED',is_running=0 WHERE id='old'`)
	resolutionMapping(t, s, "old", "ended", MappingStatusClosed, 1, 10)
	i := resolutionIntent(t, s, "old", "ended", "close_long", ExecutionIntentFilled, 1, 0)
	resolutionExec(t, s, `UPDATE copy_trade_execution_intents SET reason_code='MAPPING_ALREADY_ACKNOWLEDGED',submitted_at=CURRENT_TIMESTAMP,terminal_at=NULL WHERE id=?`, i.ID)
	if _, err = s.CopyTrade().RetireStoppedTraderCopyGuardStateWithSnapshot("old", "flat", StoppedTraderFlatSnapshot{ExchangeID: "exchange-1", TraderGeneration: 1, ObservedAt: time.Now(), PositionsEmpty: true, OrdersEmpty: true}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resolutionExec(t, s, `UPDATE traders SET exchange_id='exchange-2',lifecycle_status='STARTING',lifecycle_generation=2 WHERE id='old'`)
	if err = s.Trader().CompleteCopyGuardStart("user-1", "old", 2, "exchange-2"); err != nil {
		t.Fatalf("settled previous-account risk blocked account change: %v", err)
	}
	if retirementCount(t, s) != 1 {
		t.Fatal("restart lost or duplicated audit")
	}
}
