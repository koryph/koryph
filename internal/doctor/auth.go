// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"fmt"
	"sort"

	"github.com/koryph/koryph/internal/registry"
)

const checkNameAuth = "auth-mode"

// checkAuthMode iterates every registered project and reports, per account
// (koryph-i3b.10, design docs/designs/2026-07-api-key-auth.md §11 AC7): the
// EffectiveAuthMode(), the credential source (vault+provider or env+EnvVar
// NAME — never the secret value itself, which this check never reads), and
// the IdentityFingerprint prefix. subscription mode (the default; empty
// AuthMode) has no credential/fingerprint to report and is always OK.
//
// Per the koryph-i3b.10 operator recovery note (post-9f3ed4d): Credential
// lives in internal/authmode (registry aliases it), and per-runtime
// RuntimeAccounts entries carry their own AuthMode/EffectiveAuthMode
// (registry types.go:374-385) that can genuinely differ from the record's
// flat fields. Each such entry gets its own finding — see
// checkRuntimeAccountsAuth.
//
// Registry load failures are reported as a single WARN so a corrupt
// registry.d entry doesn't silence the whole check, matching checkProxy's
// precedent.
func checkAuthMode(opts Options) []Finding {
	recs, err := opts.registryList()
	if err != nil {
		return []Finding{{
			Check:   checkNameAuth,
			Level:   LevelWarn,
			Message: "cannot list registry records for auth-mode check: " + err.Error(),
		}}
	}

	var findings []Finding
	for _, rec := range recs {
		findings = append(findings, checkOneAccountAuth(rec))
		findings = append(findings, checkRuntimeAccountsAuth(rec)...)
	}
	return findings
}

// acctLabel names a registry record for a Finding message: the project ID,
// plus the account profile when it differs from the default personal
// profile (empty string) so a multi-account operator can tell records apart.
func acctLabel(rec *registry.Record) string {
	if rec.AccountProfile == "" {
		return rec.ProjectID
	}
	return fmt.Sprintf("%s (account %s)", rec.ProjectID, rec.AccountProfile)
}

// checkOneAccountAuth reports one registry record's auth mode, credential
// source, and fingerprint prefix. It never reads or prints a credential
// value — only the non-secret Source/Provider/EnvVar/KeyRef reference
// fields and the already-truncated IdentityFingerprint.
func checkOneAccountAuth(rec *registry.Record) Finding {
	return authFinding(acctLabel(rec), rec.EffectiveAuthMode(), rec.Credential, rec.IdentityFingerprint)
}

// checkRuntimeAccountsAuth reports one additional finding per
// RuntimeAccounts entry whose resolved auth mode, credential, or
// fingerprint diverges from the record's own flat fields — i.e. a runtime
// that genuinely authenticates differently from the record's default,
// per the koryph-i3b.10 operator note. A RuntimeAccounts entry that merely
// mirrors the record's flat fields is not reported again: it carries no
// information the base checkOneAccountAuth finding didn't already give,
// and repeating it per-runtime would just be noise. Entries are visited in
// sorted-name order so output is deterministic despite Go's randomized map
// iteration.
func checkRuntimeAccountsAuth(rec *registry.Record) []Finding {
	if len(rec.RuntimeAccounts) == 0 {
		return nil
	}

	names := make([]string, 0, len(rec.RuntimeAccounts))
	for name := range rec.RuntimeAccounts {
		names = append(names, name)
	}
	sort.Strings(names)

	label := acctLabel(rec)
	var findings []Finding
	for _, name := range names {
		ra := rec.RuntimeAccounts[name]
		if !runtimeAuthDiverges(rec, ra) {
			continue
		}
		findings = append(findings, authFinding(
			fmt.Sprintf("%s (runtime %s)", label, name),
			ra.EffectiveAuthMode(), ra.Credential, ra.IdentityFingerprint,
		))
	}
	return findings
}

// runtimeAuthDiverges reports whether ra's resolved auth mode, credential,
// or fingerprint differs from rec's own flat fields.
func runtimeAuthDiverges(rec *registry.Record, ra registry.RuntimeAccount) bool {
	if ra.EffectiveAuthMode() != rec.EffectiveAuthMode() {
		return true
	}
	if ra.IdentityFingerprint != rec.IdentityFingerprint {
		return true
	}
	return !credentialEqual(ra.Credential, rec.Credential)
}

// credentialEqual reports whether two credential references are the same,
// treating both-nil as equal. Credential is a flat struct of comparable
// fields (koryph-i3b types.go:184-204), so a value comparison is sufficient
// — no secret is read or compared here.
func credentialEqual(a, b *registry.Credential) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// authFinding reports one account-like entity's auth mode, credential
// source, and fingerprint prefix. It never reads or prints a credential
// value — only the non-secret Source/Provider/EnvVar/KeyRef reference
// fields and the already-truncated fingerprint. Shared by
// checkOneAccountAuth (a registry.Record's flat fields) and
// checkRuntimeAccountsAuth (a diverging registry.RuntimeAccount entry).
func authFinding(label, mode string, cred *registry.Credential, fingerprint string) Finding {
	if mode == registry.AuthModeSubscription {
		return Finding{
			Check:   checkNameAuth,
			Level:   LevelOK,
			Message: fmt.Sprintf("account %s: auth_mode=subscription (no credential — subscription mode)", label),
		}
	}

	if cred == nil {
		return Finding{
			Check:   checkNameAuth,
			Level:   LevelError,
			Message: fmt.Sprintf("account %s: auth_mode=%s but no credential configured", label, mode),
		}
	}

	var source string
	switch cred.Source {
	case registry.CredentialSourceVault:
		source = fmt.Sprintf("vault (provider=%s key_ref=%s)", cred.Provider, cred.KeyRef)
	case registry.CredentialSourceEnv:
		source = fmt.Sprintf("env (env_var=%s)", cred.EnvVar)
	default:
		return Finding{
			Check:   checkNameAuth,
			Level:   LevelError,
			Message: fmt.Sprintf("account %s: auth_mode=%s has unrecognized credential source %q", label, mode, cred.Source),
		}
	}

	if fingerprint == "" {
		return Finding{
			Check: checkNameAuth,
			Level: LevelWarn,
			Message: fmt.Sprintf("account %s: auth_mode=%s credential_source=%s but no identity_fingerprint recorded yet — captured at next dispatch",
				label, mode, source),
		}
	}

	return Finding{
		Check: checkNameAuth,
		Level: LevelOK,
		Message: fmt.Sprintf("account %s: auth_mode=%s credential_source=%s fingerprint=%s",
			label, mode, source, fingerprint),
	}
}
