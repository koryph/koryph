// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build darwin

package signing

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
)

// keychainService is the service name used for all koryph items in the macOS
// Keychain. The account is the key_ref (e.g. a project-scoped identifier).
const keychainService = "koryph"

// keychainTimeout bounds one security(1) invocation.
const keychainTimeout = 30 * time.Second

// FetchKeychain retrieves the secret stored for ref from the macOS Keychain.
// It invokes `security find-generic-password -s koryph -a <ref> -w` which
// prints the password to stdout.
//
// Secrets are stored base64-encoded (see StoreKeychain); FetchKeychain
// base64-decodes the raw Keychain value before returning it, preserving the
// original bytes (including multi-line PEM keys).
//
// This is the Fetch implementation for the keychain built-in provider. It is
// only compiled on darwin; on other platforms the Fetch switch in vault.go
// returns an error before reaching this function (config validation rejects
// provider=keychain on non-darwin at setup time — see postureCheck).
func FetchKeychain(ref string) ([]byte, error) {
	if ref == "" {
		return nil, fmt.Errorf("signing: provider keychain needs a key_ref")
	}

	res, err := execx.Run(context.Background(), execx.Cmd{
		Name:    "security",
		Args:    []string{"find-generic-password", "-s", keychainService, "-a", ref, "-w"},
		Timeout: keychainTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("signing: keychain: security: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("signing: keychain: item %q not found in Keychain (service=%s): run `koryph signing enable --project <id>` to store it", ref, keychainService)
	}
	encoded := strings.TrimRight(res.Stdout, "\n")
	if encoded == "" {
		return nil, fmt.Errorf("signing: keychain: empty secret for ref %q", ref)
	}
	secret, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("signing: keychain: base64 decode for ref %q: %w", ref, err)
	}
	return secret, nil
}

// StoreKeychain writes secret to the macOS Keychain for service=koryph /
// account=ref. It uses `security add-generic-password -U` (upsert) to
// create or overwrite the item. The password is passed via stdin through
// `security -i` to avoid the value appearing in the process list.
//
// The secret is base64-encoded before embedding in the stdin command so that
// multi-line key material (e.g. PEM private keys) does not break security(1)'s
// line-based interactive parser. FetchKeychain reverses this encoding. The
// base64 value is a single line with no whitespace, safe for `-w <pass>`.
//
// The command written to stdin is:
//
//	add-generic-password -s koryph -a <ref> -U -w <base64(secret)>
//
// The encoded value is never in the argv (ps) of the security process.
func StoreKeychain(ref string, secret []byte) error {
	if ref == "" {
		return fmt.Errorf("signing: provider keychain needs a key_ref")
	}

	// Base64-encode the secret so that multi-line PEM keys become a single
	// whitespace-free token safe for `security -i`'s line-based parser.
	// FetchKeychain reverses this encoding.
	encoded := base64.StdEncoding.EncodeToString(secret)

	// Build the command to feed to security -i.
	// security -i reads: add-generic-password -s <svc> -a <acct> -U -w <pass>
	// The encoded value contains no whitespace, so it is safe to embed inline.
	// This avoids the password appearing in ps output for the security process.
	cmd := fmt.Sprintf("add-generic-password -s %s -a %s -U -w %s\n", keychainService, ref, encoded)

	res, err := execx.Run(context.Background(), execx.Cmd{
		Name:    "security",
		Args:    []string{"-i"},
		Stdin:   cmd,
		Timeout: keychainTimeout,
	})
	if err != nil {
		return fmt.Errorf("signing: keychain: security -i: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("signing: keychain: store failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
