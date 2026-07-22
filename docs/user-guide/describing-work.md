<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# From prompt to beads: intent routing

The loop dispatches **beads** — typed, footprinted, dependency-linked issues.
What you have is a sentence: "add OAuth login", "the exporter drops rows",
"make startup faster". This chapter is the bridge between the two: how a
described ask becomes a design, and how a design becomes beads the parallel
scheduler can actually dispatch.

The short version: **just describe what you want built, in a normal agent
session, in an adopted repo.** The plumbing below routes it from there.

## The router: `koryph-intent.sh`

Adoption installs a `UserPromptSubmit` hook (`koryph-intent.sh`, wired into
`.claude/settings.json`) that watches your interactive prompts. When one
reads like a description of something to **build, change, or fix**, the hook
injects a small routing note telling the session to map the ask onto the
planning commands rather than implementing ad hoc. Four invariants keep it
polite:

- **Fail-open** — the hook always exits 0; a broken router never blocks a
  prompt.
- **No recursion** — it stays silent inside dispatched agent sessions
  (koryph's own phases never re-route themselves).
- **Byte-frugal** — the injected rubric is under a kilobyte; your context is
  spent on your work, not on plumbing.
- **Advisory** — it never blocks or rewrites your prompt. Slash commands,
  `!` shell lines, and questions pass through untouched.

Runtimes without hook support get the same routing table as a section of the
installed `AGENTS.md`, so the contract holds anywhere the file is read.

## The routing table

| Your ask looks like… | Route | What it does |
|---|---|---|
| A feature-sized idea in prose | `/koryph-design "<ask>"` | Design doc first, then decomposition |
| An existing design doc | `/koryph-plan <doc>` | Decompose into an epic + child beads |
| A `ROADMAP.md`, `TODO.md`, or TODO/FIXME cluster | `/koryph-import [path]` | Convert the corpus into a bead graph |
| One small, well-understood fix | `/koryph-issue "<desc>"` | File a single dispatch-shaped bead |

## `/koryph-design` { #koryph-design }

`/koryph-design` is the front door for anything feature-sized. It runs on
the frontier tier — judging scope and writing a decomposable design is
exactly what the strongest model is for — and it works in six steps:

1. **Route first** — a small fix is redirected to `/koryph-issue`, an
   existing doc to `/koryph-plan`, markdown backlogs to `/koryph-import`.
   You never pay for a design pass you didn't need.
2. **Clarify** — ambiguities in the ask become questions, not assumptions.
3. **Ground** — the design is written against your *actual* repo: it greps
   the symbols it names, reads `area_map` and the resource vocabulary, and
   searches existing beads so it never files duplicate work.
4. **Design** — with an explicit extension seam, goals and non-goals.
5. **Write** — a self-contained design doc lands at
   `docs/designs/<YYYY-MM>-<slug>.md`: problem, goals, current state,
   design, implementation outline, acceptance criteria, open questions.
6. **Stop.** The skill halts for your review. Nothing is decomposed, filed,
   or built until you approve the design — this is one of the two mandatory
   human moments in [the lifecycle](../concepts/lifecycle.md).

On approval, the doc is committed and handed to `/koryph-plan`.

## What decomposition produces

`/koryph-plan` turns the approved doc into an **epic** with child beads that
are *dispatch-shaped* — the properties the scheduler requires, computed for
you:

- **A dispatchable type** — `task`, `bug`, or `chore` (containers like
  `epic`/`feature` are never dispatched).
- **Footprint labels** — one `area:*` token per area the bead touches, plus
  `fp:read:*` for read-only touches, so the scheduler can co-run everything
  that provably can't collide at merge. Honest labels are what buy
  parallelism: an unlabelled bead serializes behind everything.
- **Resource labels** — `res:<kind>` for beads that will provision a dev
  cluster, container stack, or long-running server, so the machine is
  protected as well as the merge.
- **Model routing** — `model:<tier>` labels where a stage clearly needs a
  stronger or cheaper model. Implementers default to sonnet, review and
  recovery run on opus, and explore/debug run on haiku; label a bead
  `model:opus` when the work genuinely needs it, or `model:haiku` for a
  purely mechanical change (a rename, a version bump, a generated-file
  refresh) where sonnet's reasoning is wasted. Weigh haiku against the fact
  that a failed attempt escalates straight to opus on requeue — the cheap
  tier only pays off when the change is trivial enough to land first try,
  gate and all.
- **Dependency edges** — so the ready-graph releases work in the right
  order, and the frontier is always safe to dispatch.

The graph is validated conflict-aware before it's filed. From there the
ordinary machinery takes over: `koryph run` dispatches the frontier,
[recovery and escalation](recovery.md) absorb failures, and
[epic validation](epic-validation.md) vets the finished whole against the
design doc you approved — closing the loop between what you asked for and
what shipped.

## A worked example

```text
You (in an agent session):
  "I want CSV export on the reports page, with a column picker,
   and it should stream instead of building the file in memory."

→ intent hook injects the routing rubric
→ session runs /koryph-design "CSV export with column picker…"
→ design doc written: docs/designs/2026-07-csv-export.md
→ you read it, tighten one acceptance criterion, approve
→ /koryph-plan files:
     myapp-e12        epic: CSV export on reports page
     myapp-e12.1      task: streaming CSV encoder        [area:api]
     myapp-e12.2      task: column-picker UI             [area:web]
     myapp-e12.3      task: export endpoint + wiring     [area:api, deps: .1]
     myapp-e12.4      task: e2e export test              [area:web, res:dev-server, deps: .2 .3]
→ koryph run --project myapp --review --auto-merge
     .1 and .2 dispatch in parallel (disjoint footprints);
     .3 follows .1; .4 waits for both and holds the dev-server resource
→ epic validation reviews the union against the design doc
```

## See also

- [Work: beads and the ready-graph](../concepts/beads.md) — why the graph,
  not a human, feeds the scheduler.
- [Intake](intake.md) — pulling existing GitHub/tracker issues into the
  same funnel.
- [Epic validation](epic-validation.md) — how the finished epic is vetted
  against the design.
- [Quickstart](quickstart.md) — the full `/koryph-*` command table.
