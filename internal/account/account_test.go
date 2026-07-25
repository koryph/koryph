// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package account

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envLookup returns the value of key in env plus how many times it appears.
func envLookup(env []string, key string) (string, int) {
	var value string
	count := 0
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value = strings.TrimPrefix(kv, prefix)
			count++
		}
	}
	return value, count
}

func TestChildEnvMatrix(t *testing.T) {
	// Pollute the parent env: ChildEnv must never source these from the ambient
	// environment — CLAUDE_CONFIG_DIR/ANTHROPIC_API_KEY are injected explicitly.
	t.Setenv("CLAUDE_CONFIG_DIR", "/polluted/parent/claude")
	t.Setenv("ANTHROPIC_API_KEY", "sk-polluted-parent-key")

	personal := Profile{Name: "personal", ConfigDir: ""}
	work := Profile{Name: "work", ConfigDir: "/home/u/.claude-work"}

	cases := []struct {
		name          string
		profile       Profile
		billing       BillingMode
		apiKey        string
		wantConfigDir string // "" = must be absent
		wantAPIKey    string // "" = must be absent
	}{
		{"personal+subscription", personal, BillingSubscription, "", "", ""},
		{"work+subscription", work, BillingSubscription, "", work.ConfigDir, ""},
		{"personal+api-key", personal, BillingAPIKey, "sk-test-123", "", "sk-test-123"},
		{"work+api-key", work, BillingAPIKey, "sk-test-456", work.ConfigDir, "sk-test-456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := ChildEnv(ChildEnvSpec{Profile: tc.profile, Billing: tc.billing, APIKey: tc.apiKey})

			got, count := envLookup(env, "CLAUDE_CONFIG_DIR")
			if tc.wantConfigDir == "" {
				if count != 0 {
					t.Errorf("CLAUDE_CONFIG_DIR present (%q x%d); personal profile must leave it UNSET", got, count)
				}
			} else if count != 1 || got != tc.wantConfigDir {
				t.Errorf("CLAUDE_CONFIG_DIR = %q x%d, want %q exactly once", got, count, tc.wantConfigDir)
			}

			got, count = envLookup(env, "ANTHROPIC_API_KEY")
			if tc.wantAPIKey == "" {
				if count != 0 {
					t.Errorf("ANTHROPIC_API_KEY present (%q x%d); subscription billing must scrub it", got, count)
				}
			} else if count != 1 || got != tc.wantAPIKey {
				t.Errorf("ANTHROPIC_API_KEY = %q x%d, want %q exactly once", got, count, tc.wantAPIKey)
			}

			// The polluted parent values must never leak through.
			for _, kv := range env {
				if strings.Contains(kv, "/polluted/parent/claude") || strings.Contains(kv, "sk-polluted-parent-key") {
					t.Errorf("polluted parent value leaked into child env: %q", kv)
				}
			}
			// PATH passthrough sanity (allowlisted).
			if _, count := envLookup(env, "PATH"); count != 1 {
				t.Errorf("PATH not passed through exactly once (count %d)", count)
			}
		})
	}
}

// TestChildEnvDropsSecrets is the P1 containment guard: a dispatched agent runs
// --permission-mode dontAsk on untrusted bead text, so the operator's ambient
// credentials must NOT reach it. ChildEnv is an allowlist, so tokens/cloud
// creds and the operator's ambient SSH_AUTH_SOCK are dropped by omission.
func TestChildEnvDropsSecrets(t *testing.T) {
	secrets := map[string]string{
		"GH_TOKEN":                 "ghp_should_not_leak",
		"GITHUB_TOKEN":             "ghs_should_not_leak",
		"VAULT_TOKEN":              "hvs.should_not_leak",
		"AWS_ACCESS_KEY_ID":        "AKIASHOULDNOTLEAK",
		"AWS_SECRET_ACCESS_KEY":    "aws_secret_should_not_leak",
		"AZURE_CLIENT_SECRET":      "azure_should_not_leak",
		"OP_SERVICE_ACCOUNT_TOKEN": "op_should_not_leak",
		"SSH_AUTH_SOCK":            "/tmp/operator-ambient-agent.sock",
		"NPM_TOKEN":                "npm_should_not_leak",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	env := ChildEnv(ChildEnvSpec{Profile: Profile{Name: "personal"}, Billing: BillingSubscription})
	for k := range secrets {
		if _, count := envLookup(env, k); count != 0 {
			t.Errorf("%s leaked into dispatched-agent env; allowlist must drop it", k)
		}
	}
	// A signing socket, when provided, IS injected — but only the koryph scoped
	// one, never the ambient operator socket set above.
	env = ChildEnv(ChildEnvSpec{Profile: Profile{Name: "personal"}, Billing: BillingSubscription, SSHAuthSock: "/koryph/signing/agent.sock"})
	if got, count := envLookup(env, "SSH_AUTH_SOCK"); count != 1 || got != "/koryph/signing/agent.sock" {
		t.Errorf("SSH_AUTH_SOCK = %q x%d, want the scoped socket exactly once", got, count)
	}
}

func TestChildEnvPreservesTLSCertificateLocations(t *testing.T) {
	t.Setenv("NIX_SSL_CERT_FILE", "/nix/store/cacert/etc/ssl/certs/ca-bundle.crt")
	t.Setenv("REQUESTS_CA_BUNDLE", "/etc/ssl/certs/ca-certificates.crt")
	env := ChildEnv(ChildEnvSpec{Profile: Profile{Name: "personal"}, Billing: BillingSubscription})
	for name, want := range map[string]string{
		"NIX_SSL_CERT_FILE":  "/nix/store/cacert/etc/ssl/certs/ca-bundle.crt",
		"REQUESTS_CA_BUNDLE": "/etc/ssl/certs/ca-certificates.crt",
	} {
		if got, count := envLookup(env, name); count != 1 || got != want {
			t.Errorf("%s = %q x%d, want %q exactly once", name, got, count, want)
		}
	}
}

// TestChildEnvPassthrough proves the registry escape hatch forwards a named
// operator var while still dropping everything else.
func TestChildEnvPassthrough(t *testing.T) {
	t.Setenv("MY_PROJECT_VAR", "wanted")
	t.Setenv("GH_TOKEN", "unwanted")
	env := ChildEnv(ChildEnvSpec{Profile: Profile{Name: "personal"}, Billing: BillingSubscription, Passthrough: []string{"MY_PROJECT_VAR"}})
	if got, count := envLookup(env, "MY_PROJECT_VAR"); count != 1 || got != "wanted" {
		t.Errorf("MY_PROJECT_VAR = %q x%d, want passthrough exactly once", got, count)
	}
	if _, count := envLookup(env, "GH_TOKEN"); count != 0 {
		t.Error("GH_TOKEN leaked despite not being in the passthrough list")
	}
}

// TestOutputCapDefaults proves the koryph defaults apply automatically when
// a caller's ChildEnvSpec doesn't set the output-cap fields — which is the
// shape every one of today's four dispatch sites actually uses (main
// dispatch via internal/runtime/claude.Command, internal/review,
// internal/stage, internal/epicreview all build ChildEnvSpec without ever
// touching BashMaxOutputLength/MaxMCPOutputTokens). This is the point of
// putting the defaults inside ChildEnv itself (design doc §3 L3: "so all
// four spawn sites get them uniformly and the allowlist discipline stays
// single-point") — no per-site code changes were needed.
func TestOutputCapDefaults(t *testing.T) {
	cases := []struct {
		name string
		spec ChildEnvSpec
	}{
		// internal/runtime/claude.Command's main-dispatch shape.
		{"main dispatch", ChildEnvSpec{Profile: Profile{Name: "personal"}, Billing: BillingSubscription, Passthrough: []string{"MY_VAR"}}},
		// internal/review.attemptReview's and internal/epicreview.attemptValidate's shape.
		{"review/epicreview", ChildEnvSpec{Profile: Profile{Name: "personal"}, Billing: BillingSubscription}},
		// internal/stage.Run's shape.
		{"stage", ChildEnvSpec{Profile: Profile{Name: "work", ConfigDir: "/x/.claude-work"}, Billing: BillingAPIKey, APIKey: "sk-test", SSHAuthSock: "/koryph/signing/agent.sock"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := ChildEnv(tc.spec)
			if got, count := envLookup(env, "BASH_MAX_OUTPUT_LENGTH"); count != 1 || got != fmt.Sprintf("%d", DefaultBashMaxOutputLength) {
				t.Errorf("BASH_MAX_OUTPUT_LENGTH = %q x%d, want %d exactly once", got, count, DefaultBashMaxOutputLength)
			}
			if got, count := envLookup(env, "MAX_MCP_OUTPUT_TOKENS"); count != 1 || got != fmt.Sprintf("%d", DefaultMaxMCPOutputTokens) {
				t.Errorf("MAX_MCP_OUTPUT_TOKENS = %q x%d, want %d exactly once", got, count, DefaultMaxMCPOutputTokens)
			}
		})
	}
}

// TestOutputCapOverridesAndOptOut proves a caller can override either cap to
// a specific positive value, or opt out entirely with a negative value.
func TestOutputCapOverridesAndOptOut(t *testing.T) {
	base := ChildEnvSpec{Profile: Profile{Name: "personal"}, Billing: BillingSubscription}

	t.Run("override", func(t *testing.T) {
		spec := base
		spec.BashMaxOutputLength = 12345
		spec.MaxMCPOutputTokens = 6789
		env := ChildEnv(spec)
		if got, count := envLookup(env, "BASH_MAX_OUTPUT_LENGTH"); count != 1 || got != "12345" {
			t.Errorf("BASH_MAX_OUTPUT_LENGTH = %q x%d, want 12345 exactly once", got, count)
		}
		if got, count := envLookup(env, "MAX_MCP_OUTPUT_TOKENS"); count != 1 || got != "6789" {
			t.Errorf("MAX_MCP_OUTPUT_TOKENS = %q x%d, want 6789 exactly once", got, count)
		}
	})

	t.Run("opt-out", func(t *testing.T) {
		spec := base
		spec.BashMaxOutputLength = -1
		spec.MaxMCPOutputTokens = -1
		env := ChildEnv(spec)
		if _, count := envLookup(env, "BASH_MAX_OUTPUT_LENGTH"); count != 0 {
			t.Errorf("BASH_MAX_OUTPUT_LENGTH present despite negative opt-out (count %d)", count)
		}
		if _, count := envLookup(env, "MAX_MCP_OUTPUT_TOKENS"); count != 0 {
			t.Errorf("MAX_MCP_OUTPUT_TOKENS present despite negative opt-out (count %d)", count)
		}
	})
}

// TestChildEnvProxySeam is the koryph-3l1.1 acceptance test: ProxyBaseURL and
// SpawnKind are typed ChildEnvSpec fields injected by the single ChildEnv
// choke point, so every spawn site (main dispatch, review, stage,
// epicreview) gets them uniformly with zero per-site logic. The table spans
// the four sites' actual spec shapes x billing mode x {proxy set, unset}.
// The unset case additionally asserts the resulting env is byte-identical to
// the same spec built without ProxyBaseURL/SpawnKind ever touched — the I6
// zero-residue default.
func TestChildEnvProxySeam(t *testing.T) {
	profile := Profile{Name: "work", ConfigDir: "/home/u/.claude-work"}
	const proxyURL = "http://127.0.0.1:8091"

	sites := []struct {
		name      string
		spawnKind string // "" for main dispatch, which never sets SpawnKind
		build     func(billing BillingMode) ChildEnvSpec
	}{
		{
			name:      "main-dispatch",
			spawnKind: "",
			build: func(billing BillingMode) ChildEnvSpec {
				return ChildEnvSpec{Profile: profile, Billing: billing, APIKey: "sk-test", SSHAuthSock: "/koryph/sign.sock"}
			},
		},
		{
			name:      "review",
			spawnKind: "review",
			build: func(billing BillingMode) ChildEnvSpec {
				return ChildEnvSpec{Profile: profile, Billing: BillingSubscription}
			},
		},
		{
			name:      "stage",
			spawnKind: "stage",
			build: func(billing BillingMode) ChildEnvSpec {
				return ChildEnvSpec{Profile: profile, Billing: billing, APIKey: "sk-test", SSHAuthSock: "/koryph/sign.sock"}
			},
		},
		{
			name:      "epicreview",
			spawnKind: "epicreview",
			build: func(billing BillingMode) ChildEnvSpec {
				return ChildEnvSpec{Profile: profile, Billing: BillingSubscription}
			},
		},
	}

	for _, site := range sites {
		for _, billing := range []BillingMode{BillingSubscription, BillingAPIKey} {
			t.Run(site.name+"/"+string(billing)+"/unset", func(t *testing.T) {
				base := site.build(billing)
				before := ChildEnv(base) // exactly the pre-koryph-3l1.1 spec shape

				withZero := base
				withZero.ProxyBaseURL = ""
				withZero.SpawnKind = ""
				after := ChildEnv(withZero)

				if len(before) != len(after) {
					t.Fatalf("env length differs: %d (before) vs %d (after) — zero-residue default broken", len(before), len(after))
				}
				for i := range before {
					if before[i] != after[i] {
						t.Errorf("env[%d] = %q, want %q (byte-identical zero-residue default)", i, after[i], before[i])
					}
				}
				if _, count := envLookup(after, "ANTHROPIC_BASE_URL"); count != 0 {
					t.Error("ANTHROPIC_BASE_URL present with ProxyBaseURL unset")
				}
				if _, count := envLookup(after, "KORYPH_SPAWN_KIND"); count != 0 {
					t.Error("KORYPH_SPAWN_KIND present with SpawnKind unset")
				}
			})

			t.Run(site.name+"/"+string(billing)+"/proxy-set", func(t *testing.T) {
				spec := site.build(billing)
				spec.ProxyBaseURL = proxyURL
				spec.SpawnKind = site.spawnKind
				env := ChildEnv(spec)

				if got, count := envLookup(env, "ANTHROPIC_BASE_URL"); count != 1 || got != proxyURL {
					t.Errorf("ANTHROPIC_BASE_URL = %q x%d, want %q exactly once", got, count, proxyURL)
				}
				if site.spawnKind == "" {
					if _, count := envLookup(env, "KORYPH_SPAWN_KIND"); count != 0 {
						t.Error("KORYPH_SPAWN_KIND present for main dispatch, want absent")
					}
				} else if got, count := envLookup(env, "KORYPH_SPAWN_KIND"); count != 1 || got != site.spawnKind {
					t.Errorf("KORYPH_SPAWN_KIND = %q x%d, want %q exactly once", got, count, site.spawnKind)
				}
			})
		}
	}
}

func TestBillingModeValid(t *testing.T) {
	if !BillingSubscription.Valid() || !BillingAPIKey.Valid() {
		t.Error("canonical billing modes must be Valid")
	}
	for _, bad := range []BillingMode{"", "free", "API-KEY"} {
		if bad.Valid() {
			t.Errorf("BillingMode(%q).Valid() = true, want false", bad)
		}
	}
}

func TestConfigJSONPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := ConfigJSONPath(Profile{Name: "personal"}), filepath.Join(home, ".claude.json"); got != want {
		t.Errorf("personal ConfigJSONPath = %q, want %q", got, want)
	}
	if got, want := ConfigJSONPath(Profile{Name: "work", ConfigDir: "/x/.claude-work"}), "/x/.claude-work/.claude.json"; got != want {
		t.Errorf("work ConfigJSONPath = %q, want %q", got, want)
	}
}

// writeConfig writes a .claude.json fixture into dir and returns the profile.
func writeConfig(t *testing.T, body string) Profile {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Profile{Name: "work", ConfigDir: dir}
}

func TestVerify(t *testing.T) {
	ctx := context.Background()

	t.Run("good", func(t *testing.T) {
		p := writeConfig(t, `{"oauthAccount":{"emailAddress":"owner@example.com","organizationName":"Example Org"}}`)
		id, err := Verify(ctx, p)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if id.Email != "owner@example.com" {
			t.Errorf("Email = %q", id.Email)
		}
		if id.Organization != "Example Org" {
			t.Errorf("Organization = %q", id.Organization)
		}
		if want := ConfigJSONPath(p); id.ConfigPath != want {
			t.Errorf("ConfigPath = %q, want %q", id.ConfigPath, want)
		}
	})

	t.Run("missing oauthAccount", func(t *testing.T) {
		p := writeConfig(t, `{"someOtherKey":true}`)
		if _, err := Verify(ctx, p); err == nil {
			t.Fatal("Verify succeeded on config without oauthAccount; must fail closed")
		}
	})

	t.Run("empty email", func(t *testing.T) {
		p := writeConfig(t, `{"oauthAccount":{"emailAddress":""}}`)
		if _, err := Verify(ctx, p); err == nil {
			t.Fatal("Verify succeeded on empty emailAddress; must fail closed")
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		p := writeConfig(t, `{not json`)
		if _, err := Verify(ctx, p); err == nil {
			t.Fatal("Verify succeeded on unparseable config; must fail closed")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		p := Profile{Name: "work", ConfigDir: filepath.Join(t.TempDir(), "nope")}
		if _, err := Verify(ctx, p); err == nil {
			t.Fatal("Verify succeeded on missing config file; must fail closed")
		}
	})
}

func TestVerifyExpected(t *testing.T) {
	ctx := context.Background()
	p := writeConfig(t, `{"oauthAccount":{"emailAddress":"Owner@Example.Com"}}`)

	t.Run("case-insensitive match", func(t *testing.T) {
		id, err := VerifyExpected(ctx, p, "owner@example.com")
		if err != nil {
			t.Fatalf("VerifyExpected: %v", err)
		}
		if id.Email != "Owner@Example.Com" {
			t.Errorf("Email = %q", id.Email)
		}
	})

	t.Run("mismatch names both emails and config path", func(t *testing.T) {
		_, err := VerifyExpected(ctx, p, "other@example.com")
		if err == nil {
			t.Fatal("VerifyExpected succeeded on mismatched identity; must fail closed")
		}
		msg := err.Error()
		for _, want := range []string{"account mismatch", "Owner@Example.Com", "other@example.com", ConfigJSONPath(p)} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q does not contain %q", msg, want)
			}
		}
	})

	t.Run("empty expected fails closed", func(t *testing.T) {
		if _, err := VerifyExpected(ctx, p, ""); err == nil {
			t.Fatal("VerifyExpected succeeded with empty expected identity; must fail closed")
		}
	})
}

// withFakeLiveness substitutes probeLiveness for the duration of the test so
// VerifyAuth's api-key/oauth-token branch never touches the network; it
// restores the real anthro.ProbeLiveness on cleanup.
func withFakeLiveness(t *testing.T, fn func(ctx context.Context, credential string, useBearer bool) error) {
	t.Helper()
	orig := probeLiveness
	probeLiveness = fn
	t.Cleanup(func() { probeLiveness = orig })
}

// TestVerifyAuthSubscriptionUnchanged is the AC #5 regression guard: an
// AuthSpec with Mode "" or AuthModeSubscription must behave byte-for-byte
// like a direct VerifyExpected call — same success shape, same failure
// message on mismatch — and must never call ResolveCredential or the
// liveness probe (proven by never installing a fake and still succeeding).
func TestVerifyAuthSubscriptionUnchanged(t *testing.T) {
	ctx := context.Background()
	p := writeConfig(t, `{"oauthAccount":{"emailAddress":"owner@example.com","organizationName":"Example Org"}}`)

	for _, mode := range []AuthMode{"", AuthModeSubscription} {
		t.Run(string(mode), func(t *testing.T) {
			want, wantErr := VerifyExpected(ctx, p, "owner@example.com")
			got, gotErr := VerifyAuth(ctx, p, AuthSpec{Mode: mode, ExpectedIdentity: "owner@example.com"})
			if wantErr != nil || gotErr != nil {
				t.Fatalf("VerifyExpected err=%v, VerifyAuth err=%v", wantErr, gotErr)
			}
			if got != want {
				t.Errorf("VerifyAuth = %+v, want byte-identical %+v", got, want)
			}
		})
	}

	t.Run("mismatch still fails closed with the same message", func(t *testing.T) {
		_, wantErr := VerifyExpected(ctx, p, "other@example.com")
		_, gotErr := VerifyAuth(ctx, p, AuthSpec{ExpectedIdentity: "other@example.com"})
		if wantErr == nil || gotErr == nil || wantErr.Error() != gotErr.Error() {
			t.Errorf("VerifyAuth error = %v, want %v", gotErr, wantErr)
		}
	})
}

// TestVerifyAuthCredentialModes covers the AuthModeAPIKey/AuthModeOAuthToken
// happy path: a resolvable, fingerprint-matching, live credential verifies
// and Identity carries the fingerprint + label, never an email.
func TestVerifyAuthCredentialModes(t *testing.T) {
	ctx := context.Background()
	p := Profile{Name: "personal"}

	for _, tc := range []struct {
		mode       AuthMode
		wantBearer bool
	}{
		{AuthModeAPIKey, false},
		{AuthModeOAuthToken, true},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Setenv("KORYPH_TEST_VERIFY_CRED", "sk-live-credential-value")
			var gotBearer bool
			var probed string
			withFakeLiveness(t, func(_ context.Context, credential string, useBearer bool) error {
				probed, gotBearer = credential, useBearer
				return nil
			})

			cred := &Credential{Source: CredentialSourceEnv, EnvVar: "KORYPH_TEST_VERIFY_CRED"}
			fp := Fingerprint("sk-live-credential-value")

			id, err := VerifyAuth(ctx, p, AuthSpec{
				Mode:                tc.mode,
				ExpectedIdentity:    "my-key-label",
				Credential:          cred,
				IdentityFingerprint: fp,
			})
			if err != nil {
				t.Fatalf("VerifyAuth: %v", err)
			}
			if id.Fingerprint != fp {
				t.Errorf("Identity.Fingerprint = %q, want %q", id.Fingerprint, fp)
			}
			if id.Label != "my-key-label" {
				t.Errorf("Identity.Label = %q, want my-key-label", id.Label)
			}
			if id.Email != "" {
				t.Errorf("Identity.Email = %q, want empty (no email for credential modes)", id.Email)
			}
			if probed != "sk-live-credential-value" {
				t.Errorf("liveness probed %q, want the resolved credential value", probed)
			}
			if gotBearer != tc.wantBearer {
				t.Errorf("liveness useBearer = %v, want %v", gotBearer, tc.wantBearer)
			}
		})
	}
}

// TestVerifyAuthFingerprintMismatchFailsClosed proves a swapped credential
// (AC #4) is refused BEFORE any liveness probe — a live-but-wrong credential
// must not verify just because it happens to be a valid Anthropic key.
func TestVerifyAuthFingerprintMismatchFailsClosed(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KORYPH_TEST_VERIFY_SWAPPED", "sk-a-different-live-key")

	probed := false
	withFakeLiveness(t, func(context.Context, string, bool) error {
		probed = true
		return nil
	})

	_, err := VerifyAuth(ctx, Profile{Name: "personal"}, AuthSpec{
		Mode:                AuthModeAPIKey,
		Credential:          &Credential{Source: CredentialSourceEnv, EnvVar: "KORYPH_TEST_VERIFY_SWAPPED"},
		IdentityFingerprint: Fingerprint("sk-the-originally-enrolled-key"),
	})
	if err == nil {
		t.Fatal("VerifyAuth succeeded despite a fingerprint mismatch; must fail closed")
	}
	if probed {
		t.Error("liveness probe ran despite a fingerprint mismatch; must fail closed before spending a network call")
	}
}

// TestVerifyAuthLivenessFailureFailsClosed proves an expired/revoked
// credential (fingerprint matches, but the API rejects it) still refuses
// dispatch.
func TestVerifyAuthLivenessFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KORYPH_TEST_VERIFY_DEAD", "sk-expired-key")
	withFakeLiveness(t, func(context.Context, string, bool) error {
		return fmt.Errorf("anthro: liveness probe failed: 401")
	})

	_, err := VerifyAuth(ctx, Profile{Name: "personal"}, AuthSpec{
		Mode:                AuthModeAPIKey,
		Credential:          &Credential{Source: CredentialSourceEnv, EnvVar: "KORYPH_TEST_VERIFY_DEAD"},
		IdentityFingerprint: Fingerprint("sk-expired-key"),
	})
	if err == nil {
		t.Fatal("VerifyAuth succeeded despite a failed liveness probe; must fail closed")
	}
}

// TestVerifyAuthMissingFingerprintFailsClosed proves an unenrolled record
// (no identity_fingerprint recorded yet) refuses rather than treating a
// blank fingerprint as "anything matches".
func TestVerifyAuthMissingFingerprintFailsClosed(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KORYPH_TEST_VERIFY_UNENROLLED", "sk-some-key")
	withFakeLiveness(t, func(context.Context, string, bool) error { return nil })

	_, err := VerifyAuth(ctx, Profile{Name: "personal"}, AuthSpec{
		Mode:       AuthModeAPIKey,
		Credential: &Credential{Source: CredentialSourceEnv, EnvVar: "KORYPH_TEST_VERIFY_UNENROLLED"},
		// IdentityFingerprint intentionally left empty.
	})
	if err == nil {
		t.Fatal("VerifyAuth succeeded with no enrolled identity_fingerprint; must fail closed")
	}
}

// TestVerifyAuthUnresolvableCredentialFailsClosed proves an unset/empty
// credential source refuses before ever reaching the fingerprint check.
func TestVerifyAuthUnresolvableCredentialFailsClosed(t *testing.T) {
	ctx := context.Background()
	_, err := VerifyAuth(ctx, Profile{Name: "personal"}, AuthSpec{
		Mode:                AuthModeAPIKey,
		Credential:          nil,
		IdentityFingerprint: "sha256:doesnotmatter",
	})
	if err == nil {
		t.Fatal("VerifyAuth succeeded with no credential configured; must fail closed")
	}
}

func TestVerifyAuthUnrecognizedModeFailsClosed(t *testing.T) {
	ctx := context.Background()
	if _, err := VerifyAuth(ctx, Profile{Name: "personal"}, AuthSpec{Mode: "quantum-entanglement"}); err == nil {
		t.Fatal("VerifyAuth succeeded with an unrecognized auth_mode; must fail closed")
	}
}

// TestChildEnvCredentialSeam is the koryph-i3b acceptance test (§6/I4): the
// resolved auth-mode credential is injected under exactly its canonical
// name, the unset case is a byte-identical zero-residue default (mirroring
// TestChildEnvProxySeam's pattern), and CredentialEnvVar never coexists with
// a second ANTHROPIC_API_KEY= entry from the legacy Billing/APIKey fallback.
func TestChildEnvCredentialSeam(t *testing.T) {
	base := ChildEnvSpec{Profile: Profile{Name: "personal"}, Billing: BillingSubscription}

	t.Run("unset is byte-identical to the pre-koryph-i3b shape", func(t *testing.T) {
		before := ChildEnv(base)
		withZero := base
		withZero.Credential, withZero.CredentialEnvVar = "", ""
		after := ChildEnv(withZero)
		if len(before) != len(after) {
			t.Fatalf("env length differs: %d vs %d", len(before), len(after))
		}
		for i := range before {
			if before[i] != after[i] {
				t.Errorf("env[%d] = %q, want %q", i, after[i], before[i])
			}
		}
	})

	t.Run("api-key mode injects ANTHROPIC_API_KEY exactly once", func(t *testing.T) {
		spec := base
		spec.CredentialEnvVar = "ANTHROPIC_API_KEY"
		spec.Credential = "sk-resolved-api-key"
		env := ChildEnv(spec)
		if got, count := envLookup(env, "ANTHROPIC_API_KEY"); count != 1 || got != "sk-resolved-api-key" {
			t.Errorf("ANTHROPIC_API_KEY = %q x%d, want sk-resolved-api-key exactly once", got, count)
		}
	})

	t.Run("oauth-token mode injects CLAUDE_CODE_OAUTH_TOKEN and never ANTHROPIC_API_KEY", func(t *testing.T) {
		spec := base
		spec.CredentialEnvVar = "CLAUDE_CODE_OAUTH_TOKEN"
		spec.Credential = "oauth-resolved-token"
		env := ChildEnv(spec)
		if got, count := envLookup(env, "CLAUDE_CODE_OAUTH_TOKEN"); count != 1 || got != "oauth-resolved-token" {
			t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q x%d, want oauth-resolved-token exactly once", got, count)
		}
		if _, count := envLookup(env, "ANTHROPIC_API_KEY"); count != 0 {
			t.Error("ANTHROPIC_API_KEY present for oauth-token mode; I4 forbids setting both")
		}
	})

	t.Run("CredentialEnvVar wins over the legacy Billing/APIKey fallback — never double-injected", func(t *testing.T) {
		spec := ChildEnvSpec{
			Profile:          Profile{Name: "personal"},
			Billing:          BillingAPIKey,
			APIKey:           "sk-legacy-billing-key",
			CredentialEnvVar: "ANTHROPIC_API_KEY",
			Credential:       "sk-auth-mode-credential",
		}
		env := ChildEnv(spec)
		got, count := envLookup(env, "ANTHROPIC_API_KEY")
		if count != 1 {
			t.Fatalf("ANTHROPIC_API_KEY present x%d, want exactly once (I4: one injected credential)", count)
		}
		if got != "sk-auth-mode-credential" {
			t.Errorf("ANTHROPIC_API_KEY = %q, want the auth-mode credential to win, not the legacy billing key", got)
		}
	})
}
