// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ghpkg "github.com/koryph/koryph/internal/forge/github"
	"github.com/koryph/koryph/internal/posture"
)

// --- LocalSource.OrgRulesetsDir tests ------------------------------------------

func TestLocalSource_OrgRulesetsDir_Present(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github", "org-rulesets"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := posture.LocalSource{Root: root}
	dir, err := src.OrgRulesetsDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(dir, ".github/org-rulesets") {
		t.Errorf("unexpected dir %q", dir)
	}
}

func TestLocalSource_OrgRulesetsDir_Missing(t *testing.T) {
	root := t.TempDir()
	src := posture.LocalSource{Root: root}
	if _, err := src.OrgRulesetsDir(); err == nil {
		t.Error("expected error when org-rulesets dir is absent")
	}
}

// --- helpers ----------------------------------------------------------------

// orgRulesetSource creates a temp LocalSource with a .github/org-rulesets/
// directory populated with the given JSON files.
func orgRulesetSource(t *testing.T, files map[string]interface{}) posture.LocalSource {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "org-rulesets")
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

// --- CheckOrgRulesets / ApplyOrgRulesets integration tests ------------------

func TestCheckOrgRulesets_OK(t *testing.T) {
	want := map[string]interface{}{
		"name":          "org-protect-main",
		"enforcement":   "active",
		"target":        "branch",
		"bypass_actors": []interface{}{},
		"rules":         []interface{}{},
		"conditions":    map[string]interface{}{},
	}
	live := map[string]interface{}{
		"id":                      77,
		"name":                    "org-protect-main",
		"enforcement":             "active",
		"target":                  "branch",
		"source":                  "acme-org",
		"source_type":             "Organization",
		"bypass_actors":           []interface{}{},
		"rules":                   []interface{}{},
		"conditions":              map[string]interface{}{},
		"created_at":              "2026-01-01T00:00:00Z",
		"updated_at":              "2026-01-01T00:00:00Z",
		"node_id":                 "xyz",
		"_links":                  map[string]interface{}{},
		"current_user_can_bypass": "never",
	}
	listResp, _ := json.Marshal([]map[string]interface{}{{"id": 77, "name": "org-protect-main"}})
	liveResp, _ := json.Marshal(live)

	script := `args="$*"
case "$args" in
  "api orgs/acme-org/rulesets") echo '` + string(listResp) + `' ;;
  "api orgs/acme-org/rulesets/77") echo '` + string(liveResp) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	fakeGH(t, script)
	src := orgRulesetSource(t, map[string]interface{}{"org-protect-main": want})

	var out bytes.Buffer
	drift, err := posture.CheckOrgRulesets(context.Background(), "acme-org", src, &out, ghpkg.New().Protection())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drift {
		t.Errorf("expected no drift; output: %s", out.String())
	}
	if !strings.Contains(out.String(), "OK       org-protect-main") {
		t.Errorf("expected OK line; got: %s", out.String())
	}
}

func TestCheckOrgRulesets_Missing(t *testing.T) {
	listResp := []byte(`[]`)
	script := `args="$*"
case "$args" in
  "api orgs/acme-org/rulesets") echo '` + string(listResp) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	fakeGH(t, script)
	want := map[string]interface{}{"name": "org-protect-main", "enforcement": "active", "target": "branch"}
	src := orgRulesetSource(t, map[string]interface{}{"org-protect-main": want})

	var out bytes.Buffer
	drift, err := posture.CheckOrgRulesets(context.Background(), "acme-org", src, &out, ghpkg.New().Protection())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drift {
		t.Error("expected drift for missing org ruleset")
	}
	if !strings.Contains(out.String(), "MISSING") {
		t.Errorf("expected MISSING in output; got: %s", out.String())
	}
}

func TestCheckOrgRulesets_Drift(t *testing.T) {
	want := map[string]interface{}{
		"name":        "org-protect-main",
		"enforcement": "active",
		"target":      "branch",
	}
	live := map[string]interface{}{
		"id":          55,
		"name":        "org-protect-main",
		"enforcement": "disabled",
		"target":      "branch",
	}
	listResp, _ := json.Marshal([]map[string]interface{}{{"id": 55, "name": "org-protect-main"}})
	liveResp, _ := json.Marshal(live)

	script := `args="$*"
case "$args" in
  "api orgs/acme-org/rulesets") echo '` + string(listResp) + `' ;;
  "api orgs/acme-org/rulesets/55") echo '` + string(liveResp) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	fakeGH(t, script)
	src := orgRulesetSource(t, map[string]interface{}{"org-protect-main": want})

	var out bytes.Buffer
	drift, err := posture.CheckOrgRulesets(context.Background(), "acme-org", src, &out, ghpkg.New().Protection())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drift {
		t.Error("expected drift; got none")
	}
	if !strings.Contains(out.String(), "DRIFT") {
		t.Errorf("expected DRIFT in output; got: %s", out.String())
	}
}

func TestApplyOrgRulesets_Creates(t *testing.T) {
	// The live org has no rulesets; apply should POST to create.
	listResp := []byte(`[]`)

	var createArg string
	// We can't easily capture what was POSTed in a shell script, so just
	// verify the POST endpoint was called and return a fake created response.
	created := map[string]interface{}{
		"id":   100,
		"name": "new-org-rule",
	}
	createdResp, _ := json.Marshal(created)
	script := `args="$*"
case "$args" in
  "api orgs/acme-org/rulesets") echo '` + string(listResp) + `' ;;
  "api -X POST orgs/acme-org/rulesets"*) echo '` + string(createdResp) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	_ = createArg
	fakeGH(t, script)
	want := map[string]interface{}{"name": "new-org-rule", "enforcement": "active", "target": "branch"}
	src := orgRulesetSource(t, map[string]interface{}{"new-org-rule": want})

	var out bytes.Buffer
	if err := posture.ApplyOrgRulesets(context.Background(), "acme-org", src, &out, ghpkg.New().Protection()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "CREATED") {
		t.Errorf("expected CREATED in output; got: %s", out.String())
	}
}

func TestApplyOrgRulesets_Updates(t *testing.T) {
	// The live org has a ruleset; apply should PUT to update.
	live := map[string]interface{}{
		"id":          88,
		"name":        "update-org-rule",
		"enforcement": "disabled",
		"target":      "branch",
	}
	listResp, _ := json.Marshal([]map[string]interface{}{{"id": 88, "name": "update-org-rule"}})
	liveResp, _ := json.Marshal(live)
	updated := map[string]interface{}{"id": 88, "name": "update-org-rule", "enforcement": "active"}
	updatedResp, _ := json.Marshal(updated)

	script := `args="$*"
case "$args" in
  "api orgs/acme-org/rulesets") echo '` + string(listResp) + `' ;;
  "api orgs/acme-org/rulesets/88") echo '` + string(liveResp) + `' ;;
  "api -X PUT orgs/acme-org/rulesets/88"*) echo '` + string(updatedResp) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	fakeGH(t, script)
	want := map[string]interface{}{
		"name":        "update-org-rule",
		"enforcement": "active",
		"target":      "branch",
	}
	src := orgRulesetSource(t, map[string]interface{}{"update-org-rule": want})

	var out bytes.Buffer
	if err := posture.ApplyOrgRulesets(context.Background(), "acme-org", src, &out, ghpkg.New().Protection()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "UPDATED") {
		t.Errorf("expected UPDATED in output; got: %s", out.String())
	}
}

// --- PermissionError tests --------------------------------------------------

func TestCheckOrgRulesets_PermissionError(t *testing.T) {
	// The fake gh returns a 403-style error body and exits non-zero.
	permBody := `{"message":"Must be an organization owner to access this resource."}`
	script := `args="$*"
case "$args" in
  "api orgs/acme-org/rulesets") echo '` + permBody + `'; exit 1 ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	fakeGH(t, script)
	want := map[string]interface{}{"name": "org-rule", "enforcement": "active"}
	src := orgRulesetSource(t, map[string]interface{}{"org-rule": want})

	var out bytes.Buffer
	_, err := posture.CheckOrgRulesets(context.Background(), "acme-org", src, &out, ghpkg.New().Protection())
	if err == nil {
		t.Fatal("expected an error for 403 permission denied")
	}
	var pe *posture.PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *posture.PermissionError; got: %T: %v", err, err)
	}
	if !strings.Contains(pe.Needed, "org owner") && !strings.Contains(pe.Needed, "admin") {
		t.Errorf("unexpected Needed field: %q", pe.Needed)
	}
	if !strings.Contains(pe.Error(), "org owner") {
		t.Errorf("PermissionError.Error() should mention org owner; got: %q", pe.Error())
	}
}

func TestPermissionError_NonPermissionFailure(t *testing.T) {
	// A non-permission gh failure (network error body) should NOT return a
	// PermissionError — it should return a plain error.
	errBody := `{"message":"Not Found"}`
	script := `args="$*"
case "$args" in
  "api orgs/acme-org/rulesets") echo '` + errBody + `'; exit 1 ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	fakeGH(t, script)
	want := map[string]interface{}{"name": "org-rule", "enforcement": "active"}
	src := orgRulesetSource(t, map[string]interface{}{"org-rule": want})

	var out bytes.Buffer
	_, err := posture.CheckOrgRulesets(context.Background(), "acme-org", src, &out, ghpkg.New().Protection())
	if err == nil {
		t.Fatal("expected an error for non-zero exit")
	}
	var pe *posture.PermissionError
	if errors.As(err, &pe) {
		t.Errorf("expected plain error, not *posture.PermissionError; got: %v", err)
	}
}

func TestCheckOrgRulesets_EmptyDir(t *testing.T) {
	// An org-rulesets dir with no JSON files produces no drift and no error.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github", "org-rulesets"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := posture.LocalSource{Root: root}

	// gh should not be called at all (no files → nothing to compare); pass a
	// gh that fails on any call to detect accidental invocations.
	fakeGH(t, `echo "unexpected gh call: $*" >&2; exit 1`)

	var out bytes.Buffer
	drift, err := posture.CheckOrgRulesets(context.Background(), "acme-org", src, &out, ghpkg.New().Protection())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drift {
		t.Error("expected no drift for empty dir")
	}
}
