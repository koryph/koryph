// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitLabSaveLoad exercises the round-trip of GitLabConfig through the
// credential file on disk.
func TestGitLabSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	cfg := &GitLabConfig{
		Name:      "my-gl-bot",
		Forge:     "gitlab",
		Host:      "gitlab.com",
		Project:   "myns/myproject",
		TokenID:   999,
		TokenName: "koryph-bot-my-gl-bot",
		ExpiresAt: "2027-01-01",
		Token:     "glpat-testtoken",
	}

	if err := SaveGitLab(cfg); err != nil {
		t.Fatalf("SaveGitLab: %v", err)
	}

	// File should be 0600.
	path := GitLabBotPath(cfg.Name)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}

	got, err := LoadGitLab("my-gl-bot")
	if err != nil {
		t.Fatalf("LoadGitLab: %v", err)
	}
	if got.Name != cfg.Name ||
		got.Host != cfg.Host ||
		got.Project != cfg.Project ||
		got.TokenID != cfg.TokenID ||
		got.ExpiresAt != cfg.ExpiresAt ||
		got.Token != cfg.Token {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
}

// TestGitLabBotPath checks the file path convention.
func TestGitLabBotPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)
	path := GitLabBotPath("my-bot")
	want := filepath.Join(tmp, "bots", "my-bot.gitlab.json")
	if path != want {
		t.Errorf("GitLabBotPath = %q, want %q", path, want)
	}
}

// TestListGitLab verifies that ListGitLab returns only .gitlab.json files.
func TestListGitLab(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	// Empty dir.
	names, err := ListGitLab()
	if err != nil {
		t.Fatalf("ListGitLab empty: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected 0 bots, got %v", names)
	}

	// Save two GitLab bots.
	for _, name := range []string{"gl-a", "gl-b"} {
		if err := SaveGitLab(&GitLabConfig{Name: name, Forge: "gitlab", Host: "gitlab.com"}); err != nil {
			t.Fatalf("SaveGitLab %s: %v", name, err)
		}
	}
	// Also save a GitHub bot (should NOT appear in ListGitLab).
	if err := Save(&Config{Name: "gh-bot", AppID: 1}); err != nil {
		t.Fatalf("Save gh-bot: %v", err)
	}

	names, err = ListGitLab()
	if err != nil {
		t.Fatalf("ListGitLab: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 gitlab bots, got %v", names)
	}
	for _, n := range names {
		if n != "gl-a" && n != "gl-b" {
			t.Errorf("unexpected name %q", n)
		}
	}
}

// TestGitLabIsPointerGL checks the pointer/inline detection.
func TestGitLabIsPointerGL(t *testing.T) {
	inline := &GitLabConfig{Token: "tok"}
	if inline.IsPointerGL() {
		t.Error("IsPointerGL() = true for inline config, want false")
	}
	pointer := &GitLabConfig{Provider: "keychain", KeyRef: "ref"}
	if !pointer.IsPointerGL() {
		t.Error("IsPointerGL() = false for pointer config, want true")
	}
}

// TestGitLabResolveGLToken_Inline checks that inline mode returns Token directly.
func TestGitLabResolveGLToken_Inline(t *testing.T) {
	cfg := &GitLabConfig{Token: "glpat-mytoken"}
	tok, err := ResolveGLToken(nil, cfg) //nolint:staticcheck
	if err != nil {
		t.Fatalf("ResolveGLToken inline: %v", err)
	}
	if tok != "glpat-mytoken" {
		t.Errorf("token = %q, want glpat-mytoken", tok)
	}
}

// TestGLSettingsURL checks the settings URL construction.
func TestGLSettingsURL(t *testing.T) {
	tests := []struct {
		host    string
		project string
		want    string
	}{
		{
			host:    "gitlab.com",
			project: "myns/myproject",
			want:    "https://gitlab.com/myns%2Fmyproject/-/settings/access_tokens",
		},
		{
			host:    "gitlab.example.com",
			project: "",
			want:    "https://gitlab.example.com/-/user_settings/personal_access_tokens",
		},
		{
			host:    "gitlab.com",
			project: "",
			want:    "https://gitlab.com/-/user_settings/personal_access_tokens",
		},
	}
	for _, tc := range tests {
		got := GLSettingsURL(tc.host, tc.project)
		if got != tc.want {
			t.Errorf("GLSettingsURL(%q, %q) = %q, want %q", tc.host, tc.project, got, tc.want)
		}
	}
}

// TestGLBotInstallURL checks the CI/CD variables page URL.
func TestGLBotInstallURL(t *testing.T) {
	cfg := &GitLabConfig{Host: "gitlab.com", Project: "myns/myproject"}
	got := GLBotInstallURL(cfg)
	if !strings.Contains(got, "myns%2Fmyproject") {
		t.Errorf("GLBotInstallURL = %q; expected encoded project path", got)
	}
	if !strings.Contains(got, "ci_cd") {
		t.Errorf("GLBotInstallURL = %q; expected CI/CD settings anchor", got)
	}
}

// TestDefaultGLKeyRef checks provider-specific key reference derivation.
func TestDefaultGLKeyRef(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	tests := []struct {
		provider string
		botName  string
		wantPfx  string
	}{
		{"keychain", "my-bot", "koryph-gl-bot-my-bot"},
		{"encrypted-file", "my-bot", ".gl.age"},
		{"file", "my-bot", ".gl.token"},
		{"protonpass", "my-bot", "koryph-gl-bot-my-bot"},
	}
	for _, tc := range tests {
		got := defaultGLKeyRef(tc.provider, tc.botName)
		if !strings.Contains(got, tc.wantPfx) {
			t.Errorf("defaultGLKeyRef(%q, %q) = %q; want to contain %q",
				tc.provider, tc.botName, got, tc.wantPfx)
		}
	}
}

// TestCheckGitLabCredentials_NoBots verifies graceful empty-list handling.
func TestCheckGitLabCredentials_NoBots(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	findings, err := CheckGitLabCredentials()
	if err != nil {
		t.Fatalf("CheckGitLabCredentials empty: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

// TestCheckGitLabCredentials_PointerBot verifies pointer-mode bots produce
// an OK finding.
func TestCheckGitLabCredentials_PointerBot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	cfg := &GitLabConfig{
		Name:     "gl-ptr",
		Forge:    "gitlab",
		Host:     "gitlab.com",
		Provider: "keychain",
		KeyRef:   "koryph-gl-bot-gl-ptr",
	}
	if err := SaveGitLab(cfg); err != nil {
		t.Fatalf("SaveGitLab: %v", err)
	}

	findings, err := CheckGitLabCredentials()
	if err != nil {
		t.Fatalf("CheckGitLabCredentials: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("expected at least 1 finding")
	}
	if findings[0].Level != CheckOK {
		t.Errorf("level = %s, want ok; message: %s", findings[0].Level, findings[0].Message)
	}
}

// TestCheckGitLabCredentials_InlineToken verifies inline tokens get a posture
// warning (same pattern as GitHub bots with inline PEM).
func TestCheckGitLabCredentials_InlineToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	cfg := &GitLabConfig{
		Name:  "gl-inline",
		Forge: "gitlab",
		Host:  "gitlab.com",
		Token: "glpat-secret",
	}
	if err := SaveGitLab(cfg); err != nil {
		t.Fatalf("SaveGitLab: %v", err)
	}

	findings, err := CheckGitLabCredentials()
	if err != nil {
		t.Fatalf("CheckGitLabCredentials: %v", err)
	}
	// Expect 2 findings: OK (credentials ok) + WARN (inline token posture).
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %v", len(findings), findings)
	}
	if findings[0].Level != CheckOK {
		t.Errorf("first finding = %s, want ok", findings[0].Level)
	}
	if findings[1].Level != CheckWarn {
		t.Errorf("second finding = %s, want warn (posture)", findings[1].Level)
	}
	if !strings.Contains(findings[1].Message, "vault-migrate") {
		t.Errorf("posture warning should mention vault-migrate, got: %q", findings[1].Message)
	}
}
