// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package rules installs the koryph enforcement "rules" into a managed
// project: the hook scripts (hooks/*.sh) and their wiring in
// .claude/settings.json (the agent-boundary + worktree guards and the bd-prime
// SessionStart hook, plus the baseline permission allow/deny lists).
//
// Hook scripts install like agents/commands — whole files, hash-idempotent,
// --force overwrites a differing file. settings.json is different: it is MERGED
// additively so a project's own hooks and permissions are never clobbered. Only
// an unparseable file blocks the merge (skip + warn; --force rebuilds it).
package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/hooks"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/scaffold"
)

// settings.json merge outcomes.
const (
	SettingsCreated   = "created"   // no settings.json existed; baseline written
	SettingsMerged    = "merged"    // existing file gained koryph hooks/permissions
	SettingsUnchanged = "unchanged" // koryph wiring already present
	SettingsSkipped   = "skipped"   // unparseable/incompatible; left untouched (see --force)
)

// hookSpec is one koryph hook to ensure present in .claude/settings.json.
type hookSpec struct {
	event   string // "PreToolUse" | "SessionStart"
	matcher string // tool matcher ("" for SessionStart)
	command string // the hook command
	marker  string // substring identifying an equivalent existing hook
}

// Guard hooks are referenced from KORYPH_HOME, NOT ${CLAUDE_PROJECT_DIR}: for a
// dispatched agent CLAUDE_PROJECT_DIR is its own worktree, so a worktree-local
// guard could be overwritten by the very agent it constrains. The scripts live
// centrally (paths.HooksDir), outside any agent's write scope. Claude Code runs
// hook commands via `sh -c`, so ${KORYPH_HOME:-$HOME/.koryph} expands whether or
// not KORYPH_HOME is exported (dispatched agents get it exported; interactive
// sessions fall back to the default home).
// The SessionStart entry wraps `bd prime --hook-json` in koryph-prime.sh
// (koryph-77r.4, design: docs/designs/2026-07-token-economy.md §3 L2)
// instead of invoking it directly: the wrapper measures the injected byte
// size and substitutes a slim profile for secondary-spawn sessions
// (review/stage/epicreview) that never touch bead workflow. The trailing
// shell comment (harmless to `sh -c` — text after an unescaped `#` is
// ignored) preserves "bd prime" as a literal substring of the new command
// so ensureHook's marker match still finds and migrates a project's
// pre-wrapper "bd prime --hook-json" registration in place, exactly like
// the guards' CLAUDE_PROJECT_DIR migration.
var koryphHooks = []hookSpec{
	{event: "PreToolUse", matcher: "Bash", command: `"${KORYPH_HOME:-$HOME/.koryph}/hooks/agent-boundary-guard.sh"`, marker: "agent-boundary-guard.sh"},
	{event: "PreToolUse", matcher: "Bash|Edit|Write", command: `"${KORYPH_HOME:-$HOME/.koryph}/hooks/worktree-guard.sh"`, marker: "worktree-guard.sh"},
	{event: "SessionStart", command: `"${KORYPH_HOME:-$HOME/.koryph}/hooks/koryph-prime.sh"  # replaces: bd prime --hook-json`, marker: "bd prime"},
}

var koryphAllow = []string{"Bash(*)", "Read(**)", "Glob(**)", "Grep(**)", "Edit(**)", "Write(**)"}

var koryphDeny = []string{
	"Bash(git push --force*)", "Bash(git push -f*)", "Bash(git filter-branch*)",
	"Bash(git filter-repo*)", "Bash(gh auth*)", "Bash(gh secret*)", "Bash(sudo*)",
	"Bash(rm -rf /*)", "Bash(rm -rf ~*)", "Read(.env)", "Read(.env.*)",
	"Read(**/*.pem)", "Read(**/*.key)",
}

// Install installs the hook scripts into the central, agent-unwritable
// paths.HooksDir (NOT the project worktree) and merges the settings wiring. It
// returns the per-hook copy results and the settings-merge outcome.
func Install(root string, force bool) ([]scaffold.Result, string, error) {
	hookResults, err := scaffold.CopyEmbed(hooks.FS, paths.HooksDir(), force, 0o755)
	if err != nil {
		return nil, "", err
	}
	settings, err := MergeSettings(root, force)
	return hookResults, settings, err
}

// MergeSettings additively merges the koryph hooks and permission lists into
// <root>/.claude/settings.json, preserving every other key. See the package doc.
func MergeSettings(root string, force bool) (string, error) {
	path := filepath.Join(root, ".claude", "settings.json")
	raw, rerr := os.ReadFile(path)
	existed := rerr == nil

	cur := map[string]any{}
	if existed {
		if json.Unmarshal(raw, &cur) != nil {
			if !force {
				return SettingsSkipped, nil // unparseable — never clobber without --force
			}
			cur = map[string]any{} // --force: rebuild from baseline
		}
	}

	changed, ok := mergeInto(cur)
	if !ok {
		return SettingsSkipped, nil // incompatible shape — leave it, warn upstream
	}
	if !changed && existed {
		return SettingsUnchanged, nil
	}

	out, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := fsx.WriteAtomic(path, append(out, '\n'), 0o644); err != nil {
		return "", err
	}
	if existed {
		return SettingsMerged, nil
	}
	return SettingsCreated, nil
}

// mergeInto adds the koryph hooks and permission entries to cur without
// removing anything. changed reports whether cur was modified; ok is false when
// an existing hooks/permissions subtree has an incompatible (non-object/array)
// shape, in which case cur is left untouched.
func mergeInto(cur map[string]any) (changed, ok bool) {
	hks, ok := getMap(cur, "hooks")
	if !ok {
		return false, false
	}
	for _, h := range koryphHooks {
		arr, ok := getSlice(hks, h.event)
		if !ok {
			return false, false
		}
		newArr, ch := ensureHook(arr, h)
		if ch {
			changed = true
		}
		hks[h.event] = newArr
	}
	cur["hooks"] = hks

	perms, ok := getMap(cur, "permissions")
	if !ok {
		return false, false
	}
	for _, kv := range []struct {
		key  string
		want []string
	}{{"allow", koryphAllow}, {"deny", koryphDeny}} {
		ch, ok := unionInto(perms, kv.key, kv.want)
		if !ok {
			return false, false
		}
		if ch {
			changed = true
		}
	}
	cur["permissions"] = perms

	return changed, true
}

// getMap returns m[key] as a map (a fresh one when absent); ok is false only
// when the key is present with a non-object value.
func getMap(m map[string]any, key string) (map[string]any, bool) {
	v, present := m[key]
	if !present || v == nil {
		return map[string]any{}, true
	}
	mm, ok := v.(map[string]any)
	return mm, ok
}

// getSlice returns m[key] as a slice (a fresh one when absent); ok is false only
// when the key is present with a non-array value.
func getSlice(m map[string]any, key string) ([]any, bool) {
	v, present := m[key]
	if !present || v == nil {
		return []any{}, true
	}
	s, ok := v.([]any)
	return s, ok
}

// ensureHook makes arr carry exactly one up-to-date koryph hook for h. If an
// entry already matches h.marker but its command differs (e.g. a legacy
// ${CLAUDE_PROJECT_DIR}/hooks/... registration), the command is rewritten in
// place — this migrates old worktree-relative guards to the central path. If no
// entry matches the marker, a fresh entry is appended. Non-koryph hooks are
// untouched. changed reports whether arr was modified.
func ensureHook(arr []any, h hookSpec) (out []any, changed bool) {
	for _, e := range arr {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := em["hooks"].([]any)
		for _, hk := range inner {
			hm, ok := hk.(map[string]any)
			if !ok {
				continue
			}
			cmd, ok := hm["command"].(string)
			if !ok || !strings.Contains(cmd, h.marker) {
				continue
			}
			if cmd != h.command {
				hm["command"] = h.command // migrate legacy registration in place
				changed = true
			}
			return arr, changed // marker found → done (updated or already current)
		}
	}
	return append(arr, hookEntry(h)), true // absent → add
}

// hookEntry builds a settings.json hook entry for a koryph hook.
func hookEntry(h hookSpec) map[string]any {
	entry := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": h.command}},
	}
	if h.matcher != "" {
		entry["matcher"] = h.matcher
	}
	return entry
}

// unionInto adds each want string to perms[key] if absent; ok is false when the
// key is present with a non-array value.
func unionInto(perms map[string]any, key string, want []string) (changed, ok bool) {
	var arr []any
	if v, present := perms[key]; present && v != nil {
		a, isArr := v.([]any)
		if !isArr {
			return false, false
		}
		arr = a
	}
	seen := map[string]bool{}
	for _, v := range arr {
		if s, ok := v.(string); ok {
			seen[s] = true
		}
	}
	for _, w := range want {
		if !seen[w] {
			arr = append(arr, w)
			seen[w] = true
			changed = true
		}
	}
	if changed {
		perms[key] = arr
	}
	return changed, true
}
