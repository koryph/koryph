// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/paths"
)

func TestRulesInstallCreatesWiring(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	code, out, _ := runCmd("rules", "install", root)
	if code != 0 {
		t.Fatalf("rules install: code %d out %q", code, out)
	}
	if !strings.Contains(out, "agent-boundary-guard") || !strings.Contains(out, "settings.json: created") {
		t.Errorf("unexpected output:\n%s", out)
	}
	// Guard scripts install CENTRALLY (agent-unwritable), not into the worktree.
	if _, err := os.Stat(filepath.Join(paths.HooksDir(), "worktree-guard.sh")); err != nil {
		t.Errorf("hook script not installed to central HooksDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "hooks", "worktree-guard.sh")); err == nil {
		t.Error("hook script installed into the worktree; must live outside the agent's write scope")
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "bd prime") {
		t.Errorf("settings.json missing bd prime hook:\n%s", s)
	}
	// Hooks are wired via KORYPH_HOME, never the agent-writable project dir.
	if !strings.Contains(s, "${KORYPH_HOME:-$HOME/.koryph}/hooks/agent-boundary-guard.sh") {
		t.Errorf("settings.json does not reference the central KORYPH_HOME hook path:\n%s", s)
	}
	if strings.Contains(s, "CLAUDE_PROJECT_DIR") {
		t.Errorf("settings.json still references CLAUDE_PROJECT_DIR (worktree-writable):\n%s", s)
	}
}

func TestRulesInstallRequiresRoot(t *testing.T) {
	isolate(t)
	code, _, errs := runCmd("rules", "install")
	if code == 0 || !strings.Contains(errs, "<root> is required") {
		t.Errorf("code=%d stderr=%q, want usage error", code, errs)
	}
}

func TestRulesUnknownSubcommand(t *testing.T) {
	isolate(t)
	code, _, errs := runCmd("rules", "frobnicate")
	if code == 0 || !strings.Contains(errs, "unknown rules subcommand") {
		t.Errorf("code=%d stderr=%q, want usage error", code, errs)
	}
}
