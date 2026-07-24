// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package modelroute

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// TestResolveRuntimeNamePrecedence exercises koryph-v8u.3's runtime:<name>
// label grammar: bead label > project default_runtime > "claude".
func TestResolveRuntimeNamePrecedence(t *testing.T) {
	cases := []struct {
		name           string
		labels         []string
		defaultRuntime string
		wantName       string
		wantRationale  string
	}{
		{
			name:          "no label, no default -> claude",
			wantName:      "claude",
			wantRationale: "default",
		},
		{
			name:           "project default_runtime used absent a label",
			defaultRuntime: "codex",
			wantName:       "codex",
			wantRationale:  "project default_runtime codex",
		},
		{
			name:           "bead label wins over project default",
			labels:         []string{"runtime:cursor"},
			defaultRuntime: "codex",
			wantName:       "cursor",
			wantRationale:  "label runtime:cursor",
		},
		{
			name:     "bead label wins with no project default",
			labels:   []string{"fp:core", "runtime:grok"},
			wantName: "grok",
		},
		{
			name:     "an empty runtime: label value is ignored",
			labels:   []string{"runtime:"},
			wantName: "claude",
		},
		{
			name:     "unrelated labels are ignored",
			labels:   []string{"fp:core", "model:sonnet"},
			wantName: "claude",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, rationale := ResolveRuntimeName(tc.labels, tc.defaultRuntime)
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if tc.wantRationale != "" && rationale != tc.wantRationale {
				t.Errorf("rationale = %q, want %q", rationale, tc.wantRationale)
			}
		})
	}
}

func TestResolveCodexExactAndPortableEquivalency(t *testing.T) {
	name, rationale := ResolveRuntimeName([]string{"model:gpt-5.6-terra"}, "claude")
	if name != "codex" || !strings.Contains(rationale, "implies runtime codex") {
		t.Fatalf("ResolveRuntimeName(model:gpt-5.6-terra) = (%q, %q), want codex inference", name, rationale)
	}
	name, rationale = ResolveRuntimeName([]string{"model:" + runtime.CodexSolModel}, "claude")
	if name != "codex" || !strings.Contains(rationale, "implies runtime codex") {
		t.Fatalf("ResolveRuntimeName(model:%s) = (%q, %q), want codex inference", runtime.CodexSolModel, name, rationale)
	}

	got, err := Resolve(Req{
		Runtime: "codex", Stage: StageImplement,
		Labels: []string{"equiv:frontier:xhigh"},
		// This is the legacy default stored by projects created before
		// runtime-specific mappings. It must not reject a known Codex model.
		AllowedModels: []string{TierHaiku, TierSonnet, TierOpus},
	})
	if err != nil {
		t.Fatalf("Resolve(Codex equivalency): %v", err)
	}
	if got.Model != "gpt-5.6-terra" || got.Effort != "xhigh" || got.Equivalent != "frontier:xhigh" {
		t.Errorf("Codex equivalency = %+v, want model gpt-5.6-terra, effort xhigh, portable frontier:xhigh", got)
	}
}

func TestResolveEquivalentTranslatesNativeAndStageSelections(t *testing.T) {
	t.Run("unique Claude native model and effort map to Codex", func(t *testing.T) {
		got, err := ResolveEquivalent(
			Req{Runtime: "claude", Stage: StageImplement, Labels: []string{"model:opus", "effort:xhigh"}},
			Req{Runtime: "codex", Stage: StageImplement},
		)
		if err != nil {
			t.Fatalf("ResolveEquivalent: %v", err)
		}
		if got.Model != "gpt-5.6-terra" || got.Effort != "xhigh" || got.Equivalent != "frontier:xhigh" {
			t.Errorf("resolution = %+v, want Codex Terra/xhigh equivalent", got)
		}
		if !strings.Contains(got.Rationale, "runtime equivalent codex") {
			t.Errorf("rationale = %q, want runtime-equivalent provenance", got.Rationale)
		}
	})

	t.Run("non-explicit Codex stage default carries stage capability", func(t *testing.T) {
		got, err := ResolveEquivalent(
			Req{Runtime: "codex", Stage: StageImplement},
			Req{Runtime: "claude", Stage: StageImplement},
		)
		if err != nil {
			t.Fatalf("ResolveEquivalent: %v", err)
		}
		if got.Model != TierSonnet {
			t.Errorf("Model = %q, want %q from implement's standard capability", got.Model, TierSonnet)
		}
	})

	t.Run("ambiguous exact Codex model fails closed", func(t *testing.T) {
		_, err := ResolveEquivalent(
			Req{Runtime: "codex", Stage: StageImplement, Labels: []string{"model:gpt-5.6-terra"}},
			Req{Runtime: "claude", Stage: StageImplement},
		)
		if err == nil || !strings.Contains(err.Error(), "cannot translate exact model") || !strings.Contains(err.Error(), "equiv:") {
			t.Fatalf("ResolveEquivalent error = %v, want exact-model equiv remediation", err)
		}
	})
}

func TestCodexAdvancedPlanningUsesSolUnlessOverridden(t *testing.T) {
	got, err := Resolve(Req{Runtime: "codex", Stage: StagePlan})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != runtime.CodexSolModel {
		t.Errorf("plan model = %q, want %q", got.Model, runtime.CodexSolModel)
	}

	got, err = Resolve(Req{Runtime: "codex", Stage: StagePlan, Labels: []string{"equiv:frontier:xhigh"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-5.6-terra" {
		t.Errorf("explicit plan equivalency model = %q, want Terra", got.Model)
	}
}

func TestRunEquivalentIsProjectDefaultAndLosesToBeadModel(t *testing.T) {
	got, err := Resolve(Req{Runtime: "codex", Stage: StageImplement, RunEquivalent: "standard:xhigh"})
	if err != nil {
		t.Fatalf("Resolve(project equivalency): %v", err)
	}
	if got.Model != "gpt-5.6-terra" || got.Effort != "xhigh" || got.Rationale != "project default equivalent standard:xhigh" {
		t.Errorf("project equivalent = %+v, want Codex standard/xhigh default", got)
	}

	got, err = Resolve(Req{Runtime: "claude", Stage: StageImplement, Labels: []string{"model:opus"}, RunEquivalent: "standard:xhigh"})
	if err != nil {
		t.Fatalf("Resolve(bead model over project equivalent): %v", err)
	}
	if got.Model != TierOpus || got.Rationale != "label model:opus" {
		t.Errorf("bead label = %+v, want opus label selection", got)
	}
}

// TestResolveClaudeRuntimeParity proves an explicit Runtime: "claude" resolves
// byte-for-byte identically to leaving Runtime unset — the compatibility
// contract every pre-koryph-v8u.3 caller depends on.
func TestResolveClaudeRuntimeParity(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "koryph-implementer", `---
name: koryph-implementer
tier: light
---
`)
	base := Req{Stage: StageImplement, RepoRoot: root}
	withClaude := base
	withClaude.Runtime = "claude"

	got1, err1 := Resolve(base)
	if err1 != nil {
		t.Fatalf("Resolve(base) error = %v", err1)
	}
	got2, err2 := Resolve(withClaude)
	if err2 != nil {
		t.Fatalf("Resolve(withClaude) error = %v", err2)
	}
	if got1 != got2 {
		t.Errorf("Resolve(Runtime unset) = %+v, Resolve(Runtime=claude) = %+v; want identical", got1, got2)
	}
}

// TestResolveStubRuntimeModelMapRouting proves the persona-tier step
// (koryph-v8u.10, runtime-namespaced by koryph-v8u.3) resolves through the
// SELECTED runtime's own ModelMap, not claude's — the mechanism a future
// non-claude runtime adapter plugs into. The stub is registered into the
// real process-wide runtime.Default registry under a name unique to this
// test (Registry.Register is append-only with no Unregister, see its doc,
// so a shared/reused name would collide across a re-run within the same test
// binary).
//
// The stub deliberately maps "light" to "opus" — the OPPOSITE of claude's own
// tier light -> haiku (runtime.ClaudeModelMap) — so a pass here can only mean
// the stub's own map was consulted, not claude's. (The tier vocabulary's
// final validity check, validTier/AllowedModels, stays claude-shaped per this
// bead's scope — "Fable allowlist guard stays claude-side" — so the stub's
// mapped value must itself be one of haiku/sonnet/opus/fable; a real
// non-claude adapter's own concrete model ids are out of scope here.)
func TestResolveStubRuntimeModelMapRouting(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "koryph-implementer", `---
name: koryph-implementer
tier: light
---
`)

	const name = "modelroute-test-stub-modelmap"
	stub := runtimetest.Stub{StubName: name, Models: runtime.ModelMap{runtime.TierLight: TierOpus}}
	if err := runtime.Default.Register(stub); err != nil {
		t.Fatalf("Register(%s): %v", name, err)
	}

	got, err := Resolve(Req{
		Stage:    StageImplement,
		RepoRoot: root,
		Runtime:  name,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Model != TierOpus {
		t.Errorf("Model = %q, want opus (via the stub runtime's own ModelMap, not claude's haiku)", got.Model)
	}
	if !strings.Contains(got.Rationale, "tier light -> opus") {
		t.Errorf("Rationale = %q, want it to name the stub-mapped model", got.Rationale)
	}
}

// TestResolveUnknownRuntimeFailsClosed proves an unregistered runtime name
// errors at Resolve regardless of which precedence step would otherwise have
// won (koryph-v8u.3: never silently substitute claude).
func TestResolveUnknownRuntimeFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		req  Req
	}{
		{
			name: "unknown runtime with an explicit --model set",
			req:  Req{Stage: StageImplement, ExplicitModel: TierOpus, Runtime: "totally-unregistered-runtime"},
		},
		{
			name: "unknown runtime falling through to stage default",
			req:  Req{Stage: StageImplement, Runtime: "totally-unregistered-runtime"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(tc.req)
			if err == nil {
				t.Fatalf("expected an error for an unregistered runtime, got nil")
			}
			if !strings.Contains(err.Error(), "unknown runtime") {
				t.Errorf("error = %q, want it to name the unknown runtime", err.Error())
			}
		})
	}
}
