// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// TestSpawnJSONPropagatesCommandError confirms SpawnJSON never execs anything
// when CommandJSON rejects the spec — the capability-gating error surfaces to
// the caller and no process is launched.
func TestSpawnJSONPropagatesCommandError(t *testing.T) {
	// Stub with an unset EffortFlag capability rejects a spec that sets Effort.
	rt := runtimetest.Stub{StubName: "stub"}
	_, err := runtime.SpawnJSON(context.Background(), rt,
		runtime.JSONSpec{Effort: "high"}, runtime.JSONExec{})
	if err == nil {
		t.Fatal("SpawnJSON: want error from CommandJSON capability gate, got nil")
	}
	if !strings.Contains(err.Error(), "EffortFlag") {
		t.Errorf("SpawnJSON error = %v, want it to mention the failing capability", err)
	}
}

// TestSpawnJSONExecsResolvedArgv confirms SpawnJSON execs the runtime-resolved
// argv[0] with argv[1:] and pipes Stdin through, returning the raw
// execx.Result — the exec-routing contract the review/stage/epicreview sites
// depend on.
func TestSpawnJSONExecsResolvedArgv(t *testing.T) {
	// A tiny script standing in for the resolved runtime binary: it echoes a
	// fixed token so we can prove SpawnJSON actually launched argv[0].
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-runtime")
	script := "#!/bin/sh\nprintf 'SPAWNED\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}

	// StubName becomes argv[0], and its ModelSelect/Personas caps let the spec
	// pass CommandJSON's gate.
	rt := runtimetest.Stub{
		StubName: bin,
		Caps:     runtime.Capabilities{ModelSelect: true, Personas: true},
	}
	res, err := runtime.SpawnJSON(context.Background(), rt,
		runtime.JSONSpec{Persona: "p", Model: "m", PermissionMode: "plan"},
		runtime.JSONExec{Dir: dir, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("SpawnJSON: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "SPAWNED") {
		t.Errorf("stdout = %q, want it to contain SPAWNED (proof argv[0] ran)", res.Stdout)
	}
}
