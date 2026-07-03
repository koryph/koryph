// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package account

import (
	"context"
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

func TestEnvMatrix(t *testing.T) {
	// Pollute the parent env: Env must scrub BOTH vars before re-injecting.
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
			env := Env(tc.profile, tc.billing, tc.apiKey)

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
			// PATH passthrough sanity.
			if _, count := envLookup(env, "PATH"); count != 1 {
				t.Errorf("PATH not passed through exactly once (count %d)", count)
			}
		})
	}
}

// TestEnvPreservesSSHAuthSock guards the signing contract: dispatched agents
// sign commits via the operator's SSH agent, so SSH_AUTH_SOCK must pass
// through Env untouched (BaseEnv scrubs only CLAUDE_CONFIG_DIR and
// ANTHROPIC_API_KEY).
func TestEnvPreservesSSHAuthSock(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/koryph-test-agent.sock")
	env := Env(Profile{Name: "personal"}, BillingSubscription, "")
	got, count := envLookup(env, "SSH_AUTH_SOCK")
	if count != 1 || got != "/tmp/koryph-test-agent.sock" {
		t.Errorf("SSH_AUTH_SOCK = %q x%d, want the parent value exactly once (commit signing needs the agent)", got, count)
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
