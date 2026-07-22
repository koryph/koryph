// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package account

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/signing"
)

func TestFingerprint(t *testing.T) {
	fp := Fingerprint("sk-ant-super-secret-value")
	if !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("Fingerprint = %q, want sha256: prefix", fp)
	}
	if strings.Contains(fp, "sk-ant-super-secret-value") {
		t.Fatalf("Fingerprint leaks the credential: %q", fp)
	}
	// Deterministic and short (a lookup/display signal, not the secret).
	if got, want := Fingerprint("sk-ant-super-secret-value"), fp; got != want {
		t.Errorf("Fingerprint not deterministic: %q != %q", got, want)
	}
	if Fingerprint("a-different-secret") == fp {
		t.Error("two different credentials fingerprinted identically")
	}
}

func TestResolveCredentialSubscriptionRefused(t *testing.T) {
	if _, _, err := ResolveCredential(context.Background(), AuthModeSubscription, nil); err == nil {
		t.Fatal("ResolveCredential succeeded for AuthModeSubscription; want error (nothing to resolve)")
	}
}

func TestResolveCredentialNilCredential(t *testing.T) {
	if _, _, err := ResolveCredential(context.Background(), AuthModeAPIKey, nil); err == nil {
		t.Fatal("ResolveCredential succeeded with a nil Credential; want error")
	}
}

// TestResolveCredentialEnvSource proves the env-source path reads the named
// var and returns the canonical injection name for each mode.
func TestResolveCredentialEnvSource(t *testing.T) {
	cases := []struct {
		mode       AuthMode
		wantEnvVar string
	}{
		{AuthModeAPIKey, "ANTHROPIC_API_KEY"},
		{AuthModeOAuthToken, "CLAUDE_CODE_OAUTH_TOKEN"},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Setenv("KORYPH_TEST_CRED_VAR", "resolved-secret-value")
			envVar, value, err := ResolveCredential(context.Background(), tc.mode, &Credential{
				Source: CredentialSourceEnv,
				EnvVar: "KORYPH_TEST_CRED_VAR",
			})
			if err != nil {
				t.Fatalf("ResolveCredential: %v", err)
			}
			if envVar != tc.wantEnvVar {
				t.Errorf("envVar = %q, want %q", envVar, tc.wantEnvVar)
			}
			if value != "resolved-secret-value" {
				t.Errorf("value = %q, want resolved-secret-value", value)
			}
		})
	}
}

// TestResolveCredentialEnvSourceRefusesCanonicalNames is the anti-footgun
// (design I2/§6, mirrors internal/anthro.NewClient's ambient-key refusal):
// naming the SOURCE var one of the canonical INJECTED names must be refused,
// or a dispatched agent's own ambient env would satisfy its own credential
// lookup, defeating the vault/named-var indirection entirely.
func TestResolveCredentialEnvSourceRefusesCanonicalNames(t *testing.T) {
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Run(forbidden, func(t *testing.T) {
			// Even if ambiently set to a "valid-looking" value, ResolveCredential
			// must refuse the NAME outright, never reaching os.Getenv for it.
			t.Setenv(forbidden, "sk-ambient-should-never-be-read")
			_, _, err := ResolveCredential(context.Background(), AuthModeAPIKey, &Credential{
				Source: CredentialSourceEnv,
				EnvVar: forbidden,
			})
			if err == nil {
				t.Fatalf("ResolveCredential succeeded using canonical name %q as the source var; want refusal", forbidden)
			}
		})
	}
}

func TestResolveCredentialEnvSourceEmptyValue(t *testing.T) {
	t.Setenv("KORYPH_TEST_CRED_EMPTY", "")
	_, _, err := ResolveCredential(context.Background(), AuthModeAPIKey, &Credential{
		Source: CredentialSourceEnv,
		EnvVar: "KORYPH_TEST_CRED_EMPTY",
	})
	if err == nil {
		t.Fatal("ResolveCredential succeeded with an empty env value; want error")
	}
}

func TestResolveCredentialEnvSourceMissingEnvVarName(t *testing.T) {
	_, _, err := ResolveCredential(context.Background(), AuthModeAPIKey, &Credential{Source: CredentialSourceEnv})
	if err == nil {
		t.Fatal("ResolveCredential succeeded with an empty env_var name; want error")
	}
}

// TestResolveCredentialVaultSource exercises the vault path end-to-end using
// signing's "file" provider (a plaintext file read, no external vault
// process needed) — the same signing.FetchSecret seam internal/bot already
// uses in production for a GitLab token / GitHub App key.
func TestResolveCredentialVaultSource(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "anthropic-api-key")
	if err := os.WriteFile(secretPath, []byte("sk-ant-from-vault\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	envVar, value, err := ResolveCredential(context.Background(), AuthModeAPIKey, &Credential{
		Source:   CredentialSourceVault,
		Provider: signing.ProviderFile,
		KeyRef:   secretPath,
	})
	if err != nil {
		t.Fatalf("ResolveCredential: %v", err)
	}
	if envVar != "ANTHROPIC_API_KEY" {
		t.Errorf("envVar = %q, want ANTHROPIC_API_KEY", envVar)
	}
	// signing.Fetch does not trim; ResolveCredential does — a trailing
	// newline in the secret file must not become part of the credential.
	if value != "sk-ant-from-vault" {
		t.Errorf("value = %q, want sk-ant-from-vault (trimmed)", value)
	}
}

func TestResolveCredentialVaultSourceMissingRef(t *testing.T) {
	_, _, err := ResolveCredential(context.Background(), AuthModeAPIKey, &Credential{
		Source:   CredentialSourceVault,
		Provider: signing.ProviderFile,
	})
	if err == nil {
		t.Fatal("ResolveCredential succeeded with an empty key_ref; want error")
	}
}

func TestResolveCredentialUnknownSource(t *testing.T) {
	_, _, err := ResolveCredential(context.Background(), AuthModeAPIKey, &Credential{Source: "carrier-pigeon"})
	if err == nil {
		t.Fatal("ResolveCredential succeeded with an unknown source; want error")
	}
}
