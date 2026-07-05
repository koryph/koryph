// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package gitlab is the GitLab forge provider. It registers itself in
// [forge.Default] at import time (import for side-effect) and implements the
// [forge.Forge] interface for GitLab.com and self-hosted GitLab instances.
//
// Services backed by real GitLab logic:
//   - [forge.RepoService]       — project settings (merge method, squash option, …)
//   - [forge.ProtectionService] — protected branches, push rules, approval rules
//   - [forge.PRService]         — MR list/get/create/close/reopen/checks/merge/approve/labels
//   - [forge.SecretsService]    — CI/CD variables (project and group scope)
//   - [forge.BotService]        — guided project/group access-token creation, scope
//     and expiry validation, CI variable management
//   - [forge.CIService]         — .gitlab-ci.yml rendering: "release" (release train
//     pipeline) and "docs" (GitLab Pages docs-publish pipeline)
//
// Services stubbed (returning [forge.ErrUnsupported]) — to be extracted in
// later beads:
//   - [forge.ReleaseService] — future bead
//
// # Authentication
//
// The GitLab BotService uses a personal or project access token resolved
// through the vault layer (same providers as signing / GitHub bot keys).
// The token is read from [forge.BotConfig].PrivateKeyPEM at call time; the
// field name is reused to keep the credential schema identical to GitHub bots
// (pointer-mode: Provider + KeyRef; inline: token value stored directly).
//
// # Project-specific configuration
//
// The global registered instance (obtained via forge.Default.Get("gitlab"))
// has no project context. For operations that require a project's release
// configuration (e.g. CI().Render("release")), create a per-invocation
// provider using [New]:
//
//	glf := gitlab.New(gitlab.WithReleaseConfig(rc))
//	content, err := glf.CI().Render("release")
//
// # KORYPH_GITLAB_HOST
//
// By default all API calls go to https://gitlab.com. Set KORYPH_GITLAB_HOST
// to a bare hostname (e.g. "gitlab.example.com") to target a self-hosted
// instance.
package gitlab

import (
	"context"
	"io"

	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/project"
)

func init() {
	// Register the zero-config GitLab provider. Project-scoped operations
	// (CI rendering) require a per-invocation instance from New().
	forge.Default.Register("gitlab", &Provider{})
}

// Provider is the GitLab forge implementation.
//
// The zero value is valid and satisfies all interface methods; methods that
// need a project config (CI().Render("release")) return an error when rc is nil.
type Provider struct {
	rc      *project.ReleaseConfig // optional; required for CI().Render("release")
	gateCmd string                 // optional; gate command for CI().Render("gate"), default "make gate"
}

// Option is a functional option for [New].
type Option func(*Provider)

// WithReleaseConfig attaches the project's release config so that
// [CIService.Render]("release") can produce the release pipeline.
func WithReleaseConfig(rc *project.ReleaseConfig) Option {
	return func(p *Provider) { p.rc = rc }
}

// WithGateCommand overrides the gate command used by [CIService.Render]("gate").
// The default when this option is not supplied is "make gate".
func WithGateCommand(cmd string) Option {
	return func(p *Provider) { p.gateCmd = cmd }
}

// New constructs a GitLab [Provider] with the supplied options. Use this (not
// the global registry entry) for project-scoped operations.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name satisfies [forge.Forge].
func (p *Provider) Name() string { return "gitlab" }

// Capabilities reports the GitLab capability set.
// Draft releases, org rulesets, immutable releases, workflow dispatch,
// secret scanning, and vulnerability alerts are not supported in v1.
func (p *Provider) Capabilities() forge.Capabilities {
	return forge.Capabilities{
		DraftReleases:       false,
		Rulesets:            false,
		AppIdentity:         false, // GitLab uses access tokens, not App identities
		WorkflowDispatch:    false,
		PagesHosting:        true,
		ImmutableReleases:   false,
		OrgRulesets:         false,
		SecretScanning:      false,
		VulnerabilityAlerts: false,
	}
}

// Repo returns the GitLab project-settings service backed by the GitLab REST
// API v4.  Set KORYPH_GITLAB_TOKEN to a personal or project access token with
// api scope.
func (p *Provider) Repo() forge.RepoService { return &gitlabRepoSvc{} }

// Protection returns the GitLab protection service backed by the GitLab REST
// API v4.  It aggregates protected branches, push rules, and approval rules
// behind the [forge.ProtectionService] interface.
func (p *Provider) Protection() forge.ProtectionService { return &gitlabProtectionSvc{} }

// PRs returns the GitLab MR service backed by the GitLab REST API v4.
// Set KORYPH_GITLAB_TOKEN to a personal or project access token with api scope.
func (p *Provider) PRs() forge.PRService { return &gitlabPRSvc{} }

// Secrets returns the GitLab CI/CD variables service backed by the GitLab
// REST API v4.  Set KORYPH_GITLAB_TOKEN to a personal or project access
// token with api scope.
func (p *Provider) Secrets() forge.SecretsService { return &gitlabSecretsSvc{} }

// Releases returns a stub; to be implemented in a future bead.
func (p *Provider) Releases() forge.ReleaseService { return &stubReleaseSvc{} }

// CI returns a [forge.CIService] that renders GitLab CI/CD pipeline assets.
// Render("release") requires a non-nil ReleaseConfig; build the provider with
// [WithReleaseConfig]. Render("docs") and Render("gate") do not require a
// ReleaseConfig.
func (p *Provider) CI() forge.CIService { return &gitlabCISvc{rc: p.rc, gateCmd: p.gateCmd} }

// Bot returns a [forge.BotService] backed by the GitLab access-token flow.
func (p *Provider) Bot() forge.BotService { return &gitlabBotSvc{} }

// ---------- stubs for not-yet-implemented services ----------------------------

type stubReleaseSvc struct{}

func (s *stubReleaseSvc) Create(_ context.Context, _, _, _, _ string) (string, error) {
	return "", forge.ErrUnsupported
}
func (s *stubReleaseSvc) CreateDraft(_ context.Context, _, _, _, _ string) (string, error) {
	return "", forge.ErrUnsupported
}
func (s *stubReleaseSvc) UploadAsset(_ context.Context, _, _, _, _ string, _ io.Reader) error {
	return forge.ErrUnsupported
}
func (s *stubReleaseSvc) Publish(_ context.Context, _, _, _ string) error {
	return forge.ErrUnsupported
}

// ---------- compile-time interface guards ------------------------------------

var (
	_ forge.Forge             = (*Provider)(nil)
	_ forge.RepoService       = (*gitlabRepoSvc)(nil)
	_ forge.ProtectionService = (*gitlabProtectionSvc)(nil)
	_ forge.PRService         = (*gitlabPRSvc)(nil)
	_ forge.SecretsService    = (*gitlabSecretsSvc)(nil)
	_ forge.ReleaseService    = (*stubReleaseSvc)(nil)
	_ forge.CIService         = (*gitlabCISvc)(nil)
)
