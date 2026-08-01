package logger

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotatingFileWriterRotatesBySizeAndDateWithoutDeletingArchives(t *testing.T) {
	dir := t.TempDir()
	w, err := newRotatingFileWriter(dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	current := time.Date(2026, 8, 1, 10, 0, 0, 0, time.Local)
	w.now = func() time.Time { return current }
	// Reopen under the deterministic date used by the test.
	if err = w.rotate(); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("size")); err != nil {
		t.Fatal(err)
	}
	current = current.Add(24 * time.Hour)
	if _, err = w.Write([]byte("next-day")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	archives, err := filepath.Glob(filepath.Join(dir, "*.gz"))
	if err != nil || len(archives) < 2 {
		t.Fatalf("size and date archives missing: files=%v err=%v", archives, err)
	}
	var contents []string
	for _, archive := range archives {
		file, openErr := os.Open(archive)
		if openErr != nil {
			t.Fatal(openErr)
		}
		reader, gzipErr := gzip.NewReader(file)
		if gzipErr != nil {
			t.Fatal(gzipErr)
		}
		raw, readErr := io.ReadAll(reader)
		_ = reader.Close()
		_ = file.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		contents = append(contents, string(raw))
	}
	joined := strings.Join(contents, "|")
	if !strings.Contains(joined, "12345678") || !strings.Contains(joined, "size") {
		t.Fatalf("rotated history was lost: %q", joined)
	}
}

func TestRotatingFileWriterNeverOverwritesExistingArchive(t *testing.T) {
	dir := t.TempDir()
	w, err := newRotatingFileWriter(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	current := time.Date(2026, 8, 1, 10, 0, 0, 0, time.Local)
	w.now = func() time.Time { return current }
	if err = w.rotate(); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "nofx_2026-08-01.100000.001.log.gz")
	if err = os.WriteFile(existing, []byte("keep-me"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("next")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(existing)
	if err != nil || string(raw) != "keep-me" {
		t.Fatalf("existing archive was overwritten: %q err=%v", raw, err)
	}
}
