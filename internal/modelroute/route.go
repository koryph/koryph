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

// runtimeLabelPrefix is the bead label prefix for runtime selection
// (koryph-v8u.3), mirroring "model:<tier>"'s bare-label grammar: unlike
// model:, runtime: is never stage-scoped — a bead dispatches under exactly
// one runtime for its whole lifetime, not per pipeline stage.
const runtimeLabelPrefix = "runtime:"

// ResolveRuntimeName picks which runtime a bead dispatches under, applying
// the koryph-v8u.3 precedence: bead `runtime:<name>` label > project
// default_runtime > "claude". It is the runtime-selection analogue of
// resolveTier's label handling, factored out as its own exported function
// (rather than folded into Resolve) because the caller must know the runtime
// BEFORE it can decide whether to even attempt a dispatch —
// internal/engine's dispatchBead calls this first and blocks the slot
// outright for any non-claude result, never reaching Resolve at all. This
// function performs no validation against runtime.Default; Resolve is what
// fails closed on whatever name ends up in Req.Runtime.
func ResolveRuntimeName(labels []string, defaultRuntime string) (name, rationale string) {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, runtimeLabelPrefix); ok && v != "" {
			return v, "label " + l
		}
	}
	if defaultRuntime != "" {
		return defaultRuntime, "project default_runtime " + defaultRuntime
	}
	return "claude", "default"
}

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
// any resolved tier outside the project's allowlist is an error, "fable" is
// rejected unless it was both explicitly requested and allowlisted, and an
// unrecognized runtime (Req.Runtime, koryph-v8u.3) is an error too — Resolve
// never silently substitutes claude for a runtime it cannot vouch for.
func Resolve(r Req) (Resolution, error) {
	runtimeName := r.Runtime
	if runtimeName == "" {
		runtimeName = "claude"
	}
	// "claude" is always a valid runtime name without a registry round-trip
	// (see modelMapFor's doc for why); every OTHER name must be registered in
	// runtime.Default. Checked up front so an unknown runtime fails here
	// regardless of which resolution step would otherwise have won (e.g. an
	// explicit --model that never even looks at the runtime).
	if runtimeName != "claude" {
		if _, ok := runtime.Default.Get(runtimeName); !ok {
			return Resolution{}, fmt.Errorf(
				"modelroute: unknown runtime %q (not registered in runtime.Default)", runtimeName)
		}
	}

	allowed := r.AllowedModels
	if len(allowed) == 0 {
		allowed = defaultAllowed
	}

	tier, rationale, explicit, err := resolveTier(r, runtimeName)
	if err != nil {
		return Resolution{}, err
	}

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
// runtimeName is the already-validated (by Resolve) runtime this request
// resolves under; it only matters to steps (5) and (6) below.
func resolveTier(r Req, runtimeName string) (tier, rationale string, explicit bool, err error) {
	// (1) explicit --model on a single dispatch.
	if r.ExplicitModel != "" {
		return r.ExplicitModel, "explicit --model", true, nil
	}
	// (2) stage-scoped label model:<stage>:<tier>.
	if r.Stage != "" {
		if t, ok := stageModelLabel(r.Labels, r.Stage); ok {
			return t, fmt.Sprintf("label model:%s:%s", r.Stage, t), true, nil
		}
	}
	// (3) plain label model:<tier>.
	if t, ok := plainModelLabel(r.Labels); ok {
		return t, "label model:" + t, true, nil
	}
	// (4) run default --default-model.
	if r.RunDefault != "" {
		return r.RunDefault, "run default --default-model", false, nil
	}
	// (5) persona tier/model (koryph-v8u.10, runtime-namespaced by
	// koryph-v8u.3): the persona assigned to this stage may carry a
	// runtime-agnostic `tier:` (mapped through the SELECTED runtime's model
	// map) or a legacy `model:` pin, either of which beats the blunt
	// hardcoded stage default below. Gated on RepoRoot so callers that never
	// opt in (RepoRoot == "") see no behavior change.
	if r.RepoRoot != "" {
		persona := PersonaFor(r.Stage, r.Stages)
		if t, rat, ok := personaTierOrModel(r.RepoRoot, persona, runtimeName, r.ModelMap); ok {
			return t, rat, false, nil
		}
	}
	// (6) stage default, namespaced per runtime (koryph-v8u.3): an unknown
	// runtime here is a Resolve-level error, never a silent claude fallback.
	t, derr := stageDefault(runtimeName, r.Stage)
	if derr != nil {
		return "", "", false, derr
	}
	return t, fmt.Sprintf("stage default (%s)", r.Stage), false, nil
}

// personaTierOrModel resolves persona's frontmatter (tier, then legacy
// model) to a concrete tier value. ok is false when the persona file is
// missing/unreadable or carries neither field — the caller then falls
// through to the hardcoded stage default. A tier present but unmapped by the
// effective model map (an operator-defined tier the selected runtime/project
// override doesn't cover) also falls through to the legacy model pin rather
// than erroring here — Resolve's own allowlist/fable checks are what fail
// closed on a genuinely bad value.
func personaTierOrModel(repoRoot, persona, runtimeName string, override map[string]string) (tier, rationale string, ok bool) {
	model, _, personaTier, err := PersonaMeta(repoRoot, persona)
	if err != nil {
		return "", "", false
	}
	if personaTier != "" {
		base, found := modelMapFor(runtimeName)
		if found {
			if mapped, ok := effectiveModelMap(base, override)[personaTier]; ok && mapped != "" {
				return mapped, fmt.Sprintf("persona %s tier %s -> %s", persona, personaTier, mapped), true
			}
		}
	}
	if model != "" {
		return model, fmt.Sprintf("persona %s legacy model pin", persona), true
	}
	return "", "", false
}

// modelMapFor resolves runtimeName's tier -> concrete-model-id table
// (koryph-v8u.3, replacing the pre-existing hardcoded runtime.ClaudeModelMap
// reference). "claude" is answered directly from runtime.ClaudeModelMap
// WITHOUT a runtime.Default round-trip: this package's own tests (and any
// binary that has not imported internal/runtime/claude for its registration
// side effect) must resolve claude beads identically, with no import-order
// dependency. Any other name is looked up in runtime.Default — the
// process-wide registry every real adapter package self-registers into at
// init (internal/runtime/claude/register.go) — and its ModelMap() is used
// verbatim. found is false only when runtimeName is neither "claude" nor
// registered; by the time this is called from resolveTier, Resolve has
// already validated that up front, so this is a defensive fallback, not the
// primary fail-closed gate.
func modelMapFor(runtimeName string) (runtime.ModelMap, bool) {
	if runtimeName == "claude" {
		return runtime.ClaudeModelMap, true
	}
	rt, ok := runtime.Default.Get(runtimeName)
	if !ok {
		return nil, false
	}
	return rt.ModelMap(), true
}

// effectiveModelMap overlays a project's sparse model_map override
// (internal/project.Config.ModelMap) onto base, the selected runtime's own
// ModelMap (see modelMapFor).
func effectiveModelMap(base runtime.ModelMap, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
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

// claudeStageDefaults is claude's stage -> tier default table, preserved
// byte-for-byte from the pre-koryph-v8u.3 hardcoded switch below (see git
// history): plan/design/score/review -> opus; implement/docs/test -> sonnet;
// explore/debug -> haiku.
var claudeStageDefaults = map[string]string{
	StagePlan:      TierOpus,
	StageDesign:    TierOpus,
	StageScore:     TierOpus,
	StageReview:    TierOpus,
	StageImplement: TierSonnet,
	StageDocs:      TierSonnet,
	StageTest:      TierSonnet,
	StageExplore:   TierHaiku,
	StageDebug:     TierHaiku,
}

// runtimeStageDefaults namespaces stage-default tables by runtime name
// (koryph-v8u.3 item 3). Only "claude" is populated today — the only runtime
// internal/engine's dispatchBead actually dispatches through; any other
// selection is blocked before it ever reaches Resolve. Unlike ModelMap,
// stage-tier defaults are not part of the runtime.Runtime interface (stage
// semantics are an engine/modelroute concept, not something a CLI adapter
// exposes), so there is deliberately no generic/derived table for an
// arbitrary registered runtime: a runtime with no entry here fails resolution
// (see stageDefault) rather than silently borrowing claude's table. Tests
// add entries to exercise a second runtime end-to-end (white-box, same
// package); a future runtime adapter package wanting its own defaults would
// add its table here directly, the same way this map is populated for
// claude.
var runtimeStageDefaults = map[string]map[string]string{
	"claude": claudeStageDefaults,
}

// stageDefault returns runtimeName's default tier for stage. It fails closed
// (koryph-v8u.3) when runtimeName carries no stage-default table at all —
// never silently substituting claude's table for an unrecognized runtime;
// a table that simply lacks an entry for this particular stage falls back to
// sonnet, matching the pre-koryph-v8u.3 switch's default case.
func stageDefault(runtimeName, stage string) (string, error) {
	table, ok := runtimeStageDefaults[runtimeName]
	if !ok {
		return "", fmt.Errorf(
			"modelroute: runtime %q has no stage-default table (fail closed)", runtimeName)
	}
	if t, ok := table[stage]; ok {
		return t, nil
	}
	return TierSonnet, nil
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
