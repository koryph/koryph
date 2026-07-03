// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package execx_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/execx"
)

func TestRun_ExitZero(t *testing.T) {
	res, err := execx.Run(context.Background(), execx.Cmd{Name: "true"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", res.Duration)
	}
}

func TestRun_NonZeroExit(t *testing.T) {
	res, err := execx.Run(context.Background(), execx.Cmd{Name: "false"})
	if err != nil {
		// Non-zero exit must not be an error — only spawn/timeout failures are.
		t.Fatalf("non-zero exit should not be an error, got: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("exit code = 0, want non-zero")
	}
}

func TestRun_TimeoutKillsProcess(t *testing.T) {
	// On Go 1.20+, a process killed by SIGKILL due to context deadline returns
	// as *exec.ExitError (non-zero code, nil error from Run) rather than a
	// distinct timeout error. The observable contract is:
	//   • the command is terminated well before its natural sleep duration
	//   • no error is returned (killed process == non-zero exit, not a spawn failure)
	//   • ExitCode is non-zero (typically -1 for a signal-killed process)
	start := time.Now()
	res, err := execx.Run(context.Background(), execx.Cmd{
		Name:    "sleep",
		Args:    []string{"60"},
		Timeout: 100 * time.Millisecond,
	})
	elapsed := time.Since(start)

	// Should finish in well under 5s, not 60s.
	if elapsed >= 5*time.Second {
		t.Errorf("timed-out command ran for %v; expected early termination", elapsed)
	}
	// SIGKILL produces a non-zero ExitCode, not a returned error.
	if err != nil {
		t.Errorf("unexpected error from killed process: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("exit code = 0 for SIGKILL'd process, want non-zero")
	}
}

func TestRun_MissingBinary(t *testing.T) {
	_, err := execx.Run(context.Background(), execx.Cmd{
		Name: "this-binary-does-not-exist-koryph-test",
	})
	if err == nil {
		t.Fatal("expected spawn error for missing binary, got nil")
	}
}

func TestRun_StdinPassed(t *testing.T) {
	res, err := execx.Run(context.Background(), execx.Cmd{
		Name:  "cat",
		Stdin: "hello stdin",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Stdout != "hello stdin" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hello stdin")
	}
}

func TestRun_StdoutCaptured(t *testing.T) {
	res, err := execx.Run(context.Background(), execx.Cmd{
		Name: "echo",
		Args: []string{"koryph"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "koryph") {
		t.Errorf("stdout = %q, want 'koryph'", res.Stdout)
	}
}

func TestMustSucceed_Success(t *testing.T) {
	res, err := execx.MustSucceed(context.Background(), execx.Cmd{
		Name: "echo",
		Args: []string{"ok"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "ok") {
		t.Errorf("stdout = %q, want 'ok'", res.Stdout)
	}
}

func TestMustSucceed_NonZeroReturnsError(t *testing.T) {
	_, err := execx.MustSucceed(context.Background(), execx.Cmd{Name: "false"})
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
}

func TestMustSucceed_StderrTailTruncation(t *testing.T) {
	// Produce > 800 bytes on stderr then exit 1 so MustSucceed includes the tail.
	// We write 810 'x' characters → the raw stderr is 811 bytes (plus newline from echo).
	longLine := strings.Repeat("x", 810)
	script := "echo " + longLine + " >&2; exit 1"
	_, err := execx.MustSucceed(context.Background(), execx.Cmd{
		Name: "sh",
		Args: []string{"-c", script},
	})
	if err == nil {
		t.Fatal("expected error from MustSucceed with non-zero exit, got nil")
	}
	msg := err.Error()
	// The error must contain an exit-code reference.
	if !strings.Contains(msg, "exit 1") {
		t.Errorf("error %q missing 'exit 1'", msg)
	}
	// The stderr snippet in the error should not exceed 800 bytes.
	// Locate the tail portion after the last ": " and check its length.
	if idx := strings.LastIndex(msg, ": "); idx >= 0 {
		tail := msg[idx+2:]
		if len(tail) > 800 {
			t.Errorf("stderr tail in error is %d bytes, want <= 800", len(tail))
		}
	}
}

func TestRun_EnvOverride(t *testing.T) {
	// When Cmd.Env is set, only those variables reach the child.
	res, err := execx.Run(context.Background(), execx.Cmd{
		Name: "sh",
		Args: []string{"-c", "echo $KORYPH_TEST_VAR"},
		Env:  []string{"KORYPH_TEST_VAR=sentinel"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "sentinel") {
		t.Errorf("stdout = %q, want 'sentinel'", res.Stdout)
	}
}
