// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package account

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// Valid reports whether m is a known billing mode.
func (m BillingMode) Valid() bool {
	return m == BillingSubscription || m == BillingAPIKey
}

// baseAllow is the credential-free set of environment variables a dispatched
// (untrusted, --permission-mode dontAsk) agent inherits from the operator. It
// is an ALLOWLIST: anything not named here or matched by baseAllowPrefixes is
// dropped, so a secret the operator has in their shell (GH_TOKEN, VAULT_TOKEN,
// AWS_*, the ambient SSH_AUTH_SOCK, ...) cannot reach the agent by omission.
// Identity, billing, and the scoped signing socket are injected explicitly by
// ChildEnv — never sourced from the ambient environment.
var baseAllow = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "COLORTERM",
	"TMPDIR", "TZ", "LANG",
	// Go toolchain (non-secret build caches/config; HOME-relative defaults work
	// when absent, but forwarding them keeps agent builds warm).
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTOOLCHAIN", "GOPROXY",
	// Homebrew prefix — macOS PATH/lib resolution for tools agents invoke.
	"HOMEBREW_PREFIX", "HOMEBREW_CELLAR", "HOMEBREW_REPOSITORY",
}

// baseAllowPrefixes forwards whole non-secret namespaces: locale (LC_*), koryph
// contract vars (KORYPH_*, e.g. KORYPH_HOME for the guard-hook path), and XDG
// base-directory hints (XDG_*). None of these carry credentials.
var baseAllowPrefixes = []string{"LC_", "KORYPH_", "XDG_"}

// Output-cap defaults (koryph-77r.6, design: docs/designs/
// 2026-07-token-economy.md §3 L3). Claude Code exposes two tool-output size
// knobs (verified against https://code.claude.com/docs/en/env-vars on
// 2026-07-07):
//
//   - BASH_MAX_OUTPUT_LENGTH: max characters in a Bash tool_result before
//     Claude Code itself spills the full output to a file and hands the
//     agent a path plus a short preview — Claude Code's own native,
//     built-in CCR. No default is documented (unset behaves as effectively
//     unbounded).
//   - MAX_MCP_OUTPUT_TOKENS: max tokens in an MCP tool_result before
//     truncation. Claude Code's own default is the model's context window
//     minus 10000 tokens reserved for the response — i.e. effectively
//     unbounded in practice.
//
// Both are injected as typed ChildEnvSpec fields below (never
// env_passthrough) so every one of the four dispatch sites (main dispatch
// via internal/runtime/claude, internal/review, internal/stage,
// internal/epicreview — every one already funnels through ChildEnv) gets
// them uniformly from this single point (I6: allowlist discipline stays
// single-point; no per-site plumbing needed). The defaults are deliberately
// conservative: high enough that ordinary command output — including
// make gate-agent's own PASS/FAIL summary and tail-40 failure excerpt,
// which run to a few KB — is never touched, low enough to bound a
// genuinely pathological unbounded dump (e.g. an agent bypassing the
// verbose-command guard and cat-ing a multi-GB file).
const (
	// DefaultBashMaxOutputLength is BASH_MAX_OUTPUT_LENGTH's koryph default,
	// in characters (~400 KB).
	DefaultBashMaxOutputLength = 400_000
	// DefaultMaxMCPOutputTokens is MAX_MCP_OUTPUT_TOKENS's koryph default, in
	// tokens.
	DefaultMaxMCPOutputTokens = 50_000
)

// ChildEnvSpec parameterizes the dispatched-agent environment.
type ChildEnvSpec struct {
	Profile     Profile
	Billing     BillingMode
	APIKey      string   // injected as ANTHROPIC_API_KEY iff Billing==api-key
	SSHAuthSock string   // scoped signing socket; injected as SSH_AUTH_SOCK iff non-empty
	Passthrough []string // extra operator vars to forward (registry-declared escape hatch)

	// BashMaxOutputLength overrides BASH_MAX_OUTPUT_LENGTH (characters).
	// Zero (the common case — no caller sets this today) uses
	// DefaultBashMaxOutputLength; a negative value omits the env var
	// entirely, falling back to Claude Code's own unbounded default — an
	// explicit opt-out escape hatch, not expected to be used in practice.
	BashMaxOutputLength int
	// MaxMCPOutputTokens overrides MAX_MCP_OUTPUT_TOKENS (tokens). Zero uses
	// DefaultMaxMCPOutputTokens; negative omits the env var entirely.
	MaxMCPOutputTokens int

	// ProxyBaseURL is the project's registry-configured agent_proxy.base_url
	// (koryph-3l1.1, design docs/designs/2026-07-token-economy.md §3 L5, §2
	// I4/I6). Non-empty injects ANTHROPIC_BASE_URL=<value>; empty (the
	// default — no agent_proxy configured, or direct dispatch) leaves the
	// var ABSENT, exactly as today (it is already scrubbed by the
	// allowlist, so this is a genuine zero-residue default — see the I6
	// test asserting a default spec's env is byte-identical to pre-koryph-
	// 3l1.1 output). This is the single sanctioned source for
	// ANTHROPIC_BASE_URL; never set it via Passthrough/env_passthrough.
	ProxyBaseURL string

	// SpawnKind marks which of the four spawn sites is building this env:
	// "" for main dispatch, "review"/"stage"/"epicreview" for the three
	// secondary sites (koryph-3l1.1). Non-empty injects
	// KORYPH_SPAWN_KIND=<value>; empty leaves the var ABSENT. Consumed by a
	// parallel bead's SessionStart wrapper (koryph-77r.4) to slim
	// per-session context injection — this field only stamps the marker.
	SpawnKind string
}

// ChildEnv builds the complete child environment for a dispatched agent from an
// ALLOWLIST (baseAllow + baseAllowPrefixes + spec.Passthrough), then injects the
// account-scoped values explicitly:
//
//   - Profile.ConfigDir != "" → CLAUDE_CONFIG_DIR=<dir> (work / custom profile).
//     Personal (ConfigDir == "") stays UNSET — never point it at ~/.claude.
//   - Billing == BillingAPIKey → ANTHROPIC_API_KEY=<apiKey> (caller validates
//     non-empty; Dispatch does).
//   - SSHAuthSock != "" → SSH_AUTH_SOCK=<sock>. This is the koryph-managed
//     signing socket (paths.SigningAgentSock), which holds ONLY the signing key.
//     The operator's ambient SSH_AUTH_SOCK is never forwarded — it typically
//     carries their personal/prod keys, which an untrusted agent must not reach.
//   - ProxyBaseURL != "" → ANTHROPIC_BASE_URL=<url> (koryph-3l1.1). Empty
//     (the default) leaves it unset — a default spec's env is byte-identical
//     to a spec built before this field existed.
//   - SpawnKind != "" → KORYPH_SPAWN_KIND=<kind> (koryph-3l1.1). Empty (main
//     dispatch) leaves it unset.
func ChildEnv(spec ChildEnvSpec) []string {
	allow := baseAllow
	if len(spec.Passthrough) > 0 {
		allow = append(append([]string{}, baseAllow...), spec.Passthrough...)
	}
	env := execx.AllowEnv(allow, baseAllowPrefixes)
	if spec.Profile.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+spec.Profile.ConfigDir)
	}
	if spec.Billing == BillingAPIKey {
		env = append(env, "ANTHROPIC_API_KEY="+spec.APIKey)
	}
	if spec.SSHAuthSock != "" {
		env = append(env, "SSH_AUTH_SOCK="+spec.SSHAuthSock)
	}
	if spec.ProxyBaseURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+spec.ProxyBaseURL)
	}
	if spec.SpawnKind != "" {
		env = append(env, "KORYPH_SPAWN_KIND="+spec.SpawnKind)
	}
	env = append(env, outputCapEnv(spec)...)
	return env
}

// outputCapEnv resolves BASH_MAX_OUTPUT_LENGTH and MAX_MCP_OUTPUT_TOKENS per
// spec, applying the koryph defaults when unset (zero) and omitting the var
// entirely when the field is explicitly negative. See the Default* consts'
// doc for the values and rationale.
func outputCapEnv(spec ChildEnvSpec) []string {
	var env []string
	bashCap := spec.BashMaxOutputLength
	if bashCap == 0 {
		bashCap = DefaultBashMaxOutputLength
	}
	if bashCap > 0 {
		env = append(env, fmt.Sprintf("BASH_MAX_OUTPUT_LENGTH=%d", bashCap))
	}
	mcpCap := spec.MaxMCPOutputTokens
	if mcpCap == 0 {
		mcpCap = DefaultMaxMCPOutputTokens
	}
	if mcpCap > 0 {
		env = append(env, fmt.Sprintf("MAX_MCP_OUTPUT_TOKENS=%d", mcpCap))
	}
	return env
}

// ConfigJSONPath resolves the .claude.json path for a profile:
// $HOME/.claude.json for the personal default profile (ConfigDir == ""),
// otherwise <ConfigDir>/.claude.json.
func ConfigJSONPath(p Profile) string {
	if p.ConfigDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.Getenv("HOME")
		}
		return filepath.Join(home, ".claude.json")
	}
	return filepath.Join(p.ConfigDir, ".claude.json")
}

// claudeConfig is the subset of .claude.json we verify against.
type claudeConfig struct {
	OAuthAccount struct {
		EmailAddress     string `json:"emailAddress"`
		OrganizationName string `json:"organizationName"`
	} `json:"oauthAccount"`
}

// Verify reads and parses the profile's .claude.json and returns the
// logged-in identity. A missing file, unparseable JSON, or an empty
// emailAddress is an error — verification fails closed.
func Verify(ctx context.Context, p Profile) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	path := ConfigJSONPath(p)
	data, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("account verify (profile %q): reading %s: %w — refusing dispatch", p.Name, path, err)
	}
	var cfg claudeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Identity{}, fmt.Errorf("account verify (profile %q): parsing %s: %w — refusing dispatch", p.Name, path, err)
	}
	if cfg.OAuthAccount.EmailAddress == "" {
		return Identity{}, fmt.Errorf("account verify (profile %q): %s has no oauthAccount.emailAddress (not logged in?) — refusing dispatch", p.Name, path)
	}
	return Identity{
		Email:        cfg.OAuthAccount.EmailAddress,
		Organization: cfg.OAuthAccount.OrganizationName,
		ConfigPath:   path,
	}, nil
}

// VerifyExpected verifies the profile identity and compares the logged-in
// email to the registry's expected identity, case-insensitively. Any error
// or mismatch refuses dispatch (fail closed).
func VerifyExpected(ctx context.Context, p Profile, expected string) (Identity, error) {
	id, err := Verify(ctx, p)
	if err != nil {
		return Identity{}, err
	}
	if expected == "" {
		return Identity{}, fmt.Errorf("account verify (profile %q): registry expected identity is empty (config: %s) — refusing dispatch", p.Name, id.ConfigPath)
	}
	if !strings.EqualFold(id.Email, expected) {
		return Identity{}, fmt.Errorf("account mismatch: logged in as %s, registry expects %s (config: %s) — refusing dispatch", id.Email, expected, id.ConfigPath)
	}
	return id, nil
}
