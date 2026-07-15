<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Autonomous recovery: closing the operator-intervention gaps found in stampede-games production (2026-07-15)

Status: designed 2026-07-15 (orchestrator-built inline; refactor-core engine +
merge paths authored on main). Beads at the end of this doc.

Origin: a live `koryph run --project stampede-games --review --auto-merge`
production session named every point where an **external monitoring thread had
to intervene to correct a failure koryph could not resolve on its own**. Four
incidents, each reproduced live, not speculated. The unifying defect is not any
single bug — it is that koryph's failure paths *hand the problem to a human*
where they could either self-heal or, at minimum, refuse to make things worse.

## 1. Problem — where the human had to step in

| # | koryph's failure | The human had to | Cited root cause |
|---|---|---|---|
| F1 | A phase `koryph stop` had SIGTERM'd was auto-requeued as a new attempt; it began working a bead the operator had **already hand-implemented and `bd close`d**, a genuine file/merge collision. | Notice and force-kill the redispatched agent. | `completeSlot` (`internal/engine/poll.go:392`) cannot distinguish an operator SIGTERM from a crash — both land in "agent died with no commits" (`poll.go:516`) → `requeueSlot` (`poll.go:540`). `Slot.DeathReason` is stamped only for budget-kills. And `drain` is gated **only** at the dispatch boundary (`wave.go:182/413`); the three requeue functions (`poll.go:1487/611/768`) have no drain check, so a retry escapes drain. |
| F2 | A phase blocked on a protected path (`Makefile`) reported *what* was touched but not *how to resolve it*. | Reverse-engineer `koryph merge --allow-protected` from `--help`, then hand-run the full rebase→gate→merge→push→close→cleanup (~8 commands). | `poll.go:1261` writes `s.Note = "protected paths touched: " + …` with no resolution hint. The liftable subset (`.github/`, `Makefile`) is known (`internal/merge/types.go:67`) but never surfaced. |
| F3 | A `revert(...)` commit reached a branch; koryph's merge-gate caught the non-conventional subject but only **requeued once, then blocked** (`poll.go:1219`). | Hand-rewrite git history (detach-cherry-pick-reword). | `revert` — a standard Conventional Commits type — is absent from the hardcoded allowlist (`internal/merge/commitstyle.go:16`). The bad commit reached the branch because the project's *own* local commit-msg hook is bypassable by a compound `cd … && git commit` (template-owned, §5). |
| F4 | (none — flagged for cross-check) | (fixed downstream in stampede-games) | koryph invokes `bd remember` via direct argv (`internal/beads/adapter.go:304` → `a.run(ctx, "remember", text)`, no shell) and ships **no** double-quoted `bd remember "…"` example. Not vulnerable; **no koryph change.** |

## 2. F1 — operator-stop must never auto-retry, and drain must cover retries

Two independent, stackable fixes (the handoff's own recommendation).

**F1a — operator-stop is a terminal intent, not a death to retry.** `koryph stop`
(`cmd/koryph/ops.go:398`, with or without `--force`) currently only signals the
PID (`internal/dispatch/cli.go:417`). It must also *record the intent on the
slot* before signalling, so the engine's next `completeSlot` poll classifies the
resulting exit as operator-stopped and **parks** it (a terminal, non-retry
state with a clear reason: "operator-stopped; re-dispatch explicitly") rather
than requeueing. This closes the race at the source — it does not matter whether
the operator's `bd close` has landed yet, because the stop itself suppresses the
retry. `beadClosedMidFlight` (`poll.go:1493`) remains a second line of defence.

**F1b — drain suppresses *all* new agent starts, including retries.** `drain`'s
contract is "dispatch nothing new," but it is enforced only where fresh work is
pulled from `bd ready` (`wave.go:200/413`). The three requeue functions
(`requeueSlot`, `requeueRateLimited`, `requeueBudgetKilled`) admit a new attempt
with no drain check. Add a drain guard alongside the existing `parkForRunBudget`
guard (`poll.go:1497`) so a death during drain parks instead of requeueing.

F1a is the load-bearing fix (operator-stop should never retry, drain or not);
F1b is defence-in-depth (a crash mid-drain also should not start new work).

## 3. F2 — block messages carry the resolution path

When a phase blocks on protected paths (`poll.go:1261`), partition the touched
set (`res.Protected`) against `merge.LiftableProtected`:

- **All touched paths are liftable** (`.github/`, `Makefile`): append the exact
  sanctioned one-command path — `koryph merge --project <p> <branch>
  --allow-protected --push --close-bead <id> --reason <r>`.
- **Any touched path is unliftable** (governance defaults or project
  `protected_paths`): say so explicitly — "project/governance policy — requires
  manual review; `--allow-protected` will not lift this" — so the operator does
  not waste a flag attempt that will still refuse.

The hint rides in `Slot.Note`, the same field `koryph status` already surfaces
for a blocked phase.

## 4. F3 — remove the false-positive trigger, and point the way out

- **Allow `revert`.** It is a canonical Conventional Commits type; its absence
  from `conventionalTypes` (`commitstyle.go:16`) is the direct cause of the
  incident. Add it, and keep it in sync with what `promptc` promises agents.
- **Surface the reword path on a commit-style block.** Mirror §3: when a phase
  blocks for non-conventional subjects after the single reword requeue, the
  `Slot.Note` should name the offending subject(s) and the resolution (re-dispatch
  to reword, or hand-reword), instead of a bare failure.

## 5. Out of scope / future work (recorded, not filed as beads here)

- **Template commit-msg hook bug (F3 root cause).** stampede-games's
  `.claude/hooks/validate-conventional-commit.sh` anchors its regex to the start
  of the command, so `cd … && git commit …` bypasses it. That hook is owned by
  **claude-project-template**, a different repo — the fix belongs there, not in
  koryph. koryph's merge-gate is the correct backstop and already caught it.
- **`koryph doctor --fix` patching onboarded projects' buggy hooks.** `doctor`
  exists with `--fix/--force` asset remediation (`cmd/koryph/doctor.go:35`), but
  it reconciles *koryph-owned* assets; the buggy hook is template-owned. Cross-
  repo hook patching is a larger policy question — deferred.
- **Auto-reword self-heal for genuinely malformed subjects.** Rewriting agent
  branch history in the merge path (beyond adding `revert`) is a history-mutating
  operation that warrants its own design + sign-off; §4's "allow revert + surface
  the path" removes the actual incident's trigger without it. Deferred.
- **`bd remember` stdin/`--file` intake.** The structurally-complete fix for F4's
  hazard class lives upstream in `bd`; noted for the beads project, no koryph
  change.

## 6. Beads

Filed and implemented inline this session:

- **koryph-a1x** (P0, F1a) operator-stop terminal reason — never auto-retry a stopped phase.
- **koryph-z0x** (P1, F1b) drain suppresses retries, not just fresh dispatch.
- **koryph-zfg** (P2, F2) protected-path block Note surfaces the resolution path.
- **koryph-aw9** (P2, F3) allow `revert`; commit-style block Note surfaces the reword path.
