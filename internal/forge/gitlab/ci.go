// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/project"
)

//go:embed release-pipeline.yml.tmpl
var embeddedReleasePipelineTmpl string

//go:embed docs-pipeline.yml.tmpl
var embeddedDocsPipelineTmpl string

// gitlabCISvc implements [forge.CIService] for GitLab CI/CD.
//
// It renders forge-appropriate .gitlab-ci.yml pipeline assets using embedded
// templates. The rc field is required for the "release" kind.
type gitlabCISvc struct {
	rc *project.ReleaseConfig
}

// gitlabTemplateData is the view-model passed to the GitLab CI templates.
// It carries the fields from the project's ReleaseConfig that vary the
// rendered pipeline content.
type gitlabTemplateData struct {
	// ArtifactsDir is the directory where build artifacts land (default "dist").
	ArtifactsDir string
	// BuildMode is "goreleaser" or "commands".
	BuildMode string
	// GoreleaserVersion is the goreleaser image tag; non-empty for goreleaser mode.
	GoreleaserVersion string
	// BuildCommands is the ordered shell command list; non-empty for commands mode.
	BuildCommands []string
	// SBOM enables SBOM generation.
	SBOM bool
	// Provenance enables provenance; for GitLab this is cosign keyless only
	// (SLSA GitHub Generator is not available — gap is documented in the template).
	Provenance bool
}

// ciTemplateFuncs provides helpers for the GitLab CI templates.
var ciTemplateFuncs = template.FuncMap{
	"join": func(ss []string, sep string) string { return strings.Join(ss, sep) },
}

// Render produces the content of a GitLab CI/CD pipeline asset file.
//
// Recognised kinds:
//
//   - "release" — the .gitlab-ci.yml implementing the full release train:
//     conventional-commit version computation, Release MR via project access
//     token, gate-before-tag, assemble-then-create artifact upload, cosign
//     keyless signing. Requires a non-nil ReleaseConfig.
//
//   - "docs" — the .gitlab-ci.yml for the GitLab Pages docs-publish pipeline.
//     Does not require a ReleaseConfig.
//
// All other kinds return [forge.ErrUnsupported].
func (s *gitlabCISvc) Render(kind string) ([]byte, error) {
	switch kind {
	case "release":
		return s.renderRelease()
	case "docs":
		return s.renderDocs()
	default:
		return nil, fmt.Errorf("gitlab CI: Render(%q): %w", kind, forge.ErrUnsupported)
	}
}

// renderRelease renders the release pipeline template from the project's
// ReleaseConfig. It enforces that rc is non-nil — callers must build the
// provider with [WithReleaseConfig].
func (s *gitlabCISvc) renderRelease() ([]byte, error) {
	if s.rc == nil {
		return nil, fmt.Errorf("gitlab CI: Render(\"release\"): release config required; " +
			"build the provider with gitlab.WithReleaseConfig(rc)")
	}
	td := buildGitLabTemplateData(s.rc)
	b, err := renderCITemplate("release-pipeline.yml", embeddedReleasePipelineTmpl, td)
	if err != nil {
		return nil, fmt.Errorf("gitlab CI: render release pipeline: %w", err)
	}
	return b, nil
}

// renderDocs renders the GitLab Pages docs-publish pipeline. This kind does
// not require a ReleaseConfig.
func (s *gitlabCISvc) renderDocs() ([]byte, error) {
	b, err := renderCITemplate("docs-pipeline.yml", embeddedDocsPipelineTmpl, nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab CI: render docs pipeline: %w", err)
	}
	return b, nil
}

// buildGitLabTemplateData derives the template view-model from a ReleaseConfig.
func buildGitLabTemplateData(rc *project.ReleaseConfig) gitlabTemplateData {
	td := gitlabTemplateData{
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
			v = "v2"
		}
		// Normalise "~> v2.16" to just the image tag (strip "~> " prefix).
		td.GoreleaserVersion = strings.TrimPrefix(strings.TrimPrefix(v, "~> "), "v")
	} else {
		td.BuildMode = "commands"
		td.BuildCommands = rc.Build.Commands
	}
	return td
}

// renderCITemplate parses and executes a text/template source against data.
func renderCITemplate(name, src string, data any) ([]byte, error) {
	tmpl, err := template.New(name).Funcs(ciTemplateFuncs).Parse(src)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
