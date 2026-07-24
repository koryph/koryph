// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/koryph/koryph/internal/runtime"
)

type activityRecord struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id,omitempty"`
	Message  string `json:"message,omitempty"`
	Error    *struct {
		Message string `json:"message,omitempty"`
		Code    string `json:"code,omitempty"`
	} `json:"error,omitempty"`
	Item *struct {
		Type             string `json:"type,omitempty"`
		Text             string `json:"text,omitempty"`
		Command          string `json:"command,omitempty"`
		AggregatedOutput string `json:"aggregated_output,omitempty"`
		Name             string `json:"name,omitempty"`
		Query            string `json:"query,omitempty"`
		Status           string `json:"status,omitempty"`
	} `json:"item,omitempty"`
}

type activityScanner struct {
	buf  []byte
	done []runtime.ActivityEntry
}

func (c Codex) ExtractActivity(r io.Reader) []runtime.ActivityEntry {
	buf, err := io.ReadAll(r)
	if err != nil && len(buf) == 0 {
		return nil
	}
	s := &activityScanner{}
	s.Write(buf)
	return s.Entries()
}

func (c Codex) NewActivityScanner() runtime.ActivityScanner {
	return &activityScanner{}
}

func (s *activityScanner) Write(p []byte) {
	s.buf = append(s.buf, p...)
	start := 0
	for {
		nl := bytes.IndexByte(s.buf[start:], '\n')
		if nl < 0 {
			break
		}
		if entry, ok := codexActivityEntry(s.buf[start : start+nl]); ok {
			s.done = append(s.done, entry)
		}
		start += nl + 1
	}
	if start > 0 {
		s.buf = append(s.buf[:0], s.buf[start:]...)
	}
}

func (s *activityScanner) Entries() []runtime.ActivityEntry { return s.done }

func codexActivityEntry(line []byte) (runtime.ActivityEntry, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return runtime.ActivityEntry{}, false
	}
	var rec activityRecord
	if json.Unmarshal(line, &rec) != nil {
		return runtime.ActivityEntry{}, false
	}
	switch rec.Type {
	case "thread.started", "thread.resumed":
		return runtime.ActivityEntry{Kind: runtime.ActSession, Text: strings.TrimSpace(rec.Type + " " + rec.ThreadID)}, true
	case "turn.completed":
		return runtime.ActivityEntry{Kind: runtime.ActResult, Text: "turn completed"}, true
	case "turn.failed", "error":
		text := rec.Message
		if rec.Error != nil {
			text = strings.TrimSpace(strings.Join([]string{rec.Error.Code, rec.Error.Message, text}, " "))
		}
		if text == "" {
			text = rec.Type
		}
		return runtime.ActivityEntry{Kind: runtime.ActError, Text: text}, true
	case "item.started", "item.completed", "item.updated":
		return codexItemActivity(rec.Item)
	default:
		return runtime.ActivityEntry{}, false
	}
}

func codexItemActivity(item *struct {
	Type             string `json:"type,omitempty"`
	Text             string `json:"text,omitempty"`
	Command          string `json:"command,omitempty"`
	AggregatedOutput string `json:"aggregated_output,omitempty"`
	Name             string `json:"name,omitempty"`
	Query            string `json:"query,omitempty"`
	Status           string `json:"status,omitempty"`
}) (runtime.ActivityEntry, bool) {
	if item == nil {
		return runtime.ActivityEntry{}, false
	}
	switch item.Type {
	case "reasoning":
		return nonEmptyActivity(runtime.ActThinking, "", item.Text)
	case "agent_message":
		return nonEmptyActivity(runtime.ActMessage, "", item.Text)
	case "command_execution":
		return runtime.ActivityEntry{Kind: runtime.ActToolUse, Tool: "command", Text: jsonArg("command", item.Command)}, true
	case "file_change", "mcp_tool_call", "web_search", "todo_list":
		tool := item.Type
		if item.Name != "" {
			tool = item.Name
		}
		text := item.Text
		if item.Query != "" {
			text = jsonArg("query", item.Query)
		}
		return runtime.ActivityEntry{Kind: runtime.ActToolUse, Tool: tool, Text: text}, true
	default:
		return runtime.ActivityEntry{}, false
	}
}

func nonEmptyActivity(kind runtime.ActivityKind, tool, text string) (runtime.ActivityEntry, bool) {
	if strings.TrimSpace(text) == "" {
		return runtime.ActivityEntry{}, false
	}
	return runtime.ActivityEntry{Kind: kind, Tool: tool, Text: text}, true
}

func jsonArg(key, value string) string {
	raw, _ := json.Marshal(map[string]string{key: value})
	return string(raw)
}

var _ runtime.ActivityProjector = Codex{}
