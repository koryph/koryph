// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import "fmt"

// VaultErrClass categorises the failure mode of a vault key fetch so callers
// can surface the exact remediation to the operator without leaking raw
// secret material in the primary error message.
//
// The set is intentionally extensible: future providers (KeePassXC sealed
// state, encrypted-file wrong passphrase) map onto existing classes without
// changing the error type.
type VaultErrClass string

const (
	// VaultErrNotInstalled means the vault CLI binary is not on PATH.
	VaultErrNotInstalled VaultErrClass = "NotInstalled"

	// VaultErrNotAuthenticated means the CLI is present but the session has
	// expired or the passphrase is wrong.
	//   protonpass: run `pass-cli login`
	//   onepassword: run `op signin`
	//   encrypted-file: wrong KORYPH_PASSPHRASE or interactive prompt rejected
	VaultErrNotAuthenticated VaultErrClass = "NotAuthenticated"

	// VaultErrSealedOrLocked means the vault is sealed or locked and must be
	// unlocked before secrets can be read.
	//   openbao / hashicorp vault: run `bao/vault operator unseal`
	//   keepassxc: open the KeePassXC database
	VaultErrSealedOrLocked VaultErrClass = "SealedOrLocked"

	// VaultErrRefNotFound means the key_ref URI/path does not exist inside the
	// vault (the item was deleted, renamed, or the ref is wrong).
	VaultErrRefNotFound VaultErrClass = "RefNotFound"

	// VaultErrPermissionDenied means the authenticated session lacks read
	// access to the named secret.
	VaultErrPermissionDenied VaultErrClass = "PermissionDenied"
)

// VaultErr is the typed error returned by ResolveKey when vault access fails.
// It carries the provider-exact remediation so the operator knows exactly what
// to run — raw stderr is available in Detail for debug output only.
//
// Shape is intentionally extensible for the fr3 provider additions
// (sealed-vs-unauth distinction, KeePassXC lock states).
type VaultErr struct {
	// Class is the failure category.
	Class VaultErrClass
	// Provider is the named vault provider (e.g. "protonpass", "onepassword").
	Provider string
	// Remediation is the exact command or action the operator should take.
	Remediation string
	// Detail holds raw stderr or low-level error text. Shown only in verbose/
	// debug output; never in the primary user-facing error message.
	Detail string
}

// Error implements the error interface. The primary message surfaces the class
// and remediation; Detail is omitted to avoid leaking provider internals in
// normal output.
func (e *VaultErr) Error() string {
	if e.Remediation != "" {
		return fmt.Sprintf("vault [%s] provider=%s: %s", e.Class, e.Provider, e.Remediation)
	}
	return fmt.Sprintf("vault [%s] provider=%s", e.Class, e.Provider)
}
