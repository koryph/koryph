// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package personas_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/agents"
	"github.com/koryph/koryph/internal/personas"
	"github.com/koryph/koryph/internal/scaffold"
)

// embeddedNames returns the persona names bundled into the binary.
func embeddedNames(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(agents.FS, ".")
	if err != nil {
		t.Fatalf("read embedded FS: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	return names
}

// actionOf returns the install action reported for name.
func actionOf(results []scaffold.Result, name string) string {
	for _, r := range results {
		if r.Name == name {
			return r.Action
		}
	}
	return ""
}

// TestInstallWritesMissingPersonas verifies that Install writes every embedded
// persona into an empty .claude/agents directory.
func TestInstallWritesMissingPersonas(t *testing.T) {
	root := t.TempDir()
	results, err := personas.Install(root, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	embedded := embeddedNames(t)
	if len(results) != len(embedded) {
		t.Errorf("Install returned %d results, want %d (embedded count)", len(results), len(embedded))
	}

	for _, r := range results {
		if r.Action != scaffold.ActionInstalled {
			t.Errorf("persona %q: action=%q, want installed (empty destination)", r.Name, r.Action)
		}
		dest := filepath.Join(root, ".claude", "agents", r.Name+".md")
		if _, serr := os.Stat(dest); os.IsNotExist(serr) {
			t.Errorf("persona %q: file not found at %s", r.Name, dest)
		}
	}
}

// TestInstallSkipsDifferingContent verifies that a pre-existing file whose
// content DIFFERS from the embedded persona is left untouched (skipped) without
// --force, while the rest install.
func TestInstallSkipsDifferingContent(t *testing.T) {
	root := t.TempDir()
	agentsDir := filepath.Join(root, ".claude", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	embedded := embeddedNames(t)
	if len(embedded) == 0 {
		t.Skip("no embedded personas to test")
	}

	existing := embedded[0]
	existingPath := filepath.Join(agentsDir, existing+".md")
	sentinel := "# custom content — must not be overwritten\n"
	if err := os.WriteFile(existingPath, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := personas.Install(root, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := actionOf(results, existing); got != scaffold.ActionSkipped {
		t.Errorf("persona %q: action=%q, want skipped (differing content, no force)", existing, got)
	}
	if got, _ := os.ReadFile(existingPath); string(got) != sentinel {
		t.Errorf("existing persona %q content changed:\ngot  %q\nwant %q", existing, got, sentinel)
	}
	// The skipped file is a conflict the caller should warn about.
	if conflicts := scaffold.Conflicts(results); len(conflicts) != 1 || conflicts[0] != existing {
		t.Errorf("Conflicts = %v, want [%s]", conflicts, existing)
	}
	for _, r := range results {
		if r.Name != existing && r.Action != scaffold.ActionInstalled {
			t.Errorf("persona %q: action=%q, want installed", r.Name, r.Action)
		}
	}
}

// TestInstallForceOverwritesDiffering verifies that --force replaces a
// differing file with the embedded content.
func TestInstallForceOverwritesDiffering(t *testing.T) {
	root := t.TempDir()
	agentsDir := filepath.Join(root, ".claude", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	embedded := embeddedNames(t)
	if len(embedded) == 0 {
		t.Skip("no embedded personas to test")
	}
	existing := embedded[0]
	existingPath := filepath.Join(agentsDir, existing+".md")
	if err := os.WriteFile(existingPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := personas.Install(root, true)
	if err != nil {
		t.Fatalf("Install(force): %v", err)
	}
	if got := actionOf(results, existing); got != scaffold.ActionOverwritten {
		t.Errorf("persona %q: action=%q, want overwritten under --force", existing, got)
	}
	want, _ := agents.FS.ReadFile(existing + ".md")
	if got, _ := os.ReadFile(existingPath); string(got) != string(want) {
		t.Errorf("persona %q not overwritten with embedded content", existing)
	}
	if conflicts := scaffold.Conflicts(results); len(conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none under --force", conflicts)
	}
}

// TestInstallIdempotentUnchanged verifies that a second install of identical
// content is a silent no-op (unchanged, not skipped) and raises no conflicts.
func TestInstallIdempotentUnchanged(t *testing.T) {
	root := t.TempDir()
	if _, err := personas.Install(root, false); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	results, err := personas.Install(root, false)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	for _, r := range results {
		if r.Action != scaffold.ActionUnchanged {
			t.Errorf("second Install: persona %q action=%q, want unchanged", r.Name, r.Action)
		}
	}
	if conflicts := scaffold.Conflicts(results); len(conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none for identical re-install", conflicts)
	}
}

// TestInstallCreatesDestDir verifies that Install creates .claude/agents when
// it does not exist.
func TestInstallCreatesDestDir(t *testing.T) {
	root := t.TempDir()
	destDir := filepath.Join(root, ".claude", "agents")
	if _, err := os.Stat(destDir); !os.IsNotExist(err) {
		t.Fatalf("expected %s to not exist before Install", destDir)
	}
	if _, err := personas.Install(root, false); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(destDir); os.IsNotExist(err) {
		t.Errorf("Install did not create %s", destDir)
	}
}
