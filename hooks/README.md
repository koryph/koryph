<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Shipped Claude Code hooks

`worktree-guard.sh` and `agent-boundary-guard.sh` are `PreToolUse` hooks
scoped to Koryph-dispatched agents (`KORYPH_PHASE_ID` set); interactive
sessions are never restricted by them. `koryph-prime.sh` is a
`SessionStart` hook and runs for every session, dispatched or not.
`koryph-intent.sh` is a `UserPromptSubmit` hook with the opposite scope:
interactive sessions only — it exits silently inside any dispatch.

- **`worktree-guard.sh`** — keeps an agent inside its project tree. Denies
  `Edit`/`Write` outside `$CLAUDE_PROJECT_DIR` (or worktree/temp siblings),
  denies `cd`/redirection escapes, screens `Bash` for prompt-injection phrasing.
- **`agent-boundary-guard.sh`** — denies orchestrator-only ops: `git push`,
  `git merge`, `git checkout|switch main|master`, `bd close`, `gh pr merge`.
  `git commit` and `git rebase` (incl. onto `main`) are explicitly allowed.
  Falls back to exit-2 + stderr when `jq` is unavailable. Also nudges (deny
  with a message, not a boundary violation) the two highest-confidence
  verbose-output patterns measured in koryph-77r.5 (docs/designs/
  2026-07-token-economy.md §3 L3): `go test` with `-v` on a broad
  (`...`-wildcard) package set, and `golangci-lint run` with no `--output`
  flag — both point at `make gate-agent` / `make lint-agent` instead.
- **`koryph-prime.sh`** — wraps `bd prime --hook-json` (koryph-77r.4,
  docs/designs/2026-07-token-economy.md §3 L2). Logs the injected byte size
  to `$KORYPH_DIR/prime-size.log` (never to stdout). For secondary-spawn
  sessions that never touch bead workflow (`KORYPH_SPAWN_KIND` in
  `review`/`stage`/`epicreview`), substitutes a small (<500 byte) slim
  profile instead of invoking `bd prime` at all; every other shape — main
  dispatches, interactive/operator sessions, or an unrecognized spawn kind —
  gets the full `bd prime --hook-json` output, byte-identical to the bare
  command it replaces. Fails open: any missing/erroring/unparsable `bd`
  output is still relayed as-is with exit 0, never blocking session start.
- **`koryph-intent.sh`** — the intent→beads router
  (docs/designs/2026-07-intent-routing.md). An adopted repo's CLAUDE.md
  predates koryph (or doesn't exist), so nothing repo-side tells a session
  that implementation work must become beads. When an interactive prompt
  reads like a description of something to build/change/fix (verb + length
  heuristic; questions and `/`-, `!`-, `#`-prefixed prompts stay silent),
  this hook injects a <1KB rubric mapping the ask to `/koryph-design`,
  `/koryph-plan`, `/koryph-import`, or `/koryph-issue`. Advisory only —
  never blocks or rewrites the prompt; skips dispatched and secondary-spawn
  sessions (`KORYPH_PHASE_ID`/`KORYPH_SPAWN_KIND`); fails open without `jq`
  or on unparsable input.

## settings.json wiring

Set `KORYPH_HOME` to this repo's checkout path (e.g. via direnv), then:

```json
"PreToolUse": [
  { "matcher": "Bash|Edit|Write",
    "hooks": [{ "type": "command", "command": "${KORYPH_HOME}/hooks/worktree-guard.sh" }] },
  { "matcher": "Bash",
    "hooks": [{ "type": "command", "command": "${KORYPH_HOME}/hooks/agent-boundary-guard.sh" }] }
]
```

Vendoring instead? Copy both scripts to a project's own `koryph/hooks/`
and point at `${CLAUDE_PROJECT_DIR}/koryph/hooks/...` — same behavior.

## Defense-in-depth, not the primary control

These back up the prompt contract (`agents/implementer.md` "Koryph
protocol"); they don't replace it. The prompt keeps a well-behaved agent from
attempting these ops; the hooks stop it deterministically if the prompt
drifts or is overridden. A hook denial is a bug signal, not routine traffic.
