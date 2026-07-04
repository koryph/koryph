// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"release-bot", false},
		{"mybot", false},
		{"my-org-release-bot", false},
		{"a", false},
		{"", true},
		{"-bad", true},
		{"Bad", true},
		{"has space", true},
		{"toolong" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true}, // 40 chars total
	}
	for _, tc := range tests {
		err := ValidateName(tc.name)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateName(%q) error=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	cfg := &Config{
		Name:   "release-bot",
		AppID:  42,
		Slug:   "release-bot",
		Owner:  "octocat",
		Public: false,
		PEM:    "pem-placeholder-for-unit-test",
	}

	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File should be 0600.
	path := BotPath(cfg.Name)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}

	got, err := Load("release-bot")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AppID != cfg.AppID || got.Slug != cfg.Slug || got.PEM != cfg.PEM {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
}

func TestList(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	// Empty bots dir.
	names, err := List()
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected 0 bots, got %v", names)
	}

	// Add two bots.
	for _, name := range []string{"bot-a", "bot-b"} {
		if err := Save(&Config{Name: name, Slug: name}); err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
	}

	// A stray non-json file should be ignored.
	_ = os.WriteFile(filepath.Join(BotsDir(), "notes.txt"), []byte("ignore me"), 0o600)

	names, err = List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 bots, got %v", names)
	}
}

func TestBuildManifest(t *testing.T) {
	m := buildManifest("my-bot", true, "http://127.0.0.1:12345/callback")
	if m.Public != true {
		t.Error("manifest.public should be true")
	}
	if m.Permissions["contents"] != "write" {
		t.Error("expected contents:write")
	}
	if m.Permissions["pull_requests"] != "write" {
		t.Error("expected pull_requests:write")
	}
	if _, ok := m.Permissions["members"]; ok {
		t.Error("should not have org/members permission")
	}
	if m.RedirectURL != "http://127.0.0.1:12345/callback" {
		t.Errorf("unexpected redirect_url: %s", m.RedirectURL)
	}
}

func TestHTMLEscape(t *testing.T) {
	in := `{"name":"bot","url":"https://example.com"}`
	out := htmlEscape(in)
	if out != `{&#34;name&#34;:&#34;bot&#34;,&#34;url&#34;:&#34;https://example.com&#34;}` {
		t.Errorf("unexpected: %s", out)
	}
}

func TestInstallURL(t *testing.T) {
	cfg := &Config{Slug: "my-release-bot"}
	got := InstallURL(cfg)
	want := "https://github.com/apps/my-release-bot/installations/new"
	if got != want {
		t.Errorf("InstallURL = %q, want %q", got, want)
	}
}
