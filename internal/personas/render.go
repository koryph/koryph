// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package personas

import (
	"io/fs"
	"strings"
	"testing/fstest"

	"github.com/koryph/koryph/internal/runtime"
)

// renderPersonasFS builds an in-memory fs.FS mirroring source, with each
// persona's frontmatter `model:` value rewritten through modelMap (keyed by
// that SAME persona's `tier:` scalar) via rewriteModelPin (koryph-v8u.12).
// It is returned as an fs.FS (fstest.MapFS, rather than a plain
// map[string][]byte) so the caller can hand it straight to
// scaffold.CopyEmbed and reuse its existing hash-compare/force/skip
// overwrite policy verbatim — rendering never touches that policy, only the
// bytes fed into it. untiered names every persona (without ".md") that was
// copied unchanged; see rewriteModelPin for why.
func renderPersonasFS(source fs.FS, modelMap runtime.ModelMap) (fstest.MapFS, []string, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, nil, err
	}
	out := fstest.MapFS{}
	var untiered []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, rerr := fs.ReadFile(source, e.Name())
		if rerr != nil {
			return nil, nil, rerr
		}
		rendered, isUntiered := rewriteModelPin(data, modelMap)
		if isUntiered {
			untiered = append(untiered, strings.TrimSuffix(e.Name(), ".md"))
		}
		out[e.Name()] = &fstest.MapFile{Data: rendered, Mode: 0o644}
	}
	return out, untiered, nil
}

// rewriteModelPin rewrites the frontmatter `model:` scalar in data through
// modelMap, keyed by the SAME frontmatter's `tier:` scalar (koryph-v8u.12).
// Only the exact `model:` line's VALUE is replaced — every other byte
// (including the model: key's own original spacing) is left untouched; this
// is a deliberately minimal string substitution, never a YAML round-trip, so
// an installed persona file never gets reformatted beyond that one value
// (per this bead's "keep rendering minimal" requirement).
//
// untiered is true, and data is returned UNCHANGED, whenever: the file has
// no frontmatter at all (e.g. agents/README.md, which this same embedded FS
// also ships); the frontmatter has no `tier:` scalar; or modelMap has no
// entry (or an empty entry) for that tier — a runtime's ModelMap is
// permitted to be sparse (see runtime.ModelMap's doc), and an installer must
// never fabricate a model pin the target runtime never declared.
func rewriteModelPin(data []byte, modelMap runtime.ModelMap) (out []byte, untiered bool) {
	lines := strings.Split(string(data), "\n")
	start, end, ok := frontmatterBounds(lines)
	if !ok {
		return data, true
	}
	tier := frontmatterValue(lines, start, end, "tier")
	if tier == "" {
		return data, true
	}
	mapped, ok := modelMap[tier]
	if !ok || mapped == "" {
		return data, true
	}
	for i := start; i < end; i++ {
		line := lines[i]
		if line != strings.TrimLeft(line, " \t") {
			continue // indented -> nested list/map value, not a top-level scalar
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "model:") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		lines[i] = line[:colon+1] + " " + mapped
		break // `model:` is a single top-level scalar key; at most one line matches
	}
	return []byte(strings.Join(lines, "\n")), false
}

// frontmatterBounds returns the [start,end) line indices strictly BETWEEN
// the two "---" fences delimiting YAML frontmatter, tolerating leading
// blank lines before the opening fence. This mirrors
// modelroute.PersonaMeta/parseFrontmatter's own leading-blank-line
// tolerance, reimplemented locally (rather than exported from
// internal/modelroute) to avoid a new inter-package coupling for what is a
// dozen lines of parsing. ok is false when the file does not open with a
// "---" fence at all, or the fence is never closed.
func frontmatterBounds(lines []string) (start, end int, ok bool) {
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "---" {
		return 0, 0, false
	}
	start = i + 1
	for j := start; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if t == "---" || t == "..." {
			return start, j, true
		}
	}
	return 0, 0, false
}

// frontmatterValue returns the unquoted scalar value of a top-level "key:
// value" line within lines[start:end] ("" if key is absent, indented
// [nested], or not a plain scalar) — the same minimal subset
// modelroute.parseFrontmatter recognizes.
func frontmatterValue(lines []string, start, end int, key string) string {
	prefix := key + ":"
	for i := start; i < end; i++ {
		line := lines[i]
		if line != strings.TrimLeft(line, " \t") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		val := strings.TrimSpace(trimmed[len(prefix):])
		if val == "" || strings.HasPrefix(val, "[") || strings.HasPrefix(val, "{") {
			return ""
		}
		return unquoteScalar(val)
	}
	return ""
}

// unquoteScalar trims a single matched pair of surrounding quotes.
func unquoteScalar(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
