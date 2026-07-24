---
name: koryph-build
description: Build a single koryph issue (one bead) — pick from ready work if unspecified
---
<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Dispatch exactly one issue for this project into the koryph engine.

Optional argument (a specific bead id): $ARGUMENTS

Do this:

1. Resolve the project id from `koryph.project.json`.
2. Pick the target bead:
   - If an id was given, use it.
   - Otherwise run `bd ready`, show me the ready issues, and **ask me which one to build** — do not choose for me.
3. Dispatch just that bead: `koryph run --project <id> --once --review --only <bead>`.
   Add `--auto-merge` only if I ask for it.
4. Report the outcome and how to watch it: `koryph status --project <id>` and `koryph tail --project <id> <bead> --follow`.

Never bypass the koryph: the engine (not you) commits, merges, and closes the bead.
