// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package account constructs the Claude environment for a dispatch and
// verifies the logged-in identity, fail-closed.
//
// Account model (see docs/ide-integration.md):
//   - personal == CLAUDE_CONFIG_DIR UNSET → default profile ($HOME/.claude.json
//   - suffix-less keychain entry). NEVER point personal at ~/.claude
//     explicitly (fresh hashed keychain entry → blank profile).
//   - work == CLAUDE_CONFIG_DIR set to a dedicated dir (convention
//     ~/.claude-work) with its own <dir>/.claude.json + hashed keychain entry.
//
// Identity verification reads <configDir||$HOME>/.claude.json →
// .oauthAccount.emailAddress and compares to the registry's ExpectedIdentity.
// Any read/parse failure or mismatch MUST refuse dispatch (fail closed).
//
// Billing modes:
//   - subscription (default): child env has ANTHROPIC_API_KEY scrubbed so the
//     claude CLI uses the account's OAuth subscription.
//   - api-key (explicit): child env carries ANTHROPIC_API_KEY resolved from
//     the registry's APIKeyEnvVar. Only valid when the record's APIFallback
//     is "explicit" and the caller passed an explicit allow flag.
//
// Implementation contract (account.go):
//   - Env(profile, billing, apiKey) []string — full child env, built from
//     execx.BaseEnv scrubbing CLAUDE_CONFIG_DIR + ANTHROPIC_API_KEY, then
//     re-injecting per the rules above.
//   - Verify(ctx, profile) (Identity, error) — read + parse .claude.json.
//   - VerifyExpected(ctx, profile, expected) error — Verify + compare.
package account

// BillingMode selects how a dispatched agent is paid for.
type BillingMode string

const (
	BillingSubscription BillingMode = "subscription"
	BillingAPIKey       BillingMode = "api-key"
)

// Profile is the resolved account context for a project.
type Profile struct {
	Name      string // registry.ProfilePersonal | registry.ProfileWork | custom
	ConfigDir string // "" = unset (personal default profile)
}

// Identity is what the Claude config reports as logged in.
type Identity struct {
	Email        string `json:"email"`
	Organization string `json:"organization,omitempty"`
	ConfigPath   string `json:"config_path"`
}
