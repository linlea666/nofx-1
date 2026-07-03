package copytrade

import (
	"testing"
	"time"
)

func TestProtectionCoverageAndRetrySchedule(t *testing.T) {
	if got := protectionCoverage(8, 10); got != 0.8 {
		t.Fatalf("under coverage = %v", got)
	}
	if got := protectionRatio(12, 10); got != 1.2 {
		t.Fatalf("over coverage ratio = %v", got)
	}
	if got := protectionCoverage(12, 10); got != 1 {
		t.Fatalf("display coverage must cap at 100%%, got %v", got)
	}
	want := []time.Duration{0, time.Second, 3 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute}
	for i, expected := range want {
		if got := protectionRetryDelay(i); got != expected {
			t.Fatalf("retry %d = %v, want %v", i, got, expected)
		}
	}
}
