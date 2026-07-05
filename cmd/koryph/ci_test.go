// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/registry"
)

// registerProjectForCI registers a project with a GitHub forge and a
// koryph.project.json pointing at the given root.
func registerProjectForCI(t *testing.T, id, root string) {
	t.Helper()
	ctx := context.Background()
	store := registry.NewStore()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	rec := &registry.Record{
		ProjectID:        id,
		Name:             id,
		Root:             root,
		Remote:           "https://github.com/acme/widgets.git",
		AccountProfile:   "personal",
		ExpectedIdentity: "me@example.com",
	}
	if err := store.Add(ctx, rec); err != nil {
		t.Fatalf("store.Add: %v", err)
	}
	// Write minimal koryph.project.json.
	projJSON := `{
  "schema_version": 1,
  "project_id": "` + id + `",
  "work_source": "bd",
  "gate": ["make gate"],
  "merge_policy": "manual",
  "risk_tier_default": 2,
  "forge": "github"
}`
	if err := os.WriteFile(filepath.Join(root, "koryph.project.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatalf("write koryph.project.json: %v", err)
	}
}

// TestCIHelp asserts that `koryph ci -h` exits 0 and lists both sub-verbs.
func TestCIHelp(t *testing.T) {
	code, out, errb := runCmd("ci", "-h")
	if code != 0 {
		t.Fatalf("ci -h: code = %d (stderr=%s)", code, errb)
	}
	for _, want := range []string{"setup", "check", "SUBCOMMANDS"} {
		if !strings.Contains(out, want) {
			t.Errorf("ci -h missing %q:\n%s", want, out)
		}
	}
}

// TestCISetupHelpExitsZero asserts `koryph ci setup -h` exits 0.
func TestCISetupHelpExitsZero(t *testing.T) {
	code, _, errb := runCmd("ci", "setup", "-h")
	if code != 0 {
		t.Errorf("ci setup -h: code = %d (stderr=%s)", code, errb)
	}
}

// TestCICheckHelpExitsZero asserts `koryph ci check -h` exits 0.
func TestCICheckHelpExitsZero(t *testing.T) {
	code, _, errb := runCmd("ci", "check", "-h")
	if code != 0 {
		t.Errorf("ci check -h: code = %d (stderr=%s)", code, errb)
	}
}

// TestCISetupInstallsGateWorkflow verifies that `ci setup --kind gate` writes
// .github/workflows/koryph-gate.yml into the project root.
func TestCISetupInstallsGateWorkflow(t *testing.T) {
	isolate(t)
	root := gitRepo(t)
	registerProjectForCI(t, "citest", root)

	code, out, errb := runCmd("ci", "setup", "--project", "citest", "--kind", "gate")
	if code != 0 {
		t.Fatalf("ci setup: code = %d (stdout=%s stderr=%s)", code, out, errb)
	}

	wantFile := filepath.Join(root, ".github", "workflows", "koryph-gate.yml")
	if _, err := os.Stat(wantFile); err != nil {
		t.Errorf("gate workflow not installed at %s: %v", wantFile, err)
	}
	if !strings.Contains(out, "installed") {
		t.Errorf("setup output should contain 'installed':\n%s", out)
	}
}

// TestCISetupIdempotent verifies that running `ci setup` twice leaves the file
// unchanged and reports "already up-to-date".
func TestCISetupIdempotent(t *testing.T) {
	isolate(t)
	root := gitRepo(t)
	registerProjectForCI(t, "citest2", root)

	// First run.
	if code, _, errb := runCmd("ci", "setup", "--project", "citest2", "--kind", "gate"); code != 0 {
		t.Fatalf("first setup: code = %d (stderr=%s)", code, errb)
	}

	// Second run should be a no-op.
	code, out, errb := runCmd("ci", "setup", "--project", "citest2", "--kind", "gate")
	if code != 0 {
		t.Fatalf("second setup: code = %d (stderr=%s)", code, errb)
	}
	if !strings.Contains(out, "up-to-date") || !strings.Contains(out, "nothing to do") {
		t.Errorf("second setup should report up-to-date:\n%s", out)
	}
}

// TestCICheckNoDrift verifies `ci check` exits 0 when assets are current.
func TestCICheckNoDrift(t *testing.T) {
	isolate(t)
	root := gitRepo(t)
	registerProjectForCI(t, "cicheck1", root)

	// Install first.
	if code, _, errb := runCmd("ci", "setup", "--project", "cicheck1", "--kind", "gate"); code != 0 {
		t.Fatalf("setup: code = %d (stderr=%s)", code, errb)
	}

	// Check should be clean.
	code, out, errb := runCmd("ci", "check", "--project", "cicheck1", "--kind", "gate")
	if code != 0 {
		t.Fatalf("check: code = %d (stdout=%s stderr=%s)", code, out, errb)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("check output should contain 'ok':\n%s", out)
	}
}

// TestCICheckDriftExitsOne verifies `ci check` exits 1 when the asset is absent.
func TestCICheckDriftExitsOne(t *testing.T) {
	isolate(t)
	root := gitRepo(t)
	registerProjectForCI(t, "cicheck2", root)

	// Do NOT install; check should detect the missing file.
	code, _, _ := runCmd("ci", "check", "--project", "cicheck2", "--kind", "gate")
	if code != 1 {
		t.Errorf("ci check (absent asset): code = %d, want 1", code)
	}
}

// TestCISetupAll verifies `ci setup --kind all` runs without error.
func TestCISetupAll(t *testing.T) {
	isolate(t)
	root := gitRepo(t)
	registerProjectForCI(t, "citestall", root)

	code, _, errb := runCmd("ci", "setup", "--project", "citestall", "--kind", "all")
	if code != 0 {
		t.Fatalf("ci setup --kind all: code = %d (stderr=%s)", code, errb)
	}
}

// TestCISetupUnknownKindError verifies that an unknown --kind value returns a usage error.
func TestCISetupUnknownKindError(t *testing.T) {
	isolate(t)
	root := gitRepo(t)
	registerProjectForCI(t, "citestbadkind", root)
	code, _, errb := runCmd("ci", "setup", "--project", "citestbadkind", "--kind", "bogus")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage error for unknown kind (stderr=%s)", code, errb)
	}
}

// TestCIUnknownSubcommandError verifies that an unknown sub-verb returns a usage error.
func TestCIUnknownSubcommandError(t *testing.T) {
	code, _, errb := runCmd("ci", "frobnicate")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage error (stderr=%s)", code, errb)
	}
}
