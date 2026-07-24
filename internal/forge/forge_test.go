// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package forge_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/forge"
)

// ---------- registry tests ---------------------------------------------------

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := &forge.Registry{}
	r.Register("test-forge", &stubForge{name: "test-forge"})

	got, ok := r.Get("test-forge")
	if !ok {
		t.Fatal("Get: expected ok=true after Register")
	}
	if got.Name() != "test-forge" {
		t.Fatalf("Name() = %q, want %q", got.Name(), "test-forge")
	}
}

func TestRegistry_GetMissing(t *testing.T) {
	r := &forge.Registry{}
	if _, ok := r.Get("no-such"); ok {
		t.Fatal("Get: expected ok=false for unregistered name")
	}
}

func TestRegistry_MustGetMissing(t *testing.T) {
	r := &forge.Registry{}
	defer func() {
		if recover() == nil {
			t.Fatal("MustGet: expected panic for unregistered name")
		}
	}()
	r.MustGet("no-such")
}

func TestRegistry_RegisterDuplicate(t *testing.T) {
	r := &forge.Registry{}
	r.Register("dup", &stubForge{name: "dup"})
	defer func() {
		if recover() == nil {
			t.Fatal("Register: expected panic for duplicate name")
		}
	}()
	r.Register("dup", &stubForge{name: "dup"})
}

func TestRegistry_RegisterEmptyName(t *testing.T) {
	r := &forge.Registry{}
	defer func() {
		if recover() == nil {
			t.Fatal("Register: expected panic for empty name")
		}
	}()
	r.Register("", &stubForge{})
}

func TestRegistry_Names(t *testing.T) {
	r := &forge.Registry{}
	r.Register("z-forge", &stubForge{name: "z-forge"})
	r.Register("a-forge", &stubForge{name: "a-forge"})
	names := r.Names()
	if len(names) != 2 {
		t.Fatalf("Names() len = %d, want 2", len(names))
	}
	if names[0] != "a-forge" || names[1] != "z-forge" {
		t.Fatalf("Names() = %v, want sorted [a-forge z-forge]", names)
	}
}

// ---------- sniff tests ------------------------------------------------------

func TestSniffRemote(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"git@github.com:acme/widgets.git", "github"},
		{"https://github.com/acme/widgets", "github"},
		{"https://gitlab.com/acme/app.git", "gitlab"},
		{"git@gitlab.com:acme/app.git", "gitlab"},
		{"https://git.acme.corp/self-managed/repo.git", ""},
		{"https://bitbucket.org/acme/repo.git", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := forge.SniffRemote(tc.url)
		if got != tc.want {
			t.Errorf("SniffRemote(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// self-managed GitLab — contains "gitlab." but not "gitlab.com"
func TestSniffRemote_SelfManagedGitLab(t *testing.T) {
	url := "https://gitlab.acme.corp/infra/deploy.git"
	if got := forge.SniffRemote(url); got != "gitlab" {
		t.Errorf("SniffRemote(%q) = %q, want gitlab", url, got)
	}
}

// ---------- capabilities zero value test -------------------------------------

func TestCapabilities_ZeroValueIsValid(t *testing.T) {
	// A zero Capabilities must be usable without any nil dereference or panic.
	var caps forge.Capabilities
	if caps.DraftReleases || caps.Rulesets || caps.AppIdentity ||
		caps.WorkflowDispatch || caps.PagesHosting || caps.ImmutableReleases ||
		caps.OrgRulesets || caps.SecretScanning || caps.VulnerabilityAlerts {
		t.Fatal("zero Capabilities should have all features false")
	}
}

// ---------- ErrUnsupported ---------------------------------------------------

func TestErrUnsupported_IsErrors(t *testing.T) {
	// Callers use errors.Is(err, forge.ErrUnsupported) in switches.
	if !errors.Is(forge.ErrUnsupported, forge.ErrUnsupported) {
		t.Fatal("errors.Is(ErrUnsupported, ErrUnsupported) should be true")
	}
}

// ---------- stub implementation (satisfies the full Forge interface) ----------

// stubForge is a minimal Forge implementation used by registry tests.
// It does not implement any meaningful logic.
type stubForge struct {
	name string
}

func (s *stubForge) Name() string                        { return s.name }
func (s *stubForge) Capabilities() forge.Capabilities    { return forge.Capabilities{} }
func (s *stubForge) Repo() forge.RepoService             { return &stubRepoSvc{} }
func (s *stubForge) Protection() forge.ProtectionService { return &stubProtectionSvc{} }
func (s *stubForge) PRs() forge.PRService                { return &stubPRSvc{} }
func (s *stubForge) Secrets() forge.SecretsService       { return &stubSecretsSvc{} }
func (s *stubForge) Pages() forge.PagesService           { return &stubPagesSvc{} }
func (s *stubForge) Releases() forge.ReleaseService      { return &stubReleaseSvc{} }
func (s *stubForge) CI() forge.CIService                 { return &stubCISvc{} }
func (s *stubForge) Bot() forge.BotService               { return &stubBotSvc{} }

type stubPagesSvc struct{}

func (s *stubPagesSvc) Get(context.Context, string, string) (*forge.PagesSite, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubPagesSvc) SetCustomDomain(context.Context, string, string, string) error {
	return forge.ErrUnsupported
}
func (s *stubPagesSvc) CheckHealth(context.Context, string, string) (*forge.PagesHealth, bool, error) {
	return nil, false, forge.ErrUnsupported
}
func (s *stubPagesSvc) WaitForHealth(context.Context, string, string, time.Duration) (*forge.PagesHealth, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubPagesSvc) SetHTTPSEnforced(context.Context, string, string, bool) error {
	return forge.ErrUnsupported
}

type stubRepoSvc struct{}

func (s *stubRepoSvc) Get(_ context.Context, _, _ string) (*forge.RepoSettings, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubRepoSvc) Update(_ context.Context, _, _ string, _ *forge.RepoSettings) error {
	return forge.ErrUnsupported
}
func (s *stubRepoSvc) GetRaw(_ context.Context, _, _ string) (json.RawMessage, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubRepoSvc) PatchRaw(_ context.Context, _, _ string, _ json.RawMessage) error {
	return forge.ErrUnsupported
}
func (s *stubRepoSvc) VulnAlerts(_ context.Context, _, _ string) (bool, error) {
	return false, forge.ErrUnsupported
}
func (s *stubRepoSvc) SetVulnAlerts(_ context.Context, _, _ string, _ bool) error {
	return forge.ErrUnsupported
}
func (s *stubRepoSvc) ActionsWorkflow(_ context.Context, _, _ string) (json.RawMessage, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubRepoSvc) SetActionsWorkflow(_ context.Context, _, _ string, _ json.RawMessage) error {
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
func (s *stubPRSvc) Approve(_ context.Context, _, _ string, _ int, _ string) error {
	return forge.ErrUnsupported
}
func (s *stubPRSvc) AddLabels(_ context.Context, _, _ string, _ int, _ []string) error {
	return forge.ErrUnsupported
}
func (s *stubPRSvc) RemoveLabels(_ context.Context, _, _ string, _ int, _ []string) error {
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

type stubBotSvc struct{}

func (s *stubBotSvc) ExchangeManifest(_ context.Context, _ string) (forge.BotConfig, error) {
	return forge.BotConfig{}, forge.ErrUnsupported
}
func (s *stubBotSvc) ListInstallations(_ context.Context, _ string) ([]forge.Installation, error) {
	return nil, forge.ErrUnsupported
}
func (s *stubBotSvc) MintInstallationToken(_ context.Context, _ string, _ int64) (string, error) {
	return "", forge.ErrUnsupported
}
func (s *stubBotSvc) SetSecrets(_ context.Context, _ forge.BotConfig, _ string) error {
	return forge.ErrUnsupported
}
