---
description: Forcefully stop koryph agents (SIGKILL; this project or --all)
---

Forcefully stop koryph agents with SIGKILL. Use this **only** when a graceful `/koryph-stop` did not work: SIGKILL gives the agent no chance to commit, so uncommitted work in its worktree is lost.

Optional arguments: $ARGUMENTS  (a phase/bead id, and/or `--all`)

Do this:

1. Confirm with me that a graceful `/koryph-stop` was already tried.
2. If `--all` is given without a project: `koryph stop --all --force`.
3. Otherwise resolve the project id from `koryph.project.json`:
   - to kill **every** agent in that project, run `koryph stop --all --project <id> --force`,
   - otherwise run `koryph stop --project <id> <phase> --force` (list active phases and ask if no phase id was given).
4. Report what was killed and warn me about any worktrees that may hold uncommitted work.
