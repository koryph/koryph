// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileWriterBasicWrite(t *testing.T) {
	dir := t.TempDir()
	fw := newFileWriter(dir, 50*1024*1024)
	defer fw.Close()

	line := []byte(`{"level":"INFO","msg":"hello"}\n`)
	n, err := fw.Write(line)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(line) {
		t.Errorf("Write returned %d, want %d", n, len(line))
	}

	// Check the file exists.
	today := time.Now().UTC().Format("20060102")
	path := filepath.Join(dir, "koryph-"+today+".jsonl")
	if _, sterr := os.Stat(path); sterr != nil {
		t.Fatalf("expected file %s to exist: %v", path, sterr)
	}
}

func TestFileWriterSizeRotation(t *testing.T) {
	dir := t.TempDir()
	// Set a tiny cap so rotation triggers immediately.
	const cap = 20
	fw := newFileWriter(dir, cap)
	defer fw.Close()

	today := time.Now().UTC().Format("20060102")
	active := filepath.Join(dir, "koryph-"+today+".jsonl")

	// First write: well under cap.
	line1 := []byte(`{"msg":"first"}` + "\n")
	if _, err := fw.Write(line1); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	// Second write: will push currentSize ≥ cap so ensureOpen should rotate on
	// the NEXT call.
	line2 := []byte(`{"msg":"second"}` + "\n")
	if _, err := fw.Write(line2); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	// Third write: triggers rotation check inside ensureOpen.
	line3 := []byte(`{"msg":"third"}` + "\n")
	if _, err := fw.Write(line3); err != nil {
		t.Fatalf("third Write: %v", err)
	}
	fw.Close()

	// After rotation the directory should contain at least two .jsonl files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var jsonls []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			jsonls = append(jsonls, e.Name())
		}
	}
	if len(jsonls) < 2 {
		t.Errorf("expected at least 2 JSONL files after rotation, got %d: %v (active=%s)", len(jsonls), jsonls, active)
	}
}

func TestFileWriterCreatesDir(t *testing.T) {
	// Use a sub-directory that does not exist yet.
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "telemetry")

	fw := newFileWriter(dir, 50*1024*1024)
	defer fw.Close()

	if _, err := fw.Write([]byte(`{"msg":"x"}` + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s not created: %v", dir, err)
	}
}

func TestTelemetryDirPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)
	got := telemetryDirPath()
	want := filepath.Join(tmp, "telemetry")
	if got != want {
		t.Errorf("telemetryDirPath = %q, want %q", got, want)
	}
}
