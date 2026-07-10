// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import (
	"strings"
	"testing"
)

// delta builds one thinking_delta stream line; parent "" means main agent.
func delta(text, parent string) string {
	p := "null"
	if parent != "" {
		p = `"` + parent + `"`
	}
	return `{"type":"stream_event","event":{"type":"content_block_delta","index":0,` +
		`"delta":{"type":"thinking_delta","thinking":"` + text + `","estimated_tokens":null}},` +
		`"session_id":"s","parent_tool_use_id":` + p + `,"uuid":"u"}`
}

func TestExtractThinking(t *testing.T) {
	t.Run("concatenates deltas in order", func(t *testing.T) {
		body := strings.Join([]string{
			`{"type":"system","subtype":"init"}`,
			delta("Let me read ", ""),
			delta("the code.", ""),
			`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"Let me read the code."}]}}`,
		}, "\n")
		got := ExtractThinking(strings.NewReader(body))
		if got != "Let me read the code." {
			t.Errorf("ExtractThinking = %q, want the streamed deltas only (assistant repeat excluded)", got)
		}
	})

	t.Run("block boundary becomes a paragraph break", func(t *testing.T) {
		body := strings.Join([]string{
			delta("First block.", ""),
			`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"parent_tool_use_id":null}`,
			delta("Second block.", ""),
		}, "\n")
		got := ExtractThinking(strings.NewReader(body))
		if got != "First block.\n\nSecond block." {
			t.Errorf("ExtractThinking = %q, want a blank line between blocks", got)
		}
	})

	t.Run("subagent transitions get labeled dividers", func(t *testing.T) {
		body := strings.Join([]string{
			delta("Main reasoning.", ""),
			delta("Nested reasoning.", "toolu_0123456789abcdef"),
			delta("Back to main.", ""),
		}, "\n")
		got := ExtractThinking(strings.NewReader(body))
		if !strings.Contains(got, "── subagent …89abcdef ──\nNested reasoning.") {
			t.Errorf("ExtractThinking = %q, want a subagent divider before nested text", got)
		}
		if !strings.Contains(got, "── main agent ──\nBack to main.") {
			t.Errorf("ExtractThinking = %q, want a main-agent divider on return", got)
		}
	})

	t.Run("tolerates malformed lines and non-thinking deltas", func(t *testing.T) {
		body := strings.Join([]string{
			"not json",
			`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"visible output"}}}`,
			`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}}`,
			delta("only this.", ""),
		}, "\n")
		if got := ExtractThinking(strings.NewReader(body)); got != "only this." {
			t.Errorf("ExtractThinking = %q, want thinking deltas only", got)
		}
	})

	t.Run("no thinking yields empty", func(t *testing.T) {
		if got := ExtractThinking(strings.NewReader(`{"type":"system"}` + "\n")); got != "" {
			t.Errorf("ExtractThinking = %q, want empty", got)
		}
	})
}
