// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package hooks

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runGuard feeds a synthetic PreToolUse Bash payload to agent-boundary-guard.sh
// as a koryph-dispatched agent would see it (KORYPH_PHASE_ID set) and returns
// its stdout and exit code.
func runGuard(t *testing.T, command string) (stdout string, exitCode int) {
	t.Helper()
	payload := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(command) + `}}`
	cmd := exec.Command("bash", "agent-boundary-guard.sh")
	cmd.Env = append(os.Environ(), "KORYPH_PHASE_ID=phase-test")
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), ee.ExitCode()
		}
		t.Fatalf("running agent-boundary-guard.sh: %v", err)
	}
	return out.String(), 0
}

// jsonQuote is a minimal JSON string-literal quoter sufficient for the ASCII
// test commands below (avoids importing encoding/json for one line).
func jsonQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// TestVerboseCommandNudgePolicy is the "hook policy table" test for
// koryph-77r.5: the two highest-confidence verbose-output patterns
// (go test -v on a broad package set; unfiltered golangci-lint run) are
// denied with a message pointing at the quiet gate; narrow/ambiguous/
// already-filtered variants pass through, per the conservative,
// data-driven policy documented in the script's header comment.
func TestVerboseCommandNudgePolicy(t *testing.T) {
	cases := []struct {
		name        string
		command     string
		wantDeny    bool
		wantContain string // substring of the deny reason, when wantDeny
	}{
		{"go test broad -v trailing", "go test ./... -v", true, "make gate-agent"},
		{"go test broad -v leading", "go test -v ./...", true, "make gate-agent"},
		{"go test broad -v with extra flags", "go test ./... -v -count=1", true, "make gate-agent"},
		{"go test narrow -v allowed", "go test ./internal/rules -v", false, ""},
		{"go test broad -v=false allowed (explicit non-verbose)", "go test ./... -v=false", false, ""},
		{"go test broad -vet=off allowed (not -v)", "go test ./... -vet=off", false, ""},
		{"make test allowed", "make test", false, ""},
		{"golangci-lint run wildcard unfiltered", "golangci-lint run ./...", true, "make lint-agent"},
		{"golangci-lint run bare unfiltered", "golangci-lint run", true, "make lint-agent"},
		{"golangci-lint run filtered allowed", "golangci-lint run --output.text.print-issued-lines=false ./...", false, ""},
		{"make lint allowed", "make lint", false, ""},
		{"make gate-agent allowed", "make gate-agent", false, ""},
		{"chained: safe prefix then verbose -v denied", "make build && go test ./... -v", true, "make gate-agent"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runGuard(t, tc.command)
			denied := strings.Contains(out, `"permissionDecision": "deny"`)
			if denied != tc.wantDeny {
				t.Fatalf("command %q: denied=%v, want %v (exit=%d, out=%s)", tc.command, denied, tc.wantDeny, code, out)
			}
			if tc.wantDeny && !strings.Contains(out, tc.wantContain) {
				t.Errorf("command %q: deny reason missing %q:\n%s", tc.command, tc.wantContain, out)
			}
			if code != 0 {
				t.Errorf("command %q: exit code = %d, want 0 (jq path always exits 0; the deny signal is the JSON body)", tc.command, code)
			}
		})
	}
}

// TestBoundaryDenialsUnaffected is a narrow regression check that the
// pre-existing orchestrator-boundary denials still fire after adding the
// verbose-command nudge policy in the same script.
func TestBoundaryDenialsUnaffected(t *testing.T) {
	cases := []struct {
		name     string
		command  string
		wantDeny bool
	}{
		{"git push denied", "git push origin HEAD", true},
		{"git commit allowed", `git commit -m "feat: x"`, false},
		{"git rebase onto main allowed", "git rebase origin/main", false},
		{"bd close denied", "bd close koryph-1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := runGuard(t, tc.command)
			denied := strings.Contains(out, `"permissionDecision": "deny"`)
			if denied != tc.wantDeny {
				t.Fatalf("command %q: denied=%v, want %v (out=%s)", tc.command, denied, tc.wantDeny, out)
			}
		})
	}
}

// TestNotEnforcedWithoutPhaseID proves the whole policy (boundary +
// verbose-command nudges) is inert for interactive sessions.
func TestNotEnforcedWithoutPhaseID(t *testing.T) {
	payload := `{"tool_name":"Bash","tool_input":{"command":"go test ./... -v"}}`
	cmd := exec.Command("bash", "agent-boundary-guard.sh")
	// Explicitly scrub KORYPH_PHASE_ID rather than relying on it being absent
	// from the ambient environment (this test may itself run inside a koryph
	// dispatch).
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KORYPH_PHASE_ID=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("running agent-boundary-guard.sh without KORYPH_PHASE_ID: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output when KORYPH_PHASE_ID is unset, got: %s", out.String())
	}
}
