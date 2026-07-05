// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ciinstall_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ciinstall"
	"github.com/koryph/koryph/internal/forge"
)

// fakeCI is a test-double forge.CIService that returns canned content per kind.
type fakeCI struct {
	supported map[string][]byte // kind → content
}

func (f *fakeCI) Render(kind string) ([]byte, error) {
	if content, ok := f.supported[kind]; ok {
		return content, nil
	}
	return nil, fmt.Errorf("fake: Render(%q): %w", kind, forge.ErrUnsupported)
}

// fakeCIWith returns a fakeCI that supports the given kinds with synthetic content.
func fakeCIWith(kinds ...string) *fakeCI {
	f := &fakeCI{supported: make(map[string][]byte, len(kinds))}
	for _, k := range kinds {
		f.supported[k] = []byte("# ci asset for kind=" + k + "\n")
	}
	return f
}

// ---------- KindPath ----------------------------------------------------------

func TestKindPathGitHub(t *testing.T) {
	cases := []struct {
		kind, want string
	}{
		{"gate", filepath.Join(".github", "workflows", "koryph-gate.yml")},
		{"scanner", filepath.Join(".github", "workflows", "koryph-scanner.yml")},
	}
	for _, c := range cases {
		got, ok := ciinstall.KindPath("github", c.kind)
		if !ok {
			t.Errorf("KindPath(github, %q): ok=false, want true", c.kind)
			continue
		}
		if got != c.want {
			t.Errorf("KindPath(github, %q) = %q, want %q", c.kind, got, c.want)
		}
	}
}

func TestKindPathGitLab(t *testing.T) {
	cases := []struct {
		kind, want string
	}{
		{"gate", filepath.Join(".koryph", "ci", "koryph-gate.yml")},
		{"scanner", filepath.Join(".koryph", "ci", "koryph-scanner.yml")},
	}
	for _, c := range cases {
		got, ok := ciinstall.KindPath("gitlab", c.kind)
		if !ok {
			t.Errorf("KindPath(gitlab, %q): ok=false, want true", c.kind)
			continue
		}
		if got != c.want {
			t.Errorf("KindPath(gitlab, %q) = %q, want %q", c.kind, got, c.want)
		}
	}
}

func TestKindPathUnknownReturnsFalse(t *testing.T) {
	if _, ok := ciinstall.KindPath("github", "nonexistent"); ok {
		t.Error("expected ok=false for unknown kind")
	}
	if _, ok := ciinstall.KindPath("unknown-forge", "gate"); ok {
		t.Error("expected ok=false for unknown forge")
	}
}

// ---------- Install -----------------------------------------------------------

func TestInstallWritesFile(t *testing.T) {
	root := t.TempDir()
	ci := fakeCIWith("gate")

	res, err := ciinstall.Install(root, "github", ci, "gate")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Action != ciinstall.ActionInstalled {
		t.Errorf("action = %q, want %q", res.Action, ciinstall.ActionInstalled)
	}
	wantPath := filepath.Join(".github", "workflows", "koryph-gate.yml")
	if res.Path != wantPath {
		t.Errorf("path = %q, want %q", res.Path, wantPath)
	}
	// File must exist with the correct content.
	got, err := os.ReadFile(filepath.Join(root, wantPath))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(got), "kind=gate") {
		t.Errorf("file content missing expected marker: %s", got)
	}
}

func TestInstallIdempotent(t *testing.T) {
	root := t.TempDir()
	ci := fakeCIWith("gate")

	// First install.
	if _, err := ciinstall.Install(root, "github", ci, "gate"); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	// Second install must be a no-op.
	res, err := ciinstall.Install(root, "github", ci, "gate")
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if res.Action != ciinstall.ActionUnchanged {
		t.Errorf("second install action = %q, want %q", res.Action, ciinstall.ActionUnchanged)
	}
}

func TestInstallUnsupportedKind(t *testing.T) {
	root := t.TempDir()
	ci := fakeCIWith("gate") // scanner is not in the fake

	res, err := ciinstall.Install(root, "github", ci, "scanner")
	if err != nil {
		t.Fatalf("Install unsupported: %v", err)
	}
	if res.Action != ciinstall.ActionUnsupported {
		t.Errorf("action = %q, want %q", res.Action, ciinstall.ActionUnsupported)
	}
}

func TestInstallCreatesParentDirectory(t *testing.T) {
	root := t.TempDir()
	ci := fakeCIWith("gate")

	// .github/workflows/ does not exist yet.
	if _, err := ciinstall.Install(root, "github", ci, "gate"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".github", "workflows")); err != nil {
		t.Errorf("parent dir not created: %v", err)
	}
}

func TestInstallGitLab(t *testing.T) {
	root := t.TempDir()
	ci := fakeCIWith("gate")

	res, err := ciinstall.Install(root, "gitlab", ci, "gate")
	if err != nil {
		t.Fatalf("Install gitlab gate: %v", err)
	}
	if res.Action != ciinstall.ActionInstalled {
		t.Errorf("action = %q, want installed", res.Action)
	}
	wantPath := filepath.Join(".koryph", "ci", "koryph-gate.yml")
	if res.Path != wantPath {
		t.Errorf("path = %q, want %q", res.Path, wantPath)
	}
}

// ---------- Check -------------------------------------------------------------

func TestCheckNoDrift(t *testing.T) {
	root := t.TempDir()
	ci := fakeCIWith("gate")

	// Install first, then check.
	if _, err := ciinstall.Install(root, "github", ci, "gate"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	res, err := ciinstall.Check(root, "github", ci, "gate")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.HasDrift {
		t.Errorf("expected no drift after install, got drift")
	}
}

func TestCheckDriftWhenAbsent(t *testing.T) {
	root := t.TempDir()
	ci := fakeCIWith("gate")

	res, err := ciinstall.Check(root, "github", ci, "gate")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.HasDrift {
		t.Errorf("expected drift when file is absent")
	}
}

func TestCheckDriftWhenContentDiffers(t *testing.T) {
	root := t.TempDir()
	ci := fakeCIWith("gate")

	// Write a stale version.
	wantPath := filepath.Join(root, ".github", "workflows", "koryph-gate.yml")
	if err := os.MkdirAll(filepath.Dir(wantPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantPath, []byte("# stale content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ciinstall.Check(root, "github", ci, "gate")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.HasDrift {
		t.Errorf("expected drift when content differs")
	}
}

func TestCheckUnsupportedKind(t *testing.T) {
	root := t.TempDir()
	ci := fakeCIWith("gate")

	res, err := ciinstall.Check(root, "github", ci, "scanner")
	if err != nil {
		t.Fatalf("Check unsupported: %v", err)
	}
	if res.Action != ciinstall.ActionUnsupported {
		t.Errorf("action = %q, want %q", res.Action, ciinstall.ActionUnsupported)
	}
	if res.HasDrift {
		t.Errorf("unsupported kinds must not report drift")
	}
}

// ---------- AllKinds ---------------------------------------------------------

func TestAllKindsContainsGate(t *testing.T) {
	for _, k := range ciinstall.AllKinds {
		if k == "gate" {
			return
		}
	}
	t.Errorf("AllKinds does not contain 'gate': %v", ciinstall.AllKinds)
}

// compile-time: fakeCI satisfies forge.CIService
var _ forge.CIService = (*fakeCI)(nil)
