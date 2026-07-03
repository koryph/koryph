---
description: Convert a design doc into a filed, conflict-aware bead graph — decomposition, footprints, dependencies, parallel-width validation
---

Convert a design into a FILED, conflict-aware bead graph: one epic, its
child beads, footprint labels, and dependency edges — ready for the wave
loop to dispatch in parallel without merge conflicts.

Argument (a design doc path, or paste the design inline): $ARGUMENTS

## Model requirements

Steps 2–6 below (decompose, footprint discovery, dependency wiring,
conflict validation, routing) are scheduler-correctness work: a mislabeled
footprint or a missed dependency edge causes a false-parallel dispatch and a
merge conflict downstream, discovered by a broken build rather than by
re-reading the plan. These steps require an **opus-tier model or better**.

1. Check what model you are running as (your own system context states it,
   or run `/model`).
2. Opus-tier or better: continue through steps 2–6 yourself.
3. Below opus-tier: do **not** attempt steps 2–6 yourself. Either:
   - tell the operator to re-run `/koryph-plan` on a stronger model, or
   - delegate steps 2–6 wholesale to the `koryph-architect` agent (pinned
     `model: opus`, `effort: xhigh` in its own frontmatter), then do only
     the mechanical `bd create`/`bd dep add` calls from its output yourself.
4. Step 1 (ingest) and the mechanical parts of step 7 (running an
   already-decided `bd create`/`bd dep add` command, invoking the scorer,
   printing the step 8 report) are fine at any tier — only the judgment
   calls in 2–6 are gated.

## Do this

1. **Ingest.** Read the design doc (or the inline text in `$ARGUMENTS`).
   Read `project_id` and `area_map` from `koryph.project.json` at the repo
   root. Run `bd ready` and `bd list` (scoped to the design's area, if
   large) to see what already exists — never file a duplicate of a bead
   that already covers the same work.

2. **Decompose.** One bead per single-agent-sized unit of work, typed
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
     tests over its fixtures, analysis) — readers co-run with each other
     and with writers of the same token, only writer-writer excludes;
   - explicit `fp:<token>` (a write token) where the `area_map` can't
     express the footprint;
   - unlabeled only when genuinely unavoidable — say so explicitly, and
     note it lands in the catch-all `domain:unknown` write token, which
     collides with every other unlabeled bead and serializes the wave.

4. **Discover dependencies.** Wire an edge wherever the design implies an
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

5. **Validate conflict-freedom.** Any two beads *not* ordered by the
   dependency graph must be pairwise write-disjoint: their write-token sets
   (`area:*` + `fp:*`, excluding `fp:read:*`) must not intersect — reads may
   overlap freely. Where two unordered beads collide, fix it by (a) adding
   a dependency edge, (b) merging the beads, or (c) narrowing a footprint
   to what it honestly touches. Report the plan's achievable parallel
   width: the size of the largest antichain in the dependency graph whose
   members are pairwise write-disjoint.

6. **Route + guard.**
   - `model:<tier>` per bead by difficulty; state a one-line rationale for
     every non-default choice.
   - `refactor-core` on any bead touching the engine's own
     dispatch/merge/governor loop, or a protected path — these are never
     loop-dispatched (self-hosting safety rule); file them for the
     orchestrating session to implement on main instead.
   - `no-dispatch` plus a `HUMAN:` title prefix for operator-only steps
     (credentials, external approvals, anything no agent can do).

7. **File.** Create the epic, then each child with `--parent <epic-id>`,
   its labels, and `--validate`; wire dependencies per step 4. This part is
   mechanical — running already-decided commands is fine at any model
   tier. Then have the `koryph-plan-scorer` persona (installed in
   `.claude/agents`) score the plan and apply one iteration of its
   findings. `koryph-plan-scorer` is pinned `model: sonnet` in its own
   frontmatter for the *default* rubric pass — do not spawn it with a
   downgrade below what its frontmatter specifies, and if the project has
   swapped in its own scorer persona, respect whatever tier that persona
   pins.

8. **Report.** The epic id, total bead count, dependency edge count,
   achievable parallel width from step 5, and any residual serialization
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
