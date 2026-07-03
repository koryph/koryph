// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package promptc compiles dispatch prompts with cache-stable ordering:
//
//	[1] engine preamble   — identical for every dispatch of this engine
//	                        version (boundary rules, reporting contract,
//	                        checkpoint discipline). Cache-friendly.
//	[2] project block     — stable per project (conventions, gate commands,
//	                        cross-cutting gates, bootstrap notes).
//	[3] volatile tail     — the bead (title/description/plan), resume
//	                        context (RESUMING block with commit log guidance
//	                        + review findings path), nudge inbox pointer.
//
// The engine preamble MUST include the agent boundary contract:
//   - work only in your worktree, on your branch
//   - NO git checkout main / git merge / git push / bd close / gh pr merge —
//     the koryph merges and closes (also hook-enforced)
//   - commit early and often (commits are your checkpoints)
//   - source koryph-log helpers if present; else write status.json
//     heartbeats {state, step, pct} and a SUMMARY.md with sections:
//     What shipped / Stubs shipped / Follow-ups / Test evidence /
//     Changes requiring orchestrator review
//   - check INBOX.md in your phase dir between steps for operator nudges
//
// Implementation contract (compile.go):
//   - Compile(Input) string — deterministic, no timestamps inside sections
//     [1]/[2] (cache stability), volatile content only in [3].
package promptc

import "github.com/koryph/koryph/internal/beads"

// Input is everything the compiler may use.
type Input struct {
	EngineVersion  string
	ProjectName    string
	Conventions    string   // short project conventions text (from adapter/docs)
	Gate           []string // green-gate commands the agent must keep green
	CommitStyle    string   // "" | "conventional" | "custom"
	CommitTemplate string   // required guidance when CommitStyle == "custom"
	CrossGates     []string // cross-cutting gates beyond the green gate
	Bootstrap      []string
	Bead           beads.Issue
	PlanYAML       string // extracted koryph-plan block, if any
	ResumeSHA      string // non-empty → RESUMING block
	ReviewPath     string // non-empty → blocking review findings to address
	PhaseDir       string // where status.json / SUMMARY.md / INBOX.md live
	SummaryPath    string
	StatusPath     string
	LogPath        string
}
