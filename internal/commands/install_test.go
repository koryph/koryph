// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package commands_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/commands"
	_ "github.com/koryph/koryph/internal/runtime/codex"
	"github.com/koryph/koryph/internal/scaffold"
)

// TestInstallWritesKoryphCommands verifies the embedded koryph-* slash
// commands install into an empty project and include the expected set.
func TestInstallWritesKoryphCommands(t *testing.T) {
	root := t.TempDir()
	results, err := commands.Install(root, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := map[string]bool{}
	for _, r := range results {
		if r.Action != scaffold.ActionInstalled {
			t.Errorf("command %q: action=%q, want installed", r.Name, r.Action)
		}
		got[r.Name] = true
		dest := filepath.Join(root, ".claude", "commands", r.Name+".md")
		if _, serr := os.Stat(dest); serr != nil {
			t.Errorf("command %q not written: %v", r.Name, serr)
		}
	}
	for _, want := range []string{"koryph-adopt", "koryph-calibrate", "koryph-design", "koryph-import", "koryph-issue", "koryph-build", "koryph-loop", "koryph-plan", "koryph-replan", "koryph-stop", "koryph-kill"} {
		if !got[want] {
			t.Errorf("missing embedded command %q (installed: %v)", want, keys(got))
		}
	}
}

func TestInstallForCodexLinksCanonicalWorkflows(t *testing.T) {
	root := t.TempDir()
	results, err := commands.InstallForRuntime(root, false, "codex")
	if err != nil {
		t.Fatalf("InstallForRuntime(codex): %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no Codex workflow links installed")
	}
	for _, result := range results {
		path := filepath.Join(root, ".agents", "skills", result.Name, "SKILL.md")
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not a canonical-source symlink", path)
		}
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "name: "+result.Name) {
			t.Errorf("Codex skill %s does not resolve its canonical command: %v\n%s", result.Name, err, data)
		}
	}
}

// TestInstallIdempotent verifies a second identical install is a silent no-op.
func TestInstallIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := commands.Install(root, false); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	results, err := commands.Install(root, false)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	for _, r := range results {
		if r.Action != scaffold.ActionUnchanged {
			t.Errorf("command %q: action=%q, want unchanged on re-install", r.Name, r.Action)
		}
	}
	if c := scaffold.Conflicts(results); len(c) != 0 {
		t.Errorf("Conflicts = %v, want none", c)
	}
}

func keys(m map[string]bool) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
