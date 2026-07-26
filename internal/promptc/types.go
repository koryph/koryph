// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package promptc compiles dispatch prompts with cache-stable ordering and
// named semantic clauses:
//
//	[1] engine preamble — worktree boundary, phase protocol, result command
//	[2] project block   — canonical repository contract when the runtime does
//	                      not load it natively, plus stable project context
//	[3] volatile tail   — Bead contract, prior findings, resources, focused
//	                      tests, and the one phase inbox
//
// OPERATOR NOTES (koryph-o72): Bead.Notes carries any addendum appended via
// `bd update --append-notes` while the bead was still queued — i.e. before
// any agent existed to read an INBOX.md. It is rendered as its own
// clearly-delimited section whenever non-empty (compile.go's volatileTail),
// so a pre-dispatch nudge is guaranteed to reach the agent it was meant for.
//
// Implementation contract (compile.go):
//   - Compile(Input) string — deterministic, no timestamps inside sections
//     [1]/[2] (cache stability), volatile content only in [3].
package promptc

import "github.com/koryph/koryph/internal/beads"

// Input is everything the compiler may use.
type Input struct {
	EngineVersion      string
	ProjectName        string
	RepositoryContract string // canonical AGENTS.md; empty only when loaded natively
	Bootstrap          []string
	Bead               beads.Issue
	PlanYAML           string // extracted koryph-plan block, if any
	ResumeSHA          string // non-empty → RESUMING block
	// WIPSnapshotPath is the path to a captured uncommitted-work patch from
	// the previous attempt (koryph-77r.10, worktree.PatchSnapshot via
	// engine's refreshWorktreeForRequeue), when one exists. Non-empty →
	// RESUMING block cites it (independent of ResumeSHA: a zero-commit
	// budget-kill requeue has WIP worth restoring but no committed SHA to
	// resume from).
	WIPSnapshotPath string
	ReviewPath      string // non-empty → blocking review findings to address
	// RepairPath is authoritative validation evidence from a prior failed
	// attempt. It is distinct from a review: a gate failure must tell the
	// repair worker exactly which validation result it needs to correct.
	RepairPath  string // non-empty → required validation repair evidence
	PhaseDir    string // where status.json / SUMMARY.md / INBOX.md live
	SummaryPath string
	StatusPath  string
	LogPath     string
}
