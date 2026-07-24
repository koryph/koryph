---
name: koryph-plan
description: Convert a design doc into a filed, conflict-aware bead graph — decomposition, footprints, dependencies, parallel-width validation
---
<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Convert a design into a FILED, conflict-aware bead graph: one epic, its
child beads, footprint labels, and dependency edges — ready for the wave
loop to dispatch in parallel without merge conflicts.

Argument (a design doc path, or paste the design inline): $ARGUMENTS

## Model requirements

Steps 2–7 below (decompose, footprint discovery, resource declaration,
dependency wiring, conflict validation, routing) are scheduler-correctness
work: a mislabeled footprint or a missed dependency edge causes a
false-parallel dispatch and a merge conflict downstream, discovered by a
broken build rather than by re-reading the plan. These steps require the
**frontier reasoning tier of your agent runtime** — Claude Opus-class, or
the equivalent top tier of whatever runtime you are (codex, cursor, grok
build, ...).

1. Check what model you are running as (your own system context states it,
   or run `/model`).
2. Frontier tier or better: continue through steps 2–7 yourself.
3. Below your runtime's frontier tier: do **not** attempt steps 2–7
   yourself. Either:
   - tell the operator to re-run `/koryph-plan` on a frontier-tier model, or
   - delegate steps 2–7 wholesale to the `koryph-architect` agent (pinned
     `tier: frontier`, `effort: xhigh` in its own frontmatter), then do only
     the mechanical `bd create`/`bd dep add` calls from its output yourself.
4. Step 1 (ingest) and the mechanical parts of step 8 (running an
   already-decided `bd create`/`bd dep add` command, invoking the scorer,
   printing the step 9 report) are fine at any tier — only the judgment
   calls in 2–7 are gated.

## Do this

1. **Ingest.** Read the design doc (or the inline text in `$ARGUMENTS`).
   Read `project_id` and `area_map` from `koryph.project.json` at the repo
   root. Then run the MANDATORY dedup sweep for every bead you are about to
   create: `bd children <epic-id>` on the target epic (shows ALL states) and
   `bd search "<keywords>"` on scope words from the title. `bd ready` is NOT
   a dedup check — dependency-blocked and deferred beads are invisible
   there, and those are exactly the duplicates that come back to life later
   and collide in flight. If an existing bead overlaps, update or unblock it
   instead of creating a new one.

2. **Decompose seam-first.** When a design fans out to N sibling beads,
   the FOUNDATION bead must ship the extension seam — a registry or
   file-per-unit structure — so siblings ADD files and never EDIT shared
   ones; label siblings with per-unit write tokens plus `fp:read:` on the
   shared shell. Registration hubs (command tables, nav blocks, tab bars,
   config schemas, go.mod for dep-adding siblings) are the recurring
   serializers: if the epic adds Go dependencies, the foundation bead adds
   them all, once. Review question before filing: "when all N siblings are
   done, which files did more than one of them edit?" — the answer must be
   empty. Then decompose.

2b. **Decompose.** One bead per single-agent-sized unit of work, typed
   `task`/`bug`/`chore` — the **only** types the wave loop dispatches. Use
   `epic` only as the umbrella parent; the loop skips it. Every child bead's
   `--description` must stand alone (the loop's agents see only the bead
   text, never the design doc) and state:
   - **why** the work exists,
   - **acceptance criteria** ("done" looks like X),
   - a pointer back to the design doc path/section it came from.

3. **Discover footprints — do not guess.** For every bead, enumerate the
   concrete files/packages/dirs it will touch by *inspecting the
   repository*: grep for the symbols the design names, follow imports and
   callers, read the code the bead will change. Then label:
   - one `area:<key>` per `area_map` key touched, narrowest-honest — carry
     every area the bead actually touches (over-broad only costs
     parallelism; under-broad risks a false-parallel merge conflict);
   - `fp:read:<token>` for a **read-only** touch (docs about a package,
     tests over its fixtures, analysis) — readers of a token co-run with
     each other; a writer of that token excludes readers AND other writers
     (a shared token conflicts whenever at least one side writes it);
   - explicit `fp:<token>` (a write token) where the `area_map` can't
     express the footprint;
   - unlabeled only when genuinely unavoidable — say so explicitly, and
     note it lands in the catch-all `domain:unknown` write token, which
     collides with every other unlabeled bead and serializes the wave;
   - a **shared write token** across every bead that adds a file to a
     directory with a checked-in **derived** artifact (a migrations
     lockfile, a secrets baseline, a generated index): the derived file
     is a checksum-over-a-listing, so it collides at merge even though the
     added inputs don't. Serialize such beads on one token and ensure the
     project declares a `merge_reconcilers` / `merge_prepare` entry so a
     residual collision self-heals (docs/user-guide/merge-reconcilers.md).

4. **Declare external runtime resources — do not guess.** For every bead, ask
   what must be *running* for its acceptance criteria: a kind/k8s cluster, a
   docker compose stack, a dev server, a database, a browser suite. Label
   `res:<kind>` per kind (vocabulary in `koryph.project.json` `resources`;
   add new kinds there with a `mem_mb` estimate in the same change).
   Footprints protect the merge; resources protect the machine. Undeclared
   resources risk thrashing the host mid-wave; over-declared only costs
   parallelism.

5. **Discover dependencies.** Wire an edge wherever the design implies an
   order: producer API before its consumer, schema before the code that
   reads it, code before the docs that describe it, a feature flag before
   the change that flips it. A dependency edge is also the right fix
   whenever two beads *must* touch the same files in sequence rather than
   in parallel. Wire with:
   ```
   bd dep add <consumer-id> --blocked-by <producer-id>
   ```
   (`--blocked-by`/`--depends-on` are aliases; either reads unambiguously
   as "consumer depends on producer" — prefer this explicit form over the
   `bd create --deps type:id` shorthand, whose direction is easy to get
   backwards under review).

6. **Validate conflict-freedom.** Any two beads *not* ordered by the
   dependency graph must not share any token that **either** of them
   writes: writes(A) must be disjoint from writes(B) *and* from reads(B),
   and vice versa (write tokens are `area:*` + `fp:*` excluding
   `fp:read:*`). Only read-read overlap is free — a reader still cannot
   co-run with a writer of the same token, because the writer invalidates
   what the reader is reading. Where two unordered beads collide, fix it by (a) adding
   a dependency edge, (b) merging the beads, or (c) narrowing a footprint
   to what it honestly touches. Report the plan's achievable parallel
   width: the size of the largest antichain in the dependency graph whose
   members are pairwise write-disjoint.

7. **Route + guard.**
   - `model:<tier>` per bead by difficulty; state a one-line rationale for
     every non-default choice.
   - `refactor-core` on any bead touching the engine's own
     dispatch/merge/governor loop, or a protected path — these are never
     loop-dispatched (self-hosting safety rule); file them for the
     orchestrating session to implement on main instead.
   - `no-dispatch` plus a `HUMAN:` title prefix for operator-only steps
     (credentials, external approvals, anything no agent can do).

8. **File.** Create the epic, then each child with `--parent <epic-id>`,
   its labels, and `--validate`; wire dependencies per step 5. This part is
   mechanical — running already-decided commands is fine at any model
   tier.
   **If a wave loop is RUNNING against this project, dependency edges
   must exist the moment a child becomes visible** — a refill can snipe a
   dispatch-shaped bead in the seconds between `bd create` and `bd dep
   add`, dispatching a consumer before its producer. Either wire deps
   atomically at create time (`bd create --deps blocked-by:<producer-id>`)
   or create dependent children `--status deferred` and flip them open
   only after all edges are wired. Foundation/root beads (no incoming
   edges) are safe to create normally. Then have the `koryph-plan-scorer` persona (installed in
   `.claude/agents`) score the plan and apply one iteration of its
   findings. `koryph-plan-scorer` is pinned `tier: frontier` at `effort:
   xhigh` in its own frontmatter — plan validation is scheduler-correctness
   work, so NEVER spawn it with a model downgrade; if the project has
   swapped in its own scorer persona, respect whatever tier that persona
   pins.

9. **Report.** The epic id, total bead count, dependency edge count,
   achievable parallel width from step 6, and any residual serialization
   with the reason (shared write token, `refactor-core`, `domain:unknown`,
   or `no-dispatch`).

## Worked example

Design: "add rate limiting to the API server" — a per-client token-bucket
limiter, a config schema for its limits, wired into server startup, and
documented.

Decomposition (labels drawn from this hypothetical project's own
`area_map` — substitute the real one):

```
# Epic (umbrella; the loop never dispatches this one)
bd create --type epic --title "Add rate limiting to API server" \
  --description "Why: unbounded per-client request rate risks noisy-neighbor
outages. Design: docs/designs/2026-06-rate-limiting.md." --silent
# -> EPIC=proj-101

# Bead A: middleware — writes area:api only
bd create --parent proj-101 --type task \
  --title "Implement token-bucket rate limiter middleware" \
  --description "Why: docs/designs/2026-06-rate-limiting.md#limiter.
Done: a per-client token-bucket middleware in internal/api/middleware/
with unit tests for burst and steady-state behavior." \
  --label area:api --validate --silent
# -> A=proj-102

# Bead B: config schema — writes area:config only; disjoint from A
bd create --parent proj-101 --type task \
  --title "Add rate-limit config schema + validation" \
  --description "Why: docs/designs/2026-06-rate-limiting.md#config.
Done: per-route limit fields in internal/config/, validated on load,
with defaults matching the design doc's table." \
  --label area:config --validate --silent
# -> B=proj-103

# Bead C: wiring — consumes A and B, so it must follow both
bd create --parent proj-101 --type task \
  --title "Wire rate limiter into API server startup" \
  --description "Why: docs/designs/2026-06-rate-limiting.md#wiring.
Done: internal/api/server.go constructs the middleware from loaded
config and registers it on every route." \
  --label area:api --validate --silent
# -> C=proj-104
bd dep add proj-104 --blocked-by proj-102
bd dep add proj-104 --blocked-by proj-103

# Bead D: docs — reads A and C's code, writes only area:docs; follows C
bd create --parent proj-101 --type task \
  --title "Document rate limiting in the user guide" \
  --description "Why: docs/designs/2026-06-rate-limiting.md#docs.
Done: docs/user-guide/rate-limiting.md explains per-client limits and
how to tune them via config." \
  --label area:docs --label fp:read:api --label fp:read:config \
  --validate --silent
# -> D=proj-105
bd dep add proj-105 --blocked-by proj-104
```

Conflict check: A (`area:api`) and B (`area:config`) are unordered and
write-disjoint — they co-run. C depends on both, so it never races them. D
only reads `api`/`config` and depends on C. Achievable parallel width: **2**
(the antichain `{A, B}`); C and D serialize behind their producers, which
is the correct, dependency-driven serialization — not accidental
footprint collision.
