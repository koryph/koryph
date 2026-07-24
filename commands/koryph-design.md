---
name: koryph-design
description: Turn a natural-language ask — something to build, change, or fix — into a reviewed, repo-grounded design doc, then decompose it into a filed bead graph via /koryph-plan
---
<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Turn the operator's described intent into shippable work: first a written,
repo-grounded design, then — on approval — a filed, conflict-aware bead
graph the wave loop can dispatch. This command is the planning front door
for asks that arrive as prose rather than as a design doc.

Argument (the ask, verbatim — or empty to elicit it): $ARGUMENTS

## Route first — not every ask needs a design

- One small, self-contained fix or chore → `/koryph-issue` files it directly.
- A written design/spec doc already exists → `/koryph-plan <path>`.
- Roadmap/TODO/FIXME markdown already exists → `/koryph-import [path]`.
- Feature-sized, multi-part, or still vague → continue here.
- `$ARGUMENTS` empty → ask the operator what they want to build, change,
  or fix, then re-route through this list.

## Model requirements

Steps 1–4 (clarify, ground, design, outline) shape every downstream bead:
a wrong seam or a missed constraint here multiplies into mislabeled
footprints and false-parallel merge conflicts across the whole epic. These
steps require the **frontier reasoning tier of your agent runtime** —
Claude Opus-class, or the equivalent top tier of whatever runtime you are.
Below that tier, do not attempt them yourself: either tell the operator to
re-run `/koryph-design` on a frontier-tier model, or delegate steps 1–4
wholesale to the `koryph-architect` agent (pinned `tier: frontier`) and do
only the mechanical file-writing and reporting yourself.

## Do this

1. **Clarify.** Restate the ask in one paragraph: what changes, for whom,
   and why now. Resolve ambiguities from repo evidence where you can; ask
   the operator only what evidence cannot settle (scope boundaries,
   compatibility/performance constraints, what "done" must look like).
   Running non-interactively, make the narrowest reasonable assumption and
   record every one of them in the doc's Open questions section.

2. **Ground — do not design from memory.** Inspect the repository: grep
   for the symbols the ask names, read the packages it will touch, read
   `koryph.project.json` (the `area_map` keys and `resources` vocabulary
   the decomposition will label against), and skim `docs/designs/` for
   prior art. The design must name real files and symbols, not guesses.
   Dedup: `bd search "<scope keywords>"` — an existing epic or bead may
   already cover part of the ask; extend it rather than shadowing it.

3. **Design.** Decide the approach. Where a real choice exists, weigh at
   least two options and record why the winner wins. Design the extension
   seam explicitly: if the work fans out to N parallel units, name the
   foundation (a registry, a file-per-unit structure, the shared dep bump)
   that lets siblings ADD files rather than edit shared ones — the
   seam-first rule of `/koryph-plan` step 2 starts here, not at filing
   time.

4. **Write the doc** to `docs/designs/<YYYY-MM>-<slug>.md` (create the
   directory if the project lacks one; match the project's existing
   license-header convention if it has one). Sections:
   - **Problem** — why this work exists; the cost of not doing it.
   - **Goals / non-goals** — scope edges, stated bluntly.
   - **Current state** — the relevant files/symbols, with paths.
   - **Design** — the decisions, the alternatives considered, the seam.
   - **Implementation outline** — numbered, single-agent-sized units of
     work. For each unit: the concrete files/dirs it touches (the raw
     material for `area:*`/`fp:*` footprints), anything that must be
     *running* for its acceptance (the raw material for `res:*` labels),
     and which earlier units it consumes (the raw material for dependency
     edges).
   - **Acceptance criteria** — observable, per-unit where possible.
   - **Open questions / assumptions** — everything step 1 could not settle.
   Keep the doc self-contained: dispatched agents see only bead text plus
   this doc, never this conversation.

5. **Review gate.** Report the doc path and a one-screen summary: the
   problem, the chosen approach, the unit count, and the expected parallel
   width. Then STOP and wait for the operator to approve or edit the
   design — do not file beads without approval; a design error multiplies
   into every bead downstream.

6. **Hand off.** On approval, commit the doc (so dispatch-time worktrees
   branched from the default branch can read the path the beads will
   reference), then run `/koryph-plan <docpath>` — or follow
   `commands/koryph-plan.md` directly — to decompose it into one
   epic plus child beads with footprint labels (`area:*`, `fp:*`,
   `fp:read:*`), resource labels (`res:*`), model routing, and dependency
   edges, validated conflict-free.

Do **not** start implementing — this command designs and files; the wave
loop builds.
