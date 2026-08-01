package logger

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// rotatingFileWriter keeps one active file per calendar day and additionally
// rotates at maxBytes. Archives are gzip-compressed and intentionally have no
// retention deletion policy: operators retain the complete history.
type rotatingFileWriter struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	now      func() time.Time
	file     *os.File
	date     string
	size     int64
	sequence int
}

func newRotatingFileWriter(dir string, maxBytes int64) (*rotatingFileWriter, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("log max bytes must be positive")
	}
	w := &rotatingFileWriter{dir: dir, maxBytes: maxBytes, now: time.Now}
	if err := w.openActive(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingFileWriter) activePath(date string) string {
	return filepath.Join(w.dir, "nofx_"+date+".log")
}

func (w *rotatingFileWriter) openActive() error {
	now := w.now()
	date := now.Format("2006-01-02")
	path := w.activePath(date)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	w.file = file
	w.date = date
	w.size = info.Size()
	return nil
}

func (w *rotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		if err := w.openActive(); err != nil {
			return 0, err
		}
	}
	today := w.now().Format("2006-01-02")
	if today != w.date || w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingFileWriter) rotate() error {
	oldDate := w.date
	oldPath := w.activePath(oldDate)
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	if w.size > 0 {
		stamp := w.now().Format("150405")
		archive := ""
		for {
			w.sequence++
			candidate := filepath.Join(w.dir, fmt.Sprintf("nofx_%s.%s.%03d.log", oldDate, stamp, w.sequence))
			_, rawErr := os.Stat(candidate)
			_, gzipErr := os.Stat(candidate + ".gz")
			if os.IsNotExist(rawErr) && os.IsNotExist(gzipErr) {
				archive = candidate
				break
			}
		}
		if err := os.Rename(oldPath, archive); err != nil {
			return err
		}
		if err := gzipFile(archive); err != nil {
			// Keep the uncompressed archive when compression fails. No history is
			// discarded merely because disk compression was unavailable.
			return err
		}
	}
	return w.openActive()
}

func gzipFile(path string) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	targetPath := path + ".gz"
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(target)
	_, copyErr := io.Copy(gz, source)
	closeGzipErr := gz.Close()
	closeTargetErr := target.Close()
	if copyErr != nil || closeGzipErr != nil || closeTargetErr != nil {
		_ = os.Remove(targetPath)
		if copyErr != nil {
			return copyErr
		}
		if closeGzipErr != nil {
			return closeGzipErr
		}
		return closeTargetErr
	}
	return os.Remove(path)
}

func (w *rotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
