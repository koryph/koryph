// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package promptc

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/sched"
)

// sectionSep joins the three prompt sections. A reader sees a "---" rule
// between the engine preamble, the project block, and the volatile tail.
const sectionSep = "\n---\n"

const (
	RepositoryClauseMarker   = "<!-- koryph-clause:repository/v1 -->"
	RoleClauseMarker         = "<!-- koryph-clause:role/v1 -->"
	EngineClauseMarker       = "<!-- koryph-clause:engine/v1 -->"
	ProjectContextMarker     = "<!-- koryph-clause:project-context/v1 -->"
	TaskClauseMarker         = "<!-- koryph-clause:task/v1 -->"
	semanticClauseMarkerOpen = "<!-- koryph-clause:"
)

// Compile renders the dispatch prompt as exactly three sections — engine
// preamble, project block, volatile tail — joined by sectionSep. The output
// is deterministic: the same Input always produces byte-identical bytes (no
// maps are iterated), and no timestamps appear in sections [1] or [2].
func Compile(in Input) string {
	return SharedPrefix(in) + sectionSep + volatileTail(in)
}

// SharedPrefix returns the byte-identical cacheable prefix — sections
// [1]preamble + [2]project-block joined by sectionSep — that is stable across
// every bead in a project: it depends only on in.EngineVersion and the
// project-block fields (name, conventions, gate, bootstrap), never on in.Bead
// or any per-dispatch field. By construction Compile(in) ==
// SharedPrefix(in) + sectionSep + <volatile tail>, so the shared prefix is
// exactly the span koryph places a prompt-cache breakpoint after (registry
// prompt_cache_policy): a cached SharedPrefix is a warm read for the first
// turn of every bead's dispatch that shares it (koryph-6au). It is the API
// that lets koryph OWN the breakpoint on the request paths it controls (the
// Batch path); the wave-loop `claude -p` dispatch manages its own cache TTL
// and does not consume this.
func SharedPrefix(in Input) string {
	return strings.Join([]string{
		Preamble(in.EngineVersion),
		projectBlock(in),
	}, sectionSep)
}

// Preamble returns section [1]: the engine-stable agent boundary and
// reporting contract. It depends ONLY on engineVersion — no timestamps and no
// per-dispatch content — so the engine can hash it for cache-stability tests.
func Preamble(engineVersion string) string {
	var b strings.Builder
	b.WriteString("# Koryph dispatch (engine ")
	b.WriteString(engineVersion)
	b.WriteString(")\n\n")
	b.WriteString(preambleBody)
	return b.String()
}

// preambleBody is the one engine-owned semantic clause. Repository rules live
// in AGENTS.md, role behavior lives in the persona, and task/focused-test
// scope lives in the volatile tail. Keeping those owners disjoint prevents a
// stronger but stale copy from silently overriding the current contract.
const preambleBody = EngineClauseMarker + `
You are a Koryph subagent operating in one assigned worktree and phase.

## Engine boundary
- Work only inside the assigned worktree and branch. Never touch another
  worktree or the primary checkout.
- The orchestrator alone may run git checkout main, git merge, git push,
  bd close, or gh pr merge. Do not integrate the branch yourself.
- Commit coherent checkpoints; uncommitted work is not recoverable.
- Do not mutate shared Beads state. Request a missing scheduling declaration
  only for this bead with:
    koryph phase request label-add --label area:<value>
    koryph phase request label-add --label fp:<value>
    koryph phase request label-add --label res:<value>

## Phase protocol
- Write {"state","step","pct"} heartbeats to $KORYPH_STATUS_PATH and concise
  progress lines to $KORYPH_LOG_PATH.
- Write $KORYPH_SUMMARY_PATH with: What shipped, Stubs shipped, Follow-ups,
  Test evidence, and Changes requiring orchestrator review.
- A validation command is required only when the task names that exact command
  or it is the narrowest check that covers an acceptance criterion. A broader
  package or repository probe is optional when targeted checks cover the
  criterion; unrelated failures from that probe are not a task capability
  block. Do not report phase block when the required targeted checks pass.
- A sandbox, credential, tool, network, or host-resource failure becomes
  terminal only after the required command exits nonzero. Report it with:
    koryph phase block --capability <lowercase-token> --detail "sanitized condition"
  Warnings and intermediate stderr are not terminal capability evidence.
- status.json and SUMMARY.md are advisory. Only the following command may
  create terminal success:
    koryph phase complete --evidence "$KORYPH_PHASE_DIR/completion-evidence.json"
  The evidence must contain successful focused_tests and exactly one
  acceptance entry per AC<n>, each with a regular file or focused-test
  reference. Never hand-write result.json.`

// projectBlock returns section [2]: the canonical repository clause when the
// runtime does not load AGENTS.md natively, followed by stable non-policy
// project context. Gate commands are validation-service evidence and are
// intentionally never rendered into a worker prompt.
func projectBlock(in Input) string {
	var b strings.Builder
	if strings.TrimSpace(in.RepositoryContract) != "" {
		b.WriteString(strings.TrimSpace(in.RepositoryContract))
		b.WriteString("\n\n")
	}
	b.WriteString(ProjectContextMarker)
	b.WriteString("\n")
	b.WriteString("## Project: ")
	b.WriteString(in.ProjectName)

	if len(in.Bootstrap) > 0 {
		b.WriteString("\n\nWorktree bootstrap already completed; rerun only a focused prerequisite if needed:")
		writeBullets(&b, in.Bootstrap)
	}

	return b.String()
}

// WithCompletionRepair appends the only instructions allowed for the bounded
// completion-contract repair. The implementation is already committed; this
// dispatch may construct evidence and the terminal result, but may not reopen
// source work or broaden validation.
func WithCompletionRepair(prompt, phaseDir string) string {
	evidencePath := filepath.Join(phaseDir, "completion-evidence.json")
	return prompt + "\n\n### COMPLETION REPAIR ONLY\n" +
		"The committed implementation is complete. Do not edit source files, rewrite commits, " +
		"change scope, or run the full project gate. Inspect existing focused-test logs; run only " +
		"a missing focused check needed to authenticate an acceptance reference. Write the exact " +
		"AC<n> evidence matrix to " + evidencePath + " and finish only with:\n    " +
		"koryph phase complete --evidence " + evidencePath + "\n" +
		"If that command cannot validate the existing candidate, report the concrete capability " +
		"block or exit without modifying implementation."
}

// volatileTail returns section [3]: the per-dispatch content — bead,
// execution plan, resume/review context, and the reporting paths.
func volatileTail(in Input) string {
	var b strings.Builder
	b.WriteString(TaskClauseMarker)
	b.WriteString("\n")
	b.WriteString("## Task ")
	b.WriteString(in.Bead.ID)
	b.WriteString(": ")
	b.WriteString(in.Bead.Title)

	if strings.TrimSpace(in.Bead.Description) != "" {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(in.Bead.Description, "\n"))
	}
	if strings.TrimSpace(in.Bead.AcceptanceCriteria) != "" {
		b.WriteString("\n\n### Acceptance criteria\n")
		b.WriteString(strings.TrimRight(in.Bead.AcceptanceCriteria, "\n"))
	}

	// OPERATOR NOTES (koryph-o72): an addendum sent via `bd update
	// --append-notes` while this bead was still queued — before any agent
	// was dispatched to see it. Notes are operator guidance by construction
	// (nothing else writes bd's notes field), so they are rendered as
	// binding scope, clearly delimited from the bead's original description
	// above so a reader can tell what was filed vs. what was added later.
	if strings.TrimSpace(in.Bead.Notes) != "" {
		b.WriteString("\n\n### OPERATOR NOTES\n")
		b.WriteString("Added after this bead was filed — treat as required scope, not optional:\n\n")
		b.WriteString(strings.TrimRight(in.Bead.Notes, "\n"))
	}

	if strings.TrimSpace(in.PlanYAML) != "" {
		b.WriteString("\n\n### Execution plan (koryph-plan)\n```yaml\n")
		b.WriteString(strings.TrimRight(in.PlanYAML, "\n"))
		b.WriteString("\n```")
	}

	if in.ResumeSHA != "" || in.WIPSnapshotPath != "" {
		b.WriteString("\n\n### RESUMING\n")
		if in.ResumeSHA != "" {
			b.WriteString("This task resumes from committed work at ")
			b.WriteString(in.ResumeSHA)
			b.WriteString(". Inspect what already landed:\n    git log --oneline ")
			b.WriteString(in.ResumeSHA)
			b.WriteString("..HEAD\n")
			b.WriteString("Do NOT redo work that is already committed. Read the manifest's ")
			b.WriteString("next_action and continue from there.")
		}
		if in.WIPSnapshotPath != "" {
			if in.ResumeSHA != "" {
				b.WriteString("\n\n")
			}
			b.WriteString("A previous attempt's uncommitted work was snapshotted (git diff format) to ")
			b.WriteString(in.WIPSnapshotPath)
			b.WriteString(" before this worktree was possibly rebuilt. Check your working tree first (git status): ")
			b.WriteString("if it already carries those changes, they need no action; if not, read the snapshot and ")
			b.WriteString("apply what is still relevant (git apply ")
			b.WriteString(in.WIPSnapshotPath)
			b.WriteString(") rather than redoing the exploration from scratch. Either way, commit as you go.")
		}
	}

	if in.ReviewPath != "" {
		b.WriteString("\n\n### Blocking review findings\n")
		b.WriteString("A prior review left blocking findings at ")
		b.WriteString(in.ReviewPath)
		b.WriteString(". Read that file and resolve every finding before you finish.")
	}

	writeResourcesBlock(&b, in.Bead)

	b.WriteString("\n\n### Focused test scope")
	b.WriteString("\nRun only checks scoped to the changed packages and acceptance criteria. ")
	b.WriteString("The Koryph validation service owns broad repository validation. ")
	b.WriteString("Capture each successful command and its regular log path in completion evidence.")

	b.WriteString("\n\n### Reporting paths")
	b.WriteString("\n- Phase dir: ")
	b.WriteString(in.PhaseDir)
	b.WriteString("\n- Summary:   ")
	b.WriteString(in.SummaryPath)
	b.WriteString("\n- Status:    ")
	b.WriteString(in.StatusPath)
	b.WriteString("\n- Log:       ")
	b.WriteString(in.LogPath)
	b.WriteString("\n- Inbox (read at start, between steps, and immediately before completion): ")
	b.WriteString(inboxPath(in.PhaseDir))

	return b.String()
}

// ValidateRepositoryContract authenticates the single canonical repository
// semantic owner before a dispatch prompt is assembled. Unknown or duplicate
// clause markers fail closed so a stale projection cannot silently become a
// second policy owner.
func ValidateRepositoryContract(contract string) error {
	return validateClauseOwners(contract, [5]int{1, 0, 0, 0, 0})
}

// ValidateRoleContract authenticates a persona as role behavior only.
func ValidateRoleContract(contract string) error {
	return validateClauseOwners(contract, [5]int{0, 1, 0, 0, 0})
}

// ValidateDispatchContract checks the fully compiled worker prompt. The
// repository clause is present exactly once only for runtimes that do not
// declare native AGENTS.md loading; role behavior is assembled separately.
func ValidateDispatchContract(prompt string, includesRepository bool) error {
	repositoryCount := 0
	if includesRepository {
		repositoryCount = 1
	}
	return validateClauseOwners(prompt, [5]int{repositoryCount, 0, 1, 1, 1})
}

func validateClauseOwners(text string, wants [5]int) error {
	markers := [...]string{
		RepositoryClauseMarker,
		RoleClauseMarker,
		EngineClauseMarker,
		ProjectContextMarker,
		TaskClauseMarker,
	}
	total := 0
	for i, marker := range markers {
		got := strings.Count(text, marker)
		if got != wants[i] {
			return fmt.Errorf("semantic clause %q count %d, want %d", marker, got, wants[i])
		}
		total += got
	}
	if got := strings.Count(text, semanticClauseMarkerOpen); got != total {
		return fmt.Errorf("semantic clause marker count %d exceeds %d recognized owners", got, total)
	}
	return nil
}

// writeResourcesBlock appends the RESOURCES section of the volatile tail
// (koryph-4ql.4, design docs/designs/2026-07-resource-governor.md L6 "Agent
// contract") — the runtime-provisioning half of the agent contract that
// mirrors L1's declaration half (sched.ResourcesFor / res:<kind> labels).
// It writes nothing when the bead declares no res:<kind> labels: zero output
// change for the common (undeclared) case is the point, so every existing
// golden/substring test on an undeclared bead keeps passing unmodified
// (pinned by TestResourcesBlockAbsentWithoutLabels).
//
// sched.ResourcesFor is the single source of truth for the declared kinds —
// same LabelValues("res:") + [a-z0-9-]+ + dedupe-sort mechanics BuildWave and
// Acquire use (design L1/L4) — so promptc never re-implements the label
// grammar and can never drift from what the scheduler/governor actually
// admitted against.
//
// The five directives below are the design's "Agent contract" paragraph
// verbatim: declared kinds, provision-at-most-declared, the
// <kind>-<bead-id> naming convention leak detection (L7) keys off, teardown
// before exit (including a SIGTERM checkpoint — the engine's requeue path
// commits and requeues on SIGTERM, but the running process is still the only
// thing that can tear down what it provisioned), and reporting anything left
// behind in SUMMARY.md so a leaked instance is at least self-attributed.
func writeResourcesBlock(b *strings.Builder, bead beads.Issue) {
	kinds := sched.ResourcesFor(bead)
	if len(kinds) == 0 {
		return
	}
	b.WriteString("\n\n### RESOURCES\n")
	b.WriteString("This task declares external resource kind(s): ")
	b.WriteString(strings.Join(kinds, ", "))
	b.WriteString(".")
	writeBullets(b, []string{
		"Provision at most what you declared — no other resource kinds.",
		"Name every instance <kind>-" + bead.ID + " (e.g. " + kinds[0] + "-" + bead.ID +
			") so leak detection can attribute it to this task.",
		"Tear everything down before you exit, including when checkpointing on SIGTERM.",
		"List anything you could not tear down in SUMMARY.md.",
	})
}

// writeBullets appends "- <item>" lines for each item.
func writeBullets(b *strings.Builder, items []string) {
	for _, it := range items {
		b.WriteString("\n- ")
		b.WriteString(it)
	}
}

// inboxPath is the INBOX.md path inside the phase directory.
func inboxPath(phaseDir string) string {
	if phaseDir == "" {
		return "INBOX.md"
	}
	return filepath.Join(phaseDir, "INBOX.md")
}
