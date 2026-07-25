// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package resmon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestClassifyCommand(t *testing.T) {
	tests := []struct {
		name  string
		argv  []string
		scope CommandScope
	}{
		{name: "gate", argv: []string{"make", "gate"}, scope: CommandGate},
		{name: "quiet gate", argv: []string{"make", "gate-agent"}, scope: CommandGate},
		{name: "make build", argv: []string{"make", "build"}, scope: CommandBroad},
		{name: "full test", argv: []string{"go", "test", "./..."}, scope: CommandBroad},
		{name: "full vet with flags", argv: []string{"go", "vet", "-mod=readonly", "./..."}, scope: CommandBroad},
		{name: "lint", argv: []string{"golangci-lint", "run", "./..."}, scope: CommandBroad},
		{name: "focused package", argv: []string{"go", "test", "./internal/resmon"}, scope: CommandFocused},
		{name: "focused subtree", argv: []string{"go", "test", "./internal/resmon/..."}, scope: CommandFocused},
		{name: "current package", argv: []string{"go", "test"}, scope: CommandFocused},
		{name: "env prefix", argv: []string{"GOFLAGS=-count=1", "go", "test", "./..."}, scope: CommandBroad},
		{name: "env command prefix", argv: []string{"/usr/bin/env", "-u", "GOFLAGS", "GOFLAGS=-count=1", "/usr/bin/go", "test", "./..."}, scope: CommandBroad},
		{name: "shell wrapper", argv: []string{"/bin/sh", "-c", "make gate-agent"}, scope: CommandGate},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyCommand(tc.argv)
			if got.Scope != tc.scope {
				t.Fatalf("scope = %q, want %q", got.Scope, tc.scope)
			}
			if (tc.scope == CommandFocused) != (got.Signature == "") {
				t.Fatalf("signature = %q for scope %q", got.Signature, got.Scope)
			}
		})
	}
}

func TestClassifyCommandShellSignatureCoversFullEffectiveArgv(t *testing.T) {
	first := ClassifyCommand([]string{"sh", "-c", "go test ./...; echo first"})
	second := ClassifyCommand([]string{"sh", "-c", "go test ./...; echo second"})
	if first.Scope != CommandBroad || second.Scope != CommandBroad {
		t.Fatalf("shell scopes = %q/%q, want broad", first.Scope, second.Scope)
	}
	if first.Signature == "" || second.Signature == "" || first.Signature == second.Signature {
		t.Fatalf("shell signatures do not cover effective argv: %q/%q", first.Signature, second.Signature)
	}
}

func TestClassifyCommandFullGateSignatureIsWrapperAgnostic(t *testing.T) {
	direct := ClassifyCommand([]string{"make", "gate-agent"})
	shell := ClassifyCommand([]string{"sh", "-c", "make gate-agent"})
	alternate := ClassifyCommand([]string{"make", "gate"})
	if direct.Scope != CommandGate || shell.Scope != CommandGate || alternate.Scope != CommandGate {
		t.Fatalf("gate scopes = %q/%q/%q", direct.Scope, shell.Scope, alternate.Scope)
	}
	if direct.Signature == "" || direct.Signature != shell.Signature || direct.Signature != alternate.Signature {
		t.Fatalf("equivalent full gates did not share a lane: %q/%q/%q",
			direct.Signature, shell.Signature, alternate.Signature)
	}
}

func TestClassifyCommandSimpleBroadSignatureIsWrapperAgnostic(t *testing.T) {
	direct := ClassifyCommand([]string{"go", "test", "./..."})
	shell := ClassifyCommand([]string{"sh", "-c", "go test ./..."})
	if direct.Scope != CommandBroad || shell.Scope != CommandBroad ||
		direct.Signature == "" || direct.Signature != shell.Signature {
		t.Fatalf("simple broad wrapper changed lane: direct=%+v shell=%+v", direct, shell)
	}
}

func TestClassifyCommandSignatureIsStableAndSpecific(t *testing.T) {
	first := ClassifyCommand([]string{"go", "test", "./..."})
	same := ClassifyCommand([]string{"go", "test", "./..."})
	different := ClassifyCommand([]string{"go", "test", "-race", "./..."})
	if first.Signature == "" || first.Signature != same.Signature {
		t.Fatalf("stable signature = %q / %q", first.Signature, same.Signature)
	}
	if first.Signature == different.Signature {
		t.Fatalf("different commands shared signature %q", first.Signature)
	}
}

func TestClassifyShellCommandUsesBroadestSegment(t *testing.T) {
	got := ClassifyShellCommand("go test ./internal/resmon && make gate-agent")
	if got.Scope != CommandGate {
		t.Fatalf("scope = %q, want %q", got.Scope, CommandGate)
	}
}

func TestCommandIdentityRequiresStartAndProcessGroup(t *testing.T) {
	table := newProcTable([]procInfo{
		{pid: 100, ppid: 1, pgid: 100, birthID: "birth:one", rssKB: 8, cpuSec: 2},
		{pid: 101, ppid: 100, pgid: 100, birthID: "birth:child", rssKB: 5, cpuSec: 1},
		{pid: 102, ppid: 1, pgid: 102},
	}, false)
	want := CommandIdentity{PID: 100, StartID: "birth:one", ProcessGroup: 100}
	if got, ok := table.CommandIdentity(100); !ok || got != want {
		t.Fatalf("CommandIdentity = (%+v, %v), want (%+v, true)", got, ok, want)
	}
	if !table.MatchesCommand(want) {
		t.Fatal("exact command identity did not match")
	}
	for _, changed := range []CommandIdentity{
		{PID: 100, StartID: "birth:two", ProcessGroup: 100},
		{PID: 100, StartID: "birth:one", ProcessGroup: 999},
		{PID: 101, StartID: "birth:one", ProcessGroup: 100},
	} {
		if table.MatchesCommand(changed) {
			t.Fatalf("changed identity matched: %+v", changed)
		}
	}
	if _, ok := table.CommandIdentity(102); ok {
		t.Fatal("identity without start ID was accepted")
	}
}

func TestEnsureIndependentProcessGroup(t *testing.T) {
	if os.Getenv("KORYPH_RESMON_GROUP_HELPER") == "1" {
		identity, err := EnsureIndependentProcessGroup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(identity); err != nil {
			t.Fatal(err)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEnsureIndependentProcessGroup$")
	cmd.Env = append(os.Environ(), "KORYPH_RESMON_GROUP_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("group helper: %v\n%s", err, out)
	}
	var identity CommandIdentity
	line, _, _ := bytes.Cut(out, []byte("\n"))
	if err := json.Unmarshal(line, &identity); err != nil {
		t.Fatalf("identity JSON: %v\n%s", err, out)
	}
	if identity.PID <= 0 || identity.PID != identity.ProcessGroup || identity.StartID == "" {
		t.Fatalf("identity = %+v, want independent stable process group", identity)
	}
}
