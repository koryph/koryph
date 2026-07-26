// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	_ "embed"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/schemaver"
)

//go:embed vault.fingerprints
var vaultFingerprints []byte

//go:embed global_config.fingerprints
var globalConfigFingerprints []byte

func TestSigningSurfaceFingerprints(t *testing.T) {
	for _, test := range []struct {
		name    string
		surface schemaver.Surface
		history []byte
		value   any
	}{
		{"vault", schemaver.SigningVault, vaultFingerprints, VaultConfig{}},
		{"global config", schemaver.GlobalConfig, globalConfigFingerprints, GlobalConfig{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := schemaver.VerifyFingerprint(test.surface, test.history, test.value); err != nil {
				t.Fatal(err)
			}
			if _, err := schemaver.AppendFingerprint(test.surface, test.history, test.value); err == nil {
				t.Fatal("AppendFingerprint accepted overwrite of existing version")
			}
		})
	}
}

func TestSigningLoadsRejectNewerSchema(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)

	for _, test := range []struct {
		name string
		path string
		load func() error
	}{
		{"vault", VaultPath(), func() error { _, err := LoadVault(); return err }},
		{"global config", GlobalConfigPath(), func() error { _, err := LoadGlobalConfig(); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(test.path, []byte(`{"schema_version":2}`), 0o600); err != nil {
				t.Fatal(err)
			}
			err := test.load()
			var tooNew *schemaver.TooNewError
			if !errors.As(err, &tooNew) || tooNew.Op != "read" {
				t.Fatalf("load error = %v, want read TooNewError", err)
			}
		})
	}
}

func TestSigningSavesRejectNewerSchemaWithoutChangingState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)

	for _, test := range []struct {
		name string
		path string
		save func() error
	}{
		{"vault", VaultPath(), func() error { return SaveVault(DefaultVault()) }},
		{"global config", GlobalConfigPath(), func() error { return SaveGlobalConfig(&GlobalConfig{}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := []byte(`{"schema_version":2,"future":"keep"}`)
			if err := os.WriteFile(test.path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			err := test.save()
			var tooNew *schemaver.TooNewError
			if !errors.As(err, &tooNew) || tooNew.Op != "write" {
				t.Fatalf("save error = %v, want write TooNewError", err)
			}
			got, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(original) {
				t.Fatalf("newer state changed:\n got %s\nwant %s", got, original)
			}
		})
	}
}

func TestSigningLegacyStateMigratesInMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)

	for _, test := range []struct {
		name    string
		surface schemaver.Surface
		path    string
		raw     string
		load    func() (int, error)
	}{
		{
			name:    "vault",
			surface: schemaver.SigningVault,
			path:    VaultPath(),
			raw:     `{"providers":{"command":{"fetch":["command"]}}}`,
			load:    func() (int, error) { v, err := LoadVault(); return v.SchemaVersion, err },
		},
		{
			name:    "global config",
			surface: schemaver.GlobalConfig,
			path:    GlobalConfigPath(),
			raw:     `{"default_timeout_seconds":30}`,
			load:    func() (int, error) { c, err := LoadGlobalConfig(); return c.SchemaVersion, err },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(test.path, []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			version, err := test.load()
			if err != nil {
				t.Fatal(err)
			}
			if version != schemaver.Current(test.surface) {
				t.Errorf("schema_version = %d, want current", version)
			}
			persisted, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(persisted), "schema_version") {
				t.Fatalf("read-only migration changed file: %s", persisted)
			}
		})
	}
}

func TestSigningSavesStampCurrentSchema(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	if err := SaveVault(&VaultConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := SaveGlobalConfig(&GlobalConfig{}); err != nil {
		t.Fatal(err)
	}
	vault, err := LoadVault()
	if err != nil {
		t.Fatal(err)
	}
	global, err := LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if vault.SchemaVersion != schemaver.Current(schemaver.SigningVault) || global.SchemaVersion != schemaver.Current(schemaver.GlobalConfig) {
		t.Fatalf("saved versions = vault %d, global %d", vault.SchemaVersion, global.SchemaVersion)
	}
}
