// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
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
	// Codex normally runs tool commands through the user's login shell. Login
	// startup files can replace the adapter-injected PATH and bypass the
	// phase command shims even for a cooperative agent. Force the documented
	// non-login mode so tool subprocesses retain Koryph's guarded PATH.
	args := []string{
		"--ask-for-approval", "never", "exec", "--json",
		"-c", "allow_login_shell=false",
	}
	args = append(args, sandboxArgs(spec.SSHAuthSock, spec.RepoRoot, spec.PhaseDir)...)
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
	env = append(env, dispatchCacheEnv(spec.SSHAuthSock, spec.RepoRoot, spec.PhaseDir)...)
	env, err := installPhaseCommandGuard(spec.PhaseDir, env)
	if err != nil {
		return nil, nil, fmt.Errorf("codex: install phase command guard: %w", err)
	}
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
	args = append(args, sandboxArgs(spec.SSHAuthSock, spec.RepoRoot, spec.ScratchDir)...)
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
//   - allowlists exactly the koryph scoped signing socket; and
//   - allowlists a fixed, phase-local pool for signing integration tests.
//
// --ignore-user-config prevents a user's legacy sandbox_mode setting from
// silently overriding default_permissions; Codex authentication still uses
// CODEX_HOME, per the CLI contract. The operator's ambient SSH agent is never
// passed into this function or the child environment.
func sandboxArgs(sshAuthSock, repoRoot, testSocketDir string) []string {
	if sshAuthSock == "" {
		return []string{"--sandbox", "workspace-write"}
	}
	sockets := append([]string{sshAuthSock}, testAgentSockets(testSocketDir)...)
	return []string{
		"--ignore-user-config",
		"-c", `default_permissions="koryph_signing"`,
		"-c", signingFilesystemRule(repoRoot),
		"-c", "permissions.koryph_signing.network.enabled=true",
		"-c", unixSocketRule(sockets...),
	}
}

const (
	testAgentSocketsEnv  = "KORYPH_TEST_SSH_AGENT_SOCKS"
	testAgentSocketCount = 8
)

// testAgentSockets returns the only sockets that signing integration tests may
// bind during this dispatch. Their root is deterministically keyed by the
// phase directory but remains short even when TMPDIR is phase-local and deeply
// nested. A pool keeps independent Go test packages from sharing an agent when
// `go test ./...` runs them in parallel.
func testAgentSockets(phaseDir string) []string {
	if phaseDir == "" {
		return nil
	}
	dir := paths.SocketDir("test-ssh-agent:" + phaseDir)
	sockets := make([]string, testAgentSocketCount)
	for i := range sockets {
		sockets[i] = filepath.Join(dir, fmt.Sprintf("s%x", i))
	}
	return sockets
}

// unixSocketRule renders an exact Codex Unix-socket allowlist. Unlike domain
// rules, Unix sockets do not support patterns, so every permitted test socket
// is named explicitly and the production signing socket remains a distinct,
// exact entry.
func unixSocketRule(sockets ...string) string {
	parts := make([]string, 0, len(sockets))
	for _, socket := range sockets {
		if socket != "" {
			parts = append(parts, tomlString(socket)+`="allow"`)
		}
	}
	return "permissions.koryph_signing.network.unix_sockets={" + strings.Join(parts, ",") + "}"
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
	for _, name := range runtime.CertificateEnvNames() {
		path := filepath.Clean(strings.TrimSpace(os.Getenv(name)))
		if !filepath.IsAbs(path) {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			// Grant only the exact configured file/directory. The variable is
			// forwarded by runtime.ChildEnv; this filesystem grant makes it
			// usable under the :minimal signing profile without opening a
			// broader certificate/config tree.
			roots[path] = true
		}
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

// dispatchCacheEnv keeps mutable developer-tool state phase-local while
// allowing ordinary implementation phases in one repository to reuse Go's
// content-addressed module downloads. The primary repository's Git metadata
// is already an explicit writable sandbox root for commits, so its
// koryph-owned cache does not widen the sandbox boundary. JSON/scoring spawns
// deliberately use sandboxCacheEnv instead and therefore never share it.
func dispatchCacheEnv(sshAuthSock, repoRoot, phaseDir string) []string {
	moduleCache := ""
	if repoRoot != "" {
		moduleCache = filepath.Join(repoRoot, ".git", "koryph-cache", "go-mod-cache")
	}
	return sandboxCacheEnvWithModuleCache(sshAuthSock, phaseDir, moduleCache)
}

// sandboxCacheEnv redirects mutable developer-tool state into the
// invocation-owned scratch directory. It is active whenever the caller gives
// us a scratch directory: ordinary workspace-write launches need the same
// cache isolation as signing launches. pre-commit is the deliberate exception;
// its already-vetted hook environments are expensive and may require network
// access to rebuild, so only the signing profile receives its narrowly granted
// persistent cache.
func sandboxCacheEnv(sshAuthSock, scratchDir string) []string {
	return sandboxCacheEnvWithModuleCache(sshAuthSock, scratchDir, "")
}

func sandboxCacheEnvWithModuleCache(sshAuthSock, scratchDir, moduleCache string) []string {
	if scratchDir == "" {
		return nil
	}
	if moduleCache == "" {
		moduleCache = filepath.Join(scratchDir, "go-mod-cache")
	}
	env := []string{
		"GOCACHE=" + filepath.Join(scratchDir, "go-cache"),
		"GOMODCACHE=" + moduleCache,
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
		if sockets := testAgentSockets(scratchDir); len(sockets) > 0 {
			env = append(env, testAgentSocketsEnv+"="+strings.Join(sockets, string(filepath.ListSeparator)))
		}
	}
	return env
}

func tomlString(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

var commandGuardExecutable = os.Executable

// installPhaseCommandGuard installs tiny forwarding shims for Codex releases
// without usable pre-tool hooks. Policy stays in `koryph command exec`; the
// shell contains no role, lock, evidence path, or classification logic.
//
// A hookless runtime can still bypass PATH by invoking an absolute tool path,
// relocating/overwriting the shim tree, or changing the mutable phase
// directory/identity environment. Worktree isolation and merge-time guards
// remain the security boundary for that case; the release canary must tripwire
// unexpected unguarded broad work.
func installPhaseCommandGuard(phaseDir string, env []string) ([]string, error) {
	env = removeEnv(env, "KORYPH_COMMAND_ROLE", "KORYPH_COMMAND_EVENTS", "KORYPH_COMMAND_GUARD_DIR")
	if phaseDir == "" {
		return env, nil
	}
	if !filepath.IsAbs(phaseDir) {
		return nil, errors.New("phase path must be absolute")
	}
	info, err := os.Lstat(phaseDir)
	if os.IsNotExist(err) {
		return env, nil
	}
	if err != nil {
		return nil, err
	}
	cleanPhase := filepath.Clean(phaseDir)
	resolvedPhase, err := filepath.EvalSymlinks(cleanPhase)
	if err != nil {
		return nil, err
	}
	if resolvedPhase != cleanPhase || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("phase path is not a real directory: %s", phaseDir)
	}

	shimDir := filepath.Join(phaseDir, "command-guard-bin")
	if err := os.Mkdir(shimDir, 0o700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	shimInfo, err := os.Lstat(shimDir)
	if err != nil || !shimInfo.IsDir() || shimInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("command shim path is not a real directory")
	}
	koryphBin, err := commandGuardExecutable()
	if err != nil {
		return nil, fmt.Errorf("resolve koryph command guard binary: %w", err)
	}
	koryphBin, err = canonicalExecutable(koryphBin)
	if err != nil {
		return nil, fmt.Errorf("canonicalize koryph command guard binary: %w", err)
	}
	pathValue := envValue(env, "PATH")
	if pathValue == "" {
		pathValue = os.Getenv("PATH")
	}
	for _, name := range []string{"make", "go", "golangci-lint"} {
		realPath, err := lookPathIn(name, pathValue)
		if err != nil {
			continue // absent optional tool; the runtime would fail normally.
		}
		realPath, err = canonicalExecutable(realPath)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s: %w", name, err)
		}
		shimPath := filepath.Join(shimDir, name)
		if existing, err := os.Lstat(shimPath); err == nil &&
			(existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular()) {
			return nil, fmt.Errorf("command shim %s is a symlink or non-regular file", name)
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		script := renderCommandGuardShim(koryphBin, realPath)
		if err := fsx.WriteAtomic(shimPath, []byte(script), 0o755); err != nil {
			return nil, err
		}
	}
	pathValue = envValue(env, "PATH")
	if pathValue == "" {
		pathValue = os.Getenv("PATH")
	}
	return setEnv(env, "PATH", shimDir+string(filepath.ListSeparator)+pathValue), nil
}

func envValue(env []string, name string) string {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env)+1)
	for _, pair := range env {
		if !strings.HasPrefix(pair, prefix) {
			out = append(out, pair)
		}
	}
	return append(out, prefix+value)
}

func removeEnv(env []string, names ...string) []string {
	denied := make(map[string]struct{}, len(names))
	for _, name := range names {
		denied[name] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, pair := range env {
		name, _, _ := strings.Cut(pair, "=")
		if _, drop := denied[name]; !drop {
			out = append(out, pair)
		}
	}
	return out
}

func lookPathIn(name, pathValue string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		return exec.LookPath(name)
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func canonicalExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = abs
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", errors.New("not an executable regular file")
	}
	return resolved, nil
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func renderCommandGuardShim(koryphBin, realPath string) string {
	return "#!/bin/sh\nexec " + shellSingleQuote(koryphBin) +
		" command exec --real " + shellSingleQuote(realPath) + " -- \"$@\"\n"
}

var _ runtime.Runtime = Codex{}
var _ runtime.IdentityProber = Codex{}
var _ runtime.WorkflowProjector = Codex{}
