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

// (a) Only the default ~/.claude.json exists, with an email: one verified
// "personal" candidate.
func TestDiscoverDefaultOnly(t *testing.T) {
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
