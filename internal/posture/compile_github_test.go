// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package posture — internal tests for the GitHub compiler.
// These tests are fixture-locked: the compiler output (after normalisation)
// must be byte-identical to the golden files embedded in the oss-solo-maintainer
// built-in profile.  If the compiler drifts from the golden fixtures, these
// tests fail and must be updated intentionally.
package posture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ossSoloMaintainerIntents returns the canonical Intents for the
// oss-solo-maintainer profile.  These values must match what manifest.json
// carries under the "intents" key.
func ossSoloMaintainerIntents() Intents {
	f := false
	t := true
	return Intents{
		RequireApprovals:             1,
		RequireSignedCommits:         true,
		NoForcePush:                  true,
		NoDeletion:                   true,
		SecretScanning:               true,
		SecretScanningPushProtection: true,
		DependabotSecurityUpdates:    true,
		VulnerabilityAlerts:          true,
		AllowMergeCommit:             &f,
		AllowSquashMerge:             &t,
		AllowRebaseMerge:             &t,
		AllowAutoMerge:               &f,
		DeleteBranchOnMerge:          true,
		AllowUpdateBranch:            true,
		WebCommitSignoffRequired:     true,
		ActionsDefaultPermissions:    "read",
		ActionsCanApprovePRs:         true,
	}
}

// ─── helper assertions ───────────────────────────────────────────────────────

// assertNormalisedRulesetEqual normalises both raw JSON blobs with
// normalizeRuleset and asserts they are byte-equal.
func assertNormalisedRulesetEqual(t *testing.T, label string, golden, got []byte) {
	t.Helper()
	wantNorm, err := normalizeRuleset(golden)
	if err != nil {
		t.Fatalf("%s: normalise golden: %v", label, err)
	}
	gotNorm, err := normalizeRuleset(got)
	if err != nil {
		t.Fatalf("%s: normalise compiled: %v", label, err)
	}
	if string(wantNorm) != string(gotNorm) {
		t.Errorf("%s: compiled output differs from golden fixture after normalisation.\n"+
			"--- golden ---\n%s\n--- compiled ---\n%s", label, wantNorm, gotNorm)
	}
}

// assertNormalisedSettingsSectionEqual normalises two JSON blobs with
// jsonSortKeys and asserts they are byte-equal.
func assertNormalisedSettingsSectionEqual(t *testing.T, label string, golden, got []byte) {
	t.Helper()
	wantNorm, err := jsonSortKeys(golden)
	if err != nil {
		t.Fatalf("%s: normalise golden: %v", label, err)
	}
	gotNorm, err := jsonSortKeys(got)
	if err != nil {
		t.Fatalf("%s: normalise compiled: %v", label, err)
	}
	if string(wantNorm) != string(gotNorm) {
		t.Errorf("%s: compiled output differs from golden fixture after normalisation.\n"+
			"--- golden ---\n%s\n--- compiled ---\n%s", label, wantNorm, gotNorm)
	}
}

// readFile reads a file and returns its contents, failing the test on error.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// renderGoldenTemplate renders the oss-solo-maintainer pr-checks.json.tmpl
// with the given template data and returns the rendered bytes.
func renderGoldenTemplate(t *testing.T, data profileTemplateData) []byte {
	t.Helper()
	raw, err := builtinFS.ReadFile("builtin/oss-solo-maintainer/rulesets/pr-checks.json.tmpl")
	if err != nil {
		t.Fatalf("read pr-checks.json.tmpl: %v", err)
	}
	rendered, err := renderTemplate("pr-checks.json.tmpl", raw, data)
	if err != nil {
		t.Fatalf("render pr-checks.json.tmpl: %v", err)
	}
	return rendered
}

// readGoldenFile reads a file from the builtin embed FS.
func readGoldenFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := builtinFS.ReadFile("builtin/" + path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	return data
}

// ─── fixture-locked tests ────────────────────────────────────────────────────

// TestCompileGitHub_SignedCommits_MatchesGolden verifies that the GitHub compiler
// produces a signed-commits ruleset that normalises to the same form as the
// golden fixture embedded in the oss-solo-maintainer built-in profile.
func TestCompileGitHub_SignedCommits_MatchesGolden(t *testing.T) {
	golden := readGoldenFile(t, "oss-solo-maintainer/rulesets/signed-commits.json")

	dir := t.TempDir()
	if err := CompileGitHub(ossSoloMaintainerIntents(), nil, dir); err != nil {
		t.Fatalf("CompileGitHub: %v", err)
	}

	got := readFile(t, filepath.Join(dir, "rulesets", "signed-commits.json"))
	assertNormalisedRulesetEqual(t, "signed-commits", golden, got)
}

// TestCompileGitHub_PRChecks_NoChecks_MatchesGolden verifies that without
// required_checks the pr-checks ruleset normalises to the same form as the
// golden fixture rendered from the template with no checks.
func TestCompileGitHub_PRChecks_NoChecks_MatchesGolden(t *testing.T) {
	golden := renderGoldenTemplate(t, profileTemplateData{}) // no RequiredChecks

	dir := t.TempDir()
	if err := CompileGitHub(ossSoloMaintainerIntents(), nil, dir); err != nil {
		t.Fatalf("CompileGitHub: %v", err)
	}

	got := readFile(t, filepath.Join(dir, "rulesets", "pr-checks.json"))
	assertNormalisedRulesetEqual(t, "pr-checks (no checks)", golden, got)
}

// TestCompileGitHub_PRChecks_WithChecks_MatchesGolden verifies that with
// required_checks the pr-checks ruleset normalises to the same form as the
// golden fixture rendered from the template with those checks.
func TestCompileGitHub_PRChecks_WithChecks_MatchesGolden(t *testing.T) {
	params := map[string]string{"required_checks": "pre-commit,make gate"}
	checks := []statusCheck{
		{Context: "pre-commit"},
		{Context: "make gate"},
	}
	golden := renderGoldenTemplate(t, profileTemplateData{RequiredChecks: checks})

	dir := t.TempDir()
	if err := CompileGitHub(ossSoloMaintainerIntents(), params, dir); err != nil {
		t.Fatalf("CompileGitHub: %v", err)
	}

	got := readFile(t, filepath.Join(dir, "rulesets", "pr-checks.json"))
	assertNormalisedRulesetEqual(t, "pr-checks (with checks)", golden, got)
}

// TestCompileGitHub_RepoSettings_MatchesGolden verifies that the compiled
// repo-settings.json matches the golden fixture section-by-section after
// jsonSortKeys normalisation.
func TestCompileGitHub_RepoSettings_MatchesGolden(t *testing.T) {
	goldenRaw := readGoldenFile(t, "oss-solo-maintainer/repo-settings.json")
	var golden struct {
		Repo                json.RawMessage `json:"repo"`
		SecurityAndAnalysis json.RawMessage `json:"security_and_analysis"`
		VulnerabilityAlerts *bool           `json:"vulnerability_alerts"`
		ActionsWorkflow     json.RawMessage `json:"actions_workflow"`
	}
	if err := json.Unmarshal(goldenRaw, &golden); err != nil {
		t.Fatalf("parse golden repo-settings: %v", err)
	}

	dir := t.TempDir()
	if err := CompileGitHub(ossSoloMaintainerIntents(), nil, dir); err != nil {
		t.Fatalf("CompileGitHub: %v", err)
	}

	gotRaw := readFile(t, filepath.Join(dir, "repo-settings.json"))
	var got struct {
		Repo                json.RawMessage `json:"repo"`
		SecurityAndAnalysis json.RawMessage `json:"security_and_analysis"`
		VulnerabilityAlerts *bool           `json:"vulnerability_alerts"`
		ActionsWorkflow     json.RawMessage `json:"actions_workflow"`
	}
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatalf("parse compiled repo-settings: %v", err)
	}

	// Compare each section.
	assertNormalisedSettingsSectionEqual(t, "repo", golden.Repo, got.Repo)
	assertNormalisedSettingsSectionEqual(t, "security_and_analysis", golden.SecurityAndAnalysis, got.SecurityAndAnalysis)
	assertNormalisedSettingsSectionEqual(t, "actions_workflow", golden.ActionsWorkflow, got.ActionsWorkflow)

	// vulnerability_alerts is a scalar.
	if golden.VulnerabilityAlerts == nil || got.VulnerabilityAlerts == nil {
		if golden.VulnerabilityAlerts != got.VulnerabilityAlerts {
			t.Errorf("vulnerability_alerts: golden=%v compiled=%v", golden.VulnerabilityAlerts, got.VulnerabilityAlerts)
		}
	} else if *golden.VulnerabilityAlerts != *got.VulnerabilityAlerts {
		t.Errorf("vulnerability_alerts: golden=%v compiled=%v", *golden.VulnerabilityAlerts, *got.VulnerabilityAlerts)
	}
}

// TestCompileGitHub_NoRequiredChecksRuleType verifies that when required_checks
// is empty, the compiled pr-checks.json does not contain the
// "type": "required_status_checks" rule.
func TestCompileGitHub_NoRequiredChecksRuleType(t *testing.T) {
	dir := t.TempDir()
	if err := CompileGitHub(ossSoloMaintainerIntents(), nil, dir); err != nil {
		t.Fatalf("CompileGitHub: %v", err)
	}
	got := readFile(t, filepath.Join(dir, "rulesets", "pr-checks.json"))
	if strings.Contains(string(got), `"type": "required_status_checks"`) {
		t.Errorf("expected no required_status_checks rule type when no checks given; got:\n%s", got)
	}
}

// TestCompileGitHub_WithChecksRuleType verifies that required_status_checks
// appears in the compiled pr-checks.json when checks are given.
func TestCompileGitHub_WithChecksRuleType(t *testing.T) {
	dir := t.TempDir()
	params := map[string]string{"required_checks": "pre-commit,make gate"}
	if err := CompileGitHub(ossSoloMaintainerIntents(), params, dir); err != nil {
		t.Fatalf("CompileGitHub: %v", err)
	}
	got := string(readFile(t, filepath.Join(dir, "rulesets", "pr-checks.json")))
	if !strings.Contains(got, `"required_status_checks"`) {
		t.Errorf("expected required_status_checks in compiled output; got:\n%s", got)
	}
	if !strings.Contains(got, "pre-commit") {
		t.Errorf("expected pre-commit in compiled output; got:\n%s", got)
	}
	if !strings.Contains(got, "make gate") {
		t.Errorf("expected 'make gate' in compiled output; got:\n%s", got)
	}
}

// TestCompileGitHub_MinimalIntents verifies that CompileGitHub does not panic or
// error on a minimal (near-empty) Intents struct and does not produce any files
// when no intents are set.
func TestCompileGitHub_MinimalIntents(t *testing.T) {
	dir := t.TempDir()
	if err := CompileGitHub(Intents{}, nil, dir); err != nil {
		t.Fatalf("CompileGitHub with empty intents: %v", err)
	}
	// No files should be produced.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty dir for empty intents; got %v", entries)
	}
}

// TestCompileGitHub_ParamOverridesIntentChecks verifies that the runtime param
// "required_checks" overrides any checks specified in the intents struct.
func TestCompileGitHub_ParamOverridesIntentChecks(t *testing.T) {
	intents := Intents{
		RequireApprovals: 1,
		RequiredChecks:   []string{"original-check"},
	}
	params := map[string]string{"required_checks": "override-check"}
	dir := t.TempDir()
	if err := CompileGitHub(intents, params, dir); err != nil {
		t.Fatalf("CompileGitHub: %v", err)
	}
	got := string(readFile(t, filepath.Join(dir, "rulesets", "pr-checks.json")))
	if !strings.Contains(got, "override-check") {
		t.Errorf("expected override-check in output; got:\n%s", got)
	}
	if strings.Contains(got, "original-check") {
		t.Errorf("expected original-check to be overridden; got:\n%s", got)
	}
}
