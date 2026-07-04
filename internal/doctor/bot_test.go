// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/bot"
)

// --- bot-credentials check --------------------------------------------------

// fabricateProject is defined in project_test.go; it builds a minimal valid
// project root with a koryph.project.json.

func TestBotCredentialsNoReleaseBlock(t *testing.T) {
	root := fabricateProject(t)
	opts := projectOpts(root)
	// No release block → check should be skipped (LevelOK).
	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameBotCredentials)
	if f.Level != LevelOK {
		t.Errorf("bot-credentials: got %s %q, want ok when release not configured", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "skipped") {
		t.Errorf("bot-credentials: expected 'skipped' in message, got %q", f.Message)
	}
}

func TestBotCredentialsNoBots(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	opts.BotCredentialCheck = func() ([]bot.CredentialFinding, error) {
		return nil, nil // no bots stored
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameBotCredentials)
	if f.Level != LevelOK {
		t.Errorf("bot-credentials: got %s, want ok when no bots", f.Level)
	}
	if !strings.Contains(f.Message, "koryph bot create") {
		t.Errorf("bot-credentials: expected create hint, got %q", f.Message)
	}
}

func TestBotCredentialsValidBot(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	opts.BotCredentialCheck = func() ([]bot.CredentialFinding, error) {
		return []bot.CredentialFinding{
			{Name: "good-bot", Level: bot.CheckOK, Message: "credentials ok (app_id=42 owner=octocat)"},
		}, nil
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameBotCredentials)
	if f.Level != LevelOK {
		t.Errorf("bot-credentials: got %s %q, want ok for valid bot", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "good-bot") {
		t.Errorf("bot-credentials: expected bot name in message, got %q", f.Message)
	}
}

func TestBotCredentialsCorruptBot(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	opts.BotCredentialCheck = func() ([]bot.CredentialFinding, error) {
		return []bot.CredentialFinding{
			{Name: "bad-bot", Level: bot.CheckFail, Message: "invalid PEM: no PEM block found"},
		}, nil
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameBotCredentials)
	// Corrupt PEM is a warning in doctor (not a hard error).
	if f.Level != LevelWarn {
		t.Errorf("bot-credentials: got %s %q, want warn for corrupt PEM", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "bad-bot") {
		t.Errorf("bot-credentials: expected bot name in message, got %q", f.Message)
	}
}

func TestBotCredentialsMixedBots(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	opts.BotCredentialCheck = func() ([]bot.CredentialFinding, error) {
		return []bot.CredentialFinding{
			{Name: "good-bot", Level: bot.CheckOK, Message: "credentials ok (app_id=1 owner=x)"},
			{Name: "bad-bot", Level: bot.CheckFail, Message: "invalid PEM: no PEM block"},
		}, nil
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	all := findAllChecks(r, checkNameBotCredentials)
	if len(all) != 2 {
		t.Fatalf("bot-credentials: expected 2 findings, got %d", len(all))
	}
	counts := map[Level]int{}
	for _, f := range all {
		counts[f.Level]++
	}
	if counts[LevelOK] != 1 || counts[LevelWarn] != 1 {
		t.Errorf("bot-credentials: want 1 ok + 1 warn, got ok=%d warn=%d", counts[LevelOK], counts[LevelWarn])
	}
}

// --- end-to-end: project config includes release block + bot credentials ----

func TestReleaseInfraWithBot(t *testing.T) {
	root := fabricateProject(t)
	rc := releaseConfig()
	addReleaseBlock(t, root, rc)

	secrets := []string{"RELEASE_BOT_APP_ID", "RELEASE_BOT_PRIVATE_KEY"}
	opts := projectOptsWithRelease(root, "owner/repo", secrets, nil, true, nil)
	opts.BotCredentialCheck = func() ([]bot.CredentialFinding, error) {
		return []bot.CredentialFinding{
			{Name: "my-bot", Level: bot.CheckOK, Message: "credentials ok (app_id=1 owner=x)"},
		}, nil
	}

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameBotCredentials)
	if f.Level != LevelOK {
		t.Errorf("bot-credentials: got %s %q, want ok", f.Level, f.Message)
	}
}
