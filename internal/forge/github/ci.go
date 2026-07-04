// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

import (
	"fmt"

	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/release"
)

// githubCISvc implements [forge.CIService] for GitHub Actions.
//
// It renders forge-appropriate CI/CD pipeline asset files using the
// internal/release template system. The rc field must be non-nil for the
// "caller" kind; other kinds are stubbed for future extraction beads.
type githubCISvc struct {
	rc *project.ReleaseConfig
}

// Render produces the content of a GitHub Actions pipeline asset file.
//
// Recognised kinds:
//
//   - "caller" — the reusable release-train caller workflow
//     (.github/workflows/release.yml). Requires a non-nil ReleaseConfig;
//     build the provider with [WithReleaseConfig].
//
// All other kinds return [forge.ErrUnsupported].
func (s *githubCISvc) Render(kind string) ([]byte, error) {
	switch kind {
	case "caller":
		return s.renderCaller()
	default:
		return nil, fmt.Errorf("github CI: Render(%q): %w", kind, forge.ErrUnsupported)
	}
}

// renderCaller renders the release-train caller workflow from the project's
// ReleaseConfig using the internal/release embedded template.
func (s *githubCISvc) renderCaller() ([]byte, error) {
	if s.rc == nil {
		return nil, fmt.Errorf("github CI: Render(\"caller\"): release config required; " +
			"build the provider with github.WithReleaseConfig(rc)")
	}
	b, err := release.RenderCallerWorkflow(s.rc)
	if err != nil {
		return nil, fmt.Errorf("github CI: render caller workflow: %w", err)
	}
	return b, nil
}
