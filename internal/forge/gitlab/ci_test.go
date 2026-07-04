// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/forge"
	gitlabforge "github.com/koryph/koryph/internal/forge/gitlab"
	"github.com/koryph/koryph/internal/project"
)

// ---------- fixtures ----------------------------------------------------------

// goreleaserRC is a ReleaseConfig fixture in goreleaser mode.
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

// commandsRC is a ReleaseConfig fixture in commands mode.
func commandsRC() *project.ReleaseConfig {
	return &project.ReleaseConfig{
		Type:  "simple",
		Build: project.ReleaseBuildConfig{Commands: []string{"make build", "make package"}},
	}
}

// ---------- CIService — release kind ------------------------------------------

// TestCIRender_Release_Goreleaser verifies the release pipeline renders key
// structural markers in goreleaser mode.
func TestCIRender_Release_Goreleaser(t *testing.T) {
	p := gitlabforge.New(gitlabforge.WithReleaseConfig(goreleaserRC()))
	got, err := p.CI().Render("release")
	if err != nil {
		t.Fatalf("Render(\"release\"): %v", err)
	}
	s := string(got)

	// REUSE-IgnoreStart
	wantFragments := []string{
		// Pipeline kind selector
		"workflow:",
		"PIPELINE_KIND: release",
		"PIPELINE_KIND: upkeep",
		"PIPELINE_KIND: mr",
		// Version computation job
		"compute-version:",
		"BREAKING CHANGE",
		"NEXT_VERSION=",
		// Release MR job
		"release-mr:",
		"KORYPH_RELEASE_TOKEN",
		// Tag job
		"tag-release:",
		"chore(release):",
		// GoReleaser build
		"goreleaser release --skip=publish",
		// Signing
		"sign:",
		"cosign sign-blob",
		"SIGSTORE_ID_TOKEN",
		"id_tokens:",
		// Assemble-then-create
		"upload-packages:",
		"packages/generic",
		"release-create:",
		// SLSA gap
		"SLSA",
		// SPDX header (split to avoid REUSE scanner false-positive on the test source)
		"SPDX-License-Identifier: " + "Apache-2.0",
	}
	// REUSE-IgnoreEnd
	for _, frag := range wantFragments {
		if !strings.Contains(s, frag) {
			t.Errorf("Render(\"release\") goreleaser mode: missing fragment %q\nfull output:\n%s", frag, s)
		}
	}

	// GoReleaser image version must appear.
	if !strings.Contains(s, "2.16") {
		t.Errorf("Render(\"release\") goreleaser mode: expected goreleaser version 2.16 in output")
	}
}

// TestCIRender_Release_Commands verifies the release pipeline renders correctly
// in commands mode.
func TestCIRender_Release_Commands(t *testing.T) {
	p := gitlabforge.New(gitlabforge.WithReleaseConfig(commandsRC()))
	got, err := p.CI().Render("release")
	if err != nil {
		t.Fatalf("Render(\"release\") commands mode: %v", err)
	}
	s := string(got)

	// Commands must appear.
	if !strings.Contains(s, "make build") {
		t.Error("commands mode: 'make build' missing from output")
	}
	if !strings.Contains(s, "make package") {
		t.Error("commands mode: 'make package' missing from output")
	}
	// GoReleaser build step must NOT appear in commands mode.
	if strings.Contains(s, "goreleaser release --skip=publish") {
		t.Error("commands mode: goreleaser build step must be absent")
	}
}

// TestCIRender_Release_DefaultArtifactsDir verifies that an empty ArtifactsDir
// defaults to "dist".
func TestCIRender_Release_DefaultArtifactsDir(t *testing.T) {
	rc := &project.ReleaseConfig{
		Type:  "simple",
		Build: project.ReleaseBuildConfig{Commands: []string{"make build"}},
		// ArtifactsDir intentionally left empty
	}
	p := gitlabforge.New(gitlabforge.WithReleaseConfig(rc))
	got, err := p.CI().Render("release")
	if err != nil {
		t.Fatalf("Render(\"release\") default ArtifactsDir: %v", err)
	}
	if !strings.Contains(string(got), `"dist"`) {
		t.Error("expected default ARTIFACTS_DIR=dist when ArtifactsDir is empty")
	}
}

// TestCIRender_Release_NoReleaseConfig verifies that Render returns an
// informative error when the provider was constructed without a ReleaseConfig.
func TestCIRender_Release_NoReleaseConfig(t *testing.T) {
	p := gitlabforge.New() // no ReleaseConfig
	_, err := p.CI().Render("release")
	if err == nil {
		t.Fatal("Render(\"release\") without ReleaseConfig: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "release config required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestCIRender_Release_AssembleThenCreate verifies the pipeline comments and
// job structure implement the assemble-then-create pattern:
// upload-packages must precede release-create.
func TestCIRender_Release_AssembleThenCreate(t *testing.T) {
	p := gitlabforge.New(gitlabforge.WithReleaseConfig(goreleaserRC()))
	got, err := p.CI().Render("release")
	if err != nil {
		t.Fatalf("Render(\"release\"): %v", err)
	}
	s := string(got)

	uploadIdx := strings.Index(s, "upload-packages:")
	createIdx := strings.Index(s, "release-create:")
	if uploadIdx < 0 {
		t.Fatal("release pipeline missing upload-packages job")
	}
	if createIdx < 0 {
		t.Fatal("release pipeline missing release-create job")
	}
	if uploadIdx > createIdx {
		t.Error("release-create job appears before upload-packages — assemble-then-create order violated")
	}
}

// TestCIRender_Release_SLSAGapDocumented verifies the SLSA gap is honestly
// stated in the rendered pipeline.
func TestCIRender_Release_SLSAGapDocumented(t *testing.T) {
	p := gitlabforge.New(gitlabforge.WithReleaseConfig(goreleaserRC()))
	got, err := p.CI().Render("release")
	if err != nil {
		t.Fatalf("Render(\"release\"): %v", err)
	}
	if !strings.Contains(string(got), "SLSA") {
		t.Error("release pipeline must document the SLSA gap")
	}
}

// TestCIRender_Release_CosignOIDC verifies cosign keyless signing config.
func TestCIRender_Release_CosignOIDC(t *testing.T) {
	p := gitlabforge.New(gitlabforge.WithReleaseConfig(goreleaserRC()))
	got, err := p.CI().Render("release")
	if err != nil {
		t.Fatalf("Render(\"release\"): %v", err)
	}
	s := string(got)
	checks := []string{"id_tokens:", "SIGSTORE_ID_TOKEN", "sigstore", "cosign sign-blob"}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("release pipeline: cosign OIDC config missing %q", c)
		}
	}
}

// ---------- CIService — docs kind --------------------------------------------

// TestCIRender_Docs verifies the docs pipeline renders a GitLab Pages job.
func TestCIRender_Docs(t *testing.T) {
	// Docs kind does not require a ReleaseConfig.
	p := gitlabforge.New()
	got, err := p.CI().Render("docs")
	if err != nil {
		t.Fatalf("Render(\"docs\"): %v", err)
	}
	s := string(got)

	// REUSE-IgnoreStart
	wantFragments := []string{
		"pages:",
		"mkdocs build",
		"public",
		"CI_PAGES_URL",
		"SPDX-License-Identifier: " + "Apache-2.0",
	}
	// REUSE-IgnoreEnd
	for _, frag := range wantFragments {
		if !strings.Contains(s, frag) {
			t.Errorf("Render(\"docs\") missing fragment %q\nfull output:\n%s", frag, s)
		}
	}
}

// ---------- CIService — unknown kind -----------------------------------------

// TestCIRender_UnknownKind verifies that an unknown kind wraps ErrUnsupported.
func TestCIRender_UnknownKind(t *testing.T) {
	p := gitlabforge.New(gitlabforge.WithReleaseConfig(goreleaserRC()))
	_, err := p.CI().Render("unknown-kind")
	if err == nil {
		t.Fatal("Render(\"unknown-kind\"): expected error, got nil")
	}
	if !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("Render(\"unknown-kind\"): want errors.Is ErrUnsupported, got %v", err)
	}
}

// TestCIRender_CallerUnsupported verifies that the GitHub-specific "caller"
// kind is not supported on the GitLab provider.
func TestCIRender_CallerUnsupported(t *testing.T) {
	p := gitlabforge.New(gitlabforge.WithReleaseConfig(goreleaserRC()))
	_, err := p.CI().Render("caller")
	if err == nil {
		t.Fatal("Render(\"caller\") on GitLab: expected ErrUnsupported, got nil")
	}
	if !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("Render(\"caller\") on GitLab: want ErrUnsupported, got %v", err)
	}
}

// ---------- Provider interface satisfaction -----------------------------------

func TestProviderSatisfiesForge(t *testing.T) {
	var _ forge.Forge = gitlabforge.New()
}

// ---------- New / WithReleaseConfig ------------------------------------------

func TestNew_ZeroValueIsValid(t *testing.T) {
	p := gitlabforge.New()
	if p == nil {
		t.Fatal("New() returned nil")
	}
	if p.Name() != "gitlab" {
		t.Fatalf("Name() = %q, want \"gitlab\"", p.Name())
	}
}

func TestWithReleaseConfig_AttachesConfig(t *testing.T) {
	rc := goreleaserRC()
	p := gitlabforge.New(gitlabforge.WithReleaseConfig(rc))
	// If the config is attached, Render("release") should not error with
	// "release config required".
	_, err := p.CI().Render("release")
	if err != nil && strings.Contains(err.Error(), "release config required") {
		t.Error("WithReleaseConfig did not attach the release config")
	}
}
