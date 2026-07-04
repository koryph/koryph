// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package github is the GitHub forge provider. It registers itself in
// [forge.Default] at import time (import for side-effect) and implements the
// full [forge.Forge] interface for GitHub.com.
//
// Services backed by real GitHub logic:
//   - [forge.PRService]      — koryph-fv3.3: list/create/approve/labels/merge/checks/close-reopen via gh CLI
//   - [forge.ReleaseService] — draft-until-complete via the gh CLI
//   - [forge.CIService]      — caller-workflow rendering (internal/release templates)
//   - [forge.BotService]     — GitHub App manifest exchange, installation tokens,
//     and secret wiring
//
// Services stubbed (returning [forge.ErrUnsupported]) — implemented in
// later extraction beads (F2a/F2c):
//   - [forge.RepoService]       — koryph-fv3.2 (hygiene)
//   - [forge.ProtectionService] — koryph-fv3.2 (rulesets)
//   - [forge.SecretsService]    — koryph-fv3.2 (org/repo secrets)
//
// # Project-specific configuration
//
// The global registered instance (obtained via forge.Default.Get("github"))
// has no project context. For operations that require a project's release
// configuration (e.g. CI().Render("caller")), create a per-invocation
// provider using [New]:
//
//	ghf := github.New(github.WithReleaseConfig(rc))
//	content, err := ghf.CI().Render("caller")
package github

import (
	"context"

	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/project"
)

func init() {
	// Register the zero-config GitHub provider. Project-scoped operations
	// (CI rendering) require a per-invocation instance from New().
	forge.Default.Register("github", &Provider{})
}

// Provider is the GitHub forge implementation. It is safe to use as a value
// (not a pointer) but exported as *Provider so callers can embed or extend.
//
// The zero value is valid and satisfies all interface methods; methods that
// need a project config (CI().Render("caller")) return an error when rc is nil.
type Provider struct {
	rc *project.ReleaseConfig // optional; required for CI().Render
}

// Option is a functional option for [New].
type Option func(*Provider)

// WithReleaseConfig attaches the project's release config so that
// [CIService.Render]("caller") can produce the caller workflow.
func WithReleaseConfig(rc *project.ReleaseConfig) Option {
	return func(p *Provider) { p.rc = rc }
}

// New constructs a GitHub [Provider] with the supplied options. Use this (not
// the global registry entry) for project-scoped operations.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name satisfies [forge.Forge].
func (p *Provider) Name() string { return "github" }

// Capabilities reports the full set of GitHub capabilities.
func (p *Provider) Capabilities() forge.Capabilities {
	return forge.Capabilities{
		DraftReleases:       true,
		Rulesets:            true,
		AppIdentity:         true,
		WorkflowDispatch:    true,
		PagesHosting:        true,
		ImmutableReleases:   true,
		OrgRulesets:         true,
		SecretScanning:      true,
		VulnerabilityAlerts: true,
	}
}

// Repo returns a stub; implemented in koryph-fv3.2.
func (p *Provider) Repo() forge.RepoService { return &stubRepoSvc{} }

// Protection returns a stub; implemented in koryph-fv3.2.
func (p *Provider) Protection() forge.ProtectionService { return &stubProtectionSvc{} }

// PRs returns a [forge.PRService] backed by the gh CLI using explicit
// --repo owner/repo so it can be called from any working directory.
func (p *Provider) PRs() forge.PRService { return &githubPRSvc{} }

// Secrets returns a stub; implemented in koryph-fv3.2.
func (p *Provider) Secrets() forge.SecretsService { return &stubSecretsSvc{} }

// Releases returns a [forge.ReleaseService] backed by the gh CLI using the
// draft-until-complete strategy (CreateDraft → UploadAsset(s) → Publish).
func (p *Provider) Releases() forge.ReleaseService { return &githubReleaseSvc{} }

// CI returns a [forge.CIService] that renders GitHub Actions pipeline assets
// using internal/release templates.
func (p *Provider) CI() forge.CIService { return &githubCISvc{rc: p.rc} }

// Bot returns a [forge.BotService] backed by the GitHub App API.
func (p *Provider) Bot() forge.BotService { return &githubBotSvc{} }

// ---------- stubs for not-yet-implemented services ----------------------------

type stubRepoSvc struct{}

func (s *stubRepoSvc) Get(_ context.Context, _, _ string) (*forge.RepoSettings, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubRepoSvc) Update(_ context.Context, _, _ string, _ *forge.RepoSettings) error {
	return forge.ErrUnsupported
}

type stubProtectionSvc struct{}

func (s *stubProtectionSvc) List(_ context.Context, _ string) ([]forge.Ruleset, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubProtectionSvc) Get(_ context.Context, _, _ string) (*forge.Ruleset, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubProtectionSvc) Create(_ context.Context, _ string, _ *forge.Ruleset) (*forge.Ruleset, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubProtectionSvc) Update(_ context.Context, _ string, _ *forge.Ruleset) error {
	return forge.ErrUnsupported
}
func (s *stubProtectionSvc) Delete(_ context.Context, _, _ string) error {
	return forge.ErrUnsupported
}

type stubSecretsSvc struct{}

func (s *stubSecretsSvc) ListRepo(_ context.Context, _, _ string) ([]string, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubSecretsSvc) ListOrg(_ context.Context, _ string) ([]string, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubSecretsSvc) SetRepo(_ context.Context, _, _, _, _ string) error {
	return forge.ErrUnsupported
}
func (s *stubSecretsSvc) SetOrg(_ context.Context, _, _, _ string, _ []string) error {
	return forge.ErrUnsupported
}

// ---------- compile-time interface guards ------------------------------------

var (
	_ forge.Forge             = (*Provider)(nil)
	_ forge.RepoService       = (*stubRepoSvc)(nil)
	_ forge.ProtectionService = (*stubProtectionSvc)(nil)
	_ forge.SecretsService    = (*stubSecretsSvc)(nil)
)
