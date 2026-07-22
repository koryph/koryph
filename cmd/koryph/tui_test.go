// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/registry"
)

// tuiStore returns an initialized store handle for the isolated KORYPH_HOME.
func tuiStore(t *testing.T) *registry.Store {
	t.Helper()
	s := registry.NewStore()
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	return s
}

// --- resolveTUIProjects: default (cwd) resolution ---

func TestResolveTUIProjectsDefaultsToCwdProject(t *testing.T) {
	isolate(t)
	s := tuiStore(t)
	root := addProject(t, "demo").Root

	recs, code := resolveTUIProjects(io.Discard, s, "", false, root)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if len(recs) != 1 || recs[0].ProjectID != "demo" {
		t.Fatalf("recs = %v, want [demo]", recs)
	}
}

func TestResolveTUIProjectsDescendantCwd(t *testing.T) {
	isolate(t)
	s := tuiStore(t)
	root := addProject(t, "demo").Root

	sub := filepath.Join(root, "internal", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	recs, code := resolveTUIProjects(io.Discard, s, "", false, sub)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if len(recs) != 1 || recs[0].ProjectID != "demo" {
		t.Fatalf("recs = %v, want [demo] for a subdirectory of the repo", recs)
	}
}

func TestResolveTUIProjectsOutsideAnyProjectHints(t *testing.T) {
	isolate(t)
	s := tuiStore(t)
	addProject(t, "demo")

	var errb bytes.Buffer
	outside := t.TempDir() // a dir under no registered root
	recs, code := resolveTUIProjects(&errb, s, "", false, outside)
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want usage exit for cwd outside every project", code)
	}
	if recs != nil {
		t.Fatalf("recs = %v, want nil", recs)
	}
	out := errb.String()
	for _, want := range []string{"not inside a registered", "--project", "--all", "demo"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr = %q, want it to contain %q", out, want)
		}
	}
}

func TestResolveTUIProjectsOutsideWithNoneRegistered(t *testing.T) {
	isolate(t)
	s := tuiStore(t)

	var errb bytes.Buffer
	recs, code := resolveTUIProjects(&errb, s, "", false, t.TempDir())
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want usage exit", code)
	}
	if recs != nil {
		t.Fatalf("recs = %v, want nil", recs)
	}
	if !strings.Contains(errb.String(), "no projects registered") {
		t.Errorf("stderr = %q, want 'no projects registered' guidance", errb.String())
	}
}

// --- resolveTUIProjects: explicit selection ---

func TestResolveTUIProjectsExplicitProjectIgnoresCwd(t *testing.T) {
	isolate(t)
	s := tuiStore(t)
	addProject(t, "demo")

	// cwd is unrelated; --project must still resolve.
	recs, code := resolveTUIProjects(io.Discard, s, "demo", false, t.TempDir())
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if len(recs) != 1 || recs[0].ProjectID != "demo" {
		t.Fatalf("recs = %v, want [demo]", recs)
	}
}

func TestResolveTUIProjectsUnknownProjectIsFatal(t *testing.T) {
	isolate(t)
	s := tuiStore(t)

	var errb bytes.Buffer
	_, code := resolveTUIProjects(&errb, s, "nope", false, t.TempDir())
	if code != engine.ExitFatal {
		t.Fatalf("code = %d, want fatal for unknown project", code)
	}
	if !strings.Contains(errb.String(), "project list") {
		t.Errorf("stderr = %q, want 'project list' hint", errb.String())
	}
}

func TestResolveTUIProjectsAllProjects(t *testing.T) {
	isolate(t)
	s := tuiStore(t)
	addProject(t, "alpha")
	addProject(t, "beta")

	recs, code := resolveTUIProjects(io.Discard, s, "", true, t.TempDir())
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if len(recs) != 2 {
		t.Fatalf("len(recs) = %d, want 2 (all projects)", len(recs))
	}
}

func TestResolveTUIProjectsAllProjectsEmptyStore(t *testing.T) {
	isolate(t)
	s := tuiStore(t)

	var errb bytes.Buffer
	_, code := resolveTUIProjects(&errb, s, "", true, t.TempDir())
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "no projects registered") {
		t.Errorf("stderr = %q, want 'no projects registered'", errb.String())
	}
}

// --- full-command flag wiring (paths that return before launching the TUI) ---

func TestTUIProjectAndAllProjectsConflict(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("tui", "--project", "x", "--all-projects")
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want usage exit on conflict", code)
	}
	if !strings.Contains(errb, "mutually exclusive") {
		t.Errorf("stderr = %q, want 'mutually exclusive'", errb)
	}
}

// TestTUIAllIsThePrimarySpelling confirms --all (koryph-b8g #18's
// standardized spelling) is wired identically to the --all-projects alias it
// replaces: --project and --all still conflict.
func TestTUIAllIsThePrimarySpelling(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("tui", "--project", "x", "--all")
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want usage exit on conflict", code)
	}
	if !strings.Contains(errb, "mutually exclusive") {
		t.Errorf("stderr = %q, want 'mutually exclusive'", errb)
	}
}

// TestTUIAllProjectsFlagIsHiddenFromHelp confirms the back-compat spelling
// stays functional (see TestTUIProjectAndAllProjectsConflict) but does not
// clutter -h output — --all is the one documented spelling.
func TestTUIAllProjectsFlagIsHiddenFromHelp(t *testing.T) {
	_, out, _ := runCmd("tui", "-h")
	if !strings.Contains(out, "--all") {
		t.Errorf("tui -h should document --all:\n%s", out)
	}
	if strings.Contains(out, "--all-projects") {
		t.Errorf("tui -h should not list the deprecated --all-projects alias:\n%s", out)
	}
}

func TestTUIShortFlagWiredToAllProjects(t *testing.T) {
	isolate(t) // empty store: -a reaches the all-projects branch and bails before any TTY
	code, _, errb := runCmd("tui", "-a")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (no projects) — confirms -a is wired", code)
	}
	if !strings.Contains(errb, "no projects registered") {
		t.Errorf("stderr = %q, want 'no projects registered'", errb)
	}
}
