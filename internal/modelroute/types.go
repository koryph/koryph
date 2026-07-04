// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package modelroute resolves (stage, bead labels, run default, project
// policy) → (model tier, persona, effort, rationale).
//
// Policy (fixed by user decision 2026-07-02):
//   - Opus is the most-advanced default: planner/architect stages default to
//     opus; implementation defaults to sonnet.
//   - Fable is NEVER selected implicitly. It requires BOTH the project's
//     AllowedModels to include "fable" AND an explicit source (bead label
//     model:fable / model:<stage>:fable, or an explicit --model fable flag).
//   - Recovery upgrades: sonnet→opus, haiku→sonnet? No — recovery upgrades
//     any tier below opus to opus when confidence is low. Fable is
//     structurally excluded from recovery upgrades.
//   - Every resolution records a human-readable rationale.
//
// Label precedence (kept wire-compatible with the bash engine), extended by
// koryph-v8u.10's persona tier step:
//
//	model:<stage>:<tier>  >  model:<tier>  >  run --default-model  >
//	persona tier (via the active runtime's model map, project-overridable)  >
//	persona model (legacy pin, also the fallback when the persona carries no
//	tier or the tier is unmapped)  >
//	stage default (plan/design/score → opus; implement/docs/test → sonnet;
//	explore/debug → haiku; review → opus).
//
// The persona-tier step only runs when Req.RepoRoot is set (see Req's doc);
// it is a strict insertion below run-default and above the hardcoded stage
// default, so a project or run that never opts in (RepoRoot == "") keeps
// today's exact behavior.
//
// Runtime selection (koryph-v8u.3): every resolution runs under one runtime
// (Req.Runtime — "" means "claude"), which namespaces two things that used to
// be Claude-only globals: the stage-default table (step 6 above) and the
// tier->model ModelMap the persona-tier step (step 4 above) resolves through.
// Resolve fails closed on an unregistered/tableless runtime rather than ever
// substituting claude's — see ResolveRuntimeName for how a bead's
// `runtime:<name>` label and a project's `default_runtime` produce this
// value ahead of Resolve.
//
// Implementation contract (route.go, persona.go):
//   - Resolve(Req) (Resolution, error) — applies precedence + policy; error
//     when the resolved tier is not in AllowedModels (fail closed).
//   - RecoveryUpgrade(current string) string — see policy above.
//   - PersonaFor(stage, cfg) string — project Stages map with namespaced
//     engine fallbacks (implement→koryph-implementer, plan→koryph-architect,
//     review→koryph-security-reviewer, explore→koryph-explorer, debug→
//     koryph-debugger, docs→koryph-feature-docs-author, test→
//     koryph-test-engineer, score→koryph-plan-scorer). The koryph- prefix
//     avoids clashing with a project's own .claude/agents entries.
//   - PersonaMeta(repoRoot, persona) (model, effort, tier, error) — parse the
//     YAML frontmatter of .claude/agents/<persona>.md (hand-rolled subset
//     parser: scalars + quoted strings; ignore lists/maps). tier is the
//     runtime-agnostic capability class (koryph-v8u.10); model is the
//     Claude-specific legacy pin.
package modelroute

// Model tiers as passed to `claude --model`.
const (
	TierHaiku  = "haiku"
	TierSonnet = "sonnet"
	TierOpus   = "opus"
	TierFable  = "fable"
)

// Stages.
const (
	StagePlan      = "plan"
	StageDesign    = "design"
	StageScore     = "score"
	StageImplement = "implement"
	StageReview    = "review"
	StageDocs      = "docs"
	StageExplore   = "explore"
	StageDebug     = "debug"
	StageTest      = "test"
)

// Req is one resolution request.
type Req struct {
	Stage         string
	Labels        []string          // bead labels
	RunDefault    string            // --default-model
	ExplicitModel string            // --model on a single dispatch (highest precedence)
	AllowedModels []string          // project policy; empty → ["haiku","sonnet","opus"]
	Stages        map[string]string // project persona map (may be nil)

	// RepoRoot and ModelMap enable the persona-tier resolution step
	// (koryph-v8u.10 item 4): RepoRoot locates .claude/agents/<persona>.md
	// for a PersonaMeta lookup, and ModelMap is the project's sparse
	// tier->model override (internal/project.Config.ModelMap) layered onto
	// the resolved runtime's ModelMap (see Runtime below; hardcoded to
	// runtime.ClaudeModelMap before koryph-v8u.3). RepoRoot == "" disables
	// this step entirely (every existing caller/test that omits it keeps
	// today's label/run-default/stage-default-only behavior unchanged).
	RepoRoot string
	ModelMap map[string]string

	// Runtime is the resolved runtime name (koryph-v8u.3) this dispatch runs
	// under: "claude", or another name registered in runtime.Default. Empty
	// means "claude" — every existing caller that predates runtime selection
	// omits this field and gets byte-for-byte the same resolution as before
	// (claude's stage-default table and ModelMap). Resolve validates the name
	// up front and fails closed on anything else unregistered; it never
	// silently substitutes claude. See ResolveRuntimeName for the bead-label/
	// project-default precedence that normally produces this value before a
	// caller builds a Req.
	Runtime string
}

// Resolution is the outcome.
type Resolution struct {
	Model     string `json:"model"`
	Persona   string `json:"persona"`
	Effort    string `json:"effort,omitempty"`
	Rationale string `json:"rationale"`
}
