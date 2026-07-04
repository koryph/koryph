<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Time: rolling dispatch

*This page expands the [Concepts overview](index.md). See
[Running waves](../user-guide/running-waves.md) for the commands that operate
it.*

## The idea

The naive way to run a fleet is in **waves**: dispatch N tasks, wait for all N
to finish and merge, then scan again and dispatch the next N. It is simple, and
it wastes most of the fleet's time. A wave moves at the speed of its slowest
member — four agents that finish in two minutes sit idle while the fifth grinds
for twenty. Worse, work that *becomes* eligible mid-wave (a dependency closes,
a footprint frees) has to wait for the whole batch to drain before it can start.

koryph's default is **rolling dispatch**. There is no batch barrier. As each
agent finishes and its branch merges, the scheduler immediately re-scans the
[ready-graph](beads.md), re-checks [footprint](footprints.md) conflict-freedom,
and fills the freed slot with the next eligible bead. The fleet stays saturated
right up to its width cap, and newly-eligible work enters the moment it is
safe — without ever violating footprint safety.

## In koryph

The loop is `koryph run`:

```bash
koryph run --project koryph --once --auto-merge --review
```

- `--project <id>` — which project's ready-graph to drain.
- `--once` — run a single pass and exit (the self-build invocation).
- `--auto-merge` — land gate-green branches automatically.
- `--review` — add a post-implementation review pass before merge.
- `--dispatch-mode wave|rolling` — force a mode; rolling is the default.
- `--max N` — cap concurrent width.
- `--dry-run` — plan and print the dispatch set without launching agents.

In `rolling` mode a freed slot triggers an immediate re-scan; in `wave` mode
the scheduler waits for the whole batch. Both honor the same conflict rules and
the same width cap — rolling simply removes the idle time between a fast agent
finishing and the next bead starting.

## The failure mode it prevents

Head-of-line blocking. Under waves, one long-running task holds the entire
fleet hostage: throughput collapses to the slowest member per batch, and a
freshly-unblocked high-priority bead can wait a full wave for a slot that was
sitting empty the whole time. Rolling dispatch keeps every slot productive and
lets the ready-graph flow continuously, so total wall-clock tracks the *sum of
work divided by width* rather than *the slowest task times the number of
batches*.

## Operate it

- [Running waves](../user-guide/running-waves.md) — the dispatch-mode section
  contrasts wave and rolling on real runs.
- The loop lives in the `internal/engine` package (`engine.Run`, `Options`);
  selection and conflict-checking are in `internal/sched`.
- What flows through the slots: [beads](beads.md) off the ready-graph, filtered
  by [footprints](footprints.md), landed through the
  [worktree + green-gate](worktrees.md) pipeline, paced by the
  [governors](governors.md).
