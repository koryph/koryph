---
name: koryph-stop
description: Gracefully stop koryph agents (this project, or --all projects)
---
<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Gracefully stop running koryph agents with SIGTERM — each agent commits its open work and exits cleanly, and the engine picks up the exit on the next poll.

Optional arguments: $ARGUMENTS  (a phase/bead id, and/or `--all` to span every project)

Do this:

1. If `--all` is given without a project, stop across every project: `koryph stop --all`.
2. Otherwise resolve the project id from `koryph.project.json`:
   - to stop **every** agent in that project, run `koryph stop --all --project <id>`,
   - if a phase/bead id was given, run `koryph stop --project <id> <phase>`,
   - if not, list active phases with `koryph status --project <id>` and ask me which to stop.
3. Report what was signalled.

This is the safe stop. For an unresponsive agent, escalate with `/koryph-kill`.
