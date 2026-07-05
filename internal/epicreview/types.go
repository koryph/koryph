// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package epicreview runs the epic-scoped validation pass: a read-only
// validator agent over the union of an epic's merged children, returning a
// verdict that the engine acts on deterministically (file gap follow-ups, file
// structural findings, close the epic). A transient validator failure (rate/
// usage limit, timeout, a one-off unparseable reply) is RETRIED with
// EXPONENTIAL backoff (Opts.Attempts, default 3) so a rate limit is given
// progressively more time to clear; only when every attempt fails is
// Verdict{Degraded:true} returned, carrying a human-readable Reason.
// Validate never panics the loop — the caller (engine) decides policy.
//
// Architecture (§3 of docs/designs/2026-07-epic-validation.md):
//   - Runs on main after all children merge — no worktree, no branch.
//   - Persona koryph-epic-validator, model opus (frontier tier).
//   - --permission-mode plan: the validator NEVER writes; it returns a verdict.
//   - Verdict persisted to .koryph/epic-reviews/<epic-id>-round<N>.json.
//   - Two lenses: completeness (gaps) + structural health.
//   - Only gaps bear on met and epic closure; structural findings are
//     improvements surfaced by the epic, not obligations it failed.
package epicreview

import "github.com/koryph/koryph/internal/account"

// Gap is one completeness gap: a design goal or acceptance criterion that the
// union of the epic's children did not fully meet.
type Gap struct {
	Title      string   `json:"title"`
	Why        string   `json:"why"`        // which design section is unmet and how
	Acceptance string   `json:"acceptance"` // what done looks like
	Type       string   `json:"type"`       // task|bug|chore
	Labels     []string `json:"labels"`     // area:*, fp:read:*, …
	DependsOn  []string `json:"depends_on"` // sibling gap index or existing bead id
}

// Structural is one structural-health finding: code or architecture debt
// surfaced by viewing the entire epic at once.
type Structural struct {
	// Category is one of: extract-common, architecture, duplication.
	Category   string   `json:"category"`
	Title      string   `json:"title"`
	Why        string   `json:"why"`        // evidence with file paths
	Acceptance string   `json:"acceptance"` // what done looks like
	Type       string   `json:"type"`       // chore|task
	Labels     []string `json:"labels"`     // area:*, …
}

// Verdict is the epic validation outcome. Only Gaps bear on Met and epic
// closure; Structural findings are routed to the backlog independently.
type Verdict struct {
	Met        bool         `json:"met"`
	Summary    string       `json:"summary"`              // one paragraph
	Gaps       []Gap        `json:"gaps,omitempty"`       // completeness gaps
	Structural []Structural `json:"structural,omitempty"` // structural findings
	Degraded   bool         `json:"degraded,omitempty"`   // validator could not be obtained
	Reason     string       `json:"reason,omitempty"`     // why it degraded (never a black box)
	Attempts   int          `json:"attempts,omitempty"`   // validator spawns made
	Raw        string       `json:"-"`
}

// Child describes one completed child bead of the epic.
type Child struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	CloseReason string   `json:"close_reason,omitempty"`
	MergeSHA    string   `json:"merge_sha,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// Opts configures one epic validation run.
type Opts struct {
	// Epic metadata.
	EpicID          string
	EpicTitle       string
	EpicDescription string
	EpicNotes       string // any extra notes on the epic bead

	// Design doc path relative to RepoRoot. The validator agent reads it.
	DesignDocPath string

	// Children are the completed child beads whose work is now on main.
	Children []Child

	// PriorVerdicts holds the raw JSON of previous validation rounds (earliest
	// first) so the validator has round context.
	PriorVerdicts []string

	// Round is the 1-based round number for file naming and prompt context.
	Round int

	// Execution settings.
	RepoRoot   string          // repo root (validator runs here — main, no worktree)
	Profile    account.Profile // account profile for billing
	Persona    string          // default koryph-epic-validator
	Model      string          // default opus
	ClaudeBin  string          // default "claude"
	TimeoutSec int             // default 420
	Attempts   int             // validator spawn attempts before degrading (default 3)

	// OutDir is the directory for persisted verdict JSON.
	// Default: <RepoRoot>/.koryph/epic-reviews
	OutDir string
}
