<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph — Claude index

Task→doc map; keep this file small and stable (prompt-cache warmth).

## Always

- Tasks live in **beads**: `bd ready` / `bd show <id>` / `bd update --claim` / `bd close`.
  Persistent insights: `bd remember` / `bd memories <kw>`. Never TodoWrite/markdown TODOs.
- Build/test via the self-documenting Makefile: `make help`; the green gate is `make gate`.
- Conventional Commits + DCO sign-off (`git commit -s`) + SSH-signed commits are
  enforced (local hooks, CI, and GitHub rulesets). Signing must be enabled first:
  `koryph signing enable --project koryph` (Proton Pass serves the key).

## Task → doc

| Task | Read first |
|---|---|
| Understand the system | [docs/architecture.md](docs/architecture.md) (Mermaid diagrams) |
| Any user-facing behavior | the matching [docs/user-guide/](docs/user-guide/) chapter — update it in the same change |
| Package internals | [docs/developer-guide/packages.md](docs/developer-guide/packages.md) |
| Tests / fixtures | [docs/developer-guide/testing.md](docs/developer-guide/testing.md) |
| Releases / versioning | [docs/developer-guide/releasing.md](docs/developer-guide/releasing.md) (keyless cosign) |
| Signing / vaults | [docs/user-guide/signing.md](docs/user-guide/signing.md) |
| Contributor rules | [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md) |

## Conventions

- Apache-2.0 SPDX header pair on every source file (pre-commit enforced).
- **Beads must be dispatch-shaped or the loop silently skips them.** Only
  `task`/`bug`/`chore` are dispatched (`feature`/`epic`/`decision`/
  `merge-request` are not); footprint = `fp:*` labels → `area:*` via
  `area_map` → else the catch-all `domain:unknown` token, which collides with
  every other unlabeled bead and serializes the wave. Label implementable
  beads with an `area:*` per area they touch (over-broad costs only
  parallelism; under-broad risks a false-parallel merge conflict). Mechanics:
  `internal/sched/footprint.go`, `internal/sched/wave.go`.
- `refactor-core`-labeled beads are NEVER loop-dispatched — the orchestrating
  session authors them on main (self-hosting safety rule).
- Protected paths (worktree merges refused): `.claude/`, `.beads/`, `hooks/`,
  `agents/`, `.github/`, `koryph.project.json`, `Makefile`,
  `.pre-commit-config.yaml`, `.envrc`, `LICENSE`.
- Self-build: `koryph run --project koryph --once --auto-merge --review`.


<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->
