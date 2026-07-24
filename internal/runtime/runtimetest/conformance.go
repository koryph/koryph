// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtimetest

import (
	"os"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
)

// ConformanceFixture supplies deterministic adapter inputs to AssertConforms.
// It deliberately has no live-login or network probe: conformance tests prove
// the runtime translation contract, while each adapter's integration test owns
// its provider-specific authenticated smoke test.
type ConformanceFixture struct {
	Dispatch runtime.DispatchSpec
	JSON     runtime.JSONSpec
	Stream   string
}

// AssertConforms exercises the portable runtime contract shared by every
// adapter. New adapters call it from their own package tests, supplying only
// their native JSONL fixture. This catches the easy-to-miss seams that used to
// make secondary spawns Claude-only: full argv/env translation, one-shot JSON
// translation, event-stream consumption, and fail-closed unsupported flags.
func AssertConforms(t testing.TB, rt runtime.Runtime, f ConformanceFixture) {
	t.Helper()
	if strings.TrimSpace(rt.Name()) == "" {
		t.Error("runtime name is empty")
	}
	if strings.TrimSpace(rt.Provider()) == "" {
		t.Error("runtime provider is empty")
	}
	if strings.TrimSpace(rt.InstructionFile()) == "" {
		t.Error("runtime instruction file is empty")
	}

	argv, env, err := rt.Command(f.Dispatch)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		t.Errorf("Command argv = %q, want executable", argv)
	}
	if env == nil {
		t.Error("Command env is nil; adapters must return an explicit child environment")
	}
	assertScopedSigningEnv(t, env, f.Dispatch.SSHAuthSock)

	jsonArgv, jsonEnv, err := rt.CommandJSON(f.JSON)
	if err != nil {
		t.Fatalf("CommandJSON: %v", err)
	}
	if len(jsonArgv) == 0 || strings.TrimSpace(jsonArgv[0]) == "" {
		t.Errorf("CommandJSON argv = %q, want executable", jsonArgv)
	}
	if jsonEnv == nil {
		t.Error("CommandJSON env is nil; adapters must return an explicit child environment")
	}
	assertScopedSigningEnv(t, jsonEnv, f.JSON.SSHAuthSock)

	stream, err := rt.ParseEvents(strings.NewReader(f.Stream))
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	defer stream.Close()
	for {
		_, ok, err := stream.Next()
		if err != nil {
			t.Fatalf("EventStream.Next: %v", err)
		}
		if !ok {
			break
		}
	}

	caps := rt.Capabilities()
	if !caps.Resume {
		bad := f.Dispatch
		bad.ResumeSessionID = "resume"
		if _, _, err := rt.Command(bad); err == nil {
			t.Error("Command accepted ResumeSessionID although Capabilities.Resume is false")
		}
	}
	if !caps.BudgetFlag {
		bad := f.Dispatch
		bad.MaxBudgetUSD = 1
		if _, _, err := rt.Command(bad); err == nil {
			t.Error("Command accepted MaxBudgetUSD although Capabilities.BudgetFlag is false")
		}
		jsonBad := f.JSON
		jsonBad.MaxBudgetUSD = 1
		if _, _, err := rt.CommandJSON(jsonBad); err == nil {
			t.Error("CommandJSON accepted MaxBudgetUSD although Capabilities.BudgetFlag is false")
		}
	}
}

// assertScopedSigningEnv pins the runtime-neutral signing transport contract:
// an adapter must pass exactly the scoped socket requested by koryph, and must
// never leak a different ambient operator socket into the child environment.
func assertScopedSigningEnv(t testing.TB, env []string, want string) {
	t.Helper()
	if want == "" {
		return
	}
	got := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "SSH_AUTH_SOCK=") {
			got = strings.TrimPrefix(kv, "SSH_AUTH_SOCK=")
		}
	}
	if got != want {
		t.Errorf("SSH_AUTH_SOCK = %q, want scoped socket %q (ambient %q)", got, want, os.Getenv("SSH_AUTH_SOCK"))
	}
}
