// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- ResolveKey tests -------------------------------------------------------

func TestResolveKey_InlineMode(t *testing.T) {
	// When Provider is empty, ResolveKey returns cfg.PEM as-is (no vault call).
	pemStr := generateTestPEM(t)
	cfg := &Config{Name: "inline-bot", AppID: 1, PEM: pemStr}

	got, err := ResolveKey(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveKey inline: %v", err)
	}
	if got != pemStr {
		t.Errorf("ResolveKey inline: got different PEM than expected")
	}
}

func TestResolveKey_InlineMode_EmptyPEM(t *testing.T) {
	// Empty PEM in inline mode returns empty string (not an error here;
	// JWT minting will surface the problem with a clear message).
	cfg := &Config{Name: "empty-bot", AppID: 1, PEM: ""}
	got, err := ResolveKey(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveKey empty PEM: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty PEM, got %q", got)
	}
}

func TestResolveKey_FileProvider(t *testing.T) {
	// Provider="file" with a valid key_ref path reads the file.
	tmp := t.TempDir()
	pemStr := generateTestPEM(t)
	keyPath := filepath.Join(tmp, "bot.pem")
	if err := os.WriteFile(keyPath, []byte(pemStr), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Name:     "file-bot",
		AppID:    2,
		Provider: "file",
		KeyRef:   keyPath,
	}

	got, err := ResolveKey(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveKey file: %v", err)
	}
	if got != pemStr {
		t.Errorf("ResolveKey file: PEM round-trip mismatch")
	}
}

func TestResolveKey_FileProvider_Missing(t *testing.T) {
	// Provider="file" with a non-existent path returns a VaultErr.
	cfg := &Config{
		Name:     "missing-bot",
		AppID:    3,
		Provider: "file",
		KeyRef:   "/nonexistent/path/bot.pem",
	}

	_, err := ResolveKey(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	var ve *VaultErr
	if !errors.As(err, &ve) {
		t.Fatalf("expected *VaultErr, got %T: %v", err, err)
	}
}

// --- resolveVaultDefaults tests ---------------------------------------------

func TestResolveVaultDefaults_NoProjectRoot(t *testing.T) {
	provider, container, err := resolveVaultDefaults("")
	if err != nil {
		t.Fatalf("resolveVaultDefaults empty root: %v", err)
	}
	if provider != "" || container != "" {
		t.Errorf("expected empty results for no project root, got provider=%q container=%q", provider, container)
	}
}

func TestResolveVaultDefaults_NoProjectFile(t *testing.T) {
	tmp := t.TempDir()
	provider, container, err := resolveVaultDefaults(tmp)
	if err != nil {
		t.Fatalf("resolveVaultDefaults no project file: %v", err)
	}
	if provider != "" || container != "" {
		t.Errorf("expected empty results for missing project file, got provider=%q container=%q", provider, container)
	}
}

func TestResolveVaultDefaults_NoSigningBlock(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "koryph.project.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, container, err := resolveVaultDefaults(tmp)
	if err != nil {
		t.Fatalf("resolveVaultDefaults no signing: %v", err)
	}
	if provider != "" || container != "" {
		t.Errorf("expected empty results for no signing block, got provider=%q container=%q", provider, container)
	}
}

// TestResolveVaultDefaults_ProjectVaultBlock verifies the new top-priority
// path: an explicit vault block in koryph.project.json wins over the signing
// block legacy proxy.
func TestResolveVaultDefaults_ProjectVaultBlock(t *testing.T) {
	tmp := t.TempDir()
	// vault block wins even when a signing block is also present.
	content := `{"vault":{"provider":"onepassword","container":"Engineering"},"signing":{"provider":"protonpass","key_ref":"pass://abc/123"}}`
	if err := os.WriteFile(filepath.Join(tmp, "koryph.project.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, container, err := resolveVaultDefaults(tmp)
	if err != nil {
		t.Fatalf("resolveVaultDefaults project vault block: %v", err)
	}
	if provider != "onepassword" {
		t.Errorf("expected provider=onepassword (vault block wins), got %q", provider)
	}
	if container != "Engineering" {
		t.Errorf("expected container=Engineering, got %q", container)
	}
}

// TestResolveVaultDefaults_SigningBlockLegacyProxy verifies that the signing
// block's provider + vault_name act as the legacy proxy when no vault block is
// present. key_ref is NOT returned as container (vault_name is).
func TestResolveVaultDefaults_SigningBlockLegacyProxy(t *testing.T) {
	tmp := t.TempDir()
	content := `{"signing":{"provider":"protonpass","key_ref":"pass://abc/123","vault_name":"MyVault"}}`
	if err := os.WriteFile(filepath.Join(tmp, "koryph.project.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, container, err := resolveVaultDefaults(tmp)
	if err != nil {
		t.Fatalf("resolveVaultDefaults signing legacy proxy: %v", err)
	}
	if provider != "protonpass" {
		t.Errorf("expected provider=protonpass, got %q", provider)
	}
	// container comes from vault_name, not key_ref.
	if container != "MyVault" {
		t.Errorf("expected container=MyVault (signing.vault_name), got %q", container)
	}
}

// TestResolveVaultDefaults_SigningBlockNoVaultName verifies that a signing block
// without vault_name results in an empty container (key_ref is not container).
func TestResolveVaultDefaults_SigningBlockNoVaultName(t *testing.T) {
	tmp := t.TempDir()
	content := `{"signing":{"provider":"protonpass","key_ref":"pass://abc/123"}}`
	if err := os.WriteFile(filepath.Join(tmp, "koryph.project.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, container, err := resolveVaultDefaults(tmp)
	if err != nil {
		t.Fatalf("resolveVaultDefaults signing no vault_name: %v", err)
	}
	if provider != "protonpass" {
		t.Errorf("expected provider=protonpass, got %q", provider)
	}
	// No vault_name → empty container; key_ref is NOT returned here.
	if container != "" {
		t.Errorf("expected empty container when vault_name absent, got %q", container)
	}
}

func TestResolveVaultDefaults_MalformedJSON(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "koryph.project.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Malformed JSON returns empty strings (no error) — safe fallback.
	provider, container, err := resolveVaultDefaults(tmp)
	if err != nil {
		t.Fatalf("resolveVaultDefaults malformed JSON should not error: %v", err)
	}
	if provider != "" || container != "" {
		t.Errorf("expected empty for malformed JSON, got provider=%q container=%q", provider, container)
	}
}

// --- classifyVaultErr tests -------------------------------------------------

func TestClassifyVaultErr_NotInstalled(t *testing.T) {
	err := errors.New("exec: \"pass-cli\": executable file not found in $PATH")
	ve := classifyVaultErr("protonpass", err)
	if ve.Class != VaultErrNotInstalled {
		t.Errorf("expected NotInstalled, got %s", ve.Class)
	}
	if !strings.Contains(ve.Remediation, "pass-cli") {
		t.Errorf("expected protonpass install hint, got: %q", ve.Remediation)
	}
	if ve.Detail != err.Error() {
		t.Errorf("Detail should carry original error text")
	}
}

func TestClassifyVaultErr_NotAuthenticated(t *testing.T) {
	err := errors.New("signing: pass-cli exited 1 (not logged in? run `pass-cli login` first): session expired")
	ve := classifyVaultErr("protonpass", err)
	if ve.Class != VaultErrNotAuthenticated {
		t.Errorf("expected NotAuthenticated, got %s", ve.Class)
	}
	if ve.Remediation != "pass-cli login" {
		t.Errorf("expected pass-cli login remediation, got: %q", ve.Remediation)
	}
}

func TestClassifyVaultErr_EncryptedFilePassphrase(t *testing.T) {
	err := errors.New("signing: encrypted-file: decrypt /path/key.age: wrong passphrase")
	ve := classifyVaultErr("encrypted-file", err)
	if ve.Class != VaultErrNotAuthenticated {
		t.Errorf("expected NotAuthenticated for wrong passphrase, got %s", ve.Class)
	}
	if !strings.Contains(ve.Remediation, "KORYPH_PASSPHRASE") {
		t.Errorf("expected KORYPH_PASSPHRASE hint, got: %q", ve.Remediation)
	}
}

func TestClassifyVaultErr_SealedOrLocked(t *testing.T) {
	err := errors.New("vault: error making API request: vault is sealed")
	ve := classifyVaultErr("vault", err)
	if ve.Class != VaultErrSealedOrLocked {
		t.Errorf("expected SealedOrLocked, got %s", ve.Class)
	}
	if !strings.Contains(ve.Remediation, "unseal") {
		t.Errorf("expected unseal hint, got: %q", ve.Remediation)
	}
}

func TestClassifyVaultErr_RefNotFound(t *testing.T) {
	err := errors.New("signing: provider \"onepassword\" returned an empty secret for ref \"op://vault/missing\"")
	ve := classifyVaultErr("onepassword", err)
	if ve.Class != VaultErrRefNotFound {
		t.Errorf("expected RefNotFound, got %s", ve.Class)
	}
}

func TestClassifyVaultErr_PermissionDenied(t *testing.T) {
	err := errors.New("aws: permission denied reading secret arn:aws:secretsmanager:us-east-1:123:secret:bot")
	ve := classifyVaultErr("aws_secretsmanager", err)
	if ve.Class != VaultErrPermissionDenied {
		t.Errorf("expected PermissionDenied, got %s", ve.Class)
	}
}

// --- VaultErr.Error() tests -------------------------------------------------

func TestVaultErrError_WithRemediation(t *testing.T) {
	ve := &VaultErr{
		Class:       VaultErrNotAuthenticated,
		Provider:    "protonpass",
		Remediation: "pass-cli login",
		Detail:      "session expired",
	}
	msg := ve.Error()
	if !strings.Contains(msg, "NotAuthenticated") {
		t.Errorf("Error() should contain class, got: %q", msg)
	}
	if !strings.Contains(msg, "protonpass") {
		t.Errorf("Error() should contain provider, got: %q", msg)
	}
	if !strings.Contains(msg, "pass-cli login") {
		t.Errorf("Error() should contain remediation, got: %q", msg)
	}
	// Detail should NOT appear in the primary error message.
	if strings.Contains(msg, "session expired") {
		t.Errorf("Error() should not expose Detail in primary message, got: %q", msg)
	}
}

func TestVaultErrError_NoRemediation(t *testing.T) {
	ve := &VaultErr{
		Class:    VaultErrRefNotFound,
		Provider: "file",
	}
	msg := ve.Error()
	if !strings.Contains(msg, "RefNotFound") {
		t.Errorf("Error() missing class: %q", msg)
	}
}

// --- defaultKeyRef tests ----------------------------------------------------

func TestDefaultKeyRef_Keychain(t *testing.T) {
	ref := defaultKeyRef("keychain", "my-bot")
	if ref != "koryph-bot-my-bot" {
		t.Errorf("keychain ref = %q, want koryph-bot-my-bot", ref)
	}
}

func TestDefaultKeyRef_EncryptedFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)
	ref := defaultKeyRef("encrypted-file", "my-bot")
	if !strings.HasSuffix(ref, "my-bot.age") {
		t.Errorf("encrypted-file ref should end with my-bot.age, got %q", ref)
	}
}
