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
)

//go:embed caller-workflow.yml.tmpl
var embeddedCallerWorkflowTmpl string

//go:embed gate-workflow.yml.tmpl
var embeddedGateWorkflowTmpl string

// githubCISvc implements [forge.CIService] for GitHub Actions.
//
// It renders forge-appropriate CI/CD pipeline asset files using embedded
// templates. The rc field must be non-nil for the "caller" kind; gateCmd is
// optional (defaults to [forge.DefaultGateCommand]) for the "gate" kind.
type githubCISvc struct {
	rc      *project.ReleaseConfig
	gateCmd string // empty means use forge.DefaultGateCommand
}

// callerWorkflowData is the view-model passed to the caller-workflow template.
//
// Field names must match the {{.Field}} references in caller-workflow.yml.tmpl.
// The Version field from the manifest template is intentionally absent here —
// the caller workflow template does not reference it.
type callerWorkflowData struct {
	// Type is the release-please release type (e.g. "go", "simple").
	Type string
	// ExtraFiles is the list of additional version-bearing files.
	ExtraFiles []string
	// ArtifactsDir is the build artifact directory (default "dist").
	ArtifactsDir string
	// BuildMode is "goreleaser" or "commands".
	BuildMode string
	// GoreleaserVersion is the goreleaser action version constraint; empty
	// when build mode is "commands".
	GoreleaserVersion string
	// BuildCommands is the ordered shell command list; empty when build mode
	// is "goreleaser".
	BuildCommands []string
	// SBOM enables SBOM generation in the caller workflow.
	SBOM bool
	// Provenance enables SLSA provenance in the caller workflow.
	Provenance bool
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

// renderCaller renders the release-train caller workflow from the embedded
// caller-workflow.yml.tmpl template and the project's ReleaseConfig.
func (s *githubCISvc) renderCaller() ([]byte, error) {
	if s.rc == nil {
		return nil, fmt.Errorf("github CI: Render(\"caller\"): release config required; " +
			"build the provider with github.WithReleaseConfig(rc)")
	}
	td := buildCallerWorkflowData(s.rc)
	tmpl, err := template.New("caller-workflow.yml").Funcs(forge.TemplateFuncs).Parse(embeddedCallerWorkflowTmpl)
	if err != nil {
		return nil, fmt.Errorf("github CI: parse caller workflow template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, td); err != nil {
		return nil, fmt.Errorf("github CI: render caller workflow: %w", err)
	}
	return buf.Bytes(), nil
}

// buildCallerWorkflowData derives the caller-workflow view-model from a
// ReleaseConfig.
func buildCallerWorkflowData(rc *project.ReleaseConfig) callerWorkflowData {
	td := callerWorkflowData{
		Type:         rc.Type,
		ExtraFiles:   rc.ExtraFiles,
		ArtifactsDir: rc.ArtifactsDir,
		SBOM:         rc.SBOM,
		Provenance:   rc.Provenance,
	}
	if td.ArtifactsDir == "" {
		td.ArtifactsDir = "dist"
	}
	if rc.Build.Goreleaser != nil {
		td.BuildMode = "goreleaser"
		v := rc.Build.Goreleaser.Version
		if v == "" {
			v = "~> v2"
		}
		td.GoreleaserVersion = v
	} else {
		td.BuildMode = "commands"
		td.BuildCommands = rc.Build.Commands
	}
	return td
}

// renderGate renders the green gate workflow from the embedded template.
// The gate command defaults to [forge.DefaultGateCommand] when none was supplied.
func (s *githubCISvc) renderGate() ([]byte, error) {
	td := forge.GateTemplateData{GateCmd: forge.ResolveGateCommand(s.gateCmd)}
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
