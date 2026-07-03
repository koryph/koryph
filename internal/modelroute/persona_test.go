// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package modelroute

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAgent(t *testing.T, repoRoot, persona, body string) {
	t.Helper()
	dir := filepath.Join(repoRoot, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, persona+".md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestPersonaMetaFrontmatter(t *testing.T) {
	root := t.TempDir()
	// Mixes a quoted scalar (model), an unquoted scalar (effort), a list
	// field that must be ignored, and body content after the frontmatter.
	writeAgent(t, root, "x", `---
name: x
model: "opus"
effort: high
tools:
  - Bash
  - Read
description: 'an example agent'
---

# Body heading
model: not-this
`)

	model, effort, err := PersonaMeta(root, "x")
	if err != nil {
		t.Fatalf("PersonaMeta error = %v", err)
	}
	if model != "opus" {
		t.Errorf("model = %q, want opus (quotes trimmed)", model)
	}
	if effort != "high" {
		t.Errorf("effort = %q, want high", effort)
	}
}

func TestPersonaMetaMissingFile(t *testing.T) {
	root := t.TempDir()
	model, effort, err := PersonaMeta(root, "nope")
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if model != "" || effort != "" {
		t.Errorf("missing file = (%q, %q), want empty", model, effort)
	}
}

func TestPersonaMetaAbsentFields(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "y", `---
name: y
description: no model or effort here
---
body
`)
	model, effort, err := PersonaMeta(root, "y")
	if err != nil {
		t.Fatalf("PersonaMeta error = %v", err)
	}
	if model != "" || effort != "" {
		t.Errorf("absent fields = (%q, %q), want empty", model, effort)
	}
}

func TestPersonaMetaNoFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "z", "# just a heading\nmodel: sonnet\n")
	model, effort, err := PersonaMeta(root, "z")
	if err != nil {
		t.Fatalf("PersonaMeta error = %v", err)
	}
	if model != "" || effort != "" {
		t.Errorf("no frontmatter = (%q, %q), want empty", model, effort)
	}
}
