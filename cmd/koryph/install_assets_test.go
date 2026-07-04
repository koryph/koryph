// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
)

// TestProjectInstallAssetsAll installs the full asset set (default target) into
// a fresh root and asserts every asset kind landed.
func TestProjectInstallAssetsAll(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	code, out, errb := runCmd("project", "install-assets", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (stderr=%s)", code, errb)
	}
	for _, dir := range []string{
		filepath.Join(root, ".claude", "agents"),
		filepath.Join(root, ".claude", "commands"),
	} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("expected %s to be installed (err=%v)\nstdout=%s", dir, err, out)
		}
	}
	// The rules install wires the project settings.json (hook scripts live
	// centrally under KORYPH_HOME, referenced from here).
	if _, err := os.Stat(filepath.Join(root, ".claude", "settings.json")); err != nil {
		t.Errorf("expected settings.json wired: %v", err)
	}
}

// TestProjectInstallAssetsSingleTarget installs only one asset kind and asserts
// the others were not touched.
func TestProjectInstallAssetsSingleTarget(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	code, _, errb := runCmd("project", "install-assets", root, "agents")
	if code != 0 {
		t.Fatalf("code = %d, want 0 (stderr=%s)", code, errb)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "agents")); err != nil {
		t.Errorf("agents not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "commands")); !os.IsNotExist(err) {
		t.Errorf("commands should not be installed for target=agents (err=%v)", err)
	}
}

// TestProjectInstallAssetsRequiresRoot rejects an invocation with neither a
// root nor --all-projects.
func TestProjectInstallAssetsRequiresRoot(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("project", "install-assets")
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want usage exit (stderr=%s)", code, errb)
	}
	if !strings.Contains(errb, "<root> is required") {
		t.Errorf("stderr missing hint: %s", errb)
	}
}

// TestProjectInstallAssetsRootAllProjectsConflict rejects passing both a root
// and --all-projects.
func TestProjectInstallAssetsRootAllProjectsConflict(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("project", "install-assets", t.TempDir(), "--all-projects")
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want usage exit (stderr=%s)", code, errb)
	}
	if !strings.Contains(errb, "mutually exclusive") {
		t.Errorf("stderr missing conflict hint: %s", errb)
	}
}

// TestGlobalUsageIsSectioned asserts the top-level listing groups verbs into
// discoverability sections and leads with the getting-started flow, so the
// asset installers no longer sit at top level with equal billing to run.
func TestGlobalUsageIsSectioned(t *testing.T) {
	_, out, _ := runCmd("help")
	for _, section := range []string{"GETTING STARTED", "OPERATE", "OBSERVE", "ASSETS", "ADVANCED"} {
		if !strings.Contains(out, section) {
			t.Errorf("usage missing section %q:\n%s", section, out)
		}
	}
	// The grouped installer is advertised in the ASSETS section.
	if !strings.Contains(out, "project install-assets") {
		t.Errorf("usage missing 'project install-assets':\n%s", out)
	}
	// Getting-started verbs must precede the ASSETS section (installers are
	// subordinated, not top-billed).
	if strings.Index(out, "GETTING STARTED") > strings.Index(out, "ASSETS") {
		t.Errorf("GETTING STARTED should precede ASSETS:\n%s", out)
	}
}

// TestProjectHelpListsInstallAssets asserts the new grouped verb appears in the
// project parent's sub-verb listing.
func TestProjectHelpListsInstallAssets(t *testing.T) {
	code, out, _ := runCmd("project", "-h")
	if code != 0 || !strings.Contains(out, "install-assets") {
		t.Errorf("project help missing install-assets (code=%d):\n%s", code, out)
	}
}
