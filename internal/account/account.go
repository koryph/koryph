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

// ChildEnvSpec parameterizes the dispatched-agent environment.
type ChildEnvSpec struct {
	Profile     Profile
	Billing     BillingMode
	APIKey      string   // injected as ANTHROPIC_API_KEY iff Billing==api-key
	SSHAuthSock string   // scoped signing socket; injected as SSH_AUTH_SOCK iff non-empty
	Passthrough []string // extra operator vars to forward (registry-declared escape hatch)
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
