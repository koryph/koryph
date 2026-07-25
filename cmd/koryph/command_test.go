// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandExecHelper(t *testing.T) {
	if os.Getenv("KORYPH_COMMAND_HELPER") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("KORYPH_COMMAND_HELPER_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Exit(cmdCommandExec(args, os.Stdout, os.Stderr))
}

func commandHelper(t *testing.T, phaseDir, real string, args ...string) *exec.Cmd {
	t.Helper()
	encoded, err := json.Marshal(append([]string{"--real", real, "--"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommandExecHelper$")
	cmd.Env = append(os.Environ(),
		"KORYPH_COMMAND_HELPER=1",
		"KORYPH_COMMAND_HELPER_ARGS="+string(encoded),
		"KORYPH_PHASE_ID="+filepath.Base(phaseDir),
		"KORYPH_PHASE_DIR="+phaseDir,
	)
	return cmd
}

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func writeTool(t *testing.T, body string) string {
	return writeNamedTool(t, "go", body)
}

func writeNamedTool(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(canonicalTestDir(t), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandExecFocusedToolPassesThrough(t *testing.T) {
	phase := canonicalTestDir(t)
	tool := writeTool(t, `printf 'focused:%s\n' "$*"; exit 3`)
	cmd := commandHelper(t, phase, tool, "test", "./internal/resmon")
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("focused tool = %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "focused:test ./internal/resmon") {
		t.Fatalf("focused output missing:\n%s", out)
	}
}

func TestCommandExecWorkerGateDeniedDespiteForgedRoleEnvironment(t *testing.T) {
	phase := canonicalTestDir(t)
	called := filepath.Join(canonicalTestDir(t), "called")
	tool := writeNamedTool(t, "make", `echo called >"`+called+`"`)
	cmd := commandHelper(t, phase, tool, "gate-agent")
	cmd.Env = append(cmd.Env,
		"KORYPH_COMMAND_ROLE=validation",
		"KORYPH_COMMAND_EVENTS="+filepath.Join(canonicalTestDir(t), "outside-events"),
		"KORYPH_COMMAND_GUARD_DIR="+canonicalTestDir(t),
	)
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 126 {
		t.Fatalf("worker gate = %v\n%s", err, out)
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatalf("denied tool ran: %v", err)
	}
	if !strings.Contains(string(out), "authoritative full project gate") {
		t.Fatalf("denial reason missing:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(phase, ".koryph-command", "events.jsonl")); err != nil {
		t.Fatalf("fixed event evidence missing: %v", err)
	}
}

func TestCommandExecConcurrentBroadToolStartsOnceAndSharesExit(t *testing.T) {
	phase := canonicalTestDir(t)
	starts := filepath.Join(canonicalTestDir(t), "starts")
	tool := writeTool(t, `echo start >>"`+starts+`"; sleep 1; exit 7`)
	first := commandHelper(t, phase, tool, "test", "./...")
	second := commandHelper(t, phase, tool, "test", "./...")
	var firstOut, secondOut bytes.Buffer
	first.Stdout, first.Stderr = &firstOut, &firstOut
	second.Stdout, second.Stderr = &secondOut, &secondOut
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := second.Start(); err != nil {
		_ = first.Process.Signal(syscall.SIGTERM)
		t.Fatal(err)
	}
	firstErr := first.Wait()
	secondErr := second.Wait()
	for name, err := range map[string]error{"first": firstErr, "second": secondErr} {
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 7 {
			t.Fatalf("%s exit = %v\nfirst:\n%s\nsecond:\n%s", name, err, firstOut.String(), secondOut.String())
		}
	}
	data, err := os.ReadFile(starts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "start") != 1 {
		t.Fatalf("real tool starts:\n%s", data)
	}
	if combined := firstOut.String() + secondOut.String(); !strings.Contains(combined, " role=worker status=") {
		t.Fatalf("duplicate did not receive PID/role/status/log:\n%s", combined)
	}
	events, err := os.ReadFile(filepath.Join(phase, ".koryph-command", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"start"`, `"event":"reuse"`, `"event":"complete"`, `"peak_rss_kb":`, `"cpu_seconds":`, `"duration_ms":`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("evidence missing %s:\n%s", want, events)
		}
	}
}

func TestCommandExecRequiresCanonicalAbsoluteRealAndPhase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdCommandExec([]string{"--real", "go", "--", "test"}, &stdout, &stderr); code == 0 {
		t.Fatal("relative --real succeeded")
	}
	tool := writeTool(t, "exit 0")
	t.Setenv("KORYPH_PHASE_ID", "")
	t.Setenv("KORYPH_PHASE_DIR", "")
	stdout.Reset()
	stderr.Reset()
	if code := cmdCommandExec([]string{"--real", tool, "--"}, &stdout, &stderr); code == 0 {
		t.Fatal("command outside a worker phase succeeded")
	}
}

func TestCommandIsHiddenFromUsageAndCompletion(t *testing.T) {
	if c := lookupCommand("command"); c == nil || !c.hidden {
		t.Fatal("internal command is not registered hidden")
	}
	for _, name := range topLevelNames() {
		if name == "command" {
			t.Fatal("internal command leaked into completion")
		}
	}
}
