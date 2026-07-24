// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/koryph/koryph/internal/runtime"
)

// codexEvent is the stable subset of `codex exec --json` JSONL records used
// by koryph. Unknown event kinds remain opaque so CLI additions never break a
// running wave.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id,omitempty"`
	Message  string `json:"message,omitempty"`
	Error    *struct {
		Message string `json:"message,omitempty"`
		Code    string `json:"code,omitempty"`
	} `json:"error,omitempty"`
	Usage *struct {
		InputTokens       int64 `json:"input_tokens,omitempty"`
		CachedInputTokens int64 `json:"cached_input_tokens,omitempty"`
		OutputTokens      int64 `json:"output_tokens,omitempty"`
	} `json:"usage,omitempty"`
}

func (c Codex) ParseEvents(r io.Reader) (runtime.EventStream, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &eventStream{sc: sc}, nil
}

type eventStream struct{ sc *bufio.Scanner }

func (s *eventStream) Next() (runtime.Event, bool, error) {
	for s.sc.Scan() {
		line := bytes.TrimSpace(s.sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var in codexEvent
		if json.Unmarshal(line, &in) != nil {
			continue
		}
		raw := json.RawMessage(append([]byte(nil), line...))
		switch in.Type {
		case "thread.started", "thread.resumed":
			return runtime.Event{Kind: runtime.EventSession, SessionID: in.ThreadID, Raw: raw}, true, nil
		case "turn.completed":
			ev := runtime.Event{Kind: runtime.EventResult, Raw: raw}
			if in.Usage != nil {
				ev.InputTokens, ev.CacheReadTokens, ev.OutputTokens = in.Usage.InputTokens, in.Usage.CachedInputTokens, in.Usage.OutputTokens
				ev.HasUsage = true
			}
			return ev, true, nil
		case "error", "turn.failed":
			haystack := strings.ToLower(in.Message)
			if in.Error != nil {
				haystack += " " + strings.ToLower(in.Error.Code+" "+in.Error.Message)
			}
			rateLimited := strings.Contains(haystack, "429") || strings.Contains(haystack, "rate limit") || strings.Contains(haystack, "overloaded")
			return runtime.Event{Kind: runtime.EventError, RateLimited: rateLimited, Raw: raw}, true, nil
		default:
			return runtime.Event{Kind: runtime.EventOpaque, Raw: raw}, true, nil
		}
	}
	if err := s.sc.Err(); err != nil {
		return runtime.Event{}, false, err
	}
	return runtime.Event{}, false, nil
}

func (s *eventStream) Close() error { return nil }
