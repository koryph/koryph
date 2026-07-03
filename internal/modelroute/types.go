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
// Label precedence (kept wire-compatible with the bash engine):
//
//	model:<stage>:<tier>  >  model:<tier>  >  run --default-model  >
//	stage default (plan/design/score → opus; implement/docs/test → sonnet;
//	explore/debug → haiku; review → opus).
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
//   - PersonaMeta(repoRoot, persona) (model, effort, error) — parse the YAML
//     frontmatter of .claude/agents/<persona>.md (hand-rolled subset parser:
//     scalars + quoted strings; ignore lists/maps).
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
}

// Resolution is the outcome.
type Resolution struct {
	Model     string `json:"model"`
	Persona   string `json:"persona"`
	Effort    string `json:"effort,omitempty"`
	Rationale string `json:"rationale"`
}
