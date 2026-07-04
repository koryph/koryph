// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEncryptedFileRoundTrip verifies that StoreEncryptedFile + FetchEncryptedFile
// preserves the secret and that the stored file begins with the age header.
func TestEncryptedFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.key")
	secret := []byte("koryph-test-signing-key-material-for-testing-only")
	const passphrase = "hunter2-test-only"

	if err := StoreEncryptedFile(path, secret, passphrase); err != nil {
		t.Fatalf("StoreEncryptedFile: %v", err)
	}

	// File must exist with mode 0600.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}

	// File must start with the age header.
	raw, _ := os.ReadFile(path)
	if !IsAgeEncrypted(raw) {
		t.Errorf("stored file does not have age header; first 40 bytes: %q", raw[:min(40, len(raw))])
	}

	// Fetch must round-trip the secret.
	t.Setenv("KORYPH_PASSPHRASE", passphrase)
	got, err := FetchEncryptedFile(path)
	if err != nil {
		t.Fatalf("FetchEncryptedFile: %v", err)
	}
	if string(got) != string(secret) {
		t.Errorf("got %q, want %q", got, secret)
	}
}

// TestEncryptedFileWrongPassphrase verifies that decryption fails with the
// wrong passphrase.
func TestEncryptedFileWrongPassphrase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.key")
	if err := StoreEncryptedFile(path, []byte("secret"), "correct-passphrase"); err != nil {
		t.Fatalf("store: %v", err)
	}

	t.Setenv("KORYPH_PASSPHRASE", "wrong-passphrase")
	_, err := FetchEncryptedFile(path)
	if err == nil {
		t.Fatal("want error with wrong passphrase, got nil")
	}
	if !strings.Contains(err.Error(), "decrypt") && !strings.Contains(err.Error(), "wrong passphrase") {
		t.Errorf("error message does not mention decrypt: %v", err)
	}
}

// TestEncryptedFileEmptyPassphraseRejected verifies that StoreEncryptedFile
// rejects an empty passphrase.
func TestEncryptedFileEmptyPassphraseRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.key")
	err := StoreEncryptedFile(path, []byte("secret"), "")
	if err == nil {
		t.Fatal("want error for empty passphrase, got nil")
	}
	if !strings.Contains(err.Error(), "passphrase") {
		t.Errorf("error should mention passphrase: %v", err)
	}
}

// TestEncryptedFileViaVaultFetch verifies that the vault Fetch dispatch routes
// encrypted-file through FetchEncryptedFile.
func TestEncryptedFileViaVaultFetch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.age")
	const secret = "my-ssh-key-content"
	const passphrase = "test-vault-passphrase"

	if err := StoreEncryptedFile(path, []byte(secret), passphrase); err != nil {
		t.Fatalf("store: %v", err)
	}

	t.Setenv("KORYPH_PASSPHRASE", passphrase)
	v := DefaultVault()
	got, err := v.Fetch(context.Background(), ProviderEncryptedFile, path)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.TrimSpace(string(got)) != secret {
		t.Errorf("got %q, want %q", got, secret)
	}
}

// TestEncryptedFileProviderValidateAccepted confirms Config.Validate accepts
// encrypted-file as a provider.
func TestEncryptedFileProviderValidateAccepted(t *testing.T) {
	cfg := Config{Mode: ModeSSH, Provider: ProviderEncryptedFile, KeyRef: "/tmp/key"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with provider=encrypted-file: unexpected error: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
