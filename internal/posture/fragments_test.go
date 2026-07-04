// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/posture"
)

// --- ListFragments -----------------------------------------------------------

func TestListFragments_ContainsBuiltins(t *testing.T) {
	frags, err := posture.ListFragments()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{
		"gitleaks":          false,
		"govulncheck":       false,
		"license-allowlist": false,
	}
	for _, f := range frags {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("fragment %q not found in ListFragments; got: %v", name, frags)
		}
	}
}

func TestListFragments_ManifestParsed(t *testing.T) {
	frags, err := posture.ListFragments()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, f := range frags {
		if f.Manifest.Name == "" {
			t.Errorf("fragment %q has empty manifest Name", f.Name)
		}
		if f.Manifest.Description == "" {
			t.Errorf("fragment %q has empty manifest Description", f.Name)
		}
		if len(f.Manifest.InstalledFiles) == 0 {
			t.Errorf("fragment %q has no InstalledFiles", f.Name)
		}
	}
}

// --- ListBuiltins includes RecommendedFragments for oss-solo-maintainer ----

func TestOssSoloMaintainer_RecommendedFragments(t *testing.T) {
	entries, err := posture.ListBuiltins()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found posture.ProfileEntry
	for _, e := range entries {
		if e.Name == "oss-solo-maintainer" {
			found = e
			break
		}
	}
	if found.Name == "" {
		t.Fatal("oss-solo-maintainer not found")
	}
	if len(found.Manifest.RecommendedFragments) == 0 {
		t.Error("oss-solo-maintainer should declare recommended_fragments")
	}
	// gitleaks and govulncheck must be listed.
	rf := map[string]bool{}
	for _, r := range found.Manifest.RecommendedFragments {
		rf[r] = true
	}
	for _, want := range []string{"gitleaks", "govulncheck"} {
		if !rf[want] {
			t.Errorf("oss-solo-maintainer.recommended_fragments missing %q; got %v",
				want, found.Manifest.RecommendedFragments)
		}
	}
}

// --- RecommendedFragments helper -------------------------------------------

func TestRecommendedFragments_KnownProfile(t *testing.T) {
	frags, err := posture.RecommendedFragments("oss-solo-maintainer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(frags) == 0 {
		t.Error("expected non-empty recommended fragments for oss-solo-maintainer")
	}
}

func TestRecommendedFragments_UnknownProfile(t *testing.T) {
	frags, err := posture.RecommendedFragments("no-such-profile")
	if err != nil {
		t.Errorf("unexpected error for unknown profile: %v", err)
	}
	if frags != nil {
		t.Errorf("expected nil for unknown profile; got %v", frags)
	}
}

// --- CheckFragments ----------------------------------------------------------

func TestCheckFragments_AllMissing(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	drift, err := posture.CheckFragments(root, []string{"gitleaks"}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drift {
		t.Error("expected drift=true when fragment files are absent")
	}
	out := buf.String()
	if !strings.Contains(out, "MISSING") {
		t.Errorf("expected MISSING in output; got:\n%s", out)
	}
	if !strings.Contains(out, "fragment:gitleaks") {
		t.Errorf("expected fragment name in output; got:\n%s", out)
	}
}

func TestCheckFragments_AllInstalled(t *testing.T) {
	root := t.TempDir()
	// Install the gitleaks workflow file manually.
	wfDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a fresh copy via ApplyFragments so the hash matches.
	var applyBuf bytes.Buffer
	if _, err := posture.ApplyFragments(root, []string{"gitleaks"}, false, &applyBuf); err != nil {
		t.Fatalf("ApplyFragments setup: %v", err)
	}

	var checkBuf bytes.Buffer
	drift, err := posture.CheckFragments(root, []string{"gitleaks"}, &checkBuf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drift {
		t.Errorf("expected no drift after applying; output:\n%s", checkBuf.String())
	}
	if !strings.Contains(checkBuf.String(), "OK") {
		t.Errorf("expected OK in output; got:\n%s", checkBuf.String())
	}
}

func TestCheckFragments_StaleFile(t *testing.T) {
	root := t.TempDir()
	wfDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a stale (wrong) content.
	if err := os.WriteFile(filepath.Join(wfDir, "gitleaks.yml"), []byte("# stale content"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	drift, err := posture.CheckFragments(root, []string{"gitleaks"}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drift {
		t.Error("expected drift=true for stale file")
	}
	if !strings.Contains(buf.String(), "DRIFT") {
		t.Errorf("expected DRIFT in output; got:\n%s", buf.String())
	}
}

func TestCheckFragments_UnknownFragment(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	drift, err := posture.CheckFragments(root, []string{"no-such-fragment"}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drift {
		t.Error("expected drift=true for unknown fragment")
	}
	if !strings.Contains(buf.String(), "MISSING") {
		t.Errorf("expected MISSING for unknown fragment; got:\n%s", buf.String())
	}
}

// --- ApplyFragments ----------------------------------------------------------

func TestApplyFragments_InstallsMissingFiles(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	drift, err := posture.ApplyFragments(root, []string{"gitleaks"}, false, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drift {
		t.Error("expected drift=true since files were missing before install")
	}
	out := buf.String()
	if !strings.Contains(out, "CREATED") {
		t.Errorf("expected CREATED in output; got:\n%s", out)
	}
	// File must exist after apply.
	wfPath := filepath.Join(root, ".github", "workflows", "gitleaks.yml")
	if _, err := os.Stat(wfPath); err != nil {
		t.Errorf("workflow file not installed: %v", err)
	}
}

func TestApplyFragments_StaleNoForce(t *testing.T) {
	root := t.TempDir()
	wfDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write stale content.
	stalePath := filepath.Join(wfDir, "gitleaks.yml")
	if err := os.WriteFile(stalePath, []byte("# stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	_, err := posture.ApplyFragments(root, []string{"gitleaks"}, false, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	// Should report DRIFT but NOT overwrite.
	if !strings.Contains(out, "DRIFT") {
		t.Errorf("expected DRIFT for stale file without --force; got:\n%s", out)
	}
	// Content should remain stale.
	after, _ := os.ReadFile(stalePath)
	if string(after) != "# stale" {
		t.Errorf("stale file was overwritten without --force")
	}
}

func TestApplyFragments_StaleWithForce(t *testing.T) {
	root := t.TempDir()
	wfDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(wfDir, "gitleaks.yml")
	if err := os.WriteFile(stalePath, []byte("# stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	_, err := posture.ApplyFragments(root, []string{"gitleaks"}, true, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "UPDATED") {
		t.Errorf("expected UPDATED for stale file with --force; got:\n%s", out)
	}
	// Content should now match the embedded version (non-stale).
	after, _ := os.ReadFile(stalePath)
	if string(after) == "# stale" {
		t.Error("stale file was NOT overwritten with --force")
	}
}

func TestApplyFragments_Idempotent(t *testing.T) {
	root := t.TempDir()
	// First apply — installs.
	if _, err := posture.ApplyFragments(root, []string{"govulncheck"}, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Second apply — should be all OK, no drift.
	var buf bytes.Buffer
	drift, err := posture.ApplyFragments(root, []string{"govulncheck"}, false, &buf)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if drift {
		t.Errorf("expected no drift on second apply; output:\n%s", buf.String())
	}
}

func TestApplyFragments_LicenseAllowlist_InstallsAllFiles(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	if _, err := posture.ApplyFragments(root, []string{"license-allowlist"}, false, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Both installed_files should be present.
	for _, rel := range []string{
		".github/workflows/license-check.yml",
		"scripts/allowed-licenses.txt",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected installed file %s; stat error: %v", rel, err)
		}
	}
}

// --- CheckFragmentDrift (structured output for doctor) ----------------------

func TestCheckFragmentDrift_AllMissing(t *testing.T) {
	root := t.TempDir()
	result, err := posture.CheckFragmentDrift(root, []string{"gitleaks"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Drifts) == 0 {
		t.Fatal("expected at least one drift entry")
	}
	found := false
	for _, d := range result.Drifts {
		if d.Fragment == "gitleaks" && d.HasDrift {
			found = true
		}
	}
	if !found {
		t.Errorf("expected gitleaks drift; got %+v", result.Drifts)
	}
}

func TestCheckFragmentDrift_Installed(t *testing.T) {
	root := t.TempDir()
	// Install first.
	if _, err := posture.ApplyFragments(root, []string{"gitleaks"}, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	result, err := posture.CheckFragmentDrift(root, []string{"gitleaks"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, d := range result.Drifts {
		if d.Fragment == "gitleaks" && d.HasDrift {
			t.Errorf("expected no drift for installed gitleaks; got %+v", d)
		}
	}
}
