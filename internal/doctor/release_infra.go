// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

// release_infra.go — per-project release-infrastructure checks
//
// Five checks are grouped under the "release-infra" umbrella and called from
// RunProject after the core structural checks:
//
//  1. release-block         — release block ↔ caller workflow consistency
//  2. release-workflow-drift — installed workflow vs. current template
//  3. release-bot-secrets   — RELEASE_BOT_APP_ID/PRIVATE_KEY via gh api
//  4. actions-approval      — can_approve_pull_request_reviews via gh api
//  5. bot-credentials       — offline PEM validity for stored bots
//
// Checks 3 and 4 use gh(1) under the hood; they degrade gracefully (LevelOK
// with a "skipped" note) when gh is absent, unauthenticated, or lacks admin
// access — so they never turn a clean project into a red report just because
// the operator ran the doctor locally without credentials.
//
// Check 5 is purely offline (no network) and only runs when the project has a
// release block — it surfaces corrupted bot credentials before the operator
// tries to use them.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/bot"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/release"
)

// --- check name constants ---------------------------------------------------

const (
	checkNameReleaseBlock      = "release-block"
	checkNameReleaseWorkflow   = "release-workflow-drift"
	checkNameReleaseBotSecrets = "release-bot-secrets"
	checkNameActionsApproval   = "actions-approval"
	checkNameBotCredentials    = "bot-credentials"
)

// callerWorkflowPath returns the conventional path of the caller workflow
// relative to repoRoot.
func callerWorkflowPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".github", "workflows", "release.yml")
}

// checkReleaseInfra is the top-level dispatcher; it is called from RunProject.
// cfg may be nil (project config failed to load), in which case the checks that
// require config are skipped gracefully.
func checkReleaseInfra(opts ProjectOptions, repoRoot string, cfg *project.Config) []Finding {
	var out []Finding
	out = append(out, checkReleaseBlock(repoRoot, cfg)...)
	out = append(out, checkReleaseWorkflowDrift(repoRoot, cfg)...)
	out = append(out, checkReleaseBotSecrets(opts, repoRoot, cfg)...)
	out = append(out, checkActionsApproval(opts, repoRoot, cfg)...)
	out = append(out, checkBotCredentials(opts, cfg)...)
	return out
}

// --- 1. release-block -------------------------------------------------------

// checkReleaseBlock verifies that the presence of the release block in
// koryph.project.json is consistent with the presence of the caller workflow
// on disk:
//
//   - both present → ok
//   - block present, workflow absent → warn (run `koryph release setup`)
//   - workflow present, block absent → warn (add release block or remove file)
//   - both absent  → ok (release not set up)
func checkReleaseBlock(repoRoot string, cfg *project.Config) []Finding {
	wfPath := callerWorkflowPath(repoRoot)
	wfPresent := releaseFileExists(wfPath)
	rcPresent := cfg != nil && cfg.Release != nil

	switch {
	case rcPresent && wfPresent:
		return []Finding{{
			Check:   checkNameReleaseBlock,
			Level:   LevelOK,
			Message: "release block and caller workflow both present",
		}}
	case rcPresent && !wfPresent:
		return []Finding{{
			Check:   checkNameReleaseBlock,
			Level:   LevelWarn,
			Message: "release block present in koryph.project.json but .github/workflows/release.yml is missing (run `koryph release setup`)",
		}}
	case !rcPresent && wfPresent:
		return []Finding{{
			Check:   checkNameReleaseBlock,
			Level:   LevelWarn,
			Message: ".github/workflows/release.yml present but no release block in koryph.project.json (add a release block or remove the workflow file)",
		}}
	default: // neither present
		return []Finding{{
			Check:   checkNameReleaseBlock,
			Level:   LevelOK,
			Message: "release not configured (no release block, no caller workflow)",
		}}
	}
}

// --- 2. release-workflow-drift ----------------------------------------------

// checkReleaseWorkflowDrift compares the installed caller workflow against the
// bytes that would be rendered from the project's current release config.
// Drift means the file has diverged from the template (e.g. the template was
// updated, or the release config was changed after `koryph release setup` last
// ran).
//
// Skipped (LevelOK) when:
//   - cfg.Release is nil (no release block — release-block check covers this)
//   - the caller workflow does not exist on disk
func checkReleaseWorkflowDrift(repoRoot string, cfg *project.Config) []Finding {
	if cfg == nil || cfg.Release == nil {
		return []Finding{{
			Check:   checkNameReleaseWorkflow,
			Level:   LevelOK,
			Message: "release not configured; workflow drift check skipped",
		}}
	}

	wfPath := callerWorkflowPath(repoRoot)
	onDisk, err := os.ReadFile(wfPath)
	if os.IsNotExist(err) {
		// Already flagged by release-block check; skip here to avoid noise.
		return []Finding{{
			Check:   checkNameReleaseWorkflow,
			Level:   LevelOK,
			Message: "caller workflow absent; drift check skipped (see release-block check)",
		}}
	}
	if err != nil {
		return []Finding{{
			Check:   checkNameReleaseWorkflow,
			Level:   LevelWarn,
			Message: fmt.Sprintf("read %s: %v", wfPath, err),
		}}
	}

	expected, err := release.RenderCallerWorkflow(cfg.Release)
	if err != nil {
		return []Finding{{
			Check:   checkNameReleaseWorkflow,
			Level:   LevelWarn,
			Message: fmt.Sprintf("render caller workflow template: %v", err),
		}}
	}

	diskHash := sha256.Sum256(onDisk)
	wantHash := sha256.Sum256(expected)
	if diskHash == wantHash {
		return []Finding{{
			Check:   checkNameReleaseWorkflow,
			Level:   LevelOK,
			Message: "caller workflow matches current template",
		}}
	}
	return []Finding{{
		Check:   checkNameReleaseWorkflow,
		Level:   LevelWarn,
		Message: "caller workflow differs from current template (run `koryph release setup` to update .github/workflows/release.yml)",
	}}
}

// --- 3. release-bot-secrets -------------------------------------------------

// checkReleaseBotSecrets checks that RELEASE_BOT_APP_ID and
// RELEASE_BOT_PRIVATE_KEY are set on the project's GitHub repository. The
// check uses `gh secret list` which requires repository admin access; it
// degrades gracefully (LevelOK with a note) when gh is absent, the API call
// fails, or the repository slug cannot be determined.
//
// Skipped (LevelOK) when cfg.Release is nil.
func checkReleaseBotSecrets(opts ProjectOptions, repoRoot string, cfg *project.Config) []Finding {
	if cfg == nil || cfg.Release == nil {
		return []Finding{{
			Check:   checkNameReleaseBotSecrets,
			Level:   LevelOK,
			Message: "release not configured; secrets check skipped",
		}}
	}

	ownerRepo, err := opts.gitHubRepo(repoRoot)
	if err != nil || ownerRepo == "" {
		return []Finding{{
			Check:   checkNameReleaseBotSecrets,
			Level:   LevelWarn,
			Message: "cannot determine GitHub repo slug (add a github intake source or verify git remote origin is a github.com URL)",
		}}
	}

	names, err := opts.ghSecretList(ownerRepo)
	if err != nil {
		// API failure is expected without admin access — degrade gracefully.
		return []Finding{{
			Check:   checkNameReleaseBotSecrets,
			Level:   LevelOK,
			Message: ownerRepo + ": cannot read secrets (gh api error — likely no admin access; best-effort check skipped)",
		}}
	}

	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}

	appIDPresent := nameSet["RELEASE_BOT_APP_ID"]
	keyPresent := nameSet["RELEASE_BOT_PRIVATE_KEY"]

	// When both secrets are absent the workflow uses the GITHUB_TOKEN fallback
	// (bot-less mode). This is a valid, supported configuration — checks will
	// fire when the operator runs `koryph release kick` before each release.
	// Report this as a contextual warning rather than a bot-setup error so it
	// is clear that releases still work; the operator just needs one extra step.
	if !appIDPresent && !keyPresent {
		return []Finding{{
			Check:   checkNameReleaseBotSecrets,
			Level:   LevelWarn,
			Message: ownerRepo + ": bot-less (GITHUB_TOKEN fallback active) — run `koryph release kick --repo " + ownerRepo + "` before each release to trigger checks (or run `koryph bot attach` to configure a bot)",
		}}
	}

	var out []Finding
	for _, want := range []string{"RELEASE_BOT_APP_ID", "RELEASE_BOT_PRIVATE_KEY"} {
		if nameSet[want] {
			out = append(out, Finding{
				Check:   checkNameReleaseBotSecrets,
				Level:   LevelOK,
				Message: ownerRepo + ": " + want + ": present",
			})
		} else {
			out = append(out, Finding{
				Check:   checkNameReleaseBotSecrets,
				Level:   LevelWarn,
				Message: ownerRepo + ": " + want + ": missing (run `koryph bot attach --name <name> --repo " + ownerRepo + "` to configure)",
			})
		}
	}
	return out
}

// --- 4. actions-approval ----------------------------------------------------

// checkActionsApproval checks that the GitHub Actions
// can_approve_pull_request_reviews toggle is enabled on the project's
// repository. This toggle is required for the release bot to approve its own
// PRs via `gh api .../actions/permissions/workflow`. The check degrades
// gracefully when gh is absent or the API call fails.
//
// Skipped (LevelOK) when cfg.Release is nil.
func checkActionsApproval(opts ProjectOptions, repoRoot string, cfg *project.Config) []Finding {
	if cfg == nil || cfg.Release == nil {
		return []Finding{{
			Check:   checkNameActionsApproval,
			Level:   LevelOK,
			Message: "release not configured; Actions approval check skipped",
		}}
	}

	ownerRepo, err := opts.gitHubRepo(repoRoot)
	if err != nil || ownerRepo == "" {
		return []Finding{{
			Check:   checkNameActionsApproval,
			Level:   LevelWarn,
			Message: "cannot determine GitHub repo slug (add a github intake source or verify git remote origin is a github.com URL)",
		}}
	}

	enabled, err := opts.ghActionsPermissions(ownerRepo)
	if err != nil {
		// API failure is expected without admin — degrade gracefully.
		return []Finding{{
			Check:   checkNameActionsApproval,
			Level:   LevelOK,
			Message: ownerRepo + ": cannot read Actions permissions (gh api error — best-effort check skipped)",
		}}
	}

	if enabled {
		return []Finding{{
			Check:   checkNameActionsApproval,
			Level:   LevelOK,
			Message: ownerRepo + ": Actions can_approve_pull_request_reviews: enabled",
		}}
	}
	return []Finding{{
		Check:   checkNameActionsApproval,
		Level:   LevelWarn,
		Message: ownerRepo + ": Actions can_approve_pull_request_reviews: disabled (run `koryph bot attach --name <name> --repo " + ownerRepo + "` to enable)",
	}}
}

// --- default implementations for injectable functions -----------------------

// defaultGitHubRepo derives "owner/repo" by running
// `git -C <repoRoot> remote get-url origin` and parsing the result.
// Returns ("", nil) for non-GitHub remotes (not an error — just not applicable).
func defaultGitHubRepo(repoRoot string) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	slug, _ := parseGitHubSlug(strings.TrimSpace(string(out)))
	return slug, nil
}

// defaultGHSecretList runs `gh secret list --repo <ownerRepo> --jq '.[].name'`
// and returns the list of secret names.
func defaultGHSecretList(ownerRepo string) ([]string, error) {
	out, err := exec.Command("gh", "secret", "list",
		"--repo", ownerRepo,
		"--jq", ".[].name",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("gh secret list: %w", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// defaultGHActionsPermissions runs
// `gh api /repos/<ownerRepo>/actions/permissions/workflow --jq '.can_approve_pull_request_reviews'`
// and returns whether the toggle is enabled.
func defaultGHActionsPermissions(ownerRepo string) (bool, error) {
	out, err := exec.Command("gh", "api",
		"/repos/"+ownerRepo+"/actions/permissions/workflow",
		"--jq", ".can_approve_pull_request_reviews",
	).Output()
	if err != nil {
		return false, fmt.Errorf("gh api actions permissions: %w", err)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// parseGitHubSlug parses a GitHub remote URL (HTTPS or SSH) into the
// "owner/repo" slug. Returns ("", false) for non-GitHub URLs.
func parseGitHubSlug(remoteURL string) (string, bool) {
	u := strings.TrimSuffix(strings.TrimSpace(remoteURL), ".git")
	// HTTPS: https://github.com/owner/repo
	if after, ok := strings.CutPrefix(u, "https://github.com/"); ok {
		if parts := strings.SplitN(after, "/", 2); len(parts) == 2 &&
			parts[0] != "" && parts[1] != "" {
			return parts[0] + "/" + parts[1], true
		}
	}
	// SSH: git@github.com:owner/repo
	if after, ok := strings.CutPrefix(u, "git@github.com:"); ok {
		if parts := strings.SplitN(after, "/", 2); len(parts) == 2 &&
			parts[0] != "" && parts[1] != "" {
			return parts[0] + "/" + parts[1], true
		}
	}
	return "", false
}

// releaseFileExists is a thin os.Stat wrapper used by release-infra checks
// (kept separate from the asset-drift logic to avoid coupling).
func releaseFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- 5. bot-credentials (offline) -------------------------------------------

// checkBotCredentials is a purely offline check that verifies the PEM stored
// in each ~/.koryph/bots/*.json file can produce a structurally valid JWT.
// It only runs when the project has a release block (when release is not
// configured, bot credentials are not relevant to the project). The check
// degrades gracefully (LevelOK) when no bots are stored at all.
//
// The injectable BotCredentialCheck field on ProjectOptions allows tests to
// inject a fake list of credential findings without touching the real filesystem.
func checkBotCredentials(opts ProjectOptions, cfg *project.Config) []Finding {
	if cfg == nil || cfg.Release == nil {
		return []Finding{{
			Check:   checkNameBotCredentials,
			Level:   LevelOK,
			Message: "release not configured; bot credential check skipped",
		}}
	}

	// Use injectable function if provided (test seam); fall back to the real
	// check functions which read ~/.koryph/bots/. The default aggregates both
	// GitHub bots (*.json) and GitLab bots (*.gitlab.json).
	listFn := opts.BotCredentialCheck
	if listFn == nil {
		listFn = func() ([]bot.CredentialFinding, error) {
			ghFindings, ghErr := bot.CheckCredentials()
			glFindings, glErr := bot.CheckGitLabCredentials()
			combined := append(ghFindings, glFindings...)
			if ghErr != nil {
				return combined, ghErr
			}
			return combined, glErr
		}
	}

	credFindings, err := listFn()
	if err != nil {
		return []Finding{{
			Check:   checkNameBotCredentials,
			Level:   LevelWarn,
			Message: fmt.Sprintf("list bots: %v", err),
		}}
	}
	if len(credFindings) == 0 {
		return []Finding{{
			Check:   checkNameBotCredentials,
			Level:   LevelOK,
			Message: "no bots stored (run `koryph bot create` then `koryph bot attach --name N --repo OWNER/REPO`)",
		}}
	}

	var out []Finding
	for _, f := range credFindings {
		level := LevelOK
		if f.Level == bot.CheckFail {
			level = LevelWarn // corrupted credentials are a warning, not a hard error
		}
		out = append(out, Finding{
			Check:   checkNameBotCredentials,
			Level:   level,
			Message: fmt.Sprintf("bot %s: %s", f.Name, f.Message),
		})
	}
	return out
}
