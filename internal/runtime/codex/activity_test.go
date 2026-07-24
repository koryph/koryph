// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
)

func TestCodexActivityProjection(t *testing.T) {
	fixture := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-1"}`,
		`{"type":"item.completed","item":{"type":"reasoning","text":"inspect the scheduler"}}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"go test ./..."}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Implemented the fix."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10}}`,
		`{"type":"error","message":"example failure"}`,
		`not-json`,
		`{"type":"future.event","value":1}`,
	}, "\n") + "\n"

	got := (Codex{}).ExtractActivity(strings.NewReader(fixture))
	wantKinds := []runtime.ActivityKind{
		runtime.ActSession, runtime.ActThinking, runtime.ActToolUse,
		runtime.ActMessage, runtime.ActResult, runtime.ActError,
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("entries = %#v, want %d meaningful records", got, len(wantKinds))
	}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Errorf("entry %d kind = %v, want %v", i, got[i].Kind, want)
		}
	}
	if got[2].Tool != "command" || !strings.Contains(got[2].Text, "go test") {
		t.Errorf("command projection = %#v", got[2])
	}
}

func TestCodexActivityScannerHandlesPartialLines(t *testing.T) {
	s := (Codex{}).NewActivityScanner()
	line := []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}` + "\n")
	s.Write(line[:17])
	if len(s.Entries()) != 0 {
		t.Fatal("partial line emitted early")
	}
	s.Write(line[17:])
	got := s.Entries()
	if len(got) != 1 || got[0].Kind != runtime.ActMessage || got[0].Text != "hello" {
		t.Fatalf("entries = %#v", got)
	}
}
