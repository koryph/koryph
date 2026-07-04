// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package forge defines the contract between koryph and hosted git forge
// services (GitHub, GitLab, …). Only the edges of koryph's core loop talk to
// a forge: hygiene/posture, PR/MR operations, releases, CI assets, and bot
// identity.
//
// Everything git-native (clones, worktrees, branches, fast-forward merges,
// commit signing, the green gate) is already forge-neutral and stays
// untouched — koryph's core loop never talks to a forge directly; only the
// edges do.
//
// # Usage
//
//	// Resolve a Forge from the global registry (populated by provider init()).
//	f, ok := forge.Default.Get(cfg.Forge)
//	if !ok {
//	    return fmt.Errorf("unknown forge %q", cfg.Forge)
//	}
//	caps := f.Capabilities()
//	if !caps.DraftReleases {
//	    // use assemble-then-create strategy
//	}
//
// # Providers
//
// Implementations live under internal/forge/github/ and internal/forge/gitlab/
// and self-register into [Default] via init(). This package is contract-only:
// no provider logic lives here.
//
// # Authentication
//
// Each provider names its own CLI binary and/or token source:
//   - GitHub: the gh CLI (KORYPH_GH_BIN env override).
//   - GitLab: the glab CLI (KORYPH_GLAB_BIN) or a PAT resolved through the
//     vault layer (same providers as signing / bot keys).
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// ErrUnsupported is returned by service methods that have no equivalent on
// the current forge provider.
var ErrUnsupported = errors.New("operation not supported on this forge")

// Forge is the top-level contract every git forge provider must implement.
// Obtain an instance via [Default].Get(name) or [Default].MustGet(name).
type Forge interface {
	// Name is the canonical provider identifier, e.g. "github" or "gitlab".
	// It must match the name used to register the provider in [Default].
	Name() string

	// Capabilities reports which optional features this provider supports.
	// Callers must consult the relevant flag before calling a capability-gated
	// method (e.g. [ReleaseService.CreateDraft] requires
	// Capabilities().DraftReleases == true).
	Capabilities() Capabilities

	// Repo returns the repository-settings service.
	Repo() RepoService

	// Protection returns the branch-protection / rulesets service.
	Protection() ProtectionService

	// PRs returns the pull-request / merge-request service.
	PRs() PRService

	// Secrets returns the CI secrets / variables service.
	Secrets() SecretsService

	// Releases returns the release publishing service.
	Releases() ReleaseService

	// CI returns the CI-asset rendering service.
	CI() CIService

	// Bot returns the bot-identity lifecycle service.
	Bot() BotService
}

// ---------- supporting value types -------------------------------------------

// RepoSettings is the subset of repository settings koryph manages.
// Providers map these fields to their native GET/PATCH schema.
type RepoSettings struct {
	// AllowMergeCommit, AllowSquashMerge, AllowRebaseMerge mirror the forge's
	// merge-button settings (GitHub) or equivalent merge strategies (GitLab).
	AllowMergeCommit bool `json:"allow_merge_commit"`
	AllowSquashMerge bool `json:"allow_squash_merge"`
	AllowRebaseMerge bool `json:"allow_rebase_merge"`

	// VulnerabilityAlertsEnabled mirrors GitHub Dependabot vulnerability
	// alerts (or the forge-equivalent security-advisory toggle).
	VulnerabilityAlertsEnabled bool `json:"vulnerability_alerts_enabled"`

	// ActionsWorkflowApprovals mirrors can_approve_pull_request_reviews on
	// GitHub; on other forges it controls the equivalent bot-approval toggle.
	ActionsWorkflowApprovals bool `json:"actions_workflow_approvals"`

	// RawFull is the full provider-native JSON response from the repository
	// metadata endpoint, populated by [RepoService.GetRaw].  It is excluded
	// from JSON encoding (provider-internal use only).  Callers that need
	// fine-grained per-field access — such as the posture normalization path —
	// unmarshal directly from this field.
	RawFull json.RawMessage `json:"-"`
}

// Ruleset is a branch-protection ruleset or equivalent protective policy.
type Ruleset struct {
	// ID is the provider's opaque identifier. It is a string to accommodate
	// both GitHub's int64 IDs and GitLab's string IDs.
	ID string `json:"id"`

	// Name is a human-readable ruleset label.
	Name string `json:"name"`

	// Raw is the full provider-native JSON representation, used for
	// read-compare-apply posture workflows.
	Raw []byte `json:"-"`
}

// PR is a pull / merge request summary.
type PR struct {
	// Number is the provider's sequential PR number.
	Number int `json:"number"`
	// Title is the PR title.
	Title string `json:"title"`
	// URL is the human-readable web URL of the PR.
	URL string `json:"url"`
	// State is the PR lifecycle state: "open", "closed", or "merged".
	State string `json:"state"`
	// Labels is the set of label names attached to this PR.
	Labels []string `json:"labels,omitempty"`
	// HeadBranch is the source branch name.
	HeadBranch string `json:"head_branch,omitempty"`
	// HeadSHA is the head commit SHA of the PR's source branch.
	HeadSHA string `json:"head_sha,omitempty"`
	// Author is the login or username of the PR author.
	Author string `json:"author,omitempty"`
	// Draft indicates that the PR is in draft / work-in-progress state.
	Draft bool `json:"draft,omitempty"`
}

// CheckRun is the status of one CI check run on a PR.
type CheckRun struct {
	// Name is the check-run or pipeline-job name.
	Name string `json:"name"`
	// Status is "queued", "in_progress", or "completed".
	Status string `json:"status"`
	// Conclusion is "success", "failure", "cancelled", "skipped", or "" when
	// the run has not yet completed.
	Conclusion string `json:"conclusion"`
}

// ListPROptions filters a [PRService.List] request.
type ListPROptions struct {
	// State filters by PR lifecycle state. "" and "open" are equivalent.
	// Use "closed" or "all" for other states.
	State string
	// Limit caps the number of results; 0 means the provider's default.
	Limit int
	// Labels filters to PRs that carry ALL of the listed label names.
	// Empty means no label filter.
	Labels []string
}

// MergeOptions configures how a PR is landed by [PRService.Merge].
type MergeOptions struct {
	// Method is "merge", "squash", or "rebase". Empty means the forge's
	// default merge method.
	Method string
	// CommitMessage is an optional override for the merge-commit message.
	CommitMessage string
}

// Installation is one app installation or equivalent token-scoped access
// grant (GitHub App installation, GitLab project access token record, …).
type Installation struct {
	// ID is the provider's installation identifier.
	ID int64 `json:"id"`
	// AccountLogin is the org or user login that owns the installation.
	AccountLogin string `json:"account_login"`
}

// BotConfig holds the credentials for one bot identity.
//
// On GitHub this is an App (AppID + PEM private key + webhook secret).
// On GitLab this is a project or group access token (PrivateKeyPEM holds
// the token value; AppID and WebhookSecret are unused).
type BotConfig struct {
	// AppID is the GitHub App numerical ID; 0 for token-based forges.
	AppID int64 `json:"app_id,omitempty"`
	// Slug is the GitHub App slug; empty for token-based forges.
	Slug string `json:"slug,omitempty"`
	// PrivateKeyPEM is the PEM-encoded RSA private key for a GitHub App, or
	// the raw access-token value for token-based forges.
	// Always stored encrypted at rest via the vault layer.
	PrivateKeyPEM string `json:"private_key_pem,omitempty"`
	// WebhookSecret is the App webhook secret; GitHub only.
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

// ---------- sub-service interfaces -------------------------------------------

// RepoService manages repository-level settings.
type RepoService interface {
	// Get fetches current settings for the repository identified by
	// "owner/repo".
	Get(ctx context.Context, owner, repo string) (*RepoSettings, error)

	// Update applies settings to the repository. Only the fields present in
	// settings are changed; callers should Get first and modify selectively.
	Update(ctx context.Context, owner, repo string, settings *RepoSettings) error

	// GetRaw fetches the full provider-native repository JSON.  The returned
	// bytes are the raw API response; callers unmarshal them as needed.
	// Providers that do not support raw access return (nil, [ErrUnsupported]).
	GetRaw(ctx context.Context, owner, repo string) (json.RawMessage, error)

	// PatchRaw applies a raw JSON PATCH to the primary repository metadata
	// endpoint.  This is the low-level path used by the posture package for
	// behavior-identical settings extraction; future callers should prefer
	// [Update].  Providers that do not support raw patching return
	// [ErrUnsupported].
	PatchRaw(ctx context.Context, owner, repo string, payload json.RawMessage) error

	// VulnAlerts reports whether dependency-vulnerability scanning (Dependabot
	// on GitHub, or the forge-equivalent) is currently enabled.  Returns
	// (false, [ErrUnsupported]) on providers that lack this feature.
	VulnAlerts(ctx context.Context, owner, repo string) (bool, error)

	// SetVulnAlerts enables or disables dependency-vulnerability scanning.
	// Returns [ErrUnsupported] on providers that lack this feature.
	SetVulnAlerts(ctx context.Context, owner, repo string, enabled bool) error

	// ActionsWorkflow returns the raw JSON of CI workflow permissions (GitHub
	// Actions workflow-permission settings or the forge equivalent).  Returns
	// (nil, [ErrUnsupported]) on providers that lack this feature.
	ActionsWorkflow(ctx context.Context, owner, repo string) (json.RawMessage, error)

	// SetActionsWorkflow applies raw JSON CI workflow permissions.  Returns
	// [ErrUnsupported] on providers that lack this feature.
	SetActionsWorkflow(ctx context.Context, owner, repo string, payload json.RawMessage) error
}

// ProtectionService manages branch-protection policies (rulesets, protected
// branches, push rules, approval rules, …).
//
// The target parameter is "owner/repo" for repository-level operations and
// the bare organisation/group name for org-level operations; providers
// distinguish by context (e.g. the presence of a "/" separator).
type ProtectionService interface {
	// List returns all rulesets/policies for the target.
	List(ctx context.Context, target string) ([]Ruleset, error)

	// Get returns one ruleset by its provider-opaque ID.
	Get(ctx context.Context, target, id string) (*Ruleset, error)

	// Create creates a new ruleset and returns it with its assigned ID.
	Create(ctx context.Context, target string, rs *Ruleset) (*Ruleset, error)

	// Update replaces an existing ruleset. rs.ID must be set.
	Update(ctx context.Context, target string, rs *Ruleset) error

	// Delete removes a ruleset permanently.
	Delete(ctx context.Context, target, id string) error
}

// PRService manages pull requests (GitHub) or merge requests (GitLab).
type PRService interface {
	// List returns PRs for the repository. opts.State defaults to "open".
	List(ctx context.Context, owner, repo string, opts ListPROptions) ([]PR, error)

	// Get returns one PR by its sequential number.
	Get(ctx context.Context, owner, repo string, number int) (*PR, error)

	// Create opens a new PR from branch against base. Returns the created PR.
	Create(ctx context.Context, owner, repo, branch, base, title, body string) (*PR, error)

	// Close closes (does not merge) the PR.
	Close(ctx context.Context, owner, repo string, number int) error

	// Reopen re-opens a previously closed PR.
	Reopen(ctx context.Context, owner, repo string, number int) error

	// ListChecks returns the CI check status for the PR's head commit.
	ListChecks(ctx context.Context, owner, repo string, number int) ([]CheckRun, error)

	// Merge lands the PR. The PR must already satisfy all required checks;
	// the provider returns an error if the PR is not mergeable.
	// opts.Method and opts.CommitMessage form the explicit koryph-ufy seam:
	// callers that need to control the merge strategy (squash, rebase, merge)
	// and commit message MUST populate these fields so the service can pass them
	// through without re-interpreting them.
	Merge(ctx context.Context, owner, repo string, number int, opts MergeOptions) error

	// Approve registers an approving review on the PR. body is optional
	// review comment text. The caller is responsible for ensuring the
	// approving identity is not the PR author (forges reject self-approval).
	Approve(ctx context.Context, owner, repo string, number int, body string) error

	// AddLabels attaches one or more labels to the PR, creating them on the
	// repository if they do not already exist. Existing labels are not removed.
	AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error

	// RemoveLabels detaches the named labels from the PR. Labels that are
	// not currently attached are silently ignored.
	RemoveLabels(ctx context.Context, owner, repo string, number int, labels []string) error
}

// SecretsService manages repository-level and org-level CI secrets or
// equivalent CI/CD variables.
type SecretsService interface {
	// ListRepo returns the names (never the values) of secrets set on
	// "owner/repo".
	ListRepo(ctx context.Context, owner, repo string) ([]string, error)

	// ListOrg returns the names of org-level secrets for the given
	// organisation.
	ListOrg(ctx context.Context, org string) ([]string, error)

	// SetRepo creates or updates a repository secret.
	SetRepo(ctx context.Context, owner, repo, name, value string) error

	// SetOrg creates or updates an org-level secret, restricting visibility
	// to the named repositories. nil or empty repos means "all selected
	// repositories" per the provider's default.
	SetOrg(ctx context.Context, org, name, value string, repos []string) error
}

// ReleaseService manages published releases and their binary assets.
//
// On forges with [Capabilities.DraftReleases] == true the normal flow is:
//
//	id, _ := Releases().CreateDraft(ctx, ...)
//	Releases().UploadAsset(ctx, ..., id, ...)  // repeat per artifact
//	Releases().Publish(ctx, ..., id)
//
// On forges where DraftReleases == false callers use the assemble-then-create
// strategy: stage all assets via an intermediate store (e.g. the forge's
// generic package registry), then call [Create] once with asset links in the
// release notes.
type ReleaseService interface {
	// Create publishes a release for the given tag with the provided release
	// notes. Returns the provider-opaque release ID.
	Create(ctx context.Context, owner, repo, tag, body string) (id string, err error)

	// CreateDraft creates a draft (not yet visible) release. The returned ID
	// is passed to UploadAsset and Publish.
	//
	// Only callable when Capabilities().DraftReleases == true; returns
	// [ErrUnsupported] otherwise.
	CreateDraft(ctx context.Context, owner, repo, tag, body string) (id string, err error)

	// UploadAsset attaches a named asset to an existing release (draft or
	// published). The release is identified by the provider-opaque releaseID
	// returned from Create or CreateDraft.
	UploadAsset(ctx context.Context, owner, repo, releaseID, filename string, r io.Reader) error

	// Publish makes a draft release publicly visible.
	//
	// Only callable when Capabilities().DraftReleases == true; returns
	// [ErrUnsupported] otherwise.
	Publish(ctx context.Context, owner, repo, releaseID string) error
}

// CIService renders forge-specific CI/CD pipeline asset files.
//
// Callers request a kind and receive ready-to-write file content for the
// appropriate path (e.g. .github/workflows/docs.yml for GitHub, a
// .gitlab-ci.yml include block for GitLab).
type CIService interface {
	// Render returns the content of a forge-native pipeline asset.
	//
	// Known kinds:
	//   - "docs":    documentation-publish pipeline / workflow
	//   - "release": release-train pipeline / caller workflow
	//   - "caller":  reusable release-train caller snippet
	//   - "scanner": security-scanner fragment
	//
	// Returns an error for unknown kinds.
	Render(kind string) ([]byte, error)
}

// BotService manages the bot-identity lifecycle.
//
// On GitHub this is a GitHub App: manifest exchange, JWT signing,
// installation token minting.
//
// On GitLab there is no App concept; BotService wraps a project or group
// access token: guided creation (koryph opens the settings URL and validates
// the pasted token), scope/expiry checking, and CI variable management.
type BotService interface {
	// ExchangeManifest exchanges an app-manifest code for bot credentials.
	// GitHub: POST /app-manifests/{code}/conversions.
	// GitLab: returns [ErrUnsupported]; use the access-token flow instead.
	ExchangeManifest(ctx context.Context, code string) (BotConfig, error)

	// ListInstallations returns all installations for a GitHub App (requires
	// a signed JWT). On GitLab it returns a single synthesised installation
	// record derived from the stored access token.
	ListInstallations(ctx context.Context, jwtOrToken string) ([]Installation, error)

	// MintInstallationToken creates a short-lived installation access token
	// for the given installation ID (GitHub: POST /installations/{id}/access_tokens).
	// On GitLab it refreshes and returns the stored PAT.
	MintInstallationToken(ctx context.Context, jwtOrToken string, installID int64) (string, error)

	// SetSecrets stores the bot credentials as CI secrets or variables on the
	// target repository (and, when applicable, the owning organisation).
	SetSecrets(ctx context.Context, cfg BotConfig, ownerRepo string) error
}
