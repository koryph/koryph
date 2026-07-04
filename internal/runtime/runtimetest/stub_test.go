// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtimetest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
)

// TestStubSatisfiesRuntimeEndToEnd exercises every method of runtime.Runtime
// against Stub, proving the interface is implementable and that a
// capability-gated field is honored end-to-end (koryph-v8u.1 acceptance:
// "interface compiles with a stub adapter and unit tests over the
// registry").
func TestStubSatisfiesRuntimeEndToEnd(t *testing.T) {
	s := Stub{
		StubName:     "stub",
		StubProvider: "stub-provider",
		Present:      true,
		Version:      "1.2.3",
		Caps: runtime.Capabilities{
			JSONStream:  true,
			Resume:      true,
			EffortFlag:  true,
			BudgetFlag:  true,
			ModelSelect: true,
			Personas:    true,
		},
	}
	var rt runtime.Runtime = s // static interface-satisfaction check

	if got := rt.Name(); got != "stub" {
		t.Fatalf("Name() = %q, want stub", got)
	}
	if got := rt.Provider(); got != "stub-provider" {
		t.Fatalf("Provider() = %q, want stub-provider", got)
	}

	present, version := rt.Detect(context.Background())
	if !present || version != "1.2.3" {
		t.Fatalf("Detect() = (%v, %q), want (true, \"1.2.3\")", present, version)
	}

	if err := rt.AuthCheck(context.Background(), runtime.Profile{}); err != nil {
		t.Fatalf("AuthCheck: unexpected error: %v", err)
	}

	if got := rt.Capabilities(); got != s.Caps {
		t.Fatalf("Capabilities() = %+v, want %+v", got, s.Caps)
	}

	if got := rt.InstructionFile(); got != "AGENTS.md" {
		t.Fatalf("InstructionFile() = %q, want AGENTS.md", got)
	}

	env := rt.AccountEnv(runtime.Profile{ConfigDir: "/tmp/cfg"})
	if len(env) != 1 || env[0] != "STUB_CONFIG_DIR=/tmp/cfg" {
		t.Fatalf("AccountEnv() = %v, want [STUB_CONFIG_DIR=/tmp/cfg]", env)
	}

	spec := runtime.DispatchSpec{
		SessionID:       "sess-1",
		Persona:         "implementer",
		Model:           "sonnet",
		Effort:          "high",
		ResumeSessionID: "sess-0",
		Profile:         runtime.Profile{ConfigDir: "/tmp/cfg"},
		Billing:         runtime.BillingAPIKey,
		APIKey:          "sk-test",
		EnvPassthrough:  []string{"FOO=bar"},
	}
	argv, gotEnv, err := rt.Command(spec)
	if err != nil {
		t.Fatalf("Command: unexpected error: %v", err)
	}
	wantArgv := []string{"stub", "run", "--session-id", "sess-1", "--persona", "implementer", "--model", "sonnet", "--effort", "high", "--resume", "sess-0"}
	if !equalSlices(argv, wantArgv) {
		t.Fatalf("Command argv = %v, want %v", argv, wantArgv)
	}
	wantEnv := []string{"FOO=bar", "STUB_CONFIG_DIR=/tmp/cfg", "STUB_API_KEY=sk-test"}
	if !equalSlices(gotEnv, wantEnv) {
		t.Fatalf("Command env = %v, want %v", gotEnv, wantEnv)
	}

	stream, err := rt.ParseEvents(strings.NewReader(sampleTranscript))
	if err != nil {
		t.Fatalf("ParseEvents: unexpected error: %v", err)
	}
	defer stream.Close()

	var events []runtime.Event
	for {
		ev, ok, err := stream.Next()
		if err != nil {
			t.Fatalf("Next: unexpected error: %v", err)
		}
		if !ok {
			break
		}
		events = append(events, ev)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	if events[0].Kind != runtime.EventSession || events[0].SessionID != "sess-1" {
		t.Errorf("event[0] = %+v, want session sess-1", events[0])
	}
	if events[1].Kind != runtime.EventOpaque {
		t.Errorf("event[1] = %+v, want opaque", events[1])
	}
	if events[2].Kind != runtime.EventError || !events[2].RateLimited {
		t.Errorf("event[2] = %+v, want rate-limited error", events[2])
	}
	if events[3].Kind != runtime.EventResult || !events[3].HasCost || events[3].CostUSD != 0.42 {
		t.Errorf("event[3] = %+v, want result cost 0.42", events[3])
	}
}

// TestStubCommandRejectsUngatedCapabilities confirms a spec field mapped to
// a capability the runtime does NOT support is a hard error, matching
// runtime.Runtime.Command's documented contract.
func TestStubCommandRejectsUngatedCapabilities(t *testing.T) {
	s := Stub{} // zero-value Capabilities: nothing supported.
	_, _, err := s.Command(runtime.DispatchSpec{ResumeSessionID: "sess-0"})
	if err == nil {
		t.Fatalf("Command with ResumeSessionID and no Resume capability: expected error")
	}
}

// TestStubParseEventsToleratesMalformedLines confirms a malformed line is
// skipped rather than aborting the whole stream, matching
// dispatch/cli.go's tolerant scanning style.
func TestStubParseEventsToleratesMalformedLines(t *testing.T) {
	s := Stub{}
	stream, err := s.ParseEvents(strings.NewReader("not json\n{\"type\":\"result\",\"total_cost_usd\":1.5}\n"))
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	defer stream.Close()

	ev, ok, err := stream.Next()
	if err != nil || !ok {
		t.Fatalf("Next: (%+v, %v, %v)", ev, ok, err)
	}
	if ev.Kind != runtime.EventResult || ev.CostUSD != 1.5 {
		t.Fatalf("Next = %+v, want result cost 1.5", ev)
	}
	if _, ok, err := stream.Next(); ok || err != nil {
		t.Fatalf("second Next: expected EOF (false, nil), got (%v, %v)", ok, err)
	}
}

// TestStubParseEventsSurfacesReadErrors confirms a reader error propagates
// as a Next error rather than being silently swallowed as EOF.
func TestStubParseEventsSurfacesReadErrors(t *testing.T) {
	s := Stub{}
	boom := errors.New("boom")
	stream, err := s.ParseEvents(&errReader{err: boom})
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	defer stream.Close()

	_, ok, err := stream.Next()
	if ok || err == nil {
		t.Fatalf("Next over a failing reader: got (ok=%v, err=%v), want (false, non-nil)", ok, err)
	}
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = (*errReader)(nil)

const sampleTranscript = `{"type":"session","session_id":"sess-1"}
{"type":"assistant","text":"working on it"}
{"type":"error","error":{"type":"rate_limit_error","message":"429 too many requests"}}
{"type":"result","total_cost_usd":0.42}
`

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
