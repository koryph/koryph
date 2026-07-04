// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package modelroute

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// PersonaMeta reads the leading YAML frontmatter of
// <repoRoot>/.claude/agents/<persona>.md and returns its "model", "effort",
// and "tier" scalars ("" when absent). A missing file yields ("", "", "",
// nil) so the engine can fall back to its own defaults without treating
// absence as an error.
//
// tier (koryph-v8u.10) is the runtime-agnostic capability class documented in
// agents/README.md's frontmatter contract ("frontier"/"standard"/"light");
// model is the Claude-specific legacy pin. Callers resolve tier through the
// active runtime's model map (see internal/modelroute/route.go's
// effectiveModelMap) before falling back to model.
func PersonaMeta(repoRoot, persona string) (model, effort, tier string, err error) {
	path := filepath.Join(repoRoot, ".claude", "agents", persona+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", "", nil
		}
		return "", "", "", err
	}
	fm := parseFrontmatter(string(data))
	return fm["model"], fm["effort"], fm["tier"], nil
}

// parseFrontmatter extracts top-level scalar key/value pairs from the leading
// YAML frontmatter delimited by "---" lines. It is a deliberately small
// subset: only unindented "key: value" scalars are captured; quotes are
// trimmed, and anything else (blank values with nested lists/maps, indented
// lines, flow collections, comments) is ignored.
func parseFrontmatter(text string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(text, "\n")

	started := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !started {
			if trimmed == "" {
				continue // skip leading blank lines before the opening "---"
			}
			if trimmed == "---" {
				started = true
				continue
			}
			return out // no frontmatter at the top of the file
		}
		// Inside the frontmatter block.
		if trimmed == "---" || trimmed == "..." {
			break // closing delimiter
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Indented lines are nested (list items / block values) — ignore.
		if line != strings.TrimLeft(line, " \t") {
			continue
		}
		key, val, ok := splitKV(trimmed)
		if !ok {
			continue
		}
		// Empty value (a nested block/list follows) or a flow collection is
		// not a scalar — ignore it.
		if val == "" || strings.HasPrefix(val, "[") || strings.HasPrefix(val, "{") {
			continue
		}
		out[key] = unquote(val)
	}
	return out
}

// splitKV splits "key: value" on the first colon.
func splitKV(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	val = strings.TrimSpace(line[i+1:])
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

// unquote trims a single matched pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
