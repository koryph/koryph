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
func FirstJSONBlock(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
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
				return s[start : i+1]
			}
		}
	}
	return ""
}

// envelope is the outer JSON wrapper emitted by the Claude CLI when invoked
// with --output-format json.
type envelope struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// ParseEnvelope parses the Claude CLI JSON-envelope output and returns the
// first balanced JSON block from the "result" field.
//
// out is the trimmed stdout from a claude -p --output-format json invocation.
// The envelope must be valid JSON at the top level, or contain a balanced
// {...} block that is. Errors are returned with messages that read naturally
// when prefixed with a caller label (e.g. "validator ") in a degraded verdict
// Reason.
func ParseEnvelope(out string) (string, error) {
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
	raw := FirstJSONBlock(envl.Result)
	if raw == "" {
		return "", fmt.Errorf("no JSON verdict in result: %s", strings.TrimSpace(Tail(envl.Result, 300)))
	}
	return raw, nil
}
