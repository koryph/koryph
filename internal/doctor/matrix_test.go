// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
	"github.com/koryph/koryph/internal/signing"
)

// --- test helpers ------------------------------------------------------------

// fabricateMatrixProject creates a minimal project directory suitable for
// BuildMatrix tests: a git root, a valid koryph.project.json, and an optional
// customizer func to alter the config before it is written.
func fabricateMatrixProject(t *testing.T, customize func(*project.Config)) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &project.Config{
		SchemaVersion:   1,
		ProjectID:       "testproject",
		WorkSource:      "bd",
		Gate:            []string{"make test"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
	}
	if customize != nil {
		customize(cfg)
	}
	if err := cfg.Save(root); err != nil {
		t.Fatal("save project config:", err)
	}
	return root
}

// fixedTime is the injectable timestamp used across all matrix tests.
var fixedTime = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

// baseMatrixOpts returns MatrixOptions with test-friendly injectable deps:
// no real PATH probing, no real vault I/O, no real Stat.
func baseMatrixOpts(
	lookPath func(string) (string, error),
	vaultCfg *signing.VaultConfig,
	stat func(string) (os.FileInfo, error),
) MatrixOptions {
	return MatrixOptions{
		LookPath: lookPath,
		LoadVaultConfig: func() (*signing.VaultConfig, error) {
			if vaultCfg == nil {
				return &signing.VaultConfig{Providers: map[string]signing.ProviderTemplates{}}, nil
			}
			return vaultCfg, nil
		},
		Stat: stat,
		Now:  func() time.Time { return fixedTime },
	}
}

// findMatrixRow returns the first row matching category and name, or nil.
func findMatrixRow(m *Matrix, category, name string) *IntegrationRow {
	for i := range m.Rows {
		r := &m.Rows[i]
		if r.Category == category && r.Name == name {
			return r
		}
	}
	return nil
}

// --- signing -----------------------------------------------------------------

func TestMatrixSigningMissing(t *testing.T) {
	root := fabricateMatrixProject(t, nil)
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "signing", "signing")
	if row == nil {
		t.Fatal("no signing row")
	}
	if row.Status != MatrixMissing {
		t.Errorf("status = %q, want missing", row.Status)
	}
	if row.Suggestion == "" {
		t.Error("suggestion must not be empty for missing signing")
	}
}

func TestMatrixSigningIncomplete(t *testing.T) {
	// Signing configured (provider set) but public_key not yet captured.
	root := fabricateMatrixProject(t, func(cfg *project.Config) {
		cfg.Signing = &signing.Config{
			Mode:     signing.ModeSSH,
			Provider: signing.ProviderFile,
			KeyRef:   "/tmp/key",
			Identity: "bot@example.com",
			// PublicKey intentionally omitted
		}
	})
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "signing", "signing")
	if row == nil {
		t.Fatal("no signing row")
	}
	if row.Status != MatrixWarn {
		t.Errorf("status = %q, want warn", row.Status)
	}
	if row.Suggestion == "" {
		t.Error("suggestion must not be empty for incomplete signing")
	}
}

func TestMatrixSigningOK(t *testing.T) {
	root := fabricateMatrixProject(t, func(cfg *project.Config) {
		cfg.Signing = &signing.Config{
			Required:  true,
			Mode:      signing.ModeSSH,
			Provider:  signing.ProviderProtonPass,
			Identity:  "bot@example.com",
			PublicKey: "ssh-ed25519 AAAA...",
		}
	})
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "signing", "signing")
	if row == nil {
		t.Fatal("no signing row")
	}
	if row.Status != MatrixOK {
		t.Errorf("status = %q, want ok (detail: %s)", row.Status, row.Detail)
	}
	if row.Suggestion != "" {
		t.Errorf("suggestion should be empty for ok signing, got %q", row.Suggestion)
	}
}

// --- intake ------------------------------------------------------------------

func TestMatrixIntakeAllMissing(t *testing.T) {
	// No intake sources configured at all.
	root := fabricateMatrixProject(t, nil)
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range intakeProviders {
		row := findMatrixRow(m, "intake", p)
		if row == nil {
			t.Fatalf("no intake/%s row", p)
		}
		if row.Status != MatrixMissing {
			t.Errorf("intake/%s status = %q, want missing", p, row.Status)
		}
		if row.Suggestion == "" {
			t.Errorf("intake/%s: suggestion must not be empty", p)
		}
	}
}

func TestMatrixIntakeGitHubConfigured(t *testing.T) {
	root := fabricateMatrixProject(t, func(cfg *project.Config) {
		cfg.Intake = []project.IntakeSource{
			{Provider: "github", Source: "acme/widgets"},
			{Provider: "github", Source: "acme/core"},
		}
	})
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "intake", "github")
	if row == nil {
		t.Fatal("no intake/github row")
	}
	if row.Status != MatrixOK {
		t.Errorf("status = %q, want ok", row.Status)
	}
	// Jira and linear should still be missing.
	for _, p := range []string{"jira", "linear"} {
		r := findMatrixRow(m, "intake", p)
		if r == nil {
			t.Fatalf("no intake/%s row", p)
		}
		if r.Status != MatrixMissing {
			t.Errorf("intake/%s status = %q, want missing", p, r.Status)
		}
	}
}

func TestMatrixIntakeDefaultProviderIsGitHub(t *testing.T) {
	// An intake entry with no provider should count as github.
	root := fabricateMatrixProject(t, func(cfg *project.Config) {
		cfg.Intake = []project.IntakeSource{
			{Source: "acme/widgets"}, // provider == "" → github
		}
	})
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "intake", "github")
	if row == nil {
		t.Fatal("no intake/github row")
	}
	if row.Status != MatrixOK {
		t.Errorf("status = %q, want ok (blank provider should default to github)", row.Status)
	}
}

func TestMatrixIntakeAllProviders(t *testing.T) {
	root := fabricateMatrixProject(t, func(cfg *project.Config) {
		cfg.Intake = []project.IntakeSource{
			{Provider: "github", Source: "acme/widgets"},
			{Provider: "jira", Source: "acme.atlassian.net/ENG"},
			{Provider: "linear", Source: "ENG"},
		}
	})
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range intakeProviders {
		row := findMatrixRow(m, "intake", p)
		if row == nil {
			t.Fatalf("no intake/%s row", p)
		}
		if row.Status != MatrixOK {
			t.Errorf("intake/%s status = %q, want ok", p, row.Status)
		}
	}
}

// --- runtimes ----------------------------------------------------------------

func TestMatrixRuntimesEmptyRegistry(t *testing.T) {
	root := fabricateMatrixProject(t, nil)
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })
	// opts.Runtimes is nil → no runtime rows

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range m.Rows {
		if row.Category == "runtime" {
			t.Errorf("unexpected runtime row: %+v", row)
		}
	}
}

func TestMatrixRuntimeBinaryMissing(t *testing.T) {
	root := fabricateMatrixProject(t, nil)

	reg := runtime.NewRegistry()
	stub := runtimetest.Stub{StubName: "myruntime", Present: false}
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}

	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })
	opts.Runtimes = reg
	opts.DetectRuntime = func(_ context.Context, rt runtime.Runtime) (bool, string) {
		return rt.(runtimetest.Stub).Present, rt.(runtimetest.Stub).Version
	}

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "runtime", "myruntime")
	if row == nil {
		t.Fatal("no runtime/myruntime row")
	}
	if row.Status != MatrixMissing {
		t.Errorf("status = %q, want missing", row.Status)
	}
	if row.Suggestion == "" {
		t.Error("suggestion must not be empty when binary is missing")
	}
}

func TestMatrixRuntimeBinaryPresentButNotAuthenticated(t *testing.T) {
	root := fabricateMatrixProject(t, nil)

	reg := runtime.NewRegistry()
	authErr := errors.New("not logged in")
	stub := runtimetest.Stub{StubName: "myruntime", Present: true, Version: "1.0.0", AuthErr: authErr}
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}

	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })
	opts.Runtimes = reg
	opts.DetectRuntime = func(_ context.Context, rt runtime.Runtime) (bool, string) {
		s := rt.(runtimetest.Stub)
		return s.Present, s.Version
	}
	opts.CheckRuntimeAuth = func(_ context.Context, rt runtime.Runtime, _ runtime.Profile) error {
		return rt.(runtimetest.Stub).AuthErr
	}

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "runtime", "myruntime")
	if row == nil {
		t.Fatal("no runtime/myruntime row")
	}
	if row.Status != MatrixWarn {
		t.Errorf("status = %q, want warn", row.Status)
	}
	if row.Suggestion == "" {
		t.Error("suggestion must not be empty when auth fails")
	}
}

func TestMatrixRuntimeFullyConfigured(t *testing.T) {
	root := fabricateMatrixProject(t, nil)

	reg := runtime.NewRegistry()
	stub := runtimetest.Stub{
		StubName:     "myruntime",
		StubProvider: "anthropic",
		Present:      true,
		Version:      "2.0.0",
		AuthErr:      nil,
	}
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}

	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })
	opts.Runtimes = reg
	opts.DetectRuntime = func(_ context.Context, rt runtime.Runtime) (bool, string) {
		s := rt.(runtimetest.Stub)
		return s.Present, s.Version
	}
	opts.CheckRuntimeAuth = func(_ context.Context, rt runtime.Runtime, _ runtime.Profile) error {
		return rt.(runtimetest.Stub).AuthErr
	}

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "runtime", "myruntime")
	if row == nil {
		t.Fatal("no runtime/myruntime row")
	}
	if row.Status != MatrixOK {
		t.Errorf("status = %q, want ok (detail: %s)", row.Status, row.Detail)
	}
	if row.Suggestion != "" {
		t.Errorf("suggestion should be empty for ok runtime, got %q", row.Suggestion)
	}
}

// --- vault -------------------------------------------------------------------

func TestMatrixVaultBinaryMissing(t *testing.T) {
	root := fabricateMatrixProject(t, nil)

	vc := &signing.VaultConfig{
		Providers: map[string]signing.ProviderTemplates{
			"protonpass": {Fetch: []string{"pass-cli", "item", "view"}},
		},
	}
	lookPath := func(name string) (string, error) {
		return "", errors.New("not found")
	}
	opts := baseMatrixOpts(lookPath, vc, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "vault", "protonpass")
	if row == nil {
		t.Fatal("no vault/protonpass row")
	}
	if row.Status != MatrixMissing {
		t.Errorf("status = %q, want missing", row.Status)
	}
	if row.Suggestion == "" {
		t.Error("suggestion must not be empty when binary missing")
	}
}

func TestMatrixVaultBinaryFound(t *testing.T) {
	root := fabricateMatrixProject(t, nil)

	vc := &signing.VaultConfig{
		Providers: map[string]signing.ProviderTemplates{
			"protonpass": {Fetch: []string{"pass-cli", "item", "view"}},
		},
	}
	lookPath := func(name string) (string, error) {
		if name == "pass-cli" {
			return "/usr/local/bin/pass-cli", nil
		}
		return "", errors.New("not found")
	}
	opts := baseMatrixOpts(lookPath, vc, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "vault", "protonpass")
	if row == nil {
		t.Fatal("no vault/protonpass row")
	}
	if row.Status != MatrixOK {
		t.Errorf("status = %q, want ok", row.Status)
	}
	if row.Suggestion != "" {
		t.Errorf("suggestion should be empty for ok vault, got %q", row.Suggestion)
	}
}

func TestMatrixVaultNoFetchTemplate(t *testing.T) {
	// The "command" provider has no fetch template — it should be MatrixOK.
	root := fabricateMatrixProject(t, nil)

	vc := &signing.VaultConfig{
		Providers: map[string]signing.ProviderTemplates{
			"command": {},
		},
	}
	opts := baseMatrixOpts(
		func(string) (string, error) { return "", errors.New("not found") },
		vc,
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	)

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "vault", "command")
	if row == nil {
		t.Fatal("no vault/command row")
	}
	if row.Status != MatrixOK {
		t.Errorf("status = %q, want ok (no binary required for command provider)", row.Status)
	}
}

// --- docs --------------------------------------------------------------------

func TestMatrixDocsMissing(t *testing.T) {
	root := fabricateMatrixProject(t, nil)
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "docs", "docs")
	if row == nil {
		t.Fatal("no docs row")
	}
	if row.Status != MatrixMissing {
		t.Errorf("status = %q, want missing", row.Status)
	}
	if row.Suggestion == "" {
		t.Error("suggestion must not be empty for missing docs")
	}
}

func TestMatrixDocsMkdocsPresent(t *testing.T) {
	root := fabricateMatrixProject(t, nil)
	opts := baseMatrixOpts(nil, nil, func(path string) (os.FileInfo, error) {
		if filepath.Base(path) == "mkdocs.yml" {
			// Return a fake FileInfo (only non-nil matters here).
			return &fakeFileInfo{name: "mkdocs.yml"}, nil
		}
		return nil, os.ErrNotExist
	})

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "docs", "mkdocs")
	if row == nil {
		t.Fatal("no docs/mkdocs row")
	}
	if row.Status != MatrixOK {
		t.Errorf("status = %q, want ok", row.Status)
	}
	if row.Suggestion != "" {
		t.Errorf("suggestion should be empty for ok docs, got %q", row.Suggestion)
	}
}

// --- release -----------------------------------------------------------------

func TestMatrixReleaseMissing(t *testing.T) {
	root := fabricateMatrixProject(t, nil)
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "release", "release")
	if row == nil {
		t.Fatal("no release row")
	}
	if row.Status != MatrixMissing {
		t.Errorf("status = %q, want missing", row.Status)
	}
	if row.Suggestion == "" {
		t.Error("suggestion must not be empty for missing release")
	}
}

func TestMatrixReleaseReleasePleasePresent(t *testing.T) {
	root := fabricateMatrixProject(t, nil)
	opts := baseMatrixOpts(nil, nil, func(path string) (os.FileInfo, error) {
		if filepath.Base(path) == "release-please-config.json" {
			return &fakeFileInfo{name: "release-please-config.json"}, nil
		}
		return nil, os.ErrNotExist
	})

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	row := findMatrixRow(m, "release", "release-please")
	if row == nil {
		t.Fatal("no release/release-please row")
	}
	if row.Status != MatrixOK {
		t.Errorf("status = %q, want ok", row.Status)
	}
	if row.Suggestion != "" {
		t.Errorf("suggestion should be empty for ok release, got %q", row.Suggestion)
	}
}

// --- project not onboarded ---------------------------------------------------

func TestMatrixProjectNotOnboarded(t *testing.T) {
	// Directory has a .git but no koryph.project.json.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal("BuildMatrix must not return an error for missing config; got:", err)
	}
	if len(m.Rows) == 0 {
		t.Fatal("matrix must have at least one row for missing project config")
	}
	row := m.Rows[0]
	if row.Category != "project" {
		t.Errorf("first row category = %q, want project", row.Category)
	}
	if row.Status != MatrixMissing {
		t.Errorf("status = %q, want missing", row.Status)
	}
}

// --- At timestamp ------------------------------------------------------------

func TestMatrixAtTimestamp(t *testing.T) {
	root := fabricateMatrixProject(t, nil)
	want := fixedTime.UTC().Format("2006-01-02T15:04:05Z07:00")
	opts := baseMatrixOpts(nil, nil, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })

	m, err := BuildMatrix(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	if m.At != want {
		t.Errorf("At = %q, want %q", m.At, want)
	}
}

// --- fakeFileInfo for os.FileInfo injection ----------------------------------

type fakeFileInfo struct {
	name string
}

func (f *fakeFileInfo) Name() string       { return f.name }
func (f *fakeFileInfo) Size() int64        { return 0 }
func (f *fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f *fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f *fakeFileInfo) IsDir() bool        { return false }
func (f *fakeFileInfo) Sys() any           { return nil }
