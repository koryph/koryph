// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExpandArgv(t *testing.T) {
	got := ExpandArgv([]string{"pass-cli", "item", "view", "{ref}", "--x={ref}"}, "pass://s/i/f")
	want := []string{"pass-cli", "item", "view", "pass://s/i/f", "--x=pass://s/i/f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandArgv = %v, want %v", got, want)
	}
	// The template itself must not be mutated.
	tpl := []string{"{ref}"}
	_ = ExpandArgv(tpl, "x")
	if tpl[0] != "{ref}" {
		t.Errorf("template mutated: %v", tpl)
	}
}

func TestDefaultVaultTemplates(t *testing.T) {
	v := DefaultVault()
	pp := v.Providers[ProviderProtonPass]
	if !reflect.DeepEqual(pp.Fetch, []string{"pass-cli", "item", "view", "{ref}"}) {
		t.Errorf("protonpass fetch = %v", pp.Fetch)
	}
	if !reflect.DeepEqual(pp.AgentLoad, []string{"pass-cli", "ssh-agent", "load"}) {
		t.Errorf("protonpass agent_load = %v", pp.AgentLoad)
	}
	if pp.LoginHint != "pass-cli login" {
		t.Errorf("protonpass login hint = %q", pp.LoginHint)
	}
	op := v.Providers[ProviderOnePassword]
	if !reflect.DeepEqual(op.Fetch, []string{"op", "read", "{ref}"}) {
		t.Errorf("onepassword fetch = %v", op.Fetch)
	}
	if len(v.Providers[ProviderCommand].Fetch) != 0 {
		t.Errorf("command provider must have no default template")
	}
}

func TestLoadVaultMissingFileYieldsDefaults(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	v, err := LoadVault()
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}
	if !reflect.DeepEqual(v, DefaultVault()) {
		t.Errorf("missing vault.json should yield defaults")
	}
}

func TestLoadVaultOverrideMergesPerProvider(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	custom := &VaultConfig{
		SchemaVersion: 1,
		Providers: map[string]ProviderTemplates{
			ProviderProtonPass: {
				Fetch:     []string{"pass-cli", "item", "view", "--vault-name", "Eng", "--item-title", "{ref}", "--field", "private_key"},
				AgentLoad: []string{"pass-cli", "ssh-agent", "load", "--vault-name", "Eng"},
			},
		},
	}
	if err := SaveVault(custom); err != nil {
		t.Fatalf("SaveVault: %v", err)
	}
	v, err := LoadVault()
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}
	// Overridden provider replaced wholesale.
	if got := v.Providers[ProviderProtonPass].Fetch; got[len(got)-1] != "private_key" {
		t.Errorf("protonpass override not applied: %v", got)
	}
	// Untouched providers keep their defaults.
	if !reflect.DeepEqual(v.Providers[ProviderOnePassword].Fetch, []string{"op", "read", "{ref}"}) {
		t.Errorf("onepassword defaults lost: %v", v.Providers[ProviderOnePassword].Fetch)
	}
}

func TestFetchFileProvider(t *testing.T) {
	v := DefaultVault()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte("hush\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := v.Fetch(context.Background(), ProviderFile, p)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != "hush\n" {
		t.Errorf("Fetch = %q", got)
	}
	if _, err := v.Fetch(context.Background(), ProviderFile, ""); err == nil {
		t.Errorf("empty file ref should error")
	}
}

func TestFetchCommandProviderNeedsTemplate(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	v := DefaultVault()
	_, err := v.Fetch(context.Background(), ProviderCommand, "x")
	if err == nil || !strings.Contains(err.Error(), "vault.json") {
		t.Errorf("err = %v, want vault.json guidance", err)
	}
}

// fakeCLI writes a shell script that appends its argv to argv.log in dir and
// then runs body.
func fakeCLI(t *testing.T, dir, body string) string {
	t.Helper()
	script := filepath.Join(dir, "fake-cli")
	content := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + filepath.Join(dir, "argv.log") + "\"\n" + body + "\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func argvLog(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "argv.log"))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestFetchRunsTemplateWithRefSubstitution(t *testing.T) {
	dir := t.TempDir()
	script := fakeCLI(t, dir, `printf 'sekret-material\n'`)
	v := DefaultVault()
	v.Providers[ProviderProtonPass] = ProviderTemplates{
		Fetch:     []string{script, "item", "view", "{ref}"},
		LoginHint: "pass-cli login",
	}
	got, err := v.Fetch(context.Background(), ProviderProtonPass, "pass://share/item/key")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != "sekret-material\n" {
		t.Errorf("Fetch = %q", got)
	}
	if log := argvLog(t, dir); !strings.Contains(log, "item view pass://share/item/key") {
		t.Errorf("argv log = %q, want substituted ref", log)
	}
}

func TestFetchFailureCarriesLoginHintNeverTheSecret(t *testing.T) {
	dir := t.TempDir()
	script := fakeCLI(t, dir, `echo "would-be-secret"; echo "session expired" >&2; exit 1`)
	v := DefaultVault()
	v.Providers[ProviderProtonPass] = ProviderTemplates{
		Fetch:     []string{script, "{ref}"},
		LoginHint: "pass-cli login",
	}
	_, err := v.Fetch(context.Background(), ProviderProtonPass, "r")
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pass-cli login") {
		t.Errorf("err %q missing login hint", msg)
	}
	if !strings.Contains(msg, "session expired") {
		t.Errorf("err %q missing stderr detail", msg)
	}
	if strings.Contains(msg, "would-be-secret") {
		t.Errorf("err %q leaks stdout (potential secret)", msg)
	}
}

func TestFetchEmptySecretIsError(t *testing.T) {
	dir := t.TempDir()
	script := fakeCLI(t, dir, `exit 0`)
	v := DefaultVault()
	v.Providers[ProviderCommand] = ProviderTemplates{Fetch: []string{script, "{ref}"}}
	if _, err := v.Fetch(context.Background(), ProviderCommand, "r"); err == nil {
		t.Errorf("empty stdout should be an error")
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"empty ok (not required, defaults to ssh but nothing enforced without provider)", Config{Mode: ModeGitsign}, true},
		{"bad mode", Config{Mode: "pgp"}, false},
		{"bad provider", Config{Mode: ModeGitsign, Provider: "keychain"}, false},
		{"required needs identity", Config{Required: true, Mode: ModeGitsign}, false},
		{"ssh needs provider", Config{Mode: ModeSSH}, false},
		{"file needs key_ref", Config{Mode: ModeSSH, Provider: ProviderFile}, false},
		{"required ssh needs public key", Config{Required: true, Mode: ModeSSH, Provider: ProviderProtonPass, Identity: "a@b"}, false},
		{"required ssh complete", Config{Required: true, Mode: ModeSSH, Provider: ProviderProtonPass, Identity: "a@b", PublicKey: "ssh-ed25519 AAAA"}, true},
		{"gitsign required", Config{Required: true, Mode: ModeGitsign, Identity: "a@b"}, true},
		{"artifacts needs provider", Config{Mode: ModeGitsign, Artifacts: true}, false},
		{"artifacts needs key_ref", Config{Mode: ModeGitsign, Provider: ProviderProtonPass, Artifacts: true}, false},
		{"artifacts complete", Config{Mode: ModeGitsign, Provider: ProviderProtonPass, KeyRef: "pass://s/i", Artifacts: true}, true},
	}
	for _, tc := range cases {
		err := tc.cfg.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: want error", tc.name)
		}
	}
}
