// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"bytes"
	"encoding/pem"
	"os"
	"strings"
)

// PostureLevel classifies the security posture of a signing key configuration.
// Higher values indicate better posture.
type PostureLevel int

const (
	// PosturePlaintext — the key is stored unencrypted on disk. WARN.
	PosturePlaintext PostureLevel = iota
	// PosturePassphraseProtected — the key is encrypted at rest (passphrase-
	// protected OpenSSH private key, or an age-encrypted file). OK with info note.
	PosturePassphraseProtected
	// PostureKeychain — key is stored in the macOS Keychain. OK.
	PostureKeychain
	// PostureVault — key is managed by a password-manager or cloud vault. OK.
	PostureVault
)

// PostureResult is the output of ClassifyPosture.
type PostureResult struct {
	Level   PostureLevel
	Summary string // short human description ("vault-backed", etc.)
	Note    string // optional info note; empty for Vault/Keychain; non-empty for PassphraseProtected; WARN text for Plaintext
}

// ClassifyPosture inspects the signing config and the key on disk (if
// applicable) and returns a PostureResult that the doctor check and
// signing-status command display.
//
// Detection rules (applied in order):
//  1. Vault-backed provider (protonpass, onepassword, cloud, keepassxc, …) → Vault
//  2. provider=keychain → Keychain
//  3. provider=encrypted-file (any file content) → PassphraseProtected
//  4. provider=file, key file has bcrypt kdfname or ENCRYPTED/EncryptedPrivateKeyInfo PEM header → PassphraseProtected
//  5. provider=file, age header → PassphraseProtected
//  6. provider=file, no encryption evidence → Plaintext
//  7. provider="" → no key material configured; caller decides severity
func ClassifyPosture(cfg *Config) PostureResult {
	if cfg == nil {
		return PostureResult{Level: PosturePlaintext, Summary: "not configured"}
	}

	// Vault-backed providers always get the top posture regardless of key content.
	if IsVaultBacked(cfg.Provider) {
		return PostureResult{
			Level:   PostureVault,
			Summary: "vault-backed (" + cfg.Provider + ")",
		}
	}

	switch cfg.Provider {
	case ProviderKeychain:
		return PostureResult{
			Level:   PostureKeychain,
			Summary: "macOS Keychain",
		}

	case ProviderEncryptedFile:
		// age-encrypted file — encrypted at rest by definition.
		return PostureResult{
			Level:   PosturePassphraseProtected,
			Summary: "age-encrypted file",
			Note:    "same posture as a passphrase-protected ~/.ssh key — keep it backed up, never commit it",
		}

	case ProviderFile:
		// Inspect the key file on disk to determine if it is encrypted.
		if cfg.KeyRef == "" {
			return PostureResult{Level: PosturePlaintext, Summary: "file (no key_ref)"}
		}
		data, err := os.ReadFile(cfg.KeyRef)
		if err != nil {
			// Cannot read file; cannot determine posture; conservative.
			return PostureResult{
				Level:   PosturePlaintext,
				Summary: "file (unreadable)",
				Note:    "cannot determine key encryption state",
			}
		}
		if isEncryptedKeyMaterial(data) {
			return PostureResult{
				Level:   PosturePassphraseProtected,
				Summary: "passphrase-protected file",
				Note:    "same posture as a passphrase-protected ~/.ssh key — keep it backed up, never commit it",
			}
		}
		return PostureResult{
			Level:   PosturePlaintext,
			Summary: "plaintext file",
			Note:    "WARN: key is stored unencrypted on disk — consider `koryph signing setup --provider encrypted-file` or a vault provider (see docs/user-guide/signing.md)",
		}
	}

	// No provider configured.
	return PostureResult{Level: PosturePlaintext, Summary: "no provider"}
}

// isEncryptedKeyMaterial reports whether data contains evidence of encryption:
//   - OpenSSH bcrypt kdfname ("bcrypt" in the header section)
//   - PEM block type "ENCRYPTED PRIVATE KEY" or "ENCRYPTED RSA PRIVATE KEY"
//   - PEM Proc-Type: 4,ENCRYPTED DEK-Info header
//   - age encryption header magic
//
// This is intentionally conservative: unknown formats return false.
func isEncryptedKeyMaterial(data []byte) bool {
	if IsAgeEncrypted(data) {
		return true
	}

	// OpenSSH private key format: starts with the magic string and contains a
	// kdfname field. The kdfname "bcrypt" indicates passphrase protection; "none"
	// means no passphrase.
	//
	// The full binary format is:
	//   "openssh-key-v1\0" || ciphername || kdfname || kdfparams || ...
	//
	// We detect "bcrypt" inside the first 256 bytes after the magic header as a
	// lightweight heuristic (the correct approach would be to parse the binary
	// structure, but string search is sufficient for our posture classification).
	opensshMagic := []byte("openssh-key-v1\x00")
	if bytes.Contains(data, opensshMagic) {
		header := data
		if idx := bytes.Index(data, opensshMagic); idx >= 0 {
			// Scan the 256 bytes after the magic for "bcrypt".
			end := idx + len(opensshMagic) + 256
			if end > len(data) {
				end = len(data)
			}
			header = data[idx:end]
		}
		return bytes.Contains(header, []byte("bcrypt"))
	}

	// PEM-encoded keys: try all blocks.
	rest := data
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = next
		// "ENCRYPTED PRIVATE KEY" = PKCS#8 EncryptedPrivateKeyInfo
		// "RSA PRIVATE KEY" with Proc-Type: 4,ENCRYPTED = traditional PEM encryption
		// "EC PRIVATE KEY" with Proc-Type too
		if strings.Contains(block.Type, "ENCRYPTED") {
			return true
		}
		if block.Headers["Proc-Type"] == "4,ENCRYPTED" {
			return true
		}
	}

	return false
}

// PostureOK reports whether the posture level warrants an OK (no warning)
// in the doctor / status output.
func (p PostureLevel) PostureOK() bool {
	return p >= PosturePassphraseProtected
}
