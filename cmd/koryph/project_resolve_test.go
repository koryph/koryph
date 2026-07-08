// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
)

// --- resolveProjectRecord: shared cwd-default resolution ---

func TestResolveProjectRecordExplicitID(t *testing.T) {
	isolate(t)
	s := tuiStore(t)
	addProject(t, "demo")

	// An explicit id resolves regardless of cwd (here: an unrelated temp dir).
	rec, code := resolveProjectRecord(io.Discard, s, "demo", t.TempDir(), "roster")
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if rec == nil || rec.ProjectID != "demo" {
		t.Fatalf("rec = %v, want demo", rec)
	}
}

func TestResolveProjectRecordDefaultsToCwd(t *testing.T) {
	isolate(t)
	s := tuiStore(t)
	root := addProject(t, "demo").Root

	// Empty id + cwd inside the repo (and a subdirectory) resolves the project.
	for _, cwd := range []string{root, filepath.Join(root, "internal", "pkg")} {
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		rec, code := resolveProjectRecord(io.Discard, s, "", cwd, "roster")
		if code != 0 {
			t.Fatalf("cwd %s: code = %d, want 0", cwd, code)
		}
		if rec == nil || rec.ProjectID != "demo" {
			t.Fatalf("cwd %s: rec = %v, want demo", cwd, rec)
		}
	}
}

func TestResolveProjectRecordUnknownID(t *testing.T) {
	isolate(t)
	s := tuiStore(t)
	addProject(t, "demo")

	var errb bytes.Buffer
	rec, code := resolveProjectRecord(&errb, s, "nope", t.TempDir(), "roster")
	if code == 0 || rec != nil {
		t.Fatalf("code = %d rec = %v, want non-zero exit and nil rec", code, rec)
	}
}

func TestResolveProjectRecordOutsideAnyProjectHints(t *testing.T) {
	isolate(t)
	s := tuiStore(t)
	addProject(t, "demo")

	var errb bytes.Buffer
	rec, code := resolveProjectRecord(&errb, s, "", t.TempDir(), "roster")
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want usage exit", code)
	}
	if rec != nil {
		t.Fatalf("rec = %v, want nil", rec)
	}
	out := errb.String()
	// Lists the registered projects and carries the shared "--project is required" phrase.
	if !strings.Contains(out, "demo") || !strings.Contains(out, "--project is required") {
		t.Errorf("hint = %q, want it to list projects and mention --project is required", out)
	}
}

// --- mergeProjectID: positional + flag reconciliation (empty allowed) ---

func TestMergeProjectID(t *testing.T) {
	cases := []struct {
		name            string
		posVal, flagVal string
		wantID          string
		wantErr         bool
	}{
		{"flag only", "", "from-flag", "from-flag", false},
		{"positional only", "from-pos", "", "from-pos", false},
		{"agree", "same", "same", "same", false},
		{"neither → empty (cwd fallback)", "", "", "", false},
		{"conflict", "a", "b", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var errb bytes.Buffer
			id, code := mergeProjectID(&errb, "cmd", c.posVal, c.flagVal)
			if c.wantErr {
				if code == 0 {
					t.Fatalf("code = 0, want usage error")
				}
				return
			}
			if code != 0 {
				t.Fatalf("code = %d, want 0 (stderr=%s)", code, errb.String())
			}
			if id != c.wantID {
				t.Errorf("id = %q, want %q", id, c.wantID)
			}
		})
	}
}
