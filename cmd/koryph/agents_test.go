// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// TestCmdAgentsInstallDefaultIsVerbatimClaude asserts `koryph agents install
// <root>` with no --runtime flag installs byte-identical to the embedded
// source (koryph-v8u.12's hard compatibility requirement for the existing,
// pre-flag call shape).
func TestCmdAgentsInstallDefaultIsVerbatimClaude(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	code, _, errb := runCmd("agents", "install", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (stderr=%s)", code, errb)
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude", "agents", "koryph-implementer.md"))
	if err != nil {
		t.Fatalf("read installed persona: %v", err)
	}
	if !strings.Contains(string(data), "model: sonnet") {
		t.Errorf("expected the untouched claude model pin 'model: sonnet', got:\n%s", data)
	}
}

// TestCmdAgentsInstallRuntimeFlagRendersStubPin asserts --runtime selects a
// registered non-claude runtime and renders its ModelMap into the installed
// persona files.
func TestCmdAgentsInstallRuntimeFlagRendersStubPin(t *testing.T) {
	isolate(t)
	const name = "cmd-test-stub-runtime"
	stub := runtimetest.Stub{StubName: name, Models: runtime.ModelMap{
		runtime.TierStandard: "cmd-stub-standard-model",
	}}
	if err := runtime.Default.Register(stub); err != nil {
		t.Fatalf("Register(%s): %v", name, err)
	}

	root := t.TempDir()
	code, out, errb := runCmd("agents", "install", root, "--runtime", name)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (stdout=%s stderr=%s)", code, out, errb)
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude", "agents", "koryph-implementer.md"))
	if err != nil {
		t.Fatalf("read installed persona: %v", err)
	}
	if !strings.Contains(string(data), "model: cmd-stub-standard-model") {
		t.Errorf("expected the stub-rendered model pin, got:\n%s", data)
	}
	if !strings.Contains(errb, "koryph-architect") {
		t.Errorf("expected a note naming the untiered/unmapped personas, stderr=%s", errb)
	}
}

// TestCmdAgentsInstallRuntimeFlagUnknownRuntimeFailsClosed asserts an
// unregistered --runtime name is a hard error, never a silent claude
// fallback.
func TestCmdAgentsInstallRuntimeFlagUnknownRuntimeFailsClosed(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	code, _, errb := runCmd("agents", "install", root, "--runtime", "totally-unregistered-cmd-runtime")
	if code == 0 {
		t.Fatalf("code = 0, want a failure exit (stderr=%s)", errb)
	}
	if !strings.Contains(errb, "unregistered-cmd-runtime") {
		t.Errorf("stderr missing the unknown runtime name: %s", errb)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "agents")); !os.IsNotExist(err) {
		t.Errorf(".claude/agents was created despite the fail-closed error")
	}
}

// TestCmdAgentsInstallRuntimeAllProjectsConflict rejects combining --runtime
// with --all-projects (rendering is not wired into the bulk-install path).
func TestCmdAgentsInstallRuntimeAllProjectsConflict(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("agents", "install", "--all-projects", "--runtime", "claude")
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want usage exit (stderr=%s)", code, errb)
	}
	if !strings.Contains(errb, "mutually exclusive") {
		t.Errorf("stderr missing conflict hint: %s", errb)
	}
}
