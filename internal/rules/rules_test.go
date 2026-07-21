// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/scaffold"
)

// centralHooks points KORYPH_HOME (and thus paths.HooksDir) at a temp dir so
// hook installation is hermetic — the guard scripts install centrally now, not
// into the project worktree.
func centralHooks(t *testing.T) {
	t.Helper()
	t.Setenv("KORYPH_HOME", t.TempDir())
}

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
	centralHooks(t)
	root := t.TempDir()
	hookResults, settings, err := Install(root, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if settings != SettingsCreated {
		t.Errorf("settings action = %q, want created", settings)
	}
	// Hook scripts installed and executable. koryph-spill.sh (koryph-77r.6)
	// is not wired into settings.json hooks (it's a callable wrapper, not a
	// PreToolUse/SessionStart hook) but rides along for free via
	// scaffold.CopyEmbed's "every embedded file ships" contract — this
	// assertion is the regression guard for that. koryph-prime.sh
	// (koryph-77r.4) IS the SessionStart hook command, and koryph-intent.sh
	// is the UserPromptSubmit intent→beads router.
	for _, want := range []string{"agent-boundary-guard", "worktree-guard", "koryph-spill", "koryph-prime", "koryph-intent"} {
		found := false
		for _, r := range hookResults {
			if r.Name == want && r.Action == scaffold.ActionInstalled {
				found = true
			}
		}
		if !found {
			t.Errorf("hook %q not installed: %+v", want, hookResults)
		}
		fi, err := os.Stat(filepath.Join(paths.HooksDir(), want+".sh"))
		if err != nil {
			t.Fatalf("hook script %q missing from central HooksDir: %v", want, err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("hook %q not executable (mode %v)", want, fi.Mode())
		}
		if _, err := os.Stat(filepath.Join(root, "hooks", want+".sh")); err == nil {
			t.Errorf("hook %q installed into the worktree; must live outside agent write scope", want)
		}
	}
	// Settings carry the koryph wiring, referenced via KORYPH_HOME (not the
	// agent-writable project dir).
	blob := settingsBlob(t, root)
	for _, want := range []string{"agent-boundary-guard.sh", "worktree-guard.sh", "koryph-prime.sh", "koryph-intent.sh", "UserPromptSubmit", "bd prime", "Bash(git push --force*)", "${KORYPH_HOME:-$HOME/.koryph}/hooks/"} {
		if !strings.Contains(blob, want) {
			t.Errorf("settings.json missing %q:\n%s", want, blob)
		}
	}
	if strings.Contains(blob, "CLAUDE_PROJECT_DIR") {
		t.Errorf("settings.json references the agent-writable CLAUDE_PROJECT_DIR:\n%s", blob)
	}
}

// TestMergeMigratesLegacyHookPath proves an old worktree-relative guard
// registration (${CLAUDE_PROJECT_DIR}/hooks/...) is rewritten in place to the
// central KORYPH_HOME path, not left dangling or duplicated.
func TestMergeMigratesLegacyHookPath(t *testing.T) {
	centralHooks(t)
	root := t.TempDir()
	writeJSON(t, root, `{
	  "hooks": {
	    "PreToolUse": [
	      {"matcher":"Bash","hooks":[{"type":"command","command":"\"${CLAUDE_PROJECT_DIR}/hooks/agent-boundary-guard.sh\""}]},
	      {"matcher":"Bash|Edit|Write","hooks":[{"type":"command","command":"\"${CLAUDE_PROJECT_DIR}/hooks/worktree-guard.sh\""}]}
	    ]
	  },
	  "permissions": {"allow": [], "deny": []}
	}`)

	action, err := MergeSettings(root, false)
	if err != nil {
		t.Fatalf("MergeSettings: %v", err)
	}
	if action != SettingsMerged {
		t.Errorf("action = %q, want merged (legacy path rewritten)", action)
	}
	blob := settingsBlob(t, root)
	if strings.Contains(blob, "CLAUDE_PROJECT_DIR") {
		t.Errorf("legacy CLAUDE_PROJECT_DIR path not migrated:\n%s", blob)
	}
	if !strings.Contains(blob, "${KORYPH_HOME:-$HOME/.koryph}/hooks/agent-boundary-guard.sh") {
		t.Errorf("migrated command missing:\n%s", blob)
	}
	// No duplication: exactly one entry per guard.
	if n := strings.Count(blob, "agent-boundary-guard.sh"); n != 1 {
		t.Errorf("agent-boundary-guard.sh appears %d times, want 1 (migrated in place)", n)
	}
	// A second merge is now a no-op.
	if action, _ := MergeSettings(root, false); action != SettingsUnchanged {
		t.Errorf("second merge = %q, want unchanged", action)
	}
}

// TestMergeMigratesBarePrimeToWrapper proves a project's pre-existing bare
// "bd prime --hook-json" SessionStart registration (from before koryph-77r.4)
// is rewritten in place to the koryph-prime.sh wrapper, not duplicated
// alongside it — the shared "bd prime" marker (kept alive in the new
// command via a trailing shell comment) is what makes this migration work,
// the same mechanism the CLAUDE_PROJECT_DIR guard migration above relies on.
func TestMergeMigratesBarePrimeToWrapper(t *testing.T) {
	centralHooks(t)
	root := t.TempDir()
	writeJSON(t, root, `{
	  "hooks": {
	    "SessionStart": [
	      {"hooks":[{"type":"command","command":"bd prime --hook-json"}]}
	    ]
	  },
	  "permissions": {"allow": [], "deny": []}
	}`)

	action, err := MergeSettings(root, false)
	if err != nil {
		t.Fatalf("MergeSettings: %v", err)
	}
	if action != SettingsMerged {
		t.Errorf("action = %q, want merged (bare bd prime rewritten to wrapper)", action)
	}
	blob := settingsBlob(t, root)
	if !strings.Contains(blob, "koryph-prime.sh") {
		t.Errorf("migrated command missing koryph-prime.sh wrapper:\n%s", blob)
	}
	// No duplication: exactly one SessionStart hook entry (migrated in
	// place, not appended alongside the old bare command).
	if n := countSessionStartHooks(t, root); n != 1 {
		t.Errorf("expected exactly one SessionStart hook entry, got %d:\n%s", n, blob)
	}
	// A second merge is now a no-op.
	if action, _ := MergeSettings(root, false); action != SettingsUnchanged {
		t.Errorf("second merge = %q, want unchanged", action)
	}
}

// appendBDPrimeEntry simulates `bd init` running AFTER `koryph project add`:
// bd wires its own bare SessionStart entry independent of koryph's merge,
// appending it alongside whatever is already in settings.json.
func appendBDPrimeEntry(t *testing.T, root string) {
	t.Helper()
	m := readSettings(t, root)
	hks, _ := m["hooks"].(map[string]any)
	if hks == nil {
		hks = map[string]any{}
	}
	arr, _ := hks["SessionStart"].([]any)
	arr = append(arr, map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "bd prime --hook-json"}},
	})
	hks["SessionStart"] = arr
	m["hooks"] = hks
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEnsureHookPrimeDedup is the koryph-14p.2 regression suite: `bd init`,
// run after `koryph project add`, wires its own bare "bd prime --hook-json"
// SessionStart entry — independent of and in addition to koryph's
// koryph-prime.sh wrapper entry — producing double session priming. Every
// case proves the merge leaves exactly the entries it should (no more, no
// fewer) and that a second merge is a pure no-op.
func TestEnsureHookPrimeDedup(t *testing.T) {
	tests := []struct {
		name string
		// build seeds .claude/settings.json for the scenario.
		build func(t *testing.T, root string)
		// wantEntries is the SessionStart hook-entry count after one merge.
		wantEntries int
		// wantCommandSubstr must be present in the merged koryph entry.
		wantCommandSubstr string
		// wantSurviveSubstr, when set, is a hook command that must survive
		// the merge untouched (proves non-prime hooks are never swept up).
		wantSurviveSubstr string
	}{
		{
			// (a) koryph installed first (real Install), then bd appends its
			// own bare duplicate — the exact scenario from the sandbox repro.
			name: "koryph_installed_then_bd_appends_duplicate",
			build: func(t *testing.T, root string) {
				if _, _, err := Install(root, false); err != nil {
					t.Fatal(err)
				}
				appendBDPrimeEntry(t, root)
			},
			wantEntries:       1,
			wantCommandSubstr: "koryph-prime.sh",
		},
		{
			// (b) bd (or a pre-koryph-77r.4 koryph) got there first with the
			// bare command — migrated in place, same as
			// TestMergeMigratesBarePrimeToWrapper, kept here so the dedup
			// table is self-contained.
			name: "bd_first_bare_prime_migrated_in_place",
			build: func(t *testing.T, root string) {
				writeJSON(t, root, `{
				  "hooks": {"SessionStart": [{"hooks":[{"type":"command","command":"bd prime --hook-json"}]}]},
				  "permissions": {"allow": [], "deny": []}
				}`)
			},
			wantEntries:       1,
			wantCommandSubstr: "koryph-prime.sh",
		},
		{
			// (d) a project's own unrelated SessionStart hook sits alongside
			// the bare bd-prime entry — it must survive verbatim; only the
			// all-marker entry is a removable duplicate.
			name: "custom_session_hook_never_removed",
			build: func(t *testing.T, root string) {
				writeJSON(t, root, `{
				  "hooks": {"SessionStart": [
				    {"hooks":[{"type":"command","command":"bd prime --hook-json"}]},
				    {"hooks":[{"type":"command","command":"./my-custom-hook.sh"}]}
				  ]},
				  "permissions": {"allow": [], "deny": []}
				}`)
			},
			wantEntries:       2,
			wantCommandSubstr: "koryph-prime.sh",
			wantSurviveSubstr: "my-custom-hook.sh",
		},
		{
			// (e) a MIXED later entry: a stale bare bd-prime hook sharing an
			// entry with a project's own hook. The stale hook is stripped
			// per-hook (double priming ends), the custom hook survives in its
			// slimmed entry — the review finding on entry-granular dedupe,
			// which left this shape double-priming forever while doctor kept
			// recommending a re-run that never converged.
			name: "mixed_entry_stale_prime_stripped_custom_kept",
			build: func(t *testing.T, root string) {
				writeJSON(t, root, `{
				  "hooks": {"SessionStart": [
				    {"hooks":[{"type":"command","command":"bd prime --hook-json"}]},
				    {"hooks":[
				      {"type":"command","command":"bd prime --hook-json"},
				      {"type":"command","command":"./my-custom-hook.sh"}
				    ]}
				  ]},
				  "permissions": {"allow": [], "deny": []}
				}`)
			},
			wantEntries:       2,
			wantCommandSubstr: "koryph-prime.sh",
			wantSurviveSubstr: "my-custom-hook.sh",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			centralHooks(t)
			root := t.TempDir()
			tt.build(t, root)

			action, err := MergeSettings(root, false)
			if err != nil {
				t.Fatalf("MergeSettings: %v", err)
			}
			if action != SettingsMerged {
				t.Errorf("action = %q, want merged", action)
			}
			blob := settingsBlob(t, root)
			if !strings.Contains(blob, tt.wantCommandSubstr) {
				t.Errorf("missing %q in merged settings:\n%s", tt.wantCommandSubstr, blob)
			}
			if tt.wantSurviveSubstr != "" && !strings.Contains(blob, tt.wantSurviveSubstr) {
				t.Errorf("hook %q removed, want preserved:\n%s", tt.wantSurviveSubstr, blob)
			}
			if n := countSessionStartHooks(t, root); n != tt.wantEntries {
				t.Errorf("SessionStart hook entries = %d, want %d:\n%s", n, tt.wantEntries, blob)
			}

			// (c) idempotence: a second merge on the now-deduped state must
			// be a pure no-op.
			action2, err := MergeSettings(root, false)
			if err != nil {
				t.Fatalf("second MergeSettings: %v", err)
			}
			if action2 != SettingsUnchanged {
				t.Errorf("second merge = %q, want unchanged", action2)
			}
		})
	}
}

// countSessionStartHooks counts the individual hook command entries under
// hooks.SessionStart[*].hooks[*] in root's settings.json.
func countSessionStartHooks(t *testing.T, root string) int {
	t.Helper()
	m := readSettings(t, root)
	hks, _ := m["hooks"].(map[string]any)
	arr, _ := hks["SessionStart"].([]any)
	n := 0
	for _, e := range arr {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := em["hooks"].([]any)
		n += len(inner)
	}
	return n
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
	centralHooks(t)
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
