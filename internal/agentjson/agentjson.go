// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package agentjson provides helpers for parsing Claude CLI JSON-envelope output.
//
// The CLI emits an outer {result, is_error} envelope whose "result" field
// contains the model's text, which itself is expected to be strict JSON.
// Both internal/epicreview and internal/review use this pattern; this package
// is the single authoritative implementation so that a fix in one place
// (e.g. an escape-handling or is_error edge case) propagates to every caller.
package agentjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Tail returns the last n bytes of s, for bounding an error/log excerpt.
func Tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// FirstJSONBlock extracts the first balanced {...} block from s, respecting
// JSON string literals and escapes. Returns "" when no balanced block exists.
//
// Note: "first balanced block" is intentionally naive — it will latch onto a
// non-JSON brace token (e.g. a Svelte {@html} or a {glob%-*} the model quoted
// from a diff) if that appears before the real payload. Callers extracting a
// model verdict from prose should use SelectJSON, which skips blocks that are
// not valid JSON and can require a schema key.
func FirstJSONBlock(s string) string {
	blocks := JSONBlocks(s)
	if len(blocks) == 0 {
		return ""
	}
	return blocks[0]
}

// JSONBlocks returns every top-level balanced {...} block in s, in order,
// respecting JSON string literals and escapes. Nested braces are part of their
// enclosing block, not separate entries. Unbalanced trailing braces are
// ignored. Returns nil when s has no balanced block.
func JSONBlocks(s string) []string {
	var blocks []string
	i := 0
	for i < len(s) {
		start := strings.IndexByte(s[i:], '{')
		if start < 0 {
			break
		}
		start += i
		depth := 0
		inStr := false
		esc := false
		end := -1
		for j := start; j < len(s); j++ {
			c := s[j]
			if inStr {
				switch {
				case esc:
					esc = false
				case c == '\\':
					esc = true
				case c == '"':
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = j
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			// Unbalanced from here on; nothing more to extract.
			break
		}
		blocks = append(blocks, s[start:end+1])
		i = end + 1
	}
	return blocks
}

// fenceMarker delimits a Markdown code fence.
const fenceMarker = "```"

// FencedJSONBlocks returns the inner text of each Markdown code fence in s whose
// info string is empty or "json" (case-insensitive), in order. Fences are the
// sentinel the reviewer prompt asks the model to wrap its verdict in, so they
// are the most reliable anchor when the model's prose also contains stray
// braces. The returned text is not guaranteed to be valid JSON — SelectJSON
// validates it.
func FencedJSONBlocks(s string) []string {
	var out []string
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, fenceMarker) {
			continue
		}
		lang := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, fenceMarker)))
		// Consume the whole fenced block (through its closing fence) so a skipped
		// non-JSON fence's closer is never re-read as a new opener.
		var buf []string
		i++
		for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fenceMarker) {
			buf = append(buf, lines[i])
			i++
		}
		if lang == "" || lang == "json" {
			out = append(out, strings.TrimSpace(strings.Join(buf, "\n")))
		}
		// The loop's i++ steps past the closing fence (or off the end).
	}
	return out
}

// SelectJSON returns the JSON object embedded in model text s that best matches
// a verdict, or "" when none qualifies. It is immune to non-JSON brace tokens a
// model may quote from a diff — a Svelte {@html}, a {glob%-*}, a bare {...}
// snippet — because those are never valid JSON and are skipped.
//
// Preference order:
//  1. a fenced ```json block (the sentinel the reviewer prompt requests), then
//  2. a bare balanced {...} block in the surrounding prose;
//
// and within each tier the LAST candidate that is valid JSON and — when
// requiredKeys are given — is a JSON object containing every required key. The
// last-match rule follows the convention that a model states its final answer
// after any quoted context; requiredKeys make the selection schema-aware so a
// valid-but-unrelated JSON object quoted in prose cannot be mistaken for the
// verdict (which would otherwise silently decode to a zero-value verdict).
func SelectJSON(s string, requiredKeys ...string) string {
	var fenced []string
	for _, f := range FencedJSONBlocks(s) {
		fenced = append(fenced, JSONBlocks(f)...)
	}
	if blk := pickBlock(fenced, requiredKeys); blk != "" {
		return blk
	}
	return pickBlock(JSONBlocks(s), requiredKeys)
}

// pickBlock returns the last element of blocks that is valid JSON and, when
// requiredKeys are non-empty, is a JSON object containing every one of them.
func pickBlock(blocks, requiredKeys []string) string {
	for i := len(blocks) - 1; i >= 0; i-- {
		blk := blocks[i]
		if !json.Valid([]byte(blk)) {
			continue
		}
		if !hasKeys(blk, requiredKeys) {
			continue
		}
		return blk
	}
	return ""
}

// hasKeys reports whether the valid-JSON block is an object containing every
// key in keys. An empty keys slice always matches.
func hasKeys(block string, keys []string) bool {
	if len(keys) == 0 {
		return true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(block), &obj) != nil {
		return false // not a JSON object (array/scalar)
	}
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			return false
		}
	}
	return true
}

// envelope is the outer JSON wrapper emitted by the Claude CLI when invoked
// with --output-format json.
type envelope struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// unwrapEnvelope parses the Claude CLI JSON-envelope output and returns the
// model's "result" text (the layer that itself holds the verdict JSON). The
// outer envelope is machine-generated, so latching onto its first balanced
// block as a fallback is safe here; the fragile layer is the model *result*,
// which callers extract with SelectJSON.
func unwrapEnvelope(out string) (string, error) {
	var envl envelope
	if json.Unmarshal([]byte(out), &envl) != nil {
		blk := FirstJSONBlock(out)
		if blk == "" || json.Unmarshal([]byte(blk), &envl) != nil {
			return "", fmt.Errorf("output not JSON: %s", strings.TrimSpace(Tail(out, 300)))
		}
	}
	if envl.IsError {
		return "", fmt.Errorf("reported is_error: %s", strings.TrimSpace(Tail(envl.Result, 300)))
	}
	if envl.Result == "" {
		return "", errors.New("returned empty result")
	}
	return envl.Result, nil
}

// ParseEnvelope is ParseEnvelopeVerdict with no schema constraint: it returns
// the model verdict JSON extracted from the CLI envelope's "result" field.
func ParseEnvelope(out string) (string, error) {
	return ParseEnvelopeVerdict(out)
}

// ParseEnvelopeVerdict parses the Claude CLI JSON-envelope output and returns
// the model's verdict JSON from the "result" field, selected with SelectJSON so
// that non-JSON brace tokens the model may quote from a diff (Svelte {@html}, a
// {glob%-*}, a bare {...} snippet) are never mistaken for the verdict.
//
// out is the trimmed stdout from a claude -p --output-format json invocation.
// When requiredKeys are given, the selected block must be a JSON object
// containing every one of them — a schema anchor that keeps a valid-but-unrelated
// JSON object quoted in prose from being accepted as the verdict. Errors read
// naturally when prefixed with a caller label (e.g. "reviewer ", "validator ")
// in a degraded verdict Reason.
func ParseEnvelopeVerdict(out string, requiredKeys ...string) (string, error) {
	result, err := unwrapEnvelope(out)
	if err != nil {
		return "", err
	}
	raw := SelectJSON(result, requiredKeys...)
	if raw == "" {
		return "", fmt.Errorf("no JSON verdict in result: %s", strings.TrimSpace(Tail(result, 300)))
	}
	return raw, nil
}
