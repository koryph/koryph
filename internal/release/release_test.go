// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package release_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/release"
)

// goreleaser mode release config used across tests.
func goreleaserConfig() *project.ReleaseConfig {
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

// commands mode release config.
func commandsConfig() *project.ReleaseConfig {
	return &project.ReleaseConfig{
		Type:  "simple",
		Build: project.ReleaseBuildConfig{Commands: []string{"make build", "make package"}},
	}
}

// TestSetup_GoreleaserMode verifies Setup installs all three files in
// goreleaser (mode A) configuration.
func TestSetup_GoreleaserMode(t *testing.T) {
	root := t.TempDir()
	res, err := release.Setup(root, goreleaserConfig(), "0.3.0")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// All three output files must exist.
	for _, p := range []string{res.WorkflowPath, res.ConfigPath, res.ManifestPath} {
		if _, serr := os.Stat(p); serr != nil {
			t.Errorf("expected file %s to exist: %v", p, serr)
		}
	}

	// Manifest created for first time.
	if !res.ManifestCreated {
		t.Error("ManifestCreated should be true on first setup")
	}

	// Workflow references the reusable workflow.
	wf, err := os.ReadFile(res.WorkflowPath)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	if !strings.Contains(string(wf), "koryph/koryph/.github/workflows/release-train.yml@main") {
		t.Error("workflow does not reference release-train.yml@main")
	}
	if !strings.Contains(string(wf), `build_mode: "goreleaser"`) {
		t.Error("workflow missing build_mode: goreleaser")
	}
	if !strings.Contains(string(wf), "~> v2.16") {
		t.Error("workflow missing goreleaser version")
	}

	// Config has the release type.
	cfg, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(cfg), `"release-type": "go"`) {
		t.Error("config missing release-type go")
	}
	if !strings.Contains(string(cfg), "internal/version/version.go") {
		t.Error("config missing extra-files entry")
	}

	// Manifest has the initial version.
	mf, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(mf), "0.3.0") {
		t.Error("manifest missing initial version 0.3.0")
	}

	// Human steps are non-empty.
	if len(res.HumanSteps) == 0 {
		t.Error("HumanSteps should be non-empty")
	}
}

// TestSetup_CommandsMode verifies Setup works for commands (mode B).
func TestSetup_CommandsMode(t *testing.T) {
	root := t.TempDir()
	res, err := release.Setup(root, commandsConfig(), "")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	wf, err := os.ReadFile(res.WorkflowPath)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	if !strings.Contains(string(wf), `build_mode: "commands"`) {
		t.Error("workflow missing build_mode: commands")
	}
	// Goreleaser version input must be absent.
	if strings.Contains(string(wf), "goreleaser_version") {
		t.Error("workflow must not contain goreleaser_version in commands mode")
	}
}

// TestSetup_NilReleaseConfig verifies Setup returns an error when called
// without a release block.
func TestSetup_NilReleaseConfig(t *testing.T) {
	root := t.TempDir()
	_, err := release.Setup(root, nil, "")
	if err == nil {
		t.Error("Setup with nil ReleaseConfig should return an error")
	}
}

// TestSetup_ManifestNotOverwritten verifies the manifest is never overwritten
// on a second Setup call.
func TestSetup_ManifestNotOverwritten(t *testing.T) {
	root := t.TempDir()
	if _, err := release.Setup(root, goreleaserConfig(), "0.1.0"); err != nil {
		t.Fatalf("first Setup: %v", err)
	}
	// Overwrite the manifest manually to simulate a human bump.
	mfPath := filepath.Join(root, ".release-please-manifest.json")
	if err := os.WriteFile(mfPath, []byte(`{".":"1.2.3"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write custom manifest: %v", err)
	}
	res, err := release.Setup(root, goreleaserConfig(), "0.2.0")
	if err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if res.ManifestCreated {
		t.Error("ManifestCreated should be false on second setup")
	}
	// Content must be the human-written version, not the template output.
	mf, err := os.ReadFile(mfPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(mf), "1.2.3") {
		t.Errorf("manifest overwritten; got %s, want 1.2.3", string(mf))
	}
}

// TestSetup_WorkflowPathLocation verifies the workflow lands in
// .github/workflows/release.yml relative to the repo root.
func TestSetup_WorkflowPathLocation(t *testing.T) {
	root := t.TempDir()
	res, err := release.Setup(root, goreleaserConfig(), "")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	want := filepath.Join(root, ".github", "workflows", "release.yml")
	if res.WorkflowPath != want {
		t.Errorf("WorkflowPath = %q, want %q", res.WorkflowPath, want)
	}
}

// TestSetup_DefaultArtifactsDir verifies ArtifactsDir defaults to "dist".
func TestSetup_DefaultArtifactsDir(t *testing.T) {
	root := t.TempDir()
	rc := &project.ReleaseConfig{
		Type:  "go",
		Build: project.ReleaseBuildConfig{Goreleaser: &project.GoreleaserBuild{Version: "~> v2"}},
	}
	res, err := release.Setup(root, rc, "")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	wf, err := os.ReadFile(res.WorkflowPath)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	if !strings.Contains(string(wf), `artifacts_dir: "dist"`) {
		t.Error("workflow should default artifacts_dir to dist when not set")
	}
}
