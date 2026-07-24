// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/runtime"
)

const (
	defaultBin    = "codex"
	detectTimeout = 10 * time.Second
)

// Codex implements runtime.Runtime for the Codex CLI. Bin is primarily a
// test and operator override; an empty value resolves from PATH.
type Codex struct{ Bin string }

func New(bin string) runtime.Runtime { return Codex{Bin: bin} }

func (c Codex) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return defaultBin
}

func (c Codex) Name() string     { return "codex" }
func (c Codex) Provider() string { return "openai" }

func (c Codex) Detect(ctx context.Context) (bool, string) {
	if !execx.LookPath(c.bin()) {
		return false, ""
	}
	res, err := execx.Run(ctx, execx.Cmd{Name: c.bin(), Args: []string{"--version"}, Timeout: detectTimeout})
	if err != nil || res.ExitCode != 0 {
		return true, ""
	}
	return true, strings.TrimSpace(res.Stdout)
}

// AuthCheck uses Codex's own local status command. It spends no API quota and
// therefore works for both ChatGPT-backed and API-key-backed CLI sessions.
func (c Codex) AuthCheck(ctx context.Context, profile runtime.Profile) error {
	res, err := execx.Run(ctx, execx.Cmd{
		Name: c.bin(), Args: []string{"login", "status"}, Env: runtime.ChildEnv(runtime.EnvSpec{AccountEnv: c.AccountEnv(profile)}), Timeout: detectTimeout,
	})
	if err != nil {
		return fmt.Errorf("codex authentication check: %w", err)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return fmt.Errorf("codex is not logged in for profile %q: %s", profile.Name, msg)
	}
	return nil
}

// VerifyIdentity binds a Codex profile to a non-secret fingerprint of its
// auth.json. Codex login status intentionally does not disclose an email, so
// comparing an email would be a false security guarantee. Enrollment records
// the returned codex:<hash> identity in runtime_accounts.codex.
func (c Codex) VerifyIdentity(ctx context.Context, profile runtime.Profile, expected string) (string, error) {
	got, err := c.CurrentIdentity(ctx, profile)
	if err != nil {
		return "", err
	}
	if expected == "" {
		return "", fmt.Errorf("codex profile %q requires expected_identity %q", profile.Name, got)
	}
	if !strings.EqualFold(expected, got) {
		return "", fmt.Errorf("codex identity mismatch: got %q, want %q", got, expected)
	}
	return got, nil
}

// CurrentIdentity returns a stable, non-secret fingerprint of Codex's local
// authentication record. Codex login status intentionally does not disclose
// an email, so the fingerprint is the only identity koryph can verify
// deterministically without making a billed request.
func (c Codex) CurrentIdentity(ctx context.Context, profile runtime.Profile) (string, error) {
	if err := c.AuthCheck(ctx, profile); err != nil {
		return "", err
	}
	path, err := authPath(profile)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return "", fmt.Errorf("codex authentication record %s is unavailable", path)
	}
	sum := sha256.Sum256(b)
	return "codex:" + hex.EncodeToString(sum[:8]), nil
}

func authPath(profile runtime.Profile) (string, error) {
	if profile.ConfigDir != "" {
		return filepath.Join(profile.ConfigDir, "auth.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve default CODEX_HOME: %w", err)
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

func (c Codex) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		JSONStream: true, Personas: true, Hooks: true, Resume: false,
		EffortFlag: true, BudgetFlag: false, Sandbox: true, ModelSelect: true,
		UsageSource: false,
	}
}

func (c Codex) InstructionFile() string { return "AGENTS.md" }

func (c Codex) AccountEnv(profile runtime.Profile) []string {
	if profile.ConfigDir == "" {
		return nil
	}
	return []string{"CODEX_HOME=" + profile.ConfigDir}
}

func (c Codex) ModelMap() runtime.ModelMap { return runtime.CodexModelMap }

func (c Codex) EffortMap() runtime.EffortMap { return runtime.CodexEffortMap }

func (c Codex) Command(spec runtime.DispatchSpec) ([]string, []string, error) {
	if spec.ResumeSessionID != "" {
		return nil, nil, fmt.Errorf("codex: session resume is unavailable with Codex CLI's safe workspace-write launch mode")
	}
	if spec.MaxBudgetUSD > 0 {
		return nil, nil, fmt.Errorf("codex: MaxBudgetUSD is unsupported")
	}
	// koryph owns the hook source and installs it outside the dispatched
	// worktree. Headless runs cannot complete Codex's interactive hook-trust
	// review, so permit these already-vetted hooks without weakening sandbox or
	// approval policy.
	// --ask-for-approval is a global Codex CLI option (not an `exec` option)
	// as of Codex 0.145. Keep it before the subcommand so a dispatched agent
	// never falls back to an interactive approval prompt or exits on argument
	// parsing before it sees the task.
	args := []string{"--ask-for-approval", "never", "exec", "--json", "--sandbox", "workspace-write", "--dangerously-bypass-hook-trust", "--add-dir", spec.PhaseDir}
	// A linked worktree's .git is a pointer file whose writable metadata lives
	// in the primary repository's .git directory. Permit precisely that
	// directory so normal agent commits work without widening the sandbox to
	// the primary checkout.
	if spec.RepoRoot != "" {
		args = append(args, "--add-dir", filepath.Join(spec.RepoRoot, ".git"))
	}
	// Codex's workspace-write sandbox denies access to Unix sockets outside
	// the worktree unless their parent directory is an explicit writable
	// root. SSH_AUTH_SOCK alone is therefore insufficient: the scoped koryph
	// signing agent is reachable by Claude (which has no CLI sandbox) but not
	// by Codex. Grant only the directory containing koryph's signing-only
	// socket, never the operator's ambient agent. The generic DispatchSpec
	// keeps this transport runtime-neutral; this adapter owns the Codex-
	// specific sandbox projection.
	if spec.SSHAuthSock != "" {
		args = append(args, "--add-dir", filepath.Dir(spec.SSHAuthSock))
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(spec.Effort))
	}
	args = append(args, "--output-last-message", filepath.Join(spec.PhaseDir, "SUMMARY.md"))
	return append([]string{c.bin()}, args...), c.childEnv(spec.Profile, spec.Billing, spec.APIKey, spec.CredentialEnvVar, spec.Credential, spec.SSHAuthSock, spec.EnvPassthrough), nil
}

func (c Codex) CommandJSON(spec runtime.JSONSpec) ([]string, []string, error) {
	if spec.MaxBudgetUSD > 0 || spec.Fallback {
		return nil, nil, fmt.Errorf("codex: budget caps and fallback models are unsupported")
	}
	// Deliberately omit --json here: JSONSpec callers need the final model text
	// itself (a strict verdict JSON object), while the long-lived dispatch path
	// above needs lifecycle JSONL for polling.
	args := []string{"--ask-for-approval", "never", "exec", "--sandbox", "workspace-write", "--dangerously-bypass-hook-trust"}
	if spec.RepoRoot != "" {
		args = append(args, "--add-dir", filepath.Join(spec.RepoRoot, ".git"))
	}
	// Mutating JSON spawns (post-implementation stages) may create commits too.
	// Apply the same scoped signing transport as the long-lived dispatch.
	if spec.SSHAuthSock != "" {
		args = append(args, "--add-dir", filepath.Dir(spec.SSHAuthSock))
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(spec.Effort))
	}
	return append([]string{c.bin()}, args...), c.childEnv(spec.Profile, spec.Billing, spec.APIKey, spec.CredentialEnvVar, spec.Credential, spec.SSHAuthSock, spec.EnvPassthrough), nil
}

func (c Codex) childEnv(profile runtime.Profile, billing runtime.BillingMode, apiKey, credentialEnv, credential, sshAuthSock string, passthrough []string) []string {
	apiKeyEnv := ""
	if billing == runtime.BillingAPIKey {
		apiKeyEnv = "OPENAI_API_KEY"
	}
	return runtime.ChildEnv(runtime.EnvSpec{
		AccountEnv: c.AccountEnv(profile), APIKeyEnvVar: apiKeyEnv, APIKey: apiKey,
		CredentialEnvVar: credentialEnv, Credential: credential, SSHAuthSock: sshAuthSock, Passthrough: passthrough,
	})
}

func tomlString(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

var _ runtime.Runtime = Codex{}
var _ runtime.IdentityProber = Codex{}
var _ runtime.WorkflowProjector = Codex{}
