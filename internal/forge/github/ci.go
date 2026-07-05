// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"

	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/release"
)

//go:embed gate-workflow.yml.tmpl
var embeddedGateWorkflowTmpl string

// defaultGateCmd is the gate command used when none is specified.
const defaultGateCmd = "make gate"

// githubCISvc implements [forge.CIService] for GitHub Actions.
//
// It renders forge-appropriate CI/CD pipeline asset files using the
// internal/release template system. The rc field must be non-nil for the
// "caller" kind; gateCmd is optional (defaults to [defaultGateCmd]) for the
// "gate" kind.
type githubCISvc struct {
	rc      *project.ReleaseConfig
	gateCmd string // empty means use defaultGateCmd
}

// githubGateTemplateData is the view-model passed to the gate workflow template.
type githubGateTemplateData struct {
	// GateCmd is the shell command that runs the project's green gate.
	GateCmd string
}

// Render produces the content of a GitHub Actions pipeline asset file.
//
// Recognised kinds:
//
//   - "caller" — the reusable release-train caller workflow
//     (.github/workflows/release.yml). Requires a non-nil ReleaseConfig;
//     build the provider with [WithReleaseConfig].
//
//   - "gate" — the green gate workflow (.github/workflows/gate.yml). Runs the
//     project's gate command on every push and pull_request. The gate command
//     defaults to "make gate"; override it with [WithGateCommand].
//
// All other kinds return [forge.ErrUnsupported].
func (s *githubCISvc) Render(kind string) ([]byte, error) {
	switch kind {
	case "caller":
		return s.renderCaller()
	case "gate":
		return s.renderGate()
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

// renderGate renders the green gate workflow from the embedded template.
// The gate command defaults to [defaultGateCmd] when none was supplied.
func (s *githubCISvc) renderGate() ([]byte, error) {
	cmd := s.gateCmd
	if cmd == "" {
		cmd = defaultGateCmd
	}
	td := githubGateTemplateData{GateCmd: cmd}
	tmpl, err := template.New("gate-workflow.yml").Parse(embeddedGateWorkflowTmpl)
	if err != nil {
		return nil, fmt.Errorf("github CI: parse gate workflow template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, td); err != nil {
		return nil, fmt.Errorf("github CI: render gate workflow: %w", err)
	}
	return buf.Bytes(), nil
}
