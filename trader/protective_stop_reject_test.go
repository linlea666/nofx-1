package trader

import (
	"errors"
	"fmt"
	"testing"
)

// A trigger the venue considers already crossed is a deterministic rejection.
// Misclassifying it as transient costs the full retry budget — ten attempts
// with backoff — while the position carries no stop at all.
func TestIsProtectiveStopAlreadyTriggerable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "OKX 51280 verbatim",
			err:  fmt.Errorf("OKX place algo rejected: algoId= sCode=51280 sMsg=SL trigger price must be less than the last price"),
			want: true,
		},
		{
			name: "OKX 51280 short side wording",
			err:  errors.New("sCode=51280 sMsg=Stop loss trigger price should be higher than the last price"),
			want: true,
		},
		{
			name: "Binance -2021 by code",
			err:  errors.New(`binance api error: code=-2021 msg="Order would immediately trigger."`),
			want: true,
		},
		{
			name: "Binance -2021 by message only",
			err:  errors.New("Order would immediately trigger."),
			want: true,
		},
		{
			name: "wrapped cause is still recognized",
			err:  fmt.Errorf("place protective stop: %w", errors.New("sCode=51280 trigger price invalid vs last price")),
			want: true,
		},
		// The bare number must not be enough: quantities, prices and order ids
		// routinely contain these digits, and a false positive would abandon a
		// retry that would have succeeded.
		{
			name: "unrelated error containing the digits",
			err:  errors.New("insufficient margin for size 51280"),
			want: false,
		},
		{name: "other venue rejection", err: errors.New("sCode=51008 insufficient balance"), want: false},
		{name: "nil", err: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProtectiveStopAlreadyTriggerable(tc.err); got != tc.want {
				t.Fatalf("classification = %v, want %v for %v", got, tc.want, tc.err)
			}
		})
	}
}
