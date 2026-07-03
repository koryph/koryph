// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package fsx_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/fsx"
)

// TestWriteAtomic_VisibleOnSuccess verifies that the file is fully written and
// no temporary file is left behind after a successful WriteAtomic.
func TestWriteAtomic_VisibleOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	if err := fsx.WriteAtomic(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}

	// No .tmp-* file should remain.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("stale temp file: %s", e.Name())
		}
	}
}

// TestWriteAtomic_CreatesParentDir confirms that WriteAtomic creates missing
// parent directories rather than failing.
func TestWriteAtomic_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "out.txt")

	if err := fsx.WriteAtomic(path, []byte("nested"), 0o644); err != nil {
		t.Fatalf("WriteAtomic deep path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// TestWriteAtomic_AbsentOnFailure ensures the destination file does not appear
// when writing fails (simulated by making the directory read-only so the
// rename cannot proceed and the temp is cleaned up).
// This test is skipped when running as root because root can write anywhere.
func TestWriteAtomic_AbsentOnFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; permission check not meaningful")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly-dir", "out.txt")
	// Create the sub-directory and immediately make it read-only so WriteAtomic
	// cannot create temp files inside it.
	if err := os.Mkdir(filepath.Join(dir, "readonly-dir"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Restore so TempDir cleanup can proceed.
		_ = os.Chmod(filepath.Join(dir, "readonly-dir"), 0o755)
	})

	err := fsx.WriteAtomic(path, []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error writing into read-only dir, got nil")
	}
	// The destination must not exist.
	if fsx.Exists(path) {
		t.Error("destination file exists after failed WriteAtomic")
	}
}

// TestReadJSON_ErrorWrap checks that ReadJSON wraps a JSON-parse error with
// the file path, making it easier to diagnose in logs.
func TestReadJSON_ErrorWrap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not-valid-json"), 0o644); err != nil {
		t.Fatal(err)
	}

	var v any
	err := fsx.ReadJSON(path, &v)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not include path %q", err.Error(), path)
	}
}

// TestReadJSON_MissingFile verifies that ReadJSON propagates the OS error for
// a file that does not exist.
func TestReadJSON_MissingFile(t *testing.T) {
	var v any
	err := fsx.ReadJSON("/does/not/exist/koryph-test.json", &v)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestAppendLine_CreateAndAppend verifies that AppendLine creates the file on
// first call and appends subsequent lines without truncating earlier content.
func TestAppendLine_CreateAndAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")

	if err := fsx.AppendLine(path, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("AppendLine (create): %v", err)
	}
	if err := fsx.AppendLine(path, []byte(`{"b":2}`)); err != nil {
		t.Fatalf("AppendLine (append): %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Each call appends one line terminated by '\n'.
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2; file content:\n%s", len(lines), raw)
	}
	if lines[0] != `{"a":1}` || lines[1] != `{"b":2}` {
		t.Errorf("lines = %v, want [{\"a\":1} {\"b\":2}]", lines)
	}
}

// TestAppendLine_TrailingNewline confirms each appended entry carries its own
// trailing newline (JSONL convention).
func TestAppendLine_TrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nl.jsonl")
	if err := fsx.AppendLine(path, []byte("line")); err != nil {
		t.Fatalf("AppendLine: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Errorf("file %q does not end with newline", raw)
	}
}

// TestWriteJSONAtomic_RoundTrip marshals a struct, writes it atomically, and
// unmarshals the file back, checking the values are preserved.
func TestWriteJSONAtomic_RoundTrip(t *testing.T) {
	type payload struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	in := payload{Name: "koryph", Count: 42}
	if err := fsx.WriteJSONAtomic(path, in); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// WriteJSONAtomic appends a trailing newline.
	if data[len(data)-1] != '\n' {
		t.Error("file does not end with newline")
	}

	var out payload
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip = %+v, want %+v", out, in)
	}
}

// TestWriteJSONAtomic_ReadJSON_RoundTrip pairs WriteJSONAtomic with ReadJSON
// to cover the end-to-end fsx write→read path.
func TestWriteJSONAtomic_ReadJSON_RoundTrip(t *testing.T) {
	type state struct {
		Version int    `json:"version"`
		Tag     string `json:"tag"`
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")

	in := state{Version: 3, Tag: "beta"}
	if err := fsx.WriteJSONAtomic(path, in); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}
	var out state
	if err := fsx.ReadJSON(path, &out); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if out != in {
		t.Errorf("round-trip = %+v, want %+v", out, in)
	}
}
