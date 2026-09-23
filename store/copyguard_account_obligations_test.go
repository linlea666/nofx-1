package store

import "testing"

type accountProtectionFixture struct {
	name, status, previousAlgo, previousClient string
	cycleOwner                                 string
	cycleMissing, cycleOpen, replacement       bool
	currentMissing                             bool
	blocked                                    bool
}

func accountProtectionCases() []accountProtectionFixture {
	return []accountProtectionFixture{
		{name: "closed invalid history", status: "invalid"},
		{name: "case insensitive invalid", status: "INVALID"},
		{name: "known cancellation remains terminal", status: "canceled"},
		{name: "live remains blocked", status: "live", blocked: true},
		{name: "unknown remains blocked", status: "unknown", blocked: true},
		{name: "empty state remains blocked", status: "", blocked: true},
		{name: "invalid active cycle", status: "invalid", cycleOpen: true, blocked: true},
		{name: "invalid missing cycle", status: "invalid", cycleMissing: true, blocked: true},
		{name: "invalid foreign cycle", status: "invalid", cycleOwner: "another-trader", blocked: true},
		{name: "invalid replacement pending", status: "invalid", replacement: true, blocked: true},
		{name: "invalid retiring algo", status: "invalid", previousAlgo: "previous-order", blocked: true},
		{name: "invalid retiring client", status: "invalid", previousClient: "previous-client", blocked: true},
		{name: "previous-only invalid algo", status: "invalid", currentMissing: true, previousAlgo: "previous-order", blocked: true},
		{name: "previous-only invalid client", status: "invalid", currentMissing: true, previousClient: "previous-client", blocked: true},
		{name: "canceled current retains previous algo", status: "canceled", previousAlgo: "previous-order", blocked: true},
		{name: "previous-only canceled client", status: "canceled", currentMissing: true, previousClient: "previous-client", blocked: true},
	}
}

func seedAccountProtection(t *testing.T, st *Store, traderID string, fixture accountProtectionFixture) {
	t.Helper()
	if !fixture.cycleMissing {
		owner := fixture.cycleOwner
		if owner == "" {
			owner = traderID
		}
		var closedAt interface{} = "2026-08-01 00:00:00"
		if fixture.cycleOpen {
			closedAt = nil
		}
		resolutionExec(t, st, `INSERT INTO copy_guard_cycles
		 (id,trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,closed_at,accounting_status)
		 VALUES(1,?,'leader','position','ETHUSDT','long','cross','LEADER_CLOSED',?,'RECONCILED')`, owner, closedAt)
	}
	algoID, clientID := "historical-order", "historical-client"
	if fixture.currentMissing {
		algoID, clientID = "", ""
	}
	if err := st.CopyTrade().UpsertCopyGuardProtectiveOrder(&CopyGuardProtectiveOrder{
		CycleID: 1, TraderID: traderID, AlgoID: algoID, AlgoClientID: clientID,
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", Quantity: 1,
		Status: fixture.status, PreviousAlgoID: fixture.previousAlgo, PreviousAlgoClientID: fixture.previousClient,
		ReplacementPending: fixture.replacement,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAccountStartHandlesHistoricalInvalidProtection(t *testing.T) {
	for _, oldStatus := range []string{TraderLifecycleStopped, TraderLifecycleArchived} {
		for _, fixture := range accountProtectionCases() {
			t.Run(oldStatus+"/"+fixture.name, func(t *testing.T) {
				st := resolutionStore(t)
				createLifecycleTestTrader(t, st, "old", oldStatus, 1)
				createLifecycleTestTrader(t, st, "new", TraderLifecycleStarting, 2)
				seedAccountProtection(t, st, "old", fixture)
				before, err := st.CopyTrade().GetCopyGuardProtectiveOrder(1)
				if err != nil {
					t.Fatal(err)
				}
				err = st.Trader().CompleteCopyGuardStart("user-1", "new", 2, "exchange-1")
				if (err != nil) != fixture.blocked {
					t.Fatalf("blocked=%v, want %v: %v", err != nil, fixture.blocked, err)
				}
				state, err := st.Trader().GetLifecycle("new")
				if err != nil || state.IsRunning == fixture.blocked {
					t.Fatalf("unexpected start state: %+v %v", state, err)
				}
				preserved, err := st.CopyTrade().GetCopyGuardProtectiveOrder(1)
				if err != nil || *preserved != *before {
					t.Fatalf("account check changed protection history: %+v %v", preserved, err)
				}
			})
		}
	}
}

func TestAccountSwitchHandlesHistoricalInvalidProtection(t *testing.T) {
	for _, fixture := range accountProtectionCases() {
		t.Run(fixture.name, func(t *testing.T) {
			st := resolutionStore(t)
			createLifecycleTestTrader(t, st, "old", TraderLifecycleRunning, 1)
			if err := st.Trader().ClaimRunningCopyAccount("old", 1, "exchange-1"); err != nil {
				t.Fatal(err)
			}
			seedAccountProtection(t, st, "old", fixture)
			resolutionExec(t, st, `UPDATE traders SET exchange_id='exchange-2' WHERE id='old'`)
			err := st.Trader().ClaimRunningCopyAccount("old", 1, "exchange-2")
			if (err != nil) != fixture.blocked {
				t.Fatalf("blocked=%v, want %v: %v", err != nil, fixture.blocked, err)
			}
			var claimed string
			if err = st.DB().QueryRow(`SELECT exchange_id FROM copy_trade_account_claims WHERE trader_id='old'`).Scan(&claimed); err != nil {
				t.Fatal(err)
			}
			want := "exchange-2"
			if fixture.blocked {
				want = "exchange-1"
			}
			if claimed != want {
				t.Fatalf("account claim=%s, want %s", claimed, want)
			}
		})
	}
}
