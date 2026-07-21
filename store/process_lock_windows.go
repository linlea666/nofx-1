//go:build windows

package store

import "time"

// NOFX production targets Unix. Keep Windows source compatibility without
// pretending that the Unix advisory-lock guarantee exists there.
type processLock struct{}

func acquireProcessLock(_ string, _ time.Duration) (*processLock, error) { return &processLock{}, nil }
func (l *processLock) Close() error                                      { return nil }
