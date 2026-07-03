// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if _, err := os.Stat(filepath.Join(root, "hooks", "worktree-guard.sh")); err != nil {
		t.Errorf("hook script not installed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(data), "bd prime") {
		t.Errorf("settings.json missing bd prime hook (err %v):\n%s", err, data)
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
