// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/forge"
	githubforge "github.com/koryph/koryph/internal/forge/github"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/release"
)

// ---------- registration ------------------------------------------------------

func TestProviderRegistered(t *testing.T) {
	// The package init() registers the "github" provider in forge.Default.
	f, ok := forge.Default.Get("github")
	if !ok {
		t.Fatal("forge.Default.Get(\"github\"): not found after import")
	}
	if f.Name() != "github" {
		t.Fatalf("Name() = %q, want \"github\"", f.Name())
	}
}

// ---------- capabilities ------------------------------------------------------

func TestGitHubCapabilities(t *testing.T) {
	p := githubforge.New()
	caps := p.Capabilities()

	check := func(name string, got bool) {
		t.Helper()
		if !got {
			t.Errorf("Capabilities().%s = false, want true", name)
		}
	}
	check("DraftReleases", caps.DraftReleases)
	check("Rulesets", caps.Rulesets)
	check("AppIdentity", caps.AppIdentity)
	check("WorkflowDispatch", caps.WorkflowDispatch)
	check("PagesHosting", caps.PagesHosting)
	check("ImmutableReleases", caps.ImmutableReleases)
	check("OrgRulesets", caps.OrgRulesets)
	check("SecretScanning", caps.SecretScanning)
	check("VulnerabilityAlerts", caps.VulnerabilityAlerts)
}

// ---------- stub services return ErrUnsupported --------------------------------

func TestStubServicesReturnErrUnsupported(t *testing.T) {
	ctx := t.Context()
	p := githubforge.New()

	// Repo — still a stub (koryph-fv3.2)
	if _, err := p.Repo().Get(ctx, "owner", "repo"); !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("Repo().Get: want ErrUnsupported, got %v", err)
	}

	// Protection — still a stub (koryph-fv3.2)
	if _, err := p.Protection().List(ctx, "owner/repo"); !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("Protection().List: want ErrUnsupported, got %v", err)
	}

	// Secrets — still a stub (koryph-fv3.2)
	if _, err := p.Secrets().ListRepo(ctx, "owner", "repo"); !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("Secrets().ListRepo: want ErrUnsupported, got %v", err)
	}
}

// ---------- PRService is real (not a stub) ------------------------------------

// TestPRServiceNotNil confirms PRs() returns a real implementation (not the
// stub that returned ErrUnsupported) after koryph-fv3.3 extraction.
func TestPRServiceNotNil(t *testing.T) {
	p := githubforge.New()
	if svc := p.PRs(); svc == nil {
		t.Fatal("PRs() returned nil")
	}
}

// ---------- CIService ---------------------------------------------------------

// goreleaserRC is a ReleaseConfig fixture for goreleaser mode.
func goreleaserRC() *project.ReleaseConfig {
	return &project.ReleaseConfig{
		Type:         "go",
		ExtraFiles:   []string{"internal/version/version.go"},
		ArtifactsDir: "dist",
		Build: project.ReleaseBuildConfig{
			Goreleaser: &project.GoreleaserBuild{Version: "~> v2.16"},
		},
		SBOM:       true,
		Provenance: true,
	}
}

// commandsRC is a ReleaseConfig fixture for commands mode.
func commandsRC() *project.ReleaseConfig {
	return &project.ReleaseConfig{
		Type:  "simple",
		Build: project.ReleaseBuildConfig{Commands: []string{"make build", "make package"}},
	}
}

// TestCIRender_CallerWorkflow_Goreleaser is a fixture-locked test for the
// caller-workflow rendered in goreleaser mode. The exact content must remain
// identical across refactors of the forge seam — any divergence means the
// extraction changed behaviour.
func TestCIRender_CallerWorkflow_Goreleaser(t *testing.T) {
	p := githubforge.New(githubforge.WithReleaseConfig(goreleaserRC()))
	got, err := p.CI().Render("caller")
	if err != nil {
		t.Fatalf("Render(\"caller\"): %v", err)
	}

	// Structural assertions that fixture-lock the rendered content.
	wantFragments := []string{
		"koryph/koryph/.github/workflows/release-train.yml@main",
		`release_type: "go"`,
		`artifacts_dir: "dist"`,
		`build_mode: "goreleaser"`,
		`goreleaser_version: "~> v2.16"`,
		"internal/version/version.go",
		"sbom: true",
		"provenance: true",
	}
	s := string(got)
	for _, frag := range wantFragments {
		if !strings.Contains(s, frag) {
			t.Errorf("Render(\"caller\") missing fragment %q\ngot:\n%s", frag, s)
		}
	}
	if !strings.Contains(s, "goreleaser_version") {
		t.Error("goreleaser mode: rendered YAML missing goreleaser_version")
	}
}

// TestCIRender_CallerWorkflow_Commands exercises commands-mode rendering.
func TestCIRender_CallerWorkflow_Commands(t *testing.T) {
	p := githubforge.New(githubforge.WithReleaseConfig(commandsRC()))
	got, err := p.CI().Render("caller")
	if err != nil {
		t.Fatalf("Render(\"caller\") commands mode: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `build_mode: "commands"`) {
		t.Error("commands mode: missing build_mode: commands")
	}
	// goreleaser_version must NOT appear in commands mode.
	if strings.Contains(s, "goreleaser_version") {
		t.Error("commands mode: goreleaser_version must be absent")
	}
}

// TestCIRender_NoReleaseConfig verifies that Render returns an informative
// error when the provider was constructed without a ReleaseConfig.
func TestCIRender_NoReleaseConfig(t *testing.T) {
	p := githubforge.New() // no release config
	_, err := p.CI().Render("caller")
	if err == nil {
		t.Fatal("Render(\"caller\") without ReleaseConfig: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "release config required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestCIRender_UnknownKind verifies that an unknown kind wraps ErrUnsupported.
func TestCIRender_UnknownKind(t *testing.T) {
	p := githubforge.New(githubforge.WithReleaseConfig(goreleaserRC()))
	_, err := p.CI().Render("unknown-kind")
	if err == nil {
		t.Fatal("Render(\"unknown-kind\"): expected error, got nil")
	}
	if !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("Render(\"unknown-kind\"): want errors.Is ErrUnsupported, got %v", err)
	}
}

// TestCIRender_Identity verifies that calling CI().Render("caller") via the
// GitHub forge produces byte-identical output to calling
// release.RenderCallerWorkflow directly — the "behaviour-identical extraction"
// acceptance criterion.
func TestCIRender_Identity(t *testing.T) {
	for _, rc := range []*project.ReleaseConfig{goreleaserRC(), commandsRC()} {
		p := githubforge.New(githubforge.WithReleaseConfig(rc))
		forgeOut, err := p.CI().Render("caller")
		if err != nil {
			t.Fatalf("forge CI.Render: %v", err)
		}

		directOut, err := release.RenderCallerWorkflow(rc)
		if err != nil {
			t.Fatalf("direct RenderCallerWorkflow: %v", err)
		}

		if string(forgeOut) != string(directOut) {
			t.Errorf("CI().Render(\"caller\") differs from release.RenderCallerWorkflow:\nforge:\n%s\ndirect:\n%s",
				forgeOut, directOut)
		}
	}
}

// ---------- ReleaseService interface satisfaction ----------------------------

func TestReleaseServiceNotNil(t *testing.T) {
	p := githubforge.New()
	if svc := p.Releases(); svc == nil {
		t.Fatal("Releases() returned nil")
	}
}

// ---------- BotService interface satisfaction --------------------------------

func TestBotServiceNotNil(t *testing.T) {
	p := githubforge.New()
	if svc := p.Bot(); svc == nil {
		t.Fatal("Bot() returned nil")
	}
}

// ---------- Provider satisfies Forge interface --------------------------------

// TestProviderSatisfiesForge is a compile-time guard surfaced at test time.
func TestProviderSatisfiesForge(t *testing.T) {
	var _ forge.Forge = githubforge.New()
}
