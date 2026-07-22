// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package authmode holds the auth-mode/credential data shared by
// internal/registry, internal/quota, and internal/account (koryph-i3b,
// design docs/designs/2026-07-api-key-auth.md §4-§6). It is deliberately
// dependency-free (no internal koryph imports) so all three can import it
// directly without creating a cycle: internal/registry imports internal/quota
// (registry/store.go), and internal/quota imports internal/account
// (quota/usage.go) — internal/account importing internal/registry directly
// would cycle back. Before this package existed, the AuthMode/CredentialSource
// constants and the Credential struct were hand-mirrored in all three
// packages to dodge that cycle; each of them now re-exports these via a local
// alias (e.g. registry.AuthModeSubscription = authmode.Subscription,
// type registry.Credential = authmode.Credential) so existing call sites are
// unaffected.
package authmode

// Auth modes. Subscription is the default and today's only behavior: OAuth
// login billed against a Claude subscription, identity verified via an
// expected email. APIKey and OAuthToken are long-lived-credential modes:
// identity is verified via a fingerprint instead of an email, and the
// credential itself is resolved via Credential.
const (
	Subscription = "subscription"
	APIKey       = "api-key"
	OAuthToken   = "oauth-token"
)

// Credential source kinds.
const (
	CredentialSourceVault = "vault"
	CredentialSourceEnv   = "env"
)

// Credential is a long-lived credential reference for APIKey / OAuthToken
// accounts (koryph-i3b, design §6) — never the secret itself, only where to
// find it. Resolution happens in internal/account.ResolveCredential; this
// type is pure data.
type Credential struct {
	// Source is CredentialSourceVault (fetch via signing.FetchSecret using
	// Provider+KeyRef) or CredentialSourceEnv (read the named EnvVar).
	Source string `json:"source"`
	// Provider is the vault provider name (any signing.VaultProviders
	// value) when Source == CredentialSourceVault; ignored otherwise.
	Provider string `json:"provider,omitempty"`
	// KeyRef is the vault item reference passed to FetchSecret when
	// Source == CredentialSourceVault; ignored otherwise.
	KeyRef string `json:"key_ref,omitempty"`
	// EnvVar names the environment variable holding the credential when
	// Source == CredentialSourceEnv; ignored otherwise. MUST NOT be
	// "ANTHROPIC_API_KEY" or "CLAUDE_CODE_OAUTH_TOKEN" — those are the
	// CANONICAL names ChildEnv injects the resolved value under, so reusing
	// one as the SOURCE var would let a dispatched agent's own ambient env
	// satisfy its own credential lookup, defeating the vault/named-var
	// indirection (mirrors the batch client's refusal,
	// internal/anthro/client.go:104-105). Machine-checked at load, not just
	// documented — see validateCredential in internal/registry/store.go.
	EnvVar string `json:"env_var,omitempty"`
}
