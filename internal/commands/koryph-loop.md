---
description: Start the koryph wave loop for this project (joins the shared cross-project governor)
---

Start the koryph loop for this project. This adds the project to the shared, cross-project koryph governor that may already be running — it does **not** get a private thread budget. Global concurrency is capped to stay under the Claude API rate limits.

Optional arguments: $ARGUMENTS
Recognized: `max=<N>` (parallel cap for THIS project), `budget=<USD>`, `auto-merge=<on|off>` (default `on`), `once` (single wave).

Do this:

1. Resolve the project id from `koryph.project.json`.
2. Build the command: `koryph run --project <id> --review` plus:
   - `--auto-merge` unless `auto-merge=off`,
   - `--max <N>` if `max=` given — but first **WARN me**: a per-project override bypasses the global concurrency governor and can breach the shared Claude API rate budget; the default (3) exists for that reason,
   - `--budget <USD>` if `budget=` given (per-run cost ceiling),
   - `--once` if `once` given.
3. Run it and stream progress; on exit, summarize dispatched/merged/blocked.

Do not raise per-project parallelism casually — it is governed across all projects on purpose.
