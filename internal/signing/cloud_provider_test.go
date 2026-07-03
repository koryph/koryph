// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestDefaultVaultCloudProviderTemplates asserts that the three cloud CLI
// providers are present in DefaultVault() with the exact argv shapes the task
// specifies. Changing these shapes requires updating both the defaults and the
// user-facing signing.md documentation, so pinning them here acts as a drift
// guard analogous to TestDefaultVaultTemplates for protonpass/onepassword.
func TestDefaultVaultCloudProviderTemplates(t *testing.T) {
	v := DefaultVault()

	t.Run("aws_secretsmanager", func(t *testing.T) {
		pt, ok := v.Providers[ProviderAWSSecretsManager]
		if !ok {
			t.Fatal("ProviderAWSSecretsManager missing from DefaultVault")
		}
		want := []string{
			"aws", "secretsmanager", "get-secret-value",
			"--secret-id", "{ref}",
			"--query", "SecretString",
			"--output", "text",
		}
		if !reflect.DeepEqual(pt.Fetch, want) {
			t.Errorf("aws_secretsmanager fetch = %v\nwant                    %v", pt.Fetch, want)
		}
		if pt.LoginHint == "" {
			t.Errorf("aws_secretsmanager should have a non-empty login_hint")
		}
	})

	t.Run("azure_keyvault", func(t *testing.T) {
		pt, ok := v.Providers[ProviderAzureKeyVault]
		if !ok {
			t.Fatal("ProviderAzureKeyVault missing from DefaultVault")
		}
		want := []string{
			"az", "keyvault", "secret", "show",
			"--id", "{ref}",
			"--query", "value",
			"-o", "tsv",
		}
		if !reflect.DeepEqual(pt.Fetch, want) {
			t.Errorf("azure_keyvault fetch = %v\nwant                  %v", pt.Fetch, want)
		}
		if pt.LoginHint == "" {
			t.Errorf("azure_keyvault should have a non-empty login_hint")
		}
	})

	t.Run("gcp_secretmanager", func(t *testing.T) {
		pt, ok := v.Providers[ProviderGCPSecretManager]
		if !ok {
			t.Fatal("ProviderGCPSecretManager missing from DefaultVault")
		}
		want := []string{
			"gcloud", "secrets", "versions", "access", "latest",
			"--secret", "{ref}",
		}
		if !reflect.DeepEqual(pt.Fetch, want) {
			t.Errorf("gcp_secretmanager fetch = %v\nwant                   %v", pt.Fetch, want)
		}
		if pt.LoginHint == "" {
			t.Errorf("gcp_secretmanager should have a non-empty login_hint")
		}
	})
}

// TestCloudProviderArgvRendering verifies that ExpandArgv substitutes {ref}
// correctly in each cloud provider's default fetch template, producing the
// exact argv that would be exec'd by the ambient cloud CLI. This is the
// "argv rendering" acceptance criterion from the task spec.
func TestCloudProviderArgvRendering(t *testing.T) {
	v := DefaultVault()

	cases := []struct {
		provider     string
		ref          string
		wantContains []string // substrings that must appear as exact tokens
	}{
		{
			provider: ProviderAWSSecretsManager,
			ref:      "arn:aws:secretsmanager:us-east-1:123456789012:secret:my-secret",
			wantContains: []string{
				"aws",
				"secretsmanager",
				"get-secret-value",
				"--secret-id",
				"arn:aws:secretsmanager:us-east-1:123456789012:secret:my-secret",
				"--query", "SecretString",
				"--output", "text",
			},
		},
		{
			provider: ProviderAzureKeyVault,
			ref:      "https://my-vault.vault.azure.net/secrets/my-secret",
			wantContains: []string{
				"az",
				"keyvault",
				"secret",
				"show",
				"--id",
				"https://my-vault.vault.azure.net/secrets/my-secret",
				"--query", "value",
				"-o", "tsv",
			},
		},
		{
			provider: ProviderGCPSecretManager,
			ref:      "projects/my-project/secrets/my-secret",
			wantContains: []string{
				"gcloud",
				"secrets",
				"versions",
				"access",
				"latest",
				"--secret",
				"projects/my-project/secrets/my-secret",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			pt := v.Providers[tc.provider]
			argv := ExpandArgv(pt.Fetch, tc.ref)

			// Build a quick lookup set from the expanded argv.
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

			// Ensure the raw placeholder is gone.
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

// TestCloudProviderFetchMockedCLI is an end-to-end round-trip for each cloud
// provider: the real Fetch() path is exercised with a mocked CLI that echoes
// a canned secret, confirming the template is wired through execx correctly.
func TestCloudProviderFetchMockedCLI(t *testing.T) {
	const secret = "cloudprovider-secret-value-testonly"

	cases := []struct {
		provider string
		ref      string
		argv     func(script string) []string // override fetch template with script
	}{
		{
			provider: ProviderAWSSecretsManager,
			ref:      "arn:aws:secretsmanager:us-east-1:000000000000:secret:ci-token",
			argv: func(s string) []string {
				return []string{
					s, "secretsmanager", "get-secret-value",
					"--secret-id", RefPlaceholder,
					"--query", "SecretString",
					"--output", "text",
				}
			},
		},
		{
			provider: ProviderAzureKeyVault,
			ref:      "https://myvault.vault.azure.net/secrets/ci-token",
			argv: func(s string) []string {
				return []string{
					s, "keyvault", "secret", "show",
					"--id", RefPlaceholder,
					"--query", "value",
					"-o", "tsv",
				}
			},
		},
		{
			provider: ProviderGCPSecretManager,
			ref:      "projects/my-project/secrets/ci-token",
			argv: func(s string) []string {
				return []string{
					s, "secrets", "versions", "access", "latest",
					"--secret", RefPlaceholder,
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			dir := t.TempDir()
			script := fakeCLI(t, dir, `printf '`+secret+`\n'`)

			v := DefaultVault()
			v.Providers[tc.provider] = ProviderTemplates{
				Fetch: tc.argv(script),
			}

			got, err := v.Fetch(context.Background(), tc.provider, tc.ref)
			if err != nil {
				t.Fatalf("Fetch(%s): %v", tc.provider, err)
			}
			if strings.TrimSpace(string(got)) != secret {
				t.Errorf("Fetch(%s) = %q, want %q", tc.provider, got, secret)
			}

			// Verify the ref was substituted in the argv log.
			log := argvLog(t, dir)
			if !strings.Contains(log, tc.ref) {
				t.Errorf("argv log = %q — ref %q not substituted", log, tc.ref)
			}
		})
	}
}

// TestCloudProviderLoginHintOnFailure confirms the login hint surfaces when
// the mocked CLI exits non-zero (expired credentials scenario).
func TestCloudProviderLoginHintOnFailure(t *testing.T) {
	cases := []struct {
		provider string
		hintFrag string
	}{
		{ProviderAWSSecretsManager, "aws configure"},
		{ProviderAzureKeyVault, "az login"},
		{ProviderGCPSecretManager, "gcloud auth login"},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			dir := t.TempDir()
			script := fakeCLI(t, dir, `echo "credentials expired" >&2; exit 1`)

			v := DefaultVault()
			// Override only the first token (the binary) so the template shape
			// is real but routes through our fake script.
			pt := v.Providers[tc.provider]
			pt.Fetch[0] = script
			v.Providers[tc.provider] = pt

			_, err := v.Fetch(context.Background(), tc.provider, "some-ref")
			if err == nil {
				t.Fatal("want error on non-zero CLI exit")
			}
			if !strings.Contains(err.Error(), tc.hintFrag) {
				t.Errorf("err %q missing login hint %q", err.Error(), tc.hintFrag)
			}
		})
	}
}

// TestCloudProviderValidateAccepted confirms Config.Validate() accepts all
// three cloud provider names without error.
func TestCloudProviderValidateAccepted(t *testing.T) {
	providers := []string{
		ProviderAWSSecretsManager,
		ProviderAzureKeyVault,
		ProviderGCPSecretManager,
	}
	for _, p := range providers {
		cfg := Config{Mode: ModeGitsign, Provider: p}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate with provider=%q: unexpected error: %v", p, err)
		}
	}
}
