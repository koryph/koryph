// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import (
	"strings"
	"testing"
)

// realistic stream-json lines, in the content_block_start → deltas →
// content_block_stop shape every dispatched agent emits under
// --include-partial-messages.
func thinkBlock(idx int, parent, text string) string {
	p := "null"
	if parent != "" {
		p = `"` + parent + `"`
	}
	return join(
		start(idx, `{"type":"thinking"}`, parent),
		delta(idx, `{"type":"thinking_delta","thinking":`+q(text)+`}`, p),
		stop(idx, parent),
	)
}

func toolBlock(idx int, parent, name, inputJSON string) string {
	p := "null"
	if parent != "" {
		p = `"` + parent + `"`
	}
	return join(
		start(idx, `{"type":"tool_use","name":`+q(name)+`,"input":{}}`, parent),
		delta(idx, `{"type":"input_json_delta","partial_json":`+q(inputJSON)+`}`, p),
		stop(idx, parent),
	)
}

func textBlock(idx int, parent, text string) string {
	p := "null"
	if parent != "" {
		p = `"` + parent + `"`
	}
	return join(
		start(idx, `{"type":"text"}`, parent),
		delta(idx, `{"type":"text_delta","text":`+q(text)+`}`, p),
		stop(idx, parent),
	)
}

func start(idx int, cb, parent string) string {
	return `{"type":"stream_event","event":{"type":"content_block_start","index":` +
		itoa(idx) + `,"content_block":` + cb + `},"parent_tool_use_id":` + pjson(parent) + `}`
}
func delta(idx int, d, p string) string {
	return `{"type":"stream_event","event":{"type":"content_block_delta","index":` +
		itoa(idx) + `,"delta":` + d + `},"parent_tool_use_id":` + p + `}`
}
func stop(idx int, parent string) string {
	return `{"type":"stream_event","event":{"type":"content_block_stop","index":` +
		itoa(idx) + `},"parent_tool_use_id":` + pjson(parent) + `}`
}
func pjson(s string) string {
	if s == "" {
		return "null"
	}
	return `"` + s + `"`
}
func q(s string) string       { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }
func itoa(i int) string       { return string(rune('0' + i)) }
func join(s ...string) string { return strings.Join(s, "\n") }

func TestExtractActivityClassifies(t *testing.T) {
	body := join(
		`{"type":"system","subtype":"init"}`,
		thinkBlock(0, "", "planning the gateway fix"),
		toolBlock(1, "", "Read", `{"file_path":"/repo/main.go"}`),
		textBlock(1, "", "Here is what I found."),
		thinkBlock(0, "toolu_abcdefgh", "nested probe"),
		`{"type":"result","total_cost_usd":0.1}`,
	) + "\n"

	ents := ExtractActivity(strings.NewReader(body))
	if len(ents) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(ents), ents)
	}
	if ents[0].Kind != ActThinking || ents[0].Text != "planning the gateway fix" || ents[0].Parent != "" {
		t.Errorf("entry 0 = %+v, want main-agent thinking", ents[0])
	}
	if ents[1].Kind != ActToolUse || ents[1].Tool != "Read" || !strings.Contains(ents[1].Text, "main.go") {
		t.Errorf("entry 1 = %+v, want Read tool with input", ents[1])
	}
	if ents[2].Kind != ActMessage || ents[2].Text != "Here is what I found." {
		t.Errorf("entry 2 = %+v, want assistant message", ents[2])
	}
	if ents[3].Kind != ActThinking || ents[3].Parent != "toolu_abcdefgh" {
		t.Errorf("entry 3 = %+v, want subagent thinking", ents[3])
	}
}

// TestExtractActivityWindowEdge proves the lazy-open path: when the tail window
// begins mid-block (the content_block_start scrolled out), the in-flight deltas
// are still surfaced rather than silently dropped.
func TestExtractActivityWindowEdge(t *testing.T) {
	// No content_block_start — the window opened after it. Two deltas of an
	// in-flight thinking block, then the block closes.
	body := join(
		delta(0, `{"type":"thinking_delta","thinking":"still reasoning"}`, "null"),
		delta(0, `{"type":"thinking_delta","thinking":" about the retry path"}`, "null"),
	) + "\n"
	ents := ExtractActivity(strings.NewReader(body))
	if len(ents) != 1 || ents[0].Kind != ActThinking {
		t.Fatalf("got %+v, want one thinking entry", ents)
	}
	if ents[0].Text != "still reasoning about the retry path" {
		t.Errorf("text = %q, want the concatenated in-flight reasoning", ents[0].Text)
	}
}

// TestExtractActivityInFlightBlock proves a block with no content_block_stop yet
// (the current block on a live tail) is still flushed.
func TestExtractActivityInFlightBlock(t *testing.T) {
	body := join(
		start(0, `{"type":"tool_use","name":"Bash","input":{}}`, ""),
		delta(0, `{"type":"input_json_delta","partial_json":"{\"command\": \"go te"}`, "null"),
	) + "\n"
	ents := ExtractActivity(strings.NewReader(body))
	if len(ents) != 1 || ents[0].Kind != ActToolUse || ents[0].Tool != "Bash" {
		t.Fatalf("got %+v, want one in-flight Bash tool entry", ents)
	}
}

func TestExtractActivityEmpty(t *testing.T) {
	if ents := ExtractActivity(strings.NewReader(`{"type":"system"}` + "\n")); len(ents) != 0 {
		t.Errorf("got %+v, want no entries", ents)
	}
}

// TestActivityScannerIncremental proves the scanner accumulates a growing stream
// fed in arbitrary byte chunks (including splits mid-line) without re-parsing,
// yielding the same result as one-shot parsing plus the in-flight block.
func TestActivityScannerIncremental(t *testing.T) {
	full := join(
		thinkBlock(0, "", "first thought"),
		toolBlock(1, "", "Bash", `{"command":"go test"}`),
		textBlock(1, "", "done"),
	) + "\n"

	s := NewActivityScanner()
	// Feed one byte at a time — the worst case for line reassembly.
	for i := 0; i < len(full); i++ {
		s.Write([]byte{full[i]})
	}
	ents := s.Entries()
	if len(ents) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(ents), ents)
	}
	if ents[0].Text != "first thought" || ents[1].Tool != "Bash" || ents[2].Text != "done" {
		t.Errorf("byte-wise feed mismatch: %+v", ents)
	}

	// The scanner accumulates: appending more content extends, not replaces.
	s.Write([]byte(thinkBlock(0, "", "later thought") + "\n"))
	ents = s.Entries()
	if len(ents) != 4 || ents[3].Text != "later thought" {
		t.Errorf("append must extend history: %+v", ents)
	}
}

// TestActivityScannerInFlightThenClose proves an open block surfaces immediately
// (before its content_block_stop arrives) and is not duplicated once it closes.
func TestActivityScannerInFlightThenClose(t *testing.T) {
	s := NewActivityScanner()
	s.Write([]byte(start(0, `{"type":"thinking"}`, "") + "\n"))
	s.Write([]byte(delta(0, `{"type":"thinking_delta","thinking":"partial"}`, "null") + "\n"))
	if ents := s.Entries(); len(ents) != 1 || ents[0].Text != "partial" {
		t.Fatalf("in-flight block must show: %+v", ents)
	}
	s.Write([]byte(delta(0, `{"type":"thinking_delta","thinking":" complete"}`, "null") + "\n"))
	s.Write([]byte(stop(0, "") + "\n"))
	ents := s.Entries()
	if len(ents) != 1 || ents[0].Text != "partial complete" {
		t.Fatalf("closed block must not duplicate: %+v", ents)
	}
}
