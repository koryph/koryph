// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package release implements `koryph release setup`: rendering and installing
// the forge-specific release pipeline and release-please config/manifest files
// into a target project.
//
// The caller workflow (or equivalent forge pipeline) is rendered via the
// project's forge CI service ([forge.CIService].Render("caller")), keeping
// the forge seam sealed. The release-please config and manifest are rendered
// from templates embedded in this package.
//
// Files installed by [Setup] (GitHub default):
//
//   - .github/workflows/release.yml     — caller workflow rendered by the
//     GitHub forge provider
//   - release-please-config.json        — release-please package config
//   - .release-please-manifest.json     — initial version manifest (only when
//     the file does not yet exist, never overwritten)
//
// For other forge providers use [SetupForge] with the appropriate [forge.CIService].
package release

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/koryph/koryph/internal/forge"
	githubforge "github.com/koryph/koryph/internal/forge/github"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/project"
)

// EmbeddedConfigTmpl is the release-please config template source (exported
// for testing).
//
//go:embed release-please-config.json.tmpl
var EmbeddedConfigTmpl string

// EmbeddedManifestTmpl is the release-please manifest template source
// (exported for testing).
//
//go:embed release-please-manifest.json.tmpl
var EmbeddedManifestTmpl string

// templateData is the view-model passed to the release-please config and
// manifest templates. The caller workflow template is owned by the GitHub forge
// provider (internal/forge/github); it uses its own view-model there.
type templateData struct {
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
	// Version is the initial version for the release-please manifest.
	Version string
}

// templateFuncs provides helpers available in all templates.
var templateFuncs = template.FuncMap{
	// join concatenates ss with sep (mirrors strings.Join).
	"join": func(ss []string, sep string) string { return strings.Join(ss, sep) },
}

// SetupResult summarises what koryph release setup installed.
type SetupResult struct {
	// WorkflowPath is the absolute path to the installed caller workflow.
	WorkflowPath string
	// ConfigPath is the absolute path to the release-please config.
	ConfigPath string
	// ManifestPath is the absolute path to the release-please manifest.
	ManifestPath string
	// ManifestCreated is true when the manifest was written for the first
	// time (it is never overwritten if it already exists).
	ManifestCreated bool
	// HumanSteps lists actions that must be completed by a human operator
	// before the release pipeline can run end-to-end.
	HumanSteps []string
}

// Setup renders and installs the release pipeline files into repoRoot using
// the project's release block via the default GitHub forge provider. It:
//
//  1. Validates that rc is non-nil.
//  2. Renders the caller workflow via the GitHub forge CI service.
//  3. Renders release-please-config.json from the embedded template.
//  4. Creates .release-please-manifest.json only if it does not exist yet.
//  5. Returns a SetupResult with paths and remaining HUMAN steps.
//
// All files are written atomically (write-then-rename). The manifest is
// never overwritten — the human operator manages it from the first release
// onwards.
//
// For non-GitHub forge providers use [SetupForge] with the appropriate
// [forge.CIService] (e.g. a GitLab provider's CI service).
func Setup(repoRoot string, rc *project.ReleaseConfig, initialVersion string) (*SetupResult, error) {
	if rc == nil {
		return nil, fmt.Errorf("release: project has no release block; add one to koryph.project.json first")
	}
	ci := githubforge.New(githubforge.WithReleaseConfig(rc)).CI()
	return SetupForge(repoRoot, rc, initialVersion, ci)
}

// SetupForge is the forge-mediated variant of [Setup]. The ci parameter must
// be non-nil; use [Setup] for the default GitHub provider.
//
// ci.Render("caller") is called to produce the forge-native caller workflow
// bytes. For GitHub this renders the GitHub Actions workflow_call snippet;
// for GitLab ci.Render("caller") returns the full release pipeline. All other
// behaviour (release-please config/manifest rendering, atomic file writes) is
// forge-neutral.
//
// This is the consumption point for [forge.CIService] within internal/release:
// GitHub-specific (and GitLab-specific) callers supply the CI service from
// their forge provider, keeping the forge seam sealed and eliminating any
// forge-specific branching within this package.
func SetupForge(repoRoot string, rc *project.ReleaseConfig, initialVersion string, ci forge.CIService) (*SetupResult, error) {
	if rc == nil {
		return nil, fmt.Errorf("release: project has no release block; add one to koryph.project.json first")
	}
	if ci == nil {
		return nil, fmt.Errorf("release: SetupForge: forge CI service required; use Setup for the default GitHub provider")
	}
	if initialVersion == "" {
		initialVersion = "0.0.0"
	}

	td := buildTemplateData(rc, initialVersion)

	// Render the caller workflow via the forge CI service.
	wfBytes, err := ci.Render("caller")
	if err != nil {
		return nil, fmt.Errorf("release: forge CI render workflow: %w", err)
	}

	cfgBytes, err := renderTemplate("release-please-config.json", EmbeddedConfigTmpl, td)
	if err != nil {
		return nil, fmt.Errorf("release: render config: %w", err)
	}
	mfBytes, err := renderTemplate(".release-please-manifest.json", EmbeddedManifestTmpl, td)
	if err != nil {
		return nil, fmt.Errorf("release: render manifest: %w", err)
	}
	// Normalise rendered JSON (config + manifest) so they are always pretty-printed.
	cfgBytes, err = prettifyJSON(cfgBytes)
	if err != nil {
		return nil, fmt.Errorf("release: prettify config json: %w", err)
	}
	mfBytes, err = prettifyJSON(mfBytes)
	if err != nil {
		return nil, fmt.Errorf("release: prettify manifest json: %w", err)
	}

	// Install paths.
	wfPath := filepath.Join(repoRoot, ".github", "workflows", "release.yml")
	cfgPath := filepath.Join(repoRoot, "release-please-config.json")
	mfPath := filepath.Join(repoRoot, ".release-please-manifest.json")

	// Write workflow and config unconditionally (idempotent overwrite).
	if err := fsx.WriteAtomic(wfPath, wfBytes, 0o644); err != nil {
		return nil, fmt.Errorf("release: write %s: %w", wfPath, err)
	}
	if err := fsx.WriteAtomic(cfgPath, cfgBytes, 0o644); err != nil {
		return nil, fmt.Errorf("release: write %s: %w", cfgPath, err)
	}

	// Write the manifest ONLY if it does not yet exist.
	mfCreated := false
	if _, serr := os.Stat(mfPath); os.IsNotExist(serr) {
		if err := fsx.WriteAtomic(mfPath, mfBytes, 0o644); err != nil {
			return nil, fmt.Errorf("release: write %s: %w", mfPath, err)
		}
		mfCreated = true
	}

	res := &SetupResult{
		WorkflowPath:    wfPath,
		ConfigPath:      cfgPath,
		ManifestPath:    mfPath,
		ManifestCreated: mfCreated,
		HumanSteps:      humanSteps(rc),
	}
	return res, nil
}

// buildTemplateData derives the template view-model from a ReleaseConfig.
// This view-model is used for the release-please config and manifest templates;
// the caller workflow template is owned by the forge provider.
func buildTemplateData(rc *project.ReleaseConfig, version string) templateData {
	td := templateData{
		Type:         rc.Type,
		ExtraFiles:   rc.ExtraFiles,
		ArtifactsDir: rc.ArtifactsDir,
		SBOM:         rc.SBOM,
		Provenance:   rc.Provenance,
		Version:      version,
	}
	if rc.ArtifactsDir == "" {
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

// renderTemplate parses and executes a text/template source against data.
func renderTemplate(name, src string, data any) ([]byte, error) {
	tmpl, err := template.New(name).Funcs(templateFuncs).Parse(src)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// prettifyJSON round-trips JSON through encoding/json to produce canonical
// 2-space indented output with a trailing newline.
func prettifyJSON(src []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(src, &v); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// RenderCallerWorkflow renders the caller workflow for the project's GitHub
// forge CI service and returns the raw YAML bytes. This is exported so the
// doctor package can compare the installed .github/workflows/release.yml
// against the current Render output without re-running the full setup flow.
//
// The rendering is delegated to [forge.CIService.Render]("caller") on the
// GitHub provider, so the output is always identical to what [Setup] would
// install.
func RenderCallerWorkflow(rc *project.ReleaseConfig) ([]byte, error) {
	if rc == nil {
		return nil, fmt.Errorf("release: RenderCallerWorkflow: ReleaseConfig is nil")
	}
	return githubforge.New(githubforge.WithReleaseConfig(rc)).CI().Render("caller")
}

// humanSteps returns the ordered list of actions a human operator must
// complete before the release pipeline can run end-to-end. These are printed
// by the CLI after setup so the operator knows what remains.
func humanSteps(rc *project.ReleaseConfig) []string {
	steps := []string{
		"Bootstrap GitHub App: if no release-bot app exists, provision one and note its App ID + private key (see docs/user-guide/signing.md).",
		"Set repository secrets: RELEASE_BOT_APP_ID and RELEASE_BOT_PRIVATE_KEY (required by release-train.yml).",
		"Review branch-protection rulesets: ensure the release bot's GitHub App identity is in the 'Bypass pull request requirements' list on main.",
		"Commit and push the generated files (.github/workflows/release.yml, release-please-config.json" +
			func() string {
				if true { // manifest path
					return ", .release-please-manifest.json"
				}
				return ""
			}() + ") to trigger the first release-please run.",
	}
	if rc.Build.Goreleaser != nil {
		steps = append(steps, "Add or verify .goreleaser.yaml at the repository root (GoReleaser build mode).")
	}
	if rc.Provenance {
		steps = append(steps,
			"Confirm the repository has 'id-token: write' permission available (required by slsa-framework/slsa-github-generator).",
		)
	}
	return steps
}
