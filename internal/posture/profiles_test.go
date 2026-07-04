// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/posture"
)

// --- ListBuiltins ------------------------------------------------------------

func TestListBuiltins_ContainsOssSoloMaintainer(t *testing.T) {
	entries, err := posture.ListBuiltins()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, e := range entries {
		if e.Name == "oss-solo-maintainer" && e.Source == "builtin" {
			return // found
		}
	}
	t.Errorf("oss-solo-maintainer not found in builtins; got: %v", entries)
}

func TestListBuiltins_ManifestParsed(t *testing.T) {
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
	if found.Manifest.Description == "" {
		t.Error("manifest description is empty")
	}
	if _, ok := found.Manifest.Parameters["required_checks"]; !ok {
		t.Error("manifest missing required_checks parameter descriptor")
	}
}

// --- ListUserProfiles --------------------------------------------------------

func TestListUserProfiles_EmptyDir(t *testing.T) {
	home := t.TempDir()
	entries, err := posture.ListUserProfiles(home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty list; got %v", entries)
	}
}

func TestListUserProfiles_WithProfile(t *testing.T) {
	home := t.TempDir()
	profileDir := filepath.Join(home, "postures", "my-profile")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"name":        "my-profile",
		"description": "A custom profile",
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(profileDir, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := posture.ListUserProfiles(home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry; got %d", len(entries))
	}
	if entries[0].Name != "my-profile" {
		t.Errorf("expected name my-profile; got %s", entries[0].Name)
	}
	if entries[0].Source != "user" {
		t.Errorf("expected source user; got %s", entries[0].Source)
	}
}

// --- RenderProfile -----------------------------------------------------------

func TestRenderProfile_Builtin_NoChecks(t *testing.T) {
	home := t.TempDir()
	src, cleanup, err := posture.RenderProfile("oss-solo-maintainer", nil, home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	// RulesetsDir should exist.
	dir, err := src.RulesetsDir()
	if err != nil {
		t.Fatalf("RulesetsDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	// Expect pr-checks.json and signed-commits.json (rendered, not .tmpl).
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["pr-checks.json"] {
		t.Errorf("pr-checks.json not found; got %v", names)
	}
	if !names["signed-commits.json"] {
		t.Errorf("signed-commits.json not found; got %v", names)
	}
	if names["pr-checks.json.tmpl"] {
		t.Error(".tmpl file should not appear in output dir")
	}

	// RepoSettingsFile should exist.
	if _, err := src.RepoSettingsFile(); err != nil {
		t.Errorf("RepoSettingsFile: %v", err)
	}
}

func TestRenderProfile_Builtin_WithChecks(t *testing.T) {
	home := t.TempDir()
	params := map[string]string{"required_checks": "pre-commit,make gate"}
	src, cleanup, err := posture.RenderProfile("oss-solo-maintainer", params, home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	dir, err := src.RulesetsDir()
	if err != nil {
		t.Fatalf("RulesetsDir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "pr-checks.json"))
	if err != nil {
		t.Fatalf("read pr-checks.json: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "required_status_checks") {
		t.Errorf("expected required_status_checks block when checks are provided; got:\n%s", content)
	}
	if !strings.Contains(content, "pre-commit") {
		t.Errorf("expected pre-commit in required_status_checks; got:\n%s", content)
	}
	if !strings.Contains(content, "make gate") {
		t.Errorf("expected 'make gate' in required_status_checks; got:\n%s", content)
	}

	// Validate the rendered JSON is valid.
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Errorf("rendered pr-checks.json is not valid JSON: %v\ncontent:\n%s", err, content)
	}
}

func TestRenderProfile_Builtin_NoChecks_NoStatusChecksRule(t *testing.T) {
	// When no required_checks param is given, the required_status_checks rule
	// object (with "type": "required_status_checks") should be absent from the
	// rules array of pr-checks.json.  The _rule_descriptions metadata field may
	// still reference the key by name; the test checks for the rule type marker
	// rather than the bare string.
	home := t.TempDir()
	src, cleanup, err := posture.RenderProfile("oss-solo-maintainer", nil, home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	dir, err := src.RulesetsDir()
	if err != nil {
		t.Fatalf("RulesetsDir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "pr-checks.json"))
	if err != nil {
		t.Fatalf("read pr-checks.json: %v", err)
	}

	// Must be valid JSON.
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Errorf("rendered pr-checks.json is not valid JSON: %v", err)
	}

	// The rule type marker ("type": "required_status_checks") must not appear
	// in the rules array when no checks are parameterised.
	if strings.Contains(string(raw), `"type": "required_status_checks"`) {
		t.Errorf("expected no required_status_checks rule type when no checks given; got:\n%s", raw)
	}
}

func TestRenderProfile_Unknown(t *testing.T) {
	home := t.TempDir()
	_, cleanup, err := posture.RenderProfile("no-such-profile", nil, home)
	if err == nil {
		cleanup()
		t.Error("expected error for unknown profile")
	}
	if !strings.Contains(err.Error(), "no-such-profile") {
		t.Errorf("error should mention profile name; got: %v", err)
	}
}

func TestRenderProfile_UserOverridesBuiltin(t *testing.T) {
	home := t.TempDir()
	// Create a user profile named oss-solo-maintainer that overrides the builtin.
	profileDir := filepath.Join(home, "postures", "oss-solo-maintainer")
	if err := os.MkdirAll(filepath.Join(profileDir, "rulesets"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"name":        "oss-solo-maintainer",
		"description": "user override",
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(profileDir, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Write a custom ruleset.
	rulesetData, _ := json.Marshal(map[string]interface{}{
		"name": "custom-user-rule", "enforcement": "active", "target": "branch",
	})
	if err := os.WriteFile(filepath.Join(profileDir, "rulesets", "custom.json"), rulesetData, 0o644); err != nil {
		t.Fatal(err)
	}

	src, cleanup, err := posture.RenderProfile("oss-solo-maintainer", nil, home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	dir, err := src.RulesetsDir()
	if err != nil {
		t.Fatalf("RulesetsDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}
	// User profile only has custom.json — not the builtin files.
	if !names["custom.json"] {
		t.Errorf("expected custom.json from user profile; got %v", names)
	}
	if names["signed-commits.json"] {
		t.Error("builtin signed-commits.json should not appear when user profile overrides")
	}
}

// --- EjectCheck --------------------------------------------------------------

func TestEjectCheck_Neither(t *testing.T) {
	root := t.TempDir()
	hasRulesets, hasSettings := posture.EjectCheck(root)
	if hasRulesets || hasSettings {
		t.Errorf("expected both false; got rulesets=%v settings=%v", hasRulesets, hasSettings)
	}
}

func TestEjectCheck_BothPresent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github", "rulesets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "repo-settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hasRulesets, hasSettings := posture.EjectCheck(root)
	if !hasRulesets {
		t.Error("expected hasRulesets=true")
	}
	if !hasSettings {
		t.Error("expected hasSettings=true")
	}
}

func TestEjectCheck_OnlyRulesets(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github", "rulesets"), 0o755); err != nil {
		t.Fatal(err)
	}
	hasRulesets, hasSettings := posture.EjectCheck(root)
	if !hasRulesets {
		t.Error("expected hasRulesets=true")
	}
	if hasSettings {
		t.Error("expected hasSettings=false")
	}
}
