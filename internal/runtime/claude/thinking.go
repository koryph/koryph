// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// thinkingLine is the tolerant subset of a stream-json line the thinking
// extractor needs (koryph-xvk). Shape pinned against a real dispatched-agent
// stream (2026-07, run 20260705-042300): every dispatch runs with
// --include-partial-messages, so extended thinking arrives as
//
//	{"type":"stream_event",
//	 "event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"…"}},
//	 "parent_tool_use_id":null, …}
//
// with parent_tool_use_id non-null when the emitting context is a nested
// subagent (Task tool) rather than the main agent.
type thinkingLine struct {
	Type  string `json:"type"`
	Event *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	} `json:"event"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
}

// ExtractThinking renders the thinking stream from stream-json lines in r:
// thinking_delta text concatenated in stream order, a blank line at each
// content-block boundary (one block ≈ one uninterrupted stretch of
// reasoning), and a labeled divider whenever output moves between the main
// agent and a subagent (parent_tool_use_id transitions) so interleaved
// nested reasoning stays attributable. Only the streamed deltas are used —
// completed assistant messages repeat text the deltas already carried.
// Malformed lines are skipped (the same tolerant scanning every other
// stream-json reducer in this package uses); an input with no thinking
// yields "".
func ExtractThinking(r io.Reader) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var b strings.Builder
	// curParent tracks whose thinking the previous delta belonged to:
	// "" = main agent, otherwise the subagent's parent_tool_use_id.
	// startedInside distinguishes "no thinking yet" from "main agent".
	curParent, started := "", false
	blockBreak := false

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var tl thinkingLine
		if err := json.Unmarshal(line, &tl); err != nil || tl.Type != "stream_event" || tl.Event == nil {
			continue
		}
		switch tl.Event.Type {
		case "content_block_stop":
			blockBreak = true
		case "content_block_delta":
			d := tl.Event.Delta
			if d == nil || d.Type != "thinking_delta" || d.Thinking == "" {
				continue
			}
			parent := ""
			if tl.ParentToolUseID != nil {
				parent = *tl.ParentToolUseID
			}
			if started && parent != curParent {
				b.WriteString("\n\n")
				if parent != "" {
					b.WriteString("── subagent " + shortToolUseID(parent) + " ──\n")
				} else {
					b.WriteString("── main agent ──\n")
				}
				blockBreak = false
			} else if blockBreak && started {
				b.WriteString("\n\n")
				blockBreak = false
			}
			curParent, started = parent, true
			b.WriteString(d.Thinking)
		}
	}
	return b.String()
}

// shortToolUseID trims a tool-use id to a display-friendly suffix — the ids
// are long opaque tokens ("toolu_…") whose tail is the distinguishing part.
func shortToolUseID(id string) string {
	if len(id) > 8 {
		return "…" + id[len(id)-8:]
	}
	return id
}
