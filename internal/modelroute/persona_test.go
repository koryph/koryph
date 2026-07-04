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

	model, effort, tier, err := PersonaMeta(root, "x")
	if err != nil {
		t.Fatalf("PersonaMeta error = %v", err)
	}
	if model != "opus" {
		t.Errorf("model = %q, want opus (quotes trimmed)", model)
	}
	if effort != "high" {
		t.Errorf("effort = %q, want high", effort)
	}
	if tier != "" {
		t.Errorf("tier = %q, want empty (frontmatter carries no tier here)", tier)
	}
}

func TestPersonaMetaMissingFile(t *testing.T) {
	root := t.TempDir()
	model, effort, tier, err := PersonaMeta(root, "nope")
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if model != "" || effort != "" || tier != "" {
		t.Errorf("missing file = (%q, %q, %q), want empty", model, effort, tier)
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
	model, effort, tier, err := PersonaMeta(root, "y")
	if err != nil {
		t.Fatalf("PersonaMeta error = %v", err)
	}
	if model != "" || effort != "" || tier != "" {
		t.Errorf("absent fields = (%q, %q, %q), want empty", model, effort, tier)
	}
}

func TestPersonaMetaNoFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "z", "# just a heading\nmodel: sonnet\n")
	model, effort, tier, err := PersonaMeta(root, "z")
	if err != nil {
		t.Fatalf("PersonaMeta error = %v", err)
	}
	if model != "" || effort != "" || tier != "" {
		t.Errorf("no frontmatter = (%q, %q, %q), want empty", model, effort, tier)
	}
}

// TestPersonaMetaTierParsing exercises tier's own present/absent/quoted
// shapes independently of model/effort (koryph-v8u.10).
func TestPersonaMetaTierParsing(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "unquoted-tier", `---
name: unquoted-tier
tier: standard
---
`)
	writeAgent(t, root, "quoted-tier", `---
name: quoted-tier
tier: "frontier"
---
`)
	writeAgent(t, root, "no-tier", `---
name: no-tier
model: sonnet
---
`)

	if _, _, tier, err := PersonaMeta(root, "unquoted-tier"); err != nil || tier != "standard" {
		t.Errorf("unquoted tier = (%q, %v), want (standard, nil)", tier, err)
	}
	if _, _, tier, err := PersonaMeta(root, "quoted-tier"); err != nil || tier != "frontier" {
		t.Errorf("quoted tier = (%q, %v), want (frontier, nil), quotes must be trimmed", tier, err)
	}
	if _, _, tier, err := PersonaMeta(root, "no-tier"); err != nil || tier != "" {
		t.Errorf("absent tier = (%q, %v), want (\"\", nil)", tier, err)
	}
}
