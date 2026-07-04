// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

// Tier vocabulary (koryph-v8u.10, agents/README.md's frontmatter contract).
// These are the runtime-AGNOSTIC capability classes a persona's `tier:`
// frontmatter scalar carries; a Runtime's ModelMap translates each one to a
// concrete model id for that runtime's `--model`-equivalent flag.
const (
	// TierFrontier is the strongest reasoning class a runtime offers
	// (Claude Opus-class or better). Required for work whose errors poison
	// downstream automation: design decomposition, footprint/dependency
	// assignment, plan scoring, security review, recovery analysis.
	TierFrontier = "frontier"
	// TierStandard is the capable general-coding class (Claude Sonnet-class).
	TierStandard = "standard"
	// TierLight is the fast/cheap class (Claude Haiku-class).
	TierLight = "light"
)

// ModelMap is one runtime's tier -> concrete-model-id table (koryph-v8u.10
// item 2). It is a plain map rather than a struct so a runtime adapter can
// build one from whatever vendor pricing/model list it discovers at
// registration time, and so project config's sparse override (see
// internal/project.Config.ModelMap) can overlay onto it key-by-key without a
// bespoke merge type. A missing key means "this runtime declares no mapping
// for that tier" — callers fall back to the persona's legacy `model` pin.
type ModelMap map[string]string

// ClaudeModelMap is the claude runtime's default tier map. It is exported as
// a package-level constant-shaped value (rather than requiring a live Claude
// Runtime instance) because koryph does not yet route dispatch through the
// Registry — internal/engine calls this directly today (see
// internal/modelroute/route.go's effectiveModelMap) — and because a future
// Claude adapter's ModelMap() method (koryph-v8u.2) is expected to return
// exactly this value, so the two never drift apart.
//
// "fable" is deliberately ABSENT here: it is never an implicit tier mapping,
// only a valid explicit *override* an operator may opt into for "frontier"
// via internal/project.Config.ModelMap (e.g. {"frontier": "fable"}) — see
// modelroute.Resolve's fable guard, which still requires that override to
// have been paired with an explicit selection source before it takes effect.
var ClaudeModelMap = ModelMap{
	TierFrontier: "opus",
	TierStandard: "sonnet",
	TierLight:    "haiku",
}
