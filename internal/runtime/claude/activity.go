// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/runtime"
)

// ActivityKind classifies one entry of an agent's streamed activity so the TUI
// tail can filter thinking, tool calls, and assistant messages independently
// (koryph-xvk follow-up: "tail thinking, tool use, or other messages … filter
// between each one or all").
type ActivityKind = runtime.ActivityKind

const (
	// ActThinking is an extended-thinking block (thinking_delta text).
	ActThinking = runtime.ActThinking
	// ActToolUse is a tool invocation — Tool holds the tool name, Text holds the
	// accumulated input JSON (possibly partial for the in-flight call).
	ActToolUse = runtime.ActToolUse
	// ActMessage is assistant-visible text (text_delta) — the "other messages"
	// the agent emits alongside its reasoning and tool calls.
	ActMessage = runtime.ActMessage
)

// ActivityEntry is one classified block extracted from a slot's stream.jsonl.
type ActivityEntry = runtime.ActivityEntry

// activityLine is the tolerant superset of a stream-json line the activity
// extractor reads. Every dispatch runs with --include-partial-messages, so all
// content arrives as content_block_start/delta/stop stream_events carrying an
// index; parent_tool_use_id is non-null for nested-subagent blocks. Shape
// pinned against real dispatched-agent streams (2026-07).
type activityLine struct {
	Type  string `json:"type"`
	Event *struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Thinking    string `json:"thinking"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	} `json:"event"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
}

// openActivityBlock accumulates the deltas of a content block until its
// content_block_stop (or end-of-stream for the in-flight block).
type openActivityBlock struct {
	kind   ActivityKind
	parent string
	tool   string
	seq    int // stream start order, for flushing still-open blocks in order
	b      strings.Builder
}

// ExtractActivity parses r (stream-json lines) into classified activity entries
// in stream order: each thinking block, tool call, and assistant-text block
// becomes one entry, tagged with its originating agent (Parent). Blocks are
// keyed by their per-message index between content_block_start and
// content_block_stop; a block still open at end-of-stream — the reasoning or
// tool call the agent is emitting *right now* — is flushed too, so a live tail
// shows in-progress activity rather than waiting for the block to close.
// Malformed lines are skipped (the same tolerant scan the other reducers use).
//
// ExtractActivity is a one-shot convenience over ActivityScanner for callers
// that read a bounded window (the TUI's 512 KB tail). To accumulate a whole,
// growing stream without re-parsing from the start on every read, use
// ActivityScanner directly and feed it only the newly-appended bytes.
func ExtractActivity(r io.Reader) []ActivityEntry {
	buf, err := io.ReadAll(r)
	if err != nil && len(buf) == 0 {
		return nil
	}
	s := NewActivityScanner()
	s.Write(buf)
	return s.Entries()
}

func (c Claude) ExtractActivity(r io.Reader) []runtime.ActivityEntry {
	return ExtractActivity(r)
}

func (c Claude) NewActivityScanner() runtime.ActivityScanner {
	return NewActivityScanner()
}

// ActivityScanner incrementally parses stream-json into ActivityEntry values,
// retaining content-block state across Write calls. A caller can feed the file
// in chunks — e.g. only the bytes appended since the last read — and accumulate
// the full history without re-parsing from the start, which is what makes
// "load the entire activity" affordable on a large, still-growing stream.
//
// The zero value is not usable; construct with NewActivityScanner.
type ActivityScanner struct {
	open map[int]*openActivityBlock
	seq  int
	done []ActivityEntry
	buf  []byte // trailing bytes past the last '\n' — an incomplete line, carried to the next Write
}

// NewActivityScanner returns an empty scanner ready to accept Write.
func NewActivityScanner() *ActivityScanner {
	return &ActivityScanner{open: map[int]*openActivityBlock{}}
}

// Write feeds bytes to the scanner. Complete lines (terminated by '\n') are
// parsed immediately; a trailing partial line is buffered and completed by the
// next Write — so callers may split the stream at arbitrary byte boundaries.
func (s *ActivityScanner) Write(p []byte) {
	s.buf = append(s.buf, p...)
	start := 0
	for {
		nl := bytes.IndexByte(s.buf[start:], '\n')
		if nl < 0 {
			break
		}
		s.processLine(s.buf[start : start+nl])
		start += nl + 1
	}
	if start > 0 {
		// Retain only the unterminated remainder so s.buf can't grow without bound.
		s.buf = append(s.buf[:0], s.buf[start:]...)
	}
}

// Entries returns every finalized entry plus any block still open (the in-flight
// block on a live tail), in stream order. Cheap when nothing is open — the
// common case for a completed stream — returning the accumulated slice directly.
func (s *ActivityScanner) Entries() []ActivityEntry {
	if len(s.open) == 0 {
		return s.done
	}
	rem := make([]*openActivityBlock, 0, len(s.open))
	for _, ob := range s.open {
		rem = append(rem, ob)
	}
	sort.Slice(rem, func(i, j int) bool { return rem[i].seq < rem[j].seq })
	out := make([]ActivityEntry, 0, len(s.done)+len(rem))
	out = append(out, s.done...)
	for _, ob := range rem {
		if e, ok := entryFor(ob); ok {
			out = append(out, e)
		}
	}
	return out
}

// processLine parses a single stream-json line into the scanner's block state.
func (s *ActivityScanner) processLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var al activityLine
	if err := json.Unmarshal(line, &al); err != nil || al.Type != "stream_event" || al.Event == nil {
		return
	}
	ev := al.Event
	parent := ""
	if al.ParentToolUseID != nil {
		parent = *al.ParentToolUseID
	}
	switch ev.Type {
	case "content_block_start":
		if ev.ContentBlock == nil {
			return
		}
		var kind ActivityKind
		switch ev.ContentBlock.Type {
		case "thinking":
			kind = ActThinking
		case "text":
			kind = ActMessage
		case "tool_use":
			kind = ActToolUse
		default:
			return // redacted_thinking, images, etc. — not tailed
		}
		s.open[ev.Index] = &openActivityBlock{kind: kind, parent: parent, tool: ev.ContentBlock.Name, seq: s.seq}
		s.seq++
	case "content_block_delta":
		if ev.Delta == nil {
			return
		}
		ob := s.open[ev.Index]
		if ob == nil {
			// No content_block_start seen for this index — the block opened
			// before the reader's window began (a large in-flight tool input, or
			// the reasoning the agent is emitting right now, whose start scrolled
			// out of the 512 KB tail window). Lazily open a block inferred from
			// the delta kind so window-edge activity is never dropped; the tool
			// name lived on the missed start and stays blank. Parent is on the
			// delta line.
			var kind ActivityKind
			switch ev.Delta.Type {
			case "thinking_delta":
				kind = ActThinking
			case "text_delta":
				kind = ActMessage
			case "input_json_delta":
				kind = ActToolUse
			default:
				return // signature_delta and friends carry no shown text
			}
			ob = &openActivityBlock{kind: kind, parent: parent, seq: s.seq}
			s.seq++
			s.open[ev.Index] = ob
		}
		switch ev.Delta.Type {
		case "thinking_delta":
			ob.b.WriteString(ev.Delta.Thinking)
		case "text_delta":
			ob.b.WriteString(ev.Delta.Text)
		case "input_json_delta":
			ob.b.WriteString(ev.Delta.PartialJSON)
		}
	case "content_block_stop":
		ob := s.open[ev.Index]
		delete(s.open, ev.Index)
		if e, ok := entryFor(ob); ok {
			s.done = append(s.done, e)
		}
	}
}

// entryFor renders a completed/open block into an entry, reporting false for
// blocks that should not be shown: an empty thinking/message block (one that
// carried only a signature_delta, or no text). A tool call with no input JSON
// yet is still shown — the operator wants to see the call the instant it starts.
func entryFor(ob *openActivityBlock) (ActivityEntry, bool) {
	if ob == nil {
		return ActivityEntry{}, false
	}
	text := ob.b.String()
	if ob.kind != ActToolUse && strings.TrimSpace(text) == "" {
		return ActivityEntry{}, false
	}
	return ActivityEntry{Kind: ob.kind, Parent: ob.parent, Tool: ob.tool, Text: text}, true
}

var _ runtime.ActivityProjector = Claude{}
