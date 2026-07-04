<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph — the AI software factory

[![CI](https://github.com/koryph/koryph/actions/workflows/ci.yml/badge.svg)](https://github.com/koryph/koryph/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/koryph/koryph/badge)](https://scorecard.dev/viewer/?uri=github.com/koryph/koryph)
[![REUSE status](https://api.reuse.software/badge/github.com/koryph/koryph)](https://api.reuse.software/info/github.com/koryph/koryph)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

`koryph` takes a project from a git repo to built, signed, released software —
autonomous coding agents do the building, koryph enforces the discipline that
makes that safe. It stands on three pillars:

- **Build** — the agent factory. koryph reads each project's
  [beads](https://github.com/gastownhall/beads) ready-graph, batches
  conflict-free work by declared footprint, dispatches headless coding agents
  into isolated git worktrees under the **correct account**, checkpoints
  everything to disk, governs subscription burn, and merges finished work
  through a green gate.
- **Protect** — hygiene as code. Branch-protection rulesets and repo settings
  live as committed JSON (`make repo-check` / `make repo-apply`), commit
  signing is enforced from vault-served keys, protected paths and boundary
  guards contain every agent, and `koryph doctor` catches drift.
- **Ship** — the release train. Conventional-commit versioning for any
  language, draft-until-complete immutable releases, SBOM + cosign + SLSA
  provenance, and a vault-backed release bot — with graceful fallbacks when no
  bot is provisioned.

New here? Walk the whole path in
[Zero to shipped](docs/user-guide/zero-to-shipped.md).

## Invariants

- **Account-safe**: every dispatch constructs its Claude environment explicitly
  from the local project registry (never from the ambient shell), verifies the
  logged-in identity fail-closed, and logs the account used. Projects never
  switch accounts implicitly.
- **Subscription-first**: agent dispatch runs through the headless `claude` CLI
  on the account's subscription. Per-token API spend happens only when
  explicitly configured AND the governor has exhausted the subscription window.
  Batch mode (Message Batches API) requires explicit dispatch, always.
- **Billing guard defaults on, never blocks a baseline**: quota throttling
  (preflight, drain/stop, slot scaling) is enforced by default, automatically
  advisory while an account is uncalibrated, and disableable per run
  (`--no-billing-guard`) or per project (`billing_guard: advisory`).
- **Beads is the source of truth** for task state, per project. Each project
  carries its own beads/Dolt database (synced through that project's git
  remote), so every collaborator shares the same task graph.
- **Checkpoints live with the work**: run ledgers + manifests are repo files
  under each project's `.plan-logs/`; git commits are the durable checkpoints.
- **Opus is the model ceiling** unless a project's policy explicitly allows
  Fable. Recovery may upgrade to Opus, never beyond.
- **Never delete a dirty worktree** without explicit approval.

## Install (collaborators start here)

koryph is a single static binary — no Go toolchain or runtime needed.
Grab your platform's tarball from the
[latest release](https://github.com/koryph/koryph/releases/latest)
(signed, with checksums, SBOMs, and SLSA provenance), put `koryph` on your
`PATH`, and run `koryph version`. Building from source works with any Go
1.21+ (`go install github.com/koryph/koryph/cmd/koryph@latest` — the
pinned toolchain downloads automatically). Details:
[installation guide](docs/user-guide/installation.md).

Each collaborator keeps their own machine-local registry (`~/.koryph`,
created on first use) mapping shared projects to *their* Claude account:

```bash
koryph project add /path/to/project --account personal --identity you@example.com
koryph validate <project-id>
koryph run --project <project-id> --once
```

The shareable parts live in the project repo (`koryph.project.json`, the
beads database, `.claude/agents`); the personal parts (account mapping, quota
calibration, audit log) stay in `~/.koryph`. Full guides: `docs/` (mkdocs
book — user guide + developer guide).

## Versioning

Semantic versions, tagged `v<version>`. Projects pin a minimum engine in
`koryph.project.json`:

```json
{ "engine_version": "0.2+" }
```

`koryph run` refuses to drive a project that requires a newer engine.
Commit messages default to Conventional Commits; a project can supply its own
template via `commit_style: "custom"` + `commit_template`, or map a `commit`
persona in `stages`.

## Layout

| Path | Role |
|---|---|
| `cmd/koryph` | CLI entry point |
| `internal/engine` | wave loop (scan → batch → preflight → dispatch → poll → review → merge) |
| `internal/registry` | per-user project registry + audit log (`~/.koryph`, git-backed) |
| `internal/account` | account/profile env construction + identity verification |
| `internal/dispatch` | dispatch backends (subscription CLI, api-key CLI) |
| `internal/anthro` | direct Anthropic API + Message Batches (explicit only) |
| `internal/beads` | bd adapter (ready-graph, labels, merge-slot, snapshots) |
| `internal/sched` | footprint conflict coloring + wave building |
| `internal/ledger` | run ledger + checkpoint manifest v2 |
| `internal/worktree` | worktree lifecycle |
| `internal/merge` | rebase → green gate → ff-merge + protected paths |
| `internal/quota` | per-account usage windows + 80/90/95 loop governor |
| `internal/modelroute` | stage/label model resolution + rationale |
| `internal/promptc` | cache-stable prompt compiler |
| `internal/project` | per-project adapter config (`koryph.project.json`) |
| `internal/onboard` | project onboarding (inventory, register, validate) |
| `internal/version` | engine semver + project requirement check |
| `hooks/` | shipped Claude Code hooks (agent-boundary guard, worktree guard) |
| `agents/` | fallback agent personas installed into projects |

## For AI agents

A machine-readable index of the documentation (llmstxt.org format) lives at
[`docs/llms.txt`](docs/llms.txt) and is published at the docs-site root
(`/llms.txt`). Agents scanning this repository or the docs site should ingest it
to learn the project's structure, the operating contract in
[CLAUDE.md](CLAUDE.md), and the canonical user/developer guides.

## License

Apache-2.0. See [LICENSE](LICENSE).

## Disclaimer

This software is provided **AS IS** without warranty of any kind. Use of this
software constitutes acceptance of the terms in [DISCLAIMER.md](DISCLAIMER.md),
including the express indemnification clause and autonomous-agent risk warning.
If you do not agree, do not use this software.
