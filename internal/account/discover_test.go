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

// writeHomeConfig writes a .claude.json fixture at home/rel/.claude.json
// (home/.claude.json when rel == "") and returns its full path. Mirrors
// account_test.go's writeConfig helper, but for a specific location under a
// fixture home rather than an arbitrary work-profile ConfigDir.
func writeHomeConfig(t *testing.T, home, rel, body string) string {
	t.Helper()
	dir := home
	if rel != "" {
		dir = filepath.Join(home, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// findCandidate returns the candidate named name, failing the test if absent.
func findCandidate(t *testing.T, cands []Candidate, name string) Candidate {
	t.Helper()
	for _, c := range cands {
		if c.Profile.Name == name {
			return c
		}
	}
	t.Fatalf("no candidate named %q in %+v", name, cands)
	return Candidate{}
}

// clearAmbientCredentialEnv unsets the two ambient credential env vars
// discover() now scans (koryph-i3b, design §8) so the .claude.json-only
// scenarios below stay hermetic regardless of the host shell's actual
// environment (t.Setenv restores the prior value, unset or not, on cleanup).
func clearAmbientCredentialEnv(t *testing.T) {
	t.Helper()
	t.Setenv(ambientOAuthTokenEnvVar, "")
	t.Setenv(ambientAPIKeyEnvVar, "")
}

// (a) Only the default ~/.claude.json exists, with an email: one verified
// "personal" candidate.
func TestDiscoverDefaultOnly(t *testing.T) {
	clearAmbientCredentialEnv(t)
	home := t.TempDir()
	writeHomeConfig(t, home, "", `{"oauthAccount":{"emailAddress":"me@example.com","organizationName":"Acme"}}`)

	cands := discover(context.Background(), home)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Profile.Name != "personal" || c.Profile.ConfigDir != "" {
		t.Errorf("Profile = %+v, want {Name:personal ConfigDir:\"\"}", c.Profile)
	}
	if !c.Verified {
		t.Errorf("Verified = false, want true (Err: %q)", c.Err)
	}
	if c.Identity != "me@example.com" {
		t.Errorf("Identity = %q, want me@example.com", c.Identity)
	}
	if c.Provenance != "derived from ~/.claude.json" {
		t.Errorf("Provenance = %q, want %q", c.Provenance, "derived from ~/.claude.json")
	}
	if c.Err != "" {
		t.Errorf("Err = %q, want empty for a verified candidate", c.Err)
	}
}

// (b) Default + ~/.claude-work/.claude.json: two candidates, named
// personal/work, both verified, with correct provenance strings.
func TestDiscoverDefaultAndWork(t *testing.T) {
	clearAmbientCredentialEnv(t)
	home := t.TempDir()
	writeHomeConfig(t, home, "", `{"oauthAccount":{"emailAddress":"personal@example.com"}}`)
	writeHomeConfig(t, home, ".claude-work", `{"oauthAccount":{"emailAddress":"work@example.com","organizationName":"WorkCo"}}`)

	cands := discover(context.Background(), home)
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2: %+v", len(cands), cands)
	}

	personal := findCandidate(t, cands, "personal")
	if personal.Profile.ConfigDir != "" {
		t.Errorf("personal ConfigDir = %q, want \"\"", personal.Profile.ConfigDir)
	}
	if !personal.Verified || personal.Identity != "personal@example.com" {
		t.Errorf("personal Verified=%v Identity=%q, want true/personal@example.com (Err: %q)", personal.Verified, personal.Identity, personal.Err)
	}
	if personal.Provenance != "derived from ~/.claude.json" {
		t.Errorf("personal Provenance = %q", personal.Provenance)
	}

	work := findCandidate(t, cands, "work")
	wantDir := filepath.Join(home, ".claude-work")
	if work.Profile.ConfigDir != wantDir {
		t.Errorf("work ConfigDir = %q, want %q", work.Profile.ConfigDir, wantDir)
	}
	if !work.Verified || work.Identity != "work@example.com" {
		t.Errorf("work Verified=%v Identity=%q, want true/work@example.com (Err: %q)", work.Verified, work.Identity, work.Err)
	}
	if work.Provenance != "found ~/.claude-work/.claude.json" {
		t.Errorf("work Provenance = %q, want %q", work.Provenance, "found ~/.claude-work/.claude.json")
	}
}

// (c) No .claude.json anywhere: one unverified default candidate, with auth
// guidance in Err.
func TestDiscoverNoConfigAnywhere(t *testing.T) {
	clearAmbientCredentialEnv(t)
	home := t.TempDir()

	cands := discover(context.Background(), home)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Profile.Name != "personal" {
		t.Errorf("Profile.Name = %q, want personal", c.Profile.Name)
	}
	if c.Verified {
		t.Errorf("Verified = true, want false when no .claude.json exists anywhere")
	}
	if c.Identity != "" {
		t.Errorf("Identity = %q, want empty", c.Identity)
	}
	if !strings.Contains(c.Err, "claude auth login") {
		t.Errorf("Err = %q, want auth guidance mentioning `claude auth login`", c.Err)
	}
}

// (d) A .claude-foo directory WITHOUT a .claude.json is not a candidate at
// all — it is skipped, not reported as unverified.
func TestDiscoverSkipsProfileDirWithoutConfig(t *testing.T) {
	clearAmbientCredentialEnv(t)
	home := t.TempDir()
	writeHomeConfig(t, home, "", `{"oauthAccount":{"emailAddress":"me@example.com"}}`)
	if err := os.MkdirAll(filepath.Join(home, ".claude-foo"), 0o755); err != nil {
		t.Fatalf("mkdir .claude-foo: %v", err)
	}

	cands := discover(context.Background(), home)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (.claude-foo has no .claude.json and must be skipped): %+v", len(cands), cands)
	}
	if cands[0].Profile.Name != "personal" {
		t.Errorf("candidate = %+v, want only personal", cands[0])
	}
}

// (e) An unparseable .claude.json yields an unverified candidate with a
// reason in Err (not auth guidance — the file exists, it just doesn't parse).
func TestDiscoverUnparseableConfig(t *testing.T) {
	clearAmbientCredentialEnv(t)
	home := t.TempDir()
	writeHomeConfig(t, home, "", `{not json`)

	cands := discover(context.Background(), home)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Verified {
		t.Error("Verified = true, want false for unparseable JSON")
	}
	if c.Err == "" {
		t.Fatal("Err = empty, want a reason")
	}
	if strings.Contains(c.Err, "claude auth login") {
		t.Errorf("Err = %q, should not carry auth-login guidance for a parse failure (file exists)", c.Err)
	}
	if !strings.Contains(c.Err, "parsing") {
		t.Errorf("Err = %q, want it to mention the parse failure", c.Err)
	}
}

// Discover (the public, non-injectable entry point) must resolve home the
// same way discover's fixture-driven core does, so it stays exercised by the
// same behavior the table above proves.
func TestDiscoverPublicEntryPointUsesHOME(t *testing.T) {
	clearAmbientCredentialEnv(t)
	home := t.TempDir()
	writeHomeConfig(t, home, "", `{"oauthAccount":{"emailAddress":"me@example.com"}}`)
	t.Setenv("HOME", home)

	cands := Discover(context.Background())
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	if !cands[0].Verified || cands[0].Identity != "me@example.com" {
		t.Errorf("Discover() candidate = %+v, want verified me@example.com", cands[0])
	}
}

// (f) An OAuth-less machine with a bare ambient ANTHROPIC_API_KEY: the
// .claude.json-derived personal candidate stays unverified as before, PLUS a
// second, also-unverified "ambient-api-key" candidate reports the key and
// names the exact flag needed — api-key mode is never entered without it
// (koryph-i3b.6 acceptance criteria; design §8).
func TestDiscoverAmbientAPIKeyReportedNotVerified(t *testing.T) {
	clearAmbientCredentialEnv(t)
	t.Setenv(ambientAPIKeyEnvVar, "sk-ant-fake-key")
	home := t.TempDir()

	cands := discover(context.Background(), home)
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2 (personal + ambient-api-key): %+v", len(cands), cands)
	}

	personal := findCandidate(t, cands, "personal")
	if personal.Verified {
		t.Errorf("personal Verified = true, want false (no .claude.json)")
	}

	key := findCandidate(t, cands, "ambient-api-key")
	if key.Verified {
		t.Errorf("ambient-api-key Verified = true, want false — api-key mode must never be entered without an explicit flag")
	}
	if key.AuthMode != AuthModeAPIKey {
		t.Errorf("ambient-api-key AuthMode = %q, want %q", key.AuthMode, AuthModeAPIKey)
	}
	if key.Identity != "" {
		t.Errorf("ambient-api-key Identity = %q, want empty (never auto-resolved)", key.Identity)
	}
	if !strings.Contains(key.Err, "no OAuth login found") {
		t.Errorf("Err = %q, want it to note no OAuth login was found", key.Err)
	}
	if !strings.Contains(key.Err, "--auth-mode api-key") {
		t.Errorf("Err = %q, want it to name --auth-mode api-key", key.Err)
	}
	if !strings.Contains(key.Err, "bills per token") {
		t.Errorf("Err = %q, want it to warn about per-token billing", key.Err)
	}
}

// (g) A machine WITH a verified OAuth login plus an ambient ANTHROPIC_API_KEY:
// the api-key candidate's message drops the "no OAuth login found" preamble
// (it isn't true) but is still never auto-verified.
func TestDiscoverAmbientAPIKeyWithOAuthLoginPresent(t *testing.T) {
	clearAmbientCredentialEnv(t)
	t.Setenv(ambientAPIKeyEnvVar, "sk-ant-fake-key")
	home := t.TempDir()
	writeHomeConfig(t, home, "", `{"oauthAccount":{"emailAddress":"me@example.com"}}`)

	cands := discover(context.Background(), home)
	key := findCandidate(t, cands, "ambient-api-key")
	if key.Verified {
		t.Errorf("ambient-api-key Verified = true, want false")
	}
	if strings.Contains(key.Err, "no OAuth login found") {
		t.Errorf("Err = %q, should not claim no OAuth login when personal verified", key.Err)
	}
	if !strings.Contains(key.Err, "--auth-mode api-key") {
		t.Errorf("Err = %q, want it to name --auth-mode api-key", key.Err)
	}
}

// (h) An ambient CLAUDE_CODE_OAUTH_TOKEN is offered freely: a verified
// candidate under AuthModeOAuthToken, no explicit flag required (it bills
// against the subscription, not per token).
func TestDiscoverAmbientOAuthTokenVerifiedFreely(t *testing.T) {
	clearAmbientCredentialEnv(t)
	t.Setenv(ambientOAuthTokenEnvVar, "cot-fake-token")
	home := t.TempDir()

	cands := discover(context.Background(), home)
	tok := findCandidate(t, cands, "ambient-oauth-token")
	if !tok.Verified {
		t.Errorf("ambient-oauth-token Verified = false, want true (Err: %q)", tok.Err)
	}
	if tok.AuthMode != AuthModeOAuthToken {
		t.Errorf("ambient-oauth-token AuthMode = %q, want %q", tok.AuthMode, AuthModeOAuthToken)
	}
	if tok.Identity != Fingerprint("cot-fake-token") {
		t.Errorf("ambient-oauth-token Identity = %q, want the credential fingerprint", tok.Identity)
	}
	if tok.Err != "" {
		t.Errorf("ambient-oauth-token Err = %q, want empty for a verified candidate", tok.Err)
	}
}

// (i) Neither ambient var set: discover() behaves exactly as before these
// changes — no extra candidates appear.
func TestDiscoverNoAmbientCredentials(t *testing.T) {
	clearAmbientCredentialEnv(t)
	home := t.TempDir()
	writeHomeConfig(t, home, "", `{"oauthAccount":{"emailAddress":"me@example.com"}}`)

	cands := discover(context.Background(), home)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (no ambient credential vars set): %+v", len(cands), cands)
	}
}
