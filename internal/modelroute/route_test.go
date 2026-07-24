// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package modelroute

import (
	"strings"
	"testing"
)

// fableAllowed is the allowlist a fable-enabled project would declare.
var fableAllowed = []string{TierHaiku, TierSonnet, TierOpus, TierFable}

func TestResolvePrecedence(t *testing.T) {
	cases := []struct {
		name          string
		req           Req
		wantModel     string
		wantRationale string
	}{
		{
			name: "explicit --model beats everything",
			req: Req{
				Stage:         StageImplement,
				Labels:        []string{"model:implement:haiku", "model:sonnet"},
				RunDefault:    TierHaiku,
				ExplicitModel: TierOpus,
			},
			wantModel:     TierOpus,
			wantRationale: "explicit --model",
		},
		{
			name: "stage label beats plain label and run default",
			req: Req{
				Stage:      StageImplement,
				Labels:     []string{"model:implement:haiku", "model:sonnet"},
				RunDefault: TierOpus,
			},
			wantModel:     TierHaiku,
			wantRationale: "label model:implement:haiku",
		},
		{
			name: "plain label beats run default",
			req: Req{
				Stage:      StageImplement,
				Labels:     []string{"model:sonnet"},
				RunDefault: TierOpus,
			},
			wantModel:     TierSonnet,
			wantRationale: "label model:sonnet",
		},
		{
			name: "plain label ignores a differently-scoped stage label",
			req: Req{
				Stage:  StageImplement,
				Labels: []string{"model:plan:opus", "model:haiku"},
			},
			wantModel:     TierHaiku,
			wantRationale: "label model:haiku",
		},
		{
			name:          "run default beats stage default",
			req:           Req{Stage: StageImplement, RunDefault: TierOpus},
			wantModel:     TierOpus,
			wantRationale: "run default --default-model",
		},
		{
			name:          "stage default plan -> opus",
			req:           Req{Stage: StagePlan},
			wantModel:     TierOpus,
			wantRationale: "stage default (plan)",
		},
		{
			name:          "stage default design -> opus",
			req:           Req{Stage: StageDesign},
			wantModel:     TierOpus,
			wantRationale: "stage default (design)",
		},
		{
			name:          "stage default score -> opus",
			req:           Req{Stage: StageScore},
			wantModel:     TierOpus,
			wantRationale: "stage default (score)",
		},
		{
			name:          "stage default review -> opus",
			req:           Req{Stage: StageReview},
			wantModel:     TierOpus,
			wantRationale: "stage default (review)",
		},
		{
			name:          "stage default implement -> sonnet",
			req:           Req{Stage: StageImplement},
			wantModel:     TierSonnet,
			wantRationale: "stage default (implement)",
		},
		{
			name:          "stage default docs -> sonnet",
			req:           Req{Stage: StageDocs},
			wantModel:     TierSonnet,
			wantRationale: "stage default (docs)",
		},
		{
			name:          "stage default test -> sonnet",
			req:           Req{Stage: StageTest},
			wantModel:     TierSonnet,
			wantRationale: "stage default (test)",
		},
		{
			name:          "stage default explore -> haiku",
			req:           Req{Stage: StageExplore},
			wantModel:     TierHaiku,
			wantRationale: "stage default (explore)",
		},
		{
			name:          "stage default debug -> haiku",
			req:           Req{Stage: StageDebug},
			wantModel:     TierHaiku,
			wantRationale: "stage default (debug)",
		},
		{
			name:          "unknown stage -> sonnet",
			req:           Req{Stage: "whatever"},
			wantModel:     TierSonnet,
			wantRationale: "stage default (whatever)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.req)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if got.Rationale != tc.wantRationale {
				t.Errorf("Rationale = %q, want %q", got.Rationale, tc.wantRationale)
			}
			if got.Persona == "" {
				t.Errorf("Persona should never be empty")
			}
		})
	}
}

func TestResolveFableExplicitAllowed(t *testing.T) {
	explicitSources := []struct {
		name          string
		req           Req
		wantRationale string
	}{
		{
			name:          "explicit --model fable",
			req:           Req{Stage: StageImplement, ExplicitModel: TierFable, AllowedModels: fableAllowed},
			wantRationale: "explicit --model",
		},
		{
			name:          "stage label fable",
			req:           Req{Stage: StageImplement, Labels: []string{"model:implement:fable"}, AllowedModels: fableAllowed},
			wantRationale: "label model:implement:fable",
		},
		{
			name:          "plain label fable",
			req:           Req{Stage: StageImplement, Labels: []string{"model:fable"}, AllowedModels: fableAllowed},
			wantRationale: "label model:fable",
		},
	}
	for _, tc := range explicitSources {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.req)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.Model != TierFable {
				t.Errorf("Model = %q, want fable", got.Model)
			}
			if got.Rationale != tc.wantRationale {
				t.Errorf("Rationale = %q, want %q", got.Rationale, tc.wantRationale)
			}
		})
	}
}

func TestResolveFableRejected(t *testing.T) {
	cases := []struct {
		name string
		req  Req
	}{
		{
			name: "allowed but only via run default",
			req:  Req{Stage: StageImplement, RunDefault: TierFable, AllowedModels: fableAllowed},
		},
		{
			name: "explicit label but not allowlisted",
			req:  Req{Stage: StageImplement, Labels: []string{"model:fable"}},
		},
		{
			name: "explicit --model but not allowlisted",
			req:  Req{Stage: StageImplement, ExplicitModel: TierFable},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(tc.req)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, "Fable requires explicit per-task selection and project allowlist") {
				t.Errorf("error = %q, want the fable-policy message", msg)
			}
		})
	}
}

func TestResolveAllowlistViolation(t *testing.T) {
	// plan stage defaults to opus, which is outside the allowlist here.
	_, err := Resolve(Req{Stage: StagePlan, AllowedModels: []string{TierHaiku, TierSonnet}})
	if err == nil {
		t.Fatalf("expected fail-closed error, got nil")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error = %q, want it to name the allowlist", err.Error())
	}
}

func TestResolveUnknownTier(t *testing.T) {
	_, err := Resolve(Req{Stage: StageImplement, ExplicitModel: "gpt-4o"})
	if err == nil {
		t.Fatalf("expected error for unknown tier, got nil")
	}
	if !strings.Contains(err.Error(), "unknown model tier") {
		t.Errorf("error = %q, want unknown-tier message", err.Error())
	}
}

func TestRecoveryUpgrade(t *testing.T) {
	for _, in := range []string{TierHaiku, TierSonnet, TierOpus, TierFable, "surprise"} {
		if got := RecoveryUpgrade(in); got != TierOpus {
			t.Errorf("RecoveryUpgrade(%q) = %q, want opus", in, got)
		}
	}
}

// TestEscalationTier exercises the koryph-qf6.4 policy gate: only haiku and
// sonnet escalate, only when the target is allowlisted, and opus/fable/empty
// inputs never change (escalation must never downgrade fable).
func TestEscalationTier(t *testing.T) {
	cases := []struct {
		current string
		allowed []string
		want    string
	}{
		{TierHaiku, nil, TierOpus},
		{TierSonnet, nil, TierOpus},
		{TierSonnet, []string{TierHaiku, TierSonnet, TierOpus}, TierOpus},
		{TierSonnet, []string{TierHaiku, TierSonnet}, ""}, // opus not allowed
		{TierOpus, nil, ""},                               // already at ceiling
		{TierFable, nil, ""},                              // never downgrade fable
		{"", nil, ""},                                     // adopted/legacy slot: unknown model
		{"surprise", nil, ""},
	}
	for _, tc := range cases {
		if got := EscalationTier(tc.current, tc.allowed); got != tc.want {
			t.Errorf("EscalationTier(%q, %v) = %q, want %q", tc.current, tc.allowed, got, tc.want)
		}
	}
}

func TestRecoveryModelUsesSelectedRuntimeFrontier(t *testing.T) {
	cases := []struct {
		name        string
		current     string
		runtimeName string
		override    map[string]string
		allowed     []string
		want        string
	}{
		{"codex terra to frontier", "gpt-5.6-terra", "codex", nil, nil, "gpt-5.6-sol"},
		{"codex already frontier", "gpt-5.6-sol", "codex", nil, nil, ""},
		{"codex project frontier override", "gpt-5.6-terra", "codex", map[string]string{"frontier": "codex-top"}, []string{"codex-top"}, "codex-top"},
		{"unknown custom model is not ordered", "custom", "codex", nil, []string{"custom"}, ""},
		{"claude lower tier", TierSonnet, "claude", nil, nil, TierOpus},
		{"claude fable never downgrades", TierFable, "claude", nil, fableAllowed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecoveryModel(tc.current, tc.runtimeName, tc.override, tc.allowed); got != tc.want {
				t.Errorf("RecoveryModel(%q, %q) = %q, want %q", tc.current, tc.runtimeName, got, tc.want)
			}
		})
	}
}

// TestTierForModelID exercises the koryph-qf6.2 normalization of concrete
// model ids (modelUsage keys, version-suffixed and case-varied) to tiers.
func TestTierForModelID(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-5":          TierSonnet,
		"claude-sonnet-4-5-20250929": TierSonnet,
		"claude-haiku-4-5-20251001":  TierHaiku,
		"claude-opus-4-8":            TierOpus,
		"claude-fable-5":             TierFable,
		"Claude-Opus-4-8":            TierOpus, // case-insensitive
		"gpt-4o":                     "",       // unknown family stays raw
		"":                           "",
	}
	for id, want := range cases {
		if got := TierForModelID(id); got != want {
			t.Errorf("TierForModelID(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestResolvePersonaTierPrecedence exercises the koryph-v8u.10 persona-tier
// step: bead label still wins unchanged; a persona `tier` resolves through
// runtime.ClaudeModelMap (or a project override) when no label/run-default
// fired; a persona `model` legacy pin is the fallback when tier is absent or
// unmapped; and the hardcoded stage default is the last resort when the
// persona carries neither.
func TestResolvePersonaTierPrecedence(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "koryph-implementer", `---
name: koryph-implementer
tier: light
---
`)
	writeAgent(t, root, "legacy-pin", `---
name: legacy-pin
model: opus
---
`)
	writeAgent(t, root, "unmapped-tier", `---
name: unmapped-tier
tier: quantum
model: haiku
---
`)
	writeAgent(t, root, "bare", `---
name: bare
description: no model or tier
---
`)

	cases := []struct {
		name          string
		req           Req
		wantModel     string
		wantRationale string
	}{
		{
			name: "bead label wins over persona tier, unchanged",
			req: Req{
				Stage:    StageImplement,
				Labels:   []string{"model:opus"},
				RepoRoot: root,
			},
			wantModel:     TierOpus,
			wantRationale: "label model:opus",
		},
		{
			name: "persona tier resolves via claude default map",
			req: Req{
				Stage:    StageImplement,
				RepoRoot: root,
			},
			wantModel:     TierHaiku, // koryph-implementer's tier: light -> haiku
			wantRationale: "persona koryph-implementer tier light -> haiku",
		},
		{
			name: "project model_map override wins over the claude default",
			req: Req{
				Stage:    StageImplement,
				RepoRoot: root,
				ModelMap: map[string]string{"light": TierSonnet},
			},
			wantModel:     TierSonnet,
			wantRationale: "persona koryph-implementer tier light -> sonnet",
		},
		{
			name: "legacy model pin used when persona carries no tier",
			req: Req{
				Stage:    StageImplement,
				RepoRoot: root,
				Stages:   map[string]string{StageImplement: "legacy-pin"},
			},
			wantModel:     TierOpus,
			wantRationale: "persona legacy-pin legacy model pin",
		},
		{
			name: "legacy model pin used when the persona tier is unmapped",
			req: Req{
				Stage:    StageImplement,
				RepoRoot: root,
				Stages:   map[string]string{StageImplement: "unmapped-tier"},
			},
			wantModel:     TierHaiku,
			wantRationale: "persona unmapped-tier legacy model pin",
		},
		{
			name: "stage default when persona has neither tier nor model",
			req: Req{
				Stage:    StageImplement,
				RepoRoot: root,
				Stages:   map[string]string{StageImplement: "bare"},
			},
			wantModel:     TierSonnet,
			wantRationale: "stage default (implement)",
		},
		{
			name: "run default still beats persona tier",
			req: Req{
				Stage:      StageImplement,
				RepoRoot:   root,
				RunDefault: TierOpus,
			},
			wantModel:     TierOpus,
			wantRationale: "run default --default-model",
		},
		{
			name: "RepoRoot unset disables the persona-tier step entirely",
			req: Req{
				Stage: StageImplement,
			},
			wantModel:     TierSonnet,
			wantRationale: "stage default (implement)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.req)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if got.Rationale != tc.wantRationale {
				t.Errorf("Rationale = %q, want %q", got.Rationale, tc.wantRationale)
			}
		})
	}
}

func TestPersonaFor(t *testing.T) {
	defaults := map[string]string{
		StageImplement: "koryph-implementer",
		StagePlan:      "koryph-architect",
		StageDesign:    "koryph-architect",
		StageScore:     "koryph-plan-scorer",
		StageReview:    "koryph-security-reviewer",
		StageExplore:   "koryph-explorer",
		StageDebug:     "koryph-debugger",
		StageDocs:      "koryph-feature-docs-author",
		StageTest:      "koryph-test-engineer",
		"unknown":      "koryph-implementer",
	}
	for stage, want := range defaults {
		if got := PersonaFor(stage, nil); got != want {
			t.Errorf("PersonaFor(%q, nil) = %q, want %q", stage, got, want)
		}
	}

	// Project map wins over the engine default.
	override := map[string]string{StageImplement: "custom-impl"}
	if got := PersonaFor(StageImplement, override); got != "custom-impl" {
		t.Errorf("PersonaFor with override = %q, want custom-impl", got)
	}
	// An empty override value falls back to the engine default.
	if got := PersonaFor(StageImplement, map[string]string{StageImplement: ""}); got != "koryph-implementer" {
		t.Errorf("PersonaFor with empty override = %q, want koryph-implementer", got)
	}
}
