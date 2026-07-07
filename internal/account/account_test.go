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
