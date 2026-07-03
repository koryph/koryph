// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestDefaultVaultLocalProviderTemplates asserts that KeePassXC, OpenBao, and
// HashiCorp Vault are present in DefaultVault() with the exact argv shapes
// defined in the task spec. Changing these shapes requires updating both the
// defaults and the user-facing signing.md documentation, so pinning them here
// acts as a drift guard analogous to TestDefaultVaultCloudProviderTemplates.
func TestDefaultVaultLocalProviderTemplates(t *testing.T) {
	v := DefaultVault()

	t.Run("keepassxc", func(t *testing.T) {
		pt, ok := v.Providers[ProviderKeePassXC]
		if !ok {
			t.Fatal("ProviderKeePassXC missing from DefaultVault")
		}
		want := []string{
			"keepassxc-cli", "show",
			"--key-file", "/path/to/database.keyx",
			"--attributes", "Password",
			"/path/to/database.kdbx",
			"{ref}",
		}
		if !reflect.DeepEqual(pt.Fetch, want) {
			t.Errorf("keepassxc fetch = %v\nwant             %v", pt.Fetch, want)
		}
		if pt.LoginHint == "" {
			t.Errorf("keepassxc should have a non-empty login_hint")
		}
		// The login hint must mention the key-file path so operators know
		// they need to configure it.
		if !strings.Contains(pt.LoginHint, "key-file") {
			t.Errorf("keepassxc login_hint %q should mention --key-file constraint", pt.LoginHint)
		}
	})

	t.Run("openbao", func(t *testing.T) {
		pt, ok := v.Providers[ProviderOpenBao]
		if !ok {
			t.Fatal("ProviderOpenBao missing from DefaultVault")
		}
		want := []string{"bao", "kv", "get", "-field=value", "{ref}"}
		if !reflect.DeepEqual(pt.Fetch, want) {
			t.Errorf("openbao fetch = %v\nwant            %v", pt.Fetch, want)
		}
		if pt.LoginHint == "" {
			t.Errorf("openbao should have a non-empty login_hint")
		}
	})

	t.Run("vault", func(t *testing.T) {
		pt, ok := v.Providers[ProviderHashiCorpVault]
		if !ok {
			t.Fatal("ProviderHashiCorpVault missing from DefaultVault")
		}
		want := []string{"vault", "kv", "get", "-field=value", "{ref}"}
		if !reflect.DeepEqual(pt.Fetch, want) {
			t.Errorf("vault fetch = %v\nwant          %v", pt.Fetch, want)
		}
		if pt.LoginHint == "" {
			t.Errorf("vault should have a non-empty login_hint")
		}
	})
}

// TestLocalProviderArgvRendering verifies that ExpandArgv substitutes {ref}
// correctly in each new provider's default fetch template, producing the exact
// argv that would be exec'd by the local CLI. This is the "argv rendering"
// acceptance criterion from the task spec.
func TestLocalProviderArgvRendering(t *testing.T) {
	v := DefaultVault()

	cases := []struct {
		provider     string
		ref          string
		wantContains []string // tokens that must appear in the expanded argv
	}{
		{
			provider: ProviderKeePassXC,
			ref:      "Engineering/GitHub Token",
			wantContains: []string{
				"keepassxc-cli",
				"show",
				"--key-file", "/path/to/database.keyx",
				"--attributes", "Password",
				"/path/to/database.kdbx",
				"Engineering/GitHub Token",
			},
		},
		{
			provider: ProviderOpenBao,
			ref:      "secret/myapp",
			wantContains: []string{
				"bao",
				"kv",
				"get",
				"-field=value",
				"secret/myapp",
			},
		},
		{
			provider: ProviderHashiCorpVault,
			ref:      "secret/myapp",
			wantContains: []string{
				"vault",
				"kv",
				"get",
				"-field=value",
				"secret/myapp",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			pt := v.Providers[tc.provider]
			argv := ExpandArgv(pt.Fetch, tc.ref)

			argSet := make(map[string]bool, len(argv))
			for _, a := range argv {
				argSet[a] = true
			}

			for _, want := range tc.wantContains {
				if !argSet[want] {
					t.Errorf("ExpandArgv(%q, %q) = %v\nmissing token %q",
						tc.provider, tc.ref, argv, want)
				}
			}

			// Ensure the raw placeholder is gone after expansion.
			for _, tok := range argv {
				if strings.Contains(tok, RefPlaceholder) {
					t.Errorf("argv token %q still contains placeholder after expansion", tok)
				}
			}

			// Ensure the ref appears exactly once (substituted, not duplicated).
			count := 0
			for _, tok := range argv {
				if tok == tc.ref {
					count++
				}
			}
			if count != 1 {
				t.Errorf("ref %q appears %d times in argv (want 1): %v", tc.ref, count, argv)
			}
		})
	}
}

// TestLocalProviderFetchMockedCLI is an end-to-end round-trip for each new
// provider: the real Fetch() path is exercised with a mocked CLI that prints
// a canned secret, confirming the template is wired through execx correctly.
func TestLocalProviderFetchMockedCLI(t *testing.T) {
	const secret = "localprovider-secret-value-testonly"

	cases := []struct {
		provider string
		ref      string
		argv     func(script string) []string
	}{
		{
			// keepassxc-cli show — the database and key-file paths are replaced by
			// the script; {ref} carries the entry path.
			provider: ProviderKeePassXC,
			ref:      "Engineering/SSH Signing Key",
			argv: func(s string) []string {
				return []string{
					s, "show",
					"--key-file", "/path/to/database.keyx",
					"--attributes", "Password",
					"/path/to/database.kdbx",
					RefPlaceholder,
				}
			},
		},
		{
			provider: ProviderOpenBao,
			ref:      "secret/koryph/signing-key",
			argv: func(s string) []string {
				return []string{s, "kv", "get", "-field=value", RefPlaceholder}
			},
		},
		{
			provider: ProviderHashiCorpVault,
			ref:      "secret/koryph/signing-key",
			argv: func(s string) []string {
				return []string{s, "kv", "get", "-field=value", RefPlaceholder}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			dir := t.TempDir()
			script := fakeCLI(t, dir, `printf '`+secret+`\n'`)

			vlt := DefaultVault()
			vlt.Providers[tc.provider] = ProviderTemplates{
				Fetch: tc.argv(script),
			}

			got, err := vlt.Fetch(context.Background(), tc.provider, tc.ref)
			if err != nil {
				t.Fatalf("Fetch(%s): %v", tc.provider, err)
			}
			if strings.TrimSpace(string(got)) != secret {
				t.Errorf("Fetch(%s) = %q, want %q", tc.provider, got, secret)
			}

			// Verify the entry ref was substituted into argv.
			log := argvLog(t, dir)
			if !strings.Contains(log, tc.ref) {
				t.Errorf("argv log = %q — ref %q not substituted", log, tc.ref)
			}
		})
	}
}

// TestLocalProviderLoginHintOnFailure confirms the login hint surfaces when the
// mocked CLI exits non-zero (expired credentials / locked database scenario).
func TestLocalProviderLoginHintOnFailure(t *testing.T) {
	cases := []struct {
		provider string
		hintFrag string
	}{
		{ProviderKeePassXC, "key-file"},
		{ProviderOpenBao, "bao login"},
		{ProviderHashiCorpVault, "vault login"},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			dir := t.TempDir()
			script := fakeCLI(t, dir, `echo "authentication failed" >&2; exit 1`)

			vlt := DefaultVault()
			pt := vlt.Providers[tc.provider]
			pt.Fetch[0] = script
			vlt.Providers[tc.provider] = pt

			_, err := vlt.Fetch(context.Background(), tc.provider, "some-ref")
			if err == nil {
				t.Fatal("want error on non-zero CLI exit")
			}
			if !strings.Contains(err.Error(), tc.hintFrag) {
				t.Errorf("err %q missing login hint fragment %q", err.Error(), tc.hintFrag)
			}
		})
	}
}

// TestLocalProviderValidateAccepted confirms Config.Validate() accepts all
// three new provider names without error.
func TestLocalProviderValidateAccepted(t *testing.T) {
	providers := []string{
		ProviderKeePassXC,
		ProviderOpenBao,
		ProviderHashiCorpVault,
	}
	for _, p := range providers {
		cfg := Config{Mode: ModeGitsign, Provider: p}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate with provider=%q: unexpected error: %v", p, err)
		}
	}
}

// TestLocalProviderValidateRejectsUnknown confirms that a typo in the provider
// name (e.g. "keepass" instead of "keepassxc") is caught by Validate().
func TestLocalProviderValidateRejectsUnknown(t *testing.T) {
	unknowns := []string{"keepass", "hashicorp-vault", "openbao-hcp", "bao"}
	for _, p := range unknowns {
		cfg := Config{Mode: ModeGitsign, Provider: p}
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate with provider=%q: want error for unknown provider", p)
		}
	}
}

// TestKeePassXCAttachmentExportArgvShape verifies that a user-supplied
// attachment-export template (the recommended pattern for SSH private keys
// stored as KeePassXC file attachments) expands {ref} correctly when the
// operator overrides fetch in vault.json.
func TestKeePassXCAttachmentExportArgvShape(t *testing.T) {
	attachExportTemplate := []string{
		"keepassxc-cli", "attachment-export",
		"--key-file", "/path/to/database.keyx",
		"/path/to/database.kdbx",
		RefPlaceholder, // entry path
		"private_key",  // attachment name
		"-",            // output to stdout
	}
	ref := "SSH Keys/Koryph Signing Key"
	got := ExpandArgv(attachExportTemplate, ref)

	want := []string{
		"keepassxc-cli", "attachment-export",
		"--key-file", "/path/to/database.keyx",
		"/path/to/database.kdbx",
		"SSH Keys/Koryph Signing Key",
		"private_key",
		"-",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attachment-export argv =\n  %v\nwant\n  %v", got, want)
	}
	// Placeholder must be gone.
	for _, tok := range got {
		if strings.Contains(tok, RefPlaceholder) {
			t.Errorf("placeholder still present in token %q", tok)
		}
	}
}
