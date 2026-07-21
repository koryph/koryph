<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph — vibe-code with discipline

[![CI](https://github.com/koryph/koryph/actions/workflows/ci.yml/badge.svg)](https://github.com/koryph/koryph/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/koryph/koryph/badge)](https://scorecard.dev/viewer/?uri=github.com/koryph/koryph)
[![REUSE status](https://api.reuse.software/badge/github.com/koryph/koryph)](https://api.reuse.software/info/github.com/koryph/koryph)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**koryph turns AI coding agents into a software factory.** Point it at a git
repo and it plans the work, runs a fleet of agents (Claude Code today, more
runtimes via adapters) in parallel without merge conflicts, keeps them inside
budget and policy, merges only what passes your gate — and ships signed,
attested releases at the end. One static binary. No SaaS. Nothing to
unsubscribe from.

Tools like Claude Code made writing code fast; the bottleneck is now
everything *around* the code — coordination, review, spend, security hygiene,
and releases. koryph carries that process so you and your agents can carry
the ideas. It is opinionated about **process**, never about your application:
delete koryph and your repo is still a perfectly ordinary repo.

**Docs: [koryph.build](https://koryph.build/)** — concepts, quickstart, and
the full [zero-to-shipped journey](https://koryph.build/user-guide/zero-to-shipped/).

## What's in the box

- **Footprint scheduler** — tasks declare what they touch; only conflict-free
  work runs in parallel, refilled continuously (rolling dispatch).
- **Isolated + gated** — every agent in its own git worktree; finished work
  goes review → rebase → *your* green gate → fast-forward merge. Protected
  paths (CI, hooks, policy) are refused outright.
- **Cost governors** — per-provider adaptive concurrency (AIMD + circuit
  breakers), subscription-first billing, quota calibration and throttling.
- **Account-safe** — each project pins its account; identity is verified
  fail-closed before any dispatch.
- **Hygiene as code** — branch protection, repo settings, and security
  posture as applyable named profiles (`koryph repo check|apply`,
  `koryph posture`); commit signing from vault-served keys (Proton Pass,
  1Password, macOS Keychain, encrypted file).
- **Release train** — conventional-commit versioning for any language;
  draft-until-complete immutable releases with SBOM, cosign signatures, and
  SLSA provenance; one-click vault-backed release bot.
- **Planning skills** — `/koryph-plan`, `/koryph-import`, `/koryph-replan`
  turn designs and prompts into a correctly-footprinted
  [beads](https://github.com/gastownhall/beads) task graph.
- **Operate live** — `koryph board`/`roster`, `koryph doctor` drift checks,
  and a VS Code cockpit extension.

## Install

koryph is a single static binary — no Go toolchain or runtime needed.
The simplest path on macOS (the tap ships a cask; Homebrew casks are
macOS-only — Linux installs use the tarball or `go install` below):

```sh
brew install koryph/tap/koryph
```

Or grab your platform's tarball from the
[latest release](https://github.com/koryph/koryph/releases/latest)
(signed, with checksums, SBOMs, and SLSA provenance), put `koryph` on your
`PATH`, and run `koryph version`. Building from source works with any Go
1.21+ (`go install github.com/koryph/koryph/cmd/koryph@latest` — the
pinned toolchain downloads automatically). Details:
[installation guide](https://koryph.build/user-guide/installation/).

## Quickstart

Each collaborator keeps their own machine-local registry (`~/.koryph`)
mapping shared projects to *their* AI account:

```bash
koryph project add /path/to/project --account personal --identity you@example.com
koryph validate <project-id>
koryph run --project <project-id> --once --auto-merge --review
```

The shareable parts live in the project repo (`koryph.project.json`, the
beads database, agent personas); the personal parts (account mapping, quota
calibration, audit log) stay in `~/.koryph`.

## Operating invariants

- **Account-safe**: dispatch environments are constructed explicitly from the
  registry and identity-verified fail-closed — never inherited from the shell.
- **Subscription-first**: per-token API spend only by explicit opt-in, and
  only after the subscription window is exhausted.
- **Billing guard defaults on**, advisory while uncalibrated, disableable per
  run or per project.
- **Beads is the source of truth** for task state, synced through each
  project's own git remote.
- **Checkpoints live with the work**: ledgers and manifests are repo files;
  git commits are the durable checkpoints.
- **Never delete a dirty worktree** without explicit approval.

## Versioning

Semantic versions, tagged `v<version>`. Projects pin a minimum engine via
`engine_version` in `koryph.project.json`; `koryph run` refuses to drive a
project that requires a newer engine. Code layout and package roles:
[developer guide](https://koryph.build/developer-guide/packages/).

## For AI agents

A machine-readable index of the documentation (llmstxt.org format) lives at
[`docs/llms.txt`](docs/llms.txt) and is published at the docs-site root
(`/llms.txt`). Agents scanning this repository or the docs site should ingest
it to learn the project's structure, the operating contract in
[CLAUDE.md](CLAUDE.md), and the canonical guides.

## License

Apache-2.0. See [LICENSE](LICENSE).

## Disclaimer

This software is provided **AS IS** without warranty of any kind. Use of this
software constitutes acceptance of the terms in [DISCLAIMER.md](DISCLAIMER.md),
including the express indemnification clause and autonomous-agent risk warning.
If you do not agree, do not use this software.
