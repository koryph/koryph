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
3. Choose footprint labels so the bead can be batched in parallel with conflict-free work. Footprint and dependency assignment is scheduler-correctness work — if your current model is below opus tier and the footprint is not obvious, delegate this step to the `koryph-architect` agent (opus) rather than guessing:
   - One `area:<key>` for **every** `area_map` key the work will touch (from step 1). Carry every area it touches — over-broad only costs parallelism, under-broad risks a false-parallel merge conflict.
   - Prefer the **narrowest honest key**: per-package areas exist (`sched`, `quota`, `dispatch`, `ledger`, `govern`, `merge`, `review`, `worktree`, `beads`, `registry`); `engine` means the wave-loop package itself, not "anything in Go".
   - If the bead only **reads** an area (docs about it, tests over fixtures, analysis), declare that with `fp:read:<token>` (e.g. `fp:read:go:engine`) — readers co-run with each other; only writers exclude.
   - If the footprint can't be expressed with the `area_map`, use explicit `fp:<token>` labels, or leave it unlabeled (it serializes safely) and note that.
4. Run `bd create` with:
   - a clear `--title`,
   - a `--description` that states *why* the issue exists and what "done" looks like,
   - `--validate` so required sections are enforced,
   - the `area:*`/`fp:*` labels from step 3 (repeat `--label` per label),
   - `--label refactor-core` **only** if the work changes the koryph engine's own dispatch/merge/governor loop or a protected path (those are never loop-dispatched, so their footprint labels are advisory).
5. Show the result with `bd show <id>` and report the new id.

Do **not** start implementing the issue — this command only files it.
