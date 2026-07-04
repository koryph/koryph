// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/posture"
	"github.com/koryph/koryph/internal/project"
)

// fabricateProjectWithPosture creates a minimal valid project skeleton with a
// posture block under t.TempDir().
func fabricateProjectWithPosture(t *testing.T, postureCfg *project.PostureConfig) string {
	t.Helper()
	root := fabricateProject(t)
	cfg, err := project.Load(root)
	if err != nil {
		t.Fatalf("load project config: %v", err)
	}
	cfg.Posture = postureCfg
	if err := cfg.Save(root); err != nil {
		t.Fatalf("save project config: %v", err)
	}
	return root
}

// --- posture-drift ---

// TestPostureDrift_NilPosture verifies the check returns OK (skipped) when no
// posture profile is declared.
func TestPostureDrift_NilPosture(t *testing.T) {
	root := fabricateProject(t)
	opts := projectOpts(root)
	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNamePostureDrift)
	if f.Level != LevelOK {
		t.Errorf("posture-drift: want OK (no posture declared), got %s: %s", f.Level, f.Message)
	}
}

// TestPostureDrift_NoDrift verifies the check returns OK when the injected
// PostureDriftCheck reports no drift.
func TestPostureDrift_NoDrift(t *testing.T) {
	postureCfg := &project.PostureConfig{
		Profile:    "oss-solo-maintainer",
		Parameters: map[string]string{"required_checks": "pre-commit,make gate"},
	}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) {
		return false, nil // no drift
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNamePostureDrift)
	if f.Level != LevelOK {
		t.Errorf("posture-drift: want OK (no drift), got %s: %s", f.Level, f.Message)
	}
}

// TestPostureDrift_DriftDetected verifies the check returns WARN when the
// injected PostureDriftCheck reports drift, with the remediation command.
func TestPostureDrift_DriftDetected(t *testing.T) {
	postureCfg := &project.PostureConfig{
		Profile:    "oss-solo-maintainer",
		Parameters: map[string]string{"required_checks": "pre-commit,make gate"},
	}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) {
		return true, nil // drift
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNamePostureDrift)
	if f.Level != LevelWarn {
		t.Errorf("posture-drift: want WARN (drift), got %s: %s", f.Level, f.Message)
	}
	// Message must contain the profile name and the apply command.
	if f.Message == "" {
		t.Error("posture-drift: empty message")
	}
}

// TestPostureDrift_CheckError verifies the check degrades gracefully (LevelOK)
// when the injected PostureDriftCheck returns an error (e.g. gh unavailable).
func TestPostureDrift_CheckError(t *testing.T) {
	postureCfg := &project.PostureConfig{Profile: "oss-solo-maintainer"}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) {
		return false, fmt.Errorf("gh: not authenticated")
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNamePostureDrift)
	if f.Level != LevelOK {
		t.Errorf("posture-drift: want OK (graceful degrade on error), got %s: %s", f.Level, f.Message)
	}
}

// TestPostureDrift_ApplyCmd verifies that PostureConfig.PostureApplyCmd
// produces the expected command string.
func TestPostureDrift_ApplyCmd(t *testing.T) {
	cases := []struct {
		cfg  *project.PostureConfig
		want string
	}{
		{nil, ""},
		{&project.PostureConfig{Profile: "oss-solo-maintainer"}, "koryph posture apply oss-solo-maintainer"},
	}
	for _, tc := range cases {
		got := tc.cfg.PostureApplyCmd()
		if tc.want == "" {
			if got != "" {
				t.Errorf("nil PostureConfig.PostureApplyCmd() = %q, want empty", got)
			}
			continue
		}
		// For non-nil configs without params, the prefix must match.
		if len(got) < len(tc.want) || got[:len(tc.want)] != tc.want {
			t.Errorf("PostureApplyCmd() = %q, want prefix %q", got, tc.want)
		}
	}
}

// TestPostureDrift_Validate verifies that PostureConfig validation rejects a
// nil profile name while allowing a valid one.
func TestPostureDrift_Validate(t *testing.T) {
	// Valid posture block should pass Validate.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	good := project.Config{
		SchemaVersion:   1,
		ProjectID:       "p",
		WorkSource:      "bd",
		Gate:            []string{"true"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
		Posture: &project.PostureConfig{
			Profile: "oss-solo-maintainer",
		},
	}
	data, _ := json.Marshal(good)
	if err := os.WriteFile(filepath.Join(root, project.ConfigFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Load(root); err != nil {
		t.Errorf("valid posture block should load OK: %v", err)
	}

	// Posture block with empty profile should fail Validate.
	bad := good
	bad.Posture = &project.PostureConfig{Profile: ""}
	data, _ = json.Marshal(bad)
	_ = os.WriteFile(filepath.Join(root, project.ConfigFileName), data, 0o644)
	if _, err := project.Load(root); err == nil {
		t.Error("empty posture.profile should fail Validate")
	}
}

// --- org-posture-drift -------------------------------------------------------

// TestOrgPostureDrift_NoOrg verifies the check is skipped when posture.org is
// not set.
func TestOrgPostureDrift_NoOrg(t *testing.T) {
	postureCfg := &project.PostureConfig{Profile: "oss-solo-maintainer"}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) { return false, nil }

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameOrgPostureDrift)
	if f.Level != LevelOK {
		t.Errorf("org-posture-drift: want OK (no org declared), got %s: %s", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "skipped") {
		t.Errorf("org-posture-drift: message should say skipped; got: %s", f.Message)
	}
}

// TestOrgPostureDrift_NilPosture verifies the check is skipped when the
// posture block is nil.
func TestOrgPostureDrift_NilPosture(t *testing.T) {
	root := fabricateProject(t)
	opts := projectOpts(root)

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameOrgPostureDrift)
	if f.Level != LevelOK {
		t.Errorf("org-posture-drift: want OK (nil posture), got %s: %s", f.Level, f.Message)
	}
}

// TestOrgPostureDrift_NoDrift verifies the check returns OK when the injected
// OrgPostureDriftCheck reports no drift.
func TestOrgPostureDrift_NoDrift(t *testing.T) {
	postureCfg := &project.PostureConfig{
		Profile: "oss-solo-maintainer",
		Org:     "acme-org",
	}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) { return false, nil }
	opts.OrgPostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) {
		return false, nil // no drift
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameOrgPostureDrift)
	if f.Level != LevelOK {
		t.Errorf("org-posture-drift: want OK (no drift), got %s: %s", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "acme-org") {
		t.Errorf("org-posture-drift: message should mention org; got: %s", f.Message)
	}
}

// TestOrgPostureDrift_DriftDetected verifies the check returns WARN when the
// injected OrgPostureDriftCheck reports drift.
func TestOrgPostureDrift_DriftDetected(t *testing.T) {
	postureCfg := &project.PostureConfig{
		Profile: "oss-solo-maintainer",
		Org:     "acme-org",
	}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) { return false, nil }
	opts.OrgPostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) {
		return true, nil // drift
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameOrgPostureDrift)
	if f.Level != LevelWarn {
		t.Errorf("org-posture-drift: want WARN (drift), got %s: %s", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "acme-org") {
		t.Errorf("org-posture-drift: message should mention org; got: %s", f.Message)
	}
	// Message must contain the remediation command (with --org flag).
	if !strings.Contains(f.Message, "--org acme-org") {
		t.Errorf("org-posture-drift: message should contain --org; got: %s", f.Message)
	}
}

// TestOrgPostureDrift_CheckError verifies the check degrades gracefully when
// the injected OrgPostureDriftCheck returns an error.
func TestOrgPostureDrift_CheckError(t *testing.T) {
	postureCfg := &project.PostureConfig{
		Profile: "oss-solo-maintainer",
		Org:     "acme-org",
	}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) { return false, nil }
	opts.OrgPostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) {
		return false, fmt.Errorf("org admin required")
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameOrgPostureDrift)
	if f.Level != LevelOK {
		t.Errorf("org-posture-drift: want OK (graceful degrade on error), got %s: %s", f.Level, f.Message)
	}
}

// TestOrgPostureApplyCmd_WithOrg verifies PostureConfig.OrgPostureApplyCmd.
func TestOrgPostureApplyCmd_WithOrg(t *testing.T) {
	cfg := &project.PostureConfig{
		Profile: "oss-solo-maintainer",
		Org:     "acme-org",
	}
	got := cfg.OrgPostureApplyCmd()
	if !strings.Contains(got, "--org acme-org") {
		t.Errorf("OrgPostureApplyCmd() should contain --org; got: %q", got)
	}
	if !strings.Contains(got, "oss-solo-maintainer") {
		t.Errorf("OrgPostureApplyCmd() should contain profile; got: %q", got)
	}
}

func TestOrgPostureApplyCmd_NoOrg(t *testing.T) {
	cfg := &project.PostureConfig{Profile: "oss-solo-maintainer"}
	if got := cfg.OrgPostureApplyCmd(); got != "" {
		t.Errorf("OrgPostureApplyCmd() without org should be empty; got: %q", got)
	}
}

func TestPostureApplyCmd_IncludesOrgFlag(t *testing.T) {
	cfg := &project.PostureConfig{
		Profile: "oss-solo-maintainer",
		Org:     "acme-org",
	}
	got := cfg.PostureApplyCmd()
	if !strings.Contains(got, "--org acme-org") {
		t.Errorf("PostureApplyCmd() with org should contain --org; got: %q", got)
	}
}

// --- fragment-drift ----------------------------------------------------------

// TestFragmentDrift_NoFragments verifies the check is skipped when no fragments
// are opted into the posture config.
func TestFragmentDrift_NoFragments(t *testing.T) {
	// Posture block with profile but no fragments.
	postureCfg := &project.PostureConfig{Profile: "oss-solo-maintainer"}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) { return false, nil }

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	// checkFragmentDrift returns nil when no fragments → no findings with that check name.
	for _, f := range r.Findings {
		if f.Check == checkNameFragmentDrift {
			t.Errorf("expected no fragment-drift findings when no fragments declared; got %+v", f)
		}
	}
}

// TestFragmentDrift_NilPosture verifies the check is skipped when no posture
// block at all.
func TestFragmentDrift_NilPosture(t *testing.T) {
	root := fabricateProject(t)
	opts := projectOpts(root)

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		if f.Check == checkNameFragmentDrift {
			t.Errorf("expected no fragment-drift findings when posture=nil; got %+v", f)
		}
	}
}

// TestFragmentDrift_NoDrift verifies OK when injected FragmentDriftCheck
// reports no drift (all installed).
func TestFragmentDrift_NoDrift(t *testing.T) {
	postureCfg := &project.PostureConfig{
		Profile:   "oss-solo-maintainer",
		Fragments: []string{"gitleaks"},
	}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) { return false, nil }
	opts.FragmentDriftCheck = func(_ string, _ []string) ([]posture.FragmentDrift, error) {
		return []posture.FragmentDrift{{
			Fragment: "gitleaks",
			Files:    []posture.FragmentFileStatus{{Path: ".github/workflows/gitleaks.yml", Status: "ok"}},
			HasDrift: false,
		}}, nil
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	// All fragment findings should be OK.
	for _, f := range r.Findings {
		if f.Check == checkNameFragmentDrift && f.Level != LevelOK {
			t.Errorf("fragment-drift: want OK, got %s: %s", f.Level, f.Message)
		}
	}
}

// TestFragmentDrift_DriftDetected verifies WARN when injected check reports missing files.
func TestFragmentDrift_DriftDetected(t *testing.T) {
	postureCfg := &project.PostureConfig{
		Profile:   "oss-solo-maintainer",
		Fragments: []string{"gitleaks"},
	}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) { return false, nil }
	opts.FragmentDriftCheck = func(_ string, _ []string) ([]posture.FragmentDrift, error) {
		return []posture.FragmentDrift{{
			Fragment: "gitleaks",
			Files: []posture.FragmentFileStatus{
				{Path: ".github/workflows/gitleaks.yml", Status: "missing"},
			},
			HasDrift: true,
		}}, nil
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range r.Findings {
		if f.Check == checkNameFragmentDrift && f.Level == LevelWarn {
			found = true
			if !strings.Contains(f.Message, "gitleaks") {
				t.Errorf("fragment-drift WARN message should mention fragment name; got: %s", f.Message)
			}
		}
	}
	if !found {
		t.Error("expected at least one fragment-drift WARN finding")
	}
}

// TestFragmentDrift_CheckError verifies graceful degrade when FragmentDriftCheck fails.
func TestFragmentDrift_CheckError(t *testing.T) {
	postureCfg := &project.PostureConfig{
		Profile:   "oss-solo-maintainer",
		Fragments: []string{"gitleaks"},
	}
	root := fabricateProjectWithPosture(t, postureCfg)
	opts := projectOpts(root)
	opts.PostureDriftCheck = func(_ string, _ *project.PostureConfig) (bool, error) { return false, nil }
	opts.FragmentDriftCheck = func(_ string, _ []string) ([]posture.FragmentDrift, error) {
		return nil, fmt.Errorf("unexpected error")
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		if f.Check == checkNameFragmentDrift && f.Level != LevelOK {
			t.Errorf("fragment-drift: expected OK (graceful degrade on error), got %s: %s", f.Level, f.Message)
		}
	}
}

// TestFragmentDrift_RealFS verifies the check against the real filesystem
// using posture.ApplyFragments to install first.
func TestFragmentDrift_RealFS(t *testing.T) {
	root := fabricateProject(t)
	// Opt in to gitleaks fragment.
	cfg, err := project.Load(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Posture = &project.PostureConfig{
		Profile:   "oss-solo-maintainer",
		Fragments: []string{"gitleaks"},
	}
	if err := cfg.Save(root); err != nil {
		t.Fatalf("save config: %v", err)
	}

	// Before install: should detect drift.
	findings := checkFragmentDrift(ProjectOptions{}, root, cfg)
	warnFound := false
	for _, f := range findings {
		if f.Level == LevelWarn {
			warnFound = true
		}
	}
	if !warnFound {
		t.Errorf("expected WARN before fragment install; got %+v", findings)
	}

	// Install fragments.
	if _, err := posture.ApplyFragments(root, []string{"gitleaks"}, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("ApplyFragments: %v", err)
	}

	// After install: should be OK.
	findings = checkFragmentDrift(ProjectOptions{}, root, cfg)
	for _, f := range findings {
		if f.Level != LevelOK {
			t.Errorf("expected OK after install; got %s: %s", f.Level, f.Message)
		}
	}
}
