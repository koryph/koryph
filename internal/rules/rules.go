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

var koryphHooks = []hookSpec{
	{event: "PreToolUse", matcher: "Bash", command: `"${CLAUDE_PROJECT_DIR}/hooks/agent-boundary-guard.sh"`, marker: "agent-boundary-guard.sh"},
	{event: "PreToolUse", matcher: "Bash|Edit|Write", command: `"${CLAUDE_PROJECT_DIR}/hooks/worktree-guard.sh"`, marker: "worktree-guard.sh"},
	{event: "SessionStart", command: "bd prime --hook-json", marker: "bd prime"},
}

var koryphAllow = []string{"Bash(*)", "Read(**)", "Glob(**)", "Grep(**)", "Edit(**)", "Write(**)"}

var koryphDeny = []string{
	"Bash(git push --force*)", "Bash(git push -f*)", "Bash(git filter-branch*)",
	"Bash(git filter-repo*)", "Bash(gh auth*)", "Bash(gh secret*)", "Bash(sudo*)",
	"Bash(rm -rf /*)", "Bash(rm -rf ~*)", "Read(.env)", "Read(.env.*)",
	"Read(**/*.pem)", "Read(**/*.key)",
}

// Install installs the hook scripts and merges the settings wiring. It returns
// the per-hook copy results and the settings-merge outcome.
func Install(root string, force bool) ([]scaffold.Result, string, error) {
	hookResults, err := scaffold.CopyEmbed(hooks.FS, filepath.Join(root, "hooks"), force, 0o755)
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
		if !hookPresent(arr, h.marker) {
			hks[h.event] = append(arr, hookEntry(h))
			changed = true
		}
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

// hookPresent reports whether any entry in arr already carries a command hook
// whose command contains marker.
func hookPresent(arr []any, marker string) bool {
	for _, e := range arr {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := em["hooks"].([]any)
		for _, h := range inner {
			if hm, ok := h.(map[string]any); ok {
				if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, marker) {
					return true
				}
			}
		}
	}
	return false
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
