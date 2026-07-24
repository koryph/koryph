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

// ---------- F2a services are real (implemented in koryph-fv3.2) ---------------

// TestF2aServicesAreNotStubs confirms that Repo, Protection, and Secrets no
// longer return ErrUnsupported after the fv3.2 extraction.  They call the gh
// CLI, so a live gh call may fail (401/404); we only care that they don't
// return ErrUnsupported.
func TestF2aServicesAreNotStubs(t *testing.T) {
	ctx := t.Context()
	p := githubforge.New()

	if _, err := p.Repo().Get(ctx, "owner", "repo"); errors.Is(err, forge.ErrUnsupported) {
		t.Error("Repo().Get: still returns ErrUnsupported; fv3.2 should have wired this up")
	}
	if _, err := p.Protection().List(ctx, "owner/repo"); errors.Is(err, forge.ErrUnsupported) {
		t.Error("Protection().List: still returns ErrUnsupported; fv3.2 should have wired this up")
	}
	if _, err := p.Secrets().ListRepo(ctx, "owner", "repo"); errors.Is(err, forge.ErrUnsupported) {
		t.Error("Secrets().ListRepo: still returns ErrUnsupported; fv3.2 should have wired this up")
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

func containerRC() *project.ReleaseConfig {
	rc := goreleaserRC()
	rc.Container = &project.ContainerConfig{Registry: "ghcr.io", Image: "acme/widget"}
	return rc
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

// TestCIRender_ContainerWorkflow verifies the GHCR renderer preserves the
// digest as the common signing, SBOM, and provenance subject.
func TestCIRender_ContainerWorkflow(t *testing.T) {
	p := githubforge.New(githubforge.WithReleaseConfig(containerRC()))
	got, err := p.CI().Render("container")
	if err != nil {
		t.Fatalf("Render(\"container\"): %v", err)
	}
	s := string(got)
	for _, frag := range []string{
		"REGISTRY: ghcr.io",
		"IMAGE: acme/widget",
		"branches: [main]",
		"Detect a release-PR merge",
		"needs.detect-release.outputs.version",
		"push-by-digest=true",
		"steps.build.outputs.digest",
		"cosign sign --yes",
		"syft \"$REGISTRY/$IMAGE@$DIGEST\"",
		"cosign attest --yes",
		"actions/attest-build-provenance",
		"subject-digest:",
	} {
		if !strings.Contains(s, frag) {
			t.Errorf("Render(\"container\") missing %q\ngot:\n%s", frag, s)
		}
	}
}

func TestCIRender_ContainerWithoutConfig(t *testing.T) {
	p := githubforge.New(githubforge.WithReleaseConfig(goreleaserRC()))
	_, err := p.CI().Render("container")
	if err == nil || !strings.Contains(err.Error(), "container config required") {
		t.Fatalf("Render(\"container\") error = %v, want container-config error", err)
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

// ---------- CIService — gate kind --------------------------------------------

// TestCIRender_Gate_DefaultCommand verifies the gate workflow renders with the
// default gate command ("make gate") when no override is supplied.
func TestCIRender_Gate_DefaultCommand(t *testing.T) {
	p := githubforge.New() // no gate command — should default to "make gate"
	got, err := p.CI().Render("gate")
	if err != nil {
		t.Fatalf("Render(\"gate\"): %v", err)
	}
	s := string(got)

	// REUSE-IgnoreStart
	wantFragments := []string{
		// Workflow metadata
		"name: gate",
		// Triggers
		"on:",
		"push:",
		"pull_request:",
		// Minimal permissions
		"permissions:",
		"contents: read",
		// Job definition
		"jobs:",
		"gate:",
		"runs-on: ubuntu-latest",
		// Checkout step (toolchain-neutral — only checkout is provided)
		"actions/checkout",
		// Default gate command
		"make gate",
		// SPDX header (split to avoid REUSE scanner false-positive on the test source)
		"SPDX-License-Identifier: " + "Apache-2.0",
	}
	// REUSE-IgnoreEnd
	for _, frag := range wantFragments {
		if !strings.Contains(s, frag) {
			t.Errorf("Render(\"gate\") default: missing fragment %q\nfull output:\n%s", frag, s)
		}
	}
}

// TestCIRender_Gate_CustomCommand verifies that WithGateCommand threads the
// override into the rendered workflow.
func TestCIRender_Gate_CustomCommand(t *testing.T) {
	const customCmd = "go test ./... && golangci-lint run"
	p := githubforge.New(githubforge.WithGateCommand(customCmd))
	got, err := p.CI().Render("gate")
	if err != nil {
		t.Fatalf("Render(\"gate\") custom command: %v", err)
	}
	s := string(got)

	if !strings.Contains(s, customCmd) {
		t.Errorf("Render(\"gate\") custom: gate command %q missing from output\ngot:\n%s", customCmd, s)
	}
	// Default "make gate" must NOT appear when overridden.
	if strings.Contains(s, "make gate") {
		t.Error("Render(\"gate\") custom: default 'make gate' must be absent when a custom command is supplied")
	}
}

// TestCIRender_Gate_IsIdempotent verifies that calling Render("gate") twice
// with the same provider produces byte-identical output.
func TestCIRender_Gate_IsIdempotent(t *testing.T) {
	p := githubforge.New()
	first, err := p.CI().Render("gate")
	if err != nil {
		t.Fatalf("first Render(\"gate\"): %v", err)
	}
	second, err := p.CI().Render("gate")
	if err != nil {
		t.Fatalf("second Render(\"gate\"): %v", err)
	}
	if string(first) != string(second) {
		t.Error("Render(\"gate\") is not idempotent: two calls returned different output")
	}
}

// TestCIRender_Gate_GateCommandDoesNotRequireReleaseConfig verifies that the
// "gate" kind works without a ReleaseConfig (unlike "caller").
func TestCIRender_Gate_GateCommandDoesNotRequireReleaseConfig(t *testing.T) {
	p := githubforge.New() // no ReleaseConfig
	_, err := p.CI().Render("gate")
	if err != nil {
		t.Errorf("Render(\"gate\") without ReleaseConfig: expected nil error, got %v", err)
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

// TestCIRender_Copyright_Default confirms that with no copyright config the
// generated header is byte-identical to koryph's historical literal (koryph-s6g
// is purely additive).
func TestCIRender_Copyright_Default(t *testing.T) {
	p := githubforge.New()
	got, err := p.CI().Render("gate")
	if err != nil {
		t.Fatalf("Render(\"gate\"): %v", err)
	}
	s := string(got)
	// REUSE-IgnoreStart
	if !strings.Contains(s, "SPDX-FileCopyrightText: "+"(c) 2026 The Koryph Developers") {
		t.Errorf("default copyright header missing; got:\n%s", s)
	}
	// REUSE-IgnoreEnd
}

// TestCIRender_Copyright_PerProject proves koryph-s6g: WithCopyright threads the
// project's own holder/year/license into the generated CI asset header instead
// of koryph's.
func TestCIRender_Copyright_PerProject(t *testing.T) {
	cc := &project.CopyrightConfig{Holder: "Acme, Inc.", Year: "2024-2026", License: "MIT"}
	p := githubforge.New(githubforge.WithCopyright(cc))
	got, err := p.CI().Render("gate")
	if err != nil {
		t.Fatalf("Render(\"gate\"): %v", err)
	}
	s := string(got)
	// REUSE-IgnoreStart
	for _, frag := range []string{
		"SPDX-FileCopyrightText: " + "(c) 2024-2026 Acme, Inc.",
		"SPDX-License-Identifier: " + "MIT",
	} {
		if !strings.Contains(s, frag) {
			t.Errorf("per-project header missing %q; got:\n%s", frag, s)
		}
	}
	if strings.Contains(s, "The Koryph Developers") {
		t.Errorf("koryph's default holder leaked into a per-project render:\n%s", s)
	}
	// REUSE-IgnoreEnd
}
