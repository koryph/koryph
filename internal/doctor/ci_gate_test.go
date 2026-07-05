// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
)

// fakeCIService is a minimal forge.CIService stub that returns a fixed
// response for the "gate" kind so tests never call into real forge providers.
type fakeCIService struct {
	content []byte
	err     error
}

func (f *fakeCIService) Render(kind string) ([]byte, error) {
	if kind != "gate" {
		return nil, fmt.Errorf("fakeCIService: unsupported kind %q", kind)
	}
	return f.content, f.err
}

// ciGateOpts returns ProjectOptions wired for CI gate tests.
//
//   - Pass a non-nil fakeCIService to exercise the present/drifted/current
//     paths; the service is injected directly (forge remote detection skipped).
//   - Pass nil fakeCIService + set GitHubRepo to return ("", nil) to test the
//     no-remote skip path.
func ciGateOpts(root string, svc *fakeCIService) ProjectOptions {
	o := projectOpts(root)
	if svc != nil {
		o.CIService = svc
	} else {
		// No CIService + empty GitHubRepo = "no forge remote" path.
		o.GitHubRepo = func(_ string) (string, error) { return "", nil }
	}
	return o
}

const testGatePipelineRel = ".github/workflows/koryph-gate.yml"

// installGatePipeline writes content to the conventional gate pipeline path.
func installGatePipeline(t *testing.T, root string, content []byte) {
	t.Helper()
	dest := filepath.Join(root, testGatePipelineRel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// addGiteForge writes a koryph.project.json with forge="github" so
// gatePipelinePath resolves to the GitHub path.
func addGitHubForge(t *testing.T, root string) {
	t.Helper()
	cfgPath := filepath.Join(root, project.ConfigFileName)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("addGitHubForge: read config: %v", err)
	}
	// Replace the file with forge="github" added.
	patched := strings.ReplaceAll(string(data), `"work_source"`, `"forge":"github","work_source"`)
	if err := os.WriteFile(cfgPath, []byte(patched), 0o644); err != nil {
		t.Fatalf("addGitHubForge: write config: %v", err)
	}
}

// --- ci-assets: no forge remote → skip gracefully (ok) ---

func TestCIGatePipelineNoRemote(t *testing.T) {
	root := fabricateProject(t)
	o := ciGateOpts(root, nil) // nil svc → GitHubRepo returns ("", nil)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameCIAssets)
	if f.Level != LevelOK {
		t.Errorf("ci-assets level=%s msg=%q, want ok when no forge remote", f.Level, f.Message)
	}
}

// --- ci-assets: gate pipeline absent → warn + remediation hint ---

func TestCIGatePipelineAbsent(t *testing.T) {
	root := fabricateProject(t)
	addGitHubForge(t, root)
	rendered := []byte("# gate pipeline content\n")
	svc := &fakeCIService{content: rendered}
	o := ciGateOpts(root, svc)
	// Do NOT install the gate pipeline file.
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameCIAssets)
	if f.Level != LevelWarn {
		t.Errorf("ci-assets level=%s msg=%q, want warn when gate pipeline absent", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "koryph ci setup") {
		t.Errorf("ci-assets message=%q: want remediation hint 'koryph ci setup'", f.Message)
	}
}

// --- ci-assets: gate pipeline drifted → warn + remediation hint ---

func TestCIGatePipelineDrifted(t *testing.T) {
	root := fabricateProject(t)
	addGitHubForge(t, root)
	current := []byte("# current gate pipeline\n")
	installGatePipeline(t, root, []byte("# old gate pipeline\n"))
	svc := &fakeCIService{content: current}
	o := ciGateOpts(root, svc)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameCIAssets)
	if f.Level != LevelWarn {
		t.Errorf("ci-assets level=%s msg=%q, want warn when gate pipeline drifted", f.Level, f.Message)
	}
	if !strings.Contains(f.Message, "koryph ci setup") {
		t.Errorf("ci-assets message=%q: want remediation hint 'koryph ci setup'", f.Message)
	}
}

// --- ci-assets: gate pipeline present and current → ok ---

func TestCIGatePipelineCurrent(t *testing.T) {
	root := fabricateProject(t)
	addGitHubForge(t, root)
	content := []byte("# gate pipeline content\n")
	installGatePipeline(t, root, content)
	svc := &fakeCIService{content: content}
	o := ciGateOpts(root, svc)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameCIAssets)
	if f.Level != LevelOK {
		t.Errorf("ci-assets level=%s msg=%q, want ok when gate pipeline matches template", f.Level, f.Message)
	}
}

// --- ci-assets: render returns ErrUnsupported → ok (skip, not warn) ---

func TestCIGatePipelineRenderUnsupported(t *testing.T) {
	root := fabricateProject(t)
	addGitHubForge(t, root)
	svc := &fakeCIService{err: fmt.Errorf("gate: unsupported")}
	o := ciGateOpts(root, svc)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameCIAssets)
	if f.Level != LevelOK {
		t.Errorf("ci-assets level=%s msg=%q, want ok (skip) when Render returns error", f.Level, f.Message)
	}
}
