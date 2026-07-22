<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Work: beads and the ready-graph

*This page expands the [Concepts overview](index.md). See
[Running waves](../user-guide/running-waves.md) for the commands that operate
it.*

## The idea

A fleet of autonomous agents needs one authoritative answer to a single
question: *what should be worked on right now?* If that answer lives in a
human's head, or in a markdown checklist, or in a tracker that only the
orchestrator can read, then agents cannot coordinate — they duplicate work,
step on each other, or sit idle waiting to be told.

koryph does not maintain its own task tracker. Work lives in
[beads](https://github.com/gastownhall/beads): every task is an issue — a
"bead" — in a per-project database that travels with the repo through its git
remote. Every collaborator, human or agent, reads and writes the same graph.
There is no second source of truth to keep in sync.

Beads declare **dependencies** on each other. The subset of beads whose
dependencies are all satisfied is the **ready-graph**: the frontier of work
that could legitimately start this instant. That frontier — not a human
dispatcher — is what feeds the scheduler. Everything upstream of the frontier
is blocked by definition; everything on it is fair game.

## In koryph

Work is inspected and claimed with `bd`, never with a side-channel TODO list:

```bash
bd ready              # the frontier: beads whose dependencies are all closed
bd show koryph-oji.6  # inspect one bead — type, labels, deps, body
bd update --claim     # take a bead so no one else picks it up
bd close koryph-oji.6 # mark it done, unblocking its dependents
```

Not every bead is dispatchable. Only `task`, `bug`, and `chore` beads are
handed to agents; container types (`epic`, `feature`, `decision`,
`merge-request`) organize work but never dispatch. Beads labeled
`refactor-core` are deliberately excluded from the loop — the orchestrating
session authors those on `main`, a self-hosting safety rule. So the effective
ready-graph is: *closed-dependency beads, of a dispatchable type, not
otherwise deferred.*

When you finish a wave, you file follow-ups as beads rather than leaving them
in a comment or a chat message — the graph is the only place the next agent
will look.

## The failure mode it prevents

Two trackers always drift. The moment "the plan" lives both in an issue
tracker and in an agent's scratchpad, they disagree, and an agent confidently
works from a stale copy — reimplementing something already merged, or starting
a task whose prerequisite was abandoned. By making the shared, dependency-aware
graph the *only* source of ready work, koryph removes the drift entirely: an
agent cannot start a bead whose dependencies are open, because such a bead is
not on the frontier it was handed.

## Epic lifecycle

An *epic* is a container bead that organizes a cluster of `task`, `bug`, and
`chore` children. Epics themselves never dispatch; the loop works through
their children. When the last child closes, the loop runs a
**whole-epic validation** — a two-lens review of everything that shipped
under the epic:

1. **Completeness** — did the union meet the epic's design goals in letter
   and spirit? Gaps become new child beads; the loop dispatches them as a
   follow-up round.
2. **Structural health** — code duplication, dependency violations, and
   library-shaped code stranded in leaf packages that only emerge when the
   whole epic is visible. Findings become standalone `validation:structural`
   beads that never block the epic.

After a passing verdict the engine files a `validation:docs` child bead so
documentation is written against the settled implementation. When that bead
closes (or immediately, if `docs_update` is disabled), the epic closes.
Epics that exhaust `max_rounds` without a passing verdict are labeled
`validation:parked` for operator triage. Label an epic `no-validate` to
skip validation and the docs stage entirely.

See [Epic validation](../user-guide/epic-validation.md) for the full
lifecycle diagram, configuration reference, and the `koryph epic`
backfill command.

## Operate it

- [Running waves](../user-guide/running-waves.md) — dispatching from the
  frontier.
- The planning skills `/koryph-plan`, `/koryph-import`, and `/koryph-replan`
  turn designs and prompts into beads shaped for dispatch — typed, scoped,
  [footprinted](footprints.md), and dependency-linked.
