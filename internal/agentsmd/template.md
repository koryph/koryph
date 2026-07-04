<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# AGENTS.md — koryph operating contract

The **canonical, runtime-neutral operating contract** every agent follows in this
repository — Claude Code, Codex, Cursor, Grok, Copilot, opencode, amp, or any other
runtime. It states the *rules*; the deep *how* lives in the project's `docs/`, linked
inline. Claude-specific wiring lives in [CLAUDE.md](CLAUDE.md) when present; nothing
here assumes you are Claude.

## Capability tiers (not model names)

koryph sizes work by runtime-agnostic **tier**, mapped to your runtime's models by its
adapter:

- **frontier** — strongest reasoning tier; required where an error poisons downstream
  automation (decomposition, footprint/dependency assignment, plan scoring, security
  review, recovery analysis).
- **standard** — capable coding tier; implementation against a precise spec, tests, docs.
- **light** — fast/cheap tier; exploration, summarization, log triage.

## Task tracking: beads only

All work lives in **beads** (`bd`) — never TodoWrite or markdown TODO lists. Loop:
`bd ready` → `bd show <id>` → `bd update <id> --claim` → `bd close <id>`. Persist durable
insight with `bd remember` (no MEMORY.md files). Run `bd prime` once per session for the
full reference; the managed block below is the short form.

## The green gate

One command validates everything: `make gate` (format, build, vet, tests, lint — identical
to CI). It must be green before any work is called done; `make help` lists all targets.

## Commits

- **Conventional Commits**: `type(scope): imperative subject` — `feat`, `fix`, `docs`,
  `chore`, `refactor`, `test`, `ci`, `build`, `perf`, `style`.
- **DCO sign-off** on every commit: `git commit -s`.
- **SSH-signed** commits are required when signing is configured; enable signing first.

Commit early and often — commits are the only durable checkpoint; uncommitted work is lost
if a run is interrupted.

## Protected paths and boundary guards

Worktree merges are **refused** if the branch touches protected paths (configured in
`koryph.project.json`). Headless agents additionally run behind boundary guards that
**deterministically block** `git checkout main`, `git merge`, `git push`, `bd close`,
touching another worktree, or writing koryph's own enforcement surface. A guard denial
means you drifted — those actions belong to the orchestrator, not the agent; do not route
around it.

## Containment model

This project is managed by koryph. Containment depends on which capabilities the active
runtime supports:

- **Runtimes with hook support** (e.g., Claude Code): lifecycle hooks enforce boundary
  guards **deterministically at every tool call** — a violation is caught and blocked
  pre-execution. settings.json wiring and per-tool-use guard scripts are installed during
  `koryph project add`.
- **Runtimes without hook support** (e.g., Codex, Cursor, Grok, opencode, amp): containment
  relies on **worktree isolation** (agents work in a dedicated branch + worktree; the default
  branch is never writable) and **merge-time protected-path refusal** (the koryph engine
  refuses to merge a branch that touches a protected path, regardless of what the agent
  committed). Violations are caught post-execution at the merge gate rather than
  pre-execution.

In both cases, agents must never cross their worktree boundary. The only difference is
whether violations are caught before or after execution.

## Non-interactive shell

Always pass non-interactive flags so a `-i`-aliased tool cannot hang on a prompt:
`rm -f`, `rm -rf`, `cp -f`, `mv -f`; `ssh`/`scp -o BatchMode=yes`; `apt-get -y`.
