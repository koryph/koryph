// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CompileGitHub translates forge-neutral Intents into GitHub-native desired-state
// files under ghDir:
//
//   - ghDir/rulesets/signed-commits.json  (when signing/force-push/deletion intents)
//   - ghDir/rulesets/pr-checks.json       (when require_approvals > 0)
//   - ghDir/repo-settings.json             (when any repo/security/actions intents)
//
// params may override or augment intent fields at call time; currently only
// "required_checks" (comma-separated check names) is recognised.
//
// The GitHub compiler reproduces the oss-solo-maintainer profile's rulesets and
// settings byte-identically (after normalisation) when called with the intents
// from that profile's manifest — see compile_github_test.go for the fixture lock.
func CompileGitHub(intents Intents, params map[string]string, ghDir string) error {
	// Resolve required_checks: runtime param overrides intents list.
	checks := intents.RequiredChecks
	if v := params["required_checks"]; v != "" {
		checks = nil
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				checks = append(checks, name)
			}
		}
	}

	rulesetsDir := filepath.Join(ghDir, "rulesets")

	// signed-commits ruleset ─────────────────────────────────────────────────
	if intents.RequireSignedCommits || intents.NoForcePush || intents.NoDeletion {
		if err := os.MkdirAll(rulesetsDir, 0o755); err != nil {
			return fmt.Errorf("posture: compile github: mkdir rulesets: %w", err)
		}
		rs := buildSignedCommitsRuleset(intents)
		data, err := json.MarshalIndent(rs, "", "  ")
		if err != nil {
			return fmt.Errorf("posture: compile github: marshal signed-commits: %w", err)
		}
		if err := os.WriteFile(filepath.Join(rulesetsDir, "signed-commits.json"), data, 0o644); err != nil {
			return fmt.Errorf("posture: compile github: write signed-commits: %w", err)
		}
	}

	// pr-checks ruleset ──────────────────────────────────────────────────────
	if intents.RequireApprovals > 0 {
		if err := os.MkdirAll(rulesetsDir, 0o755); err != nil {
			return fmt.Errorf("posture: compile github: mkdir rulesets: %w", err)
		}
		rs := buildPRChecksRuleset(intents, checks)
		data, err := json.MarshalIndent(rs, "", "  ")
		if err != nil {
			return fmt.Errorf("posture: compile github: marshal pr-checks: %w", err)
		}
		if err := os.WriteFile(filepath.Join(rulesetsDir, "pr-checks.json"), data, 0o644); err != nil {
			return fmt.Errorf("posture: compile github: write pr-checks: %w", err)
		}
	}

	// repo-settings.json ─────────────────────────────────────────────────────
	if hasRepoSettings(intents) {
		settings := buildGHRepoSettings(intents)
		data, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return fmt.Errorf("posture: compile github: marshal repo-settings: %w", err)
		}
		if err := os.WriteFile(filepath.Join(ghDir, "repo-settings.json"), data, 0o644); err != nil {
			return fmt.Errorf("posture: compile github: write repo-settings: %w", err)
		}
	}

	return nil
}

// ─── ruleset builders ────────────────────────────────────────────────────────

// ghRuleset is the GitHub ruleset desired-state structure (rulesets/*.json).
// Fields are ordered alphabetically to match the normalised form produced by
// normalizeRuleset, which re-marshals through map[string]interface{}.
// Arrays (bypass_actors, rules, allowed_merge_methods) keep their declared order.
type ghRuleset struct {
	BypassActors []ghBypassActor `json:"bypass_actors"`
	Conditions   ghConditions    `json:"conditions"`
	Enforcement  string          `json:"enforcement"`
	Name         string          `json:"name"`
	Rules        []ghRuleRaw     `json:"rules"`
	Target       string          `json:"target"`
}

type ghBypassActor struct {
	ActorID    int    `json:"actor_id"`
	ActorType  string `json:"actor_type"`
	BypassMode string `json:"bypass_mode"`
}

type ghConditions struct {
	RefName ghRefName `json:"ref_name"`
}

type ghRefName struct {
	Exclude []string `json:"exclude"`
	Include []string `json:"include"`
}

// ghRuleRaw holds one ruleset rule; Parameters is marshalled inline when non-nil.
type ghRuleRaw struct {
	Parameters interface{} `json:"parameters,omitempty"`
	Type       string      `json:"type"`
}

// ghPRParams is the parameters block for the pull_request rule.
// Fields are ordered alphabetically (normalised form).
type ghPRParams struct {
	AllowedMergeMethods            []string      `json:"allowed_merge_methods"`
	DismissStaleReviewsOnPush      bool          `json:"dismiss_stale_reviews_on_push"`
	RequireCodeOwnerReview         bool          `json:"require_code_owner_review"`
	RequireLastPushApproval        bool          `json:"require_last_push_approval"`
	RequiredApprovingReviewCount   int           `json:"required_approving_review_count"`
	RequiredReviewThreadResolution bool          `json:"required_review_thread_resolution"`
	RequiredReviewers              []interface{} `json:"required_reviewers"`
}

// ghStatusChecksParams is the parameters block for required_status_checks.
// Fields are ordered alphabetically (normalised form).
type ghStatusChecksParams struct {
	DoNotEnforceOnCreate             bool            `json:"do_not_enforce_on_create"`
	RequiredStatusChecks             []ghStatusCheck `json:"required_status_checks"`
	StrictRequiredStatusChecksPolicy bool            `json:"strict_required_status_checks_policy"`
}

type ghStatusCheck struct {
	Context string `json:"context"`
}

// defaultBranchCondition is the shared ref_name condition applied to all
// rulesets compiled from intents: targets only the default branch.
var defaultBranchCondition = ghConditions{
	RefName: ghRefName{
		Exclude: []string{},
		Include: []string{"~DEFAULT_BRANCH"},
	},
}

// buildSignedCommitsRuleset compiles the signed-commits ruleset from intents.
// The rule order (required_signatures → non_fast_forward → deletion) matches
// the existing oss-solo-maintainer golden fixture.
func buildSignedCommitsRuleset(intents Intents) ghRuleset {
	var rules []ghRuleRaw
	if intents.RequireSignedCommits {
		rules = append(rules, ghRuleRaw{Type: "required_signatures"})
	}
	if intents.NoForcePush {
		rules = append(rules, ghRuleRaw{Type: "non_fast_forward"})
	}
	if intents.NoDeletion {
		rules = append(rules, ghRuleRaw{Type: "deletion"})
	}
	return ghRuleset{
		BypassActors: []ghBypassActor{},
		Conditions:   defaultBranchCondition,
		Enforcement:  "active",
		Name:         "signed-commits",
		Rules:        rules,
		Target:       "branch",
	}
}

// buildPRChecksRuleset compiles the pr-checks ruleset from intents and the
// resolved required-checks list.  The rule order (pull_request first, then
// required_status_checks when checks are non-empty) matches the golden fixture.
func buildPRChecksRuleset(intents Intents, checks []string) ghRuleset {
	prParams := ghPRParams{
		AllowedMergeMethods:            []string{"merge", "squash", "rebase"},
		DismissStaleReviewsOnPush:      false,
		RequireCodeOwnerReview:         false,
		RequireLastPushApproval:        false,
		RequiredApprovingReviewCount:   intents.RequireApprovals,
		RequiredReviewThreadResolution: false,
		RequiredReviewers:              []interface{}{},
	}

	rules := []ghRuleRaw{
		{Type: "pull_request", Parameters: prParams},
	}

	if len(checks) > 0 {
		statusChecks := make([]ghStatusCheck, len(checks))
		for i, c := range checks {
			statusChecks[i] = ghStatusCheck{Context: c}
		}
		rules = append(rules, ghRuleRaw{
			Type: "required_status_checks",
			Parameters: ghStatusChecksParams{
				DoNotEnforceOnCreate:             false,
				RequiredStatusChecks:             statusChecks,
				StrictRequiredStatusChecksPolicy: true,
			},
		})
	}

	return ghRuleset{
		BypassActors: []ghBypassActor{
			{ActorID: 5, ActorType: "RepositoryRole", BypassMode: "always"},
		},
		Conditions:  defaultBranchCondition,
		Enforcement: "active",
		Name:        "pr-checks",
		Rules:       rules,
		Target:      "branch",
	}
}

// ─── settings builder ────────────────────────────────────────────────────────

// ghRepoSettingsFile mirrors repoSettingsFile for compiler output.
// The Descriptions map carries the same rationale as the oss-solo-maintainer
// profile so that describe output remains fully self-documenting after the
// intents path replaces file-based rendering.
type ghRepoSettingsFile struct {
	Repo                map[string]interface{} `json:"repo,omitempty"`
	SecurityAndAnalysis map[string]string      `json:"security_and_analysis,omitempty"`
	VulnerabilityAlerts *bool                  `json:"vulnerability_alerts,omitempty"`
	ActionsWorkflow     map[string]interface{} `json:"actions_workflow,omitempty"`
	Descriptions        map[string]string      `json:"descriptions,omitempty"`
}

// hasRepoSettings returns true when any repo/security/actions intent is set.
func hasRepoSettings(intents Intents) bool {
	return intents.AllowMergeCommit != nil ||
		intents.AllowSquashMerge != nil ||
		intents.AllowRebaseMerge != nil ||
		intents.AllowAutoMerge != nil ||
		intents.DeleteBranchOnMerge ||
		intents.AllowUpdateBranch ||
		intents.WebCommitSignoffRequired ||
		intents.SecretScanning ||
		intents.SecretScanningPushProtection ||
		intents.DependabotSecurityUpdates ||
		intents.VulnerabilityAlerts ||
		intents.ActionsDefaultPermissions != "" ||
		intents.ActionsCanApprovePRs
}

// buildGHRepoSettings compiles a GitHub repo-settings file from intents.
// The output reproduces the oss-solo-maintainer profile's repo-settings.json
// byte-identically (after jsonSortKeys normalisation) when called with that
// profile's intents.
func buildGHRepoSettings(intents Intents) ghRepoSettingsFile {
	sf := ghRepoSettingsFile{}

	// Repo flags section.
	repo := make(map[string]interface{})
	if intents.AllowMergeCommit != nil {
		repo["allow_merge_commit"] = *intents.AllowMergeCommit
	}
	if intents.AllowSquashMerge != nil {
		repo["allow_squash_merge"] = *intents.AllowSquashMerge
	}
	if intents.AllowRebaseMerge != nil {
		repo["allow_rebase_merge"] = *intents.AllowRebaseMerge
	}
	if intents.AllowAutoMerge != nil {
		repo["allow_auto_merge"] = *intents.AllowAutoMerge
	}
	if intents.DeleteBranchOnMerge {
		repo["delete_branch_on_merge"] = true
	}
	if intents.AllowUpdateBranch {
		repo["allow_update_branch"] = true
	}
	if intents.WebCommitSignoffRequired {
		repo["web_commit_signoff_required"] = true
	}
	if len(repo) > 0 {
		sf.Repo = repo
	}

	// Security & analysis section.
	sec := make(map[string]string)
	if intents.SecretScanning {
		sec["secret_scanning"] = "enabled"
	}
	if intents.SecretScanningPushProtection {
		sec["secret_scanning_push_protection"] = "enabled"
	}
	if intents.DependabotSecurityUpdates {
		sec["dependabot_security_updates"] = "enabled"
	}
	if len(sec) > 0 {
		sf.SecurityAndAnalysis = sec
	}

	// Vulnerability alerts.
	if intents.VulnerabilityAlerts {
		t := true
		sf.VulnerabilityAlerts = &t
	}

	// Actions workflow permissions.
	act := make(map[string]interface{})
	if intents.ActionsDefaultPermissions != "" {
		act["default_workflow_permissions"] = intents.ActionsDefaultPermissions
	}
	if intents.ActionsCanApprovePRs {
		act["can_approve_pull_request_reviews"] = true
	}
	if len(act) > 0 {
		sf.ActionsWorkflow = act
	}

	// Descriptions — same rationale as the oss-solo-maintainer profile file
	// so that describe output is self-documenting regardless of rendering path.
	sf.Descriptions = compiledGHDescriptions(intents)

	return sf
}

// compiledGHDescriptions returns the rationale map for a compiled GitHub
// settings file, reproducing the oss-solo-maintainer descriptions block.
func compiledGHDescriptions(intents Intents) map[string]string {
	d := make(map[string]string)
	if intents.AllowMergeCommit != nil {
		d["allow_merge_commit"] = "Prevents merge commits on the default branch, enforcing a clean bisectable history where every change is squash-merged or rebase-merged."
	}
	if intents.AllowSquashMerge != nil {
		d["allow_squash_merge"] = "Allows squash-merging pull requests into a single commit, keeping the default branch history linear and easy to read."
	}
	if intents.AllowRebaseMerge != nil {
		d["allow_rebase_merge"] = "Allows rebase-merging pull requests for a linear history without an extra merge commit."
	}
	if intents.AllowAutoMerge != nil {
		d["allow_auto_merge"] = "Disabled so pull requests cannot auto-merge without an explicit human action, even when all checks pass."
	}
	if intents.DeleteBranchOnMerge {
		d["delete_branch_on_merge"] = "Automatically deletes merged branches to prevent stale branch accumulation."
	}
	if intents.AllowUpdateBranch {
		d["allow_update_branch"] = "Allows contributors to rebase their PR branch on the latest base, reducing stale-base merge conflicts."
	}
	if intents.WebCommitSignoffRequired {
		d["web_commit_signoff_required"] = "Requires a DCO sign-off on all commits made through the GitHub web UI, matching the CLI requirement and ensuring contributor agreement for every change."
	}
	if intents.SecretScanning {
		d["secret_scanning"] = "Scans commits for secrets (API keys, tokens, passwords) and alerts the repository owner on detection, reducing the exposure window of accidentally committed credentials."
	}
	if intents.SecretScanningPushProtection {
		d["secret_scanning_push_protection"] = "Blocks pushes that contain detected secrets before they reach GitHub — prevents credential leaks at push time, not just after the fact."
	}
	if intents.DependabotSecurityUpdates {
		d["dependabot_security_updates"] = "Automatically opens pull requests to update dependencies with known CVEs, keeping the dependency graph free of publicly-disclosed vulnerabilities."
	}
	if intents.VulnerabilityAlerts {
		d["vulnerability_alerts"] = "Alerts the repository owner when a dependency has a known security vulnerability, giving early warning of exposure in the dependency graph."
	}
	if intents.ActionsDefaultPermissions != "" {
		d["default_workflow_permissions"] = "Limits GITHUB_TOKEN to read-only scope by default, requiring workflows to explicitly request write access and reducing blast radius of a compromised workflow."
	}
	if intents.ActionsCanApprovePRs {
		d["can_approve_pull_request_reviews"] = "Allows workflow runs to approve pull requests, required for automated release bots that must unblock their own PRs."
	}
	if len(d) == 0 {
		return nil
	}
	return d
}
