// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package gitlab is the GitLab forge provider. It registers itself in
// [forge.Default] at import time (import for side-effect) and implements the
// [forge.Forge] interface for GitLab.com and self-hosted GitLab instances.
//
// Services backed by real GitLab logic:
//   - [forge.BotService] — guided project/group access-token creation, scope
//     and expiry validation, CI variable management
//
// Services stubbed (returning [forge.ErrUnsupported]) — to be extracted in
// later beads:
//   - [forge.RepoService]       — future bead
//   - [forge.ProtectionService] — future bead
//   - [forge.PRService]         — future bead
//   - [forge.SecretsService]    — future bead
//   - [forge.ReleaseService]    — future bead
//   - [forge.CIService]         — future bead
//
// # Authentication
//
// The GitLab BotService uses a personal or project access token resolved
// through the vault layer (same providers as signing / GitHub bot keys).
// The token is read from [forge.BotConfig].PrivateKeyPEM at call time; the
// field name is reused to keep the credential schema identical to GitHub bots
// (pointer-mode: Provider + KeyRef; inline: token value stored directly).
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
)

func init() {
	// Register the zero-config GitLab provider.
	forge.Default.Register("gitlab", &Provider{})
}

// Provider is the GitLab forge implementation.
// The zero value is valid and satisfies all interface methods.
type Provider struct{}

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

// Repo returns a stub; to be implemented in a future bead.
func (p *Provider) Repo() forge.RepoService { return &stubRepoSvc{} }

// Protection returns a stub; to be implemented in a future bead.
func (p *Provider) Protection() forge.ProtectionService { return &stubProtectionSvc{} }

// PRs returns a stub; to be implemented in a future bead.
func (p *Provider) PRs() forge.PRService { return &stubPRSvc{} }

// Secrets returns a stub; to be implemented in a future bead.
func (p *Provider) Secrets() forge.SecretsService { return &stubSecretsSvc{} }

// Releases returns a stub; to be implemented in a future bead.
func (p *Provider) Releases() forge.ReleaseService { return &stubReleaseSvc{} }

// CI returns a stub; to be implemented in a future bead.
func (p *Provider) CI() forge.CIService { return &stubCISvc{} }

// Bot returns a [forge.BotService] backed by the GitLab access-token flow.
func (p *Provider) Bot() forge.BotService { return &gitlabBotSvc{} }

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

type stubPRSvc struct{}

func (s *stubPRSvc) List(_ context.Context, _, _ string, _ forge.ListPROptions) ([]forge.PR, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubPRSvc) Get(_ context.Context, _, _ string, _ int) (*forge.PR, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubPRSvc) Create(_ context.Context, _, _, _, _, _, _ string) (*forge.PR, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubPRSvc) Close(_ context.Context, _, _ string, _ int) error {
	return forge.ErrUnsupported
}
func (s *stubPRSvc) Reopen(_ context.Context, _, _ string, _ int) error {
	return forge.ErrUnsupported
}
func (s *stubPRSvc) ListChecks(_ context.Context, _, _ string, _ int) ([]forge.CheckRun, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubPRSvc) Merge(_ context.Context, _, _ string, _ int, _ forge.MergeOptions) error {
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

type stubCISvc struct{}

func (s *stubCISvc) Render(_ string) ([]byte, error) { return nil, forge.ErrUnsupported }

// ---------- compile-time interface guards ------------------------------------

var (
	_ forge.Forge             = (*Provider)(nil)
	_ forge.RepoService       = (*stubRepoSvc)(nil)
	_ forge.ProtectionService = (*stubProtectionSvc)(nil)
	_ forge.PRService         = (*stubPRSvc)(nil)
	_ forge.SecretsService    = (*stubSecretsSvc)(nil)
	_ forge.ReleaseService    = (*stubReleaseSvc)(nil)
	_ forge.CIService         = (*stubCISvc)(nil)
)
