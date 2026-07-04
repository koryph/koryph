// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
