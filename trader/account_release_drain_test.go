package trader

import (
	"testing"
	"time"
)

func TestDeletionWaitsForAlreadyStoppingRuntime(t *testing.T) {
	// Another stop request has already cleared isRunning, but a worker still
	// has an outstanding request. A flat snapshot must wait for its completion.
	at := &AutoTrader{}
	at.monitorWg.Add(1)
	done := make(chan struct{})
	go func() { at.StopAndWait(); close(done) }()
	select {
	case <-done:
		at.monitorWg.Done()
		t.Fatal("deletion raced the outstanding runtime request")
	case <-time.After(20 * time.Millisecond):
	}
	at.monitorWg.Done()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deletion did not resume after runtime drain")
	}
}
