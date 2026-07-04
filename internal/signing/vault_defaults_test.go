// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"os"
	"path/filepath"
	"testing"
)

// --- VaultDefaults.Validate tests -------------------------------------------

func TestVaultDefaults_Validate_Empty(t *testing.T) {
	v := &VaultDefaults{}
	if err := v.Validate(); err != nil {
		t.Errorf("empty VaultDefaults should be valid, got: %v", err)
	}
}

func TestVaultDefaults_Validate_KnownProviders(t *testing.T) {
	for _, p := range VaultProviders {
		v := &VaultDefaults{Provider: p, Container: "vault"}
		if err := v.Validate(); err != nil {
			t.Errorf("provider %q should be valid, got: %v", p, err)
		}
	}
}

func TestVaultDefaults_Validate_UnknownProvider(t *testing.T) {
	v := &VaultDefaults{Provider: "no-such-provider"}
	if err := v.Validate(); err == nil {
		t.Error("unknown provider should fail Validate, got nil")
	}
}

func TestVaultDefaults_Validate_ContainerNoProvider(t *testing.T) {
	// Container without Provider is still valid (container alone doesn't
	// make sense but is not rejected — provider is the gating field).
	v := &VaultDefaults{Container: "Engineering"}
	if err := v.Validate(); err != nil {
		t.Errorf("container without provider should be valid, got: %v", err)
	}
}

// --- GlobalConfig round-trip tests ------------------------------------------

func TestGlobalConfig_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	orig := &GlobalConfig{
		Vault: &VaultDefaults{
			Provider:  ProviderProtonPass,
			Container: "Engineering",
		},
	}
	if err := SaveGlobalConfig(orig); err != nil {
		t.Fatalf("SaveGlobalConfig: %v", err)
	}
	got, err := LoadGlobalConfig()
	if err != nil {
		t.Fatalf("LoadGlobalConfig: %v", err)
	}
	if got.Vault == nil {
		t.Fatal("LoadGlobalConfig: Vault is nil after round-trip")
	}
	if got.Vault.Provider != orig.Vault.Provider {
		t.Errorf("Provider: got %q want %q", got.Vault.Provider, orig.Vault.Provider)
	}
	if got.Vault.Container != orig.Vault.Container {
		t.Errorf("Container: got %q want %q", got.Vault.Container, orig.Vault.Container)
	}
}

func TestGlobalConfig_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	cfg, err := LoadGlobalConfig()
	if err != nil {
		t.Fatalf("LoadGlobalConfig missing file: %v", err)
	}
	if cfg.Vault != nil {
		t.Errorf("expected nil Vault for missing file, got %+v", cfg.Vault)
	}
}

// --- ResolveVaultDefaults tests ---------------------------------------------

func TestResolveVaultDefaults_NoProjectRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp) // no global config written
	d, err := ResolveVaultDefaults("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Provider != "" || d.Container != "" {
		t.Errorf("expected empty defaults, got %+v", d)
	}
}

func TestResolveVaultDefaults_ProjectVaultBlockWins(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	// Write global config with a different provider.
	_ = SaveGlobalConfig(&GlobalConfig{Vault: &VaultDefaults{Provider: ProviderEncryptedFile}})

	// Write project config with vault block.
	root := t.TempDir()
	content := `{"vault":{"provider":"protonpass","container":"Engineering"}}`
	writeProjectJSON(t, root, content)

	d, err := ResolveVaultDefaults(root)
	if err != nil {
		t.Fatalf("ResolveVaultDefaults: %v", err)
	}
	if d.Provider != ProviderProtonPass {
		t.Errorf("vault block should win: provider=%q, want protonpass", d.Provider)
	}
	if d.Container != "Engineering" {
		t.Errorf("vault block container=%q, want Engineering", d.Container)
	}
}

func TestResolveVaultDefaults_SigningProxyFallsThrough(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	root := t.TempDir()
	// Signing block with vault_name but no vault block.
	content := `{"signing":{"provider":"onepassword","key_ref":"op://v/i/f","vault_name":"Personal"}}`
	writeProjectJSON(t, root, content)

	d, err := ResolveVaultDefaults(root)
	if err != nil {
		t.Fatalf("ResolveVaultDefaults signing proxy: %v", err)
	}
	if d.Provider != ProviderOnePassword {
		t.Errorf("signing proxy: provider=%q, want onepassword", d.Provider)
	}
	if d.Container != "Personal" {
		t.Errorf("signing proxy: container=%q, want Personal (from vault_name)", d.Container)
	}
}

func TestResolveVaultDefaults_GlobalConfigFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	// Write global config.
	_ = SaveGlobalConfig(&GlobalConfig{Vault: &VaultDefaults{
		Provider:  ProviderOnePassword,
		Container: "Global",
	}})

	// Project with no vault / signing block.
	root := t.TempDir()
	writeProjectJSON(t, root, `{}`)

	d, err := ResolveVaultDefaults(root)
	if err != nil {
		t.Fatalf("ResolveVaultDefaults global fallback: %v", err)
	}
	if d.Provider != ProviderOnePassword {
		t.Errorf("global fallback: provider=%q, want onepassword", d.Provider)
	}
	if d.Container != "Global" {
		t.Errorf("global fallback: container=%q, want Global", d.Container)
	}
}

func TestResolveVaultDefaults_EmptyFallthrough(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	root := t.TempDir()
	writeProjectJSON(t, root, `{}`)

	d, err := ResolveVaultDefaults(root)
	if err != nil {
		t.Fatalf("ResolveVaultDefaults empty: %v", err)
	}
	if d.Provider != "" {
		t.Errorf("expected empty provider, got %q", d.Provider)
	}
}

// writeProjectJSON writes content to koryph.project.json in root.
func writeProjectJSON(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "koryph.project.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
