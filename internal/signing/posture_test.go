// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// TestClassifyPostureVaultBacked verifies that all vault-backed providers
// return PostureVault.
func TestClassifyPostureVaultBacked(t *testing.T) {
	vaultProviders := []string{
		ProviderProtonPass, ProviderOnePassword,
		ProviderAWSSecretsManager, ProviderAzureKeyVault, ProviderGCPSecretManager,
		ProviderKeePassXC, ProviderOpenBao, ProviderHashiCorpVault,
		ProviderCommand,
	}
	for _, p := range vaultProviders {
		t.Run(p, func(t *testing.T) {
			cfg := &Config{Provider: p}
			r := ClassifyPosture(cfg)
			if r.Level != PostureVault {
				t.Errorf("provider %s: got level %d, want PostureVault (%d)", p, r.Level, PostureVault)
			}
			if r.Note != "" {
				t.Errorf("vault posture should have no note, got %q", r.Note)
			}
		})
	}
}

// TestClassifyPostureEncryptedFile verifies encrypted-file maps to PassphraseProtected.
func TestClassifyPostureEncryptedFile(t *testing.T) {
	cfg := &Config{Provider: ProviderEncryptedFile, KeyRef: "/some/path"}
	r := ClassifyPosture(cfg)
	if r.Level != PosturePassphraseProtected {
		t.Errorf("level = %d, want PosturePassphraseProtected", r.Level)
	}
	if r.Note == "" {
		t.Error("PassphraseProtected should carry an info note")
	}
}

// TestClassifyPosturePlaintextFile verifies an unencrypted file key returns WARN.
func TestClassifyPosturePlaintextFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "plain.key")
	// Write a fake plaintext (non-encrypted) key.
	if err := os.WriteFile(keyPath, []byte("not-an-encrypted-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Provider: ProviderFile, KeyRef: keyPath}
	r := ClassifyPosture(cfg)
	if r.Level != PosturePlaintext {
		t.Errorf("level = %d, want PosturePlaintext", r.Level)
	}
	if r.Note == "" {
		t.Error("Plaintext posture should carry a warning note")
	}
}

// TestClassifyPostureEncryptedPEMFile verifies that a passphrase-protected
// PEM key (ENCRYPTED header) is classified as PassphraseProtected.
func TestClassifyPostureEncryptedPEMFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "enc.key")

	// Write a PEM block with Proc-Type: 4,ENCRYPTED.
	block := &pem.Block{
		Type: "RSA PRIVATE KEY",
		Headers: map[string]string{
			"Proc-Type": "4,ENCRYPTED",
			"DEK-Info":  "AES-128-CBC,AABBCCDDEEFF0011",
		},
		Bytes: []byte("fake-encrypted-data"),
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Provider: ProviderFile, KeyRef: keyPath}
	r := ClassifyPosture(cfg)
	if r.Level != PosturePassphraseProtected {
		t.Errorf("level = %d, want PosturePassphraseProtected", r.Level)
	}
}

// TestClassifyPostureOpenSSHBcrypt verifies that an OpenSSH key with bcrypt
// kdfname is classified as PassphraseProtected.
func TestClassifyPostureOpenSSHBcrypt(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "openssh.key")

	// Fake OpenSSH private key header with bcrypt marker.
	// Real format: "openssh-key-v1\0" + null-terminated ciphername + kdfname.
	// We embed "bcrypt" after the magic to trigger the classifier.
	data := []byte("openssh-key-v1\x00aes256-ctr\x00bcrypt\x00more-data")
	if err := os.WriteFile(keyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Provider: ProviderFile, KeyRef: keyPath}
	r := ClassifyPosture(cfg)
	if r.Level != PosturePassphraseProtected {
		t.Errorf("level = %d, want PosturePassphraseProtected; data = %q", r.Level, data)
	}
}

// TestClassifyPostureAgeFile verifies that an age-encrypted file stored via
// the file provider is classified as PassphraseProtected.
func TestClassifyPostureAgeFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "age.key")
	const passphrase = "posture-test-passphrase"

	if err := StoreEncryptedFile(keyPath, []byte("key-material"), passphrase); err != nil {
		t.Fatalf("StoreEncryptedFile: %v", err)
	}

	// file provider (not encrypted-file) pointing at an age blob.
	cfg := &Config{Provider: ProviderFile, KeyRef: keyPath}
	r := ClassifyPosture(cfg)
	if r.Level != PosturePassphraseProtected {
		t.Errorf("level = %d, want PosturePassphraseProtected for age file via file provider", r.Level)
	}
}

// TestClassifyPostureNil verifies that nil config returns PosturePlaintext.
func TestClassifyPostureNil(t *testing.T) {
	r := ClassifyPosture(nil)
	if r.Level != PosturePlaintext {
		t.Errorf("level = %d, want PosturePlaintext for nil config", r.Level)
	}
}

// TestIsEncryptedKeyMaterialPlaintext verifies plaintext content is not flagged.
func TestIsEncryptedKeyMaterialPlaintext(t *testing.T) {
	plaintext := []byte("this is just a regular string without any key headers")
	if isEncryptedKeyMaterial(plaintext) {
		t.Error("plaintext should not be classified as encrypted key material")
	}
}

// TestPostureLevelPostureOK confirms the PostureOK helper reflects the ladder.
func TestPostureLevelPostureOK(t *testing.T) {
	cases := []struct {
		level PostureLevel
		ok    bool
	}{
		{PosturePlaintext, false},
		{PosturePassphraseProtected, true},
		{PostureKeychain, true},
		{PostureVault, true},
	}
	for _, tc := range cases {
		if got := tc.level.PostureOK(); got != tc.ok {
			t.Errorf("level %d PostureOK() = %v, want %v", tc.level, got, tc.ok)
		}
	}
}

// TestIsVaultBacked verifies the vault-backed predicate.
func TestIsVaultBacked(t *testing.T) {
	vaultProviders := []string{
		ProviderProtonPass, ProviderOnePassword,
		ProviderAWSSecretsManager, ProviderAzureKeyVault, ProviderGCPSecretManager,
		ProviderKeePassXC, ProviderOpenBao, ProviderHashiCorpVault,
		ProviderCommand,
	}
	for _, p := range vaultProviders {
		if !IsVaultBacked(p) {
			t.Errorf("IsVaultBacked(%q) = false, want true", p)
		}
	}
	for _, p := range []string{ProviderFile, ProviderEncryptedFile, ProviderKeychain} {
		if IsVaultBacked(p) {
			t.Errorf("IsVaultBacked(%q) = true, want false", p)
		}
	}
}
