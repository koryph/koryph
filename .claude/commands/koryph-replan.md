---
description: Re-classify an existing bead corpus under current scheduler semantics — footprint repair, dependency wiring, parallel-width recovery
---

<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Re-classify an existing bead corpus so that footprint labels, dependency
edges, and routing tags are correct under the project's **current** scheduler
semantics (`area_map`, footprint rules, wave algorithm).  Run this after a
koryph upgrade that changes scheduler semantics, after an `area_map` change,
or before starting a long wave loop on a backlog created under older rules.

Optional argument (a single epic id, or leave empty to replan the full
open corpus): $ARGUMENTS

---

## Model requirements

Steps 3–5 (footprint re-verification, dependency wiring, conflict resolution)
are scheduler-correctness work: a mislabeled footprint or a missed dependency
edge causes a false-parallel dispatch and a merge conflict discovered by a
broken build, not a re-read of the plan.  These steps require the **frontier
reasoning tier of your agent runtime** — Claude Opus-class, or the equivalent
top tier of whatever runtime you are (codex, cursor, grok build, …).

1. Check what model you are running as (your own system context states it,
   or run `/model`).
2. **Frontier tier or better:** continue through steps 3–5 yourself.
3. **Below frontier:** do NOT attempt steps 3–5 yourself.  Either:
   - tell the operator to re-run `/koryph-replan` on a frontier-tier model, or
   - delegate steps 3–5 wholesale to the `koryph-architect` agent (pinned
     `tier: frontier`, `effort: xhigh` in its own frontmatter), then do only
     the mechanical `bd update`/`bd dep add` calls from its output yourself.

Steps 1–2 (audit snapshot) and the mechanical parts of steps 6–7 (running
already-decided `bd update`/`bd dep add` commands, re-running audit, printing
the report) are fine at any model tier.

---

## Do this

### Step 1 — Snapshot the corpus

Read `project_id` and `area_map` from `koryph.project.json` at the repo root.

Run the deterministic conflict analysis to capture the **before** state:

```
koryph plan audit --project <project_id> --json
```

Save the JSON output — you will diff it against the **after** state in step 7.
Key fields to note:

- `parallel_width.current` — achievable parallel width before replan
- `unlabeled` — beads resolving to `domain:unknown`
- `non_dispatchable` — beads the loop would silently skip
- `conflicts` — dependency-unordered pairs whose footprints collide

If `$ARGUMENTS` names an epic id, scope the work: run
`bd show <epic-id> --children` and restrict steps 3–6 to those beads only.
Record the before-width for the scoped set (count only beads in that set that
appear in `parallel_width` eligible beads).

For large corpora (> 20 open beads), process **epic-by-epic** in priority
order (`bd list --epic <id>` per epic).  After each epic, write a progress
note to the epic bead (`bd update <epic-id> --note "replan: processed <n>/<N>
beads, width <before>→<after>"`).  That note is the resume point: if this
session is interrupted, re-read it before continuing.

---

### Step 2 — Load current scheduler rules

Before re-verifying any footprint, read the current scheduler semantics so
you do not re-apply stale rules:

- `area_map` from `koryph.project.json` (loaded in step 1)
- `internal/sched/footprint.go` — how `area:*`/`fp:*`/`fp:read:*` labels
  compose into a `Footprint` struct (Reads vs. Writes)
- `internal/sched/wave.go` — the `Conflicts` predicate: two footprints
  conflict when any token is written by at least one side and present in both

Write-token rule summary (confirm against the actual code):

- `area:<key>` → write token `area:<key>` (unless downgraded by a
  co-present `fp:read:<key>`)
- `fp:read:<token>` → read token `<token>` only (read-read is free;
  write-read conflicts)
- `fp:<token>` (no `read:` prefix) → write token `<token>`
- No label → resolved to `domain:unknown` (one write token; serializes
  against all other unknowns)

If the footprint.go implementation differs from the above, use the **actual
implementation** — this template is a guide, not a source of truth.

---

### Step 3 — Re-verify footprints (frontier tier required)

For **every** bead flagged by the audit (unlabeled, conflicting, or any
bead you suspect is mis-labeled) — and for any bead whose labels were set
before the current `area_map` or footprint rules:

1. **Inspect the repository.** Read the bead's description carefully.  Grep
   for the concrete symbols, files, and packages the description names:
   ```
   grep -rn "<symbol>" --include="*.go" .
   ```
   Follow imports and callers until you have an accurate list of files the
   bead will actually touch.  **Never trust stale labels** — re-derive from
   the code, not from prior labeling.

2. **Map to tokens.**  For each file/package touched:
   - Look up the narrowest matching key in `area_map`.  One `area:<key>` per
     key the bead honestly touches (over-broad only costs parallelism;
     under-broad risks a false-parallel merge conflict).
   - If the touch is **read-only** (reading docs, running tests over fixtures,
     analysis without edits), declare it with `fp:read:<token>` instead of an
     `area:*` write token.
   - If the footprint cannot be expressed with any `area_map` key, use an
     explicit `fp:<token>` write token and explain why.
   - If the bead is genuinely footprint-free (a pure-docs bead that touches
     only `area:docs`), say so explicitly.

3. **Apply routing guards:**
   - `model:<tier>` with a one-line rationale for every non-default routing
     choice (frontier for novel scheduler-correctness work; default for
     routine implementation).
   - `refactor-core` if the bead touches the koryph engine's own
     dispatch/merge/governor loop or any protected path (`.claude/`,
     `.beads/`, `hooks/`, `agents/`, `.github/`, `koryph.project.json`,
     `Makefile`, `.pre-commit-config.yaml`, `.envrc`, `LICENSE`).  These are
     never loop-dispatched — the orchestrating session implements them on main.
   - `no-dispatch` plus a `HUMAN:` title prefix for operator-only steps
     (credential rotation, external approvals, anything no agent can do).

4. **Update the bead.**  Construct a complete `bd update` call that replaces
   the full label set with the corrected set.  Remove stale `area:*`/`fp:*`
   labels not supported by the current re-verification.  Remove any
   `area:unknown` label and replace with the actual token(s):
   ```
   bd update <id> \
     --remove-label area:<stale> \
     --label area:<correct> \
     [--label fp:read:<token>] \
     [--label model:<tier>] \
     [--label refactor-core]
   ```
   Add a brief note explaining the re-label rationale:
   ```
   bd update <id> --note "replan: relabeled area:<old>→area:<new> (grep: <symbol> in <file>)"
   ```

Work through beads in this priority order:

1. Unlabeled beads (`domain:unknown`) — highest parallelism impact; one label
   fix immediately removes a serialization bottleneck.
2. Conflicting pairs from the audit — fix footprint or add dep edge; see
   step 4 for dep-edge guidance.
3. Non-dispatchable beads with wrong type — change type with
   `bd update <id> --type task` (or `bug`/`chore`).
4. Beads with stale routing tags (`model:*`) — update or remove per current
   complexity.

---

### Step 3b — Repair missing resource declarations

Scan every open bead's title and description for a mention of a running
external dependency: `kind`, `k8s`, `docker`, `compose`, `dev server`,
`database`, `browser` (browser-suite / e2e-in-browser wording). Any match
that carries no `res:*` label is under-declared. Footprints protect the
merge; resources protect the machine — an undeclared cluster/compose/server
bead can thrash the host mid-wave with no admission-time signal.

1. **Grep the corpus.** For each match, read the bead's description to name
   the concrete kind(s) it will provision (`kind-cluster`, `docker`,
   `dev-server`, `database`, `browser-suite`, …).
2. **Add the label.** One `res:<kind>` per kind:
   ```
   bd update <id> --label res:<kind>
   ```
   If the kind is new to the project, note it in the report (step 7) — the
   `koryph.project.json` `resources` vocabulary entry (with a `mem_mb`
   estimate) is a protected-path, orchestrator-applied change; do not edit
   `koryph.project.json` yourself.
3. **Note the rationale:**
   ```
   bd update <id> --note "replan: added res:<kind> (description mentions <keyword>)"
   ```

This pass is deliberately keyword-driven, not judgment-gated like Step 3:
over-declaring only costs parallelism, while a missed declaration leaves a
real gap, so err toward labeling on any plausible match.

---

### Step 3c — Repair derived-artifact shared-write footprints

Scan every open bead's title and description for adding a file to a directory
that carries a checked-in **derived** artifact: `migration`, `atlas.sum`,
`.secrets.baseline`, `lockfile`, `checksum`, `baseline`, a `generated` index. A
derived artifact is a checksum-over-a-listing — two beads that each add an input
regenerate it independently and collide at merge even though the inputs (distinct
filenames) don't.

Any such bead that does **not** share a **write** token with the other
derived-artifact beads is under-serialized. Fix it:

1. **Give them a shared write token.** One `area:<key>` (or explicit
   `fp:<token>`) common to every bead writing that directory:
   ```
   bd update <id> --label area:<shared-key>
   ```
2. **Confirm the self-heal is declared.** The project should carry a
   `merge_reconcilers` / `merge_prepare` entry for the artifact in
   `koryph.project.json` (a protected-path, orchestrator-applied change — note
   it in the report, do not edit `koryph.project.json` yourself). See
   docs/user-guide/merge-reconcilers.md.

Like Step 3b this is keyword-driven, not judgment-gated: over-sharing a token
only costs parallelism, while an under-shared derived artifact re-collides at
merge.

---

### Step 4 — Wire missing dependency edges (frontier tier required)

After re-labeling, scan the corpus for implied order that has no explicit
`bd dep add` edge:

- **API-before-consumer**: a bead that defines an interface or package public
  surface must precede any bead that imports it.
- **Schema-before-code**: config schema, DB migration, or protocol definition
  before the code that reads it.
- **Code-before-docs**: a bead that adds user-visible behavior before the
  bead that documents it.
- **Feature-flag-before-flip**: a bead that introduces a flag before the bead
  that changes the flag's default.
- **Conflict-as-serialization**: when two unordered beads must touch the
  same files in sequence (genuine data dependency, not just footprint overlap),
  the right fix is a dep edge, not a footprint split.

Wire each implied order:
```
bd dep add <consumer-id> --blocked-by <producer-id>
```

Also scan for **missing edges to existing dependency chains**: a bead whose
description references another bead's output, or whose files are modified by
a prior bead in the same epic, needs an explicit edge even if both beads are
already labeled correctly.

**Propose** merges and splits (do not execute without operator confirmation):

| Pattern | Proposal |
|---------|----------|
| Two beads with identical footprints and descriptions that cover the same files | Merge: one bead, close the other as duplicate |
| One bead whose description describes two disjoint units of work | Split: one bead per unit, dep edge if ordered |
| An over-broad "area:*" that serializes many unrelated beads | Narrow each to its honest per-package token |

For any merge or close operation, print the exact `bd close` command and the
reason, then **stop and ask the operator to confirm** before running it.
Relabels (`bd update --label`) do not need confirmation — only
closes/merges/splits do.

---

### Step 5 — Validate conflict-freedom (frontier tier required)

After all updates and dep edges, verify by reasoning (not by re-running the
audit yet):

For every pair of open beads that are **dependency-unordered** (neither
transitively depends on the other), check that their write-token sets are
disjoint.  The rule: writes(A) must be disjoint from writes(B) and from
reads(B), and vice versa (write-read conflicts because the writer invalidates
what the reader reads).  Only read-read overlap is free.

Where a conflict remains:
- If there is a genuine data dependency, add a dep edge (step 4).
- If the footprints are genuinely disjoint but labeled over-broadly, narrow
  the footprint (step 3).
- If both beads must edit the same file and there is no dependency, consider
  merging them (propose, do not execute — step 4).

Report any residual conflicts you cannot resolve with the evidence available,
and explain what additional information the operator needs to provide.

---

### Step 6 — Apply mechanical updates

Run the decided `bd update` and `bd dep add` commands from steps 3, 3b, 4,
and 5.  This step is mechanical — running already-decided commands is fine
at any model tier.

For large corpora, run updates per-epic and write progress notes as described
in step 1 before moving to the next epic.  The note pattern is:

```
bd update <epic-id> --note "replan: step 6 done — <n> beads relabeled, <m> dep edges added"
```

---

### Step 7 — Re-run audit and report

Run the audit again to capture the **after** state:

```
koryph plan audit --project <project_id>
```

Report side-by-side:

```
PARALLEL WIDTH
  before:  <before.parallel_width.current>
  after:   <after.parallel_width.current>
  potential (after): <after.parallel_width.potential>

RELABELING SUMMARY
  beads relabeled: <count>
  dep edges added: <count>
  unlabeled remaining: <count>  (were <before_unlabeled_count>)
  conflicting pairs remaining: <count>  (were <before_conflict_count>)
  res:* labels added (step 3b): <count>
  derived-artifact shared-write fixes (step 3c): <count>

RESIDUAL SERIALIZATION (if any)
  <id>  <title>  → <reason: shared write token / refactor-core / domain:unknown / no-dispatch>

PENDING OPERATOR CONFIRMATION
  <proposed merges/splits — one line each with the bd command to run>
```

If `parallel_width.current` after equals `parallel_width.potential` after,
the corpus is fully parallel-correct under current labels.

If residual unlabeled or conflicting beads remain, list them and explain what
footprint information is missing (symbol grep that returned nothing, ambiguous
description, area_map key that does not cover the actual package).

---

## Trigger guidance

Run `/koryph-replan` in these situations:

- **After a koryph upgrade** that changes scheduler semantics (new footprint
  resolution rules, new `Conflicts` predicate, changed `area_map` expansion
  logic).
- **After an `area_map` change** in `koryph.project.json` — existing beads
  labeled with renamed or removed keys now resolve to `domain:unknown`.
- **Before starting a long wave loop** on a backlog that predates the current
  scheduler — prevents mid-wave serialization surprises.
- **After a bulk import** (`/koryph-import`) that assigned `area:unknown`
  labels pending frontier-tier re-verification.
- **After a project fork or merge** — the merged corpus may have overlapping
  footprints that were disjoint before the merge.

Do **not** run `/koryph-replan` on a corpus that is actively being consumed
by a running wave loop — wait until the loop drains or pause it first
(`koryph stop --project <id>`).

Do **not** start implementing any bead.  Do **not** run `koryph run`.  The
purpose of this command is to correct labeling and dependency wiring, not to
build work.
