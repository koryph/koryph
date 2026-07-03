// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/scaffold"
)

func readSettings(t *testing.T, root string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v\n%s", err, data)
	}
	return m
}

// settingsBlob returns the raw settings.json text for substring assertions.
func settingsBlob(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	return string(data)
}

func TestInstallCreatesHooksAndSettings(t *testing.T) {
	root := t.TempDir()
	hookResults, settings, err := Install(root, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if settings != SettingsCreated {
		t.Errorf("settings action = %q, want created", settings)
	}
	// Hook scripts installed and executable.
	for _, want := range []string{"agent-boundary-guard", "worktree-guard"} {
		found := false
		for _, r := range hookResults {
			if r.Name == want && r.Action == scaffold.ActionInstalled {
				found = true
			}
		}
		if !found {
			t.Errorf("hook %q not installed: %+v", want, hookResults)
		}
		fi, err := os.Stat(filepath.Join(root, "hooks", want+".sh"))
		if err != nil {
			t.Fatalf("hook script %q missing: %v", want, err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("hook %q not executable (mode %v)", want, fi.Mode())
		}
	}
	// Settings carry the koryph wiring.
	blob := settingsBlob(t, root)
	for _, want := range []string{"agent-boundary-guard.sh", "worktree-guard.sh", "bd prime", "Bash(git push --force*)"} {
		if !strings.Contains(blob, want) {
			t.Errorf("settings.json missing %q:\n%s", want, blob)
		}
	}
}

func TestMergePreservesUserSettings(t *testing.T) {
	root := t.TempDir()
	writeJSON(t, root, `{
	  "customKey": "keepme",
	  "hooks": {
	    "PreToolUse": [{"matcher":"Read","hooks":[{"type":"command","command":"my-own-hook.sh"}]}]
	  },
	  "permissions": {"allow": ["Bash(ls)"], "deny": ["Bash(rm -rf /*)"]}
	}`)

	action, err := MergeSettings(root, false)
	if err != nil {
		t.Fatalf("MergeSettings: %v", err)
	}
	if action != SettingsMerged {
		t.Errorf("action = %q, want merged", action)
	}

	m := readSettings(t, root)
	if m["customKey"] != "keepme" {
		t.Errorf("customKey lost: %v", m["customKey"])
	}
	blob := settingsBlob(t, root)
	// User's own hook + all koryph hooks present.
	for _, want := range []string{"my-own-hook.sh", "agent-boundary-guard.sh", "worktree-guard.sh", "bd prime", "Bash(ls)", "Read(**)"} {
		if !strings.Contains(blob, want) {
			t.Errorf("merged settings missing %q:\n%s", want, blob)
		}
	}
	// The already-present deny entry is not duplicated.
	if n := strings.Count(blob, `"Bash(rm -rf /*)"`); n != 1 {
		t.Errorf("deny entry duplicated %d times, want 1", n)
	}
}

func TestMergeIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Install(root, false); err != nil {
		t.Fatal(err)
	}
	action, err := MergeSettings(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != SettingsUnchanged {
		t.Errorf("second merge action = %q, want unchanged", action)
	}
}

func TestMergeSkipsUnparseableWithoutForce(t *testing.T) {
	root := t.TempDir()
	writeJSON(t, root, "this is not json {")
	action, err := MergeSettings(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != SettingsSkipped {
		t.Errorf("action = %q, want skipped (unparseable, no force)", action)
	}
	// --force rebuilds from baseline.
	action, err = MergeSettings(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if action == SettingsSkipped {
		t.Errorf("action = %q, want a rebuild under --force", action)
	}
	if !strings.Contains(settingsBlob(t, root), "agent-boundary-guard.sh") {
		t.Error("forced rebuild missing koryph hooks")
	}
}

func writeJSON(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
