// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckCredentials_NoBots(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	findings, err := CheckCredentials()
	if err != nil {
		t.Fatalf("CheckCredentials empty: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestCheckCredentials_ValidPEM(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	pemStr := generateTestPEM(t)
	cfg := &Config{Name: "good-bot", AppID: 42, Owner: "octocat", PEM: pemStr}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	findings, err := CheckCredentials()
	if err != nil {
		t.Fatalf("CheckCredentials: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Level != CheckOK {
		t.Errorf("finding level = %s, want ok; message: %s", f.Level, f.Message)
	}
	if f.Name != "good-bot" {
		t.Errorf("finding name = %q, want good-bot", f.Name)
	}
}

func TestCheckCredentials_InvalidPEM(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	// Write a bot with invalid PEM.
	cfg := &Config{Name: "bad-bot", AppID: 99, PEM: "garbage-pem"}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	findings, err := CheckCredentials()
	if err != nil {
		t.Fatalf("CheckCredentials: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Level != CheckFail {
		t.Errorf("finding level = %s, want fail", f.Level)
	}
	if !strings.Contains(f.Message, "invalid PEM") {
		t.Errorf("message should mention 'invalid PEM', got %q", f.Message)
	}
}

func TestCheckCredentials_CorruptJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	// Write a corrupt JSON file directly.
	if err := os.MkdirAll(filepath.Join(tmp, "bots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(tmp, "bots", "corrupt-bot.json"),
		[]byte("not json"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	// CheckCredentials loads the bot (which fails) and records a fail finding.
	findings, err := CheckCredentials()
	if err != nil {
		t.Fatalf("CheckCredentials: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Level != CheckFail {
		t.Errorf("corrupt JSON should be CheckFail, got %s", findings[0].Level)
	}
}

func TestCredentialFindingsFor_NotFound(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	_, err := CredentialFindingsFor("nonexistent")
	if err == nil {
		t.Fatal("expected error for missing bot, got nil")
	}
}

func TestCredentialFindingsFor_OK(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)

	pemStr := generateTestPEM(t)
	cfg := &Config{Name: "my-bot", AppID: 7, Owner: "me", PEM: pemStr}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	findings, err := CredentialFindingsFor("my-bot")
	if err != nil {
		t.Fatalf("CredentialFindingsFor: %v", err)
	}
	if len(findings) != 1 || findings[0].Level != CheckOK {
		t.Errorf("expected one ok finding, got %v", findings)
	}
}

func TestPrintCheckResults_ExitCodes(t *testing.T) {
	cases := []struct {
		findings []CheckFinding
		wantCode int
	}{
		{[]CheckFinding{{Check: "a", Level: CheckOK, Message: "ok"}}, 0},
		{[]CheckFinding{{Check: "a", Level: CheckWarn, Message: "warn"}}, 1},
		{[]CheckFinding{{Check: "a", Level: CheckFail, Message: "fail"}}, 2},
		{[]CheckFinding{
			{Check: "a", Level: CheckOK},
			{Check: "b", Level: CheckWarn},
		}, 1},
		{[]CheckFinding{
			{Check: "a", Level: CheckOK},
			{Check: "b", Level: CheckFail},
			{Check: "c", Level: CheckWarn},
		}, 2},
	}

	for _, tc := range cases {
		var w strings.Builder
		got := PrintCheckResults(&w, tc.findings)
		if got != tc.wantCode {
			t.Errorf("PrintCheckResults(%v) = %d, want %d", tc.findings, got, tc.wantCode)
		}
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	owner, repo, err := splitOwnerRepo("acme/widgets")
	if err != nil || owner != "acme" || repo != "widgets" {
		t.Errorf("splitOwnerRepo: got (%q, %q, %v), want (acme, widgets, nil)", owner, repo, err)
	}

	_, _, err = splitOwnerRepo("no-slash")
	if err == nil {
		t.Error("expected error for no-slash input")
	}

	_, _, err = splitOwnerRepo("/leading-slash")
	if err == nil {
		t.Error("expected error for leading slash")
	}
}
