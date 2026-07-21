//go:build unix

package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestProcessLockRejectsConcurrentOwnerAndReleases(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "single-owner.db")
	first, err := acquireProcessLock(dbPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireProcessLock(dbPath, 50*time.Millisecond); err == nil {
		t.Fatal("second owner unexpectedly acquired the same database lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireProcessLock(dbPath, time.Second)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	_ = second.Close()
}
