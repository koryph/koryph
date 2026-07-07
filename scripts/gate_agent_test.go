// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package scripts holds a test-only harness for gate-agent.sh
// (koryph-77r.5): a verdict-parity property test proving the shared
// run_stage loop that every real `make gate-agent` stage funnels through
// never converts a failing command into a reported PASS, and never turns
// a zero exit into a reported FAIL.
//
// This does NOT re-run the real gate stages (fmt-check/build/vet/test/
// lint/reuse) — that would pay ~2-3 minutes of go-toolchain cost twice per
// test invocation for a property the harness itself doesn't special-case
// per stage. Instead it drives gate-agent.sh's KORYPH_GATE_AGENT_STAGES
// test seam with synthetic pass/fail commands, exercising the exact same
// code path (bash -c "$cmd" >log 2>&1; tee; summarize; fail-fast) real
// stages use.
//
// A real seeded-failure comparison against `make gate` / `make gate-agent`
// / `make fmt-check` (an untracked badly-gofmt'd .go file, removed after)
// was run by hand during implementation; both reported FAIL with the same
// exit code (2) and gate-agent's tail showed the same gofmt violation
// `make gate` printed directly. See the bead report (koryph-77r.5) for the
// transcript. That manual check is the honest complement to this test: it
// proves the ONE real stage command gate-agent.sh replaces verbatim
// (fmt-check) behaves identically; this test proves the wrapping harness
// around every stage (real or synthetic) preserves the verdict contract.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGateAgent invokes scripts/gate-agent.sh with a synthetic stage list,
// returning its combined stdout+stderr, its process exit code, and the log
// directory it was told to use.
func runGateAgent(t *testing.T, stages string) (output string, exitCode int, logDir string) {
	t.Helper()
	logDir = t.TempDir()
	cmd := exec.Command("bash", "gate-agent.sh", logDir)
	cmd.Env = append(os.Environ(), "KORYPH_GATE_AGENT_STAGES="+stages)
	out, err := cmd.CombinedOutput()
	output = string(out)
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("gate-agent.sh: %v\noutput:\n%s", err, output)
		}
	}
	return output, exitCode, logDir
}

func TestGateAgentAllPass_ExitsZero(t *testing.T) {
	out, code, logDir := runGateAgent(t, "a|true\nb|true")
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (all stages passed)", code)
	}
	for _, want := range []string{"==> a: PASS", "==> b: PASS", "full output: " + logDir} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, name := range []string{"gate-a.log", "gate-b.log"} {
		if _, err := os.Stat(filepath.Join(logDir, name)); err != nil {
			t.Errorf("expected log %q: %v", name, err)
		}
	}
}

// TestGateAgentSeededFailure_ExitsNonZeroAndNeverSwallowsIt is the
// verdict-parity property: a failing stage must produce a non-zero
// gate-agent.sh exit (the same "fail" verdict `make gate` would report for
// an equivalent failing prerequisite) and the failure text must reach
// stdout via the tail — the summarizer must never eat it.
func TestGateAgentSeededFailure_ExitsNonZeroAndNeverSwallowsIt(t *testing.T) {
	out, code, logDir := runGateAgent(t, "ok|true\nboom|sh -c 'echo seeded-failure-marker; exit 7'\nnever|true")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero (seeded stage failed):\n%s", out)
	}
	if !strings.Contains(out, "==> ok: PASS") {
		t.Errorf("first stage should still report PASS:\n%s", out)
	}
	if !strings.Contains(out, "==> boom: FAIL (exit 7)") {
		t.Errorf("failing stage's exit code not preserved:\n%s", out)
	}
	if !strings.Contains(out, "seeded-failure-marker") {
		t.Errorf("failure output was swallowed instead of tailed to stdout:\n%s", out)
	}
	if strings.Contains(out, "==> never:") {
		t.Errorf("stage after a failure ran; want fail-fast (matches make's own prerequisite-list behavior):\n%s", out)
	}
	if !strings.Contains(out, "full output: "+logDir) {
		t.Errorf("missing pointer to the full log dir:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(logDir, "gate-boom.log")); err != nil {
		t.Errorf("expected full log for the failing stage: %v", err)
	}
}

func TestGateAgentRequiresLogDirArg(t *testing.T) {
	cmd := exec.Command("bash", "gate-agent.sh")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure with no log-dir arg, got success:\n%s", out)
	}
}
