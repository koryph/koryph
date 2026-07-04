// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/release"
)

// --- test helpers -----------------------------------------------------------

// releaseConfig returns a minimal valid ReleaseConfig for use in tests.
func releaseConfig() *project.ReleaseConfig {
	return &project.ReleaseConfig{
		Type:         "go",
		ArtifactsDir: "dist",
		Build: project.ReleaseBuildConfig{
			Commands: []string{"go build ./..."},
		},
	}
}

// addReleaseBlock writes a new koryph.project.json that includes a release
// block to the given repo root (must already have a valid project config).
func addReleaseBlock(t *testing.T, root string, rc *project.ReleaseConfig) {
	t.Helper()
	cfgPath := filepath.Join(root, project.ConfigFileName)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("addReleaseBlock: read config: %v", err)
	}
	var cfg project.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("addReleaseBlock: unmarshal config: %v", err)
	}
	cfg.Release = rc
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("addReleaseBlock: marshal config: %v", err)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		t.Fatalf("addReleaseBlock: write config: %v", err)
	}
}

// writeCallerWorkflow installs a caller workflow file at
// .github/workflows/release.yml in root.
func writeCallerWorkflow(t *testing.T, root string, content []byte) {
	t.Helper()
	wfDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatalf("writeCallerWorkflow: mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "release.yml"), content, 0o644); err != nil {
		t.Fatalf("writeCallerWorkflow: write: %v", err)
	}
}

// projectOptsWithRelease builds injectable ProjectOptions that stub out the
// three gh-dependent injections so tests never hit real processes.
func projectOptsWithRelease(root, ownerRepo string, secretNames []string, secretsErr error, approvalEnabled bool, approvalErr error) ProjectOptions {
	opts := projectOpts(root)
	opts.GitHubRepo = func(_ string) (string, error) { return ownerRepo, nil }
	opts.GHSecretList = func(_ string) ([]string, error) { return secretNames, secretsErr }
	opts.GHActionsPermissions = func(_ string) (bool, error) { return approvalEnabled, approvalErr }
	return opts
}

// --- parseGitHubSlug --------------------------------------------------------

func TestParseGitHubSlug_HTTPS(t *testing.T) {
	cases := []struct {
		input string
		want  string
		ok    bool
	}{
		{"https://github.com/owner/repo.git", "owner/repo", true},
		{"https://github.com/owner/repo", "owner/repo", true},
		{"https://gitlab.com/owner/repo.git", "", false},
		{"https://github.com/owner", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, gotOK := parseGitHubSlug(tc.input)
		if got != tc.want || gotOK != tc.ok {
			t.Errorf("parseGitHubSlug(%q) = (%q, %v), want (%q, %v)", tc.input, got, gotOK, tc.want, tc.ok)
		}
	}
}

func TestParseGitHubSlug_SSH(t *testing.T) {
	cases := []struct {
		input string
		want  string
		ok    bool
	}{
		{"git@github.com:owner/repo.git", "owner/repo", true},
		{"git@github.com:owner/repo", "owner/repo", true},
		{"git@gitlab.com:owner/repo.git", "", false},
	}
	for _, tc := range cases {
		got, gotOK := parseGitHubSlug(tc.input)
		if got != tc.want || gotOK != tc.ok {
			t.Errorf("parseGitHubSlug(%q) = (%q, %v), want (%q, %v)", tc.input, got, gotOK, tc.want, tc.ok)
		}
	}
}

// --- release-block ----------------------------------------------------------

func TestReleaseBlockBothAbsent(t *testing.T) {
	root := fabricateProject(t)
	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameReleaseBlock)
	if f.Level != LevelOK {
		t.Errorf("release-block: got %s %q, want ok", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "not configured") {
		t.Errorf("release-block: unexpected message %q", f.Message)
	}
}

func TestReleaseBlockBothPresent(t *testing.T) {
	root := fabricateProject(t)
	rc := releaseConfig()
	addReleaseBlock(t, root, rc)
	expected, err := release.RenderCallerWorkflow(rc)
	if err != nil {
		t.Fatal(err)
	}
	writeCallerWorkflow(t, root, expected)

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseBlock)
	if f.Level != LevelOK {
		t.Errorf("release-block: got %s %q, want ok", f.Level, f.Message)
	}
}

func TestReleaseBlockPresentWorkflowMissing(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseBlock)
	if f.Level != LevelWarn {
		t.Errorf("release-block: got %s %q, want warn", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "koryph release setup") {
		t.Errorf("release-block: message should mention koryph release setup, got %q", f.Message)
	}
}

func TestReleaseBlockWorkflowPresentBlockMissing(t *testing.T) {
	root := fabricateProject(t)
	// No release block in project config; install workflow manually.
	writeCallerWorkflow(t, root, []byte("# orphan workflow\n"))

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseBlock)
	if f.Level != LevelWarn {
		t.Errorf("release-block: got %s %q, want warn", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "no release block") {
		t.Errorf("release-block: message should mention missing release block, got %q", f.Message)
	}
}

// --- release-workflow-drift -------------------------------------------------

func TestReleaseWorkflowDriftNoReleaseBlock(t *testing.T) {
	root := fabricateProject(t)
	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseWorkflow)
	if f.Level != LevelOK {
		t.Errorf("workflow-drift: got %s, want ok", f.Level)
	}
	if !strings.Contains(f.Message, "skipped") {
		t.Errorf("workflow-drift: expected 'skipped' in message, got %q", f.Message)
	}
}

func TestReleaseWorkflowDriftNoWorkflowFile(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())
	// No workflow file on disk.
	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseWorkflow)
	if f.Level != LevelOK {
		t.Errorf("workflow-drift: got %s, want ok", f.Level)
	}
}

func TestReleaseWorkflowDriftCurrentTemplate(t *testing.T) {
	root := fabricateProject(t)
	rc := releaseConfig()
	addReleaseBlock(t, root, rc)
	expected, err := release.RenderCallerWorkflow(rc)
	if err != nil {
		t.Fatal(err)
	}
	writeCallerWorkflow(t, root, expected)

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseWorkflow)
	if f.Level != LevelOK {
		t.Errorf("workflow-drift: got %s %q, want ok", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "matches current template") {
		t.Errorf("workflow-drift: unexpected message %q", f.Message)
	}
}

func TestReleaseWorkflowDriftStaleContent(t *testing.T) {
	root := fabricateProject(t)
	rc := releaseConfig()
	addReleaseBlock(t, root, rc)
	// Install a modified workflow (simulates drift after template update).
	writeCallerWorkflow(t, root, []byte("# stale custom content\nname: release\n"))

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseWorkflow)
	if f.Level != LevelWarn {
		t.Errorf("workflow-drift: got %s %q, want warn", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "koryph release setup") {
		t.Errorf("workflow-drift: message should mention koryph release setup, got %q", f.Message)
	}
}

// --- release-bot-secrets ----------------------------------------------------

func TestReleaseBotSecretsNoReleaseBlock(t *testing.T) {
	root := fabricateProject(t)
	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseBotSecrets)
	if f.Level != LevelOK {
		t.Errorf("bot-secrets: got %s, want ok", f.Level)
	}
}

func TestReleaseBotSecretsNoRepoSlug(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOpts(root)
	opts.GitHubRepo = func(_ string) (string, error) { return "", nil }
	opts.GHSecretList = func(_ string) ([]string, error) { return nil, nil }
	opts.GHActionsPermissions = func(_ string) (bool, error) { return false, nil }
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseBotSecrets)
	if f.Level != LevelWarn {
		t.Errorf("bot-secrets: got %s, want warn when repo slug empty", f.Level)
	}
}

func TestReleaseBotSecretsAPIError(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo",
		nil, fmt.Errorf("HTTP 403: must have admin rights"),
		false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameReleaseBotSecrets)
	// API error → graceful skip (LevelOK, not LevelError/LevelWarn).
	if f.Level != LevelOK {
		t.Errorf("bot-secrets: got %s %q, want ok on API error", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "best-effort check skipped") {
		t.Errorf("bot-secrets: expected 'best-effort check skipped', got %q", f.Message)
	}
}

func TestReleaseBotSecretsBothPresent(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	secrets := []string{"RELEASE_BOT_APP_ID", "RELEASE_BOT_PRIVATE_KEY", "OTHER_SECRET"}
	opts := projectOptsWithRelease(root, "owner/repo", secrets, nil, true, nil)
	r, _ := RunProject(opts)
	fs := findAllChecks(r, checkNameReleaseBotSecrets)
	for _, f := range fs {
		if f.Level != LevelOK {
			t.Errorf("bot-secrets: got %s %q, want ok", f.Level, f.Message)
		}
	}
	if len(fs) != 2 {
		t.Errorf("bot-secrets: expected 2 findings (one per secret), got %d", len(fs))
	}
}

func TestReleaseBotSecretsOnePresent(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	secrets := []string{"RELEASE_BOT_APP_ID"} // private key missing
	opts := projectOptsWithRelease(root, "owner/repo", secrets, nil, false, nil)
	r, _ := RunProject(opts)
	fs := findAllChecks(r, checkNameReleaseBotSecrets)
	if len(fs) != 2 {
		t.Fatalf("bot-secrets: expected 2 findings, got %d", len(fs))
	}
	counts := map[Level]int{}
	for _, f := range fs {
		counts[f.Level]++
	}
	if counts[LevelOK] != 1 || counts[LevelWarn] != 1 {
		t.Errorf("bot-secrets: want 1 ok + 1 warn, got ok=%d warn=%d", counts[LevelOK], counts[LevelWarn])
	}
}

func TestReleaseBotSecretsBothMissing(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo", []string{}, nil, false, nil)
	r, _ := RunProject(opts)
	fs := findAllChecks(r, checkNameReleaseBotSecrets)
	for _, f := range fs {
		if f.Level != LevelWarn {
			t.Errorf("bot-secrets: got %s %q, want warn", f.Level, f.Message)
		}
		if !strings.Contains(f.Message, "provision-release-bot.sh") {
			t.Errorf("bot-secrets: expected provision hint, got %q", f.Message)
		}
	}
}

// --- actions-approval -------------------------------------------------------

func TestActionsApprovalNoReleaseBlock(t *testing.T) {
	root := fabricateProject(t)
	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameActionsApproval)
	if f.Level != LevelOK {
		t.Errorf("actions-approval: got %s, want ok", f.Level)
	}
}

func TestActionsApprovalNoRepoSlug(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOpts(root)
	opts.GitHubRepo = func(_ string) (string, error) { return "", nil }
	opts.GHSecretList = func(_ string) ([]string, error) { return nil, nil }
	opts.GHActionsPermissions = func(_ string) (bool, error) { return false, nil }
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameActionsApproval)
	if f.Level != LevelWarn {
		t.Errorf("actions-approval: got %s, want warn when no slug", f.Level)
	}
}

func TestActionsApprovalAPIError(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo",
		nil, nil,
		false, fmt.Errorf("HTTP 403"))
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameActionsApproval)
	if f.Level != LevelOK {
		t.Errorf("actions-approval: got %s, want ok on API error", f.Level)
	}
	if !strings.Contains(f.Message, "best-effort check skipped") {
		t.Errorf("actions-approval: expected 'best-effort check skipped', got %q", f.Message)
	}
}

func TestActionsApprovalEnabled(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, true, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameActionsApproval)
	if f.Level != LevelOK {
		t.Errorf("actions-approval: got %s %q, want ok", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "enabled") {
		t.Errorf("actions-approval: expected 'enabled', got %q", f.Message)
	}
}

func TestActionsApprovalDisabled(t *testing.T) {
	root := fabricateProject(t)
	addReleaseBlock(t, root, releaseConfig())

	opts := projectOptsWithRelease(root, "owner/repo", nil, nil, false, nil)
	r, _ := RunProject(opts)
	f := findCheck(r, checkNameActionsApproval)
	if f.Level != LevelWarn {
		t.Errorf("actions-approval: got %s %q, want warn", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "disabled") {
		t.Errorf("actions-approval: expected 'disabled' in message, got %q", f.Message)
	}
	if !strings.Contains(f.Message, "provision-release-bot.sh") {
		t.Errorf("actions-approval: expected provision hint, got %q", f.Message)
	}
}

// --- end-to-end: fully-configured project -----------------------------------

// TestReleaseInfraFullyConfigured verifies that a fully set-up project
// (release block + matching workflow + both secrets + approval on) yields
// all OK findings.
func TestReleaseInfraFullyConfigured(t *testing.T) {
	root := fabricateProject(t)
	rc := releaseConfig()
	addReleaseBlock(t, root, rc)
	expected, err := release.RenderCallerWorkflow(rc)
	if err != nil {
		t.Fatal(err)
	}
	writeCallerWorkflow(t, root, expected)

	secrets := []string{"RELEASE_BOT_APP_ID", "RELEASE_BOT_PRIVATE_KEY"}
	opts := projectOptsWithRelease(root, "owner/repo", secrets, nil, true, nil)
	r, _ := RunProject(opts)

	for _, name := range []string{
		checkNameReleaseBlock,
		checkNameReleaseWorkflow,
		checkNameReleaseBotSecrets,
		checkNameActionsApproval,
	} {
		for _, f := range findAllChecks(r, name) {
			if f.Level != LevelOK {
				t.Errorf("%s: got %s %q, want ok", name, f.Level, f.Message)
			}
		}
	}
}
