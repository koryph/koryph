// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"flag"

	"github.com/koryph/koryph/internal/registry"
)

// This file holds the --auth-mode/--credential-* flags shared by `koryph
// adopt` and `koryph project add` (docs/designs/2026-07-api-key-auth.md §8).
// Both commands register the identical flag set and pass the resulting
// *registry.Credential straight through to onboard.RegisterOpts; field-level
// validation (vault needs provider+key_ref, env needs credential-env,
// canonical env var names refused, auth mode recognized) happens downstream
// in onboard.Register / account.ResolveCredential — this file only collects
// what the operator passed.

// authModeUsage is the --auth-mode flag's help text. It spells out the
// pay-per-token implication of api-key mode inline (acceptance criterion:
// "help text documents the pay-per-token implication of api-key mode") so an
// operator sees the billing consequence before the flag fails closed later.
const authModeUsage = "auth mode: subscription (default; OAuth login) | " +
	"api-key (long-lived ANTHROPIC_API_KEY; bills PAY-PER-TOKEN, not the subscription; requires --credential-*) | " +
	"oauth-token (long-lived CLAUDE_CODE_OAUTH_TOKEN; subscription-billed; requires --credential-*)"

// credentialFlags collects --credential-source/--credential-provider/
// --credential-ref/--credential-env.
type credentialFlags struct {
	source   *string
	provider *string
	keyRef   *string
	envVar   *string
}

// registerCredentialFlags registers the --credential-* flags on fs.
func registerCredentialFlags(fs *flag.FlagSet) *credentialFlags {
	return &credentialFlags{
		source:   fs.String("credential-source", "", "credential source for --auth-mode api-key|oauth-token: vault|env"),
		provider: fs.String("credential-provider", "", "vault provider name (with --credential-source vault)"),
		keyRef:   fs.String("credential-ref", "", "vault item reference/name (with --credential-source vault)"),
		envVar:   fs.String("credential-env", "", "purpose-named env var holding the credential (with --credential-source env; must not be ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN)"),
	}
}

// build returns nil when no --credential-* flag was passed (subscription
// mode, or an operator relying on discovery); otherwise a *registry.Credential
// carrying exactly what was passed, unvalidated — onboard.Register (via
// account.ResolveCredential) refuses an incomplete or invalid shape at
// register/dispatch time and names the exact fix.
func (c *credentialFlags) build() *registry.Credential {
	if *c.source == "" && *c.provider == "" && *c.keyRef == "" && *c.envVar == "" {
		return nil
	}
	return &registry.Credential{
		Source:   *c.source,
		Provider: *c.provider,
		KeyRef:   *c.keyRef,
		EnvVar:   *c.envVar,
	}
}
