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

func gateAgentEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if _, replaced := overrides[name]; !replaced {
			env = append(env, pair)
		}
	}
	for name, value := range overrides {
		env = append(env, name+"="+value)
	}
	return env
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

func TestGateAgentTestStageSanitizesDispatchEnvironment(t *testing.T) {
	logDir := t.TempDir()
	cmd := exec.Command("bash", "gate-agent.sh", logDir)
	cmd.Env = gateAgentEnv(map[string]string{
		"KORYPH_GATE_AGENT_STAGES": "test|for name in KORYPH_RUN_ID KORYPH_SESSION_ID KORYPH_PHASE_ID KORYPH_SPAWN_KIND KORYPH_PHASE_DIR KORYPH_STATUS_PATH KORYPH_SUMMARY_PATH KORYPH_LOG_PATH KORYPH_DIR; do if printenv \"$name\" >/dev/null; then echo \"$name=present\"; else echo \"$name=unset\"; fi; done; test -d \"$KORYPH_HOME\"; test -x \"$KORYPH_BD_BIN\"; test \"$(command -v bd)\" = \"$KORYPH_BD_BIN\"; \"$KORYPH_BD_BIN\" list --parent live-project; test $? -eq 97; bd list --parent live-project; test $? -eq 97",
		"KORYPH_RUN_ID":            "host-run",
		"KORYPH_SESSION_ID":        "host-session",
		"KORYPH_PHASE_ID":          "host-phase",
		"KORYPH_SPAWN_KIND":        "host-spawn",
		"KORYPH_PHASE_DIR":         "/live/phase",
		"KORYPH_STATUS_PATH":       "/live/status.json",
		"KORYPH_SUMMARY_PATH":      "/live/SUMMARY.md",
		"KORYPH_LOG_PATH":          "/live/session.log",
		"KORYPH_DIR":               "/live/dispatch",
		"KORYPH_HOME":              "/live/koryph-home",
		"KORYPH_BD_BIN":            "/live/bd",
	})
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gate-agent test stage: %v\noutput:\n%s", err, out)
	}

	stageLog, err := os.ReadFile(filepath.Join(logDir, "gate-test.log"))
	if err != nil {
		t.Fatalf("read test-stage log: %v", err)
	}
	for _, name := range []string{
		"KORYPH_RUN_ID", "KORYPH_SESSION_ID", "KORYPH_PHASE_ID", "KORYPH_SPAWN_KIND",
		"KORYPH_PHASE_DIR", "KORYPH_STATUS_PATH", "KORYPH_SUMMARY_PATH", "KORYPH_LOG_PATH", "KORYPH_DIR",
	} {
		if !strings.Contains(string(stageLog), name+"=unset") {
			t.Errorf("test subprocess inherited %s:\n%s", name, stageLog)
		}
	}
	if got := strings.Count(string(stageLog), "gate-agent test fixture refuses bd invocation: list --parent live-project"); got != 2 {
		t.Errorf("fixture did not intercept explicit and PATH bd invocations (got %d):\n%s", got, stageLog)
	}
}

func TestGateAgentFixtureSetupFailureFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		script  string
	}{
		{name: "mktemp", command: "mktemp", script: "#!/bin/sh\nexit 61\n"},
		{name: "mkdir", command: "mkdir", script: "#!/bin/sh\ncase \"$*\" in\n  *gate-test-env.*) exit 61 ;;\nesac\nexec /bin/mkdir \"$@\"\n"},
		{name: "cat", command: "cat", script: "#!/bin/sh\nexit 61\n"},
		{name: "chmod", command: "chmod", script: "#!/bin/sh\nexit 61\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			if err := os.WriteFile(filepath.Join(fakeBin, tc.command), []byte(tc.script), 0o755); err != nil {
				t.Fatalf("write failing %s fixture command: %v", tc.command, err)
			}

			logDir := t.TempDir()
			cmd := exec.Command("bash", "gate-agent.sh", logDir)
			cmd.Env = gateAgentEnv(map[string]string{
				"KORYPH_GATE_AGENT_STAGES": "test|echo test-command-ran",
				"PATH":                     fakeBin + ":" + os.Getenv("PATH"),
			})
			out, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if err == nil || !ok || exitErr.ExitCode() == 0 {
				t.Fatalf("gate-agent succeeded after %s setup failure:\n%s", tc.command, out)
			}
			if strings.Contains(string(out), "test-command-ran") {
				t.Fatalf("test command ran after %s setup failure:\n%s", tc.command, out)
			}
		})
	}
}

func TestGateAgentFailingTestStagePreservesExitCode(t *testing.T) {
	out, code, logDir := runGateAgent(t, "test|echo sanitized-test-failure; exit 23")
	if code != 23 {
		t.Fatalf("exit code = %d, want 23:\n%s", code, out)
	}
	if !strings.Contains(out, "==> test: FAIL (exit 23)") {
		t.Errorf("sanitized test stage was not reported as failed:\n%s", out)
	}
	stageLog, err := os.ReadFile(filepath.Join(logDir, "gate-test.log"))
	if err != nil {
		t.Fatalf("read test-stage log: %v", err)
	}
	if !strings.Contains(string(stageLog), "sanitized-test-failure") {
		t.Errorf("failing test-stage log missing marker:\n%s", stageLog)
	}
}
