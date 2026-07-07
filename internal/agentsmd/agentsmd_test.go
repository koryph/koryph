// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package agentsmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/agentsmd"
	"github.com/koryph/koryph/internal/scaffold"
)

// TestInstallFreshRoot writes AGENTS.md into an empty directory.
func TestInstallFreshRoot(t *testing.T) {
	root := t.TempDir()
	action, err := agentsmd.Install(root, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if action != scaffold.ActionInstalled {
		t.Errorf("action = %q, want %q", action, scaffold.ActionInstalled)
	}
	dest := filepath.Join(root, "AGENTS.md")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Spot-check: AGENTS.md must mention the containment model (hooks + worktree
	// isolation paths), so non-Claude runtimes see the documented trust delta.
	for _, marker := range []string{
		"AGENTS.md",
		"hook",
		"worktree isolation",
		"merge-time",
		"beads",
	} {
		if !strings.Contains(string(data), marker) {
			t.Errorf("AGENTS.md missing %q marker", marker)
		}
	}
}

// TestInstallIdempotent re-installs over identical content — must be a no-op.
func TestInstallIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := agentsmd.Install(root, false); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	action, err := agentsmd.Install(root, false)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if action != scaffold.ActionUnchanged {
		t.Errorf("action = %q, want %q (re-install of identical content)", action, scaffold.ActionUnchanged)
	}
}

// TestInstallSkipsWhenDifferingContent leaves a customised AGENTS.md untouched
// without force.
func TestInstallSkipsWhenDifferingContent(t *testing.T) {
	root := t.TempDir()
	custom := "# my custom AGENTS.md\ndo not overwrite me\n"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	action, err := agentsmd.Install(root, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if action != scaffold.ActionSkipped {
		t.Errorf("action = %q, want %q", action, scaffold.ActionSkipped)
	}
	// File must be unchanged.
	got, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if string(got) != custom {
		t.Errorf("content mutated; got %q, want %q", got, custom)
	}
}

// TestInstallForceOverwritesDifferingContent overwrites with force.
func TestInstallForceOverwritesDifferingContent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	action, err := agentsmd.Install(root, true)
	if err != nil {
		t.Fatalf("Install --force: %v", err)
	}
	if action != scaffold.ActionOverwritten {
		t.Errorf("action = %q, want %q", action, scaffold.ActionOverwritten)
	}
	got, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if string(got) != string(agentsmd.Template()) {
		t.Error("content not overwritten to template")
	}
}

// TestTemplateContainsContainmentModel confirms the embedded template documents
// the hooks vs. worktree-isolation containment split explicitly.
func TestTemplateContainsContainmentModel(t *testing.T) {
	tmpl := string(agentsmd.Template())
	for _, fragment := range []string{
		"Runtimes with hook support",
		"Runtimes without hook support",
		"worktree isolation",
		"merge-time protected-path refusal",
	} {
		if !strings.Contains(tmpl, fragment) {
			t.Errorf("template missing containment-model fragment %q", fragment)
		}
	}
}

// TestTemplateOutputEconomy verifies the output-economy section (design:
// docs/designs/2026-07-token-economy.md §3 L3+L4) is present in the embedded
// AGENTS.md template and teaches the three key patterns: quiet gate
// (make gate-agent), file-spill wrappers (koryph-spill.sh), and Read-based
// recovery.
func TestTemplateOutputEconomy(t *testing.T) {
	tmpl := string(agentsmd.Template())
	for _, fragment := range []string{
		"make gate-agent",
		"koryph-spill.sh",
		"full output",
		"Read tool",
		"gate-agent",
		"file-spill",
	} {
		if !strings.Contains(tmpl, fragment) {
			t.Errorf("AGENTS.md template missing output-economy fragment %q", fragment)
		}
	}
}
