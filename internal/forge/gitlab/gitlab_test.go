// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab_test

import (
	"context"
	"errors"
	"testing"

	"github.com/koryph/koryph/internal/forge"
	_ "github.com/koryph/koryph/internal/forge/gitlab" // register provider
)

func TestGitLabProviderRegistered(t *testing.T) {
	gl, ok := forge.Default.Get("gitlab")
	if !ok {
		t.Fatal("gitlab provider not registered")
	}
	if gl.Name() != "gitlab" {
		t.Errorf("Name() = %q, want gitlab", gl.Name())
	}
}

func TestGitLabCapabilities(t *testing.T) {
	gl, _ := forge.Default.Get("gitlab")
	caps := gl.Capabilities()

	// GitLab v1 does NOT have these GitHub-only capabilities.
	if caps.DraftReleases {
		t.Error("DraftReleases should be false for GitLab v1")
	}
	if caps.AppIdentity {
		t.Error("AppIdentity should be false for GitLab (access token flow)")
	}
	if caps.Rulesets {
		t.Error("Rulesets should be false for GitLab v1")
	}

	// GitLab DOES support Pages hosting.
	if !caps.PagesHosting {
		t.Error("PagesHosting should be true for GitLab")
	}
}

func TestGitLabServicesReturnStubOrReal(t *testing.T) {
	gl, _ := forge.Default.Get("gitlab")

	// These should return non-nil service objects (stubs or real).
	if gl.Repo() == nil {
		t.Error("Repo() returned nil")
	}
	if gl.Protection() == nil {
		t.Error("Protection() returned nil")
	}
	if gl.PRs() == nil {
		t.Error("PRs() returned nil")
	}
	if gl.Secrets() == nil {
		t.Error("Secrets() returned nil")
	}
	if gl.Releases() == nil {
		t.Error("Releases() returned nil")
	}
	if gl.CI() == nil {
		t.Error("CI() returned nil")
	}
	if gl.Bot() == nil {
		t.Error("Bot() returned nil")
	}
}

func TestGitLabStubServicesReturnUnsupported(t *testing.T) {
	gl, _ := forge.Default.Get("gitlab")

	// ReleaseService is still stubbed — must return ErrUnsupported.
	if _, err := gl.Releases().Create(context.Background(), "ns", "proj", "v1.0", "notes"); err != forge.ErrUnsupported { //nolint:staticcheck
		t.Errorf("Releases().Create: want ErrUnsupported, got %v", err)
	}

	// Protection().List for a bare group name (no "/") returns ErrUnsupported
	// (group-level protection not yet supported), wrapped in a context error.
	_, err := gl.Protection().List(context.Background(), "mygroup") //nolint:staticcheck
	if !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("Protection().List(group): want wrapped ErrUnsupported, got %v", err)
	}

	// RepoService and ProtectionService are now real implementations; their
	// ErrUnsupported methods are VulnAlerts / ActionsWorkflow — tested in repo_test.go.
}

// TestGitLabCIDocsRendersWithoutConfig verifies that CI().Render("docs")
// works even with the zero-config provider (no ReleaseConfig required for docs).
func TestGitLabCIDocsRendersWithoutConfig(t *testing.T) {
	gl, _ := forge.Default.Get("gitlab")
	got, err := gl.CI().Render("docs")
	if err != nil {
		t.Fatalf("CI().Render(\"docs\"): unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Error("CI().Render(\"docs\"): returned empty content")
	}
}

// TestGitLabCIReleaseRequiresConfig verifies that CI().Render("release")
// on the zero-config provider returns an informative error.
func TestGitLabCIReleaseRequiresConfig(t *testing.T) {
	gl, _ := forge.Default.Get("gitlab")
	_, err := gl.CI().Render("release")
	if err == nil {
		t.Fatal("CI().Render(\"release\") without ReleaseConfig: expected error, got nil")
	}
}

func TestGitLabBotExchangeManifestUnsupported(t *testing.T) {
	gl, _ := forge.Default.Get("gitlab")
	_, err := gl.Bot().ExchangeManifest(nil, "some-code") //nolint:staticcheck
	if err == nil {
		t.Fatal("ExchangeManifest: expected error for GitLab (no App manifest concept)")
	}
}
