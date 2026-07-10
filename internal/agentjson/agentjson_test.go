// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package agentjson_test

import (
	"encoding/json"
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

// --- JSONBlocks ---

func TestJSONBlocks(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want []string
	}{
		{"empty", "", nil},
		{"no brace", "hello", nil},
		{"one block", `x {"a":1} y`, []string{`{"a":1}`}},
		{"two blocks", `{"a":1} mid {"b":2}`, []string{`{"a":1}`, `{"b":2}`}},
		{"nested is one block", `{"a":{"b":2}}`, []string{`{"a":{"b":2}}`}},
		{"brace token then verdict", `use {@html} here {"blocking":true}`, []string{`{@html}`, `{"blocking":true}`}},
		{"brace in string not split", `{"k":"{not real}"}`, []string{`{"k":"{not real}"}`}},
		{"unclosed tail ignored", `{"a":1} and {oops`, []string{`{"a":1}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentjson.JSONBlocks(tc.s)
			if len(got) != len(tc.want) {
				t.Fatalf("JSONBlocks(%q) = %v, want %v", tc.s, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("block %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// --- FencedJSONBlocks ---

func TestFencedJSONBlocks(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want []string
	}{
		{"no fence", "just prose", nil},
		{"json fence", "pre\n```json\n{\"blocking\":false}\n```\npost", []string{`{"blocking":false}`}},
		{"bare fence", "```\n{\"a\":1}\n```", []string{`{"a":1}`}},
		{"non-json lang ignored", "```svelte\n{@html x}\n```", nil},
		{"two fences", "```json\n{\"a\":1}\n```\nmid\n```json\n{\"b\":2}\n```", []string{`{"a":1}`, `{"b":2}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentjson.FencedJSONBlocks(tc.s)
			if len(got) != len(tc.want) {
				t.Fatalf("FencedJSONBlocks(%q) = %v, want %v", tc.s, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("fence %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// --- SelectJSON: immune to diff-content brace tokens ---

func TestSelectJSON(t *testing.T) {
	cases := []struct {
		name         string
		s            string
		requiredKeys []string
		want         string
	}{
		{
			// The exact live-bug shape: a Svelte {@html} quoted before the verdict.
			// FirstJSONBlock would return "{@html}"; SelectJSON must skip it.
			name:         "html token before verdict",
			s:            "The template uses {@html} which is risky.\n{\"blocking\":true,\"findings\":[]}",
			requiredKeys: []string{"blocking"},
			want:         `{"blocking":true,"findings":[]}`,
		},
		{
			// A glob/template token that is a balanced but invalid-JSON brace block.
			name:         "glob token before verdict",
			s:            "matches {other_namespace%-*} in the diff\n{\"blocking\":false,\"findings\":[]}",
			requiredKeys: []string{"blocking"},
			want:         `{"blocking":false,"findings":[]}`,
		},
		{
			// A valid-but-unrelated JSON object quoted in prose must NOT be taken
			// as the verdict — the required key disambiguates.
			name:         "unrelated valid json before verdict",
			s:            "config was {\"a\":1} before.\n{\"blocking\":true,\"findings\":[]}",
			requiredKeys: []string{"blocking"},
			want:         `{"blocking":true,"findings":[]}`,
		},
		{
			// Fenced verdict is preferred even with a stray brace token outside.
			name:         "fenced verdict wins over stray token",
			s:            "note {@html}\n```json\n{\"blocking\":false,\"findings\":[]}\n```",
			requiredKeys: []string{"blocking"},
			want:         `{"blocking":false,"findings":[]}`,
		},
		{
			// No required key: still skips the non-JSON token, returns the object.
			name: "no required key skips non-json token",
			s:    "{@html}\n{\"met\":true}",
			want: `{"met":true}`,
		},
		{
			// Only a non-JSON brace token: nothing qualifies.
			name:         "only garbage token",
			s:            "the diff has {@html} and nothing else",
			requiredKeys: []string{"blocking"},
			want:         "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentjson.SelectJSON(tc.s, tc.requiredKeys...); got != tc.want {
				t.Errorf("SelectJSON(%q, %v) = %q, want %q", tc.s, tc.requiredKeys, got, tc.want)
			}
		})
	}
}

// TestParseEnvelopeVerdictImmuneToDiffTokens is the regression for the live bug:
// a reviewer result whose prose quotes {@html} / {other_namespace%-*} before the
// real verdict must still yield the verdict, not "verdict JSON invalid: {@html}".
func TestParseEnvelopeVerdictImmuneToDiffTokens(t *testing.T) {
	result := "The Svelte component uses {@html} and a {other_namespace%-*} glob; " +
		"here is a raw {looks:like,json} snippet too.\n" +
		"```json\n{\"blocking\": true, \"findings\": [{\"severity\":\"major\",\"summary\":\"unescaped {@html}\"}]}\n```"
	out := envelope(result)
	raw, err := agentjson.ParseEnvelopeVerdict(out, "blocking")
	if err != nil {
		t.Fatalf("ParseEnvelopeVerdict: %v", err)
	}
	if !strings.Contains(raw, `"blocking"`) || strings.HasPrefix(raw, "{@html") {
		t.Fatalf("extracted the wrong block: %q", raw)
	}
	if !json.Valid([]byte(raw)) {
		t.Fatalf("extracted block is not valid JSON: %q", raw)
	}
}

// TestParseEnvelopeVerdictUnparseableFailsClosed verifies a genuinely
// unparseable result still surfaces an error (fail-closed), not a phantom pass.
func TestParseEnvelopeVerdictUnparseableFailsClosed(t *testing.T) {
	// Only brace tokens, no real verdict object.
	out := envelope("I could not review: {@html} {other_namespace%-*} — sorry.")
	if _, err := agentjson.ParseEnvelopeVerdict(out, "blocking"); err == nil {
		t.Fatal("expected an error for an unparseable verdict, got nil")
	}
}
