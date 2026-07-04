// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package modelroute

import (
	"fmt"
	"strings"

	"github.com/koryph/koryph/internal/runtime"
)

// defaultAllowed is the allowlist when a project declares none. Note that
// "fable" is intentionally absent: fable is only ever permitted when a
// project lists it explicitly.
var defaultAllowed = []string{TierHaiku, TierSonnet, TierOpus}

// validTier reports whether s is a known model tier.
func validTier(s string) bool {
	switch s {
	case TierHaiku, TierSonnet, TierOpus, TierFable:
		return true
	default:
		return false
	}
}

// Resolve applies the label/flag precedence and the model policy, returning
// the chosen tier, persona, and a human-readable rationale. It fails closed:
// any resolved tier outside the project's allowlist is an error, and "fable"
// is rejected unless it was both explicitly requested and allowlisted.
func Resolve(r Req) (Resolution, error) {
	allowed := r.AllowedModels
	if len(allowed) == 0 {
		allowed = defaultAllowed
	}

	tier, rationale, explicit := resolveTier(r)

	if !validTier(tier) {
		return Resolution{}, fmt.Errorf(
			"modelroute: unknown model tier %q (want one of haiku, sonnet, opus, fable)", tier)
	}

	// Fable guard: a resolved "fable" is only legal when the project
	// allowlist includes it AND the source was an explicit flag or label
	// (run default and stage default may never yield fable, even if allowed).
	if tier == TierFable && (!contains(allowed, TierFable) || !explicit) {
		return Resolution{}, fmt.Errorf(
			"modelroute: Fable requires explicit per-task selection and project allowlist "+
				"(source explicit=%t, allowlist=%v)", explicit, allowed)
	}

	// Fail closed: never dispatch a tier the project has not allowed.
	if !contains(allowed, tier) {
		return Resolution{}, fmt.Errorf(
			"modelroute: resolved model %q is not in the project allowlist %v (fail closed)",
			tier, allowed)
	}

	return Resolution{
		Model:     tier,
		Persona:   PersonaFor(r.Stage, r.Stages),
		Rationale: rationale,
	}, nil
}

// resolveTier walks the precedence chain and returns the raw (unvalidated)
// tier, its rationale, and whether the source was explicit (flag or label).
func resolveTier(r Req) (tier, rationale string, explicit bool) {
	// (1) explicit --model on a single dispatch.
	if r.ExplicitModel != "" {
		return r.ExplicitModel, "explicit --model", true
	}
	// (2) stage-scoped label model:<stage>:<tier>.
	if r.Stage != "" {
		if t, ok := stageModelLabel(r.Labels, r.Stage); ok {
			return t, fmt.Sprintf("label model:%s:%s", r.Stage, t), true
		}
	}
	// (3) plain label model:<tier>.
	if t, ok := plainModelLabel(r.Labels); ok {
		return t, "label model:" + t, true
	}
	// (4) run default --default-model.
	if r.RunDefault != "" {
		return r.RunDefault, "run default --default-model", false
	}
	// (5) persona tier/model (koryph-v8u.10): the persona assigned to this
	// stage may carry a runtime-agnostic `tier:` (mapped through the active
	// runtime's model map) or a legacy `model:` pin, either of which beats
	// the blunt hardcoded stage default below. Gated on RepoRoot so callers
	// that never opt in (RepoRoot == "") see no behavior change.
	if r.RepoRoot != "" {
		persona := PersonaFor(r.Stage, r.Stages)
		if t, rat, ok := personaTierOrModel(r.RepoRoot, persona, r.ModelMap); ok {
			return t, rat, false
		}
	}
	// (6) stage default.
	return stageDefault(r.Stage), fmt.Sprintf("stage default (%s)", r.Stage), false
}

// personaTierOrModel resolves persona's frontmatter (tier, then legacy
// model) to a concrete tier value. ok is false when the persona file is
// missing/unreadable or carries neither field — the caller then falls
// through to the hardcoded stage default. A tier present but unmapped by the
// effective model map (an operator-defined tier the active runtime/project
// override doesn't cover) also falls through to the legacy model pin rather
// than erroring here — Resolve's own allowlist/fable checks are what fail
// closed on a genuinely bad value.
func personaTierOrModel(repoRoot, persona string, override map[string]string) (tier, rationale string, ok bool) {
	model, _, personaTier, err := PersonaMeta(repoRoot, persona)
	if err != nil {
		return "", "", false
	}
	if personaTier != "" {
		if mapped, found := effectiveModelMap(override)[personaTier]; found && mapped != "" {
			return mapped, fmt.Sprintf("persona %s tier %s -> %s", persona, personaTier, mapped), true
		}
	}
	if model != "" {
		return model, fmt.Sprintf("persona %s legacy model pin", persona), true
	}
	return "", "", false
}

// effectiveModelMap overlays a project's sparse model_map override
// (internal/project.Config.ModelMap) onto runtime.ClaudeModelMap.
//
// Claude is hardcoded here, not looked up through a runtime.Registry,
// because the engine does not yet route dispatch through the pluggable
// runtime layer (internal/runtime/registry.go's own doc: "registered but not
// yet consulted by the engine") — koryph only ever drives the Claude CLI
// today. When koryph-v8u.2 wires per-bead runtime selection, this becomes
// registry.Get(activeRuntimeName).ModelMap() with the identical override
// overlay below.
func effectiveModelMap(override map[string]string) map[string]string {
	out := make(map[string]string, len(runtime.ClaudeModelMap)+len(override))
	for k, v := range runtime.ClaudeModelMap {
		out[k] = v
	}
	for k, v := range override {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// stageModelLabel finds the value of a "model:<stage>:<tier>" label.
func stageModelLabel(labels []string, stage string) (string, bool) {
	prefix := "model:" + stage + ":"
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return l[len(prefix):], true
		}
	}
	return "", false
}

// plainModelLabel finds the value of a "model:<tier>" label. Stage-scoped
// labels (which carry a second ":") are ignored here so they cannot be
// mistaken for a bare tier.
func plainModelLabel(labels []string) (string, bool) {
	const prefix = "model:"
	for _, l := range labels {
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		rest := l[len(prefix):]
		if rest == "" || strings.Contains(rest, ":") {
			continue
		}
		return rest, true
	}
	return "", false
}

// stageDefault returns the engine's default tier for a stage.
func stageDefault(stage string) string {
	switch stage {
	case StagePlan, StageDesign, StageScore, StageReview:
		return TierOpus
	case StageImplement, StageDocs, StageTest:
		return TierSonnet
	case StageExplore, StageDebug:
		return TierHaiku
	default:
		return TierSonnet
	}
}

// PersonaFor maps a stage to the persona (agent) that runs it. A project's
// Stages map wins; otherwise the engine's namespaced fallback personas apply.
// The koryph- prefix keeps the shipped fallbacks from colliding with a
// project's own (or another tool's) .claude/agents entries; a project overrides
// any stage's persona via the Stages map.
func PersonaFor(stage string, stages map[string]string) string {
	if p, ok := stages[stage]; ok && p != "" {
		return p
	}
	switch stage {
	case StageImplement:
		return "koryph-implementer"
	case StagePlan, StageDesign:
		return "koryph-architect"
	case StageScore:
		return "koryph-plan-scorer"
	case StageReview:
		return "koryph-security-reviewer"
	case StageExplore:
		return "koryph-explorer"
	case StageDebug:
		return "koryph-debugger"
	case StageDocs:
		return "koryph-feature-docs-author"
	case StageTest:
		return "koryph-test-engineer"
	default:
		return "koryph-implementer"
	}
}

// RecoveryUpgrade returns the tier to retry a low-confidence task with. Every
// tier upgrades to opus. Fable is structurally excluded from recovery: a
// fable input is deliberately downgraded to opus, because recovery must never
// select fable.
func RecoveryUpgrade(current string) string {
	// current is accepted for documentation and future policy hooks; every
	// tier — including fable — resolves to opus on recovery.
	_ = current
	return TierOpus
}

// contains reports whether v is in s.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
