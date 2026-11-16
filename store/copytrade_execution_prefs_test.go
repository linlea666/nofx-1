package store

import "testing"

func TestExecutionPrefsRoundTripAndPartialUpdates(t *testing.T) {
	s := resolutionStore(t)
	createLifecycleTestTrader(t, s, "t", TraderLifecycleStopped, 0)
	c := &CopyTradeConfig{TraderID: "t", ProviderType: "hyperliquid", LeaderID: "0xleader", CopyRatio: 1, SourceGeneration: 1}
	if err := s.CopyTrade().Create(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.CopyTrade().GetByTraderID("t")
	if err != nil {
		t.Fatal(err)
	}
	if got.CopyPriceToleranceMajorPct != nil || got.CopyPriceToleranceAltPct != nil || got.CopyTakeProfitMaxWaitSeconds != nil ||
		got.CopyAutoCorrectOverweight == nil || !*got.CopyAutoCorrectOverweight {
		t.Fatalf("defaults wrong: %+v", got)
	}

	major, alt, wait, off := 0.1, 0.0, 120, false
	got.CopyPriceToleranceMajorPct, got.CopyPriceToleranceAltPct, got.CopyTakeProfitMaxWaitSeconds, got.CopyAutoCorrectOverweight = &major, &alt, &wait, &off
	if err = s.CopyTrade().Update(got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.CopyTrade().GetByTraderID("t")
	if *got.CopyPriceToleranceMajorPct != 0.1 || *got.CopyPriceToleranceAltPct != 0 || *got.CopyTakeProfitMaxWaitSeconds != 120 || *got.CopyAutoCorrectOverweight {
		t.Fatalf("prefs not saved (0 tolerance must persist): %+v", got)
	}

	// An older client that omits the fields keeps the stored values.
	got.CopyPriceToleranceMajorPct, got.CopyPriceToleranceAltPct, got.CopyTakeProfitMaxWaitSeconds, got.CopyAutoCorrectOverweight = nil, nil, nil, nil
	if err = s.CopyTrade().Update(got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.CopyTrade().GetByTraderID("t")
	if got.CopyPriceToleranceMajorPct == nil || *got.CopyPriceToleranceMajorPct != 0.1 || *got.CopyAutoCorrectOverweight {
		t.Fatalf("omitted prefs were reset: %+v", got)
	}

	bad := 6.0
	got.CopyPriceToleranceAltPct = &bad
	if err = s.CopyTrade().Update(got); err == nil {
		t.Fatal("out-of-range tolerance accepted")
	}
}
