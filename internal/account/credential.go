// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/koryph/koryph/internal/authmode"
	"github.com/koryph/koryph/internal/signing"
)

// AuthMode selects how an account authenticates (koryph-i3b, design
// docs/designs/2026-07-api-key-auth.md §4-§5). Its values alias
// internal/authmode's constants byte-for-byte — the dependency-free leaf
// package shared with registry and quota, which lets account avoid
// importing registry directly: internal/registry already imports
// internal/quota, which imports internal/account, so account importing
// registry would be a cycle. Callers holding a *registry.Record pass
// rec.EffectiveAuthMode() (cast to AuthMode) through verbatim — see
// AuthSpec.
type AuthMode string

const (
	// AuthModeSubscription is the default: OAuth login billed against a
	// Claude subscription, identity verified via the .claude.json email
	// (VerifyExpected, unchanged).
	AuthModeSubscription AuthMode = authmode.Subscription
	// AuthModeAPIKey is a long-lived ANTHROPIC_API_KEY credential, billed
	// per token.
	AuthModeAPIKey AuthMode = authmode.APIKey
	// AuthModeOAuthToken is a long-lived CLAUDE_CODE_OAUTH_TOKEN credential
	// (`claude setup-token`), billed against the subscription like
	// AuthModeSubscription.
	AuthModeOAuthToken AuthMode = authmode.OAuthToken
)

// Credential source kinds — aliased from internal/authmode (see AuthMode's
// doc for why these are shared via that leaf package rather than imported
// from registry directly).
const (
	CredentialSourceVault = authmode.CredentialSourceVault
	CredentialSourceEnv   = authmode.CredentialSourceEnv
)

// Credential is account's view of a credential reference — aliased from
// internal/authmode so callers holding a *registry.Record's Credential
// (also an authmode.Credential alias) can pass it through directly with no
// conversion (see AuthMode's doc for why this package cannot import
// registry directly). See authmode.Credential for field docs.
type Credential = authmode.Credential

// fingerprintPrefixHexLen is how many hex characters (8 bytes / 64 bits) of
// the credential's sha256 digest are persisted/compared as the non-secret
// identity_fingerprint (design §5: "a truncated SHA-256 prefix is safe to
// persist and to show in koryph doctor").
const fingerprintPrefixHexLen = 16

// Fingerprint returns the non-secret "sha256:<hex prefix>" fingerprint of a
// resolved credential — the value recorded as Record.IdentityFingerprint at
// enrollment and re-derived here at every dispatch for comparison (§5). The
// prefix is intentionally short: it is an identity/swap-detection signal,
// never a lookup key, so no more of the digest needs to be retained than is
// needed to make an accidental collision between two different real-world
// credentials practically impossible.
func Fingerprint(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return "sha256:" + hex.EncodeToString(sum[:])[:fingerprintPrefixHexLen]
}

// canonicalCredentialEnvVar are the injected-credential names ChildEnv
// writes into a dispatched agent's environment — never valid as a
// Credential.EnvVar SOURCE (design §6; mirrors internal/anthro.NewClient's
// ambient-key refusal, client.go:104-105).
var canonicalCredentialEnvVar = map[string]bool{
	"ANTHROPIC_API_KEY":       true,
	"CLAUDE_CODE_OAUTH_TOKEN": true,
}

// ResolveCredential resolves the long-lived credential for an
// AuthModeAPIKey/AuthModeOAuthToken account (koryph-i3b, design §6): a vault
// fetch (signing.FetchSecret, provider+key_ref) or a purpose-named
// environment variable — NEVER the canonical ambient name the resolved value
// is later injected under (I2/I4). Returns the CANONICAL env var name
// ChildEnv must inject the resolved value under — "ANTHROPIC_API_KEY" for
// AuthModeAPIKey, "CLAUDE_CODE_OAUTH_TOKEN" for AuthModeOAuthToken — plus the
// resolved value itself. The value is never logged by this function or by
// signing.FetchSecret (which logs only provider + key_ref, never the
// secret).
//
// mode must be AuthModeAPIKey or AuthModeOAuthToken; AuthModeSubscription
// (or any unrecognized mode) has no credential to resolve and is an error —
// callers branch on auth mode before calling this (see VerifyAuth).
func ResolveCredential(ctx context.Context, mode AuthMode, cred *Credential) (envVar string, value string, err error) {
	switch mode {
	case AuthModeAPIKey:
		envVar = "ANTHROPIC_API_KEY"
	case AuthModeOAuthToken:
		envVar = "CLAUDE_CODE_OAUTH_TOKEN"
	default:
		return "", "", fmt.Errorf("account: ResolveCredential: auth_mode %q has no credential to resolve", mode)
	}

	if cred == nil {
		return "", "", fmt.Errorf("account: ResolveCredential: auth_mode %q requires a credential — none configured", mode)
	}

	switch cred.Source {
	case CredentialSourceVault:
		if strings.TrimSpace(cred.Provider) == "" || strings.TrimSpace(cred.KeyRef) == "" {
			return "", "", fmt.Errorf("account: ResolveCredential: vault source requires both provider and key_ref")
		}
		secret, ferr := signing.FetchSecret(ctx, cred.Provider, cred.KeyRef)
		if ferr != nil {
			return "", "", fmt.Errorf("account: ResolveCredential: vault fetch (provider %q, key_ref %q): %w — fill the vault item", cred.Provider, cred.KeyRef, ferr)
		}
		value = strings.TrimSpace(string(secret))
	case CredentialSourceEnv:
		if strings.TrimSpace(cred.EnvVar) == "" {
			return "", "", fmt.Errorf("account: ResolveCredential: env source requires env_var")
		}
		if canonicalCredentialEnvVar[cred.EnvVar] {
			// Defense in depth: registry.validateCredential already refuses
			// this shape at Store.Add/Get/List time for a registry-backed
			// record, so a caller reaching here should never carry it — but
			// re-check so any other caller (or a hand-edited registry file
			// bypassing Store.Save) still fails closed instead of letting a
			// dispatched agent's own ambient env satisfy its own credential
			// lookup.
			return "", "", fmt.Errorf("account: ResolveCredential: env_var %q is a canonical injected name and must not be used as the source (name a purpose-specific var instead)", cred.EnvVar)
		}
		value = strings.TrimSpace(os.Getenv(cred.EnvVar))
	default:
		return "", "", fmt.Errorf("account: ResolveCredential: unknown credential source %q", cred.Source)
	}

	if value == "" {
		return "", "", fmt.Errorf("account: ResolveCredential: resolved credential for auth_mode %q is empty — export the named var / fill the vault item", mode)
	}
	return envVar, value, nil
}
