// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package agentjson_test

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/agentjson"
)

// --- Tail ---

func TestTail(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"empty", "", 10, ""},
		{"shorter than n", "hello", 10, "hello"},
		{"exact n", "hello", 5, "hello"},
		{"longer than n", "hello world", 5, "world"},
		{"n zero", "hello", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentjson.Tail(tc.s, tc.n); got != tc.want {
				t.Errorf("Tail(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
		})
	}
}

// --- FirstJSONBlock ---

func TestFirstJSONBlock(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want string
	}{
		{"empty", "", ""},
		{"no brace", "hello world", ""},
		{"simple object", `{"a":1}`, `{"a":1}`},
		{"nested objects", `{"a":{"b":2}}`, `{"a":{"b":2}}`},
		{"prose before", `some text {"a":1} more text`, `{"a":1}`},
		{"prose after", `{"a":1} trailing`, `{"a":1}`},
		{"prose before and after", `preamble {"x":"y"} postscript`, `{"x":"y"}`},
		{"brace inside string", `{"key":"{not a brace}","v":1}`, `{"key":"{not a brace}","v":1}`},
		{"escaped quote in string", `{"k":"say \"hi\"","v":2}`, `{"k":"say \"hi\"","v":2}`},
		{"escaped backslash", `{"k":"path\\file"}`, `{"k":"path\\file"}`},
		{"unclosed brace", `{"a":1`, ""},
		{"takes first of two blocks", `{"a":1} {"b":2}`, `{"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentjson.FirstJSONBlock(tc.s); got != tc.want {
				t.Errorf("FirstJSONBlock(%q) = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}

// --- ParseEnvelope ---

// envelope wraps a model JSON result in the Claude CLI output envelope.
func envelope(result string) string {
	return `{"type":"result","is_error":false,"result":` + jsonStr(result) + `}`
}

// jsonStr returns result encoded as a JSON string literal.
func jsonStr(s string) string {
	// Quick-and-dirty: encode only the characters JSON requires.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return `"` + s + `"`
}

func TestParseEnvelopeClean(t *testing.T) {
	inner := `{"blocking":false,"findings":[]}`
	out := envelope(inner)
	got, err := agentjson.ParseEnvelope(out)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if got != inner {
		t.Errorf("got %q, want %q", got, inner)
	}
}

func TestParseEnvelopeProseAroundBlock(t *testing.T) {
	// Model text has prose around the JSON block — extraction must tolerate it.
	inner := `{"blocking":false,"findings":[]}`
	result := "Here is my verdict:\n" + inner + "\nDone."
	out := envelope(result)
	got, err := agentjson.ParseEnvelope(out)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if got != inner {
		t.Errorf("got %q, want %q", got, inner)
	}
}

func TestParseEnvelopeEnvelopeFallback(t *testing.T) {
	// Outer stdout has prose around the JSON envelope — extraction must tolerate it.
	inner := `{"met":true}`
	out := "Some preamble " + envelope(inner)
	got, err := agentjson.ParseEnvelope(out)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if got != inner {
		t.Errorf("got %q, want %q", got, inner)
	}
}

func TestParseEnvelopeIsError(t *testing.T) {
	out := `{"is_error":true,"result":"something went wrong"}`
	_, err := agentjson.ParseEnvelope(out)
	if err == nil {
		t.Fatal("expected error for is_error:true, got nil")
	}
	if !strings.Contains(err.Error(), "is_error") {
		t.Errorf("error %q should mention is_error", err.Error())
	}
}

func TestParseEnvelopeEmptyResult(t *testing.T) {
	out := `{"is_error":false,"result":""}`
	_, err := agentjson.ParseEnvelope(out)
	if err == nil {
		t.Fatal("expected error for empty result, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q should mention empty", err.Error())
	}
}

func TestParseEnvelopeNoJSONInResult(t *testing.T) {
	out := envelope("just prose with no JSON block here")
	_, err := agentjson.ParseEnvelope(out)
	if err == nil {
		t.Fatal("expected error for no JSON block in result, got nil")
	}
	if !strings.Contains(err.Error(), "no JSON verdict") {
		t.Errorf("error %q should mention no JSON verdict", err.Error())
	}
}

func TestParseEnvelopeNotJSON(t *testing.T) {
	out := "I am not JSON at all, sorry."
	_, err := agentjson.ParseEnvelope(out)
	if err == nil {
		t.Fatal("expected error for non-JSON output, got nil")
	}
	if !strings.Contains(err.Error(), "output not JSON") {
		t.Errorf("error %q should mention output not JSON", err.Error())
	}
}

func TestParseEnvelopeNestedVerdict(t *testing.T) {
	// Verdict with nested objects in the result field.
	inner := `{"met":true,"summary":"ok","gaps":[],"structural":[{"category":"duplication","title":"dup","why":"x","acceptance":"y","type":"chore","labels":["area:engine"]}]}`
	out := envelope(inner)
	got, err := agentjson.ParseEnvelope(out)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if got != inner {
		t.Errorf("got %q, want %q", got, inner)
	}
}
