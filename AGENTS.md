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
  automation (decomposition, footprint/dependency/resource assignment, plan scoring,
  security review, recovery analysis).
- **standard** — capable coding tier; implementation against a precise spec, tests, docs.
- **light** — fast/cheap tier; exploration, summarization, log triage.

Beads that need something *running* for their acceptance criteria (a kind/k8s
cluster, docker compose stack, dev server, database, or browser suite) carry a
`res:<kind>` label per kind. Footprints protect the merge; resources protect
the machine — undeclared resources risk thrashing the host mid-wave,
over-declared only costs parallelism.

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

## Output economy — quiet gate, file-spill wrappers, Read recovery

Gate and Bash output dominate transcript bytes (and therefore quota).

### Quiet gate: `make gate-agent`

Prefer `make gate-agent` over `make gate`: identical checks, same fail-fast
order, but one `PASS`/`FAIL` line per stage (plus a short tail on failure).
Full raw logs are teed to `$KORYPH_PHASE_DIR/gate-<stage>.log` (or a
repo-local scratch dir outside a dispatch) — recover the complete log with
the Read tool. On `FAIL`: read the tail, fix, re-run `make gate-agent`; don't
re-run `make gate` just for more output.

### File-spill wrappers: `hooks/koryph-spill.sh`

For any command whose output would dominate the transcript:

```
hooks/koryph-spill.sh <label> -- <command…>
```

Captures the command's full combined stdout+stderr byte-for-byte to a spill
file under the phase dir, prints a head+tail summary, preserves the exit
code exactly (failure signals are never eaten), and always ends with
`full output: <path>` — skipped entirely when the output is already smaller
than the summary budget.

### Recovery via Read

Whenever you see `full output: <path>`, use the Read tool to fetch the
complete file — the path is absolute and available for the dispatch's
lifetime; no separate download or shell command is needed.
