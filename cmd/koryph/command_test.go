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

	"github.com/koryph/koryph/internal/commandguard"
	"github.com/koryph/koryph/internal/engine"
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

func TestCommandExecOwnerLeaseOutlivesDirectChild(t *testing.T) {
	phase := canonicalTestDir(t)
	starts := filepath.Join(canonicalTestDir(t), "starts")
	descendantDone := filepath.Join(canonicalTestDir(t), "descendant-done")
	tool := writeTool(t,
		`echo start >>"`+starts+`"
(sleep 1; echo done >"`+descendantDone+`") &
exit 0`)
	first := commandHelper(t, phase, tool, "test", "./...")
	second := commandHelper(t, phase, tool, "test", "./...")
	var firstOut, secondOut bytes.Buffer
	first.Stdout, first.Stderr = &firstOut, &firstOut
	second.Stdout, second.Stderr = &secondOut, &secondOut
	started := time.Now()
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	// The full repository gate starts many package test binaries concurrently.
	// Give this subprocess a scheduling budget that does not turn host load into
	// a false lease-lifecycle failure.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(starts); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = first.Process.Signal(syscall.SIGTERM)
			t.Fatal("real tool did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := second.Start(); err != nil {
		_ = first.Process.Signal(syscall.SIGTERM)
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first exit = %v\n%s", err, firstOut.String())
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("second exit = %v\n%s", err, secondOut.String())
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("wrappers returned before inherited lease drained: %s", elapsed)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "start") != 1 {
		t.Fatalf("real tool starts = %q, err=%v", data, err)
	}
	if data, err := os.ReadFile(descendantDone); err != nil || strings.TrimSpace(string(data)) != "done" {
		t.Fatalf("descendant completion = %q, err=%v", data, err)
	}
	if combined := firstOut.String() + secondOut.String(); !strings.Contains(combined, " role=worker status=") {
		t.Fatalf("duplicate did not reuse cohort-held lease:\n%s", combined)
	}
}

func TestCommandExecOwnerLeaseOutlivesKilledWrapper(t *testing.T) {
	phase := canonicalTestDir(t)
	starts := filepath.Join(canonicalTestDir(t), "starts")
	descendantDone := filepath.Join(canonicalTestDir(t), "descendant-done")
	tool := writeTool(t,
		`echo start >>"`+starts+`"
sleep 1
echo done >"`+descendantDone+`"`)
	first := commandHelper(t, phase, tool, "test", "./...")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	// The full repository gate starts many package test binaries concurrently.
	// Give this subprocess a scheduling budget that does not turn host load into
	// a false lease-lifecycle failure.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(starts); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = first.Process.Signal(syscall.SIGKILL)
			t.Fatal("real tool did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := first.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	guard := commandguard.NewProcessGuard(phase)
	held, err := guard.OwnerLeaseHeld([]string{tool, "test", "./..."})
	if err != nil || !held {
		t.Fatalf("owner lease after wrapper death = %v, %v; want held", held, err)
	}

	second := commandHelper(t, phase, tool, "test", "./...")
	out, err := second.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != engine.ExitFatal {
		t.Fatalf("duplicate exit = %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "koryph command reuse:") ||
		!strings.Contains(string(out), "authoritative command exited without its generation-bound result") {
		t.Fatalf("duplicate did not wait on the inherited cohort lease:\n%s", out)
	}
	if data, err := os.ReadFile(starts); err != nil || strings.Count(string(data), "start") != 1 {
		t.Fatalf("real tool starts = %q, err=%v; want exactly one", data, err)
	}
	if data, err := os.ReadFile(descendantDone); err != nil || strings.TrimSpace(string(data)) != "done" {
		t.Fatalf("surviving real tool completion = %q, err=%v", data, err)
	}
	held, err = guard.OwnerLeaseHeld([]string{tool, "test", "./..."})
	if err != nil || held {
		t.Fatalf("owner lease after cohort exit = %v, %v; want released", held, err)
	}
	if err := first.Wait(); err == nil {
		t.Fatal("killed wrapper unexpectedly succeeded")
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
