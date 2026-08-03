package copytrade

import "testing"

func TestCloseInvalidationBreachedFollowsPositionSide(t *testing.T) {
	for _, tc := range []struct {
		name       string
		side       string
		closePrice float64
		level      float64
		wantBreach bool
	}{
		{"long closes below the level", "long", 97, 98, true},
		{"long closes exactly at the level", "long", 98, 98, false},
		{"long closes above the level", "long", 99, 98, false},
		{"short closes above the level", "short", 103, 102, true},
		{"short closes exactly at the level", "short", 102, 102, false},
		{"short closes below the level", "short", 101, 102, false},
		{"uppercase side is still honoured", "SHORT", 103, 102, true},
		// An unset level must never read as breached, otherwise every filled
		// reentry without a structured condition would report an invalidation.
		{"no level configured", "long", 97, 0, false},
		{"no close price", "long", 0, 98, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := closeInvalidationBreached(tc.side, tc.closePrice, tc.level); got != tc.wantBreach {
				t.Fatalf("closeInvalidationBreached(%q, %v, %v) = %v, want %v",
					tc.side, tc.closePrice, tc.level, got, tc.wantBreach)
			}
		})
	}
}
