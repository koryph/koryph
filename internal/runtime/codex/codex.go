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
	"sort"
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
		ScopedSigningSocket: true, UsageSource: false,
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
	args := []string{"--ask-for-approval", "never", "exec", "--json"}
	args = append(args, sandboxArgs(spec.SSHAuthSock, spec.RepoRoot)...)
	args = append(args, "--dangerously-bypass-hook-trust", "--add-dir", spec.PhaseDir)
	// A linked worktree's .git is a pointer file whose writable metadata lives
	// in the primary repository's .git directory. Permit precisely that
	// directory so normal agent commits work without widening the sandbox to
	// the primary checkout.
	if spec.RepoRoot != "" {
		args = append(args, "--add-dir", filepath.Join(spec.RepoRoot, ".git"))
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(spec.Effort))
	}
	args = append(args, "--output-last-message", filepath.Join(spec.PhaseDir, "SUMMARY.md"))
	env := c.childEnv(spec.Profile, spec.Billing, spec.APIKey, spec.CredentialEnvVar, spec.Credential, spec.SSHAuthSock, spec.EnvPassthrough)
	env = append(env, sandboxCacheEnv(spec.SSHAuthSock, spec.PhaseDir)...)
	return append([]string{c.bin()}, args...), env, nil
}

func (c Codex) CommandJSON(spec runtime.JSONSpec) ([]string, []string, error) {
	if spec.MaxBudgetUSD > 0 || spec.Fallback {
		return nil, nil, fmt.Errorf("codex: budget caps and fallback models are unsupported")
	}
	// Deliberately omit --json here: JSONSpec callers need the final model text
	// itself (a strict verdict JSON object), while the long-lived dispatch path
	// above needs lifecycle JSONL for polling.
	args := []string{"--ask-for-approval", "never", "exec"}
	args = append(args, sandboxArgs(spec.SSHAuthSock, spec.RepoRoot)...)
	args = append(args, "--dangerously-bypass-hook-trust")
	if spec.RepoRoot != "" {
		args = append(args, "--add-dir", filepath.Join(spec.RepoRoot, ".git"))
	}
	if spec.ScratchDir != "" {
		args = append(args, "--add-dir", spec.ScratchDir)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(spec.Effort))
	}
	env := c.childEnv(spec.Profile, spec.Billing, spec.APIKey, spec.CredentialEnvVar, spec.Credential, spec.SSHAuthSock, spec.EnvPassthrough)
	env = append(env, sandboxCacheEnv(spec.SSHAuthSock, spec.ScratchDir)...)
	return append([]string{c.bin()}, args...), env, nil
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

// sandboxArgs selects Codex's least-privilege command sandbox. A plain
// workspace-write launch is sufficient when signing is not requested. SSH
// signing needs one additional capability that --add-dir does not grant:
// connecting to the agent's Unix-domain socket. Codex permission profiles
// model that capability directly, so use an invocation-local profile that:
//   - keeps writes limited to the workspace roots (the worktree plus add-dir);
//   - enables no public network destinations; and
//   - allowlists exactly the koryph scoped signing socket.
//
// --ignore-user-config prevents a user's legacy sandbox_mode setting from
// silently overriding default_permissions; Codex authentication still uses
// CODEX_HOME, per the CLI contract. The operator's ambient SSH agent is never
// passed into this function or the child environment.
func sandboxArgs(sshAuthSock, repoRoot string) []string {
	if sshAuthSock == "" {
		return []string{"--sandbox", "workspace-write"}
	}
	socketRule := "permissions.koryph_signing.network.unix_sockets={" +
		tomlString(sshAuthSock) + `="allow"}`
	return []string{
		"--ignore-user-config",
		"-c", `default_permissions="koryph_signing"`,
		"-c", signingFilesystemRule(repoRoot),
		"-c", "permissions.koryph_signing.network.enabled=true",
		"-c", socketRule,
	}
}

// signingFilesystemRule preserves the normal command toolchain without
// granting broad filesystem writes. Codex's :minimal profile intentionally
// excludes package-manager/Nix toolchains and the user's Git configuration;
// without read-only grants for those locations macOS falls back to the
// /usr/bin/git Xcode shim and fails before Git can request a signature.
//
// PATH is already an explicit child-environment allowlist entry. Granting read
// access to its roots does not expose a new ambient secret surface; it merely
// lets the subprocess execute the tools koryph deliberately placed on PATH.
// Homebrew and Nix symlinks resolve outside their bin directories, so collapse
// those entries to their immutable installation roots. Git config is required
// for identity and the public signing-key reference; private key material is
// never stored there or passed to Codex.
func signingFilesystemRule(repoRoot string) string {
	roots := map[string]bool{}
	if prefix := strings.TrimSpace(os.Getenv("HOMEBREW_PREFIX")); filepath.IsAbs(prefix) {
		roots[filepath.Clean(prefix)] = true
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		dir = filepath.Clean(strings.TrimSpace(dir))
		if !filepath.IsAbs(dir) {
			continue
		}
		switch {
		case strings.HasPrefix(dir, "/nix/store/"):
			roots["/nix/store"] = true
		case strings.HasPrefix(dir, "/opt/homebrew/"):
			roots["/opt/homebrew"] = true
		default:
			roots[dir] = true
		}
	}
	if _, err := os.Stat("/Library/Developer"); err == nil {
		roots["/Library/Developer"] = true
	}

	ordered := make([]string, 0, len(roots))
	for root := range roots {
		ordered = append(ordered, root)
	}
	sort.Strings(ordered)

	parts := []string{
		`":minimal"="read"`,
		`":workspace_roots"={"."="write"}`,
		`":tmpdir"="write"`,
		`":slash_tmp"="write"`,
		`"~/.gitconfig"="read"`,
		`"~/.config/git"="read"`,
		`"~/.cache/pre-commit"="write"`,
	}
	if repoRoot != "" {
		parts = append(parts,
			tomlString(filepath.Join(repoRoot, ".beads", "hooks"))+`="read"`,
			tomlString(filepath.Join(repoRoot, ".allowed_signers"))+`="read"`,
		)
	}
	for _, root := range ordered {
		parts = append(parts, tomlString(root)+`="read"`)
	}
	return "permissions.koryph_signing.filesystem={" + strings.Join(parts, ",") + "}"
}

// sandboxCacheEnv redirects mutable developer-tool state into the
// invocation-owned phase directory. It is active whenever the caller gives us
// a scratch directory: ordinary workspace-write launches need the same cache
// isolation as signing launches. pre-commit is the deliberate exception; its
// already-vetted hook environments are expensive and may require network
// access to rebuild, so only the signing profile receives its narrowly granted
// persistent cache.
func sandboxCacheEnv(sshAuthSock, scratchDir string) []string {
	if scratchDir == "" {
		return nil
	}
	env := []string{
		"GOCACHE=" + filepath.Join(scratchDir, "go-cache"),
		"GOMODCACHE=" + filepath.Join(scratchDir, "go-mod-cache"),
		// GOTELEMETRY is a computed, non-settable Go environment value as of
		// Go 1.26. TEST_TELEMETRY_DIR is the narrow Go tool override: unlike
		// HOME, it redirects only Go telemetry and leaves Codex's account
		// configuration untouched.
		"TEST_TELEMETRY_DIR=" + filepath.Join(scratchDir, "go-telemetry"),
		"XDG_CACHE_HOME=" + filepath.Join(scratchDir, "cache"),
		// The phase directory already exists before a runtime is launched;
		// using it directly avoids asking tools such as xcrun to create a
		// nested TMPDIR before their first cache operation.
		"TMPDIR=" + scratchDir,
	}
	if sshAuthSock != "" {
		env = append(env, "PRE_COMMIT_HOME="+filepath.Join(os.Getenv("HOME"), ".cache", "pre-commit"))
	}
	return env
}

func tomlString(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

var _ runtime.Runtime = Codex{}
var _ runtime.IdentityProber = Codex{}
var _ runtime.WorkflowProjector = Codex{}
