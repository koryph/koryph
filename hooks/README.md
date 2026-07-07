<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Shipped Claude Code hooks

Both are `PreToolUse` hooks scoped to Koryph-dispatched agents
(`KORYPH_PHASE_ID` set); interactive sessions are never restricted by them.

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
