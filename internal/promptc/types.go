// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package promptc compiles dispatch prompts with cache-stable ordering:
//
//	[1] engine preamble   — identical for every dispatch of this engine
//	                        version (boundary rules, reporting contract,
//	                        checkpoint discipline). Cache-friendly.
//	[2] project block     — stable per project (conventions, gate commands,
//	                        cross-cutting gates, bootstrap notes).
//	[3] volatile tail     — the bead (title/description/OPERATOR NOTES/plan),
//	                        resume context (RESUMING block with commit log
//	                        guidance + review findings path), nudge inbox
//	                        pointer.
//
// OPERATOR NOTES (koryph-o72): Bead.Notes carries any addendum appended via
// `bd update --append-notes` while the bead was still queued — i.e. before
// any agent existed to read an INBOX.md. It is rendered as its own
// clearly-delimited section whenever non-empty (compile.go's volatileTail),
// so a pre-dispatch nudge is guaranteed to reach the agent it was meant for.
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
//   - check INBOX.md in your phase dir at start, between steps, and again
//     before finishing, for operator nudges sent after dispatch
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
	// WIPSnapshotPath is the path to a captured uncommitted-work patch from
	// the previous attempt (koryph-77r.10, worktree.PatchSnapshot via
	// engine's refreshWorktreeForRequeue), when one exists. Non-empty →
	// RESUMING block cites it (independent of ResumeSHA: a zero-commit
	// budget-kill requeue has WIP worth restoring but no committed SHA to
	// resume from).
	WIPSnapshotPath string
	ReviewPath      string // non-empty → blocking review findings to address
	PhaseDir        string // where status.json / SUMMARY.md / INBOX.md live
	SummaryPath     string
	StatusPath      string
	LogPath         string
}
