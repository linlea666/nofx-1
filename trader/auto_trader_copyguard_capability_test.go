package trader

import "testing"

func TestAutoTraderCopyGuardCapabilityValidationInspectsUnderlyingExchange(t *testing.T) {
	if err := (&AutoTrader{trader: &AsterTrader{}}).ValidateCopyGuardCapabilities(); err == nil {
		t.Fatal("wrapper methods must not make an incapable underlying exchange look Copy Guard-capable")
	}
	if err := (&AutoTrader{trader: &FuturesTrader{}}).ValidateCopyGuardCapabilities(); err != nil {
		t.Fatalf("Binance futures should expose the complete Copy Guard contract: %v", err)
	}
	if err := (&AutoTrader{trader: &OKXTrader{}}).ValidateCopyGuardCapabilities(); err != nil {
		t.Fatalf("OKX should expose the complete Copy Guard contract: %v", err)
	}
}
