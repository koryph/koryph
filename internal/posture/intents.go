// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

// Intents is the forge-neutral desired-state core of a posture profile.
// Each field describes a security or workflow property the profile wants to
// enforce; forge compilers translate intents to forge-native controls:
//
//   - GitHub: rulesets (*.json) + repo-settings.json
//   - GitLab: protected branches + push rules + project settings (future)
//
// Boolean fields default to false (= "do not enforce this intent").
// Pointer-typed fields are used where false is a meaningful set-value and
// nil means "the profile does not manage this setting":
//
//	AllowMergeCommit: false → explicitly disallow merge commits
//	AllowMergeCommit: nil  → profile is silent; leave forge default alone
//
// Intents are stored in a profile's manifest.json under the "intents" key.
// The RequiredChecks list may also be supplemented or overridden at runtime
// via the --param required_checks=... flag.
type Intents struct {
	// PR gating
	//
	// RequireApprovals is the minimum number of pull-request approvals required
	// before a change can merge.  0 means no approval gate is added.
	RequireApprovals int `json:"require_approvals,omitempty"`

	// RequiredChecks is the list of CI check names that must pass before a PR
	// can merge.  The runtime --param required_checks=... flag overrides this list.
	RequiredChecks []string `json:"required_checks,omitempty"`

	// Branch integrity
	//
	// RequireSignedCommits enforces cryptographic commit signing (GPG/SSH) on
	// the default branch.
	RequireSignedCommits bool `json:"require_signed_commits,omitempty"`

	// NoForcePush prevents force-pushes (history rewrites) on the default branch.
	NoForcePush bool `json:"no_force_push,omitempty"`

	// NoDeletion prevents deletion of the default branch.
	NoDeletion bool `json:"no_deletion,omitempty"`

	// Security scanning
	//
	// SecretScanning enables secret scanning on the repository.
	SecretScanning bool `json:"secret_scanning,omitempty"`

	// SecretScanningPushProtection blocks pushes that contain detected secrets.
	SecretScanningPushProtection bool `json:"secret_scanning_push_protection,omitempty"`

	// DependabotSecurityUpdates enables automatic dependency security pull requests.
	DependabotSecurityUpdates bool `json:"dependabot_security_updates,omitempty"`

	// VulnerabilityAlerts enables Dependabot vulnerability alert notifications.
	VulnerabilityAlerts bool `json:"vulnerability_alerts,omitempty"`

	// Merge strategy
	//
	// AllowMergeCommit sets whether merge commits are permitted.
	// nil means the profile does not manage this setting.
	AllowMergeCommit *bool `json:"allow_merge_commit,omitempty"`

	// AllowSquashMerge sets whether squash merges are permitted.
	// nil means the profile does not manage this setting.
	AllowSquashMerge *bool `json:"allow_squash_merge,omitempty"`

	// AllowRebaseMerge sets whether rebase merges are permitted.
	// nil means the profile does not manage this setting.
	AllowRebaseMerge *bool `json:"allow_rebase_merge,omitempty"`

	// AllowAutoMerge sets whether auto-merge is permitted.
	// nil means the profile does not manage this setting.
	AllowAutoMerge *bool `json:"allow_auto_merge,omitempty"`

	// DeleteBranchOnMerge automatically deletes the source branch after merge.
	DeleteBranchOnMerge bool `json:"delete_branch_on_merge,omitempty"`

	// AllowUpdateBranch allows contributors to update a PR branch with the
	// latest base branch, reducing stale-base merge conflicts.
	AllowUpdateBranch bool `json:"allow_update_branch,omitempty"`

	// Web UI hygiene
	//
	// WebCommitSignoffRequired requires a DCO sign-off on commits made through
	// the forge web UI, matching the CLI requirement.
	WebCommitSignoffRequired bool `json:"web_commit_signoff_required,omitempty"`

	// CI token permissions
	//
	// ActionsDefaultPermissions sets the default GITHUB_TOKEN scope.
	// Supported values: "read", "write".  Empty means the profile does not
	// manage this setting.
	ActionsDefaultPermissions string `json:"actions_default_permissions,omitempty"`

	// ActionsCanApprovePRs allows CI workflows to approve pull requests, which
	// is required for automated release bots that must unblock their own PRs.
	ActionsCanApprovePRs bool `json:"actions_can_approve_prs,omitempty"`
}
