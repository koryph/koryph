// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/registry"
)

func TestCheckAuthModeNoRecords(t *testing.T) {
	o := Options{RegistryList: func() ([]*registry.Record, error) { return nil, nil }}
	fs := checkAuthMode(o)
	if len(fs) != 0 {
		t.Errorf("no-record case: want 0 findings, got %d: %v", len(fs), fs)
	}
}

func TestCheckAuthModeRegistryError(t *testing.T) {
	o := Options{RegistryList: func() ([]*registry.Record, error) { return nil, fmt.Errorf("registry exploded") }}
	fs := checkAuthMode(o)
	if len(fs) != 1 || fs[0].Level != LevelWarn {
		t.Errorf("registry error: want 1 WARN, got %v", fs)
	}
}

func TestCheckAuthModeSubscriptionDefault(t *testing.T) {
	// Empty AuthMode means subscription — the pre-koryph-i3b common case.
	rec := &registry.Record{ProjectID: "proj-a"}
	f := checkOneAccountAuth(rec)
	if f.Level != LevelOK {
		t.Fatalf("want OK, got %v: %s", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "auth_mode=subscription") {
		t.Errorf("message %q missing auth_mode=subscription", f.Message)
	}
}

func TestCheckAuthModeAPIKeyVaultReportsFieldsNotSecret(t *testing.T) {
	rec := &registry.Record{
		ProjectID: "proj-b",
		AuthMode:  registry.AuthModeAPIKey,
		Credential: &registry.Credential{
			Source:   registry.CredentialSourceVault,
			Provider: "proton-pass",
			KeyRef:   "koryph/anthropic-api-key",
		},
		IdentityFingerprint: "sha256:deadbeefcafef00d",
	}
	f := checkOneAccountAuth(rec)
	if f.Level != LevelOK {
		t.Fatalf("want OK, got %v: %s", f.Level, f.Message)
	}
	for _, want := range []string{
		"auth_mode=api-key",
		"credential_source=vault",
		"provider=proton-pass",
		"key_ref=koryph/anthropic-api-key",
		"fingerprint=sha256:deadbeefcafef00d",
	} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message %q missing %q", f.Message, want)
		}
	}
}

func TestCheckAuthModeOAuthTokenEnvReportsEnvVarNameNotSecret(t *testing.T) {
	const secretValue = "sk-ant-super-secret-token-do-not-print"
	rec := &registry.Record{
		ProjectID: "proj-c",
		AuthMode:  registry.AuthModeOAuthToken,
		Credential: &registry.Credential{
			Source: registry.CredentialSourceEnv,
			EnvVar: "KORYPH_OAUTH_TOKEN_SRC",
		},
		IdentityFingerprint: "sha256:0123456789abcdef",
	}
	f := checkOneAccountAuth(rec)
	if f.Level != LevelOK {
		t.Fatalf("want OK, got %v: %s", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "env_var=KORYPH_OAUTH_TOKEN_SRC") {
		t.Errorf("message %q missing env_var name", f.Message)
	}
	if strings.Contains(f.Message, secretValue) {
		t.Errorf("message leaked secret value: %q", f.Message)
	}
}

func TestCheckAuthModeMissingCredentialErrors(t *testing.T) {
	rec := &registry.Record{ProjectID: "proj-d", AuthMode: registry.AuthModeAPIKey}
	f := checkOneAccountAuth(rec)
	if f.Level != LevelError {
		t.Fatalf("want ERROR, got %v: %s", f.Level, f.Message)
	}
}

func TestCheckAuthModeMissingFingerprintWarns(t *testing.T) {
	rec := &registry.Record{
		ProjectID: "proj-e",
		AuthMode:  registry.AuthModeAPIKey,
		Credential: &registry.Credential{
			Source: registry.CredentialSourceEnv,
			EnvVar: "KORYPH_API_KEY_SRC",
		},
	}
	f := checkOneAccountAuth(rec)
	if f.Level != LevelWarn {
		t.Fatalf("want WARN, got %v: %s", f.Level, f.Message)
	}
}

func TestCheckAuthModeAccountLabelIncludesProfile(t *testing.T) {
	rec := &registry.Record{ProjectID: "proj-f", AccountProfile: "work"}
	f := checkOneAccountAuth(rec)
	if !strings.Contains(f.Message, "proj-f (account work)") {
		t.Errorf("message %q missing project+profile label", f.Message)
	}
}

func TestCheckAuthModeIntegratedViaRun(t *testing.T) {
	o := Options{
		RegistryList: func() ([]*registry.Record, error) {
			return []*registry.Record{
				{ProjectID: "proj-g", AuthMode: registry.AuthModeAPIKey, Credential: &registry.Credential{
					Source: registry.CredentialSourceEnv, EnvVar: "KORYPH_API_KEY_SRC",
				}, IdentityFingerprint: "sha256:1111111111111111"},
			}, nil
		},
	}
	fs := checkAuthMode(o)
	if len(fs) != 1 || fs[0].Check != checkNameAuth {
		t.Fatalf("want 1 auth-mode finding, got %v", fs)
	}
}

func TestCheckRuntimeAccountsAuthNoDivergenceIsSilent(t *testing.T) {
	// A runtime_accounts entry that resolves identically to the record's
	// flat fields carries no new information — it should not be reported
	// again.
	rec := &registry.Record{
		ProjectID: "proj-h",
		AuthMode:  registry.AuthModeAPIKey,
		Credential: &registry.Credential{
			Source: registry.CredentialSourceEnv,
			EnvVar: "KORYPH_API_KEY_SRC",
		},
		IdentityFingerprint: "sha256:2222222222222222",
		RuntimeAccounts: map[string]registry.RuntimeAccount{
			"claude": {
				AuthMode: registry.AuthModeAPIKey,
				Credential: &registry.Credential{
					Source: registry.CredentialSourceEnv,
					EnvVar: "KORYPH_API_KEY_SRC",
				},
				IdentityFingerprint: "sha256:2222222222222222",
			},
		},
	}
	fs := checkRuntimeAccountsAuth(rec)
	if len(fs) != 0 {
		t.Errorf("non-diverging runtime entry: want 0 findings, got %d: %v", len(fs), fs)
	}
}

func TestCheckRuntimeAccountsAuthDivergingCredentialReported(t *testing.T) {
	rec := &registry.Record{
		ProjectID:           "proj-i",
		AuthMode:            registry.AuthModeSubscription,
		IdentityFingerprint: "",
		RuntimeAccounts: map[string]registry.RuntimeAccount{
			"claude": {
				AuthMode: registry.AuthModeAPIKey,
				Credential: &registry.Credential{
					Source:   registry.CredentialSourceVault,
					Provider: "proton-pass",
					KeyRef:   "koryph/claude-key",
				},
				IdentityFingerprint: "sha256:3333333333333333",
			},
		},
	}
	fs := checkRuntimeAccountsAuth(rec)
	if len(fs) != 1 {
		t.Fatalf("diverging runtime entry: want 1 finding, got %d: %v", len(fs), fs)
	}
	f := fs[0]
	if f.Level != LevelOK {
		t.Fatalf("want OK, got %v: %s", f.Level, f.Message)
	}
	for _, want := range []string{
		"proj-i (runtime claude)",
		"auth_mode=api-key",
		"credential_source=vault",
		"provider=proton-pass",
		"fingerprint=sha256:3333333333333333",
	} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message %q missing %q", f.Message, want)
		}
	}
}

func TestCheckRuntimeAccountsAuthSortedByName(t *testing.T) {
	rec := &registry.Record{
		ProjectID: "proj-j",
		AuthMode:  registry.AuthModeSubscription,
		RuntimeAccounts: map[string]registry.RuntimeAccount{
			"zeta":  {AuthMode: registry.AuthModeAPIKey, Credential: &registry.Credential{Source: registry.CredentialSourceEnv, EnvVar: "Z_KEY"}},
			"alpha": {AuthMode: registry.AuthModeAPIKey, Credential: &registry.Credential{Source: registry.CredentialSourceEnv, EnvVar: "A_KEY"}},
		},
	}
	fs := checkRuntimeAccountsAuth(rec)
	if len(fs) != 2 {
		t.Fatalf("want 2 findings, got %d: %v", len(fs), fs)
	}
	if !strings.Contains(fs[0].Message, "runtime alpha") || !strings.Contains(fs[1].Message, "runtime zeta") {
		t.Errorf("want alpha before zeta, got %q then %q", fs[0].Message, fs[1].Message)
	}
}

func TestCheckAuthModeIntegratedIncludesDivergingRuntime(t *testing.T) {
	o := Options{
		RegistryList: func() ([]*registry.Record, error) {
			return []*registry.Record{
				{
					ProjectID: "proj-k",
					AuthMode:  registry.AuthModeSubscription,
					RuntimeAccounts: map[string]registry.RuntimeAccount{
						"claude": {
							AuthMode: registry.AuthModeAPIKey,
							Credential: &registry.Credential{
								Source: registry.CredentialSourceEnv,
								EnvVar: "KORYPH_API_KEY_SRC",
							},
							IdentityFingerprint: "sha256:4444444444444444",
						},
					},
				},
			}, nil
		},
	}
	fs := checkAuthMode(o)
	if len(fs) != 2 {
		t.Fatalf("want 2 findings (base + diverging runtime), got %d: %v", len(fs), fs)
	}
}
