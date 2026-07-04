// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/posture"
)

// --- LocalSource tests -------------------------------------------------------

func TestLocalSource_RulesetsDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github", "rulesets"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := posture.LocalSource{Root: root}
	dir, err := src.RulesetsDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(dir, ".github/rulesets") {
		t.Errorf("unexpected dir %q", dir)
	}
}

func TestLocalSource_RulesetsDir_Missing(t *testing.T) {
	root := t.TempDir()
	src := posture.LocalSource{Root: root}
	if _, err := src.RulesetsDir(); err == nil {
		t.Error("expected error when rulesets dir is absent")
	}
}

func TestLocalSource_RepoSettingsFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "repo-settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	src := posture.LocalSource{Root: root}
	f, err := src.RepoSettingsFile()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(f, "repo-settings.json") {
		t.Errorf("unexpected path %q", f)
	}
}

func TestLocalSource_RepoSettingsFile_Missing(t *testing.T) {
	root := t.TempDir()
	src := posture.LocalSource{Root: root}
	if _, err := src.RepoSettingsFile(); err == nil {
		t.Error("expected error when settings file is absent")
	}
}

// --- JSON normalization tests ------------------------------------------------

// normalizeRuleset is exported in tests via the package-level exported helper
// below; we test via CheckRulesets / ApplyRulesets to keep the export surface
// minimal.

// --- CheckRulesets / ApplyRulesets integration tests with fake gh ---------------

// fakeGH writes a shell script to a temp dir and sets KORYPH_GH_BIN.
// The script dispatches on "$1 $2 $3 ..." patterns.
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_GH_BIN", bin)
	return bin
}

// rulesetSource creates a temp directory with a .github/rulesets/ directory
// and writes the given ruleset files into it.
func rulesetSource(t *testing.T, files map[string]interface{}) posture.LocalSource {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "rulesets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		data, err := json.MarshalIndent(content, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return posture.LocalSource{Root: root}
}

func TestCheckRulesets_OK(t *testing.T) {
	// The live ruleset matches the desired state → no drift.
	want := map[string]interface{}{
		"name":          "protect-main",
		"enforcement":   "active",
		"target":        "branch",
		"bypass_actors": []interface{}{},
		"rules":         []interface{}{},
		"conditions":    map[string]interface{}{},
	}
	// Live adds server-assigned fields that normalization strips.
	live := map[string]interface{}{
		"id":                      42,
		"name":                    "protect-main",
		"enforcement":             "active",
		"target":                  "branch",
		"source":                  "owner/repo",
		"source_type":             "Repository",
		"bypass_actors":           []interface{}{},
		"rules":                   []interface{}{},
		"conditions":              map[string]interface{}{},
		"created_at":              "2026-01-01T00:00:00Z",
		"updated_at":              "2026-01-01T00:00:00Z",
		"node_id":                 "abc",
		"_links":                  map[string]interface{}{},
		"current_user_can_bypass": "never",
	}
	listResp, _ := json.Marshal([]map[string]interface{}{{"id": 42, "name": "protect-main"}})
	liveResp, _ := json.Marshal(live)

	script := `args="$*"
case "$args" in
  "api repos/acme/testrepo/rulesets") echo '` + string(listResp) + `' ;;
  "api repos/acme/testrepo/rulesets/42") echo '` + string(liveResp) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	ghBin := fakeGH(t, script)
	src := rulesetSource(t, map[string]interface{}{"protect-main": want})

	var out bytes.Buffer
	drift, err := posture.CheckRulesets(context.Background(), ghBin, "acme/testrepo", src, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drift {
		t.Errorf("expected no drift; output: %s", out.String())
	}
	if !strings.Contains(out.String(), "OK       protect-main") {
		t.Errorf("expected OK line; got: %s", out.String())
	}
}

func TestCheckRulesets_Missing(t *testing.T) {
	// The live repo has no rulesets; the desired file expects one → drift.
	listResp := []byte(`[]`)
	script := `args="$*"
case "$args" in
  "api repos/acme/testrepo/rulesets") echo '` + string(listResp) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	ghBin := fakeGH(t, script)
	want := map[string]interface{}{"name": "protect-main", "enforcement": "active", "target": "branch"}
	src := rulesetSource(t, map[string]interface{}{"protect-main": want})

	var out bytes.Buffer
	drift, err := posture.CheckRulesets(context.Background(), ghBin, "acme/testrepo", src, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drift {
		t.Error("expected drift for missing ruleset")
	}
	if !strings.Contains(out.String(), "MISSING") {
		t.Errorf("expected MISSING in output; got: %s", out.String())
	}
}

func TestCheckRulesets_Drift(t *testing.T) {
	want := map[string]interface{}{
		"name":        "protect-main",
		"enforcement": "active",
		"target":      "branch",
	}
	// Live has enforcement=disabled — drift.
	live := map[string]interface{}{
		"id":          99,
		"name":        "protect-main",
		"enforcement": "disabled",
		"target":      "branch",
	}
	listResp, _ := json.Marshal([]map[string]interface{}{{"id": 99, "name": "protect-main"}})
	liveResp, _ := json.Marshal(live)

	script := `args="$*"
case "$args" in
  "api repos/acme/testrepo/rulesets") echo '` + string(listResp) + `' ;;
  "api repos/acme/testrepo/rulesets/99") echo '` + string(liveResp) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	ghBin := fakeGH(t, script)
	src := rulesetSource(t, map[string]interface{}{"protect-main": want})

	var out bytes.Buffer
	drift, err := posture.CheckRulesets(context.Background(), ghBin, "acme/testrepo", src, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drift {
		t.Error("expected drift")
	}
	if !strings.Contains(out.String(), "DRIFT") {
		t.Errorf("expected DRIFT in output; got: %s", out.String())
	}
}

func TestApplyRulesets_Creates(t *testing.T) {
	// Fake gh: match on full argument string; handles the -X POST create call.
	script := `#!/bin/sh
# args: api [-X METHOD] endpoint [--input FILE]
args="$*"
case "$args" in
  "api repos/acme/testrepo/rulesets")
    echo '[]' ;;
  *"-X POST"*"repos/acme/testrepo/rulesets"*)
    echo '{"id":1,"name":"new-rule"}' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	bin := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_GH_BIN", bin)

	want := map[string]interface{}{"name": "new-rule", "enforcement": "active", "target": "branch"}
	src := rulesetSource(t, map[string]interface{}{"new-rule": want})

	var out bytes.Buffer
	err := posture.ApplyRulesets(context.Background(), bin, "acme/testrepo", src, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "CREATED") {
		t.Errorf("expected CREATED in output; got: %s", out.String())
	}
}

// --- settings tests ---------------------------------------------------------

// settingsSource creates a LocalSource with a .github/repo-settings.json.
func settingsSource(t *testing.T, settings map[string]interface{}) posture.LocalSource {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "repo-settings.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return posture.LocalSource{Root: root}
}

func TestCheckSettings_OK(t *testing.T) {
	desired := map[string]interface{}{
		"repo": map[string]interface{}{
			"allow_merge_commit":          false,
			"allow_squash_merge":          true,
			"allow_rebase_merge":          true,
			"allow_auto_merge":            false,
			"delete_branch_on_merge":      true,
			"allow_update_branch":         true,
			"web_commit_signoff_required": true,
		},
		"vulnerability_alerts": true,
		"actions_workflow": map[string]interface{}{
			"default_workflow_permissions":     "read",
			"can_approve_pull_request_reviews": true,
		},
		"unmanaged": []string{"some manual setting"},
	}

	liveRepo := map[string]interface{}{
		"allow_merge_commit":          false,
		"allow_squash_merge":          true,
		"allow_rebase_merge":          true,
		"allow_auto_merge":            false,
		"delete_branch_on_merge":      true,
		"allow_update_branch":         true,
		"web_commit_signoff_required": true,
		// Extra fields GitHub returns that we don't manage:
		"name":      "myrepo",
		"full_name": "acme/myrepo",
	}
	liveRepoJSON, _ := json.Marshal(liveRepo)

	actionsResp := map[string]interface{}{
		"default_workflow_permissions":     "read",
		"can_approve_pull_request_reviews": true,
	}
	actionsJSON, _ := json.Marshal(actionsResp)

	script := `#!/bin/sh
args="$*"
case "$args" in
  "api repos/acme/r")
    echo '` + string(liveRepoJSON) + `' ;;
  "api repos/acme/r/vulnerability-alerts")
    exit 0 ;;  # 204 = enabled
  "api repos/acme/r/actions/permissions/workflow")
    echo '` + string(actionsJSON) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	bin := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	src := settingsSource(t, desired)
	var out bytes.Buffer
	drift, err := posture.CheckSettings(context.Background(), bin, "acme/r", src, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drift {
		t.Errorf("expected no drift; output:\n%s", out.String())
	}
	// Unmanaged items should be printed as INFO.
	if !strings.Contains(out.String(), "INFO") {
		t.Errorf("expected INFO line for unmanaged; got:\n%s", out.String())
	}
}

func TestCheckSettings_Drift(t *testing.T) {
	desired := map[string]interface{}{
		"repo": map[string]interface{}{
			"allow_merge_commit": false,
			"allow_squash_merge": true,
		},
	}
	// Live has allow_squash_merge=false — drift.
	liveRepo := map[string]interface{}{
		"allow_merge_commit": false,
		"allow_squash_merge": false,
	}
	liveRepoJSON, _ := json.Marshal(liveRepo)

	script := `#!/bin/sh
args="$*"
case "$args" in
  "api repos/acme/r") echo '` + string(liveRepoJSON) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	bin := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	src := settingsSource(t, desired)
	var out bytes.Buffer
	drift, err := posture.CheckSettings(context.Background(), bin, "acme/r", src, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drift {
		t.Error("expected drift")
	}
	if !strings.Contains(out.String(), "DRIFT") {
		t.Errorf("expected DRIFT in output; got:\n%s", out.String())
	}
}

// TestDetectRepo checks that DetectRepo propagates gh failures as errors.
func TestDetectRepo_Failure(t *testing.T) {
	script := `#!/bin/sh
exit 1`
	bin := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := posture.DetectRepo(context.Background(), bin)
	if err == nil {
		t.Error("expected error when gh exits non-zero")
	}
}
