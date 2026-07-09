<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Parallelism: footprints

*This page expands the [Concepts overview](index.md). See
[Running waves](../user-guide/running-waves.md) for the commands that operate
it.*

## The idea

Two agents editing the same file produce a merge conflict. You cannot detect
that after the fact and call it safe — by then two branches have diverged and
someone has to reconcile them by hand. The only way to run many agents at once
*without* gambling on conflicts is to know, **before dispatch**, what each task
will touch.

Every bead carries a **footprint**: a set of labels naming the areas of the
codebase it reads and writes. The scheduler treats those labels like a
reader–writer lock over the repo. Two beads **conflict** when they share a
token and at least one of them holds it as a *write*; two readers of the same
token co-run happily. The scheduler only ever dispatches a set of beads that
are mutually conflict-free. That is what turns "run five agents at once" from
reckless into safe.

## In koryph

Footprint tokens come from a bead's labels, resolved in
`internal/sched/footprint.go`:

- `fp:read:<token>` — a **read** of that token. Readers never exclude each
  other. Example: `fp:read:go:signing`.
- `fp:<token>` — a plain **write** of that token.
- `area:<name>` — expands through the project's `area_map` (in
  `koryph.project.json`) into one or more write tokens. koryph ships
  per-package areas: `area:sched`, `area:quota`, `area:dispatch`,
  `area:ledger`, `area:govern`, `area:merge`, `area:review`, `area:worktree`,
  `area:beads`, `area:registry`, and `area:engine` for the wave-loop package
  itself.
- **No footprint label** → the catch-all write token `domain:unknown`, which
  every other unlabeled bead also holds as a write. They all conflict with each
  other and serialize.

So a bead that writes the scheduler and only reads the signing helpers is
labeled:

```
area:sched  fp:read:go:signing
```

It co-runs with anything that merely reads `sched`, and with anything that
writes an unrelated area — but it excludes any *other* writer of `sched`.

Choose the **narrowest honest area**. Over-broad labeling costs only
parallelism (beads needlessly serialize); under-broad labeling risks a
false-parallel merge conflict, which is far worse. Read-only touches should use
`fp:read:<token>` so they never block writers unnecessarily.

## The failure mode it prevents

The silent serializer is `domain:unknown`. An unlabeled bead conflicts with
every *other* unlabeled bead, so a wave that should have fanned out to five
agents instead trickles through one at a time — and the scheduler looks
"slow" for no visible reason. The louder failure is a false parallel: two beads
that both really write the same file but were labeled as if they didn't, merged
independently, colliding at integration time. Honest footprints are precisely
the price of safe concurrency: label what you touch, and the scheduler does the
rest.

## Operate it

- [Running waves](../user-guide/running-waves.md) — the footprint-labels
  section shows `fp:*` and `area:*` on real beads.
- Mechanics live in `internal/sched/footprint.go` (token derivation) and
  `internal/sched/wave.go` (conflict-free selection).
- Footprints feed [rolling dispatch](rolling-dispatch.md): the scheduler
  re-checks conflict-freedom every time a slot frees.
- Footprints protect the merge; [resources](resources.md) protect the
  machine — a second, additive admission dimension for beads that provision
  external dependencies (dev clusters, docker stacks, servers).
