package copytrade

import (
	"nofx/trader"
	"time"
)

// Production AutoTrader always supplies exchange timestamps. A failed/stale
// observation cannot fall back to last price or a time-less cached position.
func (ti *TraderIntegration) fixedStopReferencePrice(symbol string, positionMark float64) (float64, string) {
	m := ti.fixedStopMarkObservation(symbol, positionMark)
	return m.Price, m.Source
}

func (ti *TraderIntegration) fixedStopMarkObservation(symbol string, positionMark float64) trader.MarkPriceObservation {
	if provider, ok := ti.executor.(trader.TimedMarkPriceProvider); ok {
		m, err := provider.GetMarkPriceObservation(symbol)
		if err == nil && m.ValidAt(time.Now()) {
			return m
		}
		return trader.MarkPriceObservation{}
	}
	// Compatibility for embedded executors which already supplied a fresh
	// position response. Live Binance/OKX wrappers never enter this branch.
	price, source := ti.stopTriggerReferencePriceForType(symbol, positionMark, "mark")
	return trader.MarkPriceObservation{Price: price, Source: source, ObservedAt: time.Now()}
}
