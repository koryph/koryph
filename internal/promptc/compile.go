// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package promptc

import (
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/sched"
)

// sectionSep joins the three prompt sections. A reader sees a "---" rule
// between the engine preamble, the project block, and the volatile tail.
const sectionSep = "\n---\n"

// Compile renders the dispatch prompt as exactly three sections — engine
// preamble, project block, volatile tail — joined by sectionSep. The output
// is deterministic: the same Input always produces byte-identical bytes (no
// maps are iterated), and no timestamps appear in sections [1] or [2].
func Compile(in Input) string {
	return strings.Join([]string{
		Preamble(in.EngineVersion),
		projectBlock(in),
		volatileTail(in),
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

// preambleBody is the fixed contract text. It carries no timestamp or
// per-dispatch value so it stays byte-identical (and cacheable) across every
// dispatch of one engine version.
const preambleBody = `You are a Koryph subagent. You work autonomously inside one git worktree,
on one branch, and you report progress through a small set of files. This
contract is identical for every dispatch of this engine version.

## Boundary (also hook-enforced)
- Work ONLY inside your assigned worktree, on your assigned branch. Never
  touch another worktree or the primary checkout.
- FORBIDDEN operations — the koryph performs these, never you:
    - git checkout main
    - git merge
    - git push
    - bd close
    - gh pr merge
  The koryph merges and closes your work; do not integrate it yourself.
- Sign off every commit (git commit -s): the DCO sign-off trailer is
  required; unsigned-off commits are rejected by the merge gate and CI.
- If your change adds or alters user-visible behavior, update the relevant
  docs/ chapter in the same branch, and name the docs you touched in your
  SUMMARY (or state "no user-visible surface").
- Commit early and often: your commits are your only checkpoints. Uncommitted
  work is invisible to recovery and may be lost.

## When you are blocked
bd ready unconditionally excludes in_progress issues — that is by design,
so your claimed bead is never handed to a second agent. But it also means
that if you leave this bead in_progress and simply walk away, NOTHING will
ever re-check it: there is no event, no re-scan, no expiry. Before you exit
a task you cannot finish, do ONE of the following — never just leave it
in_progress with a note and stop:

- Blocked on another bead that will eventually close: wire a real
  dependency edge, then release your claim so bd ready's dependency engine
  re-surfaces this bead automatically once that bead closes —
      bd dep add <this-id> --blocked-by <blocker-id>
      bd update <this-id> --status open
- Blocked on something no bead represents (an operator action, an
  unscoped future decision): in_progress is not a "don't touch" signal.
  Label the bead no-dispatch, reset it to open the same way, and explain
  the blocker in a note so it stays visible and accurately tracked while
  the label alone keeps it out of dispatch:
      bd update <this-id> --add-label no-dispatch
      bd update <this-id> --status open
      bd update <this-id> --append-notes "why this is blocked"

## Heartbeat and reporting
- After each step, write a JSON heartbeat to $KORYPH_STATUS_PATH: an object
  with exactly these keys: {"state","step","pct"}.
- Append human-readable progress lines to $KORYPH_LOG_PATH as you work.
- Before you finish, write your summary to $KORYPH_SUMMARY_PATH (SUMMARY.md)
  with these sections, in this order:
    - What shipped
    - Stubs shipped
    - Follow-ups
    - Test evidence
    - Changes requiring orchestrator review
- Read INBOX.md in your phase directory when you start, between every step,
  and again right before you finish: a nudge appended right after dispatch
  (before your first heartbeat is even polled) is otherwise invisible until
  your next check-in, and one appended near the end can still change what
  "done" means.

## Output economy
Gate and Bash output dominate transcript bytes; keep them small:

- Prefer "make gate-agent" over "make gate". It runs identical checks with
  the same fail-fast verdict, but prints one PASS/FAIL line per stage and
  tees each stage's full log to $KORYPH_PHASE_DIR/gate-<stage>.log. On
  failure it also prints a short tail so the actionable error still reaches
  you; the full output is always recoverable via the Read tool.
- File-spill wrappers: for any long-running command, invoke
  hooks/koryph-spill.sh with a label and the command. The wrapper prints a
  head+tail summary, writes the full untruncated output to a file under your
  phase dir, and ends its summary with "full output: <path>". Recover the
  complete output at any time with the Read tool against that path.
- Keep your own replies concise: summaries, status lines, and code snippets;
  skip prose narration. Long output belongs in a file, not in your response.`

// projectBlock returns section [2]: stable per project. Conventions, the
// green gate, and optional cross-cutting gates and bootstrap notes. No
// timestamps; iteration order follows the input slices.
func projectBlock(in Input) string {
	var b strings.Builder
	b.WriteString("## Project: ")
	b.WriteString(in.ProjectName)

	if strings.TrimSpace(in.Conventions) != "" {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(in.Conventions, "\n"))
	}

	if in.CommitStyle == "custom" && strings.TrimSpace(in.CommitTemplate) != "" {
		b.WriteString("\n\nCommit style: follow this project template exactly:\n")
		b.WriteString(strings.TrimRight(in.CommitTemplate, "\n"))
	} else {
		b.WriteString("\n\nCommit style: Conventional Commits — `type(scope): subject` ")
		b.WriteString("(feat|fix|docs|chore|refactor|revert|test|ci|build|perf|style; imperative, lowercase, <=72 chars).")
	}

	b.WriteString("\n\nGreen gate (keep these green):")
	if len(in.Gate) == 0 {
		b.WriteString("\n- (none configured)")
	} else {
		writeBullets(&b, in.Gate)
	}

	if len(in.CrossGates) > 0 {
		b.WriteString("\n\nCross-cutting gates:")
		writeBullets(&b, in.CrossGates)
	}

	if len(in.Bootstrap) > 0 {
		b.WriteString("\n\nWorktree bootstrap (already run for you, rerun if needed):")
		writeBullets(&b, in.Bootstrap)
	}

	return b.String()
}

// volatileTail returns section [3]: the per-dispatch content — bead,
// execution plan, resume/review context, and the reporting paths.
func volatileTail(in Input) string {
	var b strings.Builder
	b.WriteString("## Task ")
	b.WriteString(in.Bead.ID)
	b.WriteString(": ")
	b.WriteString(in.Bead.Title)

	if strings.TrimSpace(in.Bead.Description) != "" {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(in.Bead.Description, "\n"))
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

	b.WriteString("\n\n### Reporting paths")
	b.WriteString("\n- Phase dir: ")
	b.WriteString(in.PhaseDir)
	b.WriteString("\n- Summary:   ")
	b.WriteString(in.SummaryPath)
	b.WriteString("\n- Status:    ")
	b.WriteString(in.StatusPath)
	b.WriteString("\n- Log:       ")
	b.WriteString(in.LogPath)
	b.WriteString("\n- Inbox:     ")
	b.WriteString(inboxPath(in.PhaseDir))

	return b.String()
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
