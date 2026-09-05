package trader

import (
	"fmt"
	"math"
	"time"
)

type MarkPriceObservation struct {
	Price      float64
	ObservedAt time.Time
	Source     string
}

type TimedMarkPriceProvider interface {
	GetMarkPriceObservation(symbol string) (MarkPriceObservation, error)
}

func (m MarkPriceObservation) ValidAt(now time.Time) bool {
	age := now.Sub(m.ObservedAt)
	return m.Price > 0 && !math.IsNaN(m.Price) && !math.IsInf(m.Price, 0) && !m.ObservedAt.IsZero() && age >= -5*time.Second && age <= 45*time.Second
}

func (at *AutoTrader) GetMarkPriceObservation(symbol string) (MarkPriceObservation, error) {
	if provider, ok := at.trader.(TimedMarkPriceProvider); ok {
		return provider.GetMarkPriceObservation(symbol)
	}
	return MarkPriceObservation{}, fmt.Errorf("timestamped exchange mark is unavailable for %s", symbol)
}

func (t *OKXTrader) GetMarkPrice(symbol string) (float64, error) {
	m, err := t.GetMarkPriceObservation(symbol)
	return m.Price, err
}

func (t *FuturesTrader) GetMarkPrice(symbol string) (float64, error) {
	m, err := t.GetMarkPriceObservation(symbol)
	return m.Price, err
}
