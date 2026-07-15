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
  parallelism; under-broad risks a false-parallel merge conflict). Prefer the
  **narrowest honest area** — per-package areas exist (`area:sched`,
  `area:quota`, `area:dispatch`, `area:ledger`, `area:govern`, `area:merge`,
  `area:review`, `area:worktree`, `area:beads`, `area:registry`;
  `area:engine` means the wave-loop package itself; CLI-family: `area:cli`
  = command framework (main.go, cmdregistry, completion), `area:cli:bot`,
  `area:cli:posture`, `area:cli:release`, `area:cli:signing`,
  `area:cli:project`, `area:cli:ops`). Read-only touches use
  `fp:read:<token>` (readers co-run; writers exclude). Mechanics:
  `internal/sched/footprint.go`, `internal/sched/wave.go`.
- `refactor-core`-labeled beads are NEVER loop-dispatched — the orchestrating
  session authors them on main (self-hosting safety rule).
- **Footprints protect the merge; resources protect the machine.** Beads
  needing a running cluster/compose/dev-server/database/browser suite get
  `res:<kind>`; see docs/designs/2026-07-resource-governor.md.
- **Derived artifacts serialize even when their inputs don't.** A bead adding a
  file to a directory with a checked-in derived artifact (a migrations lockfile,
  a secrets baseline, a generated index) shares a **write** footprint with every
  other such bead — the checksum collides at merge though the inputs don't.
  Declare a `merge_reconcilers` / `merge_prepare` entry so a residual collision
  self-heals; see docs/user-guide/merge-reconcilers.md.
- Protected paths (worktree merges refused): `.claude/`, `.beads/`, `hooks/`,
  `agents/`, `.github/`, `koryph.project.json`, `Makefile`,
  `.pre-commit-config.yaml`, `.envrc`, `LICENSE`.
- Self-build: `koryph run --project koryph --once --auto-merge --review`.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking — commands and rules are in
"Always" above; run `bd prime` for the full workflow.

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions. It is subordinate to the rules above and to any explicit user, repository, or orchestrator instruction. Do not commit, push, or sync unless the active instructions grant it; at handoff, report changed files, validation, issue status, and any blocked commit/push step.
<!-- END BEADS INTEGRATION -->
