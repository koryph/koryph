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
// Services implemented in this bead (F2a — hygiene behind the forge seam):
//   - [forge.RepoService]       — gh-backed settings (repo flags, security, vuln alerts, actions)
//   - [forge.ProtectionService] — gh-backed rulesets (repo-level and org-level)
//   - [forge.SecretsService]    — gh-backed secrets (repo and org)
//   - [forge.PagesService]      — custom domain, DNS health, and HTTPS settings
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
	rc        *project.ReleaseConfig   // optional; required for CI().Render("caller")
	gateCmd   string                   // optional; gate command for CI().Render("gate"), default "make gate"
	copyright *project.CopyrightConfig // optional; SPDX header for generated assets (nil ⇒ built-in default)
}

// Option is a functional option for [New].
type Option func(*Provider)

// WithReleaseConfig attaches the project's release config so that
// [CIService.Render]("caller") can produce the caller workflow.
func WithReleaseConfig(rc *project.ReleaseConfig) Option {
	return func(p *Provider) { p.rc = rc }
}

// WithGateCommand overrides the gate command used by [CIService.Render]("gate").
// The default when this option is not supplied is "make gate".
func WithGateCommand(cmd string) Option {
	return func(p *Provider) { p.gateCmd = cmd }
}

// WithCopyright attaches the project's copyright/license config so generated CI
// assets carry the project's own SPDX header (koryph-s6g). Nil is fine — the
// built-in default holder/license is used.
func WithCopyright(c *project.CopyrightConfig) Option {
	return func(p *Provider) { p.copyright = c }
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

// Repo returns the [forge.RepoService] backed by the gh CLI — repository
// settings, security features, vulnerability alerts, and actions workflow
// permissions.
func (p *Provider) Repo() forge.RepoService { return &githubRepoSvc{} }

// Protection returns the [forge.ProtectionService] backed by the gh CLI —
// repository-level and org-level rulesets.
func (p *Provider) Protection() forge.ProtectionService { return &githubProtectionSvc{} }

// PRs returns a [forge.PRService] backed by the gh CLI using explicit
// --repo owner/repo so it can be called from any working directory.
func (p *Provider) PRs() forge.PRService { return &githubPRSvc{} }

// Secrets returns the [forge.SecretsService] backed by the gh CLI — repository
// and org-level CI secrets.
func (p *Provider) Secrets() forge.SecretsService { return &githubSecretsSvc{} }

// Pages returns the [forge.PagesService] backed by the gh CLI — custom-domain
// configuration, DNS health polling, and HTTPS enforcement.
func (p *Provider) Pages() forge.PagesService { return &githubPagesSvc{} }

// Releases returns a [forge.ReleaseService] backed by the gh CLI using the
// draft-until-complete strategy (CreateDraft → UploadAsset(s) → Publish).
func (p *Provider) Releases() forge.ReleaseService { return &githubReleaseSvc{} }

// CI returns a [forge.CIService] that renders GitHub Actions pipeline assets
// using internal/release templates.
func (p *Provider) CI() forge.CIService {
	return &githubCISvc{rc: p.rc, gateCmd: p.gateCmd, copyright: p.copyright}
}

// Bot returns a [forge.BotService] backed by the GitHub App API.
func (p *Provider) Bot() forge.BotService { return &githubBotSvc{} }

// ---------- compile-time interface guard ------------------------------------

var _ forge.Forge = (*Provider)(nil)
