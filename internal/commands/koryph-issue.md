---
description: Create a well-formed beads issue in this koryph project
---

Create a new beads issue for the following request:

$ARGUMENTS

Do this:

1. Read `project_id` and `area_map` from `koryph.project.json` at the repo root — that is the beads workspace, and the `area_map` keys are the valid `area:*` footprint labels for this project.
2. Choose a `--type` and a `--priority` (`P0`–`P4`, default `P2`) from the request:
   - Use `task`, `bug`, or `chore` for anything implementable — these are the **only** types the wave loop dispatches.
   - Use `feature`/`epic` **only** for an umbrella/planning bead you do not want built directly; the loop skips them and they will sit in `bd ready` unbuilt.
3. Choose footprint labels so the bead can be batched in parallel with conflict-free work. Footprint and dependency assignment is scheduler-correctness work — if your current model is below the frontier reasoning tier of your runtime (Claude Opus-class or equivalent) and the footprint is not obvious, delegate this step to the `koryph-architect` agent (pinned `tier: frontier`) rather than guessing:
   - One `area:<key>` for **every** `area_map` key the work will touch (from step 1). Carry every area it touches — over-broad only costs parallelism, under-broad risks a false-parallel merge conflict.
   - Prefer the **narrowest honest key** the `area_map` offers — broad catch-all areas serialize everything that shares them.
   - If the bead only **reads** an area (docs about it, tests over fixtures, analysis), declare that with `fp:read:<token>` — readers co-run with each other; only writers exclude.
   - If the footprint can't be expressed with the `area_map`, use explicit `fp:<token>` labels, or leave it unlabeled (it serializes safely) and note that.
4. **Dedup before creating** (mandatory): `bd children <epic-id>` on the
   target epic and `bd search "<keywords>"` on the title's scope words.
   `bd ready` is NOT a dedup check — blocked/deferred beads are invisible
   there and are exactly the duplicates that resurrect and collide in
   flight. If an existing bead overlaps, update or unblock it instead.
5. Run `bd create` with:
   - a clear `--title`,
   - a `--description` that states *why* the issue exists and what "done" looks like,
   - `--validate` so required sections are enforced,
   - the `area:*`/`fp:*` labels from step 3 (repeat `--label` per label),
   - `--label refactor-core` **only** if the work changes the koryph engine's own dispatch/merge/governor loop or a protected path (those are never loop-dispatched, so their footprint labels are advisory).
6. Show the result with `bd show <id>` and report the new id.

Do **not** start implementing the issue — this command only files it.
