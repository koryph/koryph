// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExportRunFiltersByRunID(t *testing.T) {
	dir := t.TempDir()

	// Write a JSONL file with records for two different run IDs.
	lines := []string{
		`{"time":"2026-07-04T10:00:00Z","level":"INFO","msg":"start","run_id":"run-A","component":"engine"}`,
		`{"time":"2026-07-04T10:00:01Z","level":"INFO","msg":"tick","run_id":"run-B","component":"engine"}`,
		`{"time":"2026-07-04T10:00:02Z","level":"DEBUG","msg":"refill","run_id":"run-A","component":"sched"}`,
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "koryph-20260704.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := ExportRun(ExportOptions{Dir: dir, RunID: "run-A"}, &buf)
	if err != nil {
		t.Fatalf("ExportRun: %v", err)
	}
	if res.Records != 2 {
		t.Errorf("records = %d, want 2", res.Records)
	}
	if res.Files != 1 {
		t.Errorf("files = %d, want 1", res.Files)
	}

	// Validate output is valid JSONL with only run-A records.
	out := bytes.TrimRight(buf.Bytes(), "\n")
	outLines := bytes.Split(out, []byte("\n"))
	if len(outLines) != 2 {
		t.Fatalf("expected 2 output lines, got %d", len(outLines))
	}
	for _, ol := range outLines {
		var m map[string]any
		if err := json.Unmarshal(ol, &m); err != nil {
			t.Fatalf("output line is not valid JSON: %v", err)
		}
		if m["run_id"] != "run-A" {
			t.Errorf("unexpected run_id in output: %v", m["run_id"])
		}
	}
}

func TestExportRunRedactsSecrets(t *testing.T) {
	dir := t.TempDir()

	// Write a record that contains a field whose KEY matches secretKeyPattern
	// (IsSecretKey("token") == true).  The value does not need to be
	// secret-shaped — the key match alone triggers redaction.
	line := `{"time":"2026-07-04T10:00:00Z","level":"INFO","msg":"vault","run_id":"r1","token":"plaintext-value-for-test"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "koryph-20260704.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := ExportRun(ExportOptions{Dir: dir, RunID: "r1"}, &buf)
	if err != nil {
		t.Fatalf("ExportRun: %v", err)
	}
	if res.Records != 1 {
		t.Errorf("records = %d, want 1", res.Records)
	}

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &m); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	// The token field must be redacted.
	if m["token"] != Redacted {
		t.Errorf("token = %v, want %q", m["token"], Redacted)
	}
}

func TestExportRunEmptyDir(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	res, err := ExportRun(ExportOptions{Dir: dir, RunID: "any"}, &buf)
	if err != nil {
		t.Fatalf("ExportRun on empty dir: %v", err)
	}
	if res.Records != 0 {
		t.Errorf("records = %d, want 0", res.Records)
	}
}

func TestExportRunMissingDir(t *testing.T) {
	var buf bytes.Buffer
	res, err := ExportRun(ExportOptions{Dir: "/nonexistent-koryph-test-dir/tel", RunID: "x"}, &buf)
	if err != nil {
		t.Fatalf("ExportRun missing dir should not error: %v", err)
	}
	if res.Records != 0 {
		t.Errorf("records = %d, want 0", res.Records)
	}
}

func TestRedactMapNestedSecret(t *testing.T) {
	m := map[string]any{
		"msg":   "hello",
		"token": "sk-abcdefghijklmnopqrstuvwxyz",
		"nested": map[string]any{
			"password": "hunter2",
			"safe":     "just-a-path",
		},
	}
	out := redactMap(m)

	if out["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", out["msg"])
	}
	if out["token"] != Redacted {
		t.Errorf("token = %v, want %q", out["token"], Redacted)
	}
	nested, ok := out["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested is not a map")
	}
	if nested["password"] != Redacted {
		t.Errorf("nested.password = %v, want %q", nested["password"], Redacted)
	}
	// "safe" is not a secret key; its value is short and not token-shaped.
	if nested["safe"] != "just-a-path" {
		t.Errorf("nested.safe = %v, want just-a-path", nested["safe"])
	}
}
