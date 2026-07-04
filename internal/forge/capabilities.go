// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package forge

// Capabilities describes the optional features a [Forge] provider supports.
// Callers use capability flags instead of provider-name switches to keep
// feature selection decoupled from provider identity.
//
// A zero-value Capabilities is valid: it represents a minimal provider that
// supports the base repo, PR, secrets, releases, CI, and bot interfaces but
// none of the enumerated optional features.
type Capabilities struct {
	// DraftReleases is true when the forge supports creating a draft (not yet
	// publicly visible) release before all assets are attached. When false,
	// callers must use the assemble-then-create strategy: stage all assets
	// first, then call [ReleaseService.Create] once with links.
	//
	// True: GitHub. False: GitLab (v1).
	DraftReleases bool

	// Rulesets is true when the forge has a structured, named branch-protection
	// ruleset API (as opposed to per-branch protection rules). Posture providers
	// use this to select the ruleset vs. protected-branch compilation path.
	//
	// True: GitHub. False: GitLab (protected branches + push rules instead).
	Rulesets bool

	// AppIdentity is true when the forge supports a first-class bot-app identity
	// with a manifest-exchange flow, JWT authentication, and per-installation
	// access tokens. When false, [BotService] uses an access-token flow.
	//
	// True: GitHub Apps. False: GitLab (project/group access tokens).
	AppIdentity bool

	// WorkflowDispatch is true when the forge's CI platform supports an
	// on-demand dispatch event for pipelines (GitHub Actions workflow_dispatch).
	// GitLab pipelines can be triggered via API but expose different semantics.
	//
	// True: GitHub. False: GitLab (v1).
	WorkflowDispatch bool

	// PagesHosting is true when the forge natively hosts static sites from
	// repository content (GitHub Pages, GitLab Pages).
	PagesHosting bool

	// ImmutableReleases is true when published releases cannot be overwritten
	// or deleted after the fact. Callers that need idempotent re-runs should
	// probe before creating to avoid duplicate-release errors.
	//
	// True: GitHub Releases. False: GitLab (releases can be updated/deleted).
	ImmutableReleases bool

	// OrgRulesets is true when the forge supports organisation-scoped rulesets
	// that cascade to all repositories in the org (GitHub org rulesets).
	// When false, [ProtectionService] operates only at the repository level.
	//
	// True: GitHub. False: GitLab (group-level protected-branch settings differ).
	OrgRulesets bool

	// SecretScanning is true when the forge has a built-in secret-scanning
	// service that can be enabled or disabled via the [RepoService] API.
	//
	// True: GitHub (via repo settings JSON). False: GitLab (v1; out of API scope).
	SecretScanning bool

	// VulnerabilityAlerts is true when the forge exposes a first-class
	// dependency-vulnerability alert feature that can be toggled through the
	// [RepoService] API (GitHub Dependabot vulnerability alerts).
	//
	// True: GitHub. False: GitLab (advisory database integration differs).
	VulnerabilityAlerts bool
}
