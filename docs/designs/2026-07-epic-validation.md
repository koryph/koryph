<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Epic validation: whole-epic implementation review (2026-07-04)

Status: approved for implementation; epic + children filed from §7.
Origin: operator direction — "after all the child issues of each epic are
complete, each epic should run an implementation validation across the
whole epic to ensure that the implementations met the spirit and design
goals of the epic as a whole. If gaps are found, additional follow-up
beads can be run."

## 1. Why per-bead review is not enough

The per-bead review pass (`internal/review`) judges one branch diff against
one bead's acceptance criteria. It cannot see:

- **Integration gaps** — every child individually correct, but the seams
  between them never exercised (child A writes a config field child B
  never reads).
- **Design drift** — the epic's design doc promised X; the sum of the
  children delivered a narrower X′ because each bead was scoped honestly
  but the union has holes.
- **Spirit misses** — acceptance criteria technically met while the
  motivating problem (the "why" at the top of the design doc) is still
  reproducible.
- **Structural debt the parallel build creates** — N agents building N
  siblings independently is exactly how near-duplicate helpers, parallel
  implementations of the same function, and library-shaped code stranded
  in leaf packages accumulate. Nobody's bead was wrong; the union wants
  refactoring no single reviewer could see.

Epic validation is a **second, epic-scoped review stage** that runs once,
after the last child closes, over the union of the epic's merged work,
judged through two lenses:

1. **Completeness** — did the union meet the epic's description and
   design doc, in letter and spirit? Misses become *gap* follow-ups.
2. **Structural health** — now that the whole epic is visible at once:
   code that should be pulled out to common libraries (`internal/libs`
   or a new shared package), architecture correctness (dependency
   direction, seam placement, hub files reappearing, contract
   violations against docs/architecture.md), and overlap or duplication
   of function across the children (two beads solving the same
   sub-problem twice). Findings become *structural* follow-ups.

## 2. Trigger and lifecycle

```
child merged+closed
      │
      ▼
engine: parent epic open? ──no──▶ done
      │ yes
      ▼
any sibling open/in_progress/blocked? ──yes──▶ done (wait)
      │ no
      ▼
enqueue epic-validation (one at a time per project)
      │
      ▼
validator verdict ──met──▶ note + close epic (auto_close)
      │ gaps
      ▼
engine files follow-up child beads (round N+1 label)
epic stays open → loop dispatches follow-ups → last one closes
      │
      ▼
validation fires again … up to max_rounds, then PARK
(label validation:parked + note; operator decides)
```

- **In-loop trigger**: after the engine closes a merged bead, it checks the
  parent epic via `beads.ListChildren`. All children terminal (closed) and
  the epic still open → schedule validation. Fires for `epic`-type parents
  only; epics labeled `no-validate` are skipped.
- **On-demand**: `koryph epic validate <epic-id>` runs the same pass
  manually — the backfill path for epics that completed before this
  feature existed (2im, oji, c6j, …) and the recovery path for parked
  epics.
- **Round cap**: `max_rounds` (default 2) validation rounds per epic.
  Beyond that the epic is parked, never looped — a gap the validator
  cannot close in two rounds needs a human decision, not a third round.
- **Concurrency**: one validation at a time per project (they are cheap
  but judgment-heavy; serializing them costs nothing and keeps the event
  stream readable). Validation respects the quota guard's drain/stop the
  same way dispatch does; it is deferred, never skipped, when the ladder
  says stop.

## 3. The validator run

Mirrors `internal/review`'s architecture (one-shot read-only agent, strict
JSON, retries with exponential backoff, `Degraded` never a black box), but
epic-scoped, in a new package `internal/epicreview`:

- Runs **on main after all children merged** (not in a worktree — there is
  no branch; the work is already integrated).
- Persona `koryph-epic-validator`, pinned **frontier tier (opus)** —
  judging "spirit and design goals" and labeling follow-up beads with
  correct footprints is scheduler-correctness judgment, exactly the class
  of work koryph-plan gates to frontier.
- `--permission-mode plan` (read-only): the validator NEVER writes — no
  commits, no bead mutations. It returns a verdict; the ENGINE acts on it.
  Same separation review uses today.
- Prompt contains: the epic's title/description/notes; the design doc it
  references (the agent reads the path itself — it runs in the repo); every
  child's title, description, close reason, and merge SHA; the list of
  labels/footprints the children carried; and the epic's prior validation
  verdicts (round context).
- Strict JSON verdict:

```json
{
  "met": false,
  "summary": "one paragraph: what the epic set out to do and what landed",
  "gaps": [
    {
      "title": "…",
      "why": "which design goal / section is unmet and how",
      "acceptance": "what done looks like",
      "type": "task|bug|chore",
      "labels": ["area:…", "fp:read:…"],
      "depends_on": ["<sibling gap index or existing bead id>"]
    }
  ],
  "structural": [
    {
      "category": "extract-common|architecture|duplication",
      "title": "…",
      "why": "what exists twice / what belongs in a shared package /
               which architectural rule is violated, with file paths",
      "acceptance": "what done looks like",
      "type": "chore|task",
      "labels": ["area:…"]
    }
  ]
}
```

The two arrays have different closure semantics (§4): only `gaps` bear on
`met` and epic closure; `structural` findings are improvements the epic
surfaced, not obligations it failed. The prompt directs the validator to
diff-read the union of the children's merge commits specifically hunting
duplicate helpers, copy-adapted blocks, library-shaped code stranded in
leaf packages, and imports that violate the dependency direction in
docs/architecture.md.

- Verdict persisted to `.koryph/epic-reviews/<epic-id>-round<N>.json`
  (gitignored project state, sibling of posture snapshots) and appended to
  the run's events stream (`epic_validation` event) — the TUI events tab
  and `koryph obs tail` see it with zero extra plumbing.

## 4. Acting on the verdict (engine, deterministic)

- **met** → append the summary as an epic note, close the epic with reason
  `validated round N`. `auto_close` is configurable; default **true** —
  a clean validation IS the epic's completion. With `auto_close: false`
  the epic gets the note plus label `validation:passed` and closure stays
  an operator act.
- **gaps** → the engine (not the agent) files each gap as a child bead of
  the epic via `bd create --validate`, applying the validator's labels
  verbatim, wiring `depends_on` edges, and labeling every follow-up
  `validation:round-<N+1>` for traceability. Dedup guard: before creating,
  the engine checks existing children for a title match (the same
  collision class the koryph-plan skill guards against). The epic gets a
  note listing what was filed and why.
- **structural findings** → filed as **standalone beads** (not children
  of the epic), labeled `validation:structural` + the validator's
  footprint labels, with the source epic id in the description. They
  never block `met`, never hold the epic open, and never count against
  `max_rounds` — an epic that met its goals closes even when it surfaced
  refactoring work. Routing: if the project has a standing code-quality
  epic (koryph: `koryph-qta`), the engine parents them there via config
  (`structural_parent`); otherwise they stand alone in the backlog and
  compete on priority like any other bead. Findings that touch the
  engine's own loop get `refactor-core` per the standing rule.
- **degraded** → note on the epic, label `validation:degraded`, surfaced
  by the in-loop health patrol and `koryph doctor`. Never fails the run.

Follow-up beads are ordinary dispatch-shaped beads: the running loop picks
them up in the next refill with no special casing.

## 5. Configuration

`koryph.project.json`:

```json
"epic_validation": {
  "enabled": true,
  "model": "opus",
  "persona": "koryph-epic-validator",
  "max_rounds": 2,
  "auto_close": true,
  "timeout_seconds": 420,
  "structural_parent": "koryph-qta"
}
```

Defaults apply when the block is absent. `enabled` gates only the in-loop
trigger; `koryph epic validate` works regardless (explicit operator act).
Per-epic opt-out: label `no-validate`.

## 6. Non-goals

- Not a replacement for per-bead review — both run; they answer different
  questions (branch correctness vs epic completeness).
- No cross-epic validation (program-level review) in v1.
- No validator write access — gap-filing stays deterministic engine code.
- No automatic re-open of closed children; gaps become NEW beads.

## 7. Sequencing (the epic's children)

- **V1 foundation** — `internal/epicreview`: verdict types, prompt
  builder, one-shot runner (retry/backoff/degraded), verdict persistence.
  Mirrors `internal/review`. Fake-claude unit tests. (`area:epicreview` —
  added to area_map by the orchestrating session before filing.)
- **V2 config** — `epic_validation` block in `internal/project` config
  types + validation + defaults. Parallel with V1.
- **V3 CLI** — `koryph epic validate <id>` via cmdregistry (self-
  registering file; no shared-file edits) + roster/JSON output. After V1.
- **V4 engine hook** — post-close parent check, validation scheduling in
  the rolling loop, verdict handling (close/file/park), events, guard
  interplay. `refactor-core`: engine dispatch/merge loop — orchestrating
  session implements on main. After V1+V2.
- **V5 persona** — `koryph-epic-validator` agent file. `.claude/agents/`
  is a protected path → folded into the orchestrating session's V4 work.
- **V6 docs** — user-guide chapter + concepts cross-links + CLI reference
  regen. After V3/V4.

Parallel width 2 (V1 ∥ V2), then V3 ∥ V4-prep; honest serialization is
dependency-driven, not footprint collision.
