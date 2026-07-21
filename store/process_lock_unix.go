//go:build unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type processLock struct {
	file *os.File
}

func acquireProcessLock(dbPath string, timeout time.Duration) (*processLock, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	lockPath := absPath + ".process.lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0660)
	if err != nil {
		return nil, err
	}
	// Administrators commonly build or smoke-run NOFX as root while the panel
	// service runs as www. If root creates the lock first, preserve the database
	// owner's uid/gid so the normal service account can open it on the next
	// restart instead of failing with EACCES.
	if os.Geteuid() == 0 {
		if info, statErr := os.Stat(absPath); statErr == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				_ = file.Chown(int(stat.Uid), int(stat.Gid))
				_ = file.Chmod(0660)
			}
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_, _ = file.Seek(0, 0)
			_ = file.Truncate(0)
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			return &processLock{file: file}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = file.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("another NOFX process still owns %s after %s", absPath, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (l *processLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
