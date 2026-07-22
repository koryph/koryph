<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Epic validation

After all the children of an epic merge and close, koryph runs a
**whole-epic review** — a second, epic-scoped pass that looks at the union
of everything that shipped and asks two questions per-bead review cannot
answer:

1. **Completeness** — did the union meet the epic's description and design
   doc, in letter and spirit? Integration gaps, design drift, and spirit
   misses become *gap* follow-up beads.
2. **Structural health** — now that the whole epic is visible at once: code
   that belongs in a common library, architecture violations against
   `docs/architecture.md`, and functions duplicated across children become
   *structural* follow-up beads.

This is not a replacement for per-bead review. Both run; they answer
different questions (branch correctness vs epic completeness).

## Lifecycle

```
child merged + closed
      │
      ▼
parent epic open? ──no──▶ done
      │ yes
      ▼
any sibling open / in-progress / blocked? ──yes──▶ wait
      │ no
      ▼
epic labeled no-validate? ──yes──▶ skip validation AND docs stage; close
      │ no
      ▼
enqueue epic validation (one at a time per project)
      │
      ▼
validator verdict
      │
      ├──met──▶ label validation:passed
      │              │
      │              ▼
      │         docs_update enabled? ──no──▶ close epic (auto_close)
      │              │ yes
      │              ▼
      │         file validation:docs child bead (docs update stage)
      │              │
      │              ▼ docs bead merges
      │         close epic (completion check sees validation:passed)
      │
      ├──gaps──▶ file round-labeled child beads (validation:round-N+1)
      │          epic stays open → loop dispatches follow-ups
      │              │ last follow-up closes
      │              ▼
      │          validate again (up to max_rounds)
      │              │ max_rounds reached
      │              ▼
      └──parked──▶ label validation:parked; wait for operator
```

The docs update stage (after a passing round) is described fully in
[The docs update stage](#the-docs-update-stage) below.

## The two lenses

### Completeness

The validator reads the epic's title, description, notes, and referenced
design doc alongside every child's title, close reason, and merge SHA. It
looks for:

- **Integration gaps** — child A writes a config field child B never reads.
- **Design drift** — the union delivered a narrower implementation than the
  design promised, each bead honest in isolation.
- **Spirit misses** — acceptance criteria technically satisfied while the
  motivating problem is still reproducible.

Gap findings become child beads of the epic, labeled
`validation:round-<N+1>`. They enter the loop like ordinary beads and the
epic closes after they land. **Only gap findings bear on `met`** — the epic
closes when gaps are satisfied, even if structural findings remain.

### Structural health

In the same pass the validator hunts for debt that emerges only when the
whole epic is visible at once:

- **Extract-common** — library-shaped code stranded in a leaf package; near-
  duplicate helpers that each child implemented independently.
- **Architecture** — imports violating the dependency direction in
  `docs/architecture.md`, misplaced seams, hub files reappearing.
- **Duplication** — two beads solving the same sub-problem twice.

Structural findings are filed as **standalone beads** (not children of the
epic), labeled `validation:structural` plus the validator's footprint labels.
They never block `met`, never hold the epic open, and do not count against
`max_rounds`. If the project configures a `structural_parent` epic (koryph
uses `koryph-qta`), findings are parented there; otherwise they stand alone
in the backlog. Findings touching the engine's own loop are labeled
`refactor-core` per the standing rule.

## Configuration

Add an `epic_validation` block to `koryph.project.json` to override any
default. The block is optional; all fields below show their defaults.

```json
"epic_validation": {
  "enabled": true,
  "model": "opus",
  "persona": "koryph-epic-validator",
  "max_rounds": 2,
  "auto_close": true,
  "timeout_seconds": 420,
  "structural_parent": "",
  "docs_update": {
    "enabled": true,
    "labels": ["area:docs"]
  }
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `true` | Gate the in-loop automatic trigger. `false` disables the trigger; `koryph epic validate` still works. Per-epic opt-out: `no-validate` label. |
| `model` | `"opus"` | Model tier or concrete id for the validator agent. Frontier tier is the default — judging epic spirit and filing correctly-labeled gap beads requires it. |
| `persona` | `"koryph-epic-validator"` | Agent file used as the validator. |
| `max_rounds` | `2` | Validation rounds before the epic is parked. Must be ≥ 1 when set. |
| `auto_close` | `true` | Close the epic automatically after a passing validation (and after the docs bead merges, when `docs_update` is enabled). `false` leaves closure a manual act. |
| `timeout_seconds` | `420` | Per-run wall-clock timeout. Exceeded → `validation:degraded` with a reason naming the timeout (e.g. `validator timed out after 420s (timeout_seconds=420; large epics commonly need more)`). The default does not scale with epic size — it was sized for small epics. 6+ child epics commonly need 900s+ at opus effort; projects whose epics regularly exceed ~5 children should raise this explicitly. |
| `structural_parent` | _(absent)_ | Bead id of the epic or container under which structural findings are filed. Empty means standalone. |

### The docs update stage

When the validator returns `met`, the engine files one final child bead
before closing the epic — a documentation update bead — so that docs are
written against the settled implementation, never a moving target.

```json
"docs_update": {
  "enabled": true,
  "labels": ["area:docs"]
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `true` | File the docs update bead on a passing verdict. `false` closes the epic immediately per `auto_close`. |
| `labels` | `["area:docs"]` | Labels stamped on the filed bead alongside `validation:docs`. Add `fp:docs-nav` for projects with a shared nav block. |

The docs bead is dispatched with the `StageDocs` persona
(`koryph-feature-docs-author`, standard tier). Its description instructs the
agent to review the epic's design doc and every child's merge SHA, then
update all documentation the epic's changes touch. After the docs bead
closes, the completion check fires again, sees `validation:passed`, and
closes the epic — no re-validation round.

A `met` verdict never files a second docs bead once one has already closed
for this epic — the dedup check matches the canonical title or
`validation:docs` label against **every** child, open or closed, not just
open ones. A round that reaches `met` again after the docs bead already
merged closes the epic on the spot instead.

A parked or failed docs bead holds the epic open and surfaces through the
normal park and health-patrol channels.

Labeling an epic `no-validate` skips **both** the validation round and the
docs stage. The epic closes immediately when all children are terminal.

## Label vocabulary

| Label | Meaning |
|-------|---------|
| `no-validate` | Skip validation and the docs stage for this epic. |
| `validation:passed` | The validator returned `met` for this epic. |
| `validation:parked` | The epic exhausted `max_rounds` without reaching `met`; operator decision needed. |
| `validation:degraded` | The validator agent timed out or returned unparseable output. Non-fatal; surfaced by `koryph doctor`. |
| `validation:round-N` | Filed on gap follow-up beads by the engine to mark which validation round produced them. |
| `validation:structural` | Filed on structural follow-up beads; these never block the epic. |
| `validation:docs` | Filed on the auto-filed documentation update bead. |

## On-demand validation: `koryph epic`

Run validation manually at any time — the `enabled` flag does not gate the
explicit command. The two-word `koryph epic validate <epic-id>` form still
works as an alias:

```sh
koryph epic <epic-id> --project <project-id>
```

All children must be closed before the command will proceed. Use
`--round N` to override the round number (default: auto-detected from
existing verdict files under `.koryph/epic-reviews/`). Use `--json` to
emit the raw verdict JSON; the engine still acts on the verdict.

If the epic already carries `validation:passed` and every child — including
the docs-update bead — is now closed, the command closes the epic directly
and does **not** spawn a validator round: nothing is left to validate, and
the docs bead's closure is proof the prior round's verdict already applied.
This is the same close-after-docs shortcut the in-loop engine hook applies
(see [Self-healing](#self-healing-stranded-completed-epics) below), so
running the recovery command doctor/the health patrol name for a stalled
close-after-docs epic is always safe to re-run — it never re-validates work
that already passed.

```sh
# Backfill an epic that completed before epic validation existed:
koryph epic koryph-2im --project koryph

# Recovery path for a parked epic after operator triage:
koryph epic koryph-oji --project koryph --round 3

# Inspect the verdict without the human-readable summary:
koryph epic koryph-c6j --project koryph --json
```

Verdicts are persisted to `.koryph/epic-reviews/<epic-id>-round<N>.json`
(gitignored project state). Each run also emits an `epic_validation` event
visible in `koryph obs tail` and the TUI events tab.

## Degraded outcomes

When the validator agent times out or returns unparseable output, koryph
labels the epic `validation:degraded` and appends a note explaining the
failure. The run does not fail; subsequent waves continue. `koryph doctor`
surfaces degraded epics in its health report. Retry by running
`koryph epic <epic-id>` again after addressing the root cause (usually a
timeout — raise `timeout_seconds` or check quota headroom).

A timeout kill and an ordinary crash are distinguishable in the reason: a
wall-clock timeout always names itself, e.g. `validator timed out after
420s (timeout_seconds=420; large epics commonly need more)`. Only a genuine
non-timeout non-zero exit falls back to `validator exit <code>: <stderr
tail>`.

## Self-healing: stranded completed epics

The ordinary trigger is edge-driven: closing a child bead queues its parent
epic for a completion check on the next tick. Two situations fall outside
that edge — a crash between a child's close and the tick that would have
noticed it, or an epic that finished while no `koryph run` loop was active at
all — leaving an epic whose children are all closed but that never itself
closed or validated. Two backstops catch this:

- **Live loop.** The health patrol re-scans every open epic once per hour
  (independent of the normal 10-minute patrol cadence — listing every epic's
  children is comparatively expensive) and re-queues any stranded completed
  epic for validation exactly as if its last child had just closed.
- **`koryph doctor --project <id>`.** The `unvalidated-epics` check reports
  the same condition offline, as a warning naming `koryph epic validate <id>`
  as the recovery command — the check a stopped or never-started loop cannot
  run itself.

Both checks skip epics already carrying `no-validate`, `validation:parked`,
or `validation:degraded` — those are operator-decision or infra-failure
states surfaced elsewhere (see the label vocabulary above).

## See also

- [Work: beads and the ready-graph](../concepts/beads.md) — the bead
  lifecycle that epic validation extends.
- [CLI Reference: koryph epic](../reference/cli.md#koryph-epic)
- [Running waves](running-waves.md) — the wave loop that dispatches
  follow-up beads.
- [Billing & quota](billing-and-quota.md) — quota guard interaction with
  validation (deferred, never skipped).
