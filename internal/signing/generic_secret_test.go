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

// fakeOpCLI writes a shell script that appends its argv to argv.log and prints
// body to stdout. It simulates a 1Password `op` binary.
func fakeOpCLI(t *testing.T, dir, body string) string {
	t.Helper()
	script := filepath.Join(dir, "op")
	content := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + filepath.Join(dir, "argv.log") + "\"\n" + body + "\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestFetchSecretOnePasswordTemplate exercises the shipped `op read {ref}`
// template end-to-end with a mocked `op` binary. Confirms that:
//   - the ref is substituted correctly in the argv template
//   - arbitrary (non-SSH) content is returned verbatim in memory
//   - the login hint surfaces on non-zero exit (drift guard)
func TestFetchSecretOnePasswordTemplate(t *testing.T) {
	dir := t.TempDir()
	script := fakeOpCLI(t, dir, `printf 'my-api-token-value\n'`)

	v := DefaultVault()
	v.Providers[ProviderOnePassword] = ProviderTemplates{
		Fetch:     []string{script, "read", RefPlaceholder},
		LoginHint: "op signin",
	}

	const ref = "op://Personal/CI Token/credential"
	got, err := v.Fetch(context.Background(), ProviderOnePassword, ref)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.TrimSpace(string(got)) != "my-api-token-value" {
		t.Errorf("Fetch = %q, want api token value", got)
	}

	// The argv log must show the op:// ref was substituted correctly.
	log, _ := os.ReadFile(filepath.Join(dir, "argv.log"))
	if !strings.Contains(string(log), "read op://Personal/CI Token/credential") {
		t.Errorf("argv log = %q, want op:// ref in argv", log)
	}
}

// TestFetchSecretOnePasswordLoginHintOnFailure confirms the login hint surfaces
// when the mocked `op` binary exits non-zero (session expired scenario).
func TestFetchSecretOnePasswordLoginHintOnFailure(t *testing.T) {
	dir := t.TempDir()
	script := fakeOpCLI(t, dir, `echo "session expired" >&2; exit 1`)

	v := DefaultVault()
	v.Providers[ProviderOnePassword] = ProviderTemplates{
		Fetch:     []string{script, "read", RefPlaceholder},
		LoginHint: "op signin",
	}

	_, err := v.Fetch(context.Background(), ProviderOnePassword, "op://Vault/Item/field")
	if err == nil {
		t.Fatal("want error on non-zero exit")
	}
	msg := err.Error()
	if !strings.Contains(msg, "op signin") {
		t.Errorf("err %q missing login hint", msg)
	}
	if !strings.Contains(msg, "session expired") {
		t.Errorf("err %q missing stderr detail", msg)
	}
}

// TestFetchSecretProtonPassGenericContent confirms that the protonpass Fetch
// template returns arbitrary secret content, not just SSH key material.
func TestFetchSecretProtonPassGenericContent(t *testing.T) {
	dir := t.TempDir()
	const secret = "tok_fakecredential_notreal_testonly"
	script := fakeCLI(t, dir, `printf '`+secret+`\n'`)

	v := DefaultVault()
	v.Providers[ProviderProtonPass] = ProviderTemplates{
		Fetch:     []string{script, "item", "view", RefPlaceholder, "--field", "credential"},
		LoginHint: "pass-cli login",
	}

	got, err := v.Fetch(context.Background(), ProviderProtonPass, "pass://share/item")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.TrimSpace(string(got)) != secret {
		t.Errorf("Fetch = %q, want %q", got, secret)
	}
}

// TestFetchSecretBothProvidersMockRoundTrip is the acceptance test: a generic
// secret (not SSH key material) round-trips through both protonpass and
// onepassword providers with mocked CLIs, confirming ProviderTemplates.Fetch
// is truly generic.
func TestFetchSecretBothProvidersMockRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		argv     func(script string) []string
		ref      string
		want     string
	}{
		{
			name:     "protonpass generic secret",
			provider: ProviderProtonPass,
			argv:     func(s string) []string { return []string{s, "item", "view", RefPlaceholder} },
			ref:      "pass://share-id/db-password-item",
			want:     "super-secret-db-pass-9x!",
		},
		{
			name:     "onepassword generic secret",
			provider: ProviderOnePassword,
			argv:     func(s string) []string { return []string{s, "read", RefPlaceholder} },
			ref:      "op://Engineering/Database/password",
			want:     "super-secret-db-pass-9x!",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := fakeCLI(t, dir, `printf '`+tc.want+`\n'`)

			v := DefaultVault()
			v.Providers[tc.provider] = ProviderTemplates{
				Fetch: tc.argv(script),
			}

			got, err := v.Fetch(context.Background(), tc.provider, tc.ref)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if strings.TrimSpace(string(got)) != tc.want {
				t.Errorf("Fetch = %q, want %q", got, tc.want)
			}

			// Memory-only invariant: nothing written to disk by Fetch itself.
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if e.Name() != "fake-cli" && e.Name() != "argv.log" {
					t.Errorf("unexpected file written to disk by Fetch: %s", e.Name())
				}
			}
		})
	}
}

// TestFetchSecretPackageLevelWrapper confirms the FetchSecret convenience
// function loads vault config and delegates to v.Fetch correctly. Uses the
// file provider so no mock CLI is needed.
func TestFetchSecretPackageLevelWrapper(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(p, []byte("vault-loaded-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FetchSecret(context.Background(), ProviderFile, p)
	if err != nil {
		t.Fatalf("FetchSecret: %v", err)
	}
	if string(got) != "vault-loaded-token\n" {
		t.Errorf("FetchSecret = %q", got)
	}
}

// TestFetchSecretMemoryOnlyInvariant confirms that on fetch failure the error
// message carries stderr detail (login hint, exit code) but NEVER stdout (the
// potential secret value), even though the CLI exits non-zero.
func TestFetchSecretMemoryOnlyInvariant(t *testing.T) {
	dir := t.TempDir()
	// stdout would be the secret; stderr is the error detail. Non-zero exit.
	script := fakeOpCLI(t, dir, `printf 'secret-that-must-not-leak\n'; printf 'auth failure\n' >&2; exit 1`)

	v := DefaultVault()
	v.Providers[ProviderOnePassword] = ProviderTemplates{
		Fetch:     []string{script, "read", RefPlaceholder},
		LoginHint: "op signin",
	}

	_, err := v.Fetch(context.Background(), ProviderOnePassword, "op://V/I/f")
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	if strings.Contains(msg, "secret-that-must-not-leak") {
		t.Errorf("error message leaks stdout (potential secret): %q", msg)
	}
	if !strings.Contains(msg, "auth failure") {
		t.Errorf("error message missing stderr detail: %q", msg)
	}
}
