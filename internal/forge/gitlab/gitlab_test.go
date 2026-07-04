// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab_test

import (
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

	// Stubs must return ErrUnsupported, not panic.
	if _, err := gl.Repo().Get(nil, "ns", "proj"); err != forge.ErrUnsupported { //nolint:staticcheck
		t.Errorf("Repo().Get: want ErrUnsupported, got %v", err)
	}
	if _, err := gl.Protection().List(nil, "target"); err != forge.ErrUnsupported { //nolint:staticcheck
		t.Errorf("Protection().List: want ErrUnsupported, got %v", err)
	}
	if _, err := gl.Releases().Create(nil, "ns", "proj", "v1.0", "notes"); err != forge.ErrUnsupported { //nolint:staticcheck
		t.Errorf("Releases().Create: want ErrUnsupported, got %v", err)
	}
	if _, err := gl.CI().Render("docs"); err != forge.ErrUnsupported {
		t.Errorf("CI().Render: want ErrUnsupported, got %v", err)
	}
}

func TestGitLabBotExchangeManifestUnsupported(t *testing.T) {
	gl, _ := forge.Default.Get("gitlab")
	_, err := gl.Bot().ExchangeManifest(nil, "some-code") //nolint:staticcheck
	if err == nil {
		t.Fatal("ExchangeManifest: expected error for GitLab (no App manifest concept)")
	}
}
