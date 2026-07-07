// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package project

import (
	"reflect"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
	"github.com/koryph/koryph/internal/signing"
)

// fullConfig returns a Config with EVERY field populated to a distinct,
// non-zero, VALID value. It is the single source of truth for the round-trip
// guard: if a new field is added to Config (or signing.Config) it must be set
// here or TestConfig_AllFieldsPopulated fails, which in turn guarantees the
// new field is exercised by TestConfig_RoundTripPreservesEveryField.
func fullConfig() *Config {
	return &Config{
		SchemaVersion: 1,
		ProjectID:     "proj-x",
		// markdown work source populates PlansDir too and stays Validate-legal.
		WorkSource: "markdown",
		PlansDir:   "docs/plans",
		Footprint: []FootprintRule{
			{Pattern: "src/**", Tokens: []string{"HOT:core"}},
		},
		AreaMap:  map[string][]string{"cli": {"go:cli"}},
		Gate:     []string{"make lint", "make test"},
		Stages:   map[string]string{"implement": "implementer"},
		Tiers:    map[string]string{"fast": "haiku"},
		ModelMap: map[string]string{"frontier": "fable"},
		Pipeline: []PipelineStage{
			{Name: "docs", Persona: "feature-docs-author", Model: "sonnet", Effort: "high", Prompt: "update docs", Optional: true},
		},
		Bootstrap: []string{"go mod download"},
		Intake: []IntakeSource{{
			Provider:    "github",
			Source:      "acme/widgets",
			Trigger:     "triage",
			Limit:       20,
			CommentBack: true,
			Mapping:     map[string]string{"priority": "labels"},
		}},
		ProtectedPaths: []string{"hooks/"},
		Validation:     []string{"make validate"},
		EngineVersion:  "0.2+",
		// custom style forces CommitTemplate to be populated and legal.
		CommitStyle:     "custom",
		CommitTemplate:  "{type}: {subject}",
		MergePolicy:     "auto",
		MergeMethod:     "squash",
		RiskTierDefault: 3,
		Vault: &signing.VaultDefaults{
			Provider:  signing.ProviderOnePassword,
			Container: "Engineering",
		},
		Signing: &signing.Config{
			Required:  true,
			Mode:      signing.ModeSSH,
			Provider:  signing.ProviderOnePassword,
			KeyRef:    "op://vault/item/key",
			Identity:  "signer@example.com",
			PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample",
			VaultName: "Example Vault",
			ItemTitle: "Signing Key",
			Artifacts: true,
		},
		RequireCalibration:     true,
		MaxConcurrentSlots:     4,
		DispatchStaggerSeconds: 6,
		PollSeconds:            5,
		HealthIntervalSeconds:  120,
		DispatchMode:           "rolling",
		DefaultRuntime:         "claude",
		Runtimes: map[string]RuntimeConfig{
			"claude": {Enabled: true, ModelMap: map[string]string{"frontier": "fable"}},
		},
		Release: &ReleaseConfig{
			Type:         "go",
			ExtraFiles:   []string{"internal/version/version.go"},
			ArtifactsDir: "dist",
			Build: ReleaseBuildConfig{
				Goreleaser: &GoreleaserBuild{Version: "~> v2.16"},
			},
			SBOM:       true,
			Provenance: true,
		},
		Copyright: &CopyrightConfig{
			Holder:  "Acme, Inc.",
			Year:    "2024-2026",
			License: "MIT",
		},
		Posture: &PostureConfig{
			Profile:    "oss-solo-maintainer",
			Parameters: map[string]string{"required_checks": "pre-commit,make gate"},
			Fragments:  []string{"gitleaks", "govulncheck"},
			Org:        "acme-org",
		},
		Forge: "github",
		EpicValidation: &EpicValidationConfig{
			Enabled:          boolPtr(true),
			Model:            "opus",
			Persona:          "koryph-epic-validator",
			Effort:           "xhigh",
			MaxRounds:        3,
			AutoClose:        boolPtr(true),
			TimeoutSeconds:   300,
			StructuralParent: "koryph-qta",
			DocsUpdate: &EpicDocsUpdateConfig{
				Enabled: boolPtr(true),
				Labels:  []string{"area:docs"},
			},
		},
	}
}

// boolPtr returns a pointer to the given bool value for use in struct literals
// where *bool fields must be explicitly set.
func boolPtr(b bool) *bool { return &b }

// TestEpicDocsUpdate_Defaults locks the documented docs_update defaults: an
// absent sub-block (and an absent parent block) resolves to enabled with
// labels ["area:docs"] (design 4b/5).
func TestEpicDocsUpdate_Defaults(t *testing.T) {
	var nilCfg *EpicValidationConfig
	d := nilCfg.EffectiveDocsUpdate()
	if d.Enabled == nil || !*d.Enabled {
		t.Error("absent docs_update must default to enabled")
	}
	if len(d.Labels) != 1 || d.Labels[0] != "area:docs" {
		t.Errorf("default labels = %v, want [area:docs]", d.Labels)
	}

	off := false
	c := &EpicValidationConfig{DocsUpdate: &EpicDocsUpdateConfig{Enabled: &off, Labels: []string{"x"}}}
	d = c.EffectiveDocsUpdate()
	if *d.Enabled || len(d.Labels) != 1 || d.Labels[0] != "x" {
		t.Errorf("explicit docs_update not round-tripped: %+v", d)
	}
}

// TestConfig_AllFieldsPopulated fails loudly if fullConfig leaves any field at
// its zero value. This is the coverage forcing-function: adding a field to
// Config without populating it here trips this test, which is the signal to
// extend fullConfig — and thereby the round-trip test — for the new field.
//
// The skip map exempts inactive branches of union types that are intentionally
// zero because their complementary field is active. Each exempted path must
// have a companion *Validation test exercising the inactive branch.
func TestConfig_AllFieldsPopulated(t *testing.T) {
	// ReleaseBuildConfig is a union: fullConfig uses Goreleaser (mode A), so
	// Commands (mode B) is intentionally empty.
	// TestReleaseConfig_Validation exercises the Commands branch.
	skip := map[string]bool{
		"Config.Release.Build.Commands": true,
	}
	assertNoZeroFields(t, reflect.ValueOf(fullConfig()), "Config", skip)
}

// TestConfig_RoundTripPreservesEveryField is the direct regression guard for
// koryph-du2 (engine_version silently dropped by a signing-setup
// Load->mutate->Save cycle). It writes a fully-populated config, reads it back,
// and asserts deep equality — so no field, present or future, can be dropped by
// the JSON round-trip without a failing test.
func TestConfig_RoundTripPreservesEveryField(t *testing.T) {
	dir := t.TempDir()
	orig := fullConfig()
	if err := orig.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(orig, got) {
		t.Errorf("round-trip mutated config\n orig: %+v\n  got: %+v", orig, got)
	}
	// Belt-and-suspenders: the exact field the bug dropped.
	if got.EngineVersion != orig.EngineVersion {
		t.Errorf("engine_version dropped: want %q got %q", orig.EngineVersion, got.EngineVersion)
	}
}

// TestConfig_PipelineValidation covers the post-implement pipeline rules:
// stages need names, names are unique, and the engine-managed implement/review
// stages cannot be redeclared.
func TestConfig_PipelineValidation(t *testing.T) {
	base := func(p []PipelineStage) *Config {
		c := Default("proj")
		c.Pipeline = p
		return c
	}
	cases := []struct {
		name    string
		stages  []PipelineStage
		wantErr string
	}{
		{"empty is fine", nil, ""},
		{"single docs stage", []PipelineStage{{Name: "docs"}}, ""},
		{"docs then test", []PipelineStage{{Name: "docs"}, {Name: "test"}}, ""},
		{"missing name", []PipelineStage{{Persona: "x"}}, "name is required"},
		{"blank name", []PipelineStage{{Name: "  "}}, "name is required"},
		{"duplicate", []PipelineStage{{Name: "docs"}, {Name: "docs"}}, "duplicate stage"},
		{"reserved implement", []PipelineStage{{Name: "implement"}}, "engine-managed"},
		{"reserved review", []PipelineStage{{Name: "review"}}, "engine-managed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.stages).Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfig_CommitStyleValidation(t *testing.T) {
	cases := []struct {
		name     string
		style    string
		template string
		wantErr  string
	}{
		{"empty is fine", "", "", ""},
		{"conventional", "conventional", "", ""},
		{"none opts out", "none", "", ""},
		{"custom needs template", "custom", "", "requires commit_template"},
		{"custom with template", "custom", "JIRA-123: subject", ""},
		{"unknown value", "bogus", "", "commit_style must be conventional|custom|none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default("proj")
			c.CommitStyle = tc.style
			c.CommitTemplate = tc.template
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfig_MergeMethodValidation(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		wantErr string
	}{
		{"empty is fine", "", ""},
		{"ff", "ff", ""},
		{"squash", "squash", ""},
		{"unknown", "rebase", "merge_method must be ff|squash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default("proj")
			c.MergeMethod = tc.method
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestConfig_DispatchModeValidation proves the koryph-2im.3 config contract:
// dispatch_mode accepts wave|rolling (empty defaults to wave semantics at the
// engine layer, not here — Validate only rejects garbage), and anything else
// is a load-time error.
func TestConfig_DispatchModeValidation(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		wantErr string
	}{
		{"empty is fine (defaults to wave)", "", ""},
		{"wave", "wave", ""},
		{"rolling", "rolling", ""},
		{"unknown value", "continuous", "dispatch_mode must be wave|rolling"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default("proj")
			c.DispatchMode = tc.mode
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestConfig_DefaultRuntimeValidation exercises koryph-v8u.3's
// default_runtime contract: "" and "claude" are always valid without a
// registry lookup; any other name must be registered in runtime.Default or
// Validate fails closed.
func TestConfig_DefaultRuntimeValidation(t *testing.T) {
	const registeredName = "project-test-registered-runtime"
	stub := runtimetest.Stub{StubName: registeredName}
	if err := runtime.Default.Register(stub); err != nil {
		t.Fatalf("Register(%s): %v", registeredName, err)
	}

	cases := []struct {
		name    string
		runtime string
		wantErr string
	}{
		{"empty defaults to claude", "", ""},
		{"claude is always valid", "claude", ""},
		{"a registered runtime is valid", registeredName, ""},
		{"an unregistered runtime fails closed", "totally-unregistered-runtime", "default_runtime"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default("proj")
			c.DefaultRuntime = tc.runtime
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestReleaseConfig_Validation covers the release block contract:
// type required, exactly one build mode, both modes independently valid.
func TestReleaseConfig_Validation(t *testing.T) {
	base := func(r *ReleaseConfig) *Config {
		c := Default("proj")
		c.Release = r
		return c
	}
	goreleaser := &GoreleaserBuild{Version: "~> v2.16"}
	cases := []struct {
		name    string
		rel     *ReleaseConfig
		wantErr string
	}{
		{"nil release block is fine", nil, ""},
		{"goreleaser mode (mode A) valid", &ReleaseConfig{Type: "go", Build: ReleaseBuildConfig{Goreleaser: goreleaser}}, ""},
		{"commands mode (mode B) valid", &ReleaseConfig{Type: "go", Build: ReleaseBuildConfig{Commands: []string{"make build"}}}, ""},
		{"both modes set is rejected", &ReleaseConfig{Type: "go", Build: ReleaseBuildConfig{Goreleaser: goreleaser, Commands: []string{"make build"}}}, "only one build mode"},
		{"no mode set is rejected", &ReleaseConfig{Type: "go", Build: ReleaseBuildConfig{}}, "exactly one build mode is required"},
		{"missing type is rejected", &ReleaseConfig{Build: ReleaseBuildConfig{Goreleaser: goreleaser}}, "release.type is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.rel).Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfig_LandMethod(t *testing.T) {
	for method, want := range map[string]string{"": "ff", "ff": "ff", "squash": "squash"} {
		if got := (&Config{MergeMethod: method}).LandMethod(); got != want {
			t.Errorf("LandMethod(merge_method=%q) = %q, want %q", method, got, want)
		}
	}
}

func TestConfig_LandMethodError(t *testing.T) {
	signed := func() *Config {
		return &Config{Signing: &signing.Config{Required: true, Identity: "x@example.com"}}
	}
	cases := []struct {
		name    string
		cfg     *Config
		method  string
		wantErr string
	}{
		{"ff always ok", &Config{}, "ff", ""},
		{"squash ok without signing", &Config{}, "squash", ""},
		{"default ff ok when signing required", signed(), "", ""},
		{"squash refused when signing required", signed(), "squash", "signing.required"},
		{"unknown method", &Config{}, "rebase", "unknown merge_method"},
		{"config default squash refused when signing required", func() *Config {
			c := signed()
			c.MergeMethod = "squash"
			return c
		}(), "", "signing.required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.LandMethodError(tc.method)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("LandMethodError() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("LandMethodError() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestConfig_VaultValidation verifies that an invalid vault.provider is caught
// at Validate time and that a missing vault block is always valid.
func TestConfig_VaultValidation(t *testing.T) {
	cases := []struct {
		name    string
		vault   *signing.VaultDefaults
		wantErr string
	}{
		{"nil vault block is fine", nil, ""},
		{"valid provider", &signing.VaultDefaults{Provider: signing.ProviderProtonPass, Container: "Eng"}, ""},
		{"empty provider is fine", &signing.VaultDefaults{Container: "Eng"}, ""},
		{"unknown provider is rejected", &signing.VaultDefaults{Provider: "no-such"}, "vault.provider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default("proj")
			c.Vault = tc.vault
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfig_ForgeValidation(t *testing.T) {
	valid := []string{"", "github", "gitlab"}
	for _, v := range valid {
		c := Default("proj")
		c.Forge = v
		if err := c.Validate(); err != nil {
			t.Errorf("Forge=%q: unexpected error: %v", v, err)
		}
	}
	invalid := []string{"bitbucket", "gitea", "github.com", "GITHUB"}
	for _, v := range invalid {
		c := Default("proj")
		c.Forge = v
		if err := c.Validate(); err == nil {
			t.Errorf("Forge=%q: expected validation error, got nil", v)
		}
	}
}

func TestConfig_ResolvedForge(t *testing.T) {
	cases := []struct{ forge, want string }{
		{"", "github"},
		{"github", "github"},
		{"gitlab", "gitlab"},
	}
	for _, tc := range cases {
		c := &Config{Forge: tc.forge}
		if got := c.ResolvedForge(); got != tc.want {
			t.Errorf("ResolvedForge(forge=%q) = %q, want %q", tc.forge, got, tc.want)
		}
	}
}

func TestConfig_EnforceConventional(t *testing.T) {
	cases := map[string]bool{
		"":             true,
		"conventional": true,
		"none":         false,
		"custom":       false,
	}
	for style, want := range cases {
		c := &Config{CommitStyle: style}
		if got := c.EnforceConventional(); got != want {
			t.Errorf("EnforceConventional(commit_style=%q) = %v, want %v", style, got, want)
		}
	}
}

// assertNoZeroFields recursively asserts that v (and every field it transitively
// contains) is non-zero: nil pointers, empty strings/slices/maps, zero numbers
// and false bools all fail. It descends into structs, pointer-to-struct, and
// struct elements of slices/maps so nested config (signing.Config, FootprintRule)
// is covered too.
//
// skip is a set of dot-separated field paths (e.g. "Config.Release.Build.Commands")
// to exempt from the zero-value check. Use this ONLY for inactive branches of
// union types — fields that are intentionally zero because their complementary
// field in the same union is active. Every exemption must have a companion
// TestConfig_*Validation test that exercises the inactive branch.
func assertNoZeroFields(t *testing.T, v reflect.Value, path string, skip map[string]bool) {
	t.Helper()
	if skip[path] {
		return
	}
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			t.Errorf("%s: nil pointer (field left unset)", path)
			return
		}
		assertNoZeroFields(t, v.Elem(), path, skip)
	case reflect.Struct:
		typ := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := typ.Field(i)
			if f.PkgPath != "" {
				continue // unexported
			}
			assertNoZeroFields(t, v.Field(i), path+"."+f.Name, skip)
		}
	case reflect.Slice, reflect.Map:
		if v.Len() == 0 {
			t.Errorf("%s: empty %s (field left unset)", path, v.Kind())
			return
		}
		if v.Kind() == reflect.Slice {
			// Descend into struct elements (e.g. FootprintRule) to keep their
			// sub-fields covered.
			if elem := v.Index(0); elem.Kind() == reflect.Struct {
				assertNoZeroFields(t, elem, path+"[0]", skip)
			}
		}
	default:
		if v.IsZero() {
			t.Errorf("%s: zero value (field left unset)", path)
		}
	}
}

// TestEpicValidationConfig_Defaults asserts that an absent (nil) block —
// as well as a zero-value struct — yields the documented defaults via Effective().
func TestEpicValidationConfig_Defaults(t *testing.T) {
	// Absent block (nil pointer on Config) → all defaults.
	var nilPtr *EpicValidationConfig
	got := nilPtr.Effective()

	if got.Enabled == nil || !*got.Enabled {
		t.Errorf("Effective().Enabled: want true, got %v", got.Enabled)
	}
	if got.AutoClose == nil || !*got.AutoClose {
		t.Errorf("Effective().AutoClose: want true, got %v", got.AutoClose)
	}
	if got.Model != defaultEpicValidationModel {
		t.Errorf("Effective().Model: want %q, got %q", defaultEpicValidationModel, got.Model)
	}
	if got.Persona != defaultEpicValidationPersona {
		t.Errorf("Effective().Persona: want %q, got %q", defaultEpicValidationPersona, got.Persona)
	}
	if got.MaxRounds != defaultEpicValidationMaxRounds {
		t.Errorf("Effective().MaxRounds: want %d, got %d", defaultEpicValidationMaxRounds, got.MaxRounds)
	}
	if got.TimeoutSeconds != defaultEpicValidationTimeout {
		t.Errorf("Effective().TimeoutSeconds: want %d, got %d", defaultEpicValidationTimeout, got.TimeoutSeconds)
	}
	if got.StructuralParent != "" {
		t.Errorf("Effective().StructuralParent: want empty, got %q", got.StructuralParent)
	}

	// Zero-value struct (non-nil pointer, all fields zero) also yields defaults.
	zeroVal := &EpicValidationConfig{}
	got2 := zeroVal.Effective()
	if got2.MaxRounds != defaultEpicValidationMaxRounds {
		t.Errorf("zero-value Effective().MaxRounds: want %d, got %d", defaultEpicValidationMaxRounds, got2.MaxRounds)
	}
	if got2.Model != defaultEpicValidationModel {
		t.Errorf("zero-value Effective().Model: want %q, got %q", defaultEpicValidationModel, got2.Model)
	}
}

// TestEpicValidationConfig_RoundTrip asserts that an explicit EpicValidationConfig
// block survives a JSON save/load cycle without losing any field.
func TestEpicValidationConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := Default("proj")
	c.EpicValidation = &EpicValidationConfig{
		Enabled:          boolPtr(false),
		Model:            "fable",
		Persona:          "custom-epic-validator",
		MaxRounds:        5,
		AutoClose:        boolPtr(false),
		TimeoutSeconds:   600,
		StructuralParent: "koryph-abc",
	}
	if err := c.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ev := got.EpicValidation
	if ev == nil {
		t.Fatal("EpicValidation: nil after round-trip, want non-nil")
	}
	if ev.Enabled == nil || *ev.Enabled != false {
		t.Errorf("Enabled: want false, got %v", ev.Enabled)
	}
	if ev.Model != "fable" {
		t.Errorf("Model: want %q, got %q", "fable", ev.Model)
	}
	if ev.Persona != "custom-epic-validator" {
		t.Errorf("Persona: want %q, got %q", "custom-epic-validator", ev.Persona)
	}
	if ev.MaxRounds != 5 {
		t.Errorf("MaxRounds: want 5, got %d", ev.MaxRounds)
	}
	if ev.AutoClose == nil || *ev.AutoClose != false {
		t.Errorf("AutoClose: want false, got %v", ev.AutoClose)
	}
	if ev.TimeoutSeconds != 600 {
		t.Errorf("TimeoutSeconds: want 600, got %d", ev.TimeoutSeconds)
	}
	if ev.StructuralParent != "koryph-abc" {
		t.Errorf("StructuralParent: want %q, got %q", "koryph-abc", ev.StructuralParent)
	}
}

// TestEpicValidationConfig_Validation covers the epic_validation block contract:
// max_rounds must be >= 1 when explicitly set (non-zero), negative values are
// rejected; absent block and zero max_rounds are always valid.
func TestEpicValidationConfig_Validation(t *testing.T) {
	base := func(ev *EpicValidationConfig) *Config {
		c := Default("proj")
		c.EpicValidation = ev
		return c
	}
	cases := []struct {
		name    string
		ev      *EpicValidationConfig
		wantErr string
	}{
		{"nil block is fine", nil, ""},
		{"zero-value block is fine", &EpicValidationConfig{}, ""},
		{"max_rounds 1 is valid", &EpicValidationConfig{MaxRounds: 1}, ""},
		{"max_rounds 2 (default) explicit is valid", &EpicValidationConfig{MaxRounds: 2}, ""},
		{"max_rounds 10 is valid", &EpicValidationConfig{MaxRounds: 10}, ""},
		{"max_rounds -1 is rejected", &EpicValidationConfig{MaxRounds: -1}, "max_rounds must be >= 1"},
		{"max_rounds -100 is rejected", &EpicValidationConfig{MaxRounds: -100}, "max_rounds must be >= 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.ev).Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestConfig_EffectiveEpicValidation checks that the Config-level accessor
// delegates correctly and handles both nil and non-nil EpicValidation.
func TestConfig_EffectiveEpicValidation(t *testing.T) {
	// Nil block on config → all defaults.
	c := Default("proj")
	ev := c.EffectiveEpicValidation()
	if ev.MaxRounds != defaultEpicValidationMaxRounds {
		t.Errorf("nil block: MaxRounds want %d, got %d", defaultEpicValidationMaxRounds, ev.MaxRounds)
	}
	if ev.Model != defaultEpicValidationModel {
		t.Errorf("nil block: Model want %q, got %q", defaultEpicValidationModel, ev.Model)
	}

	// Explicit block with partial fields → non-zero fields respected, zeros defaulted.
	c.EpicValidation = &EpicValidationConfig{Model: "sonnet", MaxRounds: 3}
	ev2 := c.EffectiveEpicValidation()
	if ev2.Model != "sonnet" {
		t.Errorf("partial block: Model want %q, got %q", "sonnet", ev2.Model)
	}
	if ev2.MaxRounds != 3 {
		t.Errorf("partial block: MaxRounds want 3, got %d", ev2.MaxRounds)
	}
	if ev2.Persona != defaultEpicValidationPersona {
		t.Errorf("partial block: Persona want %q, got %q", defaultEpicValidationPersona, ev2.Persona)
	}
	if ev2.Enabled == nil || !*ev2.Enabled {
		t.Errorf("partial block: Enabled want true, got %v", ev2.Enabled)
	}
}
