---
description: Convert existing markdown designs, plans, and TODO clusters into a filed, conflict-aware bead corpus — INVENTORY → decompose → dedupe → validate
---

<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Convert a project's existing markdown work (design docs, plans, roadmaps,
TODO/FIXME clusters) into a well-formed, conflict-aware bead corpus ready
for the wave loop to dispatch.

Optional argument (a specific file or directory to import, or leave empty to
scan the whole repo): $ARGUMENTS

---

## Model requirements

Steps 2–4 (footprint discovery, dependency wiring, conflict validation) are
scheduler-correctness work. A mislabeled footprint or a missed dependency
edge causes a false-parallel dispatch and a merge conflict downstream — errors
you only discover in a broken build. These steps require the **frontier
reasoning tier of your agent runtime** — Claude Opus-class, or the equivalent
top tier of whatever runtime you are (codex, cursor, grok build, …).

1. Check what model you are running as (your own system context states it,
   or run `/model`).
2. **Frontier tier or better:** continue through steps 2–4 yourself.
3. **Below frontier:** do NOT attempt steps 2–4 yourself. Either:
   - tell the operator to re-run `/koryph-import` on a frontier-tier model, or
   - delegate steps 2–4 wholesale to the `koryph-migration-analyst` agent
     (pinned `tier: frontier`, `effort: xhigh` in its own frontmatter) and
     use only its output for the `bd create`/`bd dep add` calls.

Steps 1 (inventory) and 5–7 (mechanical filing, validation, archival notes)
are fine at any model tier.

---

## Do this

### Step 1 — INVENTORY

Locate all candidate source material. Scan the repo for:

- **Design and plan documents**: any file under `docs/plans/`, `docs/designs/`,
  `docs/architecture/`, `docs/rfcs/`, `docs/specs/`, `plans/`, or similar.
- **Roadmap and backlog files**: `ROADMAP.md`, `BACKLOG.md`, `TODO.md`,
  `PLAN.md`, and any similarly named file at the repo root or in docs.
- **Inline TODO/FIXME clusters**: grep for dense concentrations:
  ```
  grep -rn "TODO\|FIXME\|HACK\|XXX" --include="*.go" --include="*.ts" \
       --include="*.py" --include="*.md" . | grep -v "vendor/" | \
       sort | uniq -c | sort -rn | head -40
  ```
  A cluster is ≥ 3 TODO/FIXME hits in a single file or directory.

Read `koryph.project.json` for `project_id`, `area_map`, and existing
beads scope. Run `bd list --all` to see every existing bead — you will
need this for deduplication in step 4.

If the `koryph-migration-analyst` persona's report exists in
`.plan-logs/` or was produced by `koryph onboard` mode-5, read it for the
already-completed inventory and skip the grep phase.

Classify each source into one of two shapes:

| Shape | Characteristics | Conversion path |
|-------|-----------------|-----------------|
| **Design-shaped** | Has sections for motivation, approach, acceptance criteria, or a specification structure. Reads like an RFC, design doc, or detailed plan. | Full `/koryph-plan` pipeline (step 2) |
| **Checklist-shaped** | Is primarily a list of items (tasks, TODOs, backlog entries) without deep rationale per item. | Batch `bd create` (step 3) |

Report the inventory before continuing: a table of sources, their
classified shape, and a one-line summary of the work they describe.
**Ask the operator to confirm before proceeding** if the inventory is large
(> 10 sources) or if any source is ambiguous.

---

### Step 2 — Design-shaped sources → /koryph-plan pipeline

For each **design-shaped** source, run the `/koryph-plan` pipeline on it
(decompose → footprint discovery → dependency wiring → conflict validation →
file). The `/koryph-plan` prompt describes that pipeline in full; treat each
design doc as the argument to `/koryph-plan`.

When running `/koryph-plan` for each source:

- Pass the **source file path** as the argument so the bead descriptions
  include a provenance pointer (`docs/designs/foo.md#section`).
- Check the resulting beads for overlap with existing beads (step 4) before
  finalising their titles.

---

### Step 3 — Checklist-shaped sources → batch create

For each **checklist-shaped** source, convert the list items to beads in
batches. For each item:

1. Decide `--type` (`task`/`bug`/`chore` — these are the only types the wave
   loop dispatches; `feature`/`epic` only for umbrella parents).
2. Choose footprint labels by inspecting the repo for what the item touches
   (grep for symbols, follow imports); apply the narrowest honest `area:*`
   keys from `area_map`. If the footprint is unclear and you are at frontier
   tier, reason about it; if below frontier, apply `area:unknown` and note it.
2b. Declare external runtime resources — do not guess: if the item needs
   something *running* (kind/k8s cluster, docker compose stack, dev server,
   database, browser suite), label `res:<kind>` per kind (vocabulary in
   `koryph.project.json` `resources`). Footprints protect the merge;
   resources protect the machine — undeclared resources risk thrashing the
   host mid-wave, over-declared only costs parallelism.
2c. Share a write token for derived-artifact touches: if the item adds a file
   to a directory with a checked-in **derived** artifact (a migrations
   lockfile, a secrets baseline, a generated index), give it a `fp:<token>`
   (or `area:<key>`) shared with every other such item — the checksum collides
   at merge even though the added files don't. A `merge_reconcilers` /
   `merge_prepare` entry self-heals the residual (docs/user-guide/merge-reconcilers.md).
3. Write a `--description` that states *why* the item exists, what "done"
   looks like, and carries a provenance pointer:
   ```
   Source: <file>#<line-or-section>, imported from markdown by /koryph-import
   ```
4. Run:
   ```
   bd create --type <type> --title "<title>" \
     --description "<description>" \
     --label area:<key> [--label area:<key2>] [--label res:<kind>] [--label fp:<shared-token>] \
     --validate --silent
   ```

For **inline TODO/FIXME clusters**: group nearby hits by file and create one
bead per logical cluster (not one per hit). Each bead's description lists the
specific line ranges:
```
Source: internal/foo/bar.go lines 42–67 (TODO cluster), imported by /koryph-import
```

---

### Step 4 — DEDUPE against existing beads

Before filing any bead, check every candidate title and description against
the full bead list from step 1. Deduplicate when:

- An existing bead already covers the same concrete work (same files, same
  outcome) — skip the candidate and note the match.
- An existing bead is a partial overlap — file the candidate with a
  description note explaining the distinction and a `--blocked-by` or
  `--depends-on` edge if the work is sequenced.

Never file a candidate that is a pure duplicate of an existing bead. Report
any skipped candidates with the reason and the existing bead id they matched.

---

### Step 5 — Validate the corpus

After all beads are filed, run:

```
koryph plan
```

This prints:

- **Parallel width**: the size of the largest antichain of write-disjoint,
  unblocked beads (the wave's achievable concurrency).
- **Serialization sources**: any write-token collision, `refactor-core`
  gate, or `domain:unknown` catch-all that reduces width below the ideal.
- **Orphan beads**: beads whose area labels don't exist in `area_map` — fix
  these to avoid landing in `domain:unknown`.

Apply one round of fixes from the audit output before reporting. Common fixes:

| Audit finding | Fix |
|---|---|
| Orphan `area:unknown` | Add the narrowest honest `area_map` key or an explicit `fp:*` label |
| Two unordered beads sharing a write token | Add `bd dep add <consumer> --blocked-by <producer>` or merge the beads |
| Large `domain:unknown` cohort serializing the wave | Annotate each with a real area token |

---

### Step 6 — Propose archival notes

For each source document that has been fully converted (all sections mapped
to beads), produce a short archival block to paste at the top of that doc.
**Do not modify the source files yourself** — print the proposed block and
let the operator apply it:

```markdown
<!-- koryph-import: archived <ISO-date>
     All work items converted to beads.
     Bead ids: <id1>, <id2>, …  (see bd show <id> for details)
     This file is now a historical reference; beads are the live work record.
-->
```

For partially converted sources (some sections still have no bead — e.g.
prose context, ADR conclusions, reference tables), note which sections were
skipped and why.

---

### Step 7 — Report

Print a summary table:

| Source | Shape | Beads filed | Deduped (matched) | Notes |
|--------|-------|-------------|-------------------|-------|
| docs/designs/foo.md | design | 5 | 1 (matched proj-42) | /koryph-plan run |
| ROADMAP.md | checklist | 8 | 2 | area:unknown on 3; see audit |

Then report:

- **Parallel width** from the `koryph plan` output.
- **Residual serialization**: any remaining `domain:unknown`, `refactor-core`,
  or `no-dispatch` beads, with the reason.
- **Archival notes**: paste the proposed archival blocks (step 6) for the
  operator to apply.
- **Remaining sources**: any files not imported (why: too vague, needs design
  work first, operator confirmation needed, etc.).

Do **not** modify source documents. Do **not** start implementing any bead.
Do **not** run `koryph run`. The purpose of this command is to file work
correctly, not to build it.
