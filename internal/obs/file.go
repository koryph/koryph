// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// telemetryDirPath returns the canonical telemetry directory path.
// It uses KORYPH_HOME for test isolation, mirroring configFilePath.
func telemetryDirPath() string {
	home := os.Getenv("KORYPH_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err == nil {
			home = filepath.Join(h, ".koryph")
		} else {
			home = ".koryph"
		}
	}
	return filepath.Join(home, "telemetry")
}

// fileWriter writes JSONL records to a rotating set of files in dir.
// Files are named koryph-YYYYMMDD.jsonl for the active daily file.
// When the file exceeds maxSizeBytes a mid-day rotation occurs: the full
// file is renamed to koryph-YYYYMMDD-HHmmssZ.jsonl and a fresh
// koryph-YYYYMMDD.jsonl is started.
//
// fileWriter is goroutine-safe via mu.
type fileWriter struct {
	dir          string
	maxSizeBytes int64
	mu           sync.Mutex
	current      *os.File
	currentSize  int64
	currentDate  string // "YYYYMMDD" of the active file
}

// newFileWriter constructs a fileWriter for dir.
// maxSizeBytes ≤ 0 defaults to 50 MiB.
func newFileWriter(dir string, maxSizeBytes int64) *fileWriter {
	if maxSizeBytes <= 0 {
		maxSizeBytes = 50 * 1024 * 1024
	}
	return &fileWriter{
		dir:          dir,
		maxSizeBytes: maxSizeBytes,
	}
}

// Write appends p to the current telemetry file, rotating as needed.
// Write implements io.Writer so it can back a slog.JSONHandler.
func (fw *fileWriter) Write(p []byte) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	if err := fw.ensureOpen(); err != nil {
		// Silently drop the record if we cannot open the file — telemetry
		// is best-effort and must never crash the main process.
		return len(p), nil
	}

	n, err := fw.current.Write(p)
	fw.currentSize += int64(n)
	return n, err
}

// ensureOpen opens or rotates the current file.  Must be called with fw.mu held.
func (fw *fileWriter) ensureOpen() error {
	now := time.Now().UTC()
	today := now.Format("20060102")

	// Rotate on date change.
	if fw.current != nil && fw.currentDate != today {
		_ = fw.current.Close()
		fw.current = nil
		fw.currentSize = 0
	}

	// Rotate on size cap.
	if fw.current != nil && fw.currentSize >= fw.maxSizeBytes {
		stamp := now.Format("150405")
		// E.g. koryph-20260704.jsonl → koryph-20260704-123456.jsonl
		oldName := filepath.Join(fw.dir, "koryph-"+fw.currentDate+".jsonl")
		newName := filepath.Join(fw.dir, "koryph-"+fw.currentDate+"-"+stamp+".jsonl")
		_ = fw.current.Close()
		fw.current = nil
		fw.currentSize = 0
		_ = os.Rename(oldName, newName) // best-effort; if it fails we overwrite
	}

	if fw.current != nil {
		return nil
	}

	// Ensure the directory exists.
	if err := os.MkdirAll(fw.dir, 0o700); err != nil {
		return fmt.Errorf("obs file: mkdir %s: %w", fw.dir, err)
	}

	fw.currentDate = today
	path := filepath.Join(fw.dir, "koryph-"+today+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("obs file: open %s: %w", path, err)
	}

	fi, err := f.Stat()
	if err == nil {
		fw.currentSize = fi.Size()
	}
	fw.current = f
	return nil
}

// Close closes the current open file.
func (fw *fileWriter) Close() error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.current != nil {
		err := fw.current.Close()
		fw.current = nil
		fw.currentSize = 0
		return err
	}
	return nil
}

// fileJSONHandler is a slog.Handler that writes JSON records to a fileWriter.
// It stores a min-level so that Enabled() can gate before Write is called.
type fileJSONHandler struct {
	w   *fileWriter
	h   slog.Handler // inner slog.JSONHandler backed by fw
	min slog.Level
}

// newFileJSONHandler creates a fileJSONHandler for the telemetry directory dir.
// maxSizeBytes ≤ 0 defaults to 50 MiB.
func newFileJSONHandler(dir string, maxSizeBytes int64, min slog.Level) *fileJSONHandler {
	fw := newFileWriter(dir, maxSizeBytes)
	inner := slog.NewJSONHandler(fw, &slog.HandlerOptions{
		Level:       min,
		ReplaceAttr: levelReplacer,
	})
	return &fileJSONHandler{w: fw, h: inner, min: min}
}

func (f *fileJSONHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= f.min
}

func (f *fileJSONHandler) Handle(ctx context.Context, r slog.Record) error {
	return f.h.Handle(ctx, r)
}

func (f *fileJSONHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &fileJSONHandler{w: f.w, h: f.h.WithAttrs(attrs), min: f.min}
}

func (f *fileJSONHandler) WithGroup(name string) slog.Handler {
	return &fileJSONHandler{w: f.w, h: f.h.WithGroup(name), min: f.min}
}

// Close closes the underlying fileWriter.
func (f *fileJSONHandler) Close() error {
	return f.w.Close()
}
