// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/runtime"
)

// defaultBin is the claude binary name resolved when Claude.Bin is unset —
// the same default internal/dispatch.CLIBackend, internal/review.Opts, and
// internal/stage.Opts have each hardcoded independently until now (koryph-v8u.2).
const defaultBin = "claude"

// FallbackModel is the single value behind every Claude invocation's
// --fallback-model flag. Before koryph-v8u.2 the literal "sonnet" was
// duplicated at internal/dispatch/cli.go:130 and internal/stage/stage.go:72
// (flagged by the koryph-v8u.2 architecture review) — both now read this
// constant instead.
const FallbackModel = "sonnet"

// detectTimeout bounds the `claude --version` probe Detect runs; Detect must
// be cheap and local (see runtime.Runtime.Detect's doc), so a hung binary
// must not wedge a caller (e.g. `koryph doctor`) enumerating every registered
// runtime.
const detectTimeout = 10 * time.Second

// Claude implements runtime.Runtime for the claude CLI (koryph-v8u.2). The
// zero value is ready to use and resolves to the "claude" binary on PATH;
// set Bin to override it (mirrors CLIBackend.ClaudeBin / review.Opts.ClaudeBin
// / stage.Opts.ClaudeBin, which every existing call site already exposes for
// the KORYPH_CLAUDE_BIN override and tests' fake-claude scripts).
type Claude struct {
	// Bin is the path or name of the claude binary; "" resolves to
	// defaultBin.
	Bin string
}

// New returns a Runtime adapter for the claude CLI. bin overrides the binary
// path/name; "" resolves to defaultBin, matching every existing call site's
// default.
func New(bin string) runtime.Runtime {
	return Claude{Bin: bin}
}

// bin resolves the configured binary, defaulting to "claude".
func (c Claude) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return defaultBin
}

// Name implements runtime.Runtime.
func (c Claude) Name() string { return "claude" }

// Provider implements runtime.Runtime. The literal below is deliberately NOT
// an import of govern.DefaultPool — see runtime.Runtime.Provider's doc for
// why this package stays import-free of internal/govern. Keep this value in
// lockstep with govern.DefaultPool by hand; a mismatch would silently split
// Claude dispatch across two governor pools.
const claudeProvider = "anthropic" // must equal govern.DefaultPool

// Provider implements runtime.Runtime.
func (c Claude) Provider() string { return claudeProvider }

// Detect implements runtime.Runtime: a PATH lookup plus a bounded
// `--version` probe. It never burns API quota or touches the network.
func (c Claude) Detect(ctx context.Context) (present bool, version string) {
	bin := c.bin()
	if !execx.LookPath(bin) {
		return false, ""
	}
	res, err := execx.Run(ctx, execx.Cmd{Name: bin, Args: []string{"--version"}, Timeout: detectTimeout})
	if err != nil || res.ExitCode != 0 {
		return true, ""
	}
	return true, strings.TrimSpace(res.Stdout)
}

// AuthCheck implements runtime.Runtime by reading the profile's .claude.json
// exactly as account.Verify does today — the same fail-closed,
// no-quota-spent check internal/account already performs, just reachable
// through the Runtime seam. It intentionally does NOT compare against an
// expected identity (unlike VerifyIdentity/account.VerifyExpected):
// Runtime.AuthCheck's signature carries only a Profile, not an
// expected-identity string, so dispatch-time identity enforcement uses
// VerifyIdentity (koryph-v8u.5; internal/dispatch, internal/stage, and
// internal/onboard all call it) — AuthCheck stays the cheaper,
// registry-facing "is anyone logged in here" probe (koryph doctor), not a
// drop-in replacement for the fail-closed dispatch gate.
func (c Claude) AuthCheck(ctx context.Context, profile runtime.Profile) error {
	_, err := account.Verify(ctx, toAccountProfile(profile))
	return err
}

// VerifyIdentity implements runtime.Runtime (koryph-v8u.5) by delegating,
// unchanged, to account.VerifyExpected — the SAME fail-closed identity-plus-
// billing gate internal/dispatch, internal/stage, internal/onboard, and the
// engine's run-level check used to call directly. Moving those call sites
// onto this seam is exactly what koryph-v8u.5 does; this method holds no new
// logic of its own, only the account.Profile/account.BillingMode conversion
// every other Claude method here already performs.
func (c Claude) VerifyIdentity(ctx context.Context, profile runtime.Profile, expected string) (string, error) {
	id, err := account.VerifyExpected(ctx, toAccountProfile(profile), expected)
	if err != nil {
		return "", err
	}
	return id.Email, nil
}

// CurrentIdentity is the optional enrollment counterpart of VerifyIdentity.
// It is used only by the runtime-neutral account setup command; ordinary
// dispatch still verifies against the value stored in the registry.
func (c Claude) CurrentIdentity(ctx context.Context, profile runtime.Profile) (string, error) {
	id, err := account.Verify(ctx, toAccountProfile(profile))
	if err != nil {
		return "", err
	}
	return id.Email, nil
}

// Capabilities implements runtime.Runtime. Every flag Claude's CLI actually
// supports today is true. Sandbox is true because signing-enabled dispatches
// use Claude Code's native OS sandbox with an exact Unix-socket allowlist on
// macOS; hosts that cannot enforce that exact boundary fail closed before
// launch. UsageSource is true: Claude has a real, fail-closed
// usage-measurement source (ccusage / local *.jsonl transcripts, see
// internal/quota), so the governor's existing warn/drain/stop enforcement
// stays fully in force for this runtime (koryph-v8u.5) — the
// advisory-when-no-usage-source path this flag gates is for a FUTURE runtime
// without one, never for claude.
func (c Claude) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		JSONStream:          true,
		Personas:            true,
		Hooks:               true,
		Resume:              true,
		EffortFlag:          true,
		BudgetFlag:          true,
		Sandbox:             true,
		ScopedSigningSocket: hostGOOS == "darwin",
		ModelSelect:         true,
		UsageSource:         true,
	}
}

// InstructionFile implements runtime.Runtime.
func (c Claude) InstructionFile() string { return "CLAUDE.md" }

// AccountEnv implements runtime.Runtime: the CLAUDE_CONFIG_DIR subset of
// account.ChildEnv's env-construction rules (personal/default profile ==
// ConfigDir "" stays unset — never point it at ~/.claude explicitly).
func (c Claude) AccountEnv(profile runtime.Profile) []string {
	if profile.ConfigDir == "" {
		return nil
	}
	return []string{"CLAUDE_CONFIG_DIR=" + profile.ConfigDir}
}

// ModelMap implements runtime.Runtime.
func (c Claude) ModelMap() runtime.ModelMap { return runtime.ClaudeModelMap }

func (c Claude) EffortMap() runtime.EffortMap { return runtime.ClaudeEffortMap }

var _ runtime.IdentityProber = Claude{}

// Command implements runtime.Runtime: it reproduces, byte-for-byte,
// internal/dispatch/cli.go's launch.sh argv and env before koryph-v8u.2 (see
// buildArgs). argv[0] is the resolved claude binary; the caller (today,
// internal/dispatch.CLIBackend.Dispatch) is responsible for any shell-
// quoting needed to embed argv into an inspectable script — Command itself
// returns plain, unescaped argv/env, matching the interface doc's "for the
// caller to exec (or embed in an inspectable launch.sh)".
//
// env is the COMPLETE child environment (via account.ChildEnv), not a
// fragment to be appended to the process's own env — account.ChildEnv
// already builds a fully-allowlisted child env from scratch specifically so
// an untrusted dispatched agent cannot inherit an operator secret by
// omission (see account.ChildEnv's doc). Callers must assign it directly to
// their child process's Env, exactly as CLIBackend.Dispatch did before this
// extraction.
//
// Every field DispatchSpec gates behind a Capabilities flag (Persona, Model,
// Effort, MaxBudgetUSD, ResumeSessionID) is unconditionally supported by
// Claude's Capabilities() above, so no gated-field error path is reachable
// here today; the checks are omitted rather than written as dead code.
func (c Claude) Command(spec runtime.DispatchSpec) (argv []string, env []string, err error) {
	sandboxArgs, sandboxEnv, err := signingSandboxPolicy(spec.SSHAuthSock, spec.RepoRoot, spec.PhaseDir)
	if err != nil {
		return nil, nil, err
	}
	argv = append([]string{c.bin()}, sandboxArgs...)
	argv = append(argv, buildArgs(spec)...)
	env = account.ChildEnv(account.ChildEnvSpec{
		Profile:          toAccountProfile(spec.Profile),
		Billing:          toAccountBilling(spec.Billing),
		APIKey:           spec.APIKey,
		SSHAuthSock:      spec.SSHAuthSock,
		Passthrough:      spec.EnvPassthrough,
		ProxyBaseURL:     spec.ProxyBaseURL,
		Credential:       spec.Credential,
		CredentialEnvVar: spec.CredentialEnvVar,
	})
	env = append(env, sandboxEnv...)
	return argv, env, nil
}

// CommandJSON translates a one-shot JSONSpec into claude's non-streaming
// `--output-format json` argv+env (koryph-fiv finding #1) — the direct port of
// the argv the review/stage/epicreview packages each hand-built inline before
// this seam. env is built through the same account.ChildEnv contract as
// Command.
func (c Claude) CommandJSON(spec runtime.JSONSpec) (argv []string, env []string, err error) {
	sandboxArgs, sandboxEnv, err := signingSandboxPolicy(spec.SSHAuthSock, spec.RepoRoot, spec.ScratchDir)
	if err != nil {
		return nil, nil, err
	}
	argv = append([]string{c.bin()}, sandboxArgs...)
	argv = append(argv, buildJSONArgs(spec)...)
	env = account.ChildEnv(account.ChildEnvSpec{
		Profile:          toAccountProfile(spec.Profile),
		Billing:          toAccountBilling(spec.Billing),
		APIKey:           spec.APIKey,
		SSHAuthSock:      spec.SSHAuthSock,
		Passthrough:      spec.EnvPassthrough,
		ProxyBaseURL:     spec.ProxyBaseURL,
		Credential:       spec.Credential,
		CredentialEnvVar: spec.CredentialEnvVar,
		SpawnKind:        spec.SpawnKind,
	})
	env = append(env, sandboxEnv...)
	return argv, env, nil
}

var hostGOOS = goruntime.GOOS

// signingSandboxPolicy returns invocation-local Claude Code settings for a
// signing-enabled spawn. The private key and its storage reference never enter
// this adapter: the only signing capability supplied to Claude is the path of
// the one-key koryph ssh-agent socket. Claude may run git/ssh commands, which
// ask that agent to sign; it cannot read or export private key material.
//
// Claude Code can path-filter Unix sockets only on macOS. Linux/WSL exposes an
// all-or-nothing allowAllUnixSockets switch, which would also make an
// operator's ambient agents and service sockets reachable if their paths were
// discovered. Refuse that weaker posture rather than silently broadening the
// signing capability. Non-signing Claude dispatches are unchanged.
func signingSandboxPolicy(socket, repoRoot, scratchDir string) ([]string, []string, error) {
	if socket == "" {
		return nil, nil, nil
	}
	if hostGOOS != "darwin" {
		return nil, nil, fmt.Errorf("claude: secure SSH signing requires exact Unix-socket sandboxing, unavailable on %s; no private key or broad socket access was given to Claude", hostGOOS)
	}
	if !filepath.IsAbs(socket) {
		return nil, nil, fmt.Errorf("claude: scoped signing socket must be an absolute path")
	}

	gitDir := ""
	if repoRoot != "" {
		gitDir = filepath.Join(repoRoot, ".git")
	}
	allowWrite := compactPaths(scratchDir, gitDir, preCommitHome())
	sensitiveReadPaths := []string{
		"~/.ssh",
		"~/.aws",
		"~/.azure",
		"~/.config/gcloud",
		"~/.config/gh",
		"~/.docker",
		"~/.kube",
	}
	settings := map[string]any{
		"permissions": map[string]any{
			"deny": readDenyRules(sensitiveReadPaths),
		},
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,
			"autoAllowBashIfSandboxed": true,
			"allowUnsandboxedCommands": false,
			"filesystem": map[string]any{
				"allowWrite": allowWrite,
				"denyRead":   sensitiveReadPaths,
			},
			"network": map[string]any{
				"allowAllUnixSockets": false,
				"allowUnixSockets":    []string{socket},
			},
		},
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, fmt.Errorf("claude: encode signing sandbox settings: %w", err)
	}

	env := []string{}
	if scratchDir != "" {
		cacheRoot := filepath.Join(scratchDir, ".koryph-cache")
		env = append(env,
			"GOCACHE="+filepath.Join(cacheRoot, "go-build"),
			"XDG_CACHE_HOME="+cacheRoot,
		)
	}
	if home := preCommitHome(); home != "" {
		env = append(env, "PRE_COMMIT_HOME="+home)
	}
	// Project settings retain koryph's deterministic hooks and permissions.
	// Excluding user/local sources prevents an untrusted per-user array entry
	// from widening allowUnixSockets; managed policy still applies above this
	// invocation and may only be administered outside the project.
	return []string{"--setting-sources", "project", "--settings", string(raw)}, env, nil
}

func compactPaths(paths ...string) []string {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || path == "." || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

func preCommitHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "pre-commit")
}

func readDenyRules(paths []string) []string {
	rules := make([]string, 0, len(paths))
	for _, path := range paths {
		rules = append(rules, "Read("+path+"/**)")
	}
	return rules
}

// buildJSONArgs constructs the claude CLI flag sequence for a one-shot JSON
// spawn. PermissionMode is caller-selected ("plan" for the read-only
// reviewers, "dontAsk" for a mutating stage agent); the fallback-model and
// max-budget flags are opt-in so each site's pre-seam argv is preserved
// exactly (the reviewers omitted both).
func buildJSONArgs(spec runtime.JSONSpec) []string {
	mode := spec.PermissionMode
	if mode == "" {
		mode = "plan"
	}
	args := []string{
		"-p",
		"--agent", spec.Persona,
		"--permission-mode", mode,
		"--model", spec.Model,
	}
	if spec.Effort != "" {
		args = append(args, "--effort", spec.Effort)
	}
	if spec.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(spec.MaxBudgetUSD, 'f', -1, 64))
	}
	if spec.Fallback {
		args = append(args, "--fallback-model", FallbackModel)
	}
	args = append(args, "--output-format", "json")
	return args
}

// buildArgs constructs the claude CLI flag sequence (everything after
// argv[0]) for a dispatch-shaped invocation. This is a direct port of
// internal/dispatch/cli.go's pre-koryph-v8u.2 `args` construction — same
// flags, same order, same gating — with the shell single-quoting (sq(), a
// launch.sh-embedding concern) stripped out, since Command's argv is meant
// to be exec-ready plain values.
func buildArgs(spec runtime.DispatchSpec) []string {
	args := []string{
		"-p",
		"--agent", spec.Persona,
		"--session-id", spec.SessionID,
		"--permission-mode", "dontAsk",
		"--model", spec.Model,
	}
	if spec.Effort != "" {
		args = append(args, "--effort", spec.Effort)
	}
	if spec.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(spec.MaxBudgetUSD, 'f', -1, 64))
	}
	args = append(args, "--fallback-model", FallbackModel)
	if spec.SessionName != "" {
		args = append(args, "--name", spec.SessionName)
	}
	if spec.ResumeSessionID != "" {
		args = append(args, "--resume", spec.ResumeSessionID, "--fork-session")
	}
	if spec.StrictMCP {
		// koryph-kwv: agents use only file+bash tools, so loading the
		// machine's ambient MCP servers just bloats the re-read-every-turn
		// prompt prefix. --strict-mcp-config drops all of them.
		args = append(args, "--strict-mcp-config")
	}
	args = append(args,
		"--add-dir", spec.PhaseDir,
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
	)
	return args
}

// toAccountProfile mirrors runtime.Profile -> account.Profile field-for-field
// (see runtime.Profile's doc).
func toAccountProfile(p runtime.Profile) account.Profile {
	return account.Profile{Name: p.Name, ConfigDir: p.ConfigDir}
}

// toAccountBilling mirrors runtime.BillingMode -> account.BillingMode (both
// are named string types with identical values; see runtime.BillingMode's
// doc).
func toAccountBilling(b runtime.BillingMode) account.BillingMode {
	return account.BillingMode(b)
}

// var assertion: Claude must satisfy runtime.Runtime.
var _ runtime.Runtime = Claude{}
