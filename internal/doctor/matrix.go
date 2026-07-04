// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/signing"
)

// MatrixStatus is the readiness level of one integration slot.
type MatrixStatus string

const (
	// MatrixOK means the integration is fully configured and operational.
	MatrixOK MatrixStatus = "ok"
	// MatrixWarn means the integration is partially configured but has gaps that
	// will prevent it from working (e.g. signing configured but public_key absent).
	MatrixWarn MatrixStatus = "warn"
	// MatrixMissing means the integration is not configured at all.
	MatrixMissing MatrixStatus = "missing"
)

// IntegrationRow is one row in the integration matrix.
type IntegrationRow struct {
	// Category groups related rows: "signing", "intake", "runtime", "vault",
	// "docs", "release".
	Category string `json:"category"`
	// Name identifies the specific integration within its category, e.g. "signing",
	// "github", "jira", "linear", "claude", "codex", "protonpass", "mkdocs",
	// "release-please".
	Name string `json:"name"`
	// Status is ok / warn / missing.
	Status MatrixStatus `json:"status"`
	// Detail is the human-readable status line (config summary for ok/warn,
	// brief "not configured" for missing).
	Detail string `json:"detail"`
	// Suggestion is a one-line actionable step to close the gap; empty when
	// Status is ok.
	Suggestion string `json:"suggestion,omitempty"`
}

// Matrix is the full integration matrix for one project.
type Matrix struct {
	At   string           `json:"at"`
	Root string           `json:"root"`
	Rows []IntegrationRow `json:"rows"`
}

// MatrixOptions configures BuildMatrix. All injectable fields default to their
// production implementations when nil/zero.
type MatrixOptions struct {
	// Runtimes is the registry of runtime adapters to probe. nil = empty registry
	// (no runtime rows are emitted).
	Runtimes *runtime.Registry
	// LookPath locates a binary on PATH. nil uses exec.LookPath via signing.LoadVault's
	// provider check (we don't shell out directly for vault; LookPath is used for the
	// docs/release binary checks and exposed for test injection).
	LookPath func(name string) (string, error)
	// LoadVaultConfig loads the vault adapter config. nil uses signing.LoadVault.
	LoadVaultConfig func() (*signing.VaultConfig, error)
	// Stat checks whether a file exists. nil uses os.Stat.
	Stat func(path string) (os.FileInfo, error)
	// Now supplies the current time for the At timestamp. nil uses time.Now.
	Now func() time.Time
	// DetectRuntime calls rt.Detect; nil delegates to rt.Detect(ctx) directly.
	// Injectable so tests avoid spawning real binaries.
	DetectRuntime func(ctx context.Context, rt runtime.Runtime) (present bool, version string)
	// CheckRuntimeAuth calls rt.AuthCheck; nil delegates to rt.AuthCheck(ctx, profile).
	CheckRuntimeAuth func(ctx context.Context, rt runtime.Runtime, profile runtime.Profile) error
}

func (o *MatrixOptions) stat(path string) (os.FileInfo, error) {
	if o.Stat != nil {
		return o.Stat(path)
	}
	return os.Stat(path)
}

func (o *MatrixOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *MatrixOptions) loadVaultConfig() (*signing.VaultConfig, error) {
	if o.LoadVaultConfig != nil {
		return o.LoadVaultConfig()
	}
	return signing.LoadVault()
}

func (o *MatrixOptions) detectRuntime(ctx context.Context, rt runtime.Runtime) (bool, string) {
	if o.DetectRuntime != nil {
		return o.DetectRuntime(ctx, rt)
	}
	return rt.Detect(ctx)
}

func (o *MatrixOptions) checkRuntimeAuth(ctx context.Context, rt runtime.Runtime, profile runtime.Profile) error {
	if o.CheckRuntimeAuth != nil {
		return o.CheckRuntimeAuth(ctx, rt, profile)
	}
	return rt.AuthCheck(ctx, profile)
}

// BuildMatrix constructs the integration matrix for the project at repoRoot.
// It loads koryph.project.json and probes each integration category; a missing
// or invalid project config is reported as a single "signing" missing row
// (since all per-project categories depend on it) alongside a suggestion.
func BuildMatrix(repoRoot string, opts MatrixOptions) (*Matrix, error) {
	m := &Matrix{
		At:   opts.now().UTC().Format(time.RFC3339),
		Root: repoRoot,
	}

	cfg, err := project.Load(repoRoot)
	if err != nil {
		// Project not onboarded — add a single project-config row and return
		// a minimal matrix indicating nothing can be checked.
		m.Rows = []IntegrationRow{{
			Category:   "project",
			Name:       "config",
			Status:     MatrixMissing,
			Detail:     err.Error(),
			Suggestion: "run `koryph onboard` to initialise the project",
		}}
		return m, nil
	}

	ctx := context.Background()

	m.Rows = append(m.Rows, matrixSigning(cfg))
	m.Rows = append(m.Rows, matrixIntake(cfg)...)
	m.Rows = append(m.Rows, matrixRuntimes(ctx, &opts)...)
	m.Rows = append(m.Rows, matrixVault(&opts)...)
	m.Rows = append(m.Rows, matrixDocs(repoRoot, &opts))
	m.Rows = append(m.Rows, matrixRelease(repoRoot, &opts))

	return m, nil
}

// --- signing -----------------------------------------------------------------

func matrixSigning(cfg *project.Config) IntegrationRow {
	sc := cfg.Signing
	if sc == nil {
		return IntegrationRow{
			Category:   "signing",
			Name:       "signing",
			Status:     MatrixMissing,
			Detail:     "not configured",
			Suggestion: "run `koryph signing setup` to enable vault-backed commit signing",
		}
	}
	if sc.EffectiveMode() == signing.ModeSSH && sc.Provider != "" && sc.PublicKey == "" {
		return IntegrationRow{
			Category:   "signing",
			Name:       "signing",
			Status:     MatrixWarn,
			Detail:     "configured (mode=" + sc.EffectiveMode() + " provider=" + sc.Provider + ") but public_key not captured",
			Suggestion: "run `koryph signing setup` to complete key capture",
		}
	}
	req := "optional"
	if sc.Required {
		req = "required"
	}
	return IntegrationRow{
		Category: "signing",
		Name:     "signing",
		Status:   MatrixOK,
		Detail:   req + " mode=" + sc.EffectiveMode() + " provider=" + sc.Provider + " identity=" + sc.Identity,
	}
}

// --- intake ------------------------------------------------------------------

// intakeProviders is the ordered set of known intake providers the matrix
// always shows a row for, even when unconfigured, so gaps are visible.
var intakeProviders = []string{"github", "jira", "linear"}

func matrixIntake(cfg *project.Config) []IntegrationRow {
	// Count sources per provider.
	counts := make(map[string]int, len(intakeProviders))
	for _, src := range cfg.Intake {
		p := src.Provider
		if p == "" {
			p = "github"
		}
		counts[p]++
	}

	rows := make([]IntegrationRow, 0, len(intakeProviders))
	for _, p := range intakeProviders {
		n := counts[p]
		if n == 0 {
			rows = append(rows, IntegrationRow{
				Category:   "intake",
				Name:       p,
				Status:     MatrixMissing,
				Detail:     "no " + p + " intake source configured",
				Suggestion: "add an intake entry with provider=" + p + " to koryph.project.json",
			})
		} else {
			detail := itoa(n) + " source"
			if n > 1 {
				detail += "s"
			}
			rows = append(rows, IntegrationRow{
				Category: "intake",
				Name:     p,
				Status:   MatrixOK,
				Detail:   detail + " configured",
			})
		}
	}
	return rows
}

// itoa is a tiny int-to-string helper that avoids importing fmt.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// --- runtimes ----------------------------------------------------------------

func matrixRuntimes(ctx context.Context, opts *MatrixOptions) []IntegrationRow {
	if opts.Runtimes == nil {
		return nil
	}
	rts := opts.Runtimes.List()
	if len(rts) == 0 {
		return nil
	}
	rows := make([]IntegrationRow, 0, len(rts))
	for _, rt := range rts {
		rows = append(rows, matrixOneRuntime(ctx, opts, rt))
	}
	return rows
}

func matrixOneRuntime(ctx context.Context, opts *MatrixOptions, rt runtime.Runtime) IntegrationRow {
	base := IntegrationRow{
		Category: "runtime",
		Name:     rt.Name(),
	}

	present, version := opts.detectRuntime(ctx, rt)
	if !present {
		base.Status = MatrixMissing
		base.Detail = "binary not found on PATH"
		base.Suggestion = "install the " + rt.Name() + " CLI and ensure it is on PATH"
		return base
	}

	authErr := opts.checkRuntimeAuth(ctx, rt, runtime.Profile{})
	if authErr != nil {
		base.Status = MatrixWarn
		detail := "binary found"
		if version != "" {
			detail += " (" + version + ")"
		}
		detail += "; not authenticated: " + authErr.Error()
		base.Detail = detail
		base.Suggestion = "authenticate with " + rt.Name() + " (check its login command)"
		return base
	}

	base.Status = MatrixOK
	detail := "binary found"
	if version != "" {
		detail += " (" + version + ")"
	}
	detail += "; authenticated; provider=" + rt.Provider()
	base.Detail = detail
	return base
}

// --- vault -------------------------------------------------------------------

func matrixVault(opts *MatrixOptions) []IntegrationRow {
	vc, err := opts.loadVaultConfig()
	if err != nil {
		return []IntegrationRow{{
			Category:   "vault",
			Name:       "vault",
			Status:     MatrixWarn,
			Detail:     "cannot load vault config: " + err.Error(),
			Suggestion: "check ~/.koryph/vault.json for syntax errors",
		}}
	}

	// Sort provider names for deterministic output.
	names := make([]string, 0, len(vc.Providers))
	for name := range vc.Providers {
		names = append(names, name)
	}
	sortStrings(names)

	rows := make([]IntegrationRow, 0, len(names))
	for _, name := range names {
		pt := vc.Providers[name]
		rows = append(rows, matrixOneVault(opts, name, pt))
	}
	return rows
}

func matrixOneVault(opts *MatrixOptions, name string, pt signing.ProviderTemplates) IntegrationRow {
	base := IntegrationRow{
		Category: "vault",
		Name:     name,
	}
	if len(pt.Fetch) == 0 {
		// provider has no fetch template — it's the "command" or "file"
		// provider where the operator supplies the argv or path.
		base.Status = MatrixOK
		base.Detail = "no binary required (provider=" + name + ")"
		return base
	}
	bin := pt.Fetch[0]
	lp := opts.LookPath
	if lp == nil {
		// Production: delegate to os/exec.LookPath via our own import-free shim.
		lp = defaultLookPath
	}
	if _, err := lp(bin); err != nil {
		hint := ""
		if pt.LoginHint != "" {
			hint = " (login: `" + pt.LoginHint + "`)"
		}
		base.Status = MatrixMissing
		base.Detail = "binary " + bin + " not found on PATH" + hint
		base.Suggestion = "install " + bin + " and ensure it is on PATH"
		return base
	}
	base.Status = MatrixOK
	base.Detail = "binary " + bin + " found on PATH"
	return base
}

// defaultLookPath is the production PATH probe (os/exec.LookPath).
// We call it through a function variable so tests can override opts.LookPath
// without having to replace defaultLookPath at the package level.
func defaultLookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// --- docs --------------------------------------------------------------------

// docsMarkers is the ordered list of filenames that indicate a docs integration.
// We check them in order and report the first match as the docs row's Name.
var docsMarkers = []struct{ name, file, suggestion string }{
	{"mkdocs", "mkdocs.yml", "run `mkdocs new .` to set up MkDocs"},
	{"mkdocs", "mkdocs.yaml", "run `mkdocs new .` to set up MkDocs"},
}

func matrixDocs(repoRoot string, opts *MatrixOptions) IntegrationRow {
	for _, m := range docsMarkers {
		if _, err := opts.stat(filepath.Join(repoRoot, m.file)); err == nil {
			return IntegrationRow{
				Category: "docs",
				Name:     m.name,
				Status:   MatrixOK,
				Detail:   m.file + " present",
			}
		}
	}
	return IntegrationRow{
		Category:   "docs",
		Name:       "docs",
		Status:     MatrixMissing,
		Detail:     "no docs site configuration found (mkdocs.yml)",
		Suggestion: "run `mkdocs new .` to set up MkDocs or add a docs site config",
	}
}

// --- release -----------------------------------------------------------------

// releaseMarkers is the ordered list of file patterns that indicate a release
// pipeline is configured.
var releaseMarkers = []struct{ name, file string }{
	{"release-please", "release-please-config.json"},
	{"goreleaser", ".goreleaser.yml"},
	{"goreleaser", ".goreleaser.yaml"},
}

func matrixRelease(repoRoot string, opts *MatrixOptions) IntegrationRow {
	for _, m := range releaseMarkers {
		if _, err := opts.stat(filepath.Join(repoRoot, m.file)); err == nil {
			return IntegrationRow{
				Category: "release",
				Name:     m.name,
				Status:   MatrixOK,
				Detail:   m.file + " present",
			}
		}
	}
	return IntegrationRow{
		Category:   "release",
		Name:       "release",
		Status:     MatrixMissing,
		Detail:     "no release pipeline configuration found",
		Suggestion: "add release-please-config.json or .goreleaser.yml to configure automated releases",
	}
}

// --- helpers -----------------------------------------------------------------

// sortStrings sorts a string slice in-place (avoids importing sort in tests).
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}
