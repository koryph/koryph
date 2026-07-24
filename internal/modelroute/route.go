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
// internal/engine's dispatchBead calls this first and validates the selected
// runtime before dispatch. This function performs no validation against runtime.Default; Resolve is what
// fails closed on whatever name ends up in Req.Runtime.
func ResolveRuntimeName(labels []string, defaultRuntime string) (name, rationale string) {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, runtimeLabelPrefix); ok && v != "" {
			return v, "label " + l
		}
	}
	// A concrete model uniquely identifies its owning built-in runtime when no
	// runtime label was supplied. This makes `model:gpt-5.6-terra` sufficient for a
	// Codex bead while retaining `runtime:` as the explicit disambiguator for
	// a future model offered by more than one runtime.
	if model, ok := plainModelLabel(labels); ok {
		if inferred, ok := inferRuntimeForModel(model); ok {
			return inferred, "model " + model + " implies runtime " + inferred
		}
	}
	if defaultRuntime != "" {
		return defaultRuntime, "project default_runtime " + defaultRuntime
	}
	return "claude", "default"
}

func inferRuntimeForModel(model string) (string, bool) {
	owners := make([]string, 0, 2)
	for _, candidate := range []struct {
		name string
		map_ runtime.ModelMap
	}{
		{"claude", runtime.ClaudeModelMap},
		{"codex", runtime.CodexModelMap},
	} {
		if candidate.name == "claude" && validTier(model) {
			owners = append(owners, candidate.name)
			continue
		}
		for _, id := range candidate.map_ {
			if id == model {
				owners = append(owners, candidate.name)
				break
			}
		}
	}
	if len(owners) == 1 {
		return owners[0], true
	}
	return "", false
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
	if runtimeName != "claude" && runtimeName != "codex" {
		if _, ok := runtime.Default.Get(runtimeName); !ok {
			return Resolution{}, fmt.Errorf(
				"modelroute: unknown runtime %q (not registered in runtime.Default)", runtimeName)
		}
	}

	allowed := r.AllowedModels
	if len(allowed) == 0 {
		allowed = defaultAllowed
	}

	// An equivalency is a portable (capability, effort) request. It is
	// deliberately distinct from model:<id>, which always denotes an exact
	// runtime-native model. Combining them would be ambiguous, so fail closed.
	equivTier, equivEffort, hasLabelEquiv := equivalencyLabel(r.Labels)
	_, hasStageModel := stageModelLabel(r.Labels, r.Stage)
	_, hasModel := plainModelLabel(r.Labels)
	if hasLabelEquiv && (r.ExplicitModel != "" || hasStageModel || hasModel) {
		return Resolution{}, fmt.Errorf("modelroute: equiv label cannot be combined with an explicit model selection")
	}
	// A bead's model/equiv labels and a run --default-model both outrank a
	// project default equivalency. Only use the latter when no higher source
	// selected a concrete model. This mirrors resolveTier's normal precedence.
	hasEquiv := hasLabelEquiv
	if !hasEquiv && r.RunEquivalent != "" && r.ExplicitModel == "" && !hasStageModel && !hasModel && r.RunDefault == "" {
		var ok bool
		equivTier, equivEffort, ok = parseEquivalency(r.RunEquivalent)
		if !ok {
			return Resolution{}, fmt.Errorf("modelroute: invalid run/project default equivalency %q (want <frontier|standard|light>:<effort>)", r.RunEquivalent)
		}
		hasEquiv = true
	}

	tier, rationale, explicit, err := resolveTier(r, runtimeName)
	if err != nil {
		return Resolution{}, err
	}
	equivalent := ""
	if hasEquiv {
		mapped, ok := effectiveModelMapFor(runtimeName, r.ModelMap)[equivTier]
		if !ok || mapped == "" {
			return Resolution{}, fmt.Errorf("modelroute: runtime %q has no model equivalency for %q", runtimeName, equivTier)
		}
		nativeEffort, ok := effectiveEffortMapFor(runtimeName, r.EffortMap)[equivEffort]
		if !ok || nativeEffort == "" {
			return Resolution{}, fmt.Errorf("modelroute: runtime %q has no effort equivalency for %q", runtimeName, equivEffort)
		}
		if r.RunEquivalent != "" && !hasLabelEquiv {
			rationale = "project default equivalent " + equivTier + ":" + equivEffort
		} else {
			rationale = "label equiv:" + equivTier + ":" + equivEffort
		}
		tier, explicit = mapped, true
		equivalent = equivTier + ":" + equivEffort
		equivEffort = nativeEffort
	}

	if !validModelForRuntime(tier, runtimeName, r.ModelMap, r.AllowedModels) {
		if runtimeName == "claude" {
			return Resolution{}, fmt.Errorf(
				"modelroute: unknown model tier %q (want one of haiku, sonnet, opus, fable)", tier)
		}
		return Resolution{}, fmt.Errorf(
			"modelroute: model %q is not known for runtime %q", tier, runtimeName)
	}

	// Fable guard: a resolved "fable" is only legal when the project
	// allowlist includes it AND the source was an explicit flag or label
	// (run default and stage default may never yield fable, even if allowed).
	if runtimeName == "claude" && tier == TierFable && (!contains(allowed, TierFable) || !explicit) {
		return Resolution{}, fmt.Errorf(
			"modelroute: Fable requires explicit per-task selection and project allowlist "+
				"(source explicit=%t, allowlist=%v)", explicit, allowed)
	}

	// Fail closed: never dispatch a tier the project has not allowed.
	// AllowedModels predates runtime selection and therefore commonly holds
	// Claude's legacy aliases on otherwise multi-runtime projects. A native
	// model declared by the selected runtime (or its project model_map) is
	// already an explicit, fail-closed allowlist for that runtime; do not make
	// an old Claude-only list accidentally disable Codex. Unknown exact models
	// still require an explicit AllowedModels entry.
	if len(r.AllowedModels) > 0 && !contains(allowed, tier) && !declaredModelForRuntime(tier, runtimeName, r.ModelMap) {
		return Resolution{}, fmt.Errorf(
			"modelroute: resolved model %q is not in the project allowlist %v (fail closed)",
			tier, allowed)
	}

	res := Resolution{
		Model:      tier,
		Persona:    PersonaFor(r.Stage, r.Stages),
		Effort:     equivEffort,
		Equivalent: equivalent,
		Rationale:  rationale,
	}
	if effort, ok := effortLabel(r.Labels); ok && !hasEquiv {
		res.Effort = effort
	}
	return res, nil
}

// ResolveEquivalent translates source's normal model decision through target's
// model and effort maps. It deliberately resolves the source first: the
// existing label/default/persona/stage precedence remains authoritative, and
// a runtime override changes only the concrete runtime/model selected after
// that portable capability is known.
//
// An exact native model may be translated only when it maps to exactly one
// portable tier. This is essential for maps such as Codex's current default,
// where the same concrete id intentionally occupies frontier, standard, and
// light; guessing in that case would silently change the work's capability.
// Callers pass source with the normally resolved Runtime and target with the
// forced Runtime plus that runtime's per-project maps.
func ResolveEquivalent(source, target Req) (Resolution, error) {
	sourceRes, err := Resolve(source)
	if err != nil {
		return Resolution{}, err
	}
	eq, err := EquivalencyFor(source, sourceRes)
	if err != nil {
		return Resolution{}, err
	}
	targetRuntime := target.Runtime
	if targetRuntime == "" {
		targetRuntime = "claude"
	}
	mapped, ok := effectiveModelMapFor(targetRuntime, target.ModelMap)[eq.Tier]
	if !ok || mapped == "" {
		return Resolution{}, fmt.Errorf("modelroute: runtime %q has no model equivalency for %q", targetRuntime, eq.Tier)
	}

	// Preserve the stage/persona selection but discard every source model
	// directive. Passing the mapped native model explicitly through Resolve
	// retains its runtime/allowlist/fable validation rather than duplicating
	// that policy here.
	target.Labels = nonModelLabels(target.Labels)
	target.ExplicitModel = mapped
	target.RunDefault = ""
	target.RunEquivalent = ""
	res, err := Resolve(target)
	if err != nil {
		return Resolution{}, err
	}
	if eq.Effort != "" {
		nativeEffort, ok := effectiveEffortMapFor(targetRuntime, target.EffortMap)[eq.Effort]
		if !ok || nativeEffort == "" {
			return Resolution{}, fmt.Errorf("modelroute: runtime %q has no effort equivalency for %q", targetRuntime, eq.Effort)
		}
		res.Effort = nativeEffort
		res.Equivalent = eq.Tier + ":" + eq.Effort
	}
	sourceRuntime := source.Runtime
	if sourceRuntime == "" {
		sourceRuntime = "claude"
	}
	res.Rationale = fmt.Sprintf("runtime equivalent %s from %s: %s -> %s", targetRuntime, sourceRuntime, sourceRes.Rationale, eq.Tier)
	return res, nil
}

// EquivalencyFor derives the portable capability behind a normal resolution.
// equiv: is already portable. Native model selections use their source
// runtime's inverse model map only when that inverse is unambiguous. For a
// non-explicit default that cannot be reverse-mapped, stage and persona tier
// provenance is safe; for an explicit native model it is not, so we refuse it
// with a remediation that preserves the operator's intent.
func EquivalencyFor(source Req, res Resolution) (Equivalency, error) {
	if res.Equivalent != "" {
		tier, effort, ok := parseEquivalency(res.Equivalent)
		if ok {
			return Equivalency{Tier: tier, Effort: effort}, nil
		}
	}
	runtimeName := source.Runtime
	if runtimeName == "" {
		runtimeName = "claude"
	}
	tier := uniqueTierForModel(res.Model, runtimeName, source.ModelMap)
	personaPinnedModel := false
	if tier == "" && !hasExplicitModelSource(source) {
		if source.RepoRoot != "" {
			persona := PersonaFor(source.Stage, source.Stages)
			if personaModel, _, personaTier, err := PersonaMeta(source.RepoRoot, persona); err == nil {
				personaPinnedModel = personaModel != ""
				if isPortableTier(personaTier) {
					tier = personaTier
				}
			}
		}
		if tier == "" && !personaPinnedModel {
			tier = portableTierForStage(source.Stage)
		}
	}
	if tier == "" {
		return Equivalency{}, fmt.Errorf(
			"modelroute: cannot translate exact model %q from runtime %q to an equivalent capability; use equiv:<frontier|standard|light>:<effort> on the bead or configure default_equivalent",
			res.Model, runtimeName)
	}

	effort := ""
	if native, ok := effortLabel(source.Labels); ok {
		var candidates []string
		for portable, mapped := range effectiveEffortMapFor(runtimeName, source.EffortMap) {
			if mapped == native {
				candidates = append(candidates, portable)
			}
		}
		if len(candidates) != 1 {
			return Equivalency{}, fmt.Errorf(
				"modelroute: cannot translate native effort %q from runtime %q; use equiv:%s:<effort>", native, runtimeName, tier)
		}
		effort = candidates[0]
	}
	return Equivalency{Tier: tier, Effort: effort}, nil
}

func nonModelLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if strings.HasPrefix(label, "model:") || strings.HasPrefix(label, "equiv:") || strings.HasPrefix(label, "effort:") {
			continue
		}
		out = append(out, label)
	}
	return out
}

func hasExplicitModelSource(r Req) bool {
	if r.ExplicitModel != "" || r.RunDefault != "" {
		return true
	}
	_, stage := stageModelLabel(r.Labels, r.Stage)
	_, plain := plainModelLabel(r.Labels)
	return stage || plain
}

func uniqueTierForModel(model, runtimeName string, override map[string]string) string {
	var matches []string
	for tier, candidate := range effectiveModelMapFor(runtimeName, override) {
		if candidate == model {
			matches = append(matches, tier)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func portableTierForStage(stage string) string {
	switch stage {
	case StagePlan, StageDesign, StageScore, StageReview:
		return runtime.TierFrontier
	case StageExplore, StageDebug:
		return runtime.TierLight
	default:
		return runtime.TierStandard
	}
}

func isPortableTier(tier string) bool {
	switch tier {
	case runtime.TierFrontier, runtime.TierStandard, runtime.TierLight:
		return true
	default:
		return false
	}
}

func declaredModelForRuntime(model, runtimeName string, override map[string]string) bool {
	if runtimeName == "claude" {
		return false
	}
	for _, id := range effectiveModelMapFor(runtimeName, override) {
		if id == model {
			return true
		}
	}
	return false
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
	if runtimeName == "codex" {
		return runtime.CodexModelMap, true
	}
	rt, ok := runtime.Default.Get(runtimeName)
	if !ok {
		return nil, false
	}
	return rt.ModelMap(), true
}

func effectiveModelMapFor(runtimeName string, override map[string]string) map[string]string {
	base, _ := modelMapFor(runtimeName)
	return effectiveModelMap(base, override)
}

func effectiveEffortMapFor(runtimeName string, override map[string]string) map[string]string {
	var base runtime.EffortMap
	switch runtimeName {
	case "claude":
		base = runtime.ClaudeEffortMap
	case "codex":
		base = runtime.CodexEffortMap
	default:
		if rt, ok := runtime.Default.Get(runtimeName); ok {
			if mapper, ok := rt.(runtime.EffortMapper); ok {
				base = mapper.EffortMap()
			}
		}
	}
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

// validModelForRuntime accepts either one of the runtime's declared concrete
// models or an operator-allowlisted exact model. Claude's long-standing tier
// aliases remain valid for compatibility.
func validModelForRuntime(model, runtimeName string, override map[string]string, allowed []string) bool {
	if runtimeName == "claude" && validTier(model) {
		return true
	}
	for _, id := range effectiveModelMapFor(runtimeName, override) {
		if id == model {
			return true
		}
	}
	return contains(allowed, model)
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

// equivalencyLabel reads the portable model-plus-effort form
// `equiv:<frontier|standard|light>:<effort>`. It has a separate namespace so
// existing model:<tier> labels remain exact legacy Claude selections.
func equivalencyLabel(labels []string) (tier, effort string, ok bool) {
	for _, l := range labels {
		if !strings.HasPrefix(l, "equiv:") {
			continue
		}
		return parseEquivalency(strings.TrimPrefix(l, "equiv:"))
	}
	return "", "", false
}

func parseEquivalency(value string) (tier, effort string, ok bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	if isPortableTier(parts[0]) {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func effortLabel(labels []string) (string, bool) {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "effort:"); ok && v != "" && !strings.Contains(v, ":") {
			return v, true
		}
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
	"codex": {
		StagePlan:      runtime.CodexModelMap[runtime.TierFrontier],
		StageDesign:    runtime.CodexModelMap[runtime.TierFrontier],
		StageScore:     runtime.CodexModelMap[runtime.TierFrontier],
		StageReview:    runtime.CodexModelMap[runtime.TierFrontier],
		StageImplement: runtime.CodexModelMap[runtime.TierStandard],
		StageDocs:      runtime.CodexModelMap[runtime.TierStandard],
		StageTest:      runtime.CodexModelMap[runtime.TierStandard],
		StageExplore:   runtime.CodexModelMap[runtime.TierLight],
		StageDebug:     runtime.CodexModelMap[runtime.TierLight],
	},
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

// TierForModelID normalizes a concrete model id (e.g. "claude-sonnet-4-5",
// as reported by a result line's modelUsage keys — see dispatch.
// ParseActualModel, koryph-qf6.2) to its tier, or "" when the id names no
// known tier. Substring matching is deliberate: model ids carry version
// suffixes that change across CLI releases, while the tier family name
// embedded in the id is the stable part. Checked longest-family-first so a
// hypothetical compound id cannot mis-bucket.
func TierForModelID(id string) string {
	lower := strings.ToLower(id)
	for _, tier := range []string{TierSonnet, TierHaiku, TierOpus, TierFable} {
		if strings.Contains(lower, tier) {
			return tier
		}
	}
	return ""
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

// EscalationTier returns the tier the FINAL attempt of a bead-fault requeue
// should escalate to (koryph-qf6.4): RecoveryUpgrade's target when current is
// a strictly lower tier AND the target is in the project allowlist (nil/empty
// allowed means the default allowlist, which includes opus). Returns "" when
// no escalation applies: current is already at or above the target (opus, and
// fable — escalation must never DOWNGRADE a fable bead to opus), current is
// empty/unknown (an adopted or legacy slot whose model we cannot vouch for),
// or the target is not allowlisted. This is the policy gate the engine
// consults instead of calling RecoveryUpgrade raw — the requeue path bypasses
// Resolve (koryph-ehx freeze), so the allowlist check Resolve would normally
// perform has to happen here.
func EscalationTier(current string, allowed []string) string {
	if current != TierHaiku && current != TierSonnet {
		return ""
	}
	up := RecoveryUpgrade(current)
	if len(allowed) == 0 {
		allowed = defaultAllowed
	}
	if !contains(allowed, up) {
		return ""
	}
	return up
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
